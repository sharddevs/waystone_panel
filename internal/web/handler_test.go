// SPDX-License-Identifier: AGPL-3.0-only

package web

import (
	"io"
	"log"
	"net/http"
	"strings"
	"testing"
)

var okHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") })

// R3: New refuses anything that isn't a well-formed, declared route, and
// a CSRF policy that doesn't fit the route (C-W5, Q5).
func TestNewRefusesBadRoutes(t *testing.T) {
	o := mustOrigin(t, "https://panel.example.com")
	quiet := log.New(io.Discard, "", 0)
	cases := []struct {
		name  string
		route Route
		want  string
	}{
		{"no CSRF policy", Route{Method: "GET", Pattern: "/x", Handler: okHandler}, "CSRF policy"},
		{"POST not applicable", Route{Method: "POST", Pattern: "/x", Handler: okHandler, CSRF: CSRFNotApplicable}, "only a GET"},
		{"DELETE not applicable", Route{Method: "DELETE", Pattern: "/x", Handler: okHandler, CSRF: CSRFNotApplicable}, "only a GET"},
		{"POST required without sessions", Route{Method: "POST", Pattern: "/x", Handler: okHandler, CSRF: CSRFRequired}, "Config.Sessions"},
		{"GET session without sessions", Route{Method: "GET", Pattern: "/x", Handler: okHandler, CSRF: CSRFNotApplicable, Session: true}, "Config.Sessions"},
		{"GET required", Route{Method: "GET", Pattern: "/x", Handler: okHandler, CSRF: CSRFRequired}, "must be CSRFNotApplicable"},
		{"pre-session elsewhere", Route{Method: "POST", Pattern: "/x", Handler: okHandler, CSRF: CSRFPreSession}, "may be CSRFPreSession"},
		{"pre-session on GET", Route{Method: "GET", Pattern: LoginPattern, Handler: okHandler, CSRF: CSRFPreSession}, "may be CSRFPreSession"},
		{"pre-session on DELETE", Route{Method: "DELETE", Pattern: LoginPattern, Handler: okHandler, CSRF: CSRFPreSession}, "may be CSRFPreSession"},
		{"pre-session with a session", Route{Method: "POST", Pattern: LoginPattern, Handler: okHandler, CSRF: CSRFPreSession, Session: true}, "before there is a session"},
		{"unknown policy", Route{Method: "GET", Pattern: "/x", Handler: okHandler, CSRF: CSRFPolicy(9)}, "CSRF policy"},
		{"HEAD", Route{Method: "HEAD", Pattern: "/x", Handler: okHandler, CSRF: CSRFNotApplicable}, "method"},
		{"OPTIONS", Route{Method: "OPTIONS", Pattern: "/x", Handler: okHandler, CSRF: CSRFNotApplicable}, "method"},
		{"lowercase method", Route{Method: "get", Pattern: "/x", Handler: okHandler, CSRF: CSRFNotApplicable}, "method"},
		{"no method", Route{Pattern: "/x", Handler: okHandler, CSRF: CSRFNotApplicable}, "method"},
		{"host pattern", Route{Method: "GET", Pattern: "evil.test/x", Handler: okHandler, CSRF: CSRFNotApplicable}, "pattern"},
		{"method in pattern", Route{Method: "GET", Pattern: "GET /x", Handler: okHandler, CSRF: CSRFNotApplicable}, "pattern"},
		{"empty pattern", Route{Method: "GET", Handler: okHandler, CSRF: CSRFNotApplicable}, "pattern"},
		{"no handler", Route{Method: "GET", Pattern: "/x", CSRF: CSRFNotApplicable}, "handler"},
		{"shadows healthz", Route{Method: "GET", Pattern: "/healthz", Handler: okHandler, CSRF: CSRFNotApplicable}, "twice"},
		{"bad wildcard", Route{Method: "GET", Pattern: "/x/{", Handler: okHandler, CSRF: CSRFNotApplicable}, "route"},
	}
	for _, c := range cases {
		_, err := New(Config{Public: o, ProxyUID: testProxyUID, Log: quiet}, c.route)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want one mentioning %q", c.name, err, c.want)
		}
	}
	// Two routes that conflict in the mux.
	a := Route{Method: "GET", Pattern: "/a/{x}", Handler: okHandler, CSRF: CSRFNotApplicable}
	b := Route{Method: "GET", Pattern: "/a/{y}", Handler: okHandler, CSRF: CSRFNotApplicable}
	if _, err := New(Config{Public: o, ProxyUID: testProxyUID, Log: quiet}, a, b); err == nil {
		t.Error("conflicting patterns accepted")
	}
	if _, err := New(Config{Public: o, ProxyUID: testProxyUID, Log: quiet}, a, a); err == nil {
		t.Error("the same route twice accepted")
	}
}

// New refuses a missing origin, root as the proxy and HSTS out of range.
func TestNewConfig(t *testing.T) {
	o := mustOrigin(t, "https://panel.example.com")
	for _, c := range []Config{
		{ProxyUID: testProxyUID},
		{Public: o},
		{Public: o, ProxyUID: 0},
		{Public: o, ProxyUID: testProxyUID, HSTSMaxAge: -1},
		{Public: o, ProxyUID: testProxyUID, HSTSMaxAge: MaxHSTSMaxAge + 1},
	} {
		if _, err := New(c); err == nil {
			t.Errorf("New(%+v) succeeded", c)
		}
	}
	h, err := New(Config{Public: o, ProxyUID: testProxyUID, HSTSMaxAge: MaxHSTSMaxAge, Log: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	if got := serve(h, caddyRequest("GET", "/healthz")).Header().Get("Strict-Transport-Security"); got != "max-age=63072000" {
		t.Errorf("HSTS %q", got)
	}
	if h.Origin() != o {
		t.Error("Origin() differs")
	}
}

// C-W1: every registered route, the built-in one included, is behind the
// whole guard. The list comes from Routes(), which comes from the same
// slice the mux was built from, so a new route is covered automatically.
// RouteCoverage is exported for the controller's own test of its real
// route set.
func TestRouteCoverage(t *testing.T) {
	h, _ := newTestHandler(t,
		Route{Method: "GET", Pattern: "/servers", Handler: okHandler, CSRF: CSRFNotApplicable},
		Route{Method: "GET", Pattern: "/servers/{id}", Handler: okHandler, CSRF: CSRFNotApplicable},
	)
	routes := h.Routes()
	if len(routes) != 3 || routes[0].Pattern != "/healthz" || routes[2].Pattern != "/servers/{id}" {
		t.Fatalf("Routes() = %+v", routes)
	}
	routes[0].Pattern = "/changed"
	if h.Routes()[0].Pattern != "/healthz" {
		t.Error("Routes() exposes the handler's own slice")
	}
	if err := RouteCoverage(h); err != nil {
		t.Fatal(err)
	}
}

// RouteCoverage itself must notice a route that the guard doesn't cover.
// A handler built around the guard can't exist, so this hands it a fake.
func TestRouteCoverageCatchesGap(t *testing.T) {
	h, _ := newTestHandler(t, Route{Method: "GET", Pattern: "/open", Handler: okHandler, CSRF: CSRFNotApplicable})
	unguarded := &Handler{origin: h.origin, routes: h.routes, mux: h.mux, log: h.log, hsts: h.hsts}
	if err := routeCoverage(unguarded, unguarded.mux); err == nil {
		t.Error("a route served without the guard passed the coverage check")
	}
}

func TestCSRFPolicyString(t *testing.T) {
	if CSRFNotApplicable.String() != "not_applicable" || CSRFRequired.String() != "required" || CSRFPreSession.String() != "pre_session" || csrfUnset.String() != "unset" {
		t.Error("CSRFPolicy names")
	}
}
