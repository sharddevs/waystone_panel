// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package browser

// The browser API against a real PostgreSQL, through the real web.sock
// handler (guard, session, CSRF) and next to the real api.sock handler on
// the same service, as the controller wires them. Like the other
// integration tests, these wipe the waystone_test database, so run them
// one package at a time (-p 1).

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sharddevs/waystone_panel/internal/api"
	"github.com/sharddevs/waystone_panel/internal/password"
	"github.com/sharddevs/waystone_panel/internal/service"
	"github.com/sharddevs/waystone_panel/internal/session"
	"github.com/sharddevs/waystone_panel/internal/store"
	"github.com/sharddevs/waystone_panel/internal/view"
	"github.com/sharddevs/waystone_panel/internal/web"
)

const (
	tenantB  = "0199e1a0-0000-7000-8000-00000000000b"
	nodeA    = "0199e1a0-0000-7000-8000-0000000000a1"
	nodeB    = "0199e1a0-0000-7000-8000-0000000000b1"
	serverA1 = "0199e1a0-0000-7000-8000-000000000a01"
	serverA2 = "0199e1a0-0000-7000-8000-000000000a02"
	serverB1 = "0199e1a0-0000-7000-8000-000000000b01"
	missing  = "0199e1a0-0000-7000-8000-00000000ffff"

	goodPassword = "correct horse battery"
)

// cheap are argon2id costs small enough for tests.
var cheap = password.Params{Memory: 64, Time: 1, Threads: 1}

// The local caller: uid 1000, admin of the same organization as the
// browser admin, so both see exactly the same servers and actions.
var (
	local      = store.Actor{Type: store.ActorLocalUser, ID: "1000", Name: "trevor"}
	localJSON  = `{"type":"local_user","id":"1000","name":"admin a"}`
	localNames = map[string]string{"1000": "admin a", "5000": "not an admin"}
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func ownerDSN() string {
	return envOr("WAYSTONE_TEST_OWNER_DSN", "host=/var/run/postgresql dbname=waystone_test user=waystone_owner")
}

func appDSN() string {
	return envOr("WAYSTONE_TEST_APP_DSN", "host=/var/run/postgresql dbname=waystone_test user=waystone_app")
}

type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type env struct {
	web, api http.Handler
	owner    *pgx.Conn
	tenantA  string
	trevor   store.User // browser admin of A
	sam      store.User // browser user of A with role "node"
	now      time.Time
	logs     syncBuffer
}

func setup(t *testing.T) *env {
	t.Helper()
	return setupWith(t, nil)
}

// setupWith is setup with the service's config adjusted by cfg first.
func setupWith(t *testing.T, cfg func(*service.Config)) *env {
	t.Helper()
	ctx := context.Background()
	if !strings.Contains(ownerDSN(), "waystone_test") {
		t.Fatalf("refusing to wipe a database that isn't waystone_test: %q", ownerDSN())
	}
	owner, err := pgx.Connect(ctx, ownerDSN())
	if err != nil {
		t.Fatalf("connect as owner: %v", err)
	}
	t.Cleanup(func() { owner.Close(context.Background()) })
	e := &env{owner: owner, now: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)}
	e.exec(t, `SET waystone.owner_override = 'on'`)
	for _, q := range []string{
		`DROP SCHEMA IF EXISTS waystone CASCADE`,
		`DROP SCHEMA IF EXISTS waystone_meta CASCADE`,
		`CREATE SCHEMA waystone`,
		`CREATE SCHEMA waystone_meta`,
	} {
		e.exec(t, q)
	}
	if err := store.Migrate(ctx, ownerDSN()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// The accounts first, while there is no organization yet: bootstrap
	// makes the one organization they belong to.
	acc, err := store.OpenAccounts(ctx, ownerDSN())
	if err != nil {
		t.Fatalf("OpenAccounts: %v", err)
	}
	t.Cleanup(func() { acc.Close(context.Background()) })
	hash, err := password.Hash(goodPassword, cheap)
	if err != nil {
		t.Fatal(err)
	}
	if e.trevor, err = acc.Bootstrap(ctx, local, store.NewUser{Username: "trevor", DisplayName: "Trevor", PasswordHash: hash}); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if e.sam, err = acc.Create(ctx, local, store.NewUser{Username: "sam", DisplayName: "Sam", PasswordHash: hash}, store.RoleAdmin); err != nil {
		t.Fatalf("Create: %v", err)
	}
	e.tenantA = e.trevor.TenantID
	e.exec(t, `UPDATE waystone.memberships SET role = 'node' WHERE actor_type = 'user' AND actor_id = $1`, e.sam.ID)

	for _, q := range []string{
		`INSERT INTO waystone.tenants (tenant_id, name) VALUES ('` + tenantB + `', 'B')`,
		`INSERT INTO waystone.memberships (tenant_id, actor_type, actor_id, role) VALUES
			('` + e.tenantA + `', 'local_user', '1000', 'admin'),
			('` + e.tenantA + `', 'local_user', '5000', 'node')`,
		`INSERT INTO waystone.nodes (tenant_id, node_id, name, last_seen_at) VALUES
			('` + e.tenantA + `', '` + nodeA + `', 'node a', '2100-01-01T00:00:00Z'),
			('` + tenantB + `', '` + nodeB + `', 'node b', '2100-01-01T00:00:00Z')`,
		`INSERT INTO waystone.servers (tenant_id, server_id, node_id, name, profile, runtime) VALUES
			('` + e.tenantA + `', '` + serverA1 + `', '` + nodeA + `', 'a1', 'minecraft-java', 'systemd'),
			('` + e.tenantA + `', '` + serverA2 + `', '` + nodeA + `', 'a2', 'minecraft-java', 'systemd'),
			('` + tenantB + `', '` + serverB1 + `', '` + nodeB + `', 'b1', 'minecraft-java', 'systemd')`,
	} {
		e.exec(t, q)
	}

	st, err := store.Open(ctx, appDSN())
	if err != nil {
		t.Fatalf("Open as app: %v", err)
	}
	t.Cleanup(st.Close)
	clock := func() time.Time { return e.now }
	st.Now = clock
	logger := log.New(&e.logs, "", 0)

	// As the controller wires it: one service, both sockets.
	svcCfg := service.Config{Store: st, Log: logger, Now: clock}
	if cfg != nil {
		cfg(&svcCfg)
	}
	svc := service.New(svcCfg)
	e.api = api.New(api.Config{Version: "test", Service: svc, Log: logger, Lookup: func(uid string) (string, error) {
		if n, ok := localNames[uid]; ok {
			return n, nil
		}
		return "", errors.New("no such user")
	}})
	sessions, err := session.New(session.Config{Store: st, Params: cheap, Now: clock, Log: logger})
	if err != nil {
		t.Fatal(err)
	}
	o, err := web.ParsePublicURL("https://panel.example.com")
	if err != nil {
		t.Fatal(err)
	}
	e.web, err = web.New(web.Config{Public: o, ProxyUID: 4242, Log: logger, Sessions: sessions},
		append(sessions.Routes(), Routes(svc)...)...)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := e.owner.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func (e *env) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := e.owner.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// setState moves an operation as the owner, with the override on.
func (e *env) setState(t *testing.T, opID string, states ...string) {
	t.Helper()
	for _, s := range states {
		e.exec(t, `UPDATE waystone.operations SET state = $1 WHERE operation_id = $2`, s, opID)
	}
}

// req is one request. On web.sock it goes as Caddy forwards it, with the
// cookie and CSRF token of b (if set); on api.sock as uid.
type req struct {
	method, path, body string
	contentType        string // default application/json when there's a body
	key                string // Idempotency-Key
}

type browserUser struct {
	cookie, csrf string
}

func newRequest(q req) *http.Request {
	r := httptest.NewRequest(q.method, q.path, strings.NewReader(q.body))
	if q.body != "" {
		ct := q.contentType
		if ct == "" {
			ct = "application/json"
		}
		r.Header.Set("Content-Type", ct)
	}
	if q.key != "" {
		r.Header.Set("Idempotency-Key", q.key)
	}
	return r
}

func (e *env) onWeb(b browserUser, q req) *httptest.ResponseRecorder {
	r := newRequest(q)
	r.Host = "panel.example.com"
	r.Header.Set("X-Forwarded-For", "203.0.113.7")
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-Host", "panel.example.com")
	if q.method != http.MethodGet {
		r.Header.Set("Origin", "https://panel.example.com")
	}
	if b.cookie != "" {
		r.AddCookie(&http.Cookie{Name: web.SessionCookieName, Value: b.cookie})
	}
	if b.csrf != "" {
		r.Header.Set(web.CSRFHeader, b.csrf)
	}
	w := httptest.NewRecorder()
	e.web.ServeHTTP(w, r)
	return w
}

func (e *env) onAPI(uid uint32, q req) *httptest.ResponseRecorder {
	r := newRequest(q)
	r = r.WithContext(api.ContextWithPeer(r.Context(), api.Peer{UID: uid}))
	w := httptest.NewRecorder()
	e.api.ServeHTTP(w, r)
	return w
}

// signIn signs in on web.sock and returns the cookie and CSRF token.
func (e *env) signIn(t *testing.T, username string) browserUser {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"username": username, "password": goodPassword})
	w := e.onWeb(browserUser{}, req{method: "POST", path: "/api/v1/session", body: string(body)})
	if w.Code != http.StatusNoContent {
		t.Fatalf("sign in %s: %d %s", username, w.Code, w.Body.String())
	}
	var b browserUser
	for _, c := range w.Result().Cookies() {
		if c.Name == web.SessionCookieName {
			b.cookie = c.Value
		}
	}
	w = e.onWeb(b, req{method: "GET", path: "/api/v1/session"})
	var s struct {
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil || s.CSRFToken == "" {
		t.Fatalf("session for %s: %d %s", username, w.Code, w.Body.String())
	}
	b.csrf = s.CSRFToken
	return b
}

func actorJSON(u store.User) string {
	b, _ := json.Marshal(view.Actor{Type: store.ActorUser, ID: u.ID, Name: u.DisplayName})
	return string(b)
}

// normalize blanks out what may differ between the sockets: the
// request's ID (a fresh UUID each time) and the actor, plus the given
// literal strings (operation IDs created separately on each).
func normalize(w *httptest.ResponseRecorder, actor string, more ...string) string {
	s := w.Body.String()
	s = strings.ReplaceAll(s, actor, "ACTOR")
	if id := w.Header().Get("X-Request-ID"); id != "" {
		s = strings.ReplaceAll(s, id, "REQUEST_ID")
	}
	for i := 0; i+1 < len(more); i += 2 {
		s = strings.ReplaceAll(s, more[i], more[i+1])
	}
	return s
}

// same checks that the two responses agree: status, Content-Type,
// Location and the body byte for byte, apart from the actor and request
// ID (E1, gate S7).
func same(t *testing.T, name string, aw, ww *httptest.ResponseRecorder, wwActor string, more ...string) {
	t.Helper()
	if aw.Code != ww.Code {
		t.Errorf("%s: status api %d, web %d\napi: %s\nweb: %s", name, aw.Code, ww.Code, aw.Body.String(), ww.Body.String())
		return
	}
	for _, h := range []string{"Content-Type", "Location", "Retry-After"} {
		a, w := aw.Header().Get(h), ww.Header().Get(h)
		for i := 0; i+1 < len(more); i += 2 {
			a, w = strings.ReplaceAll(a, more[i], more[i+1]), strings.ReplaceAll(w, more[i], more[i+1])
		}
		if a != w {
			t.Errorf("%s: %s api %q, web %q", name, h, a, w)
		}
	}
	if ww.Header().Get("X-Request-ID") == "" {
		t.Errorf("%s: web.sock sent no X-Request-ID", name)
	}
	if a, w := normalize(aw, localJSON, more...), normalize(ww, wwActor, more...); a != w {
		t.Errorf("%s: bodies differ\napi: %s\nweb: %s", name, a, w)
	}
}

func opID(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var op view.Operation
	if err := json.Unmarshal(w.Body.Bytes(), &op); err != nil || op.ID == "" {
		t.Fatalf("not an operation: %d %s", w.Code, w.Body.String())
	}
	return op.ID
}

// E1, gate S7: every route, success and refusal, answers the same on
// both sockets.
func TestSameJSONOnBothSockets(t *testing.T) {
	e := setup(t)
	trevor := e.signIn(t, "trevor")
	tj := actorJSON(e.trevor)
	both := func(name string, q req, more ...string) (aw, ww *httptest.ResponseRecorder) {
		t.Helper()
		aw, ww = e.onAPI(1000, q), e.onWeb(trevor, q)
		same(t, name, aw, ww, tj, more...)
		return aw, ww
	}

	// Reads and refusals that change nothing.
	for _, c := range []struct {
		name string
		q    req
	}{
		{"list servers", req{method: "GET", path: "/api/v1/servers"}},
		{"get server", req{method: "GET", path: "/api/v1/servers/" + serverA1}},
		{"other tenant's server", req{method: "GET", path: "/api/v1/servers/" + serverB1}},
		{"missing server", req{method: "GET", path: "/api/v1/servers/" + missing}},
		{"bad server ID", req{method: "GET", path: "/api/v1/servers/not-a-uuid"}},
		{"unknown query", req{method: "GET", path: "/api/v1/servers?verbose=1"}},
		{"no operations", req{method: "GET", path: "/api/v1/operations"}},
		{"bad limit", req{method: "GET", path: "/api/v1/operations?limit=0"}},
		{"bad server_id", req{method: "GET", path: "/api/v1/operations?server_id=x"}},
		{"misspelt server_id", req{method: "GET", path: "/api/v1/operations?serverid=" + serverA1}},
		{"missing operation", req{method: "GET", path: "/api/v1/operations/" + missing}},
		{"start bad ID", req{method: "POST", path: "/api/v1/servers/nope/start"}},
		{"start unknown field", req{method: "POST", path: "/api/v1/servers/" + serverA1 + "/start", body: `{"force":true}`}},
		{"start not JSON", req{method: "POST", path: "/api/v1/servers/" + serverA1 + "/start", body: `{}`, contentType: "text/plain"}},
		{"start body too large", req{method: "POST", path: "/api/v1/servers/" + serverA1 + "/start", body: `{"x":"` + strings.Repeat("x", service.MaxBodyBytes) + `"}`}},
		{"start bad key", req{method: "POST", path: "/api/v1/servers/" + serverA1 + "/start", key: "no spaces"}},
		{"start other tenant's", req{method: "POST", path: "/api/v1/servers/" + serverB1 + "/start"}},
		{"start missing", req{method: "POST", path: "/api/v1/servers/" + missing + "/start"}},
		{"resolve missing", req{method: "POST", path: "/api/v1/operations/" + missing + "/resolve", body: `{"outcome":"failed"}`}},
		{"resolve bad ID", req{method: "POST", path: "/api/v1/operations/nope/resolve", body: `{"outcome":"failed"}`}},
	} {
		both(c.name, c.q)
	}
	if n := e.count(t, `SELECT count(*) FROM waystone.operations`); n != 0 {
		t.Fatalf("%d operations after refusals, want 0", n)
	}

	// Start on each socket, each on a server of its own, then compare
	// what was sent and what reads back.
	aw := e.onAPI(1000, req{method: "POST", path: "/api/v1/servers/" + serverA1 + "/start", key: "k-1"})
	ww := e.onWeb(trevor, req{method: "POST", path: "/api/v1/servers/" + serverA2 + "/start", key: "k-1"})
	if aw.Code != http.StatusAccepted {
		t.Fatalf("api start: %d %s", aw.Code, aw.Body.String())
	}
	apiOp, webOp := opID(t, aw), opID(t, ww)
	// The browser's operation is the user's, by name (gate S6).
	if !strings.Contains(ww.Body.String(), `"actor":`+tj) {
		t.Errorf("web start: actor isn't the user: %s", ww.Body.String())
	}
	server := []string{apiOp, "OP", webOp, "OP", serverA1, "SERVER", serverA2, "SERVER"}
	same(t, "start", aw, ww, tj, server...)
	// A replay is 200 with the same operation, on both.
	same(t, "replay",
		e.onAPI(1000, req{method: "POST", path: "/api/v1/servers/" + serverA1 + "/start", key: "k-1"}),
		e.onWeb(trevor, req{method: "POST", path: "/api/v1/servers/" + serverA2 + "/start", key: "k-1"}), tj, server...)
	// The same key for another request is refused.
	both("key reused", req{method: "POST", path: "/api/v1/servers/" + serverA1 + "/stop", key: "k-1"})
	same(t, "get operation",
		e.onAPI(1000, req{method: "GET", path: "/api/v1/operations/" + apiOp}),
		e.onWeb(trevor, req{method: "GET", path: "/api/v1/operations/" + webOp}), tj, server...)
	same(t, "get server while busy",
		e.onAPI(1000, req{method: "GET", path: "/api/v1/servers/" + serverA1}),
		e.onWeb(trevor, req{method: "GET", path: "/api/v1/servers/" + serverA2}), tj, append(server, `"a1"`, "NAME", `"a2"`, "NAME")...)
	// A second action on a busy server: 409 naming the blocking operation.
	same(t, "in progress",
		e.onAPI(1000, req{method: "POST", path: "/api/v1/servers/" + serverA1 + "/stop"}),
		e.onWeb(trevor, req{method: "POST", path: "/api/v1/servers/" + serverA2 + "/stop"}), tj, server...)
	same(t, "not uncertain",
		e.onAPI(1000, req{method: "POST", path: "/api/v1/operations/" + apiOp + "/resolve", body: `{"outcome":"failed"}`}),
		e.onWeb(trevor, req{method: "POST", path: "/api/v1/operations/" + webOp + "/resolve", body: `{"outcome":"failed"}`}), tj, server...)

	// Both operations' outcomes become unknown; each socket resolves its own.
	e.setState(t, apiOp, "running", "uncertain")
	e.setState(t, webOp, "running", "uncertain")
	same(t, "previous uncertain",
		e.onAPI(1000, req{method: "POST", path: "/api/v1/servers/" + serverA1 + "/start"}),
		e.onWeb(trevor, req{method: "POST", path: "/api/v1/servers/" + serverA2 + "/start"}), tj, server...)
	e.now = e.now.Add(time.Hour)
	resolve := `{"outcome":"failed","note":"checked by hand"}`
	aw = e.onAPI(1000, req{method: "POST", path: "/api/v1/operations/" + apiOp + "/resolve", body: resolve})
	ww = e.onWeb(trevor, req{method: "POST", path: "/api/v1/operations/" + webOp + "/resolve", body: resolve})
	if aw.Code != http.StatusOK || !strings.Contains(ww.Body.String(), `"by":`+tj) {
		t.Errorf("resolve: api %d %s, web %d %s", aw.Code, aw.Body.String(), ww.Code, ww.Body.String())
	}
	same(t, "resolve", aw, ww, tj, server...)
	same(t, "already resolved",
		e.onAPI(1000, req{method: "POST", path: "/api/v1/operations/" + apiOp + "/resolve", body: resolve}),
		e.onWeb(trevor, req{method: "POST", path: "/api/v1/operations/" + webOp + "/resolve", body: resolve}), tj, server...)

	// Lists: each socket's own operation, filtered to its server.
	same(t, "list by server",
		e.onAPI(1000, req{method: "GET", path: "/api/v1/operations?server_id=" + serverA1 + "&limit=5"}),
		e.onWeb(trevor, req{method: "GET", path: "/api/v1/operations?server_id=" + serverA2 + "&limit=5"}), tj, server...)
	// The full list is the same two operations on both, the actors
	// included: it's the same data, whoever asks.
	aw, ww = e.onAPI(1000, req{method: "GET", path: "/api/v1/operations"}), e.onWeb(trevor, req{method: "GET", path: "/api/v1/operations"})
	if aw.Code != http.StatusOK || aw.Body.String() != ww.Body.String() {
		t.Errorf("list all differs\napi: %d %s\nweb: %d %s", aw.Code, aw.Body.String(), ww.Code, ww.Body.String())
	}
	both("list servers after", req{method: "GET", path: "/api/v1/servers"})
}

// A browser user whose role may do nothing gets the same 403 as a local
// one, and the refusal is audited under the user (C1, F12).
func TestForbiddenIsAuditedAsUser(t *testing.T) {
	e := setup(t)
	sam := e.signIn(t, "sam")
	sj := actorJSON(e.sam)
	for _, q := range []req{
		{method: "GET", path: "/api/v1/servers"},
		{method: "GET", path: "/api/v1/servers/" + serverA1},
		{method: "POST", path: "/api/v1/servers/" + serverA1 + "/start"},
		{method: "GET", path: "/api/v1/operations"},
	} {
		aw, ww := e.onAPI(5000, q), e.onWeb(sam, q)
		if ww.Code != http.StatusForbidden {
			t.Errorf("%s %s: %d %s, want 403", q.method, q.path, ww.Code, ww.Body.String())
		}
		same(t, q.method+" "+q.path, aw, ww, sj)
	}
	if n := e.count(t, `SELECT count(*) FROM waystone.audit_events
		WHERE actor_type = 'user' AND actor_id = $1 AND actor_name = 'Sam' AND tenant_id = $2
		  AND outcome = 'denied' AND reason = 'forbidden'`, e.sam.ID, e.tenantA); n != 4 {
		t.Errorf("%d denied audit rows for the user, want 4", n)
	}
	if n := e.count(t, `SELECT count(*) FROM waystone.audit_events
		WHERE actor_type = 'user' AND action = 'server.start' AND resource_id = $1`, serverA1); n != 1 {
		t.Errorf("%d audit rows for the refused start, want 1", n)
	}
	if n := e.count(t, `SELECT count(*) FROM waystone.operations`); n != 0 {
		t.Errorf("%d operations, want 0", n)
	}
}

// web.sock's own checks come first: no session is 401 and a POST without
// the session's CSRF token is 403, and neither reaches the database.
func TestSessionAndCSRFComeFirst(t *testing.T) {
	e := setup(t)
	trevor := e.signIn(t, "trevor")
	sam := e.signIn(t, "sam")
	start := req{method: "POST", path: "/api/v1/servers/" + serverA1 + "/start"}
	for _, c := range []struct {
		name   string
		b      browserUser
		q      req
		status int
		code   string
	}{
		{"GET without a session", browserUser{}, req{method: "GET", path: "/api/v1/servers"}, 401, web.CodeUnauthenticated},
		{"POST without a session", browserUser{csrf: trevor.csrf}, start, 401, web.CodeUnauthenticated},
		{"POST without CSRF", browserUser{cookie: trevor.cookie}, start, 403, web.CodeCSRF},
		{"POST with another session's CSRF", browserUser{cookie: trevor.cookie, csrf: sam.csrf}, start, 403, web.CodeCSRF},
		{"resolve without CSRF", browserUser{cookie: trevor.cookie}, req{method: "POST", path: "/api/v1/operations/" + missing + "/resolve", body: `{"outcome":"failed"}`}, 403, web.CodeCSRF},
	} {
		w := e.onWeb(c.b, c.q)
		if w.Code != c.status || !strings.Contains(w.Body.String(), `"code":"`+c.code+`"`) {
			t.Errorf("%s: %d %s, want %d %s", c.name, w.Code, w.Body.String(), c.status, c.code)
		}
	}
	if n := e.count(t, `SELECT count(*) FROM waystone.operations`); n != 0 {
		t.Errorf("%d operations, want 0", n)
	}
	if n := e.count(t, `SELECT count(*) FROM waystone.audit_events WHERE action LIKE 'server.%'`); n != 0 {
		t.Errorf("%d server audit rows, want 0: the refusals came before the service", n)
	}
	// And a signed-out session can't be used again (gate S5).
	if w := e.onWeb(trevor, req{method: "DELETE", path: "/api/v1/session"}); w.Code != http.StatusNoContent {
		t.Fatalf("sign out: %d %s", w.Code, w.Body.String())
	}
	if w := e.onWeb(trevor, req{method: "GET", path: "/api/v1/servers"}); w.Code != http.StatusUnauthorized {
		t.Errorf("after sign-out: %d, want 401", w.Code)
	}
}

// The stage 4 POST limit applies per browser user too, and separately
// from the same person's local identity.
func TestRateLimitPerUser(t *testing.T) {
	e := setup(t)
	trevor := e.signIn(t, "trevor")
	bad := req{method: "POST", path: "/api/v1/servers/not-a-uuid/start"}
	for i := 0; i < service.RateLimit; i++ {
		if w := e.onWeb(trevor, bad); w.Code != http.StatusBadRequest {
			t.Fatalf("POST %d: %d %s, want 400", i+1, w.Code, w.Body.String())
		}
	}
	aw, ww := e.onAPI(1000, bad), e.onWeb(trevor, bad)
	if aw.Code != http.StatusBadRequest {
		t.Errorf("local POST: %d, want 400 (its own limit)", aw.Code)
	}
	if ww.Code != http.StatusTooManyRequests || ww.Header().Get("Retry-After") == "" ||
		!strings.Contains(ww.Body.String(), `"code":"rate_limited"`) {
		t.Errorf("POST %d: %d %s, want 429 rate_limited with Retry-After", service.RateLimit+1, ww.Code, ww.Body.String())
	}
	// Reads aren't limited (W5: the pages poll).
	for i := 0; i < service.RateLimit+5; i++ {
		if w := e.onWeb(trevor, req{method: "GET", path: "/api/v1/servers"}); w.Code != http.StatusOK {
			t.Fatalf("GET %d: %d", i+1, w.Code)
		}
	}
}
