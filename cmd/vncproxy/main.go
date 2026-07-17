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
	meta := `{"type":"meta","width":1404,"height":1872,"bpp":2,"format":"rgb565le","compressed":"lz4"}`
	if err := wsConn.WriteMessage(websocket.TextMessage, []byte(meta)); err != nil {
		log.Fatalf("WS write meta: %v", err)
	}

	// Start restream as subprocess
	// reMarkable 2 firmware ≥3.24: 4bpp BGRA, 1872×1404 (landscape), LZ4 compressed
	// reStream.sh: width=1872, height=1404, bytes_per_pixel=4, pixel_format=bgra
	// transpose=2 (rotate 180°), skip_offset=4705256 (firmware ≥3.27)
	cmd := exec.Command(restreamBin, "-w", "1872", "-h", "1404", "-b", "4", "-f", ":mem:")
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
	frameCount := 0
	totalBytes := 0
	buf := make([]byte, 65536)
	reconnectAttempts := 0

	for {
		n, err := stdout.Read(buf)
		if n > 0 {
			data := buf[:n]
			if err := wsConn.WriteMessage(websocket.BinaryMessage, data); err != nil {
				log.Printf("WS write: %v", err)
				// Try to reconnect
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
			totalBytes += n
			if frameCount%100 == 0 {
				log.Printf("  streamed %d chunks (total=%d bytes)", frameCount, totalBytes)
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