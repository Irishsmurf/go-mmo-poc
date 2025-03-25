package main

import (
	"bytes"
	"compress/zlib"
	"context"
	"log"
	"math"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/irishsmurf/go-mmo-poc/proto" // Update path

	"github.com/gorilla/websocket"
	protos "google.golang.org/protobuf/proto"
)

var serverAddr = "ws://localhost:8080/ws" // Make sure port matches server

type ClientState struct {
	mu            sync.RWMutex
	connected     bool
	myEntityID    string
	playerStates  map[string]*proto.EntityState // entityId -> state
	worldChunks   map[string][]byte             // "cx,cy" -> decompressed terrain bytes
	worldInfo     *proto.InitData               // Store relevant parts
	lastUpdateMs  int64
	pendingChunks map[string]bool // Chunks requested but not received
}

var clientState = ClientState{
	playerStates:  make(map[string]*proto.EntityState),
	worldChunks:   make(map[string][]byte),
	pendingChunks: make(map[string]bool),
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.Println("Starting client...")

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt, syscall.SIGTERM)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	conn, _, err := websocket.DefaultDialer.Dial(serverAddr, nil)
	if err != nil {
		log.Fatalf("Dial error: %v", err)
	}
	defer conn.Close()
	log.Printf("Connected to %s", serverAddr)
	clientState.mu.Lock()
	clientState.connected = true
	clientState.mu.Unlock()

	done := make(chan struct{}) // To signal read loop completion

	// Goroutine to read messages from server
	go func() {
		defer close(done)
		defer log.Println("Read loop finished.")
		conn.SetReadDeadline(time.Now().Add(pongWaitClient)) // From client perspective
		conn.SetPongHandler(func(string) error { conn.SetReadDeadline(time.Now().Add(pongWaitClient)); return nil })

		for {
			select {
			case <-ctx.Done():
				log.Println("Read loop exiting due to context cancellation.")
				return
			default:
				messageType, message, err := conn.ReadMessage()
				if err != nil {
					if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
						log.Println("Connection closed normally.")
					} else {
						log.Printf("Read error: %v", err)
					}
					clientState.mu.Lock()
					clientState.connected = false
					clientState.mu.Unlock()
					return // Exit goroutine on error/close
				}

				if messageType == websocket.BinaryMessage {
					handleServerMessage(message)
				} else {
					log.Printf("Received non-binary message type: %d", messageType)
				}
			}
		}
	}()

	// Goroutine for sending pings and potentially client input/requests
	pingTicker := time.NewTicker(pingPeriodClient)
	requestTicker := time.NewTicker(5 * time.Second) // Example: request nearby chunks periodically
	defer pingTicker.Stop()
	defer requestTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("Main loop exiting due to context cancellation.")
			// Attempt graceful close
			err := conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
			if err != nil {
				log.Println("Error sending close message:", err)
			}
			select {
			case <-done: // Wait for read loop to finish
			case <-time.After(time.Second): // Or timeout
				log.Println("Read loop did not finish in time.")
			}
			return

		case <-done: // Read loop finished (error or closed)
			log.Println("Connection lost. Exiting.")
			return

		case <-pingTicker.C:
			conn.SetWriteDeadline(time.Now().Add(writeWaitClient))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				log.Println("Ping error:", err)
				cancel() // Signal exit if ping fails
				continue
			}

		case <-requestTicker.C:
			// --- Example: Request missing chunks around player ---
			clientState.mu.RLock()
			myState := clientState.playerStates[clientState.myEntityID]
			info := clientState.worldInfo
			connected := clientState.connected
			clientState.mu.RUnlock()

			if connected && myState != nil && myState.Position != nil && info != nil {
				cx, cy := getClientGridCellCoords(myState.Position.X, myState.Position.Y, info)
				aoiKeys := getClientAoICellKeys(cx, cy, 1) // Radius 1

				clientState.mu.Lock() // Lock for pendingChunks access
				for key := range aoiKeys {
					if _, exists := clientState.worldChunks[key]; !exists {
						if !clientState.pendingChunks[key] {
							cxStr, cyStr, ok := parseClientGridKey(key)
							if ok {
								req := &proto.ClientRequest{
									RequestType: &proto.ClientRequest_RequestChunk{
										RequestChunk: &proto.RequestChunk{ChunkX: cxStr, ChunkY: cyStr},
									},
								}
								log.Printf("Requesting missing chunk: %s", key)
								sendClientRequest(conn, req)
								clientState.pendingChunks[key] = true // Mark as requested
							}
						}
					}
				}
				clientState.mu.Unlock()
			}
			// --- End Example ---

			// --- Example: Send dummy input periodically ---
			// inputReq := &proto.ClientRequest{
			// 	RequestType: &proto.ClientRequest_PlayerInput{
			// 		PlayerInput: &proto.PlayerInput{ MoveForward: true },
			// 	},
			// }
			// sendClientRequest(conn, inputReq)
			// --- End Example ---

		case <-interrupt: // Handle Ctrl+C
			log.Println("Interrupt received, shutting down.")
			cancel() // Signal goroutines to exit
			// Allow time for graceful shutdown attempt in main select loop
		}
	}
}

func handleServerMessage(data []byte) {
	msg := &proto.ServerMessage{}
	if err := protos.Unmarshal(data, msg); err != nil {
		log.Printf("Failed to unmarshal server message: %v", err)
		return
	}

	clientState.mu.Lock()
	defer clientState.mu.Unlock()

	switch x := msg.MessageType.(type) {
	case *proto.ServerMessage_InitData:
		init := x.InitData
		log.Printf("Received InitData for %s", init.YourEntityId)
		clientState.myEntityID = init.YourEntityId
		clientState.worldInfo = init // Store world info
		if init.InitialState != nil {
			clientState.playerStates[init.YourEntityId] = init.InitialState
		}
		// Could store color map if needed client-side beyond rendering

	case *proto.ServerMessage_WorldChunk:
		chunk := x.WorldChunk
		key := strconv.Itoa(int(chunk.ChunkX)) + "," + strconv.Itoa(int(chunk.ChunkY))
		// log.Printf("Received chunk %s, compression: %s", key, chunk.Compression)

		var decompressed []byte
		// var err error
		if chunk.Compression == proto.CompressionType_ZLIB {
			r, err := zlib.NewReader(bytes.NewReader(chunk.TerrainData))
			if err != nil {
				log.Printf("Error creating zlib reader for chunk %s: %v", key, err)
				delete(clientState.pendingChunks, key) // Stop waiting if corrupt
				return
			}
			// Read all decompressed data
			buf := new(bytes.Buffer)
			_, err = buf.ReadFrom(r)
			r.Close()
			if err != nil {
				log.Printf("Error decompressing chunk %s: %v", key, err)
				delete(clientState.pendingChunks, key)
				return
			}
			decompressed = buf.Bytes()
		} else if chunk.Compression == proto.CompressionType_NONE {
			decompressed = chunk.TerrainData
		} else {
			log.Printf("Chunk %s has unknown compression %d", key, chunk.Compression)
			delete(clientState.pendingChunks, key)
			return
		}

		log.Printf("Stored chunk %s (%d decompressed bytes)", key, len(decompressed))
		clientState.worldChunks[key] = decompressed
		delete(clientState.pendingChunks, key) // Mark as received

	case *proto.ServerMessage_StateUpdate:
		update := x.StateUpdate
		clientState.lastUpdateMs = update.ServerTimestampMs
		// log.Printf("Received StateUpdate at %d with %d entities", update.ServerTimestampMs, len(update.Entities))

		currentVisible := make(map[string]bool)
		for _, state := range update.Entities {
			if state != nil {
				clientState.playerStates[state.EntityId] = state
				currentVisible[state.EntityId] = true
			}
		}

		// Simple console output of current player state
		if myState, ok := clientState.playerStates[clientState.myEntityID]; ok && myState.Position != nil {
			log.Printf("MyPos: (%.1f, %.1f), Players: %d", myState.Position.X, myState.Position.Y, len(clientState.playerStates))
		}

		// Remove players not present in the update (simple cleanup)
		idsToRemove := []string{}
		for id := range clientState.playerStates {
			if !currentVisible[id] {
				idsToRemove = append(idsToRemove, id)
			}
		}
		for _, id := range idsToRemove {
			delete(clientState.playerStates, id)
		}
		// Could also process update.RemovedEntityIds if server sent them

	default:
		log.Printf("Received unknown server message type")
	}
}

func sendClientRequest(conn *websocket.Conn, req *proto.ClientRequest) {
	data, err := protos.Marshal(req)
	if err != nil {
		log.Printf("Failed to marshal client request: %v", err)
		return
	}
	conn.SetWriteDeadline(time.Now().Add(writeWaitClient))
	err = conn.WriteMessage(websocket.BinaryMessage, data)
	if err != nil {
		log.Printf("Error sending client request: %v", err)
		// Consider triggering disconnect/reconnect logic here
	}
}

// --- Client-side constants and helpers (mirror some server logic) ---
const (
	writeWaitClient  = 10 * time.Second
	pongWaitClient   = 60 * time.Second
	pingPeriodClient = (pongWaitClient * 9) / 10
)

func getClientGridCellCoords(worldX, worldY float32, info *proto.InitData) (cx, cy int32) {
	if info == nil || info.ChunkSizeTiles == 0 || info.TileRenderSize == 0 {
		return 0, 0 // Cannot calculate without world info
	}
	cellSizeX := float64(info.ChunkSizeTiles * info.TileRenderSize)
	cellSizeY := float64(info.ChunkSizeTiles * info.TileRenderSize)
	cx = int32(math.Floor(float64(worldX) / cellSizeX))
	cy = int32(math.Floor(float64(worldY) / cellSizeY))
	return
}

func getClientAoICellKeys(gridCX, gridCY int32, radius int) map[string]struct{} {
	keys := make(map[string]struct{})
	r32 := int32(radius)
	for cy := gridCY - r32; cy <= gridCY+r32; cy++ {
		for cx := gridCX - r32; cx <= gridCX+r32; cx++ {
			keys[strconv.Itoa(int(cx))+","+strconv.Itoa(int(cy))] = struct{}{}
		}
	}
	return keys
}

func parseClientGridKey(key string) (cx, cy int32, ok bool) {
	// Duplicate of server helper - move to shared package ideally
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
