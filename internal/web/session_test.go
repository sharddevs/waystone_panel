// SPDX-License-Identifier: AGPL-3.0-only

package web

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	goodToken = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" // 43 characters
	goodCSRF  = "csrf-csrf-csrf-csrf-csrf-csrf-csrf-csrf-csr"
)

// fakeSessions knows one token. A token of "BBBB…" makes it fail as if
// the database were down. It counts its calls.
type fakeSessions struct{ calls atomic.Int32 }

func (f *fakeSessions) ResolveSession(ctx context.Context, token string) (Session, error) {
	f.calls.Add(1)
	switch token {
	case goodToken:
		return Session{ID: "s1", UserID: "u1", DisplayName: "Trevor", CSRFToken: goodCSRF}, nil
	case strings.Repeat("B", TokenLength):
		return Session{}, errors.New("database down")
	}
	return Session{}, ErrNoSession
}

// sessionHandler has one route of each kind. Each handler echoes the
// session's user, so a test can see that the handler ran and with what.
func sessionHandler(t *testing.T, f *fakeSessions) *Handler {
	t.Helper()
	echo := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s, ok := SessionFrom(r)
		if !ok {
			io.WriteString(w, "no session")
			return
		}
		io.WriteString(w, "user="+s.UserID)
	})
	h, err := New(Config{
		Public:   mustOrigin(t, "https://panel.example.com"),
		ProxyUID: testProxyUID,
		Log:      log.New(io.Discard, "", 0),
		Sessions: f,
	},
		Route{Method: "POST", Pattern: LoginPattern, CSRF: CSRFPreSession, Handler: echo},
		Route{Method: "GET", Pattern: LoginPattern, CSRF: CSRFNotApplicable, Session: true, Handler: echo},
		Route{Method: "DELETE", Pattern: LoginPattern, CSRF: CSRFRequired, Handler: echo},
		Route{Method: "GET", Pattern: "/public", CSRF: CSRFNotApplicable, Handler: echo},
	)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func withCookie(r *http.Request, value string) *http.Request {
	r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: value})
	return r
}

// C-W5 and the session middleware: a session route needs a live session;
// a CSRFRequired one also needs exactly the session's token in the
// header. Each refusal comes before the handler runs.
func TestSessionRoutes(t *testing.T) {
	f := &fakeSessions{}
	h := sessionHandler(t, f)
	type tc struct {
		name   string
		req    *http.Request
		status int
		body   string // a substring of the response
	}
	del := func(token string, csrf ...string) *http.Request {
		r := withCookie(caddyRequest("DELETE", LoginPattern), token)
		for _, c := range csrf {
			r.Header.Add(CSRFHeader, c)
		}
		return r
	}
	cases := []tc{
		{"GET no cookie", caddyRequest("GET", LoginPattern), 401, `"unauthenticated"`},
		{"GET malformed cookie", withCookie(caddyRequest("GET", LoginPattern), "short"), 401, `"unauthenticated"`},
		{"GET unknown session", withCookie(caddyRequest("GET", LoginPattern), strings.Repeat("Z", TokenLength)), 401, `"unauthenticated"`},
		{"GET store down", withCookie(caddyRequest("GET", LoginPattern), strings.Repeat("B", TokenLength)), 503, `"controller_busy"`},
		{"GET live session, no CSRF needed", withCookie(caddyRequest("GET", LoginPattern), goodToken), 200, "user=u1"},
		{"DELETE no cookie", caddyRequest("DELETE", LoginPattern), 401, `"unauthenticated"`},
		{"DELETE no token", del(goodToken), 403, `"csrf_failed"`},
		{"DELETE wrong token", del(goodToken, strings.Repeat("x", TokenLength)), 403, `"csrf_failed"`},
		{"DELETE token too short", del(goodToken, goodCSRF[:42]), 403, `"csrf_failed"`},
		{"DELETE token twice", del(goodToken, goodCSRF, goodCSRF), 403, `"csrf_failed"`},
		{"DELETE right token", del(goodToken, goodCSRF), 200, "user=u1"},
		{"sign-in needs no session", caddyRequest("POST", LoginPattern), 200, "no session"},
		{"public route", caddyRequest("GET", "/public"), 200, "no session"},
	}
	for _, c := range cases {
		w := serve(h, c.req)
		if w.Code != c.status || !strings.Contains(w.Body.String(), c.body) {
			t.Errorf("%s: %d %q, want %d containing %q", c.name, w.Code, w.Body.String(), c.status, c.body)
		}
	}
	// A malformed cookie never reaches the store, and a public route
	// never asks it.
	before := f.calls.Load()
	serve(h, withCookie(caddyRequest("GET", LoginPattern), "not a token!"))
	serve(h, withCookie(caddyRequest("GET", "/public"), goodToken))
	if f.calls.Load() != before {
		t.Error("the store was asked for a malformed token or on a public route")
	}
}

// The guard still runs first: sign-in from a foreign Origin, or without
// Origin and Sec-Fetch-Site, is refused before the route (W9, Q5). The
// cookie's SameSite=Lax is the other half of the login-CSRF defence.
func TestSignInStillNeedsOrigin(t *testing.T) {
	h := sessionHandler(t, &fakeSessions{})
	r := caddyRequest("POST", LoginPattern)
	r.Header.Set("Origin", "https://evil.example")
	if w := serve(h, r); w.Code != 403 || !strings.Contains(w.Body.String(), `"forbidden"`) {
		t.Errorf("foreign Origin: %d %q", w.Code, w.Body.String())
	}
	r = caddyRequest("POST", LoginPattern)
	r.Header.Del("Origin")
	if w := serve(h, r); w.Code != 403 {
		t.Errorf("no Origin: %d", w.Code)
	}
}

// C-W1 holds for session routes too: every one is behind the whole guard.
func TestSessionRoutesCovered(t *testing.T) {
	if err := RouteCoverage(sessionHandler(t, &fakeSessions{})); err != nil {
		t.Fatal(err)
	}
}

// RouteInfo says which routes need a session; CSRFRequired implies it.
func TestRouteInfoSession(t *testing.T) {
	got := map[string]bool{}
	for _, ri := range sessionHandler(t, &fakeSessions{}).Routes() {
		got[ri.Method+" "+ri.Pattern] = ri.Session
	}
	want := map[string]bool{"GET /healthz": false, "POST " + LoginPattern: false, "GET " + LoginPattern: true, "DELETE " + LoginPattern: true, "GET /public": false}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: Session = %v, want %v", k, got[k], v)
		}
	}
}

// C-W2: the session cookie set through this package has every attribute,
// and a token that isn't one is refused before any header is written.
func TestSetSessionCookie(t *testing.T) {
	w := httptest.NewRecorder()
	if err := SetSessionCookie(w, goodToken, 7*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	got := w.Header().Get("Set-Cookie")
	for _, want := range []string{SessionCookieName + "=" + goodToken, "Path=/", "Max-Age=604800", "HttpOnly", "Secure", "SameSite=Lax"} {
		if !strings.Contains(got, want) {
			t.Errorf("Set-Cookie %q lacks %q", got, want)
		}
	}
	if strings.Contains(strings.ToLower(got), "domain") {
		t.Errorf("Set-Cookie %q has a Domain", got)
	}
	for _, bad := range []string{"", "short", strings.Repeat("A", 44), strings.Repeat("A", 42) + "="} {
		w := httptest.NewRecorder()
		if err := SetSessionCookie(w, bad, time.Hour); err == nil || w.Header().Get("Set-Cookie") != "" {
			t.Errorf("token %q accepted", bad)
		}
	}
	w = httptest.NewRecorder()
	ClearSession(w)
	if got := w.Header().Get("Set-Cookie"); !strings.Contains(got, SessionCookieName+"=;") || !strings.Contains(got, "Max-Age=0") {
		t.Errorf("ClearSession: %q", got)
	}
}

// SessionToken only returns a cookie value of a token's shape.
func TestSessionToken(t *testing.T) {
	if tok, ok := SessionToken(withCookie(caddyRequest("GET", "/"), goodToken)); !ok || tok != goodToken {
		t.Errorf("good token: %q %v", tok, ok)
	}
	for _, v := range []string{"", "x", strings.Repeat("A", 42) + "!"} {
		if _, ok := SessionToken(withCookie(caddyRequest("GET", "/"), v)); ok {
			t.Errorf("%q accepted", v)
		}
	}
	if _, ok := SessionToken(caddyRequest("GET", "/")); ok {
		t.Error("no cookie accepted")
	}
}

// The exported error writer uses the fixed codes, statuses and messages.
func TestWriteErrorCodes(t *testing.T) {
	for code, status := range map[string]int{
		CodeUnauthenticated: 401, CodeInvalidCredentials: 401, CodeCSRF: 403,
		CodeTooManyRequests: 429, CodeUnsupportedMedia: 415, CodeBusy: 503, "nonsense": 400,
	} {
		w := httptest.NewRecorder()
		WriteError(w, code)
		if w.Code != status {
			t.Errorf("%s: %d, want %d", code, w.Code, status)
		}
	}
}
