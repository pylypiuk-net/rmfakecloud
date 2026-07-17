// fbproxy: Framebuffer proxy for reMarkable screen sharing.
// Runs ON the tablet. Uses the Toltec `restream` binary to capture xochitl's
// framebuffer from process memory (:mem: mode), then pipes the LZ4-compressed
// stream to rmfakecloud over WebSocket for the web UI viewer.
//
// The native VNC server sends blank frames — this approach reads the actual
// composited framebuffer from xochitl's /proc/<pid>/maps memory mapping.
//
// Frame format: 1404x1872, 2 bytes per pixel, RGB565 LE.
// restream outputs an LZ4 frame stream.
package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"

	"github.com/gorilla/websocket"
)

const (
	frameWidth  = 1404
	frameHeight = 1872
	frameBPP    = 2
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
	deviceToken := os.Getenv("DEVICE_TOKEN")
	if deviceToken == "" && len(os.Args) > 1 {
		deviceToken = os.Args[1]
	}

	log.Printf("fbproxy starting: server=%s:%s", serverHost, serverPort)

	// Connect to rmfakecloud via WebSocket
	wsURL := fmt.Sprintf("ws://%s:%s/ui/api/screenshare/vnc/connect?token=%s", serverHost, serverPort, deviceToken)
	wsConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		log.Fatalf("WS connect to %s: %v", wsURL, err)
	}
	defer wsConn.Close()
	log.Printf("WS connected to %s", wsURL)

	// Start restream binary
	restreamPath := "/opt/bin/restream"
	if _, err := os.Stat(restreamPath); err != nil {
		restreamPath = "restream"
	}

	cmd := exec.Command(restreamPath,
		"--width", fmt.Sprintf("%d", frameWidth),
		"--height", fmt.Sprintf("%d", frameHeight),
		"--bytes-per-pixel", fmt.Sprintf("%d", frameBPP),
		"--file", ":mem:",
	)
	cmd.Stderr = os.Stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		log.Fatalf("start restream: %v", err)
	}
	log.Printf("restream started (pid=%d)", cmd.Process.Pid)
	defer cmd.Process.Kill()

	// Send initial metadata so the web UI knows the frame format
	meta := fmt.Sprintf(`{"type":"meta","width":%d,"height":%d,"bpp":%d,"format":"rgb565le","compressed":"lz4"}`,
		frameWidth, frameHeight, frameBPP)
	wsConn.WriteMessage(websocket.TextMessage, []byte(meta))

	// Pipe restream stdout (LZ4 compressed) → WebSocket (binary messages)
	totalBytes := 0
	buf := make([]byte, 65536)
	for {
		n, err := stdout.Read(buf)
		if err != nil {
			if err == io.EOF {
				log.Printf("restream closed (total=%d bytes)", totalBytes)
			} else {
				log.Printf("restream read: %v", err)
			}
			break
		}
		totalBytes += n
		if err := wsConn.WriteMessage(websocket.BinaryMessage, buf[:n]); err != nil {
			log.Printf("WS write: %v", err)
			break
		}
		if totalBytes < 1000 || totalBytes%1000000 < 65536 {
			log.Printf("  streamed %d bytes (total=%d)", n, totalBytes)
		}
	}

	log.Printf("Stream ended (total=%d bytes)", totalBytes)
}