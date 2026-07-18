// fbproxy: Framebuffer proxy for reMarkable screen sharing.
// Runs ON the tablet. Uses the Toltec `restream` binary to capture xochitl's
// framebuffer from process memory (:mem: mode via ptrace), then pipes the
// LZ4-compressed stream to rmfakecloud's WebSocket endpoint.
//
// The web UI viewer decompresses the LZ4 frames and renders RGB565 pixels.
package main

import (
	"bufio"
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
	//
	// IMPORTANT: restream outputs LZ4 frames (magic 04 22 4D 18) when writing to a file,
	// but raw data WITHOUT framing when writing to a pipe (stdout pipe). To get LZ4 frames,
	// we redirect restream's stdout to a temp file and tail it.
	cmd := exec.Command(restreamBin, "-w", "1872", "-h", "1404", "-b", "1", "-s", "8", "-f", ":mem:")
	cmd.Env = []string{"PATH=/opt/bin:/usr/bin:/bin"}

	// Use a temp file for restream output (restream outputs LZ4 frames to files, not pipes)
	tmpFile := "/tmp/restream_output.raw"
	outFile, err := os.Create(tmpFile)
	if err != nil {
		log.Fatalf("create temp file: %v", err)
	}
	defer outFile.Close()
	cmd.Stdout = outFile
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

	// Tail the restream output file and forward LZ4 frames via WebSocket.
	// restream writes LZ4 frames (with magic 04 22 4D 18) to the file.
	// We read complete frames and forward them so the viewer can decompress each one.
	frameCount := 0
	totalBytes := 0

	// Open the file for reading after restream starts writing to it
	time.Sleep(100 * time.Millisecond) // give restream time to start
	f, err := os.Open(tmpFile)
	if err != nil {
		log.Fatalf("open temp file for reading: %v", err)
	}
	defer f.Close()

	buf := make([]byte, 65536)
	var accum []byte

	for {
		n, err := f.Read(buf)
		if n > 0 {
			accum = append(accum, buf[:n]...)

			// Forward complete LZ4 frames (delimited by magic 04 22 4D 18)
			for {
				if len(accum) < 8 {
					break
				}
				// Check if accum starts with LZ4 magic
				if !(accum[0] == 0x04 && accum[1] == 0x22 && accum[2] == 0x4D && accum[3] == 0x18) {
					// Find magic in buffer
					found := false
					for i := 1; i < len(accum)-3; i++ {
						if accum[i] == 0x04 && accum[i+1] == 0x22 && accum[i+2] == 0x4D && accum[i+3] == 0x18 {
							accum = accum[i:]
							found = true
							break
						}
					}
					if !found {
						// Keep last 4 bytes (magic may span reads)
						if len(accum) > 4 {
							accum = accum[len(accum)-4:]
						}
						break
					}
				}

				// Find next magic
				nextMagic := -1
				for i := 4; i < len(accum)-3; i++ {
					if accum[i] == 0x04 && accum[i+1] == 0x22 && accum[i+2] == 0x4D && accum[i+3] == 0x18 {
						nextMagic = i
						break
					}
				}

				if nextMagic < 0 {
					// No next frame yet — wait for more data
					// But if we have >5MB, send it as one frame
					if len(accum) < 5*1024*1024 {
						break
					}
					// Send as one frame
					frameData := make([]byte, len(accum))
					copy(frameData, accum)
					accum = accum[:0]
					if wsErr := wsConn.WriteMessage(websocket.BinaryMessage, frameData); wsErr != nil {
						log.Printf("WS write: %v", wsErr)
						break
					}
					frameCount++
					totalBytes += len(frameData)
					if frameCount%10 == 0 {
						log.Printf("  streamed %d frames (total=%d bytes)", frameCount, totalBytes)
					}
					break
				}

				// Send complete frame
				frameData := make([]byte, nextMagic)
				copy(frameData, accum[:nextMagic])
				accum = accum[nextMagic:]
				if wsErr := wsConn.WriteMessage(websocket.BinaryMessage, frameData); wsErr != nil {
					log.Printf("WS write: %v", wsErr)
					break
				}
				frameCount++
				totalBytes += len(frameData)
				if frameCount%10 == 0 {
					log.Printf("  streamed %d frames (total=%d bytes)", frameCount, totalBytes)
				}
			}
		}
		if err == io.EOF {
			// File is still being written — wait and retry
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if err != nil {
			log.Printf("file read: %v", err)
			break
		}
	}

	cmd.Wait()
	log.Printf("Stream ended: %d frames, %d bytes", frameCount, totalBytes)
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