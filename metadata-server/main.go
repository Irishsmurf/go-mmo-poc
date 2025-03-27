package main

import (
	"context"
	"flag"
	"hash/maphash"
	stlog "log/slog" // Use slog
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	pb "github.com/irishsmurf/go-mmo-poc/proto"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/reflection" // Enable server reflection
	"google.golang.org/grpc/status"
)

var (
	grpcAddr  = flag.String("addr", ":50051", "gRPC service address")
	logLevel  = flag.String("log", "info", "Log level (debug, info, warn, error)")
	numShards = flag.Int("shards", 32, "Number of concurrent map shards")
)

type playerShardData struct {
	names    map[string]string               // player_id -> display_name
	statuses map[string]*pb.PlayerStatusInfo // player_id -> status info (use pointer for easier updates)
}

type mapShard struct {
	mu   sync.RWMutex
	data playerShardData
}

// metadataServer implements the proto.MetadataServiceServer interface.
type metadataServer struct {
	pb.UnimplementedMetadataServiceServer // Embed for forward compatibility
	shards                                []*mapShard
	hasher                                maphash.Hash
	logger                                *stlog.Logger
}

// NewMetadataServer creates a new server instance with sharded maps.
func NewMetadataServer(logger *stlog.Logger, shardCount int) *metadataServer {
	if logger == nil {
		logger = stlog.Default()
	}
	if shardCount <= 0 {
		shardCount = 32
	}

	s := &metadataServer{
		shards: make([]*mapShard, shardCount),
		logger: logger.With("component", "metadata-server"),
	}

	for i := 0; i < shardCount; i++ {
		s.shards[i] = &mapShard{
			data: playerShardData{
				names:    make(map[string]string),               // Initialize the inner 'names' map
				statuses: make(map[string]*pb.PlayerStatusInfo), // Initialize the inner 'statuses' map
			},
		}
	}
	logger.Info("Initialized metadata server", "shards", shardCount)
	return s
}

// getShard determines the appropriate shard for a given key.
func (s *metadataServer) getShard(key string) *mapShard {
	// Use maphash for good distribution
	var h maphash.Hash         // Get a new hash instance (lightweight)
	h.SetSeed(s.hasher.Seed()) // Use the server's seed
	h.WriteString(key)
	hashValue := h.Sum64()
	shardIndex := hashValue % uint64(len(s.shards)) // Modulo operation
	return s.shards[shardIndex]
}

// GetPlayerName retrieves a player's display name from the correct shard.
func (s *metadataServer) GetPlayerName(ctx context.Context, req *pb.GetPlayerNameRequest) (*pb.GetPlayerNameResponse, error) {
	playerID := req.GetPlayerId()
	if playerID == "" { /* ... error ... */
	}

	shard := s.getShard(playerID)
	shard.mu.RLock()
	name, found := shard.data.names[playerID] // Access names map
	shard.mu.RUnlock()

	s.logger.Debug("GetPlayerName called", "playerId", playerID, "found", found)

	if !found {
		return &pb.GetPlayerNameResponse{PlayerId: playerID, DisplayName: "Player_" + playerID[:4], Found: false}, nil
	}
	return &pb.GetPlayerNameResponse{PlayerId: playerID, DisplayName: name, Found: true}, nil
}

// SetPlayerName sets or updates a player's display name in the correct shard.
func (s *metadataServer) SetPlayerName(ctx context.Context, req *pb.SetPlayerNameRequest) (*pb.SetPlayerNameResponse, error) {
	playerID := req.GetPlayerId()
	displayName := req.GetDisplayName()
	if playerID == "" || displayName == "" { /* ... error ... */
	}

	shard := s.getShard(playerID)
	shard.mu.Lock()
	shard.data.names[playerID] = displayName // Access names map
	shard.mu.Unlock()

	s.logger.Info("SetPlayerName called", "playerId", playerID, "displayName", displayName)
	return &pb.SetPlayerNameResponse{Success: true}, nil
}

func (s *metadataServer) SetPlayerStatus(ctx context.Context, req *pb.SetPlayerStatusRequest) (*pb.SetPlayerStatusResponse, error) {
	playerID := req.GetPlayerId()
	statusVal := req.GetStatus()

	if playerID == "" {
		return nil, status.Errorf(codes.InvalidArgument, "player_id cannot be empty")
	}
	// Validate status enum if necessary (gRPC usually handles unknown enum values)
	if _, ok := pb.OnlineStatus_name[int32(statusVal)]; !ok && statusVal != pb.OnlineStatus_STATUS_UNKNOWN {
		return nil, status.Errorf(codes.InvalidArgument, "invalid status value: %d", statusVal)
	}

	shard := s.getShard(playerID)
	shard.mu.Lock() // Lock shard for writing status

	nowMs := time.Now().UnixMilli() // Get current timestamp

	// Get existing status info or create new
	statusInfo, exists := shard.data.statuses[playerID]
	if !exists {
		statusInfo = &pb.PlayerStatusInfo{PlayerId: playerID}
		shard.data.statuses[playerID] = statusInfo // Add to map if new
	}

	// Update status and timestamp
	statusInfo.Status = statusVal
	statusInfo.LastSeenTimestampMs = nowMs
	// statusInfo.ServerId = req.GetServerId() // Update server ID if provided

	shard.mu.Unlock()

	s.logger.Info("SetPlayerStatus called", "playerId", playerID, "status", statusVal.String())

	return &pb.SetPlayerStatusResponse{Success: true}, nil
}

func (s *metadataServer) GetPlayerStatus(ctx context.Context, req *pb.GetPlayerStatusRequest) (*pb.GetPlayerStatusResponse, error) {
	playerIDs := req.GetPlayerIds()
	if len(playerIDs) == 0 {
		return nil, status.Errorf(codes.InvalidArgument, "player_ids cannot be empty")
	}

	responseMap := make(map[string]*pb.PlayerStatusInfo)

	// This could be parallelized further if needed, but sharding helps a lot.
	// Group IDs by shard first for fewer lock acquisitions if many IDs requested.
	idsByShard := make(map[int][]*string) // Map shard index to list of IDs
	shardIndices := make(map[string]int)  // Map player ID to its shard index

	for i := range playerIDs {
		id := playerIDs[i] // Avoid closure capture issue if using goroutines later
		// Calculate shard index - needed if parallelizing reads later
		var h maphash.Hash
		h.SetSeed(s.hasher.Seed())
		h.WriteString(id)
		idx := int(h.Sum64() % uint64(len(s.shards)))
		idsByShard[idx] = append(idsByShard[idx], &id)
		shardIndices[id] = idx
	}

	// Iterate through shards that have requested IDs
	for shardIndex, idsInShard := range idsByShard {
		shard := s.shards[shardIndex]
		shard.mu.RLock() // Read lock the shard

		for _, idPtr := range idsInShard {
			playerID := *idPtr
			statusInfo, found := shard.data.statuses[playerID]
			if found {
				// IMPORTANT: Create a COPY of the status info before putting it in the response map,
				// otherwise, all responses might point to the same underlying struct which could be modified later.
				// Using proto.Clone is the safest way if the proto is complex.
				// statusCopy := proto.Clone(statusInfo).(*pb.PlayerStatusInfo) // Safest
				statusCopy := &pb.PlayerStatusInfo{ // Manual copy for simple struct
					PlayerId:            statusInfo.PlayerId,
					Status:              statusInfo.Status,
					LastSeenTimestampMs: statusInfo.LastSeenTimestampMs,
					// ServerId: statusInfo.ServerId,
				}
				responseMap[playerID] = statusCopy
			} else {
				// Optionally return default OFFLINE status if not found
				responseMap[playerID] = &pb.PlayerStatusInfo{
					PlayerId: playerID,
					Status:   pb.OnlineStatus_STATUS_OFFLINE, // Or STATUS_UNKNOWN
					// LastSeenTimestampMs: 0, // Or leave unset
				}
			}
		}
		shard.mu.RUnlock()
	}

	s.logger.Debug("GetPlayerStatus called", "requestedCount", len(playerIDs), "foundCount", len(responseMap))

	return &pb.GetPlayerStatusResponse{Statuses: responseMap}, nil
}

func main() {
	flag.Parse()

	// --- Setup Logging ---
	var leveler stlog.LevelVar
	switch *logLevel { // Use level from flag
	case "debug":
		leveler.Set(stlog.LevelDebug)
	case "warn":
		leveler.Set(stlog.LevelWarn)
	case "error":
		leveler.Set(stlog.LevelError)
	default:
		leveler.Set(stlog.LevelInfo)
	}
	logger := stlog.New(stlog.NewJSONHandler(os.Stdout, &stlog.HandlerOptions{Level: &leveler}))
	stlog.SetDefault(logger) // Use this logger globally in this service
	// --- End Logging ---

	lis, err := net.Listen("tcp", *grpcAddr)
	if err != nil {
		logger.Error("Failed to listen for gRPC", "address", *grpcAddr, "error", err)
		os.Exit(1)
	}

	grpcServer := grpc.NewServer()
	metaSrv := NewMetadataServer(logger, *numShards)
	pb.RegisterMetadataServiceServer(grpcServer, metaSrv)

	// Enable reflection for tools like grpcurl
	reflection.Register(grpcServer)

	logger.Info("Metadata gRPC server starting", "address", *grpcAddr)

	// --- Graceful Shutdown Handling ---
	go func() {
		if err := grpcServer.Serve(lis); err != nil {
			logger.Error("gRPC server failed to serve", "error", err)
		}
	}()

	// Wait for termination signal
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit // Block until signal received

	logger.Info("Shutting down gRPC server...")
	grpcServer.GracefulStop() // Attempt graceful shutdown
	logger.Info("gRPC server stopped.")
}
