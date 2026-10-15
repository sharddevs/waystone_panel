// SPDX-License-Identifier: AGPL-3.0-only

package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The supported proxy's exact request is accepted, gets HSTS and the
// baseline headers, and nothing about the version.
func TestGuardAcceptsCaddyRequest(t *testing.T) {
	h, logs := newTestHandler(t)
	w := serve(h, caddyRequest(http.MethodGet, "/healthz"))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", w.Code, w.Body)
	}
	if strings.TrimSpace(w.Body.String()) != `{"status":"ok"}` {
		t.Errorf("body %q", w.Body)
	}
	if got := w.Header().Get("Strict-Transport-Security"); got != "max-age=300" {
		t.Errorf("HSTS %q, want max-age=300 (no includeSubDomains, no preload)", got)
	}
	checkBaseline(t, w.Header())
	if !strings.Contains(logs.String(), `client_ip="203.0.113.7"`) {
		t.Errorf("client IP not logged: %s", logs)
	}
	// HEAD goes through the GET route.
	if w := serve(h, caddyRequest(http.MethodHead, "/healthz")); w.Code != http.StatusOK {
		t.Errorf("HEAD: status %d", w.Code)
	}
}

// W8: every response carries the baseline, refusals included, and the CSP
// is exactly the reviewed constant.
func checkBaseline(t *testing.T, h http.Header) {
	t.Helper()
	want := map[string]string{
		"Content-Security-Policy":      "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'; object-src 'none'",
		"X-Content-Type-Options":       "nosniff",
		"Referrer-Policy":              "same-origin",
		"X-Frame-Options":              "DENY",
		"Cross-Origin-Opener-Policy":   "same-origin",
		"Cross-Origin-Resource-Policy": "same-origin",
		"Cache-Control":                "no-store",
	}
	for k, v := range want {
		if got := h.Values(k); len(got) != 1 || got[0] != v {
			t.Errorf("%s = %q, want exactly %q", k, got, v)
		}
	}
}

// The doc's §9 negatives and the R1 table: each forged, missing,
// duplicated or conflicting value is refused with the right status, gets
// the baseline headers but never HSTS, and never echoes the bad value.
func TestGuardRefusals(t *testing.T) {
	h, _ := newTestHandler(t)
	cases := []struct {
		name   string
		method string
		edit   func(r *http.Request)
		status int
	}{
		{"wrong host", "GET", func(r *http.Request) { r.Host = "evil.test" }, 421},
		{"host with trailing dot", "GET", func(r *http.Request) { r.Host = "panel.example.com." }, 421},
		{"host with other port", "GET", func(r *http.Request) { r.Host = "panel.example.com:8443" }, 421},
		{"empty host", "GET", func(r *http.Request) { r.Host = "" }, 421},
		{"host suffix trick", "GET", func(r *http.Request) { r.Host = "panel.example.com.evil.test" }, 421},
		{"absolute-form target", "GET", func(r *http.Request) { r.RequestURI = "http://evil.test/healthz"; r.URL.Host = "evil.test" }, 400},
		{"asterisk target", "OPTIONS", func(r *http.Request) { r.RequestURI = "*" }, 400},

		{"proto missing", "GET", func(r *http.Request) { r.Header.Del("X-Forwarded-Proto") }, 400},
		{"proto http", "GET", func(r *http.Request) { r.Header.Set("X-Forwarded-Proto", "http") }, 400},
		{"proto uppercase", "GET", func(r *http.Request) { r.Header.Set("X-Forwarded-Proto", "HTTPS") }, 400},
		{"proto list", "GET", func(r *http.Request) { r.Header.Set("X-Forwarded-Proto", "https,https") }, 400},
		{"proto twice", "GET", func(r *http.Request) { r.Header.Add("X-Forwarded-Proto", "https") }, 400},
		{"proto empty", "GET", func(r *http.Request) { r.Header.Set("X-Forwarded-Proto", "") }, 400},
		{"proto spoofed first", "GET", func(r *http.Request) { r.Header["X-Forwarded-Proto"] = []string{"http", "https"} }, 400},

		{"fwd host missing", "GET", func(r *http.Request) { r.Header.Del("X-Forwarded-Host") }, 400},
		{"fwd host evil", "GET", func(r *http.Request) { r.Header.Set("X-Forwarded-Host", "evil.test") }, 400},
		{"fwd host list", "GET", func(r *http.Request) { r.Header.Set("X-Forwarded-Host", "panel.example.com, evil.test") }, 400},
		{"fwd host twice", "GET", func(r *http.Request) { r.Header.Add("X-Forwarded-Host", "panel.example.com") }, 400},
		{"fwd host empty", "GET", func(r *http.Request) { r.Header.Set("X-Forwarded-Host", "") }, 400},

		{"xff missing", "GET", func(r *http.Request) { r.Header.Del("X-Forwarded-For") }, 400},
		{"xff chain", "GET", func(r *http.Request) { r.Header.Set("X-Forwarded-For", "6.6.6.6, 203.0.113.7") }, 400},
		{"xff twice", "GET", func(r *http.Request) { r.Header.Add("X-Forwarded-For", "6.6.6.6") }, 400},
		{"xff with port", "GET", func(r *http.Request) { r.Header.Set("X-Forwarded-For", "203.0.113.7:4444") }, 400},
		{"xff bracketed", "GET", func(r *http.Request) { r.Header.Set("X-Forwarded-For", "[2001:db8::1]") }, 400},
		{"xff zone", "GET", func(r *http.Request) { r.Header.Set("X-Forwarded-For", "fe80::1%eth0") }, 400},
		{"xff text", "GET", func(r *http.Request) { r.Header.Set("X-Forwarded-For", "unknown") }, 400},
		{"xff empty", "GET", func(r *http.Request) { r.Header.Set("X-Forwarded-For", "") }, 400},

		{"Forwarded", "GET", func(r *http.Request) { r.Header.Set("Forwarded", "for=6.6.6.6;host=evil.test;proto=https") }, 400},
		{"X-Real-IP", "GET", func(r *http.Request) { r.Header.Set("X-Real-IP", "6.6.6.6") }, 400},
		{"X-Forwarded-Port", "GET", func(r *http.Request) { r.Header.Set("X-Forwarded-Port", "443") }, 400},
		{"X-Forwarded-Server", "GET", func(r *http.Request) { r.Header.Set("X-Forwarded-Server", "evil.test") }, 400},
		{"X-Forwarded-Prefix", "GET", func(r *http.Request) { r.Header.Set("X-Forwarded-Prefix", "/evil") }, 400},
		{"X-Original-URL", "GET", func(r *http.Request) { r.Header.Set("X-Original-URL", "/admin") }, 400},
		{"X-Original-Host", "GET", func(r *http.Request) { r.Header.Set("X-Original-Host", "evil.test") }, 400},
		{"X-Rewrite-URL", "GET", func(r *http.Request) { r.Header.Set("X-Rewrite-URL", "/admin") }, 400},
		{"X-Client-IP", "GET", func(r *http.Request) { r.Header.Set("X-Client-IP", "6.6.6.6") }, 400},
		{"True-Client-IP", "GET", func(r *http.Request) { r.Header.Set("True-Client-IP", "6.6.6.6") }, 400},
		{"CF-Connecting-IP", "GET", func(r *http.Request) { r.Header.Set("CF-Connecting-IP", "6.6.6.6") }, 400},
		{"empty Forwarded", "GET", func(r *http.Request) { r.Header["Forwarded"] = []string{""} }, 400},

		{"POST foreign Origin", "POST", func(r *http.Request) { r.Header.Set("Origin", "https://evil.test") }, 403},
		{"POST null Origin", "POST", func(r *http.Request) { r.Header.Set("Origin", "null") }, 403},
		{"POST http Origin", "POST", func(r *http.Request) { r.Header.Set("Origin", "http://panel.example.com") }, 403},
		{"POST Origin with default port", "POST", func(r *http.Request) { r.Header.Set("Origin", "https://panel.example.com:443") }, 403},
		{"POST Origin twice", "POST", func(r *http.Request) { r.Header.Add("Origin", "https://evil.test") }, 403},
		{"POST no Origin, cross-site", "POST", func(r *http.Request) { r.Header.Del("Origin"); r.Header.Set("Sec-Fetch-Site", "cross-site") }, 403},
		{"POST no Origin, same-site", "POST", func(r *http.Request) { r.Header.Del("Origin"); r.Header.Set("Sec-Fetch-Site", "same-site") }, 403},
		{"POST no Origin, none", "POST", func(r *http.Request) { r.Header.Del("Origin"); r.Header.Set("Sec-Fetch-Site", "none") }, 403},
		{"POST no Origin at all", "POST", func(r *http.Request) { r.Header.Del("Origin") }, 403},
		{"POST Origin foreign but fetch same-origin", "POST", func(r *http.Request) {
			r.Header.Set("Origin", "https://evil.test")
			r.Header.Set("Sec-Fetch-Site", "same-origin")
		}, 403},
		{"DELETE foreign Origin", "DELETE", func(r *http.Request) { r.Header.Set("Origin", "https://evil.test") }, 403},
		{"PUT foreign Origin", "PUT", func(r *http.Request) { r.Header.Set("Origin", "https://evil.test") }, 403},
		{"PATCH foreign Origin", "PATCH", func(r *http.Request) { r.Header.Set("Origin", "https://evil.test") }, 403},
		{"unknown method, foreign Origin", "PROPFIND", func(r *http.Request) { r.Header.Set("Origin", "https://evil.test") }, 403},
	}
	for _, c := range cases {
		r := caddyRequest(c.method, "/healthz")
		c.edit(r)
		w := serve(h, r)
		if w.Code != c.status {
			t.Errorf("%s: status %d, want %d", c.name, w.Code, c.status)
		}
		if w.Header().Get("Strict-Transport-Security") != "" {
			t.Errorf("%s: HSTS sent on a refusal", c.name)
		}
		checkBaseline(t, w.Header())
		var body struct {
			Error struct{ Code, Message string } `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Error.Code == "" {
			t.Errorf("%s: body %q is not a JSON error", c.name, w.Body)
		}
		for _, leak := range []string{"evil", "6.6.6.6", "admin"} {
			if strings.Contains(w.Body.String(), leak) {
				t.Errorf("%s: body echoes %q: %s", c.name, leak, w.Body)
			}
		}
	}
}

// Unsafe methods get through the Origin check with the canonical Origin,
// or with no Origin but a browser's Sec-Fetch-Site: same-origin. No route
// takes POST before stage 5, so past the guard the answer is the mux's
// not_found, not a refusal.
func TestOriginAccepted(t *testing.T) {
	h, _ := newTestHandler(t)
	pastGuard := func(name string, r *http.Request) {
		w := serve(h, r)
		if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), `"not_found"`) {
			t.Errorf("%s: status %d body %s, want the mux's not_found", name, w.Code, w.Body)
		}
	}
	pastGuard("canonical Origin", caddyRequest(http.MethodPost, "/healthz"))
	r := caddyRequest(http.MethodPost, "/healthz")
	r.Header.Del("Origin")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	pastGuard("Sec-Fetch-Site same-origin", r)
	// Safe methods don't need an Origin; a foreign one doesn't matter.
	r = caddyRequest(http.MethodGet, "/healthz")
	r.Header.Set("Origin", "https://evil.test")
	if w := serve(h, r); w.Code != http.StatusOK {
		t.Errorf("GET with foreign Origin: status %d", w.Code)
	}
}

// Accepted spellings: Host in another case or with the default port, and
// an IPv6 client.
func TestGuardAcceptedVariants(t *testing.T) {
	h, logs := newTestHandler(t)
	for _, edit := range []func(r *http.Request){
		func(r *http.Request) { r.Host = "PANEL.example.com" },
		func(r *http.Request) {
			r.Host = "panel.example.com:443"
			r.Header.Set("X-Forwarded-Host", "panel.example.com:443")
		},
		func(r *http.Request) { r.Header.Set("X-Forwarded-For", "2001:db8::7") },
	} {
		r := caddyRequest(http.MethodGet, "/healthz")
		edit(r)
		if w := serve(h, r); w.Code != http.StatusOK {
			t.Errorf("status %d for %v", w.Code, r.Header)
		}
	}
	if !strings.Contains(logs.String(), `client_ip="2001:db8::7"`) {
		t.Errorf("IPv6 client not logged: %s", logs)
	}
}

// C-W7: the local API's routes don't exist on the browser side, and an
// unknown path is a JSON 404 with the baseline headers.
func TestNoAPIRoutes(t *testing.T) {
	h, _ := newTestHandler(t)
	for _, p := range []string{"/api/v1/servers", "/api/v1/operations", "/", "/healthz/"} {
		w := serve(h, caddyRequest(http.MethodGet, p))
		if w.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", p, w.Code)
		}
		checkBaseline(t, w.Header())
	}
}

// ClientIP and the helpers only work inside the chain.
func TestHelpersOutsideChain(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if _, ok := ClientIP(r); ok {
		t.Error("ClientIP without the guard")
	}
	if CheckWebSocketOrigin(r) {
		t.Error("CheckWebSocketOrigin without the guard")
	}
	w := httptest.NewRecorder()
	Redirect(w, r, "/x", http.StatusSeeOther)
	if w.Code != http.StatusBadRequest || w.Header().Get("Location") != "" {
		t.Errorf("Redirect without the guard: %d %q", w.Code, w.Header().Get("Location"))
	}
}
