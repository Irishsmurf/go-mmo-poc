package server

import (
	"bytes"
	stlog "log/slog"
	"strconv"
	"sync"
	"time"

	// For atomic counters

	"github.com/irishsmurf/go-mmo-poc/game" // Update path
	"github.com/irishsmurf/go-mmo-poc/proto"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// --- Prometheus Metrics Definitions ---
// Use promauto for easier registration. Add labels if needed (e.g., region)
var (
	connectedClientsGauge = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "mmo_connected_clients_total",
		Help: "Total number of currently connected clients.",
	})
	registerEventsCounter = promauto.NewCounter(prometheus.CounterOpts{
		Name: "mmo_register_events_total",
		Help: "Total number of client register events.",
	})
	unregisterEventsCounter = promauto.NewCounter(prometheus.CounterOpts{
		Name: "mmo_unregister_events_total",
		Help: "Total number of client unregister events.",
	})
	processedClientMessagesCounter = promauto.NewCounter(prometheus.CounterOpts{
		Name: "mmo_client_messages_processed_total",
		Help: "Total number of messages received from clients and processed.",
	})
	sentServerMessagesCounter = promauto.NewCounterVec(prometheus.CounterOpts{ // Use CounterVec for message types
		Name: "mmo_server_messages_sent_total",
		Help: "Total number of messages sent to clients, labeled by type.",
	}, []string{"message_type"}) // Label for Init, Chunk, StateUpdate etc.
	sentBytesCounter = promauto.NewCounter(prometheus.CounterOpts{
		Name: "mmo_bytes_sent_total",
		Help: "Total number of payload bytes sent to clients.",
	})
	receivedBytesCounter = promauto.NewCounter(prometheus.CounterOpts{
		Name: "mmo_bytes_received_total",
		Help: "Total number of payload bytes received from clients.",
	})
	gameTickDurationHistogram = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "mmo_game_tick_duration_seconds",
		Help:    "Histogram of game tick processing durations.",
		Buckets: prometheus.ExponentialBuckets(0.005, 2, 10), // 5ms, 10ms, 20ms ... ~2.5s
	})
	// Add more: e.g., errors, queue lengths, specific message type rates
)

// GridUpdate struct to pass grid changes via channel
type GridUpdate struct {
	client *Client
	oldCX  int32
	oldCY  int32
	newCX  int32
	newCY  int32
}

type Hub struct {
	clients        map[*Client]bool
	clientsMux     sync.RWMutex
	spatialGrid    map[string]map[*Client]bool
	spatialGridMux sync.RWMutex
	register       chan *Client
	unregister     chan *Client
	updateGrid     chan *GridUpdate
	ticker         *time.Ticker
	logger         *stlog.Logger // Injected structured logger
	// Remove atomic counters if solely using Prometheus now
	// processedClientMessages uint64
	// sentStateUpdates        uint64
}

// NewHub creates a new Hub instance, accepting a logger.
func NewHub(logger *stlog.Logger) *Hub {
	if logger == nil {
		logger = stlog.Default() // Fallback to default if nil passed
	}
	return &Hub{
		clients:     make(map[*Client]bool),
		spatialGrid: make(map[string]map[*Client]bool),
		register:    make(chan *Client),
		unregister:  make(chan *Client),
		updateGrid:  make(chan *GridUpdate),
		ticker:      time.NewTicker(game.ServerTickRateMs * time.Millisecond),
		logger:      logger.With("component", "hub"), // Add context to logger
	}
}

// Run starts the hub's event processing loop.
func (h *Hub) Run() {
	h.logger.Info("Hub started")
	defer func() {
		h.ticker.Stop()
		h.logger.Info("Hub stopped")
	}()
	for {
		select {
		case client := <-h.register:
			h.handleRegister(client)

		case client := <-h.unregister:
			h.handleUnregister(client)

		case update := <-h.updateGrid:
			h.handleGridUpdate(update)

		case <-h.ticker.C:
			h.runGameTick()
		}
	}
}

func (h *Hub) handleRegister(client *Client) {
	h.clientsMux.Lock()
	h.clients[client] = true
	clientCount := len(h.clients) // Get count while locked
	h.clientsMux.Unlock()

	connectedClientsGauge.Set(float64(clientCount)) // Update gauge
	registerEventsCounter.Inc()                     // Increment counter

	// Assign ID/State - Use client logger from now on
	client.logger = h.logger.With("clientId", client.playerID, "remoteAddr", client.conn.RemoteAddr().String()) // Add context
	client.logger.Info("Player registered", "gridX", client.gridCX, "gridY", client.gridCY)

	// Add to spatial grid (same logic)
	h.spatialGridMux.Lock()
	gridKey := strconv.Itoa(int(client.gridCX)) + "," + strconv.Itoa(int(client.gridCY))
	if _, ok := h.spatialGrid[gridKey]; !ok {
		h.spatialGrid[gridKey] = make(map[*Client]bool)
	}
	h.spatialGrid[gridKey][client] = true
	h.spatialGridMux.Unlock()

	// --- Send InitData & Initial Chunks (Add metrics) ---
	// Send InitData
	initMsg := &proto.ServerMessage{ /* ... as before ... */ }
	// BEFORE sending: Get message type label
	msgTypeLabel := getMessageTypeLabel(initMsg)
	// Call sendProto which now updates metrics
	client.sendProto(initMsg, msgTypeLabel)

	// Send initial chunks
	aoiKeys := game.GetAoICellKeys(client.gridCX, client.gridCY, game.AoIChunkRadius)
	initialChunkTasks := []func() error{} // Use functions for potential async send
	for key := range aoiKeys {
		cxStr, cyStr, ok := parseGridKey(key)
		if !ok {
			continue
		}
		chunkData := game.GetOrGenerateChunk(cxStr, cyStr)
		chunkMsg := &proto.ServerMessage{
			MessageType: &proto.ServerMessage_WorldChunk{
				WorldChunk: chunkData,
			},
		}
		msgLabel := getMessageTypeLabel(chunkMsg)
		// Create closure to capture variables for potential concurrent sending
		sendFunc := func() error {
			client.sendProto(chunkMsg, msgLabel)
			return nil // Return error if sendProto could return one
		}
		initialChunkTasks = append(initialChunkTasks, sendFunc)
	}
	// Send sequentially for simplicity now, or use WaitGroup + goroutines for concurrency
	for _, task := range initialChunkTasks {
		task()
	}
	// --- End Init ---
}

func (h *Hub) handleUnregister(client *Client) {
	h.clientsMux.Lock()
	_, ok := h.clients[client]
	if ok {
		delete(h.clients, client)
		close(client.send)
		clientCount := len(h.clients) // Get count while locked
		h.clientsMux.Unlock()         // Unlock BEFORE logging/metrics

		connectedClientsGauge.Set(float64(clientCount)) // Update gauge
		unregisterEventsCounter.Inc()                   // Increment counter
		client.logger.Info("Player unregistered")
	} else {
		h.clientsMux.Unlock() // Ensure unlock even if client not found
		h.logger.Warn("Attempted to unregister client not found in map")
		return // Avoid spatial grid manipulation if client wasn't registered
	}

	// Remove from spatial grid (same logic)
	h.spatialGridMux.Lock()
	gridKey := strconv.Itoa(int(client.gridCX)) + "," + strconv.Itoa(int(client.gridCY))
	if clientsInCell, cellExists := h.spatialGrid[gridKey]; cellExists {
		delete(clientsInCell, client)
		if len(clientsInCell) == 0 {
			delete(h.spatialGrid, gridKey)
		}
	}
	h.spatialGridMux.Unlock()
}

func (h *Hub) handleGridUpdate(update *GridUpdate) {
	h.spatialGridMux.Lock()
	defer h.spatialGridMux.Unlock()

	oldKey := strconv.Itoa(int(update.oldCX)) + "," + strconv.Itoa(int(update.oldCY))
	if clientsInCell, cellExists := h.spatialGrid[oldKey]; cellExists {
		delete(clientsInCell, update.client)
		if len(clientsInCell) == 0 {
			delete(h.spatialGrid, oldKey)
		}
	}

	newKey := strconv.Itoa(int(update.newCX)) + "," + strconv.Itoa(int(update.newCY))
	if _, ok := h.spatialGrid[newKey]; !ok {
		h.spatialGrid[newKey] = make(map[*Client]bool)
	}
	h.spatialGrid[newKey][update.client] = true
	// log.Printf("Player %s grid update %s -> %s", update.client.playerID, oldKey, newKey)
}

func (h *Hub) runGameTick() {
	tickStart := time.Now()
	serverTimestamp := tickStart.UnixMilli()

	// Snapshot clients
	h.clientsMux.RLock()
	currentClients := make([]*Client, 0, len(h.clients))
	for c := range h.clients {
		currentClients = append(currentClients, c)
	}
	h.clientsMux.RUnlock()

	var wg sync.WaitGroup
	for _, client := range currentClients {
		wg.Add(1)
		go func(c *Client) {
			defer wg.Done()
			if c.conn == nil {
				return
			}

			// ... Determine AoI cells and gather visible entities (same logic) ...
			// playerCX, playerCY := c.getGridCoords()
			// aoiCellKeys := game.GetAoICellKeys(playerCX, playerCY, game.AoIChunkRadius)
			visibleEntities := make([]*proto.EntityState, 0, 50)
			//processedPlayers := make(map[*Client]bool)
			h.spatialGridMux.RLock()
			// ... loop through aoiCellKeys and spatialGrid ... gather states ...
			h.spatialGridMux.RUnlock()

			// --- Create and Send StateUpdate (Add Metrics) ---
			if len(visibleEntities) > 0 {
				updateMsg := &proto.ServerMessage{
					MessageType: &proto.ServerMessage_StateUpdate{
						StateUpdate: &proto.StateUpdate{
							ServerTimestampMs: serverTimestamp,
							Entities:          visibleEntities,
						},
					},
				}
				msgLabel := getMessageTypeLabel(updateMsg)
				c.sendProto(updateMsg, msgLabel) // Pass label to update metrics
			}
		}(client)
	}
	wg.Wait()

	tickDuration := time.Since(tickStart)
	gameTickDurationHistogram.Observe(tickDuration.Seconds()) // Record duration

	// Log tick duration periodically (less frequently than before)
	// Use Debug level for frequent logs
	// if rand.Intn(50) == 0 { // e.g., every 5 seconds if tick is 100ms
	// 	h.logger.Debug("Game tick finished", "duration", tickDuration)
	// }
}

// getMessageTypeLabel extracts a string label for Prometheus based on message type.
func getMessageTypeLabel(msg *proto.ServerMessage) string {
	if msg == nil {
		return "unknown"
	}
	switch msg.MessageType.(type) {
	case *proto.ServerMessage_InitData:
		return "init_data"
	case *proto.ServerMessage_WorldChunk:
		return "world_chunk"
	case *proto.ServerMessage_StateUpdate:
		return "state_update"
	default:
		return "unknown"
	}
}

// Helper to parse grid key - can be moved to game package
func parseGridKey(key string) (cx, cy int32, ok bool) {
	parts := bytes.SplitN([]byte(key), []byte(","), 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	cxInt, err1 := strconv.Atoi(string(parts[0]))
	cyInt, err2 := strconv.Atoi(string(parts[1]))
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return int32(cxInt), int32(cyInt), true
}
