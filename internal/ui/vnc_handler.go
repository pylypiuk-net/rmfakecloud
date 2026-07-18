package ui

import (
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ddvk/rmfakecloud/internal/common"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v4"
	"github.com/gorilla/websocket"
	log "github.com/sirupsen/logrus"
)

// deviceTokenClaims matches the JWT structure used by the tablet's UserToken.
type deviceTokenClaims struct {
	UserID   string `json:"auth0-userid"`
	DeviceID string `json:"device-id"`
	Scopes   string `json:"scopes,omitempty"`
	jwt.StandardClaims
}

var upgraderVNC = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

// vnHub bridges the tablet VNC proxy to web UI viewers.
var vnHub = newVNCHub()

type VNCHub struct {
	mu        sync.Mutex
	clients   map[*websocket.Conn]bool
	broadcast chan []byte
	register  chan *websocket.Conn
	// lastFrame caches the most recent frame data so newly connected
	// viewers immediately see the current screen instead of waiting
	// for the next change.
	lastFrame []byte
	// lastMeta caches the RFBF meta (pixel format) — sent first to
	// every new client so the viewer knows how to decode frames.
	lastMeta []byte
	// stats
	totalBytes  uint64
	totalFrames uint64
}

func newVNCHub() *VNCHub {
	h := &VNCHub{
		clients:   make(map[*websocket.Conn]bool),
		broadcast: make(chan []byte, 4096),
		register:  make(chan *websocket.Conn, 16),
	}
	go h.run()
	return h
}

// isRFBFMeta returns true if data is the 24-byte RFBF pixel-format header.
func isRFBFMeta(data []byte) bool {
	return len(data) == 24 && data[0] == 'R' && data[1] == 'F' && data[2] == 'B' && data[3] == 'F'
}

// run uses two separate goroutines so broadcast traffic can never
// starve client registration (and vice versa).
func (h *VNCHub) run() {
	// Registration goroutine — never blocked by broadcast traffic.
	// Sends cached meta + last frame to the new client immediately.
	go func() {
		for client := range h.register {
			h.mu.Lock()
			h.clients[client] = true
			n := len(h.clients)
			// Replay cached meta + last frame so the viewer sees the
			// current screen without waiting for the next change.
			if h.lastMeta != nil {
				client.WriteMessage(websocket.BinaryMessage, h.lastMeta)
			}
			if h.lastFrame != nil {
				client.WriteMessage(websocket.BinaryMessage, h.lastFrame)
			}
			h.mu.Unlock()
			log.Infof("[vnc-hub] client registered, total: %d (replayed %d meta + %d frame bytes)",
				n, len(h.lastMeta), len(h.lastFrame))
		}
	}()

	// Broadcast loop — fans out to all registered clients.
	for data := range h.broadcast {
		atomic.AddUint64(&h.totalBytes, uint64(len(data)))
		atomic.AddUint64(&h.totalFrames, 1)

		// Cache meta and full frames for late joiners.
		if isRFBFMeta(data) {
			h.mu.Lock()
			h.lastMeta = data
			h.mu.Unlock()
		} else if len(data) > 100 {
			// Each WebSocket message from the proxy is a complete RFB
			// FramebufferUpdate. Cache the largest frame seen — that's
			// the full-screen update. Incremental updates are small and
			// meaningless without the base frame.
			h.mu.Lock()
			if h.lastFrame == nil || len(data) > len(h.lastFrame) {
				h.lastFrame = data
			}
			h.mu.Unlock()
		}

		h.mu.Lock()
		for client := range h.clients {
			if err := client.WriteMessage(websocket.BinaryMessage, data); err != nil {
				delete(h.clients, client)
				client.Close()
			}
		}
		n := len(h.clients)
		h.mu.Unlock()

		// Log every 500th frame to avoid spam.
		frames := atomic.LoadUint64(&h.totalFrames)
		if frames%500 == 0 {
			bytes := atomic.LoadUint64(&h.totalBytes)
			log.Infof("[vnc-hub] frame %d, %d bytes total, %d clients", frames, bytes, n)
		}
	}
}

// registerClient adds a web UI viewer to the hub.
func (h *VNCHub) registerClient(ws *websocket.Conn) {
	h.register <- ws
}

// push broadcasts RFB data from the tablet proxy to all viewers.
func (h *VNCHub) push(data []byte) {
	select {
	case h.broadcast <- data:
	default:
		// Channel full — drop frame to avoid blocking the proxy.
		log.Warn("[vnc-hub] broadcast channel full, dropping frame")
	}
}

// vncStreamHandler: web UI connects here via WebSocket to receive VNC frames
func (app *ReactAppWrapper) vncStreamHandler(c *gin.Context) {
	ws, err := upgraderVNC.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("VNC stream upgrade: %v", err)
		return
	}
	defer ws.Close()

	// Set ping/pong deadlines to keep the connection alive through
	// proxies/load-balancers that idle-timeout after ~30-60s.
	ws.SetReadDeadline(time.Now().Add(90 * time.Second))
	ws.SetPingHandler(func(app string) error {
		ws.SetReadDeadline(time.Now().Add(90 * time.Second))
		return ws.WriteMessage(websocket.PongMessage, []byte(app))
	})
	ws.SetPongHandler(func(app string) error {
		ws.SetReadDeadline(time.Now().Add(90 * time.Second))
		return nil
	})

	log.Info("VNC: web UI client connected")
	vnHub.registerClient(ws)

	// Periodically ping the client to keep the connection alive.
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			if err := ws.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}()

	// Keep connection alive, read any input events from web UI
	for {
		_, _, err := ws.ReadMessage()
		if err != nil {
			break
		}
	}
	log.Info("VNC: web UI client disconnected")

	// Remove from hub
	vnHub.mu.Lock()
	delete(vnHub.clients, ws)
	vnHub.mu.Unlock()
}

// vncProxyConnectHandler: tablet proxy connects here to push RFB data.
// Uses device token auth (screenshare scope) instead of web JWT.
func (app *ReactAppWrapper) vncProxyConnectHandler(c *gin.Context) {
	// Auth: device token via query param or Authorization header
	token, err := common.GetToken(c)
	if err != nil {
		log.Warn("[vnc-proxy] no token: ", err)
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}

	// Validate token — device tokens use the same JWT secret
	claims := &deviceTokenClaims{}
	err = common.ClaimsFromToken(claims, token, app.cfg.JWTSecretKey)
	if err != nil {
		log.Warn("[vnc-proxy] token invalid: ", err)
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}

	// Check screenshare scope
	scopes := strings.Fields(claims.Scopes)
	hasScreenShare := false
	for _, s := range scopes {
		if s == "screenshare" {
			hasScreenShare = true
			break
		}
	}
	if !hasScreenShare {
		log.Warn("[vnc-proxy] no screenshare scope, scopes: ", claims.Scopes)
		c.AbortWithStatus(http.StatusForbidden)
		return
	}

	log.Infof("[vnc-proxy] tablet proxy connected, user: %s, device: %s", claims.UserID, claims.DeviceID)

	ws, err := upgraderVNC.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("[vnc-proxy] upgrade: %v", err)
		return
	}
	defer ws.Close()

	// Read RFB data from proxy and broadcast to web UI clients
	for {
		msgType, data, err := ws.ReadMessage()
		if err != nil {
			log.Printf("[vnc-proxy] read error: %v", err)
			break
		}
		if msgType == websocket.BinaryMessage {
			vnHub.push(data)
		}
	}

	log.Info("[vnc-proxy] tablet proxy disconnected")
}