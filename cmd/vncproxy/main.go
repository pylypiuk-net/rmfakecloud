// vncproxy: VNC proxy for reMarkable native screen share.
// Runs ON the tablet. Listens for the tablet's UDP broadcast on port 5901,
// computes the RFB auth challenge, connects to the tablet's RFB server on
// port 5900 via SSL, completes the RFB handshake, and pipes the raw RFB
// stream to rmfakecloud over TCP for WebSocket bridging to the web UI.
//
// Protocol reverse-engineered from the rmview project:
// https://github.com/bordaigorl/rmview
package main

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync"

	"github.com/gorilla/websocket"
)

// RFB encoding constants
const (
	RAW_ENCODING       = 0
	COPYRECT_ENCODING  = 1
	RRE_ENCODING       = 2
	CORRE_ENCODING     = 4
	HEXTILE_ENCODING   = 5
	ZLIB_ENCODING      = 6
	ZRLE_ENCODING      = 16
	PSEUDO_CURSOR      = -239
	PSEUDO_DESKTOPSIZE = -223
)

func main() {
	userID := os.Getenv("USER_ID")
	if userID == "" && len(os.Args) > 1 {
		userID = os.Args[1]
	}
	if userID == "" {
		log.Fatal("USER_ID required (first arg or env)")
	}

	deviceToken := os.Getenv("DEVICE_TOKEN")
	if deviceToken == "" && len(os.Args) > 2 {
		deviceToken = os.Args[2]
	}

	serverHost := os.Getenv("SERVER_HOST")
	if serverHost == "" {
		serverHost = "172.21.1.1"
	}
	serverPort := os.Getenv("SERVER_PORT")
	if serverPort == "" {
		serverPort = "3000"
	}

	listenPort := os.Getenv("LISTEN_PORT")
	if listenPort == "" {
		listenPort = "5901"
	}

	log.Printf("vncproxy starting: userID=%s server=%s:%s listen=:%s", userID, serverHost, serverPort, listenPort)

	addr, err := net.ResolveUDPAddr("udp", ":"+listenPort)
	if err != nil {
		log.Fatalf("resolve udp: %v", err)
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		log.Fatalf("listen udp: %v", err)
	}
	defer conn.Close()
	log.Printf("Listening on :%s", listenPort)

	buf := make([]byte, 256)
	connected := false
	var mu sync.Mutex

	for {
		n, remoteAddr, err := conn.ReadFromUDP(buf)
		if err != nil {
			log.Printf("udp read: %v", err)
			continue
		}
		if n < 12 {
			continue
		}

		mu.Lock()
		if connected {
			mu.Unlock()
			continue // already connected, skip repeated broadcasts
		}
		mu.Unlock()

		log.Printf("Broadcast %d bytes from %s", n, remoteAddr)

		timestamp := buf[0:8]
		hashLen := binary.BigEndian.Uint32(buf[8:12])
		if 12+int(hashLen) > n {
			continue
		}

		// Parse RFB server address from broadcast
		addrOffset := 12 + int(hashLen)
		rfbHost := "127.0.0.1"
		rfbPort := uint16(5900)
		if addrOffset+7 <= n && buf[addrOffset] == 0x00 {
			addrOffset++
			rfbHost = net.IP(buf[addrOffset : addrOffset+4]).String()
			rfbPort = binary.BigEndian.Uint16(buf[addrOffset+4 : addrOffset+6])
		}
		log.Printf("RFB server at %s:%d", rfbHost, rfbPort)

		// Compute challenge: SHA256(timestamp + SHA256(userId))
		userIDHash := sha256.Sum256([]byte(userID))
		challengeInput := append(timestamp, userIDHash[:]...)
		challenge := sha256.Sum256(challengeInput)

		// Connect to RFB server via SSL
		rfbAddr := fmt.Sprintf("%s:%d", rfbHost, rfbPort)
		tlsConn, err := tls.Dial("tcp", rfbAddr, &tls.Config{InsecureSkipVerify: true})
		if err != nil {
			log.Printf("RFB connect: %v", err)
			continue
		}

		// RFB handshake
		err = rfbHandshake(tlsConn, challenge[:])
		if err != nil {
			log.Printf("RFB handshake: %v", err)
			tlsConn.Close()
			continue
		}
		log.Printf("RFB handshake OK!")

		mu.Lock()
		connected = true
		mu.Unlock()

		// Send SetEncodings + FramebufferUpdateRequest, then pipe to server
		go pipeRFBStream(tlsConn, serverHost, serverPort, deviceToken)
	}
}

func rfbHandshake(conn *tls.Conn, challenge []byte) error {
	// Read server version (12 bytes)
	version := make([]byte, 12)
	if _, err := io.ReadFull(conn, version); err != nil {
		return fmt.Errorf("read version: %w", err)
	}
	log.Printf("  Server: %s", string(version))

	// Echo version back (reMarkable uses "reM 001.001\n")
	if _, err := conn.Write(version); err != nil {
		return fmt.Errorf("send version: %w", err)
	}

	// Read number of security types
	numTypesBuf := make([]byte, 1)
	if _, err := io.ReadFull(conn, numTypesBuf); err != nil {
		return fmt.Errorf("read num sec types: %w", err)
	}
	numTypes := int(numTypesBuf[0])
	if numTypes == 0 {
		reasonLenBuf := make([]byte, 4)
		if _, err := io.ReadFull(conn, reasonLenBuf); err == nil {
			reasonLen := binary.BigEndian.Uint32(reasonLenBuf)
			if reasonLen > 0 && reasonLen < 256 {
				reason := make([]byte, reasonLen)
				io.ReadFull(conn, reason)
				return fmt.Errorf("rejected: %s", string(reason))
			}
		}
		return fmt.Errorf("no security types")
	}

	secTypes := make([]byte, numTypes)
	if _, err := io.ReadFull(conn, secTypes); err != nil {
		return fmt.Errorf("read sec types: %w", err)
	}
	log.Printf("  Sec types: %v", secTypes)

	// Select RM_AUTH (100) or NO_AUTH (1)
	useRMAuth := false
	for _, t := range secTypes {
		if t == 100 {
			useRMAuth = true
			break
		}
	}

	if useRMAuth {
		conn.Write([]byte{100})
		// Send challenge length (4 bytes) + challenge (32 bytes)
		lenBuf := make([]byte, 4)
		binary.BigEndian.PutUint32(lenBuf, uint32(len(challenge)))
		conn.Write(lenBuf)
		conn.Write(challenge)
		log.Printf("  Sent RM_AUTH challenge")

		// Read auth result (1 byte)
		result := make([]byte, 1)
		if _, err := io.ReadFull(conn, result); err != nil {
			return fmt.Errorf("read auth result: %w", err)
		}
		if result[0] != 0 {
			return fmt.Errorf("auth failed (result=%d)", result[0])
		}
		log.Printf("  Auth OK!")
	} else {
		// Try NO_AUTH
		hasNoAuth := false
		for _, t := range secTypes {
			if t == 1 {
				hasNoAuth = true
				break
			}
		}
		if !hasNoAuth {
			return fmt.Errorf("no supported auth type: %v", secTypes)
		}
		conn.Write([]byte{1})
		result := make([]byte, 4)
		io.ReadFull(conn, result)
		if binary.BigEndian.Uint32(result) != 0 {
			return fmt.Errorf("auth failed")
		}
	}

	// Send ClientInit (shared=1)
	conn.Write([]byte{1})

	// Read ServerInit (24 bytes)
	header := make([]byte, 24)
	if _, err := io.ReadFull(conn, header); err != nil {
		return fmt.Errorf("read serverinit: %w", err)
	}
	width := binary.BigEndian.Uint16(header[0:2])
	height := binary.BigEndian.Uint16(header[2:4])
	nameLen := binary.BigEndian.Uint32(header[20:24])
	log.Printf("  FB: %dx%d, nameLen=%d", width, height, nameLen)

	// Read server name if present
	if nameLen > 0 && nameLen < 1024 {
		name := make([]byte, nameLen)
		io.ReadFull(conn, name)
		log.Printf("  Name: %s", string(name))
	} else if nameLen == 0 {
		// reMarkable may send name as a separate frame — try reading it
		// The first data after ServerInit might be the name
		// Actually, the 18 bytes we saw ("reMarkable rfb") might be
		// a FramebufferUpdate or a custom message. Let's read it.
		// We'll handle it in the stream parsing.
		log.Printf("  No name in ServerInit, will parse stream")
	}

	return nil
}

func pipeRFBStream(rfbConn *tls.Conn, serverHost, serverPort, deviceToken string) {
	defer rfbConn.Close()

	// Send SetEncodings: message-type=2, padding=0, count=6, encodings
	encodings := []int32{
		RAW_ENCODING,
		PSEUDO_DESKTOPSIZE,
		PSEUDO_CURSOR,
	}
	setEnc := make([]byte, 4+len(encodings)*4)
	setEnc[0] = 2 // SetEncodings
	setEnc[1] = 0 // padding
	binary.BigEndian.PutUint16(setEnc[2:4], uint16(len(encodings)))
	for i, enc := range encodings {
		binary.BigEndian.PutUint32(setEnc[4+i*4:8+i*4], uint32(enc))
	}
	if _, err := rfbConn.Write(setEnc); err != nil {
		log.Printf("SetEncodings: %v", err)
		return
	}
	log.Printf("Sent SetEncodings: %v", encodings)

	// Send FramebufferUpdateRequest: type=3, incremental=0, x=0, y=0, w=0, h=0
	fbReq := make([]byte, 10)
	fbReq[0] = 3 // FramebufferUpdateRequest
	fbReq[1] = 0 // non-incremental
	binary.BigEndian.PutUint16(fbReq[2:4], 0) // x
	binary.BigEndian.PutUint16(fbReq[4:6], 0) // y
	binary.BigEndian.PutUint16(fbReq[6:8], 0) // width (0 = full)
	binary.BigEndian.PutUint16(fbReq[8:10], 0) // height (0 = full)
	if _, err := rfbConn.Write(fbReq); err != nil {
		log.Printf("FBUpdateRequest: %v", err)
		return
	}
	log.Printf("Sent FramebufferUpdateRequest")

	// Connect to rmfakecloud via WebSocket
	wsURL := fmt.Sprintf("ws://%s:%s/ui/api/screenshare/vnc/connect?token=%s", serverHost, serverPort, deviceToken)
	wsConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		log.Printf("WS connect to %s: %v", wsURL, err)
		return
	}
	defer wsConn.Close()
	log.Printf("WS connected to %s", wsURL)

	done := make(chan struct{}, 2)
	totalBytes := 0

	// RFB -> WebSocket (binary messages)
	go func() {
		buf := make([]byte, 65536)
		for {
			n, err := rfbConn.Read(buf)
			if err != nil {
				log.Printf("RFB read ended: %v (total=%d bytes)", err, totalBytes)
				done <- struct{}{}
				return
			}
			totalBytes += n
			if err := wsConn.WriteMessage(websocket.BinaryMessage, buf[:n]); err != nil {
				log.Printf("WS write ended: %v", err)
				done <- struct{}{}
				return
			}
			if totalBytes < 500 {
				log.Printf("  RFB->WS: %d bytes: %x", n, buf[:min(n, 32)])
			}
			// After each frame, request incremental update
			fbReq[1] = 1 // incremental
			rfbConn.Write(fbReq)
		}
	}()

	// WebSocket -> RFB (for input events from web UI)
	go func() {
		for {
			_, data, err := wsConn.ReadMessage()
			if err != nil {
				done <- struct{}{}
				return
			}
			if _, err := rfbConn.Write(data); err != nil {
				done <- struct{}{}
				return
			}
		}
	}()

	<-done
	log.Printf("Stream ended (total=%d bytes)", totalBytes)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}