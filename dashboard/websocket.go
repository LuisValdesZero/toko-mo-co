package dashboard

import (
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

const (
	writeWait      = 10 * time.Second
	pongWait       = 15 * time.Second // shortened so dead connections are cleaned up quickly
	pingPeriod     = (pongWait * 9) / 10
	maxMessageSize = 512 // max bytes we expect to read from the browser (only pong frames)
)

// NewUpgrader returns a WebSocket upgrader configured with the allowed origins.
// If allowedOrigins is nil/empty, all origins are allowed (dev mode) — a warning is logged.
// In production pass a list of allowed origins, e.g. ["https://app.example.com"].
func NewUpgrader(allowedOrigins []string) websocket.Upgrader {
	if len(allowedOrigins) == 0 {
		log.Println("[WS] WARNING: No allowed origins configured — accepting WebSocket connections from ANY origin. Set ALLOWED_ORIGINS in production.")
	}
	return websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 4096,
		CheckOrigin: func(r *http.Request) bool {
			if len(allowedOrigins) == 0 {
				return true
			}
			origin := r.Header.Get("Origin")
			for _, allowed := range allowedOrigins {
				if strings.EqualFold(origin, strings.TrimSpace(allowed)) {
					return true
				}
			}
			log.Printf("[WS] rejected origin: %q", origin)
			return false
		},
	}
}

// ServeWS handles WebSocket upgrade requests using the provided upgrader.
func ServeWS(hub *Hub, upgrader websocket.Upgrader, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("[WS] upgrade error:", err)
		return
	}

	client := &Client{
		hub:  hub,
		send: make(chan []byte, 256),
	}

	client.hub.register <- client

	go client.writePump(conn)
	go client.readPump(conn)
}

// readPump pumps messages from WebSocket to hub
func (c *Client) readPump(conn *websocket.Conn) {
	defer func() {
		c.hub.unregister <- c
		conn.Close()
	}()

	conn.SetReadLimit(maxMessageSize)
	conn.SetReadDeadline(time.Now().Add(pongWait))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	for {
		_, _, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("[WS] unexpected close: %v", err)
			}
			break
		}
	}
}

// writePump pumps messages from hub to WebSocket connection
func (c *Client) writePump(conn *websocket.Conn) {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		conn.Close()
	}()

	for {
		select {
		case message, ok := <-c.send:
			conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := conn.WriteMessage(websocket.TextMessage, message); err != nil {
				return
			}

		case <-ticker.C:
			conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// ServeIndex serves the dashboard HTML (no-cache so updates are always picked up)
func ServeIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	http.ServeFile(w, r, "./dashboard/index.html")
}

// ServeSettings serves the settings page HTML
func ServeSettings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	http.ServeFile(w, r, "./dashboard/settings.html")
}
