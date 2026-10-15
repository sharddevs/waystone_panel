// SPDX-License-Identifier: AGPL-3.0-only

package api

// Tests that need no database: request decoding, the fingerprint, the
// limits, the error mapping, the actor and authorization.

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
	got, err := Fingerprint("POST", "/api/v1/servers/{id}/start", id, actionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256([]byte("POST\n/api/v1/servers/{id}/start\n" + id + "\n{}"))
	if got != want {
		t.Errorf("Fingerprint = %x, want %x", got, want)
	}

	got, err = Fingerprint("POST", "/api/v1/operations/{id}/resolve", id, resolveRequest{Outcome: "failed", Note: "x"})
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
	decode := func(body string) resolveRequest {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		var v resolveRequest
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
		var v resolveRequest
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
	h := limitConcurrency(2, busy, slow)

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
	// An *apiError passes through as it is, even wrapped.
	orig := newError(CodeRateLimited, nil)
	if got := errorFor(fmt.Errorf("x: %w", orig)); got != orig {
		t.Errorf("apiError: got %+v, want the same value back", got)
	}
}

// Every code has a status and message, and writeError sends exactly them.
func TestWriteError(t *testing.T) {
	api := &API{log: discardLogger()}
	for code, info := range errorInfo {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		api.writeError(rec, req, &apiError{code: code, cause: errors.New("secret detail: pg_hba.conf")})
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
	api.writeError(rec, httptest.NewRequest(http.MethodGet, "/", nil), &apiError{code: "made_up"})
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), `"internal"`) {
		t.Errorf("unknown code: %d %s, want 500 internal", rec.Code, rec.Body.String())
	}
}

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
	a := &API{log: discardLogger(), lookup: func(string) (string, error) { return "root", nil }}
	called := false
	h := withRequestID(a.withActor(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })))
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
	h = a.withActor(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got, _ = actorFrom(r.Context()) }))
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
