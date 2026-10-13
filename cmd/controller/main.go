// SPDX-License-Identifier: AGPL-3.0-only

// Command controller runs the Waystone controller. It serves the HTTP API
// on a Unix socket only, and "controller migrate -db <dsn>" applies the
// database migrations.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sharddevs/waystone_panel/internal/api"
	"github.com/sharddevs/waystone_panel/internal/sdlisten"
	"github.com/sharddevs/waystone_panel/internal/store"
)

const version = "0.1.0-alpha"

// defaultDSN is the controller's database: the waystone database as
// waystone_app, over the local socket with peer authentication.
const defaultDSN = "host=/var/run/postgresql dbname=waystone user=waystone_app"

// shutdownTimeout is how long in-flight requests get to finish after
// SIGTERM or SIGINT.
const shutdownTimeout = 10 * time.Second

func main() {
	// "migrate" is a separate mode: it updates the schema and exits
	// without starting the server.
	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		os.Exit(runMigrate(os.Args[2:], os.Stdout, os.Stderr, store.Migrate))
	}
	os.Exit(run(os.Args[1:], os.Stderr))
}

// newServer returns the http.Server for handler with the I12 limits:
// ReadHeaderTimeout 5 s, ReadTimeout 10 s, WriteTimeout 30 s, IdleTimeout
// 60 s, MaxHeaderBytes 16 KiB. ConnContext is api.ConnContext, which is
// what gives every request its caller's identity. There is no Addr: the
// server only ever runs on a listener we hand it.
//
// TODO
func newServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ConnContext:       api.ConnContext,
	}
}

// apiListener returns the API socket. With socketPath set (dev mode, the
// -api-socket flag) it calls listenDev. Without it the controller must
// have been started by systemd socket activation: sdlisten.Listener with
// the name "waystone-controller-api", pid and getenv.
//
// TODO
func apiListener(socketPath string, pid int, getenv func(string) string) (net.Listener, error) {
	if socketPath != "" {
		return listenDev(socketPath)
	}

	return sdlisten.Listener("waystone-controller-api", pid, getenv)
}

// run starts the controller and returns the exit code:
//   - 2 for bad flags;
//   - 1 if the database, the listener or the server fails;
//   - 0 after a clean shutdown on SIGTERM or SIGINT.
//
// Flags: -db (default defaultDSN) and -api-socket (dev mode, default "").
//
// TODO: parse flags (ContinueOnError, output to stderr); store.Open;
// apiListener(..., os.Getpid(), os.Getenv); serve api.New(...) with
// newServer; on a signal, Shutdown with shutdownTimeout. Log where it
// listens. Close the store on the way out.
func run(args []string, stderr io.Writer) int {
	flags := flag.NewFlagSet("controller", flag.ContinueOnError)
	flags.SetOutput(stderr)

	dsn := flags.String("db", defaultDSN, "PostgreSQL connection string")
	socketPath := flags.String("api-socket", "", "Development Unix socket path")

	flags.Usage = func() {
		fmt.Fprintln(stderr, "usage: controller [-db <dsn>] [-api-socket <path>]")
	}

	if err := flags.Parse(args); err != nil {
		return 2
	}

	if flags.NArg() != 0 || *dsn == "" {
		flags.Usage()
		return 2
	}

	ctx := context.Background()

	db, err := store.Open(ctx, *dsn)
	if err != nil {
		fmt.Fprintln(stderr, "database:", err)
		return 1
	}
	defer db.Close()

	listener, err := apiListener(*socketPath, os.Getpid(), os.Getenv)
	if err != nil {
		fmt.Fprintln(stderr, "API listener:", err)
		return 1
	}
	defer listener.Close()

	handler := api.New(api.Config{
		Version: version,
		Store:   db,
	})

	server := newServer(handler)

	fmt.Fprintf(stderr, "Waystone controller listening on %s\n", listener.Addr())

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)

	serveErrors := make(chan error, 1)

	go func() {
		serveErrors <- server.Serve(listener)
	}()

	select {
	case err := <-serveErrors:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(stderr, "HTTP server:", err)
			return 1
		}
		return 0

	case sig := <-signals:
		fmt.Fprintln(stderr, "received signal:", sig)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		fmt.Fprintln(stderr, "HTTP shutdown:", err)
		_ = server.Close()
		return 1
	}

	return 0
}
