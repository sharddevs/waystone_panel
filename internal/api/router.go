// SPDX-License-Identifier: AGPL-3.0-only

// Package api is the controller's HTTP API. It is only ever served on a
// Unix socket: the caller is identified by the kernel (SO_PEERCRED), not
// by anything in the request.
package api

import (
	"log"
	"net/http"
	"time"

	"github.com/sharddevs/waystone_panel/internal/store"
)

// Config is what New needs. Store is required; the rest have defaults.
type Config struct {
	Version string           // what /healthz reports
	Store   *store.Store     // the database, as waystone_app
	Lookup  LookupName       // uid to login name; nil means the passwd database
	Log     *log.Logger      // nil means log.Default()
	Now     func() time.Time // clock for the rate limit; nil means time.Now
	// OnOperation, if set, is called with each new operation once it has
	// committed, never for a replay. The controller uses it to wake the
	// node's agent session (4c, Q7). It must not block.
	OnOperation func(store.Operation)
}

// API holds what the handlers share.
type API struct {
	version string
	store   *store.Store
	lookup  LookupName
	log     *log.Logger
	now     func() time.Time
	limiter *rateLimiter

	onOperation func(store.Operation)
}

func (a *API) logf(format string, args ...any) { a.log.Printf(format, args...) }

// New returns the controller's HTTP handler.
//
// Every request, from the outside in, goes through: withRequestID,
// logRequests, withRecover, limitConcurrency (MaxConcurrent; busy →
// server_busy), then the mux. The /api/v1 routes are also wrapped in
// withActor and then withRateLimit; /healthz isn't, because it needs no
// identity.
//
// Routes (Go 1.22 patterns):
//
//	GET  /healthz
//	GET  /api/v1/servers
//	POST /api/v1/servers/{id}/start      (also stop, restart)
//	GET  /api/v1/operations
//	GET  /api/v1/operations/{id}
//	POST /api/v1/operations/{id}/resolve
//	/    anything else: not_found, as JSON
//
// TODO: fill in the defaults, build the mux, and wrap it as described.
func New(cfg Config) http.Handler {
	if cfg.Lookup == nil {
		cfg.Lookup = systemLookup
	}
	if cfg.Log == nil {
		cfg.Log = log.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}

	a := &API{
		version: cfg.Version,
		store:   cfg.Store,
		lookup:  cfg.Lookup,
		log:     cfg.Log,
		now:     cfg.Now,
		limiter: newRateLimiter(RateLimit, RateWindow),

		onOperation: cfg.OnOperation,
	}

	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", healthz(a.version))

	apiMux := http.NewServeMux()

	apiMux.HandleFunc("GET /api/v1/servers", a.handleListServers)

	apiMux.HandleFunc(
		"POST /api/v1/servers/{id}/start",
		a.handleAction(store.ActionStart, "/api/v1/servers/{id}/start"),
	)

	apiMux.HandleFunc(
		"POST /api/v1/servers/{id}/stop",
		a.handleAction(store.ActionStop, "/api/v1/servers/{id}/stop"),
	)

	apiMux.HandleFunc(
		"POST /api/v1/servers/{id}/restart",
		a.handleAction(store.ActionRestart, "/api/v1/servers/{id}/restart"),
	)

	apiMux.HandleFunc("GET /api/v1/operations", a.handleListOperations)
	apiMux.HandleFunc("GET /api/v1/operations/{id}", a.handleGetOperation)
	apiMux.HandleFunc("POST /api/v1/operations/{id}/resolve", a.handleResolve)

	apiMux.HandleFunc("/", a.handleNotFound)

	mux.Handle("/api/v1/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, pattern := apiMux.Handler(r)

		if pattern == "/" {
			a.handleNotFound(w, r)
			return
		}

		a.withActor(a.withRateLimit(apiMux)).ServeHTTP(w, r)
	}))
	mux.HandleFunc("/", a.handleNotFound)

	busy := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.writeError(w, r, newError(CodeServerBusy, nil))
	})

	var handler http.Handler = mux
	handler = limitConcurrency(MaxConcurrent, busy, handler)
	handler = a.withRecover(handler)
	handler = a.logRequests(handler)
	handler = withRequestID(handler)

	return handler
}
