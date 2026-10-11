// Command controller runs the Waystone controller. For now it only serves
// /healthz on localhost.
package main

import (
	"log"
	"net/http"
	"time"

	"github.com/sharddevs/waystone_panel/internal/api"
)

// newServer is split out of main so the tests can check its settings
// without starting a listener. It listens on loopback only for now, and
// ReadHeaderTimeout stops a client from holding a connection open by
// sending its headers very slowly.
func newServer(version string) *http.Server {

	return &http.Server{
		Addr:              "127.0.0.1:8080",
		Handler:           api.NewRouter(version),
		ReadHeaderTimeout: 5 * time.Second,
	}
}
func main() {

	server := newServer("0.1.0-alpha")
	log.Println("Waystone controller listening on", server.Addr)

	if err := server.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
