package game

import (
	"bytes"
	"compress/zlib"
	"log"
	"math"
	"math/rand"
	"strconv"
	"sync"

	"github.com/irishsmurf/go-mmo-poc/proto" // Update with your module path
)

const (
	WorldTileWidth   = 1500
	WorldTileHeight  = 1500
	TileRenderSize   = 10
	ChunkSizeTiles   = 32
	NumChunkX        = (WorldTileWidth + ChunkSizeTiles - 1) / ChunkSizeTiles
	NumChunkY        = (WorldTileHeight + ChunkSizeTiles - 1) / ChunkSizeTiles
	AoIChunkRadius   = 1   // How many chunks away to send updates (1 = 3x3)
	ServerTickRateMs = 100 // 100ms = 10Hz
)

// Grid cell size matches chunk size for simplicity
var (
	SpatialGridCellSizeX = float64(ChunkSizeTiles * TileRenderSize)
	SpatialGridCellSizeY = float64(ChunkSizeTiles * TileRenderSize)
)

// --- World State ---
var (
	worldChunks      = make(map[string]*proto.WorldChunk)
	worldChunksMutex sync.RWMutex

	terrainColorMapProto *proto.TerrainColorMap // Initialized later
)

func init() { // Initialize color map once
	terrainColorMapProto = &proto.TerrainColorMap{
		TypeToColor: make(map[int32]*proto.ColorDefinition),
	}
	terrainColorMapProto.TypeToColor[int32(proto.TerrainType_GRASS)] = &proto.ColorDefinition{HexColor: "#2a9d8f"}
	terrainColorMapProto.TypeToColor[int32(proto.TerrainType_CLAY)] = &proto.ColorDefinition{HexColor: "#e76f51"}
	terrainColorMapProto.TypeToColor[int32(proto.TerrainType_SAND)] = &proto.ColorDefinition{HexColor: "#e9c46a"}
	terrainColorMapProto.TypeToColor[int32(proto.TerrainType_WATER)] = &proto.ColorDefinition{HexColor: "#219ebc"}
}

func GetTerrainColorMap() *proto.TerrainColorMap {
	return terrainColorMapProto
}

// GetGridCellCoords calculates the spatial grid cell containing the world coordinates.
func GetGridCellCoords(worldX, worldY float32) (cx, cy int32) {
	cx = int32(math.Floor(float64(worldX) / SpatialGridCellSizeX))
	cy = int32(math.Floor(float64(worldY) / SpatialGridCellSizeY))
	return
}

// GetAoICellKeys calculates the set of cell keys ("cx,cy") within the AoI radius.
func GetAoICellKeys(gridCX, gridCY int32, radius int) map[string]struct{} {
	keys := make(map[string]struct{})
	r32 := int32(radius)
	for cy := gridCY - r32; cy <= gridCY+r32; cy++ {
		for cx := gridCX - r32; cx <= gridCX+r32; cx++ {
			keys[strconv.Itoa(int(cx))+","+strconv.Itoa(int(cy))] = struct{}{}
		}
	}
	return keys
}

func generateTerrainDataCompressed() ([]byte, proto.CompressionType) {
	ids := make([]byte, ChunkSizeTiles*ChunkSizeTiles)
	terrainTypes := []proto.TerrainType{
		proto.TerrainType_GRASS,
		proto.TerrainType_CLAY,
		proto.TerrainType_SAND,
		proto.TerrainType_WATER,
	}
	for i := range ids {
		ids[i] = byte(terrainTypes[rand.Intn(len(terrainTypes))])
	}

	var b bytes.Buffer
	w := zlib.NewWriter(&b)
	_, err := w.Write(ids)
	if err != nil {
		log.Printf("Error compressing chunk data: %v. Sending uncompressed.", err)
		return ids, proto.CompressionType_NONE // Fallback
	}
	w.Close() // Important: Flush compressed data
	return b.Bytes(), proto.CompressionType_ZLIB
}

// GetOrGenerateChunk retrieves or generates a world chunk. Thread-safe.
func GetOrGenerateChunk(cx, cy int32) *proto.WorldChunk {
	key := strconv.Itoa(int(cx)) + "," + strconv.Itoa(int(cy))

	worldChunksMutex.RLock()
	chunk, exists := worldChunks[key]
	worldChunksMutex.RUnlock()

	if exists {
		return chunk
	}

	// Generate if not exists (acquire write lock)
	worldChunksMutex.Lock()
	defer worldChunksMutex.Unlock()

	// Double check if another goroutine generated it while waiting for lock
	chunk, exists = worldChunks[key]
	if exists {
		return chunk
	}

	// log.Printf("Generating chunk %d,%d", cx, cy)
	newChunk := &proto.WorldChunk{
		ChunkX: cx,
		ChunkY: cy,
	}

	if cx < 0 || cx >= NumChunkX || cy < 0 || cy >= NumChunkY {
		// Boundary chunk (e.g., all water)
		terrainBytes := bytes.Repeat([]byte{byte(proto.TerrainType_WATER)}, ChunkSizeTiles*ChunkSizeTiles)
		var b bytes.Buffer
		w := zlib.NewWriter(&b)
		w.Write(terrainBytes)
		w.Close()
		newChunk.TerrainData = b.Bytes()
		newChunk.Compression = proto.CompressionType_ZLIB
	} else {
		newChunk.TerrainData, newChunk.Compression = generateTerrainDataCompressed()
	}

	worldChunks[key] = newChunk
	return newChunk
}

// Helper to create initial state (can be expanded)
func CreateInitialState(id string) *proto.EntityState {
	posX := rand.Float32() * float32(WorldTileWidth*TileRenderSize)
	posY := rand.Float32() * float32(WorldTileHeight*TileRenderSize)
	colorStr := "#" + strconv.FormatInt(rand.Int63n(0xFFFFFF+1), 16)

	// Ensure color has 6 digits with padding if necessary
	for len(colorStr) < 7 {
		colorStr = "#0" + colorStr[1:]
	}

	return &proto.EntityState{
		EntityId: id,
		Position: &proto.Vector2{X: posX, Y: posY},
		ColorHex: colorStr, // Use pointers for optional fields
	}
}

// ApplyInput updates player state based on input (basic simulation)
func ApplyInput(state *proto.EntityState, input *proto.PlayerInput) {
	// Replace with actual input processing & physics
	if input.MoveForward { // Example basic move
		// Assume orientation exists or is implicit (e.g., always forward)
		moveSpeed := float32(TileRenderSize * 0.8) // Adjust speed
		if state.Position != nil {
			// Simple forward movement along Y axis for demo
			state.Position.Y += moveSpeed
		}
	}
	// Apply turn delta if needed

	// Clamp position to world bounds
	if state.Position != nil {
		maxX := float32(WorldTileWidth*TileRenderSize) - 1
		maxY := float32(WorldTileHeight*TileRenderSize) - 1
		state.Position.X = max(0, min(state.Position.X, maxX))
		state.Position.Y = max(0, min(state.Position.Y, maxY))
	}
}

// Utility min/max for float32
func min(a, b float32) float32 {
	if a < b {
		return a
	}
	return b
}

func max(a, b float32) float32 {
	if a > b {
		return a
	}
	return b
}
