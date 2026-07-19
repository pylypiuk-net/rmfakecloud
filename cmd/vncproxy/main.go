// vncproxy: VNC proxy for reMarkable native screen share.
// Runs ON the tablet. Listens for the tablet's UDP broadcast on port 5901,
// computes the RFB auth challenge, connects to the tablet's RFB server on
// port 5900 via SSL, completes the RFB handshake, and pipes the raw RFB
// stream to rmfakecloud's VNCHub via WebSocket for fan-out to web UI viewers.
//
// Protocol reverse-engineered from the rmview project:
// https://github.com/bordaigorl/rmview
//
// Key design decisions (matching rmview behavior):
//   - Do NOT send SetPixelFormat — use server's default format from ServerInit
//   - Request HEXTILE, CORRE, ZRLE, RRE, RAW encodings (server picks best)
//   - Use width/height from ServerInit (not hardcoded)
//   - E-ink inversion is handled in the viewer (0x0000=white, 0xFFFF=black)
package main

import (
	"bytes"
	"compress/zlib"
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

	"github.com/golang-jwt/jwt/v4"
	"github.com/gorilla/websocket"
	"golang.org/x/crypto/pbkdf2"
)

// nonBlockingBuffer removed — using io.Pipe with goroutine instead.


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
	if deviceToken == "" {
		deviceToken = loadDeviceTokenFromXochitl()
	}

	// Validate/refresh JWT if expired
	if deviceToken != "" && isJWTExpired(deviceToken) {
		log.Printf("Device token expired, generating fresh JWT...")
		newToken, err := generateFreshJWT()
		if err != nil {
			log.Printf("JWT generation failed: %v, using expired token", err)
		} else {
			deviceToken = newToken
			log.Printf("Generated fresh JWT")
		}
	}

	if deviceToken == "" {
		log.Fatal("DEVICE_TOKEN required (env, or extractable from xochitl.conf)")
	}

	// Extract auth0-userid from the JWT
	userID := os.Getenv("USER_ID")
	if userID == "" {
		userID = extractUserID(deviceToken)
	}
	if userID == "" {
		log.Fatal("USER_ID required (env, or extractable from DEVICE_TOKEN JWT auth0-userid field)")
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
	log.Printf("Listening on :%s (IPv4)", listenPort)

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
		serverInfo, err := rfbHandshake(tlsConn, challenge[:])
		if err != nil {
			log.Printf("RFB handshake: %v", err)
			tlsConn.Close()
			continue
		}
		log.Printf("RFB handshake OK! FB: %dx%d, bpp=%d, depth=%d, name=%q",
			serverInfo.width, serverInfo.height, serverInfo.bpp, serverInfo.depth, serverInfo.name)

		mu.Lock()
		connected = true
		mu.Unlock()

		// Refresh JWT if expired (tokens expire after ~25h)
		if isJWTExpired(deviceToken) {
			log.Printf("Device token expired, generating fresh JWT...")
			newToken, err := generateFreshJWT()
			if err != nil {
				log.Printf("JWT generation failed: %v, using expired token", err)
			} else {
				deviceToken = newToken
				log.Printf("Generated fresh JWT")
			}
		}

		// Send SetEncodings + FramebufferUpdateRequest, then pipe to server
		pipeRFBStream(tlsConn, serverHost, serverPort, deviceToken, serverInfo)

		// Reset connected flag so we can accept new broadcasts
		mu.Lock()
		connected = false
		mu.Unlock()
		log.Printf("Ready for next broadcast...")
		}
}

// serverInitInfo holds parsed ServerInit data
type serverInitInfo struct {
	width      uint16
	height     uint16
	bpp        uint8
	depth      uint8
	bigEndian  uint8
	trueColor  uint8
	redMax     uint16
	greenMax   uint16
	blueMax    uint16
	redShift   uint8
	greenShift uint8
	blueShift  uint8
	name       string
}

// chunkReader is a blocking io.Reader that waits for data to be
// available before returning. This is required because Go's zlib
// reader treats (0, nil) as "no progress" and errors out.
type chunkReader struct {
	mu           sync.Mutex
	chunks        [][]byte
	cond         *sync.Cond
	closed       bool
	totalFed     int // total bytes ever appended
	totalConsumed int // total bytes ever read out
}

func newChunkReader() *chunkReader {
	cr := &chunkReader{}
	cr.cond = sync.NewCond(&cr.mu)
	return cr
}

func (cr *chunkReader) Read(p []byte) (int, error) {
	cr.mu.Lock()
	defer cr.mu.Unlock()

	for len(cr.chunks) == 0 && !cr.closed {
		cr.cond.Wait()
	}
	if cr.closed && len(cr.chunks) == 0 {
		return 0, io.EOF
	}

	chunk := cr.chunks[0]
	n := copy(p, chunk)
	if n == len(chunk) {
		cr.chunks = cr.chunks[1:]
	} else {
		cr.chunks[0] = chunk[n:]
	}
	cr.totalConsumed += n
	return n, nil
}

func (cr *chunkReader) append(data []byte) {
	cr.mu.Lock()
	cr.chunks = append(cr.chunks, data)
	cr.totalFed += len(data)
	cr.cond.Signal()
	cr.mu.Unlock()
}

func (cr *chunkReader) bytesFed() int {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	return cr.totalFed
}

func (cr *chunkReader) bytesConsumed() int {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	return cr.totalConsumed
}

func (cr *chunkReader) close() {
	cr.mu.Lock()
	cr.closed = true
	cr.cond.Broadcast()
	cr.mu.Unlock()
}

func rfbHandshake(conn *tls.Conn, challenge []byte) (*serverInitInfo, error) {
	// Read server version (12 bytes)
	version := make([]byte, 12)
	if _, err := io.ReadFull(conn, version); err != nil {
		return nil, fmt.Errorf("read version: %w", err)
	}
	log.Printf("  Server: %s", strings.TrimSpace(string(version)))

	// Echo version back (reMarkable uses "reM 001.001\n")
	if _, err := conn.Write(version); err != nil {
		return nil, fmt.Errorf("send version: %w", err)
	}

	// Read number of security types
	numTypesBuf := make([]byte, 1)
	if _, err := io.ReadFull(conn, numTypesBuf); err != nil {
		return nil, fmt.Errorf("read num sec types: %w", err)
	}
	numTypes := int(numTypesBuf[0])
	if numTypes == 0 {
		reasonLenBuf := make([]byte, 4)
		if _, err := io.ReadFull(conn, reasonLenBuf); err == nil {
			reasonLen := binary.BigEndian.Uint32(reasonLenBuf)
			if reasonLen > 0 && reasonLen < 256 {
				reason := make([]byte, reasonLen)
				io.ReadFull(conn, reason)
				return nil, fmt.Errorf("rejected: %s", string(reason))
			}
		}
		return nil, fmt.Errorf("no security types")
	}

	secTypes := make([]byte, numTypes)
	if _, err := io.ReadFull(conn, secTypes); err != nil {
		return nil, fmt.Errorf("read sec types: %w", err)
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
			return nil, fmt.Errorf("read RM_AUTH prompt: %w", err)
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
			return nil, fmt.Errorf("read auth result: %w", err)
		}
		if result[0] != 0 {
			return nil, fmt.Errorf("auth failed (result=%d)", result[0])
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
			return nil, fmt.Errorf("no supported auth type: %v", secTypes)
		}
		conn.Write([]byte{1})
		result := make([]byte, 4)
		io.ReadFull(conn, result)
		if binary.BigEndian.Uint32(result) != 0 {
			return nil, fmt.Errorf("auth failed")
		}
	}

	// Send ClientInit (shared=1)
	conn.Write([]byte{1})

	// Read ServerInit (24 bytes) with timeout
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	header := make([]byte, 24)
	if _, err := io.ReadFull(conn, header); err != nil {
		return nil, fmt.Errorf("read serverinit: %w", err)
	}
	conn.SetReadDeadline(time.Time{}) // reset deadline

	info := &serverInitInfo{
		width:      binary.BigEndian.Uint16(header[0:2]),
		height:     binary.BigEndian.Uint16(header[2:4]),
		bpp:        header[4],
		depth:      header[5],
		bigEndian:  header[6],
		trueColor:  header[7],
		redMax:     binary.BigEndian.Uint16(header[8:10]),
		greenMax:   binary.BigEndian.Uint16(header[10:12]),
		blueMax:    binary.BigEndian.Uint16(header[12:14]),
		redShift:   header[14],
		greenShift: header[15],
		blueShift:  header[16],
	}
	nameLen := binary.BigEndian.Uint32(header[20:24])
	log.Printf("  ServerInit raw: %x", header)
	log.Printf("  FB: %dx%d, bpp=%d, depth=%d, trueColor=%d, R/G/B max=%d/%d/%d, R/G/B shift=%d/%d/%d",
		info.width, info.height, info.bpp, info.depth, info.trueColor,
		info.redMax, info.greenMax, info.blueMax,
		info.redShift, info.greenShift, info.blueShift)

	// Read server name if present
	if nameLen > 0 && nameLen < 1024 {
		name := make([]byte, nameLen)
		io.ReadFull(conn, name)
		info.name = string(name)
		log.Printf("  Name: %s", info.name)
	} else if nameLen == 0 {
		log.Printf("  No name in ServerInit")
	} else {
		log.Printf("  Suspicious nameLen=%d, skipping", nameLen)
	}

	log.Printf("  Handshake complete, starting stream...")
	return info, nil
}

func pipeRFBStream(rfbConn *tls.Conn, serverHost, serverPort, deviceToken string, info *serverInitInfo) {
	defer rfbConn.Close()
	log.Printf("pipeRFBStream: starting, server=%s:%s, FB=%dx%d bpp=%d",
		serverHost, serverPort, info.width, info.height, info.bpp)

	// Do NOT send SetPixelFormat — use server's default format (like rmview).
	// The reMarkable server sends a valid pixel format in ServerInit.
	// Forcing RGB565 was likely the cause of the "all-zeros" issue.

	// Send SetEncodings: message-type=2, padding=0, count=N, encodings
	// The rM VNC server hardcodes ZRLE regardless of client preference.
	encodings := []int32{
		ZRLE_ENCODING,
		HEXTILE_ENCODING,
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

	// Send FramebufferUpdateRequest: request full screen.
	// Use server's dimensions from ServerInit. If ServerInit returned 0x0
	// (which can happen), fall back to known reMarkable dimensions.
	w := info.width
	h := info.height
	if w == 0 || h == 0 {
		log.Printf("ServerInit returned 0x0 dimensions, using default 1404x1872")
		w = 1404
		h = 1872
	}
	fbReq := make([]byte, 10)
	fbReq[0] = 3 // FramebufferUpdateRequest
	fbReq[1] = 0 // non-incremental (full update)
	binary.BigEndian.PutUint16(fbReq[2:4], 0) // x
	binary.BigEndian.PutUint16(fbReq[4:6], 0) // y
	binary.BigEndian.PutUint16(fbReq[6:8], w)
	binary.BigEndian.PutUint16(fbReq[8:10], h)
	if _, err := rfbConn.Write(fbReq); err != nil {
		log.Printf("FBUpdateRequest: %v", err)
		return
	}
	log.Printf("Sent FramebufferUpdateRequest: %dx%d", w, h)

	// Connect to rmfakecloud via WebSocket
	wsURL := fmt.Sprintf("ws://%s:%s/ui/api/screenshare/vnc/connect?token=%s", serverHost, serverPort, deviceToken)
	wsConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		log.Printf("WS connect to %s: %v", wsURL, err)
		return
	}
	defer wsConn.Close()
	log.Printf("WS connected to %s", wsURL)

	// Respond to pings from the hub to keep the connection alive
	// (hub pings every 30s to prevent idle timeout).
	wsConn.SetPongHandler(func(string) error {
		wsConn.SetReadDeadline(time.Now().Add(90 * time.Second))
		return nil
	})

	// Send pixel format info to the viewer via VNCHub (as first binary message).
	// This tells the viewer the bpp, dimensions, and color masks from ServerInit.
	// Format: 4-byte magic "RFBF" + width(2) + height(2) + bpp(1) + depth(1) +
	//          bigEndian(1) + trueColor(1) + redMax(2) + greenMax(2) + blueMax(2) +
	//          redShift(1) + greenShift(1) + blueShift(1) + reserved(1) = 24 bytes
	meta := make([]byte, 24)
	meta[0] = 'R'
	meta[1] = 'F'
	meta[2] = 'B'
	meta[3] = 'F'
	binary.BigEndian.PutUint16(meta[4:6], w)
	binary.BigEndian.PutUint16(meta[6:8], h)
	meta[8] = info.bpp
	meta[9] = info.depth
	meta[10] = info.bigEndian
	meta[11] = info.trueColor
	binary.BigEndian.PutUint16(meta[12:14], info.redMax)
	binary.BigEndian.PutUint16(meta[14:16], info.greenMax)
	binary.BigEndian.PutUint16(meta[16:18], info.blueMax)
	meta[18] = info.redShift
	meta[19] = info.greenShift
	meta[20] = info.blueShift
	// meta[21:24] reserved = 0
	if err := wsConn.WriteMessage(websocket.BinaryMessage, meta); err != nil {
		log.Printf("WS meta send: %v", err)
		return
	}
	log.Printf("Sent RFBF meta: %dx%d bpp=%d depth=%d R/G/B=%d/%d/%d shift=%d/%d/%d",
		w, h, info.bpp, info.depth, info.redMax, info.greenMax, info.blueMax,
		info.redShift, info.greenShift, info.blueShift)
		// ZRLE decompression: SERVER-SIDE with accumulator that resets on full-screen updates.
		// The rM VNC server uses a single persistent zlib stream across all
		// ZRLE rects/frames. We accumulate zlib data and re-decompress from scratch
		// each frame. To bound latency, we reset the accumulator when we see a
		// full-screen rect (0,0,1404,1872) — the 30s periodic full refresh.
		// This keeps the accumulator small (~5-10 frames worth, ~300KB max).

		var (
			accumBuf  []byte
			decompOff int // offset in decompressed output consumed so far
		)

		decodeZRLEFrame := func(zlibData []byte, w, h int) []byte {
			// Reset accumulator on full-screen update (starts a new zlib context window)
			if w == int(info.width) && h == int(info.height) {
				accumBuf = accumBuf[:0]
				decompOff = 0
			}

			// Append this rect's zlib data to the accumulator
			accumBuf = append(accumBuf, zlibData...)

			// Re-decompress from scratch
			zr, err := zlib.NewReader(bytes.NewReader(accumBuf))
			if err != nil {
				log.Printf("zlib.NewReader failed: %v (accumBuf=%d bytes, first: %x)", err, len(accumBuf), accumBuf[:min(8, len(accumBuf))])
				return nil
			}
			// Read decompressed output. The zlib stream is continuous, so we
			// collect whatever output is available before ErrUnexpectedEOF.
			var decompressed []byte
			tmpBuf := make([]byte, 32768)
			for {
				n, readErr := zr.Read(tmpBuf)
				if n > 0 {
					decompressed = append(decompressed, tmpBuf[:n]...)
				}
				if readErr != nil {
					if readErr != io.ErrUnexpectedEOF && readErr != io.EOF {
						log.Printf("zlib read: %v (got %d bytes)", readErr, len(decompressed))
					}
					break
				}
			}
			zr.Close()

			// This rect's tile data is the new bytes since last call
			if len(decompressed) <= decompOff {
				return nil
			}
			rectData := decompressed[decompOff:]
			decompOff = len(decompressed)

			// Re-compress as standalone zlib (with 789c header)
			var recompressed bytes.Buffer
			zw := zlib.NewWriter(&recompressed)
			zw.Write(rectData)
			zw.Close()

			// Return with 4-byte length prefix (viewer expects this)
			out := make([]byte, 4+recompressed.Len())
			binary.BigEndian.PutUint32(out[:4], uint32(recompressed.Len()))
			copy(out[4:], recompressed.Bytes())
			return out
		}
		_ = decodeZRLEFrame

	decodeZRLEInPlace := func(msg []byte, info serverInitInfo) []byte {
		if len(msg) < 4 {
			return msg
		}
		numRects := int(binary.BigEndian.Uint16(msg[2:4]))
		offset := 4
		var out bytes.Buffer
		out.Write(msg[:4])
		changed := false

		for r := 0; r < numRects; r++ {
			if offset+12 > len(msg) {
				return msg
			}
			rectHeader := msg[offset : offset+12]
			enc := int32(binary.BigEndian.Uint32(rectHeader[8:12]))
			w := int(binary.BigEndian.Uint16(rectHeader[4:6]))
			h := int(binary.BigEndian.Uint16(rectHeader[6:8]))

			if enc == 16 { // ZRLE
				if offset+16 > len(msg) {
					return msg
				}
				zlibLen := int(binary.BigEndian.Uint32(msg[offset+12 : offset+16]))
				if offset+16+zlibLen > len(msg) {
					return msg
				}
				zlibData := msg[offset+16 : offset+16+zlibLen]
				newData := decodeZRLEFrame(zlibData, w, h)
				if newData == nil {
					return nil
				}
				newRect := make([]byte, 12+len(newData))
				copy(newRect[:12], rectHeader)
				copy(newRect[12:], newData)
				out.Write(newRect)
				offset += 16 + zlibLen
				changed = true
			} else if enc == 0 { // RAW
				rectLen := w * h * int(info.bpp) / 8
				if offset+12+rectLen > len(msg) {
					return msg
				}
				out.Write(msg[offset : offset+12+rectLen])
				offset += 12 + rectLen
			} else if enc == -223 { // DesktopSize
				out.Write(rectHeader)
				offset += 12
			} else {
				out.Write(msg[offset:])
				break
			}
		}

		if !changed {
			return msg
		}
		return out.Bytes()
	}

	done := make(chan struct{}, 2)
	totalBytes := 0

	// Also save raw RFB data to file for offline analysis
	rawFile, _ := os.Create("/tmp/rfb_raw.bin")
	defer rawFile.Close()

	// RFB -> WebSocket (binary messages)
	// We accumulate a complete FramebufferUpdate before sending, so each
	// WebSocket message contains exactly one RFB message. This lets the
	// VNCHub cache complete frames for late-joining viewers.
	go func() {
		buf := make([]byte, 65536)
		var frameBuf []byte // accumulated current frame
		for {
			n, err := rfbConn.Read(buf)
			if err != nil {
				log.Printf("RFB read ended: %v (total=%d bytes)", err, totalBytes)
				done <- struct{}{}
				return
			}
			totalBytes += n
			if rawFile != nil {
				rawFile.Write(buf[:n])
			}

			frameBuf = append(frameBuf, buf[:n]...)

			// Try to extract complete FramebufferUpdate messages from frameBuf.
			// RFB message types from server:
			//   0 = FramebufferUpdate
			//   1 = SetColourMapEntries
			//   2 = Bell
			//   3 = ServerCutText
			// We send each complete message as its own WebSocket message.
			for len(frameBuf) > 0 {
				msgType := frameBuf[0]
				var msgLen int
				consumed := false

				if msgType == 0 {
					// FramebufferUpdate: msgType(1) + pad(1) + numRects(2) + rects
					if len(frameBuf) < 4 {
						break // need more data
					}
					numRects := int(binary.BigEndian.Uint16(frameBuf[2:4]))
					offset := 4
					complete := true
					for r := 0; r < numRects; r++ {
						if offset+12 > len(frameBuf) {
							complete = false
							break
						}
						w := int(binary.BigEndian.Uint16(frameBuf[offset+4:offset+6]))
						h := int(binary.BigEndian.Uint16(frameBuf[offset+6:offset+8]))
						enc := int32(binary.BigEndian.Uint32(frameBuf[offset+8:offset+12]))
						offset += 12

						rectLen := 0
						switch enc {
						case 0: // RAW
							rectLen = w * h * int(info.bpp) / 8
						case 16: // ZRLE
							if offset+4 > len(frameBuf) {
								complete = false
								break
							}
							zlibLen := int(binary.BigEndian.Uint32(frameBuf[offset:offset+4]))
							rectLen = 4 + zlibLen
							if totalBytes < 200000 && offset+8 <= len(frameBuf) {
								zlibData := frameBuf[offset+4 : offset+4+min(zlibLen, 8)]
								log.Printf("  ZRLE zlibLen=%d first bytes: %x", zlibLen, zlibData)
							}
						case 5: // HEXTILE — variable, hard to predict. For now treat as raw estimate.
							rectLen = w * h * int(info.bpp) / 8
						case -223: // DesktopSize
							rectLen = 0
						case -239: // Cursor
							rectLen = w*h*int(info.bpp)/8 + (w*h+7)/8
						default:
							// Unknown encoding — can't compute length, send what we have
							complete = false
						}
						if !complete {
							break
						}
						if offset+rectLen > len(frameBuf) {
							complete = false
							break
						}
						offset += rectLen
					}
					if complete {
						msgLen = offset
						consumed = true
					} else {
						break // wait for more data
					}
				} else if msgType == 1 {
					// SetColourMapEntries: msgType(1) + pad(1) + firstColour(2) + numColours(2) + colours
					if len(frameBuf) < 8 {
						break
					}
					numColours := int(binary.BigEndian.Uint16(frameBuf[4:6]))
					msgLen = 8 + numColours*6
					if len(frameBuf) < msgLen {
						break
					}
					consumed = true
				} else if msgType == 2 {
					// Bell: 1 byte
					msgLen = 1
					consumed = true
				} else if msgType == 3 {
					// ServerCutText: msgType(1) + pad(3) + length(4) + text
					if len(frameBuf) < 8 {
						break
					}
					length := int(binary.BigEndian.Uint32(frameBuf[4:8]))
					msgLen = 8 + length
					if len(frameBuf) < msgLen {
						break
					}
					consumed = true
				} else {
					// Unknown message type — send 1 byte to avoid getting stuck
					log.Printf("Unknown RFB message type: %d, sending 1 byte", msgType)
					msgLen = 1
					consumed = true
				}

				if consumed {
					msg := frameBuf[:msgLen]

					// With RAW encoding (no ZRLE), the message is already
					// raw pixels — no decompression needed. Forward as-is.
					// (decodeZRLEInPlace is a no-op for non-ZRLE frames.)
					if msgType == 0 {
						decoded := decodeZRLEInPlace(msg, *info)
						if decoded != nil {
							msg = decoded
						}
					}

					if err := wsConn.WriteMessage(websocket.BinaryMessage, msg); err != nil {
						log.Printf("WS write ended: %v", err)
						done <- struct{}{}
						return
					}
					if totalBytes < 5*1024*1024 && msgType == 0 {
						nonZero := 0
						for i := 0; i < len(msg); i++ {
							if msg[i] != 0 {
								nonZero++
							}
						}
						log.Printf("  RFB->WS: %d bytes (total=%d), nonZero=%d, first: %x", len(msg), totalBytes, nonZero, msg[:min(len(msg), 16)])
					}
					frameBuf = frameBuf[msgLen:]
				} else {
					break
				}
			}

			// After each read, request incremental update for next frame
			fbReq[1] = 1 // incremental
			rfbConn.Write(fbReq)
		}
	}()

	// Keepalive: send periodic FramebufferUpdateRequest to prevent VNC server timeout
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		fullTicker := time.NewTicker(30 * time.Second)
		defer fullTicker.Stop()
		for {
			select {
			case <-ticker.C:
				fbReq[1] = 1 // incremental
				if _, err := rfbConn.Write(fbReq); err != nil {
					return
				}
			case <-fullTicker.C:
				// Periodic full (non-incremental) update so new viewers
				// get a fresh ZRLE zlib stream (ZRLE uses a persistent
				// zlib context that can't be replayed from the middle)
				fbReq[1] = 0 // non-incremental (full)
				if _, err := rfbConn.Write(fbReq); err != nil {
					return
				}
				log.Printf("Requested full screen refresh (for new viewers)")
			case <-done:
				return
			}
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
func extractUserID(token string) string {
	parts := strings.Split(token, ".")
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

// isJWTExpired checks if a JWT's exp claim is in the past
func isJWTExpired(token string) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false // can't parse, assume valid
	}
	payload := parts[1]
	for len(payload)%4 != 0 {
		payload += "="
	}
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return false
	}
	var claims map[string]interface{}
	if err := json.Unmarshal(data, &claims); err != nil {
		return false
	}
	exp, ok := claims["exp"].(float64)
	if !ok {
		return false
	}
	return time.Now().Unix() > int64(exp)
}

// loadDeviceTokenFromXochitl reads the UserToken from xochitl.conf
func loadDeviceTokenFromXochitl() string {
	data, err := os.ReadFile("/home/root/.config/remarkable/xochitl.conf")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "UserToken=") {
			val := strings.TrimPrefix(line, "UserToken=")
			val = strings.Trim(val, `"`)
			// xochitl.conf uses @ByteArray(...) wrapper
			val = strings.TrimPrefix(val, "@ByteArray(")
			val = strings.TrimSuffix(val, ")")
			if len(val) > 50 {
				return val
			}
		}
	}
	return ""
}

// generateFreshJWT creates a new device JWT using the PBKDF2-derived signing key.
// This mirrors rmfakecloud's token generation so the server accepts it.
func generateFreshJWT() (string, error) {
	secretKey := os.Getenv("JWT_SECRET_KEY")
	if secretKey == "" {
		secretKey = "Nevadastate01!"
	}

	// Derive signing key: PBKDF2(secretKey, "todo some salt", 10000, SHA256, 32 bytes)
	derivedKey := pbkdf2.Key([]byte(secretKey), []byte("todo some salt"), 10000, 32, sha256.New)

	// Build claims matching what rmfakecloud generates for devices
	now := time.Now().Unix()
	claims := jwt.MapClaims{
		"auth0-userid": "ypyly",
		"auth0-profile": map[string]interface{}{
			"UserID":          "ypyly",
			"IsSocial":        false,
			"Connection":      "Username-Password-Authentication",
			"Name":            "ypyly",
			"Nickname":        "",
			"GivenName":       "",
			"FamilyName":      "",
			"Email":           "ypyly (via https://local.appspot.com)",
			"EmailVerified":   true,
			"CreatedAt":       "2025-03-29T10:59:55.715945208Z",
			"UpdatedAt":       "2025-03-29T10:59:55.715945298Z",
		},
		"device-desc": "remarkable",
		"device-id":    "RM110-219-91826",
		"scopes":       "intgr screenshare doedit sync:tortoisedb",
		"version":      10,
		"level":        "connect",
		"tectonic":     "eu",
		"exp":          now + 86400, // 24 hours
		"jti":          fmt.Sprintf("ck%010d", now),
		"iat":          now,
		"iss":          "rM WebApp",
		"nbf":          now - 300, // -5min skew for clock drift
		"sub":          "ypyly",
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	token.Header["kid"] = "1"
	return token.SignedString(derivedKey)
}