// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// Setup for the API's integration tests: the real handler, a real
// PostgreSQL, and a fake caller identity. Like the store's tests, these
// wipe the waystone_test database. Both packages use the same database,
// so run them one package at a time:
//
//	CGO_ENABLED=1 go test -race -p 1 -tags integration ./internal/...

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sharddevs/waystone_panel/internal/store"
)

const (
	tenantA       = "0199e1a0-0000-7000-8000-00000000000a"
	tenantB       = "0199e1a0-0000-7000-8000-00000000000b"
	nodeA         = "0199e1a0-0000-7000-8000-0000000000a1"
	nodeB         = "0199e1a0-0000-7000-8000-0000000000b1"
	serverA1      = "0199e1a0-0000-7000-8000-000000000a01"
	serverA2      = "0199e1a0-0000-7000-8000-000000000a02"
	serverB1      = "0199e1a0-0000-7000-8000-000000000b01"
	missingServer = "0199e1a0-0000-7000-8000-00000000ffff"
)

// The callers, by uid. names is what the fake passwd lookup knows.
const (
	uidAdminA   = 1000 // admin of A
	uidAdminB   = 2000 // admin of B
	uidAdminAB  = 3000 // admin of A and B: ambiguous
	uidStranger = 4000 // no membership
	uidNodeRole = 5000 // member of A with role "node": not allowed any API action
)

var names = map[string]string{"1000": "admin a", "2000": "admin b", "5000": "not an admin"}

func fakeLookup(uid string) (string, error) {
	if n, ok := names[uid]; ok {
		return n, nil
	}
	return "", errors.New("no such user")
}

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
	h     http.Handler
	owner *pgx.Conn
	now   time.Time    // the rate limiter's and the store's clock
	logs  bytes.Buffer // everything the API logged
}

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
	// Fixtures move operations by hand, which the 4c guard allows the
	// owner only with the override on. The history rules still apply.
	mustExec(t, owner, `SET waystone.owner_override = 'on'`)
	for _, q := range []string{
		`DROP SCHEMA IF EXISTS waystone CASCADE`,
		`DROP SCHEMA IF EXISTS waystone_meta CASCADE`,
		`CREATE SCHEMA waystone`,
		`CREATE SCHEMA waystone_meta`,
	} {
		mustExec(t, owner, q)
	}
	if err := store.Migrate(ctx, ownerDSN()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	for _, q := range []string{
		`INSERT INTO waystone.tenants (tenant_id, name) VALUES ('` + tenantA + `', 'A'), ('` + tenantB + `', 'B')`,
		`INSERT INTO waystone.memberships (tenant_id, actor_type, actor_id, role) VALUES
			('` + tenantA + `', 'local_user', '1000', 'admin'),
			('` + tenantB + `', 'local_user', '2000', 'admin'),
			('` + tenantA + `', 'local_user', '3000', 'admin'),
			('` + tenantB + `', 'local_user', '3000', 'admin'),
			('` + tenantA + `', 'local_user', '5000', 'node')`,
		`INSERT INTO waystone.nodes (tenant_id, node_id, name) VALUES
			('` + tenantA + `', '` + nodeA + `', 'node a'), ('` + tenantB + `', '` + nodeB + `', 'node b')`,
		`INSERT INTO waystone.servers (tenant_id, server_id, node_id, name, profile, runtime) VALUES
			('` + tenantA + `', '` + serverA1 + `', '` + nodeA + `', 'a1', 'minecraft-java', 'systemd'),
			('` + tenantA + `', '` + serverA2 + `', '` + nodeA + `', 'a2', 'minecraft-java', 'systemd'),
			('` + tenantB + `', '` + serverB1 + `', '` + nodeB + `', 'b1', 'minecraft-java', 'systemd')`,
	} {
		mustExec(t, owner, q)
	}

	st, err := store.Open(ctx, appDSN())
	if err != nil {
		t.Fatalf("Open as app: %v", err)
	}
	t.Cleanup(st.Close)
	e := &env{owner: owner, now: time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)}
	st.Now = func() time.Time { return e.now }
	e.h = New(Config{Version: "test", Store: st, Lookup: fakeLookup, Log: log.New(&e.logs, "", 0),
		Now: func() time.Time { return e.now }})
	return e
}

// securityEvents returns the security_event lines logged so far.
func (e *env) securityEvents() []string {
	var out []string
	for _, l := range strings.Split(e.logs.String(), "\n") {
		if strings.HasPrefix(l, "security_event=") {
			out = append(out, l)
		}
	}
	return out
}

func mustExec(t *testing.T, conn *pgx.Conn, sql string, args ...any) {
	t.Helper()
	if _, err := conn.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// req is one request to send. body "" means no body; a body is sent as
// application/json unless contentType says otherwise.
type req struct {
	method, path, body string
	uid                uint32
	key                string // Idempotency-Key
	contentType        string
}

func (e *env) do(t *testing.T, r req) *httptest.ResponseRecorder {
	t.Helper()
	var body io.Reader
	if r.body != "" {
		body = strings.NewReader(r.body)
	}
	hr := httptest.NewRequest(r.method, r.path, body)
	if r.body != "" {
		ct := r.contentType
		if ct == "" {
			ct = "application/json"
		}
		hr.Header.Set("Content-Type", ct)
	}
	if r.key != "" {
		hr.Header.Set("Idempotency-Key", r.key)
	}
	hr = hr.WithContext(ContextWithPeer(hr.Context(), Peer{UID: r.uid}))
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, hr)
	return rec
}

// wantOK decodes a success response into v and checks its status.
func wantOK(t *testing.T, rec *httptest.ResponseRecorder, status int, v any) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status %d, want %d: %s", rec.Code, status, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("response isn't JSON: %v (%s)", err, rec.Body.String())
	}
}

// wantError checks an error response's status and code and returns it.
func wantError(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) errorJSON {
	t.Helper()
	e := decodeError(t, rec)
	if rec.Code != status || e.Code != code {
		t.Fatalf("got %d %s, want %d %s: %s", rec.Code, e.Code, status, code, rec.Body.String())
	}
	return e
}

func start(server string) string { return "/api/v1/servers/" + server + "/start" }
func stop(server string) string  { return "/api/v1/servers/" + server + "/stop" }

// setState moves an operation as the owner, with the override on: the
// history rules still apply, the per-actor rules don't.
func (e *env) setState(t *testing.T, opID string, states ...string) {
	t.Helper()
	for _, s := range states {
		mustExec(t, e.owner, `UPDATE waystone.operations SET state = $1 WHERE operation_id = $2`, s, opID)
	}
}
