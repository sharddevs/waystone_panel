// SPDX-License-Identifier: AGPL-3.0-only

package web

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
)

// CSRFPolicy is what a route declares about cross-site request forgery.
// Its zero value is invalid on purpose: every route must say (R3, C-W5).
type CSRFPolicy int

const (
	csrfUnset CSRFPolicy = iota
	// CSRFNotApplicable is only allowed for GET routes (which also answer
	// HEAD), and they must not change state.
	CSRFNotApplicable
	// CSRFRequired is for every route that can change state. It needs the
	// session-bound token that stage 5 adds. Until then, New refuses it,
	// so no state-changing browser route can exist without one.
	CSRFRequired
)

func (p CSRFPolicy) String() string {
	switch p {
	case CSRFNotApplicable:
		return "not_applicable"
	case CSRFRequired:
		return "required"
	}
	return "unset"
}

// errCSRFNotBuilt: stage 5 builds session-bound CSRF tokens into this
// package, and only then can a route ask for them.
var errCSRFNotBuilt = errors.New("CSRF tokens are not built yet (stage 5); a state-changing browser route can't be registered before then")

// Route is one browser route. Pattern is a path pattern as http.ServeMux
// understands it ("/servers/{id}"), without a method or host.
type Route struct {
	Method  string
	Pattern string
	Handler http.Handler
	CSRF    CSRFPolicy
}

// RouteInfo describes a registered route, for the route-coverage tests.
type RouteInfo struct {
	Method  string
	Pattern string
	CSRF    CSRFPolicy
}

// Config is what New needs. Public and ProxyUID are required.
type Config struct {
	Public     Origin
	ProxyUID   uint32
	HSTSMaxAge int         // 0 means DefaultHSTSMaxAge
	Log        *log.Logger // nil means log.Default()
}

// Handler is the browser-facing handler. Its fields are unexported, so
// New is the only way to build one, and New's routes the only way in.
// The controller serves web.sock only through Handler.NewServer, whose
// type admits nothing else (R3).
type Handler struct {
	origin  Origin
	proxy   uint32
	hsts    int
	log     *log.Logger
	routes  []RouteInfo
	mux     *http.ServeMux
	counter atomic.Uint64
}

// healthzRoute is built in: the proxy (and a person) can check that the
// path through Caddy works. It names no version, unlike the local
// /healthz.
var healthzRoute = Route{
	Method:  http.MethodGet,
	Pattern: "/healthz",
	CSRF:    CSRFNotApplicable,
	Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}),
}

// New builds the browser-facing handler with the built-in /healthz and
// the given routes. Every request goes through, in order: the guard
// (Host, forwarding headers, Origin for unsafe methods), the security
// headers, then the route. Anything unrouted is a JSON 404.
func New(cfg Config, routes ...Route) (*Handler, error) {
	if cfg.Public.IsZero() {
		return nil, errors.New("web: the public origin is required")
	}
	if cfg.ProxyUID == 0 {
		return nil, errors.New("web: the proxy uid must not be root (0)")
	}
	switch {
	case cfg.HSTSMaxAge == 0:
		cfg.HSTSMaxAge = DefaultHSTSMaxAge
	case cfg.HSTSMaxAge < 0 || cfg.HSTSMaxAge > MaxHSTSMaxAge:
		return nil, fmt.Errorf("web: HSTS max-age must be 1 to %d seconds", MaxHSTSMaxAge)
	}
	if cfg.Log == nil {
		cfg.Log = log.Default()
	}
	h := &Handler{
		origin: cfg.Public,
		proxy:  cfg.ProxyUID,
		hsts:   cfg.HSTSMaxAge,
		log:    cfg.Log,
		mux:    http.NewServeMux(),
	}
	seen := map[string]bool{}
	for _, rt := range append([]Route{healthzRoute}, routes...) {
		if err := checkRoute(rt); err != nil {
			return nil, fmt.Errorf("web: route %s %s: %w", rt.Method, rt.Pattern, err)
		}
		key := rt.Method + " " + rt.Pattern
		if seen[key] {
			return nil, fmt.Errorf("web: route %s registered twice", key)
		}
		seen[key] = true
		if err := register(h.mux, key, rt.Handler); err != nil {
			return nil, fmt.Errorf("web: route %s: %w", key, err)
		}
		h.routes = append(h.routes, RouteInfo{Method: rt.Method, Pattern: rt.Pattern, CSRF: rt.CSRF})
	}
	h.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { writeError(w, CodeNotFound) })
	return h, nil
}

// checkRoute enforces what a route may be: a known method, a path-only
// pattern (no host, no method, no spaces), a handler, and a CSRF policy
// that fits the method.
func checkRoute(rt Route) error {
	switch rt.Method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		return errors.New("method must be GET, POST, PUT, PATCH or DELETE")
	}
	if !strings.HasPrefix(rt.Pattern, "/") || strings.ContainsAny(rt.Pattern, " \t\r\n") {
		return errors.New("pattern must be a path starting with '/', without a host or method")
	}
	if rt.Handler == nil {
		return errors.New("no handler")
	}
	switch rt.CSRF {
	case CSRFNotApplicable:
		if rt.Method != http.MethodGet {
			return errors.New("only a GET route may be CSRFNotApplicable")
		}
	case CSRFRequired:
		return errCSRFNotBuilt
	default:
		return errors.New("the route must declare its CSRF policy")
	}
	return nil
}

// register adds a pattern, turning ServeMux's panic on a conflicting or
// malformed pattern into an error.
func register(mux *http.ServeMux, pattern string, handler http.Handler) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("%v", p)
		}
	}()
	mux.Handle(pattern, handler)
	return nil
}

// Routes lists the registered routes, the built-in /healthz first. It
// comes from the same list the mux was built from, so it can't drift.
func (h *Handler) Routes() []RouteInfo { return append([]RouteInfo(nil), h.routes...) }

// Origin is the canonical public origin this handler answers to.
func (h *Handler) Origin() Origin { return h.origin }

// ServeHTTP runs the guard, then the route. The baseline security headers
// go on every response, refusals included. HSTS only goes on requests the
// guard accepted, i.e. when the trusted proxy confirmed the scheme, and
// only for https.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id := h.counter.Add(1)
	sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	setBaselineHeaders(sw.Header())
	ip, refused := check(h.origin, r)
	if refused != nil {
		h.log.Printf("web request=%d refused=%s reason=%q method=%q path=%q", id, refused.code, refused.reason, r.Method, r.URL.Path)
		writeError(sw, refused.code)
		return
	}
	if h.origin.scheme == "https" {
		sw.Header().Set("Strict-Transport-Security", hstsValue(h.hsts))
	}
	ctx := context.WithValue(r.Context(), infoKey{}, requestInfo{origin: h.origin, clientIP: ip})
	h.mux.ServeHTTP(sw, r.WithContext(ctx))
	h.log.Printf("web request=%d method=%q path=%q status=%d client_ip=%q", id, r.Method, r.URL.Path, sw.status, ip.String())
}

// statusWriter remembers the status for the request log.
type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusWriter) WriteHeader(code int) {
	if !s.wroteHeader {
		s.status, s.wroteHeader = code, true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	s.wroteHeader = true
	return s.ResponseWriter.Write(b)
}
