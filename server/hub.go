package server

import (
	"bytes"
	"context"
	"log"
	"math/rand"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	// For atomic counters

	"github.com/google/uuid"
	"github.com/irishsmurf/go-mmo-poc/game" // Update path
	"github.com/irishsmurf/go-mmo-poc/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// GridUpdate struct to pass grid changes via channel
type GridUpdate struct {
	client *Client
	oldCX  int32
	oldCY  int32
	newCX  int32
	newCY  int32
}

// Hub maintains the set of active clients and broadcasts messages.
type Hub struct {
	// Registered clients. Map conn to client struct.
	clients    map[*Client]bool
	clientsMux sync.RWMutex

	// Spatial grid. Map cell key "cx,cy" to set of clients in that cell.
	spatialGrid    map[string]map[*Client]bool
	spatialGridMux sync.RWMutex

	// Register requests from the clients.
	register chan *Client

	// Unregister requests from clients.
	unregister chan *Client

	// Update grid requests
	updateGrid chan *GridUpdate

	// Ticker for game loop
	ticker                  *time.Ticker
	processedClientMessages uint64 // Counter for incoming messages
	sentStateUpdates        uint64 // Counter for outgoing StateUpdates

	metadataClient proto.MetadataServiceClient
	grpcConn       *grpc.ClientConn
}

// NewHub creates a new Hub instance.
func NewHub(metadataServiceAddr string) *Hub {

	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}
	conn, err := grpc.Dial(metadataServiceAddr, opts...)
	if err != nil {
		log.Fatalf("Failed to connect to metadata service: %v", err)
	}

	var metaClient proto.MetadataServiceClient
	if conn != nil {
		metaClient = proto.NewMetadataServiceClient(conn)
	}

	return &Hub{
		clients:                 make(map[*Client]bool),
		spatialGrid:             make(map[string]map[*Client]bool),
		register:                make(chan *Client),
		unregister:              make(chan *Client),
		updateGrid:              make(chan *GridUpdate),
		processedClientMessages: 0,
		sentStateUpdates:        0,
		ticker:                  time.NewTicker(game.ServerTickRateMs * time.Millisecond),
		metadataClient:          metaClient,
		grpcConn:                conn,
	}
}

// Run starts the hub's event processing loop.
func (h *Hub) Run() {
	log.Println("Hub started")
	defer func() {
		h.ticker.Stop()
		if h.grpcConn != nil {
			h.grpcConn.Close()
			log.Println("Metadata gRPC connection closed")
		}
		log.Println("Hub stopped")
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
	h.clientsMux.Unlock()

	// Assign ID and initial state
	client.playerID = "player_" + uuid.New().String()[:8]
	client.state = game.CreateInitialState(client.playerID)

	// Metadata Server stuff
	var displayName = "Player_" + client.playerID[:4]
	if h.metadataClient != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)

		_, err := h.metadataClient.SetPlayerStatus(
			ctx, &proto.SetPlayerStatusRequest{
				PlayerId: client.playerID,
				Status:   proto.OnlineStatus_STATUS_ONLINE,
			})
		cancel()

		if err != nil {
			log.Printf("Failed to set player status in metadata service. ID: %s, Error: %v", client.playerID, err)
		} else {
			log.Printf("Set player status to online in metadata service. ID: %s", client.playerID)
		}

		getResp, err := h.metadataClient.GetPlayerName(
			ctx, &proto.GetPlayerNameRequest{PlayerId: client.playerID},
		)
		cancel()

		if err == nil && getResp.Found {
			displayName = getResp.DisplayName
			log.Printf("Retrieved player name from metadata service. ID: %s, Name: %s", client.playerID, displayName)
		} else {
			if err != nil {
				log.Printf("Failed to retrieve player name from metadata service. ID: %s, Error: %v", client.playerID, err)
			}

			ctxSet, cancelSet := context.WithTimeout(context.Background(), 2*time.Second)
			setResp, errSet := h.metadataClient.SetPlayerName(
				ctxSet, &proto.SetPlayerNameRequest{PlayerId: client.playerID, DisplayName: displayName},
			)
			cancelSet()
			if errSet != nil {
				log.Printf("Failed to set player name in metadata service. ID: %s, Name: %s, Error: %v")
			} else if setResp.Success {
				log.Printf("Set default player name via Metadata service")
			}
		}
	} else {
		log.Print("Metadata client not available")
	}

	client.gridCX, client.gridCY = game.GetGridCellCoords(client.state.Position.X, client.state.Position.Y)

	log.Printf("Player %s registered in grid %d,%d", client.playerID, client.gridCX, client.gridCY)

	// Add to spatial grid
	h.spatialGridMux.Lock()
	gridKey := strconv.Itoa(int(client.gridCX)) + "," + strconv.Itoa(int(client.gridCY))
	if _, ok := h.spatialGrid[gridKey]; !ok {
		h.spatialGrid[gridKey] = make(map[*Client]bool)
	}
	h.spatialGrid[gridKey][client] = true
	h.spatialGridMux.Unlock()

	// Send InitData
	initMsg := &proto.ServerMessage{
		MessageType: &proto.ServerMessage_InitData{
			InitData: &proto.InitData{
				YourEntityId:    client.playerID,
				InitialState:    client.state,
				ColorMap:        game.GetTerrainColorMap(),
				WorldTileWidth:  game.WorldTileWidth,
				WorldTileHeight: game.WorldTileHeight,
				ChunkSizeTiles:  game.ChunkSizeTiles,
				TileRenderSize:  game.TileRenderSize,
			},
		},
	}
	client.sendProto(initMsg)

	// Send initial chunks
	aoiKeys := game.GetAoICellKeys(client.gridCX, client.gridCY, game.AoIChunkRadius)
	for key := range aoiKeys {
		cxStr, cyStr, _ := parseGridKey(key) // Assume valid format
		chunkData := game.GetOrGenerateChunk(cxStr, cyStr)
		chunkMsg := &proto.ServerMessage{
			MessageType: &proto.ServerMessage_WorldChunk{
				WorldChunk: chunkData,
			},
		}
		client.sendProto(chunkMsg) // Send chunks sequentially for simplicity here
	}
}

func (h *Hub) handleUnregister(client *Client) {
	h.clientsMux.Lock()

	if _, ok := h.clients[client]; ok {
		delete(h.clients, client)
		close(client.send) // Close send channel to stop writePump)
		log.Printf("Player %s unregistered", client.playerID)
	}

	if h.metadataClient != nil && client.playerID != "" && !strings.HasPrefix(client.playerID, "pending_") {
		ctxStatus, cancelStatus := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := h.metadataClient.SetPlayerStatus(
			ctxStatus, &proto.SetPlayerStatusRequest{
				PlayerId: client.playerID,
				Status:   proto.OnlineStatus_STATUS_OFFLINE,
			})
		cancelStatus()

		if err != nil {
			log.Printf("Failed to set player status to offline in metadata service. ID: %s, Error: %v", client.playerID, err)
		} else {
			log.Printf("Set player status to offline in metadata service. ID: %s", client.playerID)
		}
	}

	// Remove from spatial grid
	h.spatialGridMux.Lock()
	gridKey := strconv.Itoa(int(client.gridCX)) + "," + strconv.Itoa(int(client.gridCY))
	if clientsInCell, cellExists := h.spatialGrid[gridKey]; cellExists {
		delete(clientsInCell, client)
		if len(clientsInCell) == 0 {
			delete(h.spatialGrid, gridKey) // Clean up empty cell entry
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
	tickStart := time.Now() // Start timer
	serverTimestamp := tickStart.UnixMilli()
	var updatesSentThisTick uint64 = 0 // Counter for this specific tick

	// Snapshot clients for safe iteration
	h.clientsMux.RLock()
	currentClients := make([]*Client, 0, len(h.clients))
	for c := range h.clients {
		currentClients = append(currentClients, c)
	}
	h.clientsMux.RUnlock()

	// Prepare updates for each client
	// Using WaitGroup to potentially parallelize update calculation/sending prep
	var wg sync.WaitGroup
	for _, client := range currentClients {
		wg.Add(1)
		go func(c *Client) { // Process each client concurrently
			defer wg.Done()
			if c.conn == nil {
				return
			} // Skip if connection gone somehow

			playerCX, playerCY := c.getGridCoords() // Use thread-safe getter

			// 1. Determine visible cells for this client
			aoiCellKeys := game.GetAoICellKeys(playerCX, playerCY, game.AoIChunkRadius)

			// 2. Gather entities in AoI cells (read lock on spatial grid)
			visibleEntities := make([]*proto.EntityState, 0, 50) // Preallocate estimate
			processedPlayers := make(map[*Client]bool)

			h.spatialGridMux.RLock()
			for key := range aoiCellKeys {
				if clientsInCell, ok := h.spatialGrid[key]; ok {
					for otherClient := range clientsInCell {
						if !processedPlayers[otherClient] {
							otherState := otherClient.getState() // Use thread-safe getter
							if otherState != nil {
								// --- Delta Compression Point ---
								// Here you would compare otherState with the last state sent to 'c'
								// and create/append a delta state if changes occurred.
								// For now, send full state.
								visibleEntities = append(visibleEntities, otherState)
							}
							processedPlayers[otherClient] = true
						}
					}
				}
			}
			h.spatialGridMux.RUnlock()

			// 3. Create and send StateUpdate message
			if len(visibleEntities) > 0 {
				updateMsg := &proto.ServerMessage{
					MessageType: &proto.ServerMessage_StateUpdate{
						StateUpdate: &proto.StateUpdate{
							ServerTimestampMs: serverTimestamp,
							Entities:          visibleEntities,
							// Add removed_entity_ids logic if tracking state changes precisely
						},
					},
				}
				// Send proto (non-blocking queue)
				c.sendProto(updateMsg)
				atomic.AddUint64(&updatesSentThisTick, 1) // Count successful queues
			}
		}(client)
	}
	wg.Wait() // Wait for all client processing goroutines to finish
	atomic.AddUint64(&h.sentStateUpdates, updatesSentThisTick)
	tickDuration := time.Since(tickStart)
	// Log metrics periodically (e.g., every 10 seconds)
	// This should ideally be done less frequently or via a separate monitoring task
	// For simplicity, log here occasionally:
	if rand.Intn(100) == 0 { // Log roughly 1% of the time
		currentProcessed := atomic.LoadUint64(&h.processedClientMessages)
		currentSent := atomic.LoadUint64(&h.sentStateUpdates)
		h.clientsMux.RLock()
		clientCount := len(h.clients)
		h.clientsMux.RUnlock()
		log.Printf("Tick duration: %v, Clients: %d, ProcessedReq: %d, SentUpdates: %d",
			tickDuration, clientCount, currentProcessed, currentSent)
	}
	// Reset counters periodically if needed for rate calculation, or use Prometheus gauges/counters
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
