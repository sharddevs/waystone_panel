// SPDX-License-Identifier: AGPL-3.0-only

// Command controller runs the Waystone controller. For now it serves
// /healthz on localhost, and "controller migrate -db <dsn>" applies the
// database migrations.
package main

import (
	"log"
	"net/http"
	"os"
	"time"

	"github.com/sharddevs/waystone_panel/internal/api"
	"github.com/sharddevs/waystone_panel/internal/store"
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
	// "migrate" is a separate mode: it updates the schema and exits
	// without starting the server.
	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		os.Exit(runMigrate(os.Args[2:], os.Stdout, os.Stderr, store.Migrate))
	}
	server := newServer("0.1.0-alpha")
	log.Println("Waystone controller listening on", server.Addr)

	if err := server.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
