package game

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"image"
	"image/color"
	"log"
	"math"
	"math/rand"
	"os"
	"path/filepath"
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

	baseMapImage image.Image
	mapImagePath string = "map.png"
	mapLoaded    bool   = false
)

type ChunkData struct {
	protoData    *proto.WorldChunk
	terrainBytes []byte
}

var colorToTerrain = map[color.RGBA]proto.TerrainType{
	{R: 34, G: 139, B: 34, A: 255}:  proto.TerrainType_GRASS, // ForestGreen
	{R: 160, G: 82, B: 45, A: 255}:  proto.TerrainType_CLAY,  // Sienna
	{R: 244, G: 164, B: 96, A: 255}: proto.TerrainType_SAND,  // SandyBrown
	{R: 70, G: 130, B: 180, A: 255}: proto.TerrainType_WATER, // SteelBlue
	// Add more terrain types...
}

// Optional: Mapping for objects
type ObjectType int // Define an enum or consts for objects
const (
	ObjectTypeNone ObjectType = iota
	ObjectTypeTree
	ObjectTypeRock
	ObjectTypeSpawnPoint
)

var colorToObject = map[color.RGBA]ObjectType{
	{R: 139, G: 69, B: 19, A: 255}:   ObjectTypeTree,       // SaddleBrown
	{R: 128, G: 128, B: 128, A: 255}: ObjectTypeRock,       // Gray
	{R: 255, G: 0, B: 0, A: 255}:     ObjectTypeSpawnPoint, // Red for spawn points
}

// Default if color doesn't match any mapping
const defaultTerrain = proto.TerrainType_GRASS

func init() { // Initialize color map once
	terrainColorMapProto = &proto.TerrainColorMap{
		TypeToColor: make(map[int32]*proto.ColorDefinition),
	}
	terrainColorMapProto.TypeToColor[int32(proto.TerrainType_GRASS)] = &proto.ColorDefinition{HexColor: "#2a9d8f"}
	terrainColorMapProto.TypeToColor[int32(proto.TerrainType_CLAY)] = &proto.ColorDefinition{HexColor: "#e76f51"}
	terrainColorMapProto.TypeToColor[int32(proto.TerrainType_SAND)] = &proto.ColorDefinition{HexColor: "#e9c46a"}
	terrainColorMapProto.TypeToColor[int32(proto.TerrainType_WATER)] = &proto.ColorDefinition{HexColor: "#219ebc"}

	err := LoadMapImage(mapImagePath)
	if err != nil {
		log.Printf("Error loading map image: %v", err)
	} else {
		log.Printf("Map image loaded successfully - %s (%dx%d)", mapImagePath, baseMapImage.Bounds().Dx(), baseMapImage.Bounds().Dy())
		if baseMapImage.Bounds().Dx() < WorldTileWidth || baseMapImage.Bounds().Dy() < WorldTileHeight {
			log.Printf("Warning: Loaded map image dimensions (%dx%d) are smaller than world dimensions (%dx%d).", baseMapImage.Bounds().Dx(), baseMapImage.Bounds().Dy(), WorldTileWidth, WorldTileHeight)
		}
	}
}

// LoadMapImage loads the base terrain/object map from file.
func LoadMapImage(filePath string) error {
	absPath, err := filepath.Abs(filePath)
	if err != nil {
		return fmt.Errorf("failed to get absolute path for %s: %w", filePath, err)
	}

	file, err := os.Open(absPath)
	if err != nil {
		return fmt.Errorf("failed to open map file %s: %w", absPath, err)
	}
	defer file.Close()

	img, format, err := image.Decode(file) // Decodes PNG, JPG, GIF if decoders imported
	if err != nil {
		return fmt.Errorf("failed to decode map image %s: %w", absPath, err)
	}
	log.Printf("Map image format: %s", format)

	// Convert to RGBA if not already (simplifies color checking)
	rgbaImg, ok := img.(*image.RGBA)
	if !ok {
		log.Printf("Converting map image to RGBA format")
		bounds := img.Bounds()
		rgbaImg = image.NewRGBA(bounds)
		for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
			for x := bounds.Min.X; x < bounds.Max.X; x++ {
				rgbaImg.Set(x, y, img.At(x, y))
			}
		}
	}

	baseMapImage = rgbaImg // Store the RGBA image
	mapLoaded = true
	return nil
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

func GetOrGenerateChunk(cx, cy int32) (*proto.WorldChunk, error) { // Return error if map not loaded
	key := strconv.Itoa(int(cx)) + "," + strconv.Itoa(int(cy))

	worldChunksMutex.RLock()
	chunkData, exists := worldChunks[key]
	worldChunksMutex.RUnlock()

	if exists {
		return chunkData.protoData, nil // Return cached proto data
	}

	// --- Generate if not exists ---
	worldChunksMutex.Lock()
	defer worldChunksMutex.Unlock()

	// Double check
	chunkData, exists = worldChunks[key]
	if exists {
		return chunkData.protoData, nil
	}

	// --- Check if map image is loaded ---
	if !mapLoaded || baseMapImage == nil {
		return nil, fmt.Errorf("map image '%s' not loaded, cannot generate chunk %d,%d", mapImagePath, cx, cy)
	}

	log.Printf("Generating chunk %d,%d from map image", cx, cy)
	terrainBytes := make([]byte, ChunkSizeTiles*ChunkSizeTiles)
	// chunkObjects := []ObjectInstance{} // If tracking objects

	mapMaxX := baseMapImage.Bounds().Max.X
	mapMaxY := baseMapImage.Bounds().Max.Y

	for tileY := 0; tileY < ChunkSizeTiles; tileY++ {
		for tileX := 0; tileX < ChunkSizeTiles; tileX++ {
			// Calculate corresponding pixel coordinates in the source image
			// Assuming 1 pixel = 1 tile
			pixelX := int(cx)*ChunkSizeTiles + tileX
			pixelY := int(cy)*ChunkSizeTiles + tileY

			idx := tileY*ChunkSizeTiles + tileX
			terrain := defaultTerrain // Default

			// Check image bounds
			if pixelX >= 0 && pixelX < mapMaxX && pixelY >= 0 && pixelY < mapMaxY {
				// Get pixel color (already RGBA due to conversion in LoadMapImage)
				// Adjust if baseMapImage isn't guaranteed RGBA
				c := baseMapImage.At(pixelX, pixelY).(color.RGBA)

				// --- Map Color to Terrain/Object ---
				foundMapping := false
				// Check Objects first (optional, depends if objects "override" terrain)
				if objType, ok := colorToObject[c]; ok {
					// Store object data if needed
					// chunkObjects = append(chunkObjects, ObjectInstance{Type: objType, TileX: tileX, TileY: tileY})
					// Decide if object implies a specific underlying terrain (e.g., rock is on 'clay')
					// Or just use the object color and assign default terrain below?
					// For simplicity now, let's prioritize terrain mapping
					// Remove this block if terrain color should take precedence
					foundMapping = true      // Mark that we found *an* object mapping
					terrain = defaultTerrain // Or assign specific terrain based on object
				}

				// Check Terrain (only if no object found or if terrain takes precedence)
				// if !foundMapping { // Uncomment if objects override terrain visually
				if terrType, ok := colorToTerrain[c]; ok {
					terrain = terrType
					foundMapping = true
				}
				// }

				if !foundMapping {
					// Optional: Log unknown colors once per color?
					// log.Printf("Unknown color %v at pixel %d,%d", c, pixelX, pixelY)
					terrain = defaultTerrain
				}

			} else {
				// Out of image bounds - treat as water or default
				terrain = proto.TerrainType_WATER
			}

			terrainBytes[idx] = byte(terrain)
		}
	}

	// --- Compress Terrain Data ---
	var b bytes.Buffer
	w := zlib.NewWriter(&b)
	_, err := w.Write(terrainBytes)
	if err != nil {
		// Handle compression error - maybe return error or uncompressed?
		log.Printf("Error compressing chunk %d,%d: %v. Storing uncompressed.", cx, cy, err)
		// Fallback or return error
		return nil, fmt.Errorf("failed to compress chunk %d,%d: %w", cx, cy, err)
	}
	w.Close()
	compressedData := b.Bytes()
	// --- End Compression ---

	newProtoData := &proto.WorldChunk{
		ChunkX:      cx,
		ChunkY:      cy,
		TerrainData: compressedData,
		Compression: proto.CompressionType_ZLIB, // Assuming ZLIB
	}

	newChunkData := &ChunkData{
		protoData:    newProtoData,
		terrainBytes: terrainBytes, // Store decompressed bytes for server logic if needed
		// objects:      chunkObjects, // Store objects
	}

	worldChunks[key] = newChunkData
	return newProtoData, nil
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
