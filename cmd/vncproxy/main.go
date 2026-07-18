// fbproxy: Framebuffer proxy for reMarkable screen sharing.
// Runs ON the tablet. Uses the Toltec `restream` binary to capture xochitl's
// framebuffer from process memory (:mem: mode via ptrace), then pipes the
// LZ4-compressed stream to rmfakecloud's WebSocket endpoint.
//
// The web UI viewer decompresses the LZ4 frames and renders RGB565 pixels.
package main

import (
	"bufio"
	"bytes"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

const (
	displayWidth  = 1404
	displayHeight = 1872
	frameSize     = displayWidth * displayHeight * 2 // RGB565, 2 bpp
)

func main() {
	serverHost := os.Getenv("SERVER_HOST")
	if serverHost == "" {
		serverHost = "172.21.1.1"
	}
	serverPort := os.Getenv("SERVER_PORT")
	if serverPort == "" {
		serverPort = "3000"
	}
	restreamBin := os.Getenv("RESTREAM_BIN")
	if restreamBin == "" {
		restreamBin = "/opt/bin/restream"
	}

	deviceToken := os.Getenv("DEVICE_TOKEN")
	if deviceToken == "" && len(os.Args) > 1 {
		deviceToken = os.Args[1]
	}
	if deviceToken == "" {
		// Try to read from xochitl config
		deviceToken = readTokenFromConfig()
	}
	if deviceToken == "" {
		log.Fatal("no device token provided (set DEVICE_TOKEN or pass as arg)")
	}

	log.Printf("fbproxy starting: server=%s:%s restream=%s", serverHost, serverPort, restreamBin)

	// Connect to rmfakecloud via WebSocket
	wsURL := "ws://" + net.JoinHostPort(serverHost, serverPort) + "/ui/api/screenshare/vnc/connect?token=" + deviceToken
	wsConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		log.Fatalf("WS connect: %v", err)
	}
	defer wsConn.Close()
	log.Printf("WS connected to %s", wsURL)

	// Send metadata
	meta := `{"type":"meta","width":1872,"height":1404,"bpp":1,"format":"gray8","compressed":"lz4"}`
	if err := wsConn.WriteMessage(websocket.TextMessage, []byte(meta)); err != nil {
		log.Fatalf("WS write meta: %v", err)
	}

	// Start restream as subprocess
	// reMarkable 2 firmware 3.3.2: 1bpp gray8, 1872×1404 (landscape), LZ4 compressed
	// The 4bpp mode fails (anonymous mapping after fb0 is only 8MB, not enough for 10.5MB frame)
	// The 2bpp mode produces tiled output (bytes are duplicated — correlation 1.0 between high/low)
	// 1bpp with skip=8 produces clean frames. Use restream v1.5.0 (has -s flag).
	cmd := exec.Command(restreamBin, "-w", "1872", "-h", "1404", "-b", "1", "-s", "8", "-f", ":mem:")
	cmd.Env = []string{"PATH=/opt/bin:/usr/bin:/bin"}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Fatalf("restream stdout pipe: %v", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		log.Fatalf("restream stderr pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		log.Fatalf("start restream: %v", err)
	}
	log.Printf("restream started (pid=%d)", cmd.Process.Pid)

	// Log restream stderr
	go func() {
		r := bufio.NewReader(stderr)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			log.Printf("[restream] %s", strings.TrimSpace(line))
		}
	}()

	// Pipe restream stdout → WebSocket
	// restream outputs LZ4 frames (magic: 04 22 4D 18). We buffer the stdout
	// and only forward complete LZ4 frames so the viewer can decompress each
	// WebSocket message independently.
	frameCount := 0
	totalBytes := 0
	readBuf := make([]byte, 65536)
	var accum []byte // accumulated data from restream
	reconnectAttempts := 0

	// LZ4 frame magic
	lz4Magic := []byte{0x04, 0x22, 0x4D, 0x18}

	for {
		n, err := stdout.Read(readBuf)
		if n > 0 {
			accum = append(accum, readBuf[:n]...)

			// Extract complete LZ4 frames from the accumulated buffer.
			// A complete frame is from one magic to the next magic (or end).
			for {
				// Find first magic
				firstMagic := bytes.Index(accum, lz4Magic)
				if firstMagic < 0 {
					// No magic yet — discard data before a reasonable point
					// Keep last 4 bytes in case magic spans a chunk boundary
					if len(accum) > 4 {
						accum = accum[len(accum)-4:]
					}
					break
				}

				// Discard any data before the first magic
				if firstMagic > 0 {
					accum = accum[firstMagic:]
				}

				// Find next magic (end of current frame)
				nextMagic := bytes.Index(accum[4:], lz4Magic)
				if nextMagic < 0 {
					// No next magic — wait for more data
					// But if we have a lot of data, try sending it as one frame
					if len(accum) < 3*1024*1024 {
						break
					}
					// Enough data — send as one frame
					frameData := make([]byte, len(accum))
					copy(frameData, accum)
					accum = accum[:0]

					if wsErr := wsConn.WriteMessage(websocket.BinaryMessage, frameData); wsErr != nil {
						log.Printf("WS write: %v", wsErr)
						wsConn.Close()
						for reconnectAttempts < 3 {
							reconnectAttempts++
							log.Printf("Reconnecting to WS (attempt %d)...", reconnectAttempts)
							time.Sleep(2 * time.Second)
							wsConn, _, err = websocket.DefaultDialer.Dial(wsURL, nil)
							if err != nil {
								log.Printf("Reconnect failed: %v", err)
								continue
							}
							wsConn.WriteMessage(websocket.TextMessage, []byte(meta))
							reconnectAttempts = 0
							break
						}
						if reconnectAttempts >= 3 {
							log.Fatal("Max reconnect attempts reached")
						}
						break
					}
					frameCount++
					totalBytes += len(frameData)
					if frameCount%10 == 0 {
						log.Printf("  streamed %d frames (total=%d bytes)", frameCount, totalBytes)
					}
					break
				}

				// We have a complete frame from 0 to nextMagic+4
				endIdx := nextMagic + 4
				frameData := make([]byte, endIdx)
				copy(frameData, accum[:endIdx])
				accum = accum[endIdx:]

				if wsErr := wsConn.WriteMessage(websocket.BinaryMessage, frameData); wsErr != nil {
					log.Printf("WS write: %v", wsErr)
					wsConn.Close()
					for reconnectAttempts < 3 {
						reconnectAttempts++
						log.Printf("Reconnecting to WS (attempt %d)...", reconnectAttempts)
						time.Sleep(2 * time.Second)
						wsConn, _, err = websocket.DefaultDialer.Dial(wsURL, nil)
						if err != nil {
							log.Printf("Reconnect failed: %v", err)
							continue
						}
						wsConn.WriteMessage(websocket.TextMessage, []byte(meta))
						reconnectAttempts = 0
						break
					}
					if reconnectAttempts >= 3 {
						log.Fatal("Max reconnect attempts reached")
					}
					continue
				}
				frameCount++
				totalBytes += len(frameData)
				if frameCount%10 == 0 {
					log.Printf("  streamed %d frames (total=%d bytes)", frameCount, totalBytes)
				}
			}
		}
		if err == io.EOF {
			log.Printf("restream stdout closed")
			break
		}
		if err != nil {
			log.Printf("restream read: %v", err)
			break
		}
	}

	cmd.Wait()
	log.Printf("Stream ended: %d chunks, %d bytes", frameCount, totalBytes)
}

func readTokenFromConfig() string {
	paths := []string{
		"/home/root/.config/remarkable/xochitl.conf",
		"/home/root/.xochitl.conf",
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		lines := strings.Split(string(data), "\n")
		for _, line := range lines {
			if strings.Contains(line, "UserToken") {
				// Format: UserToken=@ByteArray(eyJ...)
				start := strings.Index(line, "@ByteArray(")
				if start >= 0 {
					token := line[start+11:]
					end := strings.Index(token, ")")
					if end >= 0 {
						token = token[:end]
					}
					if len(token) > 20 {
						log.Printf("Read token from %s (len=%d)", path, len(token))
						return token
					}
				}
				// Try without @ByteArray wrapper
				parts := strings.SplitN(line, "=", 2)
				if len(parts) == 2 {
					token := strings.TrimSpace(parts[1])
					token = strings.Trim(token, "@ByteArray()")
					if len(token) > 20 {
						return token
					}
				}
			}
		}
	}
	return ""
}

// Unused but kept for reference
var _ = strconv.Itoa