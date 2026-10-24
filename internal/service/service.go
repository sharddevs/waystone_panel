// SPDX-License-Identifier: AGPL-3.0-only

// Package service is the server and operation API that both sockets
// serve (stage 5b, Q1): authorization, the audited run pattern, the
// handlers and their JSON. internal/api mounts it on api.sock with the
// actor from SO_PEERCRED; internal/browser mounts it on web.sock with the
// actor from the browser session. Whoever mounts it puts the actor in the
// request's context (WithActor) and gives the request an ID first; this
// package never looks at a header, a cookie or a peer to find out who is
// calling.
package service

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/sharddevs/waystone_panel/internal/ids"
	"github.com/sharddevs/waystone_panel/internal/store"
)

// Config is what New needs. Store is required for every route that
// touches the database; the rest have defaults.
type Config struct {
	Store *store.Store     // the database, as waystone_app
	Log   *log.Logger      // nil means log.Default()
	Now   func() time.Time // clock for the rate limit and staleness; nil means time.Now
	// OnOperation, if set, is called with each new operation once it has
	// committed, never for a replay. The controller uses it to wake the
	// node's agent session (4c, Q7). It must not block.
	OnOperation func(store.Operation)
	// AgentLink is the agent listener a new node is told to dial. Nil
	// means there is no TCP listener, and no token can be made.
	AgentLink *AgentLink
	// OnNodeRevoked, if set, is called with a node's ID once its
	// revocation has committed, never for one already revoked. The
	// controller uses it to cut the node's live stream (D5). It must not
	// block.
	OnNodeRevoked func(nodeID string)
	// ReadLog reads a server's log on its node (7a). Nil means there is
	// no agent link, and every read is node_unreachable.
	ReadLog LogReader
}

// Service holds what the handlers share. One Service serves both
// sockets, so the POST rate limit counts requests from both (it is per
// actor, and each identity is its own actor). MaxConcurrent is not
// shared: each socket brings its own Slots, so a browser can't fill
// api.sock's, the local admin's way in (5b audit H1).
type Service struct {
	store         *store.Store
	log           *log.Logger
	now           func() time.Time
	limiter       *rateLimiter
	onOperation   func(store.Operation)
	agentLink     *AgentLink
	onNodeRevoked func(string)
	readLog       LogReader
}

// New builds the service.
func New(cfg Config) *Service {
	if cfg.Log == nil {
		cfg.Log = log.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Service{
		store:         cfg.Store,
		log:           cfg.Log,
		now:           cfg.Now,
		limiter:       newRateLimiter(RateLimit, RateWindow),
		onOperation:   cfg.OnOperation,
		agentLink:     cfg.AgentLink,
		onNodeRevoked: cfg.OnNodeRevoked,
		readLog:       cfg.ReadLog,
	}
}

func (s *Service) logf(format string, args ...any) { s.log.Printf(format, args...) }

// Logf writes one line to the service's log.
func (s *Service) Logf(format string, args ...any) { s.logf(format, args...) }

// Route is one of the service's routes. Pattern is a path pattern as
// http.ServeMux understands it, without the method.
// commandRoute sends a console command (7b).
const commandRoute = "/api/v1/servers/{id}/command"

type Route struct {
	Method  string
	Pattern string
	Handler http.Handler
}

// Routes are the server, operation and node routes, the same list for both
// sockets. The caller wraps each handler so the request has an ID and an
// actor (and, for POST, passes the rate limit) before it runs.
func (s *Service) Routes() []Route {
	action := func(a store.Action, verb string) Route {
		p := "/api/v1/servers/{id}/" + verb
		return Route{http.MethodPost, p, s.handleAction(a, p)}
	}
	return []Route{
		{http.MethodGet, "/api/v1/servers", http.HandlerFunc(s.handleListServers)},
		{http.MethodPost, createRoute, http.HandlerFunc(s.handleCreateServer)},
		{http.MethodGet, "/api/v1/servers/{id}", http.HandlerFunc(s.handleGetServer)},
		{http.MethodGet, "/api/v1/servers/{id}/logs", http.HandlerFunc(s.handleServerLogs)},
		action(store.ActionStart, "start"),
		action(store.ActionStop, "stop"),
		action(store.ActionRestart, "restart"),
		action(store.ActionInstall, "install"),
		action(store.ActionAcceptEula, "accept-eula"),
		{http.MethodPost, commandRoute, s.handleCommand(commandRoute)},
		{http.MethodGet, "/api/v1/operations", http.HandlerFunc(s.handleListOperations)},
		{http.MethodGet, "/api/v1/operations/{id}", http.HandlerFunc(s.handleGetOperation)},
		{http.MethodPost, "/api/v1/operations/{id}/resolve", http.HandlerFunc(s.handleResolve)},
		{http.MethodGet, "/api/v1/nodes", http.HandlerFunc(s.handleListNodes)},
		{http.MethodGet, "/api/v1/nodes/{id}", http.HandlerFunc(s.handleGetNode)},
		{http.MethodPost, "/api/v1/nodes/{id}/revoke", http.HandlerFunc(s.handleRevokeNode)},
		{http.MethodGet, "/api/v1/nodes/enrollments", http.HandlerFunc(s.handleListEnrollments)},
		{http.MethodPost, "/api/v1/nodes/enrollments", http.HandlerFunc(s.handleCreateEnrollment)},
	}
}

type requestIDKey struct{}
type actorKey struct{}

// RequestIDFrom returns the request's ID, or "" outside AssignRequestID.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

func requestIDFrom(ctx context.Context) string { return RequestIDFrom(ctx) }

// WithActor returns ctx carrying the actor the handlers act as. Only the
// socket layers call it, with an identity from the kernel or the session.
func WithActor(ctx context.Context, a store.Actor) context.Context {
	return context.WithValue(ctx, actorKey{}, a)
}

// ActorFrom returns the actor WithActor stored, and whether there is one.
func ActorFrom(ctx context.Context) (store.Actor, bool) {
	a, ok := ctx.Value(actorKey{}).(store.Actor)
	return a, ok
}

func actorFrom(ctx context.Context) (store.Actor, bool) { return ActorFrom(ctx) }

// AssignRequestID gives every request a fresh ID (a UUIDv7), sent back as
// the X-Request-ID header and included in every error body and log line.
// A client-supplied X-Request-ID is ignored: the ID has to be unique for
// the log to mean anything.
func AssignRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := ids.NewV7()
		if err != nil {
			id = "unavailable"
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
	})
}

// Recover turns a panic in a handler into a 500 instead of a dropped
// connection, and logs it.
func (s *Service) Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				s.writeError(w, r, newError(CodeInternal, fmt.Errorf("panic: %v", v)))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// LimitConcurrency lets at most cap(slots) requests, across everything
// wrapped with the same slots, run at once. One more gets controller_busy
// at once.
func (s *Service) LimitConcurrency(slots *Slots, next http.Handler) http.Handler {
	return limitConcurrency(slots.c, s.Busy(), next)
}

// Busy answers controller_busy.
func (s *Service) Busy() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.writeError(w, r, newError(CodeControllerBusy, nil))
	})
}

// LimitRate applies RateLimit to POST requests, per actor. Reads aren't
// limited: they change nothing, and MaxConcurrent already bounds them. It
// must run after the actor is set.
func (s *Service) LimitRate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			next.ServeHTTP(w, r)
			return
		}
		actor, ok := actorFrom(r.Context())
		if !ok {
			s.writeError(w, r, newError(CodeInternal, fmt.Errorf("missing actor in request context")))
			return
		}
		if !s.limiter.allow(actor.Type+":"+actor.ID, s.now()) {
			s.writeError(w, r, newError(CodeRateLimited, nil))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// NotFound answers every path (and method) no route matches, so that even
// those errors have the usual JSON shape.
func (s *Service) NotFound(w http.ResponseWriter, r *http.Request) {
	s.writeError(w, r, newError(CodeNotFound, nil))
}

// WriteError sends e the way every service error goes out, logging its
// cause to the service's log.
func (s *Service) WriteError(w http.ResponseWriter, r *http.Request, e *Error) { s.writeError(w, r, e) }
