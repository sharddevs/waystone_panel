// SPDX-License-Identifier: AGPL-3.0-only

package api

// Tests that need no database: the actor, and the router's use of the
// service's limits. The shared parts are tested in internal/service.

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sharddevs/waystone_panel/internal/service"
	"github.com/sharddevs/waystone_panel/internal/store"
)

func TestActorFor(t *testing.T) {
	lookup := func(names map[string]string) LookupName {
		return func(uid string) (string, error) {
			if n, ok := names[uid]; ok {
				return n, nil
			}
			return "", errors.New("unknown uid")
		}
	}
	names := map[string]string{
		"1000": "trevor",
		"1001": "",
		"1002": "bad\nname",
		"1003": strings.Repeat("x", 101),
		"1004": "\xff\xfe",
		"1005": strings.Repeat("é", 50), // exactly 100 bytes
	}
	cases := []struct {
		uid      uint32
		wantID   string
		wantName string
	}{
		{1000, "1000", "trevor"},
		{0, "0", "uid 0"},
		{4294967295, "4294967295", "uid 4294967295"},
		{1001, "1001", "uid 1001"},
		{1002, "1002", "uid 1002"},
		{1003, "1003", "uid 1003"},
		{1004, "1004", "uid 1004"},
		{1005, "1005", strings.Repeat("é", 50)},
	}
	for _, tc := range cases {
		a := actorFor(Peer{UID: tc.uid, GID: 5, PID: 99}, lookup(names))
		if a.Type != store.ActorLocalUser || a.ID != tc.wantID || a.Name != tc.wantName {
			t.Errorf("uid %d: %+v, want local_user %s %q", tc.uid, a, tc.wantID, tc.wantName)
		}
		if err := store.ValidateActor(a); err != nil {
			t.Errorf("uid %d: actor fails store.ValidateActor: %v", tc.uid, err)
		}
	}
}

// A request that somehow arrives without peer credentials must not go
// on: 500, before any database work (this API has no store at all).
func TestNoPeerIsInternal(t *testing.T) {
	h := New(Config{Version: "dev", Log: discardLogger()})
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/servers"},
		{http.MethodPost, "/api/v1/servers/0199e1a0-0000-7000-8000-000000000a01/start"},
		{http.MethodGet, "/api/v1/operations"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if e := decodeError(t, rec); rec.Code != http.StatusInternalServerError || e.Code != CodeInternal {
			t.Errorf("%s %s without a peer: %d %s, want 500 internal", tc.method, tc.path, rec.Code, e.Code)
		}
	}
}

// withActor stops a request without a peer: the next handler never runs,
// so nothing can act as a made-up user (uid 0 is root).
func TestWithActorNeedsPeer(t *testing.T) {
	a := &API{log: discardLogger(), lookup: func(string) (string, error) { return "root", nil },
		svc: service.New(service.Config{Log: discardLogger()})}
	called := false
	h := service.AssignRequestID(a.withActor(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/servers", nil))
	if called {
		t.Error("the next handler ran without a peer")
	}
	if e := decodeError(t, rec); rec.Code != http.StatusInternalServerError || e.Code != CodeInternal {
		t.Errorf("got %d %s, want 500 internal", rec.Code, e.Code)
	}

	// With a peer, the actor reaches the handler.
	var got store.Actor
	h = a.withActor(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got, _ = service.ActorFrom(r.Context()) }))
	r := httptest.NewRequest(http.MethodGet, "/api/v1/servers", nil)
	h.ServeHTTP(httptest.NewRecorder(), r.WithContext(ContextWithPeer(r.Context(), Peer{UID: 1000})))
	if want := (store.Actor{Type: store.ActorLocalUser, ID: "1000", Name: "root"}); got != want {
		t.Errorf("actor = %+v, want %+v", got, want)
	}
}

// The rate limit applies to POSTs, per actor, before anything else runs.
// These requests fail validation before touching the database, so no
// store is needed.
func TestRateLimitThroughRouter(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	h := New(Config{Version: "dev", Log: discardLogger(), Now: func() time.Time { return now },
		Lookup: func(string) (string, error) { return "someone", nil }})
	send := func(method, path string, uid uint32) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		req = req.WithContext(ContextWithPeer(req.Context(), Peer{UID: uid}))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	for i := 0; i < RateLimit; i++ {
		if rec := send(http.MethodPost, "/api/v1/servers/not-a-uuid/start", 1000); rec.Code != http.StatusBadRequest {
			t.Fatalf("POST %d: %d, want 400 (bad ID, but not yet limited)", i+1, rec.Code)
		}
	}
	rec := send(http.MethodPost, "/api/v1/servers/not-a-uuid/stop", 1000)
	if e := decodeError(t, rec); rec.Code != http.StatusTooManyRequests || e.Code != CodeRateLimited {
		t.Errorf("POST %d: %d %s, want 429 rate_limited", RateLimit+1, rec.Code, e.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("429 without Retry-After")
	}
	if rec := send(http.MethodPost, "/api/v1/servers/not-a-uuid/start", 1001); rec.Code != http.StatusBadRequest {
		t.Errorf("another actor: %d, want 400 (not limited)", rec.Code)
	}
	for i := 0; i < RateLimit+5; i++ {
		if rec := send(http.MethodGet, "/api/v1/operations/not-a-uuid", 1000); rec.Code != http.StatusBadRequest {
			t.Fatalf("GET %d: %d, want 400 (reads aren't rate limited)", i+1, rec.Code)
		}
	}
	now = now.Add(RateWindow)
	if rec := send(http.MethodPost, "/api/v1/servers/not-a-uuid/start", 1000); rec.Code != http.StatusBadRequest {
		t.Errorf("after the window: %d, want 400 (allowed again)", rec.Code)
	}
}

// api.sock has MaxConcurrent slots of its own: with every one held (here
// by requests stuck in the uid lookup), even /healthz gets
// controller_busy at once. Slots taken elsewhere on the same service
// (web.sock's) don't count (5b audit H1).
func TestConcurrencyThroughRouter(t *testing.T) {
	svc := service.New(service.Config{Log: discardLogger()})
	release := make(chan struct{})
	entered := make(chan struct{}, service.MaxConcurrent)
	h := New(Config{Version: "dev", Log: discardLogger(), Service: svc, Lookup: func(string) (string, error) {
		entered <- struct{}{}
		<-release
		return "", errors.New("no such user")
	}})

	elsewhere := svc.LimitConcurrency(service.NewSlots(service.MaxConcurrent), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		entered <- struct{}{}
		<-release
	}))
	done := make(chan struct{})
	for i := 0; i < service.MaxConcurrent; i++ {
		go func() {
			elsewhere.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
			done <- struct{}{}
		}()
		<-entered
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("with another pool full: %d, want 200", rec.Code)
	}

	for i := 0; i < service.MaxConcurrent; i++ {
		go func() {
			r := httptest.NewRequest(http.MethodGet, "/api/v1/servers", nil)
			h.ServeHTTP(httptest.NewRecorder(), r.WithContext(ContextWithPeer(r.Context(), Peer{UID: 1000})))
			done <- struct{}{}
		}()
		<-entered
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if e := decodeError(t, rec); rec.Code != http.StatusServiceUnavailable || e.Code != CodeControllerBusy {
		t.Errorf("with every api.sock slot taken: %d %s, want 503 controller_busy", rec.Code, e.Code)
	}
	close(release)
	for i := 0; i < 2*service.MaxConcurrent; i++ {
		<-done
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("after release: %d, want 200", rec.Code)
	}
}
