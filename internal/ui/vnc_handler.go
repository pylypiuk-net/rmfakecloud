package ui

import (
	"net/http"
	"strings"
	"sync"

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
var vnHub = &VNCHub{
	broadcast: make(chan []byte, 4096),
	register:  make(chan *websocket.Conn, 1),
}

type VNCHub struct {
	mu        sync.Mutex
	clients   map[*websocket.Conn]bool
	broadcast chan []byte
	register  chan *websocket.Conn
}

func (h *VNCHub) run() {
	h.clients = make(map[*websocket.Conn]bool)
	for {
		select {
		case data := <-h.broadcast:
			h.mu.Lock()
			for client := range h.clients {
				err := client.WriteMessage(websocket.BinaryMessage, data)
				if err != nil {
					delete(h.clients, client)
					client.Close()
				}
			}
			h.mu.Unlock()
		case client := <-h.register:
			h.mu.Lock()
			h.clients[client] = true
			h.mu.Unlock()
		}
	}
}

func init() {
	go vnHub.run()
}

// vncStreamHandler: web UI connects here via WebSocket to receive VNC frames
func (app *ReactAppWrapper) vncStreamHandler(c *gin.Context) {
	ws, err := upgraderVNC.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("VNC stream upgrade: %v", err)
		return
	}
	defer ws.Close()

	log.Info("VNC: web UI client connected")
	vnHub.register <- ws

	// Keep connection alive, read any input events from web UI
	for {
		_, _, err := ws.ReadMessage()
		if err != nil {
			break
		}
	}
	log.Info("VNC: web UI client disconnected")
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
				vnHub.broadcast <- data
			}
	}

	log.Info("[vnc-proxy] tablet proxy disconnected")
}