// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package session

// Sign-in end to end, through web's real handler and guard, against a
// real PostgreSQL. Like the store tests, these wipe waystone_test:
//
//	CGO_ENABLED=1 go test -race -tags integration ./internal/session/

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sharddevs/waystone_panel/internal/password"
	"github.com/sharddevs/waystone_panel/internal/store"
	"github.com/sharddevs/waystone_panel/internal/web"
)

// cheap are argon2id costs small enough for tests; the rules don't depend
// on them.
var cheap = password.Params{Memory: 64, Time: 1, Threads: 1}

const (
	goodPassword = "correct horse battery"
	clientIP     = "203.0.113.7"
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

type env struct {
	h     *web.Handler
	m     *Manager
	owner *pgx.Conn
	acc   *store.Accounts
	mu    sync.Mutex
	now   time.Time
	logs  syncBuffer
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
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

func (e *env) clock() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.now
}

func (e *env) advance(d time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.now = e.now.Add(d)
}

var local = store.Actor{Type: store.ActorLocalUser, ID: "1000", Name: "trevor"}

// setup migrates a fresh waystone_test and creates two accounts with the
// owner's user commands: trevor (bootstrap) and sam (create).
func setup(t *testing.T) *env {
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
	for _, q := range []string{
		`DROP SCHEMA IF EXISTS waystone CASCADE`,
		`DROP SCHEMA IF EXISTS waystone_meta CASCADE`,
		`CREATE SCHEMA waystone`,
		`CREATE SCHEMA waystone_meta`,
	} {
		if _, err := owner.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := store.Migrate(ctx, ownerDSN()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	acc, err := store.OpenAccounts(ctx, ownerDSN())
	if err != nil {
		t.Fatalf("OpenAccounts: %v", err)
	}
	t.Cleanup(func() { acc.Close(context.Background()) })
	hash, err := password.Hash(goodPassword, cheap)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acc.Bootstrap(ctx, local, store.NewUser{Username: "trevor", DisplayName: "Trevor", PasswordHash: hash}); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if _, err := acc.Create(ctx, local, store.NewUser{Username: "sam", DisplayName: "Sam", PasswordHash: hash}, "admin"); err != nil {
		t.Fatalf("Create: %v", err)
	}

	st, err := store.Open(ctx, appDSN())
	if err != nil {
		t.Fatalf("Open as app: %v", err)
	}
	t.Cleanup(st.Close)
	e := &env{owner: owner, acc: acc, now: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)}
	st.Now = e.clock
	e.m, err = New(Config{Store: st, Params: cheap, Now: e.clock, Log: log.New(&e.logs, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	o, err := web.ParsePublicURL("https://panel.example.com")
	if err != nil {
		t.Fatal(err)
	}
	e.h, err = web.New(web.Config{Public: o, ProxyUID: 4242, Log: log.New(&e.logs, "", 0), Sessions: e.m}, e.m.Routes()...)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// req is one browser request as Caddy forwards it.
type req struct {
	method, path, body string
	cookie, csrf       string
	ip                 string // default clientIP
}

func (e *env) do(t *testing.T, q req) *httptest.ResponseRecorder {
	t.Helper()
	var body *strings.Reader
	if q.body != "" {
		body = strings.NewReader(q.body)
	} else {
		body = strings.NewReader("")
	}
	r := httptest.NewRequest(q.method, q.path, body)
	r.Host = "panel.example.com"
	ip := q.ip
	if ip == "" {
		ip = clientIP
	}
	r.Header.Set("X-Forwarded-For", ip)
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-Host", "panel.example.com")
	r.Header.Set("User-Agent", "Firefox test")
	if q.method != http.MethodGet {
		r.Header.Set("Origin", "https://panel.example.com")
	}
	if q.body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if q.cookie != "" {
		r.AddCookie(&http.Cookie{Name: web.SessionCookieName, Value: q.cookie})
	}
	if q.csrf != "" {
		r.Header.Set(web.CSRFHeader, q.csrf)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

func login(user, pw string) string {
	b, _ := json.Marshal(map[string]string{"username": user, "password": pw})
	return string(b)
}

// signIn signs in and returns the session token from the cookie.
func (e *env) signIn(t *testing.T, user string) string {
	t.Helper()
	w := e.do(t, req{method: "POST", path: "/api/v1/session", body: login(user, goodPassword)})
	if w.Code != http.StatusNoContent {
		t.Fatalf("sign in %s: %d %s", user, w.Code, w.Body.String())
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == web.SessionCookieName {
			return c.Value
		}
	}
	t.Fatalf("sign in %s: no session cookie", user)
	return ""
}

func (e *env) session(t *testing.T, token string) (sessionJSON, int) {
	t.Helper()
	w := e.do(t, req{method: "GET", path: "/api/v1/session", cookie: token})
	var s sessionJSON
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil {
			t.Fatal(err)
		}
	}
	return s, w.Code
}

func (e *env) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := e.owner.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func wantCode(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if w.Code != status || !strings.Contains(w.Body.String(), `"code":"`+code+`"`) {
		t.Errorf("got %d %s, want %d %s", w.Code, w.Body.String(), status, code)
	}
}

// The whole flow: sign in, see who I am, change appearance, sign out.
func TestSignInFlow(t *testing.T) {
	e := setup(t)
	w := e.do(t, req{method: "POST", path: "/api/v1/session", body: login("trevor", goodPassword)})
	if w.Code != http.StatusNoContent {
		t.Fatalf("sign in: %d %s", w.Code, w.Body.String())
	}
	cookie := w.Header().Get("Set-Cookie")
	for _, want := range []string{web.SessionCookieName + "=", "Max-Age=604800", "HttpOnly", "Secure", "SameSite=Lax", "Path=/"} {
		if !strings.Contains(cookie, want) {
			t.Errorf("Set-Cookie %q lacks %q", cookie, want)
		}
	}
	token := w.Result().Cookies()[0].Value

	s, code := e.session(t, token)
	if code != http.StatusOK {
		t.Fatalf("GET session: %d", code)
	}
	if s.User.DisplayName != "Trevor" || len(s.CSRFToken) != web.TokenLength || s.CSRFToken == token {
		t.Errorf("session %+v", s)
	}
	if !s.ExpiresAt.Equal(e.clock().Add(AbsoluteTimeout)) {
		t.Errorf("expires_at %v, want %v", s.ExpiresAt, e.clock().Add(AbsoluteTimeout))
	}
	if s.Preferences != (preferencesJSON{"stone", "comfortable", "dark"}) {
		t.Errorf("default preferences %+v", s.Preferences)
	}

	// Only the token's SHA-256 is stored, and the row knows the client.
	sum := sha256.Sum256([]byte(token))
	if n := e.count(t, `SELECT count(*) FROM waystone.sessions WHERE token_sha256 = $1 AND client_ip = $2 AND user_agent = 'Firefox test'`, sum[:], clientIP); n != 1 {
		t.Errorf("%d session rows with the token's hash", n)
	}
	if n := e.count(t, `SELECT count(*) FROM waystone.sessions s WHERE position($1 in s::text) > 0`, token); n != 0 {
		t.Error("the raw token is stored")
	}

	w = e.do(t, req{method: "PATCH", path: PreferencesPattern, cookie: token, csrf: s.CSRFToken, body: `{"theme":"slate","density":"compact","mode":"system"}`})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"theme":"slate"`) {
		t.Fatalf("PATCH preferences: %d %s", w.Code, w.Body.String())
	}
	if s, _ = e.session(t, token); s.Preferences != (preferencesJSON{"slate", "compact", "system"}) {
		t.Errorf("preferences after PATCH %+v", s.Preferences)
	}

	w = e.do(t, req{method: "DELETE", path: "/api/v1/session", cookie: token, csrf: s.CSRFToken})
	if w.Code != http.StatusNoContent || !strings.Contains(w.Header().Get("Set-Cookie"), "Max-Age=0") {
		t.Fatalf("sign out: %d %q", w.Code, w.Header().Get("Set-Cookie"))
	}
	if _, code := e.session(t, token); code != http.StatusUnauthorized {
		t.Errorf("after sign out: %d", code)
	}
	if n := e.count(t, `SELECT count(*) FROM waystone.sessions`); n != 0 {
		t.Errorf("%d sessions left after sign out", n)
	}
}

// Every bad sign-in looks the same from outside. Known accounts get an
// audit row; nothing is created.
func TestSignInRefusals(t *testing.T) {
	e := setup(t)
	if err := e.acc.Disable(context.Background(), local, "sam"); err != nil {
		t.Fatal(err)
	}
	var bodies []string
	for _, c := range []struct{ user, pw string }{
		{"trevor", "wrong password!!"},
		{"nobody", goodPassword},
		{"sam", goodPassword}, // disabled
		{"Not A Username!", goodPassword},
	} {
		w := e.do(t, req{method: "POST", path: "/api/v1/session", body: login(c.user, c.pw), ip: "198.51.100." + strconv.Itoa(len(bodies)+1)})
		wantCode(t, w, http.StatusUnauthorized, web.CodeInvalidCredentials)
		if w.Header().Get("Set-Cookie") != "" {
			t.Errorf("%s: a refused sign-in set a cookie", c.user)
		}
		bodies = append(bodies, w.Body.String())
	}
	for _, b := range bodies[1:] {
		if b != bodies[0] {
			t.Errorf("refusals differ: %q vs %q", b, bodies[0])
		}
	}
	if n := e.count(t, `SELECT count(*) FROM waystone.sessions`); n != 0 {
		t.Errorf("%d sessions after refusals", n)
	}
	for reason, want := range map[string]int{"wrong_password": 1, "account_disabled": 1} {
		if n := e.count(t, `SELECT count(*) FROM waystone.audit_events WHERE action = 'session.create' AND outcome = 'denied' AND reason = $1 AND actor_type = 'user'`, reason); n != want {
			t.Errorf("%d audit rows with reason %s, want %d", n, reason, want)
		}
	}

	// Malformed requests.
	for _, c := range []struct {
		body, ctype string
		status      int
		code        string
	}{
		{`{"username":"trevor","password":"` + goodPassword + `","admin":true}`, "", 400, web.CodeBadRequest},
		{`{"username":"trevor"`, "", 400, web.CodeBadRequest},
		{`username=trevor`, "form", 415, web.CodeUnsupportedMedia},
	} {
		r := httptest.NewRequest("POST", "/api/v1/session", strings.NewReader(c.body))
		r.Host = "panel.example.com"
		r.Header.Set("X-Forwarded-For", clientIP)
		r.Header.Set("X-Forwarded-Proto", "https")
		r.Header.Set("X-Forwarded-Host", "panel.example.com")
		r.Header.Set("Origin", "https://panel.example.com")
		if c.ctype == "form" {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		} else {
			r.Header.Set("Content-Type", "application/json")
		}
		w := httptest.NewRecorder()
		e.h.ServeHTTP(w, r)
		wantCode(t, w, c.status, c.code)
	}
}

// Five failures for one username lock it for the rest of the window, even
// with the right password; twenty from one address lock that address.
func TestSignInRateLimit(t *testing.T) {
	e := setup(t)
	for i := 0; i < limitPerUser; i++ {
		w := e.do(t, req{method: "POST", path: "/api/v1/session", body: login("trevor", "wrong password!!"), ip: "198.51.100." + strconv.Itoa(i+1)})
		wantCode(t, w, http.StatusUnauthorized, web.CodeInvalidCredentials)
		e.advance(time.Minute)
	}
	w := e.do(t, req{method: "POST", path: "/api/v1/session", body: login("trevor", goodPassword), ip: "198.51.100.99"})
	wantCode(t, w, http.StatusTooManyRequests, web.CodeTooManyRequests)
	// The first failure was at 12:00 and it's now 12:05: 10 minutes left.
	if got := w.Header().Get("Retry-After"); got != "600" {
		t.Errorf("Retry-After %q, want 600", got)
	}
	if n := e.count(t, `SELECT count(*) FROM waystone.audit_events WHERE reason = 'locked_out'`); n != 1 {
		t.Errorf("%d locked_out audit rows, want 1", n)
	}
	// Another account isn't affected, and the lock ends with the window.
	e.signIn(t, "sam")
	e.advance(10 * time.Minute)
	e.signIn(t, "trevor")

	// One address: twenty failures over many usernames.
	for i := 0; i < limitPerIP; i++ {
		w := e.do(t, req{method: "POST", path: "/api/v1/session", body: login("user"+strconv.Itoa(i), goodPassword), ip: "192.0.2.50"})
		wantCode(t, w, http.StatusUnauthorized, web.CodeInvalidCredentials)
	}
	w = e.do(t, req{method: "POST", path: "/api/v1/session", body: login("trevor", goodPassword), ip: "192.0.2.50"})
	wantCode(t, w, http.StatusTooManyRequests, web.CodeTooManyRequests)
	e.signIn(t, "trevor") // from the usual address
}

// Idle and absolute expiry, and last_seen_at written at most once a minute.
func TestSessionExpiry(t *testing.T) {
	e := setup(t)
	token := e.signIn(t, "trevor")
	lastSeen := func() time.Time {
		var ts time.Time
		if err := e.owner.QueryRow(context.Background(), `SELECT last_seen_at FROM waystone.sessions`).Scan(&ts); err != nil {
			t.Fatal(err)
		}
		return ts
	}
	start := e.clock()
	e.advance(30 * time.Second)
	if _, code := e.session(t, token); code != 200 || !lastSeen().Equal(start) {
		t.Errorf("at +30s: %d, last_seen %v (want it unchanged)", code, lastSeen())
	}
	e.advance(time.Minute)
	if _, code := e.session(t, token); code != 200 || !lastSeen().Equal(e.clock()) {
		t.Errorf("at +90s: %d, last_seen %v (want it touched)", code, lastSeen())
	}

	// Idle: 12 hours without a request ends it, and the row goes.
	e.advance(IdleTimeout)
	if _, code := e.session(t, token); code != http.StatusUnauthorized {
		t.Errorf("after 12 h idle: %d", code)
	}
	if n := e.count(t, `SELECT count(*) FROM waystone.sessions`); n != 0 {
		t.Errorf("%d sessions after idle expiry", n)
	}

	// Absolute: used every 11 hours, it still ends after 7 days.
	token = e.signIn(t, "trevor")
	signedIn := e.clock()
	for e.clock().Add(11 * time.Hour).Before(signedIn.Add(AbsoluteTimeout)) {
		e.advance(11 * time.Hour)
		if _, code := e.session(t, token); code != 200 {
			t.Fatalf("at %v: %d", e.clock().Sub(signedIn), code)
		}
	}
	e.advance(signedIn.Add(AbsoluteTimeout).Sub(e.clock()))
	if _, code := e.session(t, token); code != http.StatusUnauthorized {
		t.Errorf("at 7 days: %d", code)
	}
}

// A sign-in always makes a new session and ends the one the browser came
// with (no fixation).
func TestSignInReplacesSession(t *testing.T) {
	e := setup(t)
	old := e.signIn(t, "trevor")
	w := e.do(t, req{method: "POST", path: "/api/v1/session", body: login("trevor", goodPassword), cookie: old})
	if w.Code != http.StatusNoContent {
		t.Fatalf("second sign in: %d", w.Code)
	}
	fresh := w.Result().Cookies()[0].Value
	if fresh == old {
		t.Fatal("the same token again")
	}
	if _, code := e.session(t, old); code != http.StatusUnauthorized {
		t.Errorf("old session: %d", code)
	}
	if _, code := e.session(t, fresh); code != 200 {
		t.Errorf("new session: %d", code)
	}
	if n := e.count(t, `SELECT count(*) FROM waystone.sessions`); n != 1 {
		t.Errorf("%d sessions, want 1", n)
	}
}

// C-W5 on the real routes: without the session's own token, sign-out and
// the preferences change nothing. Another session's token doesn't work.
func TestSessionRoutesNeedCSRF(t *testing.T) {
	e := setup(t)
	token := e.signIn(t, "trevor")
	other := e.signIn(t, "sam")
	otherCSRF, _ := e.session(t, other)
	for _, csrf := range []string{"", otherCSRF.CSRFToken} {
		w := e.do(t, req{method: "DELETE", path: "/api/v1/session", cookie: token, csrf: csrf})
		wantCode(t, w, http.StatusForbidden, web.CodeCSRF)
		w = e.do(t, req{method: "PATCH", path: PreferencesPattern, cookie: token, csrf: csrf, body: `{"theme":"slate","density":"compact","mode":"light"}`})
		wantCode(t, w, http.StatusForbidden, web.CodeCSRF)
	}
	s, code := e.session(t, token)
	if code != 200 || s.Preferences.Theme != "stone" {
		t.Errorf("after refused requests: %d %+v", code, s.Preferences)
	}
	// Bad preference values, with the right token.
	for _, b := range []string{`{"theme":"neon","density":"compact","mode":"light"}`, `{"theme":"slate","density":"compact"}`, `{"theme":"slate","density":"compact","mode":"light","x":1}`} {
		w := e.do(t, req{method: "PATCH", path: PreferencesPattern, cookie: token, csrf: s.CSRFToken, body: b})
		wantCode(t, w, http.StatusBadRequest, web.CodeBadRequest)
	}
}

// Disabling an account (the owner's command) ends its sessions at once.
func TestDisableEndsSessions(t *testing.T) {
	e := setup(t)
	token := e.signIn(t, "sam")
	if err := e.acc.Disable(context.Background(), local, "sam"); err != nil {
		t.Fatal(err)
	}
	if _, code := e.session(t, token); code != http.StatusUnauthorized {
		t.Errorf("after disable: %d", code)
	}
}

// The log never shows what was typed as a username (it may be a
// password), and junk usernames share one limiter key.
func TestSignInLogsNoUsername(t *testing.T) {
	e := setup(t)
	secret := "my secret password typed in the wrong box"
	for i := 0; i < 3; i++ {
		e.do(t, req{method: "POST", path: "/api/v1/session", body: login(secret+strconv.Itoa(i), goodPassword), ip: "198.51.100." + strconv.Itoa(i+1)})
	}
	e.do(t, req{method: "POST", path: "/api/v1/session", body: login("nobody", "x"), ip: "198.51.100.9"})
	if strings.Contains(e.logs.String(), "secret") || strings.Contains(e.logs.String(), "nobody") {
		t.Errorf("a typed username reached the log:\n%s", e.logs.String())
	}
	e.m.limits.mu.Lock()
	n := len(e.m.limits.failures)
	e.m.limits.mu.Unlock()
	// 4 addresses, the shared junk key, and "user:nobody".
	if n != 6 {
		t.Errorf("%d limiter keys, want 6", n)
	}
}
