package server

import (
	"net"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin:     func(r *http.Request) bool { return true }, // Allow all origins for PoC
}

// ServeWs handles websocket requests from the peer.
func ServeWs(hub *Hub, w http.ResponseWriter, r *http.Request) {
	// --- Get Client IP ---
	// Try X-Forwarded-For first (if behind proxy)
	ip := r.Header.Get("X-Forwarded-For")
	if ip == "" {
		// Fallback to RemoteAddr
		ip, _, _ = net.SplitHostPort(r.RemoteAddr)
	} else {
		// X-Forwarded-For can be a list, take the first one
		ip = strings.Split(ip, ",")[0]
	}
	// --- End Get Client IP ---

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		// Use hub logger or default if hub isn't available yet?
		hub.logger.Error("WebSocket upgrade error", "remoteAddr", r.RemoteAddr, "error", err)
		return
	}

	// Create client, assign temporary ID for logging until registered
	tempID := "pending_" + uuid.New().String()[:4]
	clientLogger := hub.logger.With("clientId", tempID, "remoteAddr", ip) // Log IP

	client := &Client{
		hub:      hub,
		conn:     conn,
		send:     make(chan []byte, 256),
		playerID: tempID,       // Hub will assign final ID during registration
		logger:   clientLogger, // Pass specific logger
	}

	client.hub.register <- client // Register sends client to hub loop

	// Goroutines start pumps
	go client.writePump()
	go client.readPump()

	// Log connection established AFTER potential registration start
	// The definitive registration log happens in handleRegister
	clientLogger.Info("WebSocket connection established")
}
