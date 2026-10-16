// SPDX-License-Identifier: AGPL-3.0-only

// Package browser mounts the server and operation API (internal/service)
// on web.sock (stage 5b, E1). It is the browser's counterpart of
// internal/api's thin layer: the routes, their JSON and their checks are
// the service's, and the only thing this package adds is who the caller
// is, taken from the signed-in session that internal/web resolved. It
// never imports internal/api (C-W7): the peer uid is not an identity
// here, the proxy's is the only one on this socket.
package browser

import (
	"errors"
	"net/http"
	"sync"

	"github.com/sharddevs/waystone_panel/internal/service"
	"github.com/sharddevs/waystone_panel/internal/store"
	"github.com/sharddevs/waystone_panel/internal/web"
)

// MaxPerUser is how many requests one account may have running on
// web.sock at once, across all its sessions and tabs. One more gets
// controller_busy at once, so a single account (a stolen session, a UI
// bug, many tabs) can't take every slot (5b review).
const MaxPerUser = 8

// Routes are the service's routes as browser routes. Each one needs a
// session; GETs are CSRFNotApplicable, everything else CSRFRequired
// (scope §5). web.New checks the session and the CSRF token before the
// handler runs. Then, from the outside in: a request ID, Recover,
// MaxPerUser, LimitConcurrency with web.sock's own MaxConcurrent slots
// (not api.sock's, 5b audit H1), the actor from the session, LimitRate
// (shared with api.sock, per actor), and the service's handler.
func Routes(svc *service.Service) []web.Route { return mount(svc, svc.Routes()) }

// mount wraps rts as Routes describes, all of them sharing one pool of
// slots and one per-user count.
func mount(svc *service.Service, rts []service.Route) []web.Route {
	slots := service.NewSlots(service.MaxConcurrent)
	users := &perUser{n: map[string]int{}}
	var out []web.Route
	for _, rt := range rts {
		csrf := web.CSRFRequired
		if rt.Method == http.MethodGet {
			csrf = web.CSRFNotApplicable
		}
		var h http.Handler = rt.Handler
		h = svc.LimitRate(h)
		h = withActor(svc, h)
		h = svc.LimitConcurrency(slots, h)
		h = limitPerUser(svc, users, h)
		h = svc.Recover(h)
		h = service.AssignRequestID(h)
		out = append(out, web.Route{Method: rt.Method, Pattern: rt.Pattern, Handler: h, CSRF: csrf, Session: true})
	}
	return out
}

// withActor makes the session's account the actor: type "user", the
// user's ID, and the display name for history (C4). A request without a
// session can't get here, since web.New wraps every route in the session
// check; if it does anyway, it stops with internal, before any database
// work, the same as api.sock without a peer.
func withActor(svc *service.Service, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s, ok := web.SessionFrom(r)
		if !ok {
			svc.WriteError(w, r, service.NewError(service.CodeInternal, errors.New("browser route without a session")))
			return
		}
		actor := store.Actor{Type: store.ActorUser, ID: s.UserID, Name: s.DisplayName}
		next.ServeHTTP(w, r.WithContext(service.WithActor(r.Context(), actor)))
	})
}

// perUser counts each account's running requests.
type perUser struct {
	mu sync.Mutex
	n  map[string]int
}

func (p *perUser) acquire(user string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.n[user] >= MaxPerUser {
		return false
	}
	p.n[user]++
	return true
}

func (p *perUser) release(user string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.n[user]--; p.n[user] <= 0 {
		delete(p.n, user)
	}
}

// limitPerUser applies MaxPerUser, keyed by the session's account. A
// request without a session stops with internal, as in withActor.
func limitPerUser(svc *service.Service, users *perUser, next http.Handler) http.Handler {
	busy := svc.Busy()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s, ok := web.SessionFrom(r)
		if !ok {
			svc.WriteError(w, r, service.NewError(service.CodeInternal, errors.New("browser route without a session")))
			return
		}
		if !users.acquire(s.UserID) {
			busy.ServeHTTP(w, r)
			return
		}
		defer users.release(s.UserID)
		next.ServeHTTP(w, r)
	})
}
