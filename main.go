package main

import (
	"flag"
	"log"
	"math/rand"
	"net/http"
	_ "net/http/pprof" // Import for side-effects (registers handlers)
	"time"

	"github.com/irishsmurf/go-mmo-poc/server" // Update path
)

var addr = flag.String("addr", ":8080", "http service address")
var pprofAddr = flag.String("pprof", ":6060", "pprof http service address") // Add pprof flag
var metadataAddr = flag.String("metadata", ":50051", "metadata gRPC service address")

func main() {
	flag.Parse()
	rand.Seed(time.Now().UnixNano()) // Seed random number generator

	hub := server.NewHub(*metadataAddr)
	go hub.Run() // Start the hub's processing loop

	// Start pprof server in a separate goroutine
	go func() {
		log.Printf("Starting pprof HTTP server on %s", *pprofAddr)
		if err := http.ListenAndServe(*pprofAddr, nil); err != nil {
			log.Fatalf("Pprof ListenAndServe error: %v", err)
		}
	}()

	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		server.ServeWs(hub, w, r)
	})

	log.Printf("Starting HTTP server on %s", *addr)
	err := http.ListenAndServe(*addr, nil)
	if err != nil {
		log.Fatal("ListenAndServe: ", err)
	}
}
