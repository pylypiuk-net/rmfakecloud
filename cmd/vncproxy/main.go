// fbproxy: Framebuffer proxy for reMarkable screen sharing.
// Runs ON the tablet. Reads xochitl's framebuffer directly from process
// memory (/proc/<pid>/maps), converts from BGRA to grayscale, and sends
// raw frames to rmfakecloud over WebSocket.
//
// This replaces both the VNC proxy (sends blank frames) and the restream
// binary (wrong pixel format for firmware 3.27+).
//
// For rM2 firmware 3.27+:
//   - Framebuffer is 4 bytes per pixel (BGRA)
//   - Width=1872, Height=1404 (landscape), needs 90° rotation
//   - Skip offset: 4705256 bytes from the start of the fb0 mapping
//   - Actual display: 1404x1872 (portrait after rotation)
package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

const (
	// Display dimensions (portrait)
	displayWidth  = 1404
	displayHeight = 1872
	// Framebuffer dimensions (landscape, before rotation)
	fbWidth  = 1872
	fbHeight = 1404
	fbBPP    = 4 // bytes per pixel (BGRA)
	fbStride = fbWidth * fbBPP
	// Skip offset for firmware 3.27+ (from reStream.sh)
	skipOffset = 4705256
	// Frame size in bytes (landscape, 4bpp)
	fbFrameSize = fbWidth * fbHeight * fbBPP
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

	// Find xochitl PID
	xochitlPID, err := findXochitlPID()
	if err != nil {
		log.Fatalf("find xochitl: %v", err)
	}
	log.Printf("xochitl PID: %d", xochitlPID)

	// Find /dev/fb0 memory mapping
	fbAddr, fbSize, err := findFBMapping(xochitlPID)
	if err != nil {
		log.Fatalf("find fb0 mapping: %v", err)
	}
	log.Printf("fb0 mapping: addr=0x%x size=%d", fbAddr, fbSize)

	// Open /proc/<pid>/mem
	memPath := fmt.Sprintf("/proc/%d/mem", xochitlPID)
	memFile, err := os.Open(memPath)
	if err != nil {
		log.Fatalf("open %s: %v", memPath, err)
	}
	defer memFile.Close()

	// Connect to rmfakecloud via WebSocket
	wsURL := fmt.Sprintf("ws://%s:%s/ui/api/screenshare/vnc/connect?token=%s", serverHost, serverPort, deviceToken)
	wsConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		log.Fatalf("WS connect: %v", err)
	}
	defer wsConn.Close()
	log.Printf("WS connected to %s", wsURL)

	// Send metadata
	meta := fmt.Sprintf(`{"type":"meta","width":%d,"height":%d,"bpp":1,"format":"gray8","compressed":"none"}`,
		displayWidth, displayHeight)
	wsConn.WriteMessage(websocket.TextMessage, []byte(meta))

	// Read frames: skip offset, read fbFrameSize, convert BGRA→grayscale, rotate, send
	frameCount := 0
	totalBytes := 0

	// Allocate buffers once
	fbBuf := make([]byte, fbFrameSize)
	// Grayscale frame (rotated to portrait): displayWidth × displayHeight
	grayFrame := make([]byte, displayWidth*displayHeight)

	for {
		// Seek to fb0 + skipOffset
		_, err := memFile.Seek(int64(fbAddr)+int64(skipOffset), 0)
		if err != nil {
			log.Printf("seek: %v", err)
			break
		}

		// Read full frame
		_, err = io.ReadFull(memFile, fbBuf)
		if err != nil {
			log.Printf("read frame: %v", err)
			break
		}

		// Convert BGRA landscape → grayscale portrait (90° rotation)
		// Source: fbBuf[fbHeight][fbWidth] in BGRA format
		// We only need the R channel (byte index 2 in each 4-byte pixel)
		// Rotate 90° clockwise: (x,y) → (height-1-y, x) for landscape→portrait
		for y := 0; y < fbHeight; y++ {
			for x := 0; x < fbWidth; x++ {
				// Source pixel (x, y) in landscape BGRA
				srcIdx := (y*fbWidth + x) * fbBPP
				// R channel (byte 2 in BGRA)
				gray := fbBuf[srcIdx+2]

				// Rotate 90° clockwise to portrait
				// (x, y) → (y, fbHeight-1-x)
				// But we want portrait (displayWidth × displayHeight) = (1404 × 1872)
				// Landscape is (1872 × 1404) = (fbWidth × fbHeight)
				// After 90° CW rotation: portrait width = fbHeight = 1404, portrait height = fbWidth = 1872
				dstX := fbHeight - 1 - y // 0..1403
				dstY := x               // 0..1871
				dstIdx := dstY*displayWidth + dstX
				grayFrame[dstIdx] = gray
			}
		}

		// Send frame
		if err := wsConn.WriteMessage(websocket.BinaryMessage, grayFrame); err != nil {
			log.Printf("WS write: %v", err)
			break
		}
		frameCount++
		totalBytes += len(grayFrame)
		if frameCount%10 == 0 {
			log.Printf("  frame %d (total=%d bytes)", frameCount, totalBytes)
		}

		// Frame rate: ~10fps (100ms per frame)
		// E-ink refreshes slowly, so 10fps is sufficient
		time.Sleep(100 * time.Millisecond)
	}

	log.Printf("Stream ended: %d frames, %d bytes", frameCount, totalBytes)
}

// findXochitlPID finds the PID of the xochitl process
func findXochitlPID() (int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err != nil {
			continue
		}
		if strings.Contains(string(cmdline), "xochitl") {
			return pid, nil
		}
	}
	return 0, fmt.Errorf("xochitl process not found")
}

// findFBMapping finds the /dev/fb0 memory mapping in xochitl's address space
func findFBMapping(pid int) (addr uint64, size uint64, err error) {
	mapsPath := fmt.Sprintf("/proc/%d/maps", pid)
	f, err := os.Open(mapsPath)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.Contains(line, "/dev/fb0") {
			continue
		}
		// Parse: start-end perm offset dev inode pathname
		// Example: "b6c00000-b6e00000 rw-s 00000000 00:0c 3 /dev/fb0"
		parts := strings.Fields(line)
		addrRange := parts[0]
		addrParts := strings.Split(addrRange, "-")
		if len(addrParts) != 2 {
			continue
		}
		start, err1 := strconv.ParseUint(addrParts[0], 16, 64)
		end, err2 := strconv.ParseUint(addrParts[1], 16, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		return start, end - start, nil
	}

	return 0, 0, fmt.Errorf("/dev/fb0 not found in /proc/%d/maps", pid)
}