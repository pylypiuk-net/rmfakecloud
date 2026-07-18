// fbproxy: Framebuffer proxy for reMarkable screen sharing.
// Runs ON the tablet. Uses the Toltec `restream` binary to capture xochitl's
// framebuffer from process memory (:mem: mode via ptrace), then pipes the
// LZ4-compressed stream to rmfakecloud's WebSocket endpoint.
//
// The web UI viewer decompresses the LZ4 frames and renders RGB565 pixels.
package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"crypto/sha256"
	"golang.org/x/crypto/pbkdf2"
	"github.com/golang-jwt/jwt/v4"
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

	// If the token from xochitl.conf is expired, generate a fresh JWT.
	// The JWT signing key is PBKDF2(rawSecret, "todo some salt", 10000, 32, sha256).
	secretStr := os.Getenv("JWT_SECRET_KEY")
	if secretStr == "" {
		secretStr = "Nevadastate01!" // default for our deployment
	}

	// Try to use the token from xochitl.conf, but if it's expired, generate a fresh one
	if deviceToken == "" {
		deviceToken = readTokenFromConfig()
	}

	// Check if the token is expired; if so, generate a fresh one
	if isTokenExpired(deviceToken) && secretStr != "" {
		log.Printf("Token from config is expired, generating fresh JWT")
		// Read device info from xochitl.conf
		deviceID, deviceDesc := readDeviceInfo()
		freshToken, err := generateJWT(secretStr, deviceID, deviceDesc)
		if err != nil {
			log.Printf("Failed to generate JWT: %v", err)
		} else {
			deviceToken = freshToken
			log.Printf("Generated fresh JWT (len=%d)", len(freshToken))
		}
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
	// IMPORTANT: restream outputs LZ4 frames (magic 04 22 4D 18) when writing to a file/FIFO,
	// but raw data WITHOUT framing when writing to a pipe (stdout pipe). To get LZ4 frames,
	// we redirect restream's stdout to a named pipe (FIFO) and read from it.
	fifoPath := "/tmp/restream_fifo"
	os.Remove(fifoPath)
	if err := syscall.Mkfifo(fifoPath, 0644); err != nil {
		log.Fatalf("create FIFO: %v", err)
	}
	defer os.Remove(fifoPath)

	cmd := exec.Command(restreamBin, "-w", "1872", "-h", "1404", "-b", "1", "-s", "8", "-f", ":mem:")
	cmd.Env = []string{"PATH=/opt/bin:/usr/bin:/bin"}

	// Open the FIFO. On Linux, O_RDONLY blocks until O_WRONLY opens and vice versa,
	// so we open both in goroutines. Must use O_WRONLY (not O_RDWR) so restream
	// detects a proper FIFO and outputs LZ4 frames.
	type fifoResult struct {
		f   *os.File
		err error
	}
	writeCh := make(chan fifoResult, 1)
	readCh := make(chan fifoResult, 1)
	go func() {
		fw, err := os.OpenFile(fifoPath, os.O_WRONLY, 0)
		writeCh <- fifoResult{fw, err}
	}()
	go func() {
		fr, err := os.Open(fifoPath)
		readCh <- fifoResult{fr, err}
	}()

	wr := <-writeCh
	if wr.err != nil {
		log.Fatalf("open FIFO for writing: %v", wr.err)
	}
	defer wr.f.Close()
	cmd.Stdout = wr.f

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

	// Tail the restream output file and forward data via WebSocket.
	// restream writes LZ4 frames (with magic 04 22 4D 18) to the file.
	// We forward chunks; the viewer reassembles LZ4 frames.
	frameCount := 0
	totalBytes := 0

	// Wait for the read end of the FIFO to open
	rr := <-readCh
	if rr.err != nil {
		log.Fatalf("open FIFO for reading: %v", rr.err)
	}
	defer rr.f.Close()

	buf := make([]byte, 65536)

	for {
		n, err := rr.f.Read(buf)
		if n > 0 {
			data := buf[:n]
			if wsErr := wsConn.WriteMessage(websocket.BinaryMessage, data); wsErr != nil {
				log.Printf("WS write: %v", wsErr)
				break
			}
			frameCount++
			totalBytes += n
			if frameCount%100 == 0 {
				log.Printf("  streamed %d chunks (total=%d bytes)", frameCount, totalBytes)
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

// isTokenExpired checks if a JWT token's exp claim is in the past.
func isTokenExpired(token string) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return true
	}
	// Decode the payload (base64url, no padding)
	payload := parts[1]
	// Add padding if needed
	for len(payload)%4 != 0 {
		payload += "="
	}
	decoded, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		return true
	}
	// Parse JSON to get exp
	var claims map[string]interface{}
	if err := json.Unmarshal(decoded, &claims); err != nil {
		return true
	}
	exp, ok := claims["exp"].(float64)
	if !ok {
		return true
	}
	return time.Now().Unix() > int64(exp)
}

// readDeviceInfo reads the device ID and description from xochitl.conf
func readDeviceInfo() (string, string) {
	paths := []string{
		"/home/root/.config/remarkable/xochitl.conf",
		"/home/root/.xochitl.conf",
	}
	deviceID := ""
	deviceDesc := "remarkable"
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			if strings.Contains(line, "DeviceID=") || strings.Contains(line, "device-id=") {
				parts := strings.SplitN(line, "=", 2)
				if len(parts) == 2 {
					deviceID = strings.Trim(strings.TrimSpace(parts[1]), "@ByteArray()")
				}
			}
		}
	}
	if deviceID == "" {
		deviceID = "RM110-219-91826"
	}
	return deviceID, deviceDesc
}

// generateJWT creates a fresh JWT for device authentication
func generateJWT(secret, deviceID, deviceDesc string) (string, error) {
	// Derive signing key using PBKDF2 (same as rmfakecloud)
	dk := pbkdf2.Key([]byte(secret), []byte("todo some salt"), 10000, 32, sha256.New)

	// Create claims matching the device token format
	now := time.Now()
	claims := jwt.MapClaims{
		"auth0-user-id": "ypyly",
		"auth0-profile": map[string]interface{}{
			"UserID":      "ypyly",
			"IsSocial":    false,
			"Connection":  "Username-Password-Authentication",
			"Name":        "ypyly",
			"Nickname":    "",
			"GivenName":   "",
			"FamilyName":  "",
			"Email":       "ypyly (via https://local.appspot.com)",
			"EmailVerified": true,
			"CreatedAt":   "2025-03-29T10:59:55.715945208Z",
			"UpdatedAt":   "2025-03-29T10:59:55.715945298Z",
		},
		"device-desc":  deviceDesc,
		"device-id":    deviceID,
		"scopes":       "intgr screenshare docedit sync:tortoises",
		"version":      10,
		"level":        "connect",
		"telectonic":   "eu",
		"exp":          now.Add(24 * time.Hour).Unix(),
		"jti":          fmt.Sprintf("%d", now.UnixNano()),
		"iat":          now.Unix(),
		"iss":          "rM WebApp",
		"nbf":          now.Add(-5 * time.Minute).Unix(), // skew tolerance
		"sub":          "ypyly",
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	token.Header["kid"] = "1"
	return token.SignedString(dk)
}

// Unused but kept for reference
var _ = strconv.Itoa