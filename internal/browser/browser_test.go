// SPDX-License-Identifier: AGPL-3.0-only

package browser

import (
	"context"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sharddevs/waystone_panel/internal/api"
	"github.com/sharddevs/waystone_panel/internal/service"
	"github.com/sharddevs/waystone_panel/internal/web"
)

func quiet() *service.Service {
	return service.New(service.Config{Log: log.New(io.Discard, "", 0)})
}

// Every service route is here, behind a session, and only the GETs skip
// the CSRF token (scope §5).
func TestRoutesPolicy(t *testing.T) {
	svc := quiet()
	got := map[string]web.Route{}
	for _, rt := range Routes(svc) {
		got[rt.Method+" "+rt.Pattern] = rt
	}
	want := svc.Routes()
	if len(got) != len(want) {
		t.Fatalf("%d browser routes, want %d", len(got), len(want))
	}
	for _, sr := range want {
		rt, ok := got[sr.Method+" "+sr.Pattern]
		if !ok {
			t.Errorf("%s %s missing", sr.Method, sr.Pattern)
			continue
		}
		if !rt.Session {
			t.Errorf("%s %s doesn't need a session", sr.Method, sr.Pattern)
		}
		wantCSRF := web.CSRFRequired
		if sr.Method == http.MethodGet {
			wantCSRF = web.CSRFNotApplicable
		}
		if rt.CSRF != wantCSRF {
			t.Errorf("%s %s: CSRF %v, want %v", sr.Method, sr.Pattern, rt.CSRF, wantCSRF)
		}
	}
}

// Without a session in the context the handler never runs, and the
// answer is internal with a request ID, not a request as nobody.
func TestNoSessionIsInternal(t *testing.T) {
	for _, rt := range Routes(quiet()) {
		w := httptest.NewRecorder()
		rt.Handler.ServeHTTP(w, httptest.NewRequest(rt.Method, strings.Replace(rt.Pattern, "{id}", "0199e1a0-0000-7000-8000-000000000a01", 1), nil))
		if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), `"code":"internal"`) {
			t.Errorf("%s %s without a session: %d %s, want 500 internal", rt.Method, rt.Pattern, w.Code, w.Body.String())
		}
		if w.Header().Get("X-Request-ID") == "" {
			t.Errorf("%s %s: no X-Request-ID", rt.Method, rt.Pattern)
		}
	}
}

// C-W7 for the browser path: neither this package nor the service it
// mounts imports the local API, whose identity is the peer uid.
func TestNoLocalAPIImport(t *testing.T) {
	for _, dir := range []string{".", filepath.Join("..", "service")} {
		matches, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil || len(matches) == 0 {
			t.Fatalf("%s: %v %v", dir, matches, err)
		}
		for _, m := range matches {
			if strings.HasSuffix(m, "_test.go") {
				continue
			}
			f, err := parser.ParseFile(token.NewFileSet(), m, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, imp := range f.Imports {
				if strings.Contains(imp.Path.Value, "/internal/api\"") {
					t.Errorf("%s imports %s", m, imp.Path.Value)
				}
			}
		}
	}
}

// sessions resolves any token to a session whose account is the token's
// first letter: tokens that start with the same letter are different
// sessions of one account.
type sessions struct{}

func (sessions) ResolveSession(_ context.Context, token string) (web.Session, error) {
	return web.Session{ID: token, UserID: "user-" + token[:1], TenantID: "t", DisplayName: token[:1]}, nil
}

func tok(user byte, n int) string {
	t := fmt.Sprintf("%c%042d", user, n)
	return t[:web.TokenLength]
}

// holdEnv is web.sock with the browser routes plus one that blocks until
// released, mounted as Routes mounts them, next to api.sock on the same
// service.
type holdEnv struct {
	web, api http.Handler
	entered  chan struct{}
	release  chan struct{}
	done     chan int
}

func newHoldEnv(t *testing.T) *holdEnv {
	t.Helper()
	svc := quiet()
	e := &holdEnv{entered: make(chan struct{}, 64), release: make(chan struct{}), done: make(chan int, 64)}
	hold := service.Route{Method: http.MethodGet, Pattern: "/api/v1/hold", Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.entered <- struct{}{}
		<-e.release
	})}
	o, err := web.ParsePublicURL("https://panel.example.com")
	if err != nil {
		t.Fatal(err)
	}
	e.web, err = web.New(web.Config{Public: o, ProxyUID: 4242, Log: log.New(io.Discard, "", 0), Sessions: sessions{}},
		mount(svc, append(svc.Routes(), hold))...)
	if err != nil {
		t.Fatal(err)
	}
	e.api = api.New(api.Config{Version: "test", Service: svc, Log: log.New(io.Discard, "", 0),
		Lookup: func(string) (string, error) { return "admin", nil }})
	return e
}

func (e *holdEnv) get(path, tok string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Host = "panel.example.com"
	r.Header.Set("X-Forwarded-For", "203.0.113.7")
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-Host", "panel.example.com")
	r.AddCookie(&http.Cookie{Name: web.SessionCookieName, Value: tok})
	w := httptest.NewRecorder()
	e.web.ServeHTTP(w, r)
	return w
}

// holdN starts n blocking requests with tokens from tok and waits until
// they are all in the handler.
func (e *holdEnv) holdN(n int, tok func(i int) string) {
	for i := 0; i < n; i++ {
		go func() { e.done <- e.get("/api/v1/hold", tok(i)).Code }()
		<-e.entered
	}
}

func (e *holdEnv) finish(t *testing.T, n int) {
	t.Helper()
	close(e.release)
	for i := 0; i < n; i++ {
		if code := <-e.done; code != http.StatusOK {
			t.Errorf("held request: %d, want 200", code)
		}
	}
}

func busy(w *httptest.ResponseRecorder) bool {
	return w.Code == http.StatusServiceUnavailable && strings.Contains(w.Body.String(), `"controller_busy"`)
}

// web.sock has its own MaxConcurrent: with every one of its slots held
// by browser requests, the next browser request is controller_busy, but
// api.sock and its /healthz still answer (5b audit H1).
func TestFullBrowserPoolLeavesAPISock(t *testing.T) {
	e := newHoldEnv(t)
	users := service.MaxConcurrent / MaxPerUser
	e.holdN(service.MaxConcurrent, func(i int) string { return tok(byte('a'+i%users), i) })

	if w := e.get("/api/v1/servers/not-a-uuid", tok('z', 1)); !busy(w) {
		t.Errorf("service route with web.sock full: %d %s, want 503 controller_busy", w.Code, w.Body.String())
	}
	w := httptest.NewRecorder()
	e.api.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Code != http.StatusOK {
		t.Errorf("api.sock /healthz with web.sock full: %d %s, want 200", w.Code, w.Body.String())
	}
	r := httptest.NewRequest(http.MethodGet, "/api/v1/servers/not-a-uuid", nil)
	w = httptest.NewRecorder()
	e.api.ServeHTTP(w, r.WithContext(api.ContextWithPeer(r.Context(), api.Peer{UID: 1000})))
	if w.Code != http.StatusBadRequest {
		t.Errorf("api.sock route with web.sock full: %d %s, want 400 (it ran)", w.Code, w.Body.String())
	}

	e.finish(t, service.MaxConcurrent)
	if w := e.get("/api/v1/servers/not-a-uuid", tok('z', 2)); w.Code != http.StatusBadRequest {
		t.Errorf("after release: %d %s, want 400", w.Code, w.Body.String())
	}
}

// One account may have MaxPerUser requests running, across all its
// sessions; another account isn't held up by it, and the count goes back
// down when they finish.
func TestPerUserLimit(t *testing.T) {
	e := newHoldEnv(t)
	e.holdN(MaxPerUser, func(i int) string { return tok('a', i) }) // MaxPerUser sessions of one account

	if w := e.get("/api/v1/servers/not-a-uuid", tok('a', 99)); !busy(w) {
		t.Errorf("account over MaxPerUser: %d %s, want 503 controller_busy", w.Code, w.Body.String())
	}
	if w := e.get("/api/v1/servers/not-a-uuid", tok('b', 0)); w.Code != http.StatusBadRequest {
		t.Errorf("other account: %d %s, want 400 (it ran)", w.Code, w.Body.String())
	}
	e.finish(t, MaxPerUser)
	for i := 0; i < MaxPerUser+1; i++ { // sequential: each releases its place
		if w := e.get("/api/v1/servers/not-a-uuid", tok('a', i)); w.Code != http.StatusBadRequest {
			t.Fatalf("after release, request %d: %d %s, want 400", i+1, w.Code, w.Body.String())
		}
	}
}

func TestPerUserCount(t *testing.T) {
	p := &perUser{n: map[string]int{}}
	for i := 0; i < MaxPerUser; i++ {
		if !p.acquire("u") {
			t.Fatalf("acquire %d refused", i+1)
		}
	}
	if p.acquire("u") || !p.acquire("v") {
		t.Fatal("u should be full and v free")
	}
	p.release("v")
	for i := 0; i < MaxPerUser; i++ {
		p.release("u")
	}
	if len(p.n) != 0 {
		t.Errorf("after releasing everything: %v, want empty", p.n)
	}
}
