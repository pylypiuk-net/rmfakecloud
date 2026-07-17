package ui

import (
	"net/http"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	log "github.com/sirupsen/logrus"
)

var upgraderVNC = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

// vnHub bridges the tablet VNC proxy to web UI viewers.
// The proxy connects via POST /vnc/connect (raw TCP stream),
// and web UI clients connect via GET /vnc/stream (WebSocket).
var vnHub = &VNCHub{
	broadcast: make(chan []byte, 256),
	register:  make(chan *websocket.Conn, 1),
}

type VNCHub struct {
	mu        sync.Mutex
	proxyConn *websocket.Conn // the tablet proxy connection
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

// vncConnectHandler: tablet proxy connects here to push RFB data
// The proxy sends RFB frames as binary WebSocket messages.
func (app *ReactAppWrapper) vncConnectHandler(c *gin.Context) {
	ws, err := upgraderVNC.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("VNC connect upgrade: %v", err)
		return
	}
	defer ws.Close()

	log.Info("VNC: tablet proxy connected")
	vnHub.mu.Lock()
	vnHub.proxyConn = ws
	vnHub.mu.Unlock()

	// Read RFB data from proxy and broadcast to web UI clients
	for {
		msgType, data, err := ws.ReadMessage()
		if err != nil {
			log.Printf("VNC: proxy read error: %v", err)
			break
		}
		if msgType == websocket.BinaryMessage {
			select {
			case vnHub.broadcast <- data:
			default:
				// buffer full, drop frame
			}
		}
	}

	vnHub.mu.Lock()
	vnHub.proxyConn = nil
	vnHub.mu.Unlock()
	log.Info("VNC: tablet proxy disconnected")
}