// SPDX-License-Identifier: AGPL-3.0-only

package service

// Tests that need no database: request decoding, the fingerprint, the
// limits, the error mapping and authorization.

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sharddevs/waystone_panel/internal/store"
)

// The fingerprint is exactly C1's formula, computed here independently.
func TestFingerprintFormula(t *testing.T) {
	const id = "0199e1a0-0000-7000-8000-000000000a01"
	got, err := Fingerprint("POST", "/api/v1/servers/{id}/start", id, ActionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256([]byte("POST\n/api/v1/servers/{id}/start\n" + id + "\n{}"))
	if got != want {
		t.Errorf("Fingerprint = %x, want %x", got, want)
	}

	got, err = Fingerprint("POST", "/api/v1/operations/{id}/resolve", id, ResolveRequest{Outcome: "failed", Note: "x"})
	if err != nil {
		t.Fatal(err)
	}
	want = sha256.Sum256([]byte("POST\n/api/v1/operations/{id}/resolve\n" + id + "\n" + `{"outcome":"failed","note":"x"}`))
	if got != want {
		t.Errorf("resolve fingerprint = %x, want %x", got, want)
	}
}

// Key order and whitespace don't matter; method, route, server and
// content do.
func TestFingerprintCanonical(t *testing.T) {
	const id = "0199e1a0-0000-7000-8000-000000000a01"
	decode := func(body string) ResolveRequest {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		var v ResolveRequest
		if e := decodeJSON(httptest.NewRecorder(), req, &v); e != nil {
			t.Fatalf("decode %q: %v", body, e)
		}
		return v
	}
	fp := func(method, route, server string, body any) [32]byte {
		t.Helper()
		f, err := Fingerprint(method, route, server, body)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	const route = "/api/v1/operations/{id}/resolve"
	a := fp("POST", route, id, decode(`{"note":"x","outcome":"failed"}`))
	b := fp("POST", route, id, decode("{ \"outcome\" : \"failed\",\n \"note\": \"x\" }"))
	if a != b {
		t.Error("same body with another key order and spacing gave a different fingerprint")
	}
	for name, other := range map[string][32]byte{
		"method": fp("PUT", route, id, decode(`{"outcome":"failed","note":"x"}`)),
		"route":  fp("POST", "/api/v1/servers/{id}/start", id, decode(`{"outcome":"failed","note":"x"}`)),
		"server": fp("POST", route, "0199e1a0-0000-7000-8000-000000000a02", decode(`{"outcome":"failed","note":"x"}`)),
		"body":   fp("POST", route, id, decode(`{"outcome":"failed","note":"y"}`)),
	} {
		if other == a {
			t.Errorf("a different %s gave the same fingerprint", name)
		}
	}
}

func TestDecodeJSON(t *testing.T) {
	big := `{"note":"` + strings.Repeat("a", MaxBodyBytes) + `"}`
	exact := `{"note":"` + strings.Repeat("a", MaxBodyBytes-len(`{"note":""}`)) + `"}`
	cases := []struct {
		name, contentType, body string
		want                    string // "" means accepted
	}{
		{"no body", "", "", ""},
		{"no body, json type", "application/json", "", ""},
		{"empty object", "application/json", "{}", ""},
		{"charset is fine", "application/json; charset=utf-8", `{"outcome":"failed"}`, ""},
		{"exactly the limit", "application/json", exact, ""},
		{"no content type", "", "{}", CodeUnsupportedMediaType},
		{"text/plain", "text/plain", "{}", CodeUnsupportedMediaType},
		{"broken type", "application/", "{}", CodeUnsupportedMediaType},
		{"unknown field", "application/json", `{"outcome":"failed","force":true}`, CodeBadRequest},
		{"trailing data", "application/json", `{} {}`, CodeBadRequest},
		{"trailing junk", "application/json", `{}x`, CodeBadRequest},
		{"null", "application/json", `null`, CodeBadRequest},
		{"array", "application/json", `[]`, CodeBadRequest},
		{"string", "application/json", `"x"`, CodeBadRequest},
		{"wrong type", "application/json", `{"outcome":1}`, CodeBadRequest},
		{"broken", "application/json", `{"outcome":`, CodeBadRequest},
		{"too large", "application/json", big, CodeBodyTooLarge},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body))
		if tc.contentType != "" {
			req.Header.Set("Content-Type", tc.contentType)
		}
		var v ResolveRequest
		e := decodeJSON(httptest.NewRecorder(), req, &v)
		switch {
		case tc.want == "" && e != nil:
			t.Errorf("%s: refused with %s, want accepted", tc.name, e.code)
		case tc.want != "" && e == nil:
			t.Errorf("%s: accepted, want %s", tc.name, tc.want)
		case tc.want != "" && e.code != tc.want:
			t.Errorf("%s: %s, want %s", tc.name, e.code, tc.want)
		}
	}
}

func TestRateLimiter(t *testing.T) {
	l := newRateLimiter(3, time.Minute)
	t0 := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		if !l.allow("a", t0.Add(time.Duration(i)*time.Second)) {
			t.Fatalf("request %d refused, want allowed", i+1)
		}
	}
	if l.allow("a", t0.Add(10*time.Second)) {
		t.Error("4th request in the window allowed, want refused")
	}
	if !l.allow("b", t0.Add(10*time.Second)) {
		t.Error("another key was refused; limits must be per key")
	}
	// Refused requests don't count, and the window starts at the first
	// request: at t0+1m a new window begins with a full allowance.
	for i := 0; i < 3; i++ {
		if !l.allow("a", t0.Add(time.Minute+time.Duration(i)*time.Second)) {
			t.Fatalf("new window, request %d refused", i+1)
		}
	}
	if l.allow("a", t0.Add(time.Minute+5*time.Second)) {
		t.Error("4th request in the second window allowed")
	}
	// Ended windows are dropped, so the map doesn't grow forever.
	l.allow("c", t0.Add(10*time.Minute))
	l.mu.Lock()
	n := len(l.windows)
	l.mu.Unlock()
	if n != 1 {
		t.Errorf("%d windows kept after the others ended, want 1", n)
	}
}

func TestRateLimiterConcurrent(t *testing.T) {
	l := newRateLimiter(50, time.Minute)
	now := time.Now()
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if l.allow("a", now) {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if allowed != 50 {
		t.Errorf("%d of 200 concurrent requests allowed, want exactly 50", allowed)
	}
}

func TestLimitConcurrency(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 10)
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-release
		w.WriteHeader(http.StatusOK)
	})
	busy := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	h := limitConcurrency(make(chan struct{}, 2), busy, slow)

	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
			codes <- rec.Code
		}()
	}
	<-entered
	<-entered

	// Both slots are taken: the third request is turned away at once.
	done := make(chan int)
	go func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		done <- rec.Code
	}()
	select {
	case code := <-done:
		if code != http.StatusServiceUnavailable {
			t.Errorf("third request: %d, want 503", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("third request waited for a slot instead of failing fast")
	}

	close(release)
	wg.Wait()
	for i := 0; i < 2; i++ {
		if c := <-codes; c != http.StatusOK {
			t.Errorf("held request: %d, want 200", c)
		}
	}
	// The slots are free again.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("after release: %d, want 200", rec.Code)
	}
}

func TestErrorFor(t *testing.T) {
	const opID = "0199e1a0-0000-7000-8000-00000000c001"
	wrap := func(err error) error { return fmt.Errorf("in tx: %w", err) }
	cases := []struct {
		name string
		err  error
		code string
		opID string
	}{
		{"in progress (pending)", &store.InProgressError{OperationID: opID, State: store.StatePending}, CodeOperationInProgress, opID},
		{"in progress (running), wrapped", wrap(&store.InProgressError{OperationID: opID, State: store.StateRunning}), CodeOperationInProgress, opID},
		{"uncertain", wrap(&store.InProgressError{OperationID: opID, State: store.StateUncertain}), CodePreviousOperationUncertain, opID},
		{"bare in progress", store.ErrOperationInProgress, CodeOperationInProgress, ""},
		{"no membership", store.ErrNoMembership, CodeForbidden, ""},
		{"ambiguous membership", wrap(store.ErrAmbiguousMembership), CodeForbidden, ""},
		{"not found", wrap(store.ErrNotFound), CodeNotFound, ""},
		{"invalid input", fmt.Errorf("%w: limit", store.ErrInvalidInput), CodeBadRequest, ""},
		{"key reused", wrap(store.ErrIdempotencyKeyReused), CodeIdempotencyKeyReused, ""},
		{"not uncertain", store.ErrNotUncertain, CodeNotUncertain, ""},
		{"already resolved", wrap(store.ErrAlreadyResolved), CodeAlreadyResolved, ""},
		{"node unreachable", wrap(store.ErrNodeUnreachable), CodeNodeUnreachable, ""},
		{"node revoked", wrap(store.ErrNodeRevoked), CodeNodeRevoked, ""},
		{"not an admin", wrap(store.ErrNotAdmin), CodeForbidden, ""},
		{"port taken", wrap(store.ErrPortTaken), CodePortTaken, ""},
		{"server running", wrap(store.ErrServerRunning), CodeServerRunning, ""},
		{"not installed", wrap(store.ErrNotInstalled), CodeNotInstalled, ""},
		{"eula not accepted", wrap(store.ErrEulaNotAccepted), CodeEulaNotAccepted, ""},
		{"already accepted", wrap(store.ErrAlreadyAccepted), CodeAlreadyAccepted, ""},
		{"node full", wrap(store.ErrTooManyServers), CodeNodeFull, ""},
		{"invalid actor is our bug", fmt.Errorf("%w: name", store.ErrInvalidActor), CodeInternal, ""},
		{"database down", errors.New("connection refused"), CodeInternal, ""},
		{"context", context.DeadlineExceeded, CodeInternal, ""},
	}
	for _, tc := range cases {
		e := errorFor(tc.err)
		if e == nil {
			t.Errorf("%s: nil", tc.name)
			continue
		}
		if e.code != tc.code || e.operationID != tc.opID {
			t.Errorf("%s: got (%s, %q), want (%s, %q)", tc.name, e.code, e.operationID, tc.code, tc.opID)
		}
		if !errors.Is(e, tc.err) && e.cause != tc.err {
			t.Errorf("%s: the original error isn't kept as the cause", tc.name)
		}
	}
	// An *Error passes through as it is, even wrapped.
	orig := newError(CodeRateLimited, nil)
	if got := errorFor(fmt.Errorf("x: %w", orig)); got != orig {
		t.Errorf("*Error: got %+v, want the same value back", got)
	}
}

// Every code has a status and message, and writeError sends exactly them.
func TestWriteError(t *testing.T) {
	svc := &Service{log: discardLogger()}
	for code, info := range errorInfo {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		svc.writeError(rec, req, &Error{code: code, cause: errors.New("secret detail: pg_hba.conf")})
		if rec.Code != info.status {
			t.Errorf("%s: status %d, want %d", code, rec.Code, info.status)
		}
		if strings.Contains(rec.Body.String(), "secret") {
			t.Errorf("%s: the cause leaked into the response: %s", code, rec.Body.String())
		}
		if code == CodeRateLimited && rec.Header().Get("Retry-After") == "" {
			t.Error("rate_limited without Retry-After")
		}
	}
	rec := httptest.NewRecorder()
	svc.writeError(rec, httptest.NewRequest(http.MethodGet, "/", nil), &Error{code: "made_up"})
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), `"internal"`) {
		t.Errorf("unknown code: %d %s, want 500 internal", rec.Code, rec.Body.String())
	}
}

func TestAuthorize(t *testing.T) {
	actions := []string{
		string(store.ActionStart), string(store.ActionStop), string(store.ActionRestart),
		actionServerList, actionOperationList, actionOperationRead, actionOperationResolve,
	}
	for _, act := range actions {
		if !authorize(store.RoleAdmin, act) {
			t.Errorf("admin refused %s", act)
		}
		for _, role := range []string{store.RoleNode, "", "Admin", "owner"} {
			if authorize(role, act) {
				t.Errorf("role %q allowed %s", role, act)
			}
		}
	}
}

// Routes is exactly the stage 4 route table plus the Nodes API (6b) and
// create, install and the EULA (6c), so both sockets mount the same
// sixteen routes.
func TestRoutes(t *testing.T) {
	var got []string
	for _, rt := range New(Config{Log: discardLogger()}).Routes() {
		if rt.Handler == nil {
			t.Errorf("%s %s has no handler", rt.Method, rt.Pattern)
		}
		got = append(got, rt.Method+" "+rt.Pattern)
	}
	want := []string{
		"GET /api/v1/servers",
		"POST /api/v1/servers",
		"GET /api/v1/servers/{id}",
		"POST /api/v1/servers/{id}/start",
		"POST /api/v1/servers/{id}/stop",
		"POST /api/v1/servers/{id}/restart",
		"POST /api/v1/servers/{id}/install",
		"POST /api/v1/servers/{id}/accept-eula",
		"GET /api/v1/operations",
		"GET /api/v1/operations/{id}",
		"POST /api/v1/operations/{id}/resolve",
		"GET /api/v1/nodes",
		"GET /api/v1/nodes/{id}",
		"POST /api/v1/nodes/{id}/revoke",
		"GET /api/v1/nodes/enrollments",
		"POST /api/v1/nodes/enrollments",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("routes:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// LimitRate needs an actor; a handler without one never runs. Recover
// turns a panic into internal.
func TestMiddleware(t *testing.T) {
	s := New(Config{Log: discardLogger()})
	called := false
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })
	w := httptest.NewRecorder()
	AssignRequestID(s.LimitRate(next)).ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/", nil))
	if called || w.Code != http.StatusInternalServerError {
		t.Errorf("POST without an actor: called=%v status %d", called, w.Code)
	}
	w = httptest.NewRecorder()
	AssignRequestID(s.LimitRate(next)).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if !called {
		t.Error("a GET was rate limited")
	}
	w = httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	AssignRequestID(s.Recover(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))).ServeHTTP(w, r)
	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), `"internal"`) {
		t.Errorf("panic: %d %s", w.Code, w.Body.String())
	}
}

// Handlers wrapped with the same Slots share them; another Slots (the
// other socket's) is untouched (5b audit H1).
func TestSlotsPerPool(t *testing.T) {
	s := New(Config{Log: discardLogger()})
	pool, other := NewSlots(MaxConcurrent), NewSlots(MaxConcurrent)
	release := make(chan struct{})
	entered := make(chan struct{}, MaxConcurrent)
	slow := s.LimitConcurrency(pool, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		entered <- struct{}{}
		<-release
	}))
	samePool := s.LimitConcurrency(pool, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	otherPool := s.LimitConcurrency(other, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	var wg sync.WaitGroup
	for i := 0; i < MaxConcurrent; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			slow.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
		}()
		<-entered
	}
	w := httptest.NewRecorder()
	AssignRequestID(samePool).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), `"controller_busy"`) {
		t.Errorf("same pool with every slot taken: %d %s, want 503 controller_busy", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	AssignRequestID(otherPool).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusOK {
		t.Errorf("other pool: %d %s, want 200", w.Code, w.Body.String())
	}
	close(release)
	wg.Wait()
}
