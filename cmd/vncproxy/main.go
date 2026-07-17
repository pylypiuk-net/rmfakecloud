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
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"time"

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
	deviceToken := os.Getenv("DEVICE_TOKEN")
	if deviceToken == "" && len(os.Args) > 2 {
		deviceToken = os.Args[2]
	}

	// Extract auth0-userid from the JWT
	userID := os.Getenv("USER_ID")
	if userID == "" {
		userID = extractUserID(deviceToken)
	}
	if userID == "" {
		log.Fatal("USER_ID required (env, arg, or extractable from DEVICE_TOKEN JWT)")
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

	// Must bind to IPv4 explicitly — Go defaults to IPv6 which doesn't receive IPv4 broadcasts
	listenAddr := net.IPv4zero.String() + ":" + listenPort
	addr, err := net.ResolveUDPAddr("udp4", listenAddr)
	if err != nil {
		log.Fatalf("resolve udp: %v", err)
	}
	conn, err := net.ListenUDP("udp4", addr)
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
		
		// Server sends 4 bytes (challenge prompt/nonce) — MUST read before sending our challenge
		serverPrompt := make([]byte, 4)
		if _, err := io.ReadFull(conn, serverPrompt); err != nil {
			return fmt.Errorf("read RM_AUTH prompt: %w", err)
		}
		log.Printf("  RM_AUTH server prompt: %x", serverPrompt)
		
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

	// Read ServerInit (24 bytes) with timeout
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	header := make([]byte, 24)
	if _, err := io.ReadFull(conn, header); err != nil {
		return fmt.Errorf("read serverinit: %w", err)
	}
	conn.SetReadDeadline(time.Time{}) // reset deadline
	width := binary.BigEndian.Uint16(header[0:2])
	height := binary.BigEndian.Uint16(header[2:4])
	nameLen := binary.BigEndian.Uint32(header[20:24])
	log.Printf("  FB: %dx%d, nameLen=%d", width, height, nameLen)
	log.Printf("  ServerInit raw: %x", header)

	// Read server name if present
	if nameLen > 0 && nameLen < 1024 {
		name := make([]byte, nameLen)
		io.ReadFull(conn, name)
		log.Printf("  Name: %s", string(name))
	} else if nameLen == 0 {
		log.Printf("  No name in ServerInit, proceeding")
	}

	log.Printf("  Handshake complete, starting stream...")
	return nil
}

func pipeRFBStream(rfbConn *tls.Conn, serverHost, serverPort, deviceToken string) {
	defer rfbConn.Close()
	log.Printf("pipeRFBStream: starting, server=%s:%s", serverHost, serverPort)

	// Send SetPixelFormat: RGB565 (16bpp, 16bit depth, truecolor)
	// The reMarkable's ServerInit has a garbage pixel format, so we need
	// to explicitly set RGB565: red_max=31, green_max=63, blue_max=31,
	// red_shift=11, green_shift=5, blue_shift=0
	pixFmt := make([]byte, 20)
	pixFmt[0] = 0 // SetPixelFormat
	// padding bytes 1-3 = 0
	pixFmt[4] = 16 // bpp
	pixFmt[5] = 16 // depth
	pixFmt[6] = 0  // big-endian
	pixFmt[7] = 1  // true-color
	binary.BigEndian.PutUint16(pixFmt[8:10], 31)   // red-max
	binary.BigEndian.PutUint16(pixFmt[10:12], 63)  // green-max
	binary.BigEndian.PutUint16(pixFmt[12:14], 31)  // blue-max
	pixFmt[14] = 11 // red-shift
	pixFmt[15] = 5  // green-shift
	pixFmt[16] = 0  // blue-shift
	// padding bytes 17-19 = 0
	if _, err := rfbConn.Write(pixFmt); err != nil {
		log.Printf("SetPixelFormat: %v", err)
		return
	}
	log.Printf("Sent SetPixelFormat: RGB565 (16bpp)")

	// Send SetEncodings: message-type=2, padding=0, count=3, encodings
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

	// Send FramebufferUpdateRequest: request full screen
	// reMarkable screen is 1404x1872. The ServerInit returned 0x0,
	// so we need to specify the actual dimensions.
	fbReq := make([]byte, 10)
	fbReq[0] = 3 // FramebufferUpdateRequest
	fbReq[1] = 0 // non-incremental
	binary.BigEndian.PutUint16(fbReq[2:4], 0)    // x
	binary.BigEndian.PutUint16(fbReq[4:6], 0)    // y
	binary.BigEndian.PutUint16(fbReq[6:8], 1404) // width
	binary.BigEndian.PutUint16(fbReq[8:10], 1872) // height
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
			if totalBytes < 5000 || totalBytes%100000 < 65536 {
				log.Printf("  RFB->WS: %d bytes (total=%d), first: %x", n, totalBytes, buf[:min(n, 16)])
			}
			// Log the very first message in detail
			if totalBytes == n {
				log.Printf("  FIRST MESSAGE (full hex, %d bytes): %x", n, buf[:min(n, 64)])
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

// extractUserID extracts auth0-userid from a JWT token
func extractUserID(jwt string) string {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return ""
	}
	payload := parts[1]
	// Add padding
	for len(payload)%4 != 0 {
		payload += "="
	}
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return ""
	}
	var claims map[string]interface{}
	if err := json.Unmarshal(data, &claims); err != nil {
		return ""
	}
	if uid, ok := claims["auth0-userid"].(string); ok {
		return uid
	}
	return ""
}