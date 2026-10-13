// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/sharddevs/waystone_panel/internal/ids"
	"github.com/sharddevs/waystone_panel/internal/store"
)

type requestIDKey struct{}
type actorKey struct{}

// requestIDFrom returns the request's ID, or "" outside withRequestID.
func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// actorFrom returns the actor withActor stored, and whether there is one.
func actorFrom(ctx context.Context) (store.Actor, bool) {
	a, ok := ctx.Value(actorKey{}).(store.Actor)
	return a, ok
}

// withRequestID gives every request a fresh ID (a UUIDv7), sent back as
// the X-Request-ID header and included in every error body and log line.
// A client-supplied X-Request-ID is ignored: the ID has to be unique for
// the log to mean anything.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := ids.NewV7()
		if err != nil {
			id = "unavailable"
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
	})
}

// statusWriter remembers the status a handler sent, for the log line.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

// logRequests writes one line per request: ID, method, route, status and
// how long it took. The route is the pattern that matched, not the raw
// path, so log lines group nicely and no odd path ends up in the log.
func (a *API) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		route := r.Pattern
		if route == "" {
			route = "-"
		}
		a.logf("request_id=%s method=%s route=%q status=%d duration=%s",
			requestIDFrom(r.Context()), r.Method, route, sw.status, time.Since(start).Round(time.Microsecond))
	})
}

// withRecover turns a panic in a handler into a 500 instead of a dropped
// connection, and logs it.
func (a *API) withRecover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				a.writeError(w, r, newError(CodeInternal, fmt.Errorf("panic: %v", v)))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// withActor finds out who is calling and stores the actor in the
// request's context for the handlers. The peer comes from ConnContext.
// No peer, or a failed SO_PEERCRED, is an internal error: on a Unix
// socket it can't happen, so it means something is wired up wrong, and
// the request must not go on without an identity.
//
// TODO: peerFrom, then actorFor with a.lookup, then call next with the
// actor in the context (actorKey{}).
func (a *API) withActor(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer, err := peerFrom(r.Context())
		if err != nil {
			a.writeError(w, r, newError(CodeInternal, err))
			return
		}

		actor := actorFor(peer, a.lookup)

		ctx := context.WithValue(r.Context(), actorKey{}, actor)

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// withRateLimit applies RateLimit to POST requests, per actor. Reads
// aren't limited: they change nothing, and MaxConcurrent already bounds
// them. It must run after withActor.
//
// TODO: only for POST; key on actor type and ID together (a.limiter,
// a.now()); over the limit → rate_limited.
func (a *API) withRateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			next.ServeHTTP(w, r)
			return
		}

		actor, ok := actorFrom(r.Context())
		if !ok {
			a.writeError(w, r, newError(
				CodeInternal,
				fmt.Errorf("missing actor in request context"),
			))
			return
		}

		key := actor.Type + ":" + actor.ID

		if !a.limiter.allow(key, a.now()) {
			a.writeError(w, r, newError(CodeRateLimited, nil))
			return
		}

		next.ServeHTTP(w, r)
	})
}
