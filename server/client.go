package server

import (
	"log"
	"sync"
	"sync/atomic"
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
	hub *Hub

	// The websocket connection.
	conn *websocket.Conn

	// Buffered channel of outbound messages.
	send chan []byte

	// Game state associated with this client
	mu       sync.RWMutex // Protects playerState and grid coords
	playerID string
	state    *proto.EntityState
	gridCX   int32
	gridCY   int32
}

// readPump pumps messages from the websocket connection to the hub.
func (c *Client) readPump() {
	defer func() {
		c.hub.unregister <- c
		c.conn.Close()
		log.Printf("Client %s readPump finished", c.playerID)
	}()
	c.conn.SetReadLimit(maxMessageSize)
	c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error { c.conn.SetReadDeadline(time.Now().Add(pongWait)); return nil })

	for {
		messageType, message, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("Client %s read error: %v", c.playerID, err)
			} else {
				log.Printf("Client %s connection closed normally or expectedly", c.playerID)
			}
			break
		}

		if messageType == websocket.BinaryMessage {
			// Process Protobuf message
			clientRequest := &proto.ClientRequest{}
			if err := protos.Unmarshal(message, clientRequest); err != nil {
				log.Printf("Client %s failed to unmarshal message: %v", c.playerID, err)
				continue // Ignore malformed messages
			}
			// Handle the request (potentially pass to hub or handle directly)
			c.handleClientRequest(clientRequest)
		} else {
			log.Printf("Client %s received non-binary message type: %d", c.playerID, messageType)
			// Ignore non-binary messages for now
		}
	}
}

// writePump pumps messages from the hub to the websocket connection.
func (c *Client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.conn.Close() // Close connection if writing fails
		// Unregistering happens in readPump's defer or if hub closes client explicitly
		log.Printf("Client %s writePump finished", c.playerID)
	}()
	for {
		select {
		case message, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				// The hub closed the channel.
				log.Printf("Client %s send channel closed, sending close message.", c.playerID)
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			err := c.conn.WriteMessage(websocket.BinaryMessage, message)
			if err != nil {
				log.Printf("Client %s write error: %v", c.playerID, err)
				return // Stop pump on write error
			}

		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				log.Printf("Client %s ping error: %v", c.playerID, err)
				return // Stop pump on ping error
			}
		}
	}
}

// handleClientRequest processes requests received from the client's websocket.
func (c *Client) handleClientRequest(req *proto.ClientRequest) {
	requestType := req.GetRequestType()
	atomic.AddUint64(&c.hub.processedClientMessages, 1) // Increment counter

	switch x := requestType.(type) {
	case *proto.ClientRequest_RequestChunk:
		chunkReq := x.RequestChunk
		// log.Printf("Client %s requested chunk %d,%d", c.playerID, chunkReq.ChunkX, chunkReq.ChunkY)
		// Fetch chunk (generation is handled by game.GetOrGenerateChunk)
		chunkData := game.GetOrGenerateChunk(chunkReq.ChunkX, chunkReq.ChunkY)
		response := &proto.ServerMessage{
			MessageType: &proto.ServerMessage_WorldChunk{
				WorldChunk: chunkData,
			},
		}
		c.sendProto(response)

	case *proto.ClientRequest_PlayerInput:
		input := x.PlayerInput
		// Apply input to player state (protected by mutex)
		c.mu.Lock()
		oldCX, oldCY := c.gridCX, c.gridCY
		if c.state != nil {
			game.ApplyInput(c.state, input) // Update position based on input
			// Recalculate grid cell after movement
			c.gridCX, c.gridCY = game.GetGridCellCoords(c.state.Position.X, c.state.Position.Y)
		}
		c.mu.Unlock()

		// Notify hub if grid cell changed
		if oldCX != c.gridCX || oldCY != c.gridCY {
			c.hub.updateGrid <- &GridUpdate{
				client: c,
				oldCX:  oldCX,
				oldCY:  oldCY,
				newCX:  c.gridCX,
				newCY:  c.gridCY,
			}
		}

	default:
		log.Printf("Client %s sent unknown request type", c.playerID)
	}
}

// sendProto serializes a ServerMessage and queues it for sending.
func (c *Client) sendProto(msg *proto.ServerMessage) {
	data, err := protos.Marshal(msg)
	if err != nil {
		log.Printf("Client %s failed to marshal message: %v", c.playerID, err)
		return
	}

	select {
	case c.send <- data:
		// Message queued
	default:
		// Send buffer is full, client might be slow or disconnected
		log.Printf("Client %s send buffer full, dropping message type %T", c.playerID, msg.MessageType)
		// Consider closing the connection here if buffer stays full consistently
		// close(c.send) // This would trigger writePump cleanup
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
