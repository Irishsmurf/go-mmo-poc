package main

import (
	"flag"
	"log"
	"math/rand"
	"net/http"
	"time"

	"github.com/irishsmurf/go-mmo-poc/server" // Update path
)

var addr = flag.String("addr", ":8080", "http service address")

func main() {
	flag.Parse()
	rand.Seed(time.Now().UnixNano()) // Seed random number generator

	hub := server.NewHub()
	go hub.Run() // Start the hub's processing loop

	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		server.ServeWs(hub, w, r)
	})

	log.Printf("Starting HTTP server on %s", *addr)
	err := http.ListenAndServe(*addr, nil)
	if err != nil {
		log.Fatal("ListenAndServe: ", err)
	}
}
