// SPDX-License-Identifier: AGPL-3.0-only

package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
)

// RouteCoverage checks that every registered route is behind the whole
// guard (contract C-W1): for each one, a forged Host is 421, a request
// that didn't come over HTTPS is 400, a stripped header is 400, and the
// proxy's exact request reaches the route with HSTS and the baseline
// headers. Tests call it with the controller's real route set, so a route
// added in stage 5 is covered without anyone updating a list.
func RouteCoverage(h *Handler) error { return routeCoverage(h, h) }

func routeCoverage(h *Handler, serveVia http.Handler) error {
	for _, rt := range h.Routes() {
		path := samplePath(rt.Pattern)
		name := rt.Method + " " + rt.Pattern
		req := func() *http.Request {
			r := httptest.NewRequest(rt.Method, path, nil)
			r.Host = h.origin.Authority()
			r.Header.Set(headerXFF, "203.0.113.7")
			r.Header.Set(headerXFP, h.origin.scheme)
			r.Header.Set(headerXFH, h.origin.Authority())
			if unsafeMethod(rt.Method) {
				r.Header.Set(headerOrig, h.origin.String())
			}
			return r
		}
		do := func(r *http.Request) *httptest.ResponseRecorder {
			w := httptest.NewRecorder()
			serveVia.ServeHTTP(w, r)
			return w
		}
		r := req()
		r.Host = "coverage.invalid"
		if w := do(r); w.Code != http.StatusMisdirectedRequest {
			return fmt.Errorf("%s: a forged Host got %d, want 421", name, w.Code)
		}
		r = req()
		r.Header.Set(headerXFP, "http")
		if h.origin.scheme == "http" {
			r.Header.Set(headerXFP, "https")
		}
		if w := do(r); w.Code != http.StatusBadRequest {
			return fmt.Errorf("%s: the wrong X-Forwarded-Proto got %d, want 400", name, w.Code)
		}
		r = req()
		r.Header.Set("Forwarded", "for=192.0.2.1")
		if w := do(r); w.Code != http.StatusBadRequest {
			return fmt.Errorf("%s: a Forwarded header got %d, want 400", name, w.Code)
		}
		if unsafeMethod(rt.Method) {
			r = req()
			r.Header.Set(headerOrig, "https://coverage.invalid")
			if w := do(r); w.Code != http.StatusForbidden {
				return fmt.Errorf("%s: a foreign Origin got %d, want 403", name, w.Code)
			}
		}
		// The proxy's own request must pass the guard. HSTS is only set
		// past it, so on https its presence proves that; on http (devweb)
		// only the guard answers 421 or a bad_request 400.
		w := do(req())
		if w.Header().Get("Content-Security-Policy") != ContentSecurityPolicy {
			return fmt.Errorf("%s: no CSP on the response", name)
		}
		if h.origin.scheme == "https" {
			if w.Header().Get("Strict-Transport-Security") != hstsValue(h.hsts) {
				return fmt.Errorf("%s: the proxy's own request got %d without HSTS: refused, or not behind the guard", name, w.Code)
			}
		} else if w.Code == http.StatusMisdirectedRequest || w.Code == http.StatusBadRequest && strings.Contains(w.Body.String(), `"`+CodeBadRequest+`"`) {
			return fmt.Errorf("%s: the proxy's own request was refused (%d)", name, w.Code)
		}
	}
	return nil
}

// samplePath turns a pattern into a path it matches: each {wildcard}
// becomes "x", and a trailing {name...} or {$} disappears.
func samplePath(pattern string) string {
	var b strings.Builder
	for i := 0; i < len(pattern); i++ {
		if pattern[i] != '{' {
			b.WriteByte(pattern[i])
			continue
		}
		end := strings.IndexByte(pattern[i:], '}')
		if end < 0 {
			b.WriteString(pattern[i:])
			break
		}
		w := pattern[i+1 : i+end]
		if w != "$" && !strings.HasSuffix(w, "...") {
			b.WriteString("x")
		}
		i += end
	}
	return b.String()
}
