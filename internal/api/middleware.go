// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"net/http"
	"time"

	"github.com/sharddevs/waystone_panel/internal/service"
)

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
			service.RequestIDFrom(r.Context()), r.Method, route, sw.status, time.Since(start).Round(time.Microsecond))
	})
}

// withActor finds out who is calling and stores the actor in the
// request's context for the handlers (service.WithActor). The peer comes
// from ConnContext. No peer, or a failed SO_PEERCRED, is an internal
// error: on a Unix socket it can't happen, so it means something is wired
// up wrong, and the request must not go on without an identity.
func (a *API) withActor(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer, err := peerFrom(r.Context())
		if err != nil {
			a.svc.WriteError(w, r, service.NewError(service.CodeInternal, err))
			return
		}
		ctx := service.WithActor(r.Context(), actorFor(peer, a.lookup))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
