package main

import (
	"flag"
	"log"            // Keep standard log for initial startup errors
	stlog "log/slog" // Use slog for structured logging
	"math/rand"
	"net/http"
	_ "net/http/pprof"
	"os" // For hostname
	"time"

	"github.com/irishsmurf/go-mmo-poc/server" // UPDATE PATH

	"github.com/prometheus/client_golang/prometheus/promhttp" // Prometheus handler
)

var addr = flag.String("addr", ":8080", "WebSocket service address")
var pprofAddr = flag.String("pprof", ":6060", "pprof http service address")
var metricsAddr = flag.String("metrics", ":9091", "Prometheus metrics http service address") // Metrics endpoint
var logLevel = flag.String("log", "info", "Log level (debug, info, warn, error)")

func main() {
	flag.Parse()
	rand.Seed(time.Now().UnixNano())

	// --- Setup Structured Logging (slog) ---
	var leveler stlog.LevelVar
	switch *logLevel {
	case "debug":
		leveler.Set(stlog.LevelDebug)
	case "warn":
		leveler.Set(stlog.LevelWarn)
	case "error":
		leveler.Set(stlog.LevelError)
	default:
		leveler.Set(stlog.LevelInfo)
	}
	hostname, _ := os.Hostname() // Add hostname for context
	logger := stlog.New(stlog.NewJSONHandler(os.Stdout, &stlog.HandlerOptions{
		Level: &leveler,
		ReplaceAttr: func(groups []string, a stlog.Attr) stlog.Attr {
			// Add hostname to all logs
			if a.Key == stlog.TimeKey && len(groups) == 0 {
				return stlog.Group("ctx", stlog.String("host", hostname), a)
			}
			return a
		},
	}))
	stlog.SetDefault(logger) // Set as default logger
	// --- End Logging Setup ---

	hub := server.NewHub(logger) // Pass logger to Hub
	go hub.Run()

	// --- Start pprof server ---
	go func() {
		stlog.Info("Starting pprof HTTP server", "address", *pprofAddr)
		// Use standard log for this critical startup error, slog might not be fully ready
		if err := http.ListenAndServe(*pprofAddr, nil); err != nil {
			log.Fatalf("Pprof ListenAndServe error: %v", err)
		}
	}()

	// --- Start Prometheus metrics server ---
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.Handler()) // Expose Prometheus metrics
	go func() {
		stlog.Info("Starting Prometheus metrics HTTP server", "address", *metricsAddr)
		if err := http.ListenAndServe(*metricsAddr, metricsMux); err != nil {
			log.Fatalf("Metrics ListenAndServe error: %v", err)
		}
	}()

	// --- Start WebSocket server ---
	wsMux := http.NewServeMux()
	wsMux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		server.ServeWs(hub, w, r) // ServeWs needs access to the hub
	})

	stlog.Info("Starting WebSocket server", "address", *addr)
	// Use standard log for this critical startup error
	if err := http.ListenAndServe(*addr, wsMux); err != nil {
		log.Fatal("WebSocket ListenAndServe: ", err)
	}
}
