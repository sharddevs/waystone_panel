// SPDX-License-Identifier: AGPL-3.0-only

package web

import (
	"io"
	"log"
	"net/http"
	"strings"
	"testing"
)

// single is the "exactly one value" rule for every forwarding header: one
// header line, not empty, not a list.
func TestSingle(t *testing.T) {
	for _, c := range []struct {
		vals []string
		ok   bool
	}{
		{[]string{"https"}, true},
		{nil, false},
		{[]string{""}, false},
		{[]string{"https,https"}, false},
		{[]string{"a, b"}, false},
		{[]string{"https", "https"}, false},
	} {
		h := http.Header{}
		if c.vals != nil {
			h["X-Test"] = c.vals
		}
		if v, ok := single(h, "X-Test"); ok != c.ok || (ok && v != c.vals[0]) {
			t.Errorf("single(%q) = %q, %v; want ok=%v", c.vals, v, ok, c.ok)
		}
	}
}

// OPTIONS is a safe method: it needs no Origin, and past the guard the mux
// answers it (the route takes GET only). It must not be refused as
// cross-site.
func TestOptionsIsSafe(t *testing.T) {
	h, _ := newTestHandler(t)
	for _, origin := range []string{"", "https://evil.test"} {
		r := caddyRequest("OPTIONS", "/healthz")
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		w := serve(h, r)
		if w.Code == http.StatusForbidden || w.Header().Get("Strict-Transport-Security") == "" {
			t.Errorf("OPTIONS with Origin %q: %d, HSTS %q; want past the guard", origin, w.Code, w.Header().Get("Strict-Transport-Security"))
		}
	}
}

// A port that isn't plain digits never matches, even though it doesn't
// parse to a different number; and nothing may follow an IPv6 literal but
// a port.
func TestMatchAuthorityMalformed(t *testing.T) {
	o := mustOrigin(t, "https://panel.example.com")
	for _, v := range []string{"panel.example.com:44a", "panel.example.com:abc", "panel.example.com:+443", "panel.example.com:4 43"} {
		if o.matchAuthority(v) {
			t.Errorf("%q matched", v)
		}
	}
	v6 := mustOrigin(t, "https://[2001:db8::1]")
	for _, v := range []string{"[2001:db8::1]x", "[2001:db8::1]x443", "[2001:db8::1]:", "[2001:db8::1"} {
		if v6.matchAuthority(v) {
			t.Errorf("%q matched", v)
		}
	}
}

// Each early check in ParsePublicURL refuses with its own explanation, so
// an administrator learns what is wrong with the value.
func TestParsePublicURLMessages(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"", "required"},
		{"https://" + strings.Repeat("a", 2050) + ".example.com", "longer than 2048"},
		{"https://panel.example.com/\x7f", "printable ASCII"},
		{"https://pänel.example.com", "printable ASCII"},
		{"https://panel example.com", "printable ASCII"},
	} {
		_, err := ParsePublicURL(c.in)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: err = %v, want one mentioning %q", c.in, err, c.want)
		}
	}
}

// A pattern with whitespace is refused by New itself, whatever the mux
// would make of it.
func TestNewRefusesWhitespacePattern(t *testing.T) {
	o := mustOrigin(t, "https://panel.example.com")
	for _, p := range []string{"/a b", "/a\tb", "/a\nb"} {
		_, err := New(Config{Public: o, ProxyUID: testProxyUID, Log: log.New(io.Discard, "", 0)},
			Route{Method: "GET", Pattern: p, Handler: okHandler, CSRF: CSRFNotApplicable})
		if err == nil || !strings.Contains(err.Error(), "pattern") {
			t.Errorf("pattern %q: err = %v", p, err)
		}
	}
}

// Every refusal is logged with its code and reason, and no request line
// is logged as accepted.
func TestRefusalIsLogged(t *testing.T) {
	h, logs := newTestHandler(t)
	r := caddyRequest("GET", "/healthz")
	r.Header.Set("X-Forwarded-Proto", "http")
	serve(h, r)
	r = caddyRequest("GET", "/healthz")
	r.Host = "evil.test"
	serve(h, r)
	out := logs.String()
	for _, want := range []string{`refused=bad_request reason="X-Forwarded-Proto is not exactly https"`, `refused=misdirected_request reason="host is not the public origin"`} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "status=") {
		t.Errorf("a refused request was logged as served:\n%s", out)
	}
}

// RouteCoverage must fail for each kind of hole: a Host that isn't
// checked, a response without HSTS, and an unsafe route without the
// Origin check. Each fake wraps the real handler and opens one hole.
func TestRouteCoverageCatchesEachHole(t *testing.T) {
	h, _ := newTestHandler(t)
	post := &Handler{origin: h.origin, proxy: h.proxy, hsts: h.hsts, log: h.log, mux: h.mux,
		routes: []RouteInfo{{Method: "POST", Pattern: "/x", CSRF: CSRFRequired}}}
	cases := []struct {
		name string
		h    *Handler
		via  http.Handler
	}{
		{"Host not checked", h, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Host = "panel.example.com"
			h.ServeHTTP(w, r)
		})},
		{"no HSTS", h, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h.ServeHTTP(dropHSTS{w}, r)
		})},
		{"Origin not checked", post, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Origin") != "" {
				r.Header.Set("Origin", "https://panel.example.com")
			}
			h.ServeHTTP(w, r)
		})},
	}
	for _, c := range cases {
		if err := routeCoverage(c.h, c.via); err == nil {
			t.Errorf("%s: the coverage check passed", c.name)
		}
	}
	// The real handler still passes with the same POST route list, so
	// the Origin case fails for the hole and not for something else.
	if err := routeCoverage(post, h); err != nil {
		t.Errorf("the real handler failed: %v", err)
	}
}

// dropHSTS removes HSTS from a response just before it is written.
type dropHSTS struct{ http.ResponseWriter }

func (d dropHSTS) WriteHeader(code int) {
	d.Header().Del("Strict-Transport-Security")
	d.ResponseWriter.WriteHeader(code)
}

func (d dropHSTS) Write(b []byte) (int, error) {
	d.Header().Del("Strict-Transport-Security")
	return d.ResponseWriter.Write(b)
}
