package server

import (
	stlog "log/slog"
	"sync"
	"time"

	"github.com/irishsmurf/go-mmo-poc/game" // Update path
	"github.com/irishsmurf/go-mmo-poc/proto"

	"github.com/gorilla/websocket"
	protos "google.golang.org/protobuf/proto" // Use this for marshal/unmarshal
)

const (
	writeWait      = 10 * time.Second    // Time allowed to write a message to the peer.
	pongWait       = 60 * time.Second    // Time allowed to read the next pong message from the peer.
	pingPeriod     = (pongWait * 9) / 10 // Send pings to peer with this period. Must be less than pongWait.
	maxMessageSize = 2048                // Maximum message size allowed from peer. (Adjust based on expected Protobuf size)
)

// Client is a middleman between the websocket connection and the hub.
type Client struct {
	hub      *Hub
	conn     *websocket.Conn
	send     chan []byte
	mu       sync.RWMutex
	playerID string
	state    *proto.EntityState
	gridCX   int32
	gridCY   int32
	logger   *stlog.Logger // Added logger field
}

// readPump pumps messages from the websocket connection to the hub.
func (c *Client) readPump() {
	defer func() {
		c.hub.unregister <- c
		c.conn.Close()
		// Use client's specific logger if available, otherwise hub's logger
		logger := c.logger
		if logger == nil {
			logger = c.hub.logger
		}
		logger.Info("Client readPump finished")
	}()
	// ... SetReadLimit, SetReadDeadline, SetPongHandler ...

	for {
		messageType, message, err := c.conn.ReadMessage()
		// ... error handling ...
		if err != nil {
			// Logging done inside error handling block
			break
		}

		receivedBytesCounter.Add(float64(len(message))) // Record received bytes

		if messageType == websocket.BinaryMessage {
			// Process Protobuf message
			clientRequest := &proto.ClientRequest{}
			if err := protos.Unmarshal(message, clientRequest); err != nil {
				c.logger.Error("Failed to unmarshal client message", "error", err)
				continue
			}
			processedClientMessagesCounter.Inc() // Increment processed counter
			c.handleClientRequest(clientRequest)
		} else {
			c.logger.Warn("Received non-binary message", "type", messageType)
		}
	}
}

// writePump pumps messages from the hub to the websocket connection.
func (c *Client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.conn.Close()
		logger := c.logger
		if logger == nil {
			logger = c.hub.logger
		}
		logger.Info("Client writePump finished")
	}()
	for {
		select {
		case message, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				c.logger.Info("Send channel closed, sending close message.")
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			err := c.conn.WriteMessage(websocket.BinaryMessage, message)
			if err != nil {
				c.logger.Error("WebSocket write error", "error", err)
				return
			}
			// Metrics for sent bytes are updated in sendProto

		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				c.logger.Error("WebSocket ping error", "error", err)
				return
			}
		}
	}
}

// handleClientRequest processes requests received from the client's websocket.
func (c *Client) handleClientRequest(req *proto.ClientRequest) {
	// Use slog Debug level for frequent events like requests
	// c.logger.Debug("Handling client request", "type", fmt.Sprintf("%T", req.GetRequestType()))

	requestType := req.GetRequestType()
	switch x := requestType.(type) {
	case *proto.ClientRequest_RequestChunk:
		chunkReq := x.RequestChunk
		chunkData := game.GetOrGenerateChunk(chunkReq.ChunkX, chunkReq.ChunkY)
		response := &proto.ServerMessage{
			MessageType: &proto.ServerMessage_WorldChunk{
				WorldChunk: chunkData,
			},
		}
		// Pass label for metrics
		c.sendProto(response, getMessageTypeLabel(response))

	case *proto.ClientRequest_PlayerInput:
		// ... Apply input logic (same as before) ...
		// Logging of input could be done here at Debug level
	default:
		c.logger.Warn("Received unknown client request type")
	}
}

// sendProto serializes a ServerMessage, queues it, and updates metrics.
// Now accepts the message type label.
func (c *Client) sendProto(msg *proto.ServerMessage, msgTypeLabel string) {
	data, err := protos.Marshal(msg)
	if err != nil {
		c.logger.Error("Failed to marshal server message", "type", msgTypeLabel, "error", err)
		return
	}

	select {
	case c.send <- data:
		// Message queued - Update metrics AFTER successful queue
		sentServerMessagesCounter.WithLabelValues(msgTypeLabel).Inc()
		sentBytesCounter.Add(float64(len(data)))
	default:
		c.logger.Warn("Client send buffer full, dropping message", "type", msgTypeLabel)
		// Consider adding a metric for dropped messages
	}
}

// getState provides thread-safe access to the client's state.
func (c *Client) getState() *proto.EntityState {
	c.mu.RLock()
	defer c.mu.RUnlock()
	// Return a copy? Or assume caller won't modify without locking?
	// For simplicity, return pointer, but caller MUST NOT modify without lock.
	// A safer approach returns a copy:
	// if c.state == nil { return nil }
	// stateCopy := proto.Clone(c.state).(*proto.EntityState)
	// return stateCopy
	return c.state // Return pointer for now, requires careful handling
}

// getGridCoords provides thread-safe access to grid coordinates.
func (c *Client) getGridCoords() (int32, int32) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.gridCX, c.gridCY
}
