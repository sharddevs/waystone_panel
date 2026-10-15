// SPDX-License-Identifier: AGPL-3.0-only

package web

import (
	"net/http"
	"strings"
	"testing"
)

// C-W8: SafeNext only lets plain same-origin paths through.
func TestSafeNext(t *testing.T) {
	for _, p := range []string{"/", "/servers", "/servers/0199e1a0-0000-7000-8000-0000000000a1", "/a?b=c", "/a#frag", "/%E2%9C%93", "/a/../b", "/a%2Fb"} {
		if got := SafeNext(p); got != p {
			t.Errorf("SafeNext(%q) = %q, want it unchanged", p, got)
		}
	}
	for _, p := range []string{
		"", "servers", "//evil.test", "///evil.test", "/\\evil.test", "\\\\evil.test", "/a\\b",
		"https://evil.test", "http://evil.test/", "javascript:alert(1)", "data:text/html,x",
		"%2F%2Fevil.test", "/%2Fevil.test", "/%2F%2Fevil.test", "/%5Cevil.test", "/%5cevil.test",
		"/a b", "/a\tb", "/a\nb", "/a\rb", "/\x00", "/%00", "/%0d%0aSet-Cookie:x", "/%7f",
		"/" + strings.Repeat("a", 2048), "/%zz", " /servers", "/servers\x7f",
	} {
		if got := SafeNext(p); got != "/" {
			t.Errorf("SafeNext(%q) = %q, want \"/\"", p, got)
		}
	}
}

// Redirect builds Location from the canonical origin only, whatever the
// request's Host or X-Forwarded-Host said, and falls back to "/" for an
// unsafe path.
func TestRedirect(t *testing.T) {
	var path string
	var code int
	h, _ := newTestHandler(t, Route{Method: "GET", Pattern: "/go", CSRF: CSRFNotApplicable,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { Redirect(w, r, path, code) })})
	for _, c := range []struct {
		path     string
		code     int
		location string
		status   int
	}{
		{"/servers", http.StatusSeeOther, "https://panel.example.com/servers", 303},
		{"/servers", http.StatusFound, "https://panel.example.com/servers", 302},
		{"/servers", http.StatusTemporaryRedirect, "https://panel.example.com/servers", 307},
		{"/servers", http.StatusMovedPermanently, "https://panel.example.com/servers", 303},
		{"/servers", http.StatusOK, "https://panel.example.com/servers", 303},
		{"//evil.test/x", http.StatusSeeOther, "https://panel.example.com/", 303},
		{"https://evil.test", http.StatusSeeOther, "https://panel.example.com/", 303},
		{"/\\evil.test", http.StatusSeeOther, "https://panel.example.com/", 303},
	} {
		path, code = c.path, c.code
		r := caddyRequest(http.MethodGet, "/go")
		r.Host = "PANEL.EXAMPLE.COM:443"
		r.Header.Set("X-Forwarded-Host", "panel.example.com:443")
		w := serve(h, r)
		if w.Code != c.status || w.Header().Get("Location") != c.location {
			t.Errorf("Redirect(%q, %d): %d %q, want %d %q", c.path, c.code, w.Code, w.Header().Get("Location"), c.status, c.location)
		}
	}

	p, _ := New(Config{Public: mustOrigin(t, "https://panel.example.com:8443"), ProxyUID: testProxyUID, Log: h.log},
		Route{Method: "GET", Pattern: "/go", CSRF: CSRFNotApplicable,
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { Redirect(w, r, "/x", http.StatusSeeOther) })})
	r := caddyRequest(http.MethodGet, "/go")
	r.Host = "panel.example.com:8443"
	r.Header.Set("X-Forwarded-Host", "panel.example.com:8443")
	if got := serve(p, r).Header().Get("Location"); got != "https://panel.example.com:8443/x" {
		t.Errorf("non-default port: Location %q", got)
	}
}
