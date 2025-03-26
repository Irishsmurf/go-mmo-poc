package main

import (
	"bytes"
	"compress/zlib"
	"context"
	"fmt"
	"image/color"
	"log"
	"math"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/irishsmurf/go-mmo-poc/proto" // *** UPDATE YOUR MODULE PATH ***

	"github.com/gorilla/websocket"
	"github.com/hajimehoshi/ebiten/v2"
	"github.com/hajimehoshi/ebiten/v2/ebitenutil"
	"github.com/hajimehoshi/ebiten/v2/inpututil"
	"github.com/hajimehoshi/ebiten/v2/vector"
	protos "google.golang.org/protobuf/proto"
)

// --- Constants ---
const (
	screenWidth      = 1024
	screenHeight     = 768
	serverAddr       = "ws://localhost:8080/ws" // Make sure port matches server
	writeWaitClient  = 10 * time.Second
	pongWaitClient   = 60 * time.Second
	pingPeriodClient = (pongWaitClient * 9) / 10
	inputSendRate    = 50 * time.Millisecond // Send input ~20 times/sec
)

// --- Client State ---
type ClientStateStore struct {
	mu            sync.RWMutex
	connected     bool
	myEntityID    string
	playerStates  map[string]*proto.EntityState // entityId -> state
	worldChunks   map[string][]byte             // "cx,cy" -> decompressed terrain bytes
	worldInfo     *proto.InitData               // Store relevant parts
	terrainColors map[int32]color.Color         // Parsed colors
	lastUpdateMs  int64
	pendingChunks map[string]bool // Chunks requested but not received
	conn          *websocket.Conn // Active websocket connection
}

var clientState = ClientStateStore{
	playerStates:  make(map[string]*proto.EntityState),
	worldChunks:   make(map[string][]byte),
	pendingChunks: make(map[string]bool),
	terrainColors: make(map[int32]color.Color),
}

// --- Ebitengine Game Struct ---
type Game struct {
	// Camera state
	cameraX float64
	cameraY float64
	zoom    float64

	// Input state
	moveForward bool
	turnLeft    bool
	turnRight   bool

	// Resources
	tileImage   *ebiten.Image // Reusable image for drawing tiles
	playerImage *ebiten.Image // Reusable image for players
}

// --- Game Initialization ---
func NewGame() *Game {
	g := &Game{
		cameraX: 0,
		cameraY: 0,
		zoom:    1.0,
	}
	// Create reusable images
	g.tileImage = ebiten.NewImage(1, 1) // 1x1 pixel image to color for tiles
	g.tileImage.Fill(color.White)       // Fill with white initially

	g.playerImage = ebiten.NewImage(10, 10) // Simple 10x10 square/circle for player
	// Draw a simple circle (approximate)
	radius := float32(5)
	centerX, centerY := float32(5), float32(5)
	vector.DrawFilledCircle(g.playerImage, centerX, centerY, radius, color.White, true)

	return g
}

// --- Ebitengine Game Loop Functions ---

// Update handles game logic updates (input, camera, sending messages)
func (g *Game) Update() error {
	// Handle window closing
	if inpututil.IsKeyJustPressed(ebiten.KeyEscape) {
		return ebiten.Termination // Ask Ebitengine to close
	}

	// --- Handle Input ---
	g.moveForward = ebiten.IsKeyPressed(ebiten.KeyUp) || ebiten.IsKeyPressed(ebiten.KeyW)
	// Simulate turning with left/right for now (adjust actual mechanics later)
	g.turnLeft = ebiten.IsKeyPressed(ebiten.KeyLeft) || ebiten.IsKeyPressed(ebiten.KeyA)
	g.turnRight = ebiten.IsKeyPressed(ebiten.KeyRight) || ebiten.IsKeyPressed(ebiten.KeyD)

	// --- Handle Camera Zoom ---
	_, wheelY := ebiten.Wheel()
	if wheelY > 0 {
		g.zoom *= 1.1
	} else if wheelY < 0 {
		g.zoom /= 1.1
	}
	g.zoom = math.Max(0.1, math.Min(g.zoom, 5.0)) // Clamp zoom

	// --- Center Camera on Player (if available) ---
	clientState.mu.RLock()
	myID := clientState.myEntityID
	myState := clientState.playerStates[myID]
	clientState.mu.RUnlock()

	if myState != nil && myState.Position != nil {
		// Smooth camera follow (optional)
		targetX := float64(myState.Position.X)
		targetY := float64(myState.Position.Y)
		lerpFactor := 0.1 // Adjust for desired smoothness
		g.cameraX += (targetX - g.cameraX) * lerpFactor
		g.cameraY += (targetY - g.cameraY) * lerpFactor
	}

	// Request missing chunks based on camera view (can be done less frequently)
	g.requestVisibleChunks()

	return nil
}

// Draw handles rendering the game state
func (g *Game) Draw(screen *ebiten.Image) {
	clientState.mu.RLock()
	worldInfo := clientState.worldInfo
	myID := clientState.myEntityID
	// Make copies of data needed for drawing under RLock
	chunksToDraw := make(map[string][]byte)
	for k, v := range clientState.worldChunks {
		chunksToDraw[k] = v // Copy pointer/slice header, not deep copy (bytes are immutable anyway)
	}
	playersToDraw := make(map[string]*proto.EntityState)
	for k, v := range clientState.playerStates {
		// playersToDraw[k] = proto.Clone(v).(*proto.EntityState) // Safer: Clone state
		playersToDraw[k] = v // Less safe but maybe ok for drawing if state isn't mutated elsewhere
	}
	terrainColors := clientState.terrainColors
	connected := clientState.connected
	clientState.mu.RUnlock()

	if !connected || worldInfo == nil {
		ebitenutil.DebugPrint(screen, "Connecting...")
		return
	}

	tileSize := float64(worldInfo.TileRenderSize)
	chunkSizeTiles := int(worldInfo.ChunkSizeTiles)
	if tileSize <= 0 || chunkSizeTiles <= 0 {
		ebitenutil.DebugPrint(screen, "Waiting for world info...")
		return
	}

	// --- Calculate Camera/Screen Transformation ---
	op := &ebiten.DrawImageOptions{}
	// 1. Center camera view: Translate so camera pos is at screen center
	op.GeoM.Translate(-g.cameraX, -g.cameraY)
	// 2. Apply zoom
	op.GeoM.Scale(g.zoom, g.zoom)
	// 3. Translate so the center of the view maps to the center of the screen
	op.GeoM.Translate(screenWidth/2, screenHeight/2)

	// --- Determine Visible Chunks ---
	// Inverse transform screen corners to world coords to find bounds
	invGeoM := op.GeoM
	invGeoM.Invert() // Calculate inverse transform

	// Screen corner coordinates
	corners := []struct{ x, y float64 }{
		{0, 0}, {screenWidth, 0}, {0, screenHeight}, {screenWidth, screenHeight},
	}
	minWorldX, minWorldY := math.Inf(1), math.Inf(1)
	maxWorldX, maxWorldY := math.Inf(-1), math.Inf(-1)

	for _, p := range corners {
		wx, wy := invGeoM.Apply(p.x, p.y)
		minWorldX = math.Min(minWorldX, wx)
		maxWorldX = math.Max(maxWorldX, wx)
		minWorldY = math.Min(minWorldY, wy)
		maxWorldY = math.Max(maxWorldY, wy)
	}

	// Convert world bounds to chunk coordinates (+ buffer)
	bufferTiles := 5 // Load slightly outside view
	minChunkX := int32(math.Floor((minWorldX/tileSize)-float64(bufferTiles)) / float64(chunkSizeTiles))
	maxChunkX := int32(math.Ceil((maxWorldX/tileSize)+float64(bufferTiles)) / float64(chunkSizeTiles))
	minChunkY := int32(math.Floor((minWorldY/tileSize)-float64(bufferTiles)) / float64(chunkSizeTiles))
	maxChunkY := int32(math.Ceil((maxWorldY/tileSize)+float64(bufferTiles)) / float64(chunkSizeTiles))

	// --- Draw Terrain Chunks ---
	for cy := minChunkY; cy <= maxChunkY; cy++ {
		for cx := minChunkX; cx <= maxChunkX; cx++ {
			chunkKey := strconv.Itoa(int(cx)) + "," + strconv.Itoa(int(cy))
			terrainBytes, ok := chunksToDraw[chunkKey]
			if !ok {
				continue
			} // Skip if chunk not loaded

			chunkWorldX := float64(cx) * float64(chunkSizeTiles) * tileSize
			chunkWorldY := float64(cy) * float64(chunkSizeTiles) * tileSize

			for tileY := 0; tileY < chunkSizeTiles; tileY++ {
				for tileX := 0; tileX < chunkSizeTiles; tileX++ {
					idx := tileY*chunkSizeTiles + tileX
					if idx >= len(terrainBytes) {
						continue
					} // Bounds check

					terrainType := int32(terrainBytes[idx])
					tileColor, colorOk := terrainColors[terrainType]
					if !colorOk {
						tileColor = color.RGBA{R: 128, G: 0, B: 128, A: 255} // Magenta for unknown
					}

					tileOp := &ebiten.DrawImageOptions{}
					// Calculate world position of the tile's top-left corner
					tileWorldX := chunkWorldX + float64(tileX)*tileSize
					tileWorldY := chunkWorldY + float64(tileY)*tileSize

					// Scale the 1x1 tile image to tile size
					tileOp.GeoM.Scale(tileSize, tileSize)
					// Translate to world position
					tileOp.GeoM.Translate(tileWorldX, tileWorldY)
					// Apply camera transform
					tileOp.GeoM.Concat(op.GeoM)
					// Apply color
					tileOp.ColorScale.ScaleWithColor(tileColor)
					// Draw the scaled, translated, colored 1x1 image
					screen.DrawImage(g.tileImage, tileOp)
				}
			}
		}
	}

	// --- Draw Players ---
	playerSize := tileSize // Base size, adjust as needed
	for _, pState := range playersToDraw {
		if pState.Position == nil {
			continue
		}

		playerColor := color.White // Default

		playerOp := &ebiten.DrawImageOptions{}

		// Scale player image maybe? For now, fixed size relative to screen
		// Scale based on player size / image source size
		scaleX := playerSize / float64(g.playerImage.Bounds().Dx())
		scaleY := playerSize / float64(g.playerImage.Bounds().Dy())
		playerOp.GeoM.Scale(scaleX, scaleY)

		// Translate to center on world position
		playerOp.GeoM.Translate(float64(pState.Position.X)-(playerSize*scaleX)/2.0, float64(pState.Position.Y)-(playerSize*scaleY)/2.0)

		// Apply camera transform
		playerOp.GeoM.Concat(op.GeoM)
		// Apply color (use ColorScale for tinting the white base image)
		playerOp.ColorScale.ScaleWithColor(playerColor)

		// Draw player
		screen.DrawImage(g.playerImage, playerOp)

		// Optional: Draw player ID label
		// debugX, debugY := playerOp.GeoM.Apply(0,0) // Get screen coords
		// ebitenutil.DebugPrintAt(screen, id[:4], int(debugX), int(debugY))
	}

	// --- Draw Debug Info ---
	myPosStr := "N/A"
	if myState, ok := playersToDraw[myID]; ok && myState.Position != nil {
		myPosStr = fmt.Sprintf("%.0f, %.0f", myState.Position.X, myState.Position.Y)
	}
	chunkCount := len(chunksToDraw)
	playerCount := len(playersToDraw)
	debugText := fmt.Sprintf("Connected: %v\nID: %s\nPos: %s\nZoom: %.2f\nChunks: %d\nPlayers: %d\nFPS: %.1f",
		connected, myID, myPosStr, g.zoom, chunkCount, playerCount, ebiten.ActualFPS())
	ebitenutil.DebugPrint(screen, debugText)
}

// Layout defines logical screen size (usually same as window size)
func (g *Game) Layout(outsideWidth, outsideHeight int) (int, int) {
	return screenWidth, screenHeight
}

// --- Helper Functions ---

// worldToScreen converts world coordinates to screen coordinates based on current camera
func (g *Game) worldToScreen(worldX, worldY float64) (screenX, screenY int) {
	// Similar calculation to Draw, but just apply the transform
	x := worldX
	y := worldY

	// Apply camera transform: Translate, Scale, Translate to screen center
	x -= g.cameraX
	y -= g.cameraY
	x *= g.zoom
	y *= g.zoom
	x += screenWidth / 2
	y += screenHeight / 2

	return int(x), int(y)
}

// requestVisibleChunks identifies chunks needed based on camera view and queues requests
func (g *Game) requestVisibleChunks() {
	clientState.mu.RLock()
	info := clientState.worldInfo
	myConn := clientState.conn // Get current connection
	clientState.mu.RUnlock()

	if info == nil || myConn == nil {
		return
	} // Need world info and connection

	tileSize := float64(info.TileRenderSize)
	chunkSizeTiles := int(info.ChunkSizeTiles)
	if tileSize <= 0 || chunkSizeTiles <= 0 {
		return
	}

	// --- Determine visible chunks (same logic as in Draw) ---
	op := &ebiten.DrawImageOptions{}
	op.GeoM.Translate(-g.cameraX, -g.cameraY)
	op.GeoM.Scale(g.zoom, g.zoom)
	op.GeoM.Translate(screenWidth/2, screenHeight/2)
	invGeoM := op.GeoM
	invGeoM.Invert()
	// ... calculate minWorldX/Y, maxWorldX/Y from screen corners ...
	corners := []struct{ x, y float64 }{{0, 0}, {screenWidth, 0}, {0, screenHeight}, {screenWidth, screenHeight}}
	minWorldX, minWorldY := math.Inf(1), math.Inf(1)
	maxWorldX, maxWorldY := math.Inf(-1), math.Inf(-1)
	for _, p := range corners {
		wx, wy := invGeoM.Apply(p.x, p.y)
		minWorldX = math.Min(minWorldX, wx)
		maxWorldX = math.Max(maxWorldX, wx)
		minWorldY = math.Min(minWorldY, wy)
		maxWorldY = math.Max(maxWorldY, wy)
	}
	bufferTiles := 2 // Request slightly outside view
	minChunkX := int32(math.Floor((minWorldX/tileSize)-float64(bufferTiles)) / float64(chunkSizeTiles))
	maxChunkX := int32(math.Ceil((maxWorldX/tileSize)+float64(bufferTiles)) / float64(chunkSizeTiles))
	minChunkY := int32(math.Floor((minWorldY/tileSize)-float64(bufferTiles)) / float64(chunkSizeTiles))
	maxChunkY := int32(math.Ceil((maxWorldY/tileSize)+float64(bufferTiles)) / float64(chunkSizeTiles))

	requestsToSend := []*proto.ClientRequest{}

	clientState.mu.Lock() // Lock for checking chunks and pending
	for cy := minChunkY; cy <= maxChunkY; cy++ {
		for cx := minChunkX; cx <= maxChunkX; cx++ {
			key := strconv.Itoa(int(cx)) + "," + strconv.Itoa(int(cy))
			_, chunkExists := clientState.worldChunks[key]
			_, pending := clientState.pendingChunks[key]
			if !chunkExists && !pending {
				// Queue request if not present and not pending
				req := &proto.ClientRequest{
					RequestType: &proto.ClientRequest_RequestChunk{
						RequestChunk: &proto.RequestChunk{ChunkX: cx, ChunkY: cy},
					},
				}
				requestsToSend = append(requestsToSend, req)
				clientState.pendingChunks[key] = true // Mark as pending
				// log.Printf("Queueing request for chunk %s", key)
			}
		}
	}
	clientState.mu.Unlock()

	// Send queued requests (outside lock)
	for _, req := range requestsToSend {
		sendClientRequest(myConn, req) // Use the captured connection
	}
}

// --- Networking ---

// Connects and runs the WebSocket read loop
func runWebSocket(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			log.Println("WebSocket context cancelled, exiting loop.")
			return
		default:
			log.Printf("Attempting to connect to %s", serverAddr)
			conn, _, err := websocket.DefaultDialer.Dial(serverAddr, nil)
			if err != nil {
				log.Printf("Dial error: %v. Retrying in 5s...", err)
				clientState.mu.Lock()
				clientState.connected = false
				clientState.conn = nil
				clientState.mu.Unlock()
				time.Sleep(5 * time.Second)
				continue // Retry connection
			}
			log.Printf("Connected to %s", serverAddr)

			// Store connection and mark as connected
			clientState.mu.Lock()
			clientState.connected = true
			clientState.conn = conn
			// Reset state on successful connection
			clientState.myEntityID = ""
			clientState.playerStates = make(map[string]*proto.EntityState)
			clientState.worldChunks = make(map[string][]byte)
			clientState.pendingChunks = make(map[string]bool)
			clientState.worldInfo = nil
			clientState.mu.Unlock()

			done := make(chan struct{}) // Signal channel for read loop completion

			// --- WebSocket Read Loop ---
			go func(connHandle *websocket.Conn) {
				defer close(done)
				defer log.Println("WebSocket read loop finished.")
				connHandle.SetReadDeadline(time.Now().Add(pongWaitClient))
				connHandle.SetPongHandler(func(string) error { connHandle.SetReadDeadline(time.Now().Add(pongWaitClient)); return nil })

				for {
					messageType, message, err := connHandle.ReadMessage()
					if err != nil {
						// Log errors unless it's a normal closure
						if !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
							log.Printf("WebSocket read error: %v", err)
						} else {
							log.Println("WebSocket connection closed normally.")
						}
						return // Exit goroutine on error/close
					}
					if messageType == websocket.BinaryMessage {
						handleServerMessage(message) // Process server messages
					}
				}
			}(conn) // Pass current connection handle to goroutine

			// --- WebSocket Ping Loop (and Input Sending) ---
			pingTicker := time.NewTicker(pingPeriodClient)
			inputTicker := time.NewTicker(inputSendRate) // Ticker for sending input
		pingLoop:
			for {
				select {
				case <-ctx.Done(): // Context cancelled from main
					log.Println("Ping loop context cancelled.")
					conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "Client shutting down"))
					break pingLoop // Exit inner loop
				case <-done: // Read loop finished (connection closed/error)
					log.Println("Read loop done signal received, exiting ping loop.")
					break pingLoop // Exit inner loop
				case <-pingTicker.C:
					conn.SetWriteDeadline(time.Now().Add(writeWaitClient))
					if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
						log.Println("Ping error:", err)
						// Don't break loop here, let read loop detect closure
					}
				case <-inputTicker.C:
					// Check input state and send PlayerInput if needed
					// Access Game struct 'g' directly - needs careful consideration or passing input state via channel
					// For simplicity, assume 'g' is accessible globally or passed differently
					// This part needs refinement if Game struct isn't global
					// *** This requires access to the 'g *Game' instance ***
					// sendPlayerInput(conn, g.moveForward, g.turnLeft, g.turnRight)
					sendPlayerInput(conn, currentGameInstance.moveForward, currentGameInstance.turnLeft, currentGameInstance.turnRight) // Access global/passed instance
				}
			}
			pingTicker.Stop()
			inputTicker.Stop()

			// Cleanup after connection loop ends
			conn.Close() // Ensure connection is closed
			clientState.mu.Lock()
			clientState.connected = false
			clientState.conn = nil
			log.Println("Disconnected.")
			clientState.mu.Unlock()

			// If context is not done, wait before retrying
			select {
			case <-ctx.Done():
				return // Exit outer loop if context cancelled
			case <-time.After(5 * time.Second):
				// Wait before reconnecting
			}
		} // End of outer connection loop
	} // End of runWebSocket
}

// handleServerMessage processes messages received from the server
func handleServerMessage(data []byte) {
	msg := &proto.ServerMessage{}
	if err := protos.Unmarshal(data, msg); err != nil {
		log.Printf("Failed to unmarshal server message: %v", err)
		return
	}

	clientState.mu.Lock() // Lock state for modification
	defer clientState.mu.Unlock()

	switch x := msg.MessageType.(type) {
	case *proto.ServerMessage_InitData:
		init := x.InitData
		log.Printf("Received InitData for %s", init.YourEntityId)
		clientState.myEntityID = init.YourEntityId
		clientState.worldInfo = init
		if init.InitialState != nil {
			clientState.playerStates[init.YourEntityId] = init.InitialState
		}
		// Parse and store colors
		clientState.terrainColors = make(map[int32]color.Color)
		if init.ColorMap != nil {
			for typeID, cDef := range init.ColorMap.TypeToColor {
				if c, err := parseHexColor(cDef.HexColor); err == nil {
					clientState.terrainColors[typeID] = c
				} else {
					log.Printf("Failed to parse color %s for type %d: %v", cDef.HexColor, typeID, err)
				}
			}
		}

	case *proto.ServerMessage_WorldChunk:
		chunk := x.WorldChunk
		key := strconv.Itoa(int(chunk.ChunkX)) + "," + strconv.Itoa(int(chunk.ChunkY))

		var decompressed []byte
		if chunk.Compression == proto.CompressionType_ZLIB {
			r, err := zlib.NewReader(bytes.NewReader(chunk.TerrainData))
			if err != nil {
				log.Printf("Error creating zlib reader for chunk %s: %v", key, err)
				delete(clientState.pendingChunks, key)
				return
			}
			buf := new(bytes.Buffer)
			_, err = buf.ReadFrom(r)
			r.Close()
			if err != nil {
				log.Printf("Error decompressing chunk %s: %v", key, err)
				delete(clientState.pendingChunks, key)
				return
			}
			decompressed = buf.Bytes()
		} else { // Assume NONE or handle other types
			decompressed = chunk.TerrainData
		}

		// log.Printf("Stored chunk %s (%d bytes)", key, len(decompressed))
		clientState.worldChunks[key] = decompressed
		delete(clientState.pendingChunks, key) // Mark as received

	case *proto.ServerMessage_StateUpdate:
		update := x.StateUpdate
		clientState.lastUpdateMs = update.ServerTimestampMs

		currentVisible := make(map[string]bool)
		for _, state := range update.Entities {
			if state != nil {
				// Basic interpolation placeholder (replace with proper logic)
				// Lerp position towards new state over time in Update() instead
				clientState.playerStates[state.EntityId] = state
				currentVisible[state.EntityId] = true
			}
		}
		// Remove players no longer in the update
		idsToRemove := []string{}
		for id := range clientState.playerStates {
			if !currentVisible[id] {
				idsToRemove = append(idsToRemove, id)
			}
		}
		for _, id := range idsToRemove {
			delete(clientState.playerStates, id)
		}

	default:
		log.Printf("Received unhandled server message type: %T", x)
	}
}

// sendClientRequest sends a message to the server
func sendClientRequest(conn *websocket.Conn, req *proto.ClientRequest) {
	if conn == nil {
		return
	} // Don't send if not connected

	data, err := protos.Marshal(req)
	if err != nil {
		log.Printf("Failed to marshal client request: %v", err)
		return
	}
	conn.SetWriteDeadline(time.Now().Add(writeWaitClient))
	err = conn.WriteMessage(websocket.BinaryMessage, data)
	if err != nil {
		log.Printf("Error sending client request: %v", err)
		// Network loop will detect write errors and handle disconnect
	}
}

// sendPlayerInput creates and sends the PlayerInput message
func sendPlayerInput(conn *websocket.Conn, forward, left, right bool) {
	if conn == nil || (!forward && !left && !right) {
		return // No input to send or not connected
	}

	// Basic turn simulation for demo
	turnDelta := float32(0.0)
	if left {
		turnDelta = -0.1
	}
	if right {
		turnDelta = 0.1
	}

	input := &proto.PlayerInput{
		MoveForward: forward,
		TurnDelta:   turnDelta,
	}
	req := &proto.ClientRequest{
		RequestType: &proto.ClientRequest_PlayerInput{PlayerInput: input},
	}
	sendClientRequest(conn, req)
}

// parseHexColor converts "#RRGGBB" to color.Color
func parseHexColor(s string) (color.Color, error) {
	if len(s) > 0 && s[0] == '#' {
		s = s[1:]
	}
	c := color.RGBA{A: 255}
	var err error
	switch len(s) {
	case 6:
		_, err = fmt.Sscanf(s, "%02x%02x%02x", &c.R, &c.G, &c.B)
	case 3:
		_, err = fmt.Sscanf(s, "%1x%1x%1x", &c.R, &c.G, &c.B)
		// Double the hex digits: 1 -> 11, F -> FF, etc.
		c.R *= 17
		c.G *= 17
		c.B *= 17
	default:
		err = fmt.Errorf("invalid length, must be 7 or 4")

	}
	return c, err
}

// Global variable to hold the game instance, needed for input sending from network goroutine
// This isn't ideal Go practice but simplest for this example.
// Better ways: Channels for input state, dependency injection.
var currentGameInstance *Game

// --- Main Function ---
func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.Println("Starting Ebitengine client...")

	// --- Context for graceful shutdown ---
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // Ensure cancel is called on exit

	// Handle Ctrl+C / termination signals
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-interrupt
		log.Println("Interrupt received, shutting down...")
		cancel() // Trigger context cancellation
	}()

	// --- Start WebSocket connection in background ---
	go runWebSocket(ctx)

	// --- Setup and run Ebitengine ---
	ebiten.SetWindowSize(screenWidth, screenHeight)
	ebiten.SetWindowTitle("Go MMO PoC Client")
	ebiten.SetWindowResizingMode(ebiten.WindowResizingModeEnabled)

	currentGameInstance = NewGame() // Assign to global

	if err := ebiten.RunGame(currentGameInstance); err != nil {
		if err == ebiten.Termination {
			log.Println("Ebitengine closed normally.")
		} else {
			log.Fatalf("Ebitengine error: %v", err)
		}
	}

	// Ebitengine RunGame blocks until termination.
	// Ensure context is cancelled if Ebitengine exits first.
	cancel()
	log.Println("Client finished.")

	// Wait briefly for network goroutine to potentially finish cleanup
	time.Sleep(500 * time.Millisecond)
}
