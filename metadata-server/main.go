package main

import (
	"context"
	"flag"
	stlog "log/slog" // Use slog
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"

	pb "github.com/irishsmurf/go-mmo-poc/proto" // UPDATE PATH

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/reflection" // Enable server reflection
	"google.golang.org/grpc/status"
)

var (
	grpcAddr = flag.String("addr", ":50051", "gRPC service address")
	logLevel = flag.String("log", "info", "Log level (debug, info, warn, error)")
)

// metadataServer implements the proto.MetadataServiceServer interface.
type metadataServer struct {
	pb.UnimplementedMetadataServiceServer // Embed for forward compatibility
	mu                                    sync.RWMutex
	playerData                            map[string]string // player_id -> display_name (In-memory store for PoC)
	logger                                *stlog.Logger
}

// NewMetadataServer creates a new server instance.
func NewMetadataServer(logger *stlog.Logger) *metadataServer {
	if logger == nil {
		logger = stlog.Default()
	}
	return &metadataServer{
		playerData: make(map[string]string),
		logger:     logger.With("component", "metadata-server"),
	}
}

// GetPlayerName retrieves a player's display name.
func (s *metadataServer) GetPlayerName(ctx context.Context, req *pb.GetPlayerNameRequest) (*pb.GetPlayerNameResponse, error) {
	playerID := req.GetPlayerId()
	if playerID == "" {
		return nil, status.Errorf(codes.InvalidArgument, "player_id cannot be empty")
	}

	s.mu.RLock()
	name, found := s.playerData[playerID]
	s.mu.RUnlock()

	s.logger.Debug("GetPlayerName called", "playerId", playerID, "found", found, "name", name)

	if !found {
		// Return a default name or indicate not found
		return &pb.GetPlayerNameResponse{
			PlayerId:    playerID,
			DisplayName: "Player_" + playerID[:4], // Example default
			Found:       false,
		}, nil
	}

	return &pb.GetPlayerNameResponse{
		PlayerId:    playerID,
		DisplayName: name,
		Found:       true,
	}, nil
}

// SetPlayerName sets or updates a player's display name.
func (s *metadataServer) SetPlayerName(ctx context.Context, req *pb.SetPlayerNameRequest) (*pb.SetPlayerNameResponse, error) {
	playerID := req.GetPlayerId()
	displayName := req.GetDisplayName()

	if playerID == "" || displayName == "" {
		return nil, status.Errorf(codes.InvalidArgument, "player_id and display_name cannot be empty")
	}
	// Add more validation for display name (length, characters etc.) if needed

	s.mu.Lock()
	s.playerData[playerID] = displayName
	s.mu.Unlock()

	s.logger.Info("SetPlayerName called", "playerId", playerID, "displayName", displayName)

	return &pb.SetPlayerNameResponse{Success: true}, nil
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
	metaSrv := NewMetadataServer(logger)
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
