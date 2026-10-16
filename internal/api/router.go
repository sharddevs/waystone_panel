// SPDX-License-Identifier: AGPL-3.0-only

// Package api is the controller's HTTP API. It is only ever served on a
// Unix socket: the caller is identified by the kernel (SO_PEERCRED), not
// by anything in the request. The routes themselves are internal/service,
// which web.sock serves too (stage 5b).
package api

import (
	"log"
	"net/http"
	"time"

	"github.com/sharddevs/waystone_panel/internal/service"
	"github.com/sharddevs/waystone_panel/internal/store"
)

// Config is what New needs. Store is required unless Service is set; the
// rest have defaults.
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
	// Service, if set, serves the routes, and Store, Now and OnOperation
	// are not used: the controller shares one service between api.sock
	// and web.sock. Nil means New builds one from those three and Log.
	Service *service.Service
}

// API holds what the socket layer needs; the handlers are the service's.
type API struct {
	version string
	lookup  LookupName
	log     *log.Logger
	svc     *service.Service
}

func (a *API) logf(format string, args ...any) { a.log.Printf(format, args...) }

// New returns the controller's HTTP handler.
//
// Every request, from the outside in, goes through: AssignRequestID,
// logRequests, Recover, LimitConcurrency (MaxConcurrent slots of its
// own, not web.sock's; busy → controller_busy), then the mux. The /api/v1 routes are
// also wrapped in withActor and then LimitRate; /healthz isn't, because
// it needs no identity.
//
// Routes (Go 1.22 patterns): GET /healthz, the service's routes (see
// service.Routes), and anything else not_found, as JSON.
func New(cfg Config) http.Handler {
	if cfg.Lookup == nil {
		cfg.Lookup = systemLookup
	}
	if cfg.Log == nil {
		cfg.Log = log.Default()
	}
	if cfg.Service == nil {
		cfg.Service = service.New(service.Config{Store: cfg.Store, Log: cfg.Log, Now: cfg.Now, OnOperation: cfg.OnOperation})
	}

	a := &API{
		version: cfg.Version,
		lookup:  cfg.Lookup,
		log:     cfg.Log,
		svc:     cfg.Service,
	}

	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", healthz(a.version))

	apiMux := http.NewServeMux()
	for _, rt := range a.svc.Routes() {
		apiMux.Handle(rt.Method+" "+rt.Pattern, rt.Handler)
	}
	apiMux.HandleFunc("/", a.svc.NotFound)

	mux.Handle("/api/v1/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, pattern := apiMux.Handler(r)

		if pattern == "/" {
			a.svc.NotFound(w, r)
			return
		}

		a.withActor(a.svc.LimitRate(apiMux)).ServeHTTP(w, r)
	}))
	mux.HandleFunc("/", a.svc.NotFound)

	var handler http.Handler = mux
	handler = a.svc.LimitConcurrency(service.NewSlots(service.MaxConcurrent), handler)
	handler = a.svc.Recover(handler)
	handler = a.logRequests(handler)
	handler = service.AssignRequestID(handler)

	return handler
}
