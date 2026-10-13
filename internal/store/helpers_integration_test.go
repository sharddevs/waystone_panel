// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Shared setup for the integration tests. They run against a real
// PostgreSQL (never mocks) and wipe the waystone_test database each time:
//
//	CGO_ENABLED=1 go test -race -tags integration ./internal/store/
//
// The connection strings default to the peer-authenticated roles created
// by deploy/postgres/bootstrap.sql; override them with
// WAYSTONE_TEST_OWNER_DSN and WAYSTONE_TEST_APP_DSN. A database that can't
// be reached fails the tests, it never skips them.

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	defaultOwnerDSN = "host=/var/run/postgresql dbname=waystone_test user=waystone_owner"
	defaultAppDSN   = "host=/var/run/postgresql dbname=waystone_test user=waystone_app"
)

// Fixture IDs. Tenant A has two servers on node A; tenant B has one.
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

// Fixture actors.
var (
	adminA   = Actor{Type: ActorLocalUser, ID: "1000", Name: "admin a"}  // admin of A
	adminB   = Actor{Type: ActorLocalUser, ID: "2000", Name: "admin b"}  // admin of B
	adminAB  = Actor{Type: ActorLocalUser, ID: "3000", Name: "admin ab"} // admin of A and B
	stranger = Actor{Type: ActorLocalUser, ID: "4000", Name: "stranger"} // no membership
	nodeActA = Actor{Type: ActorNode, ID: nodeA, Name: "node a"}         // node member of A
)

// The tables the first migration creates. Each one must have tenant_id
// and forced RLS.
var wantTables = []string{"tenants", "memberships", "nodes", "servers", "operations", "idempotency_keys", "audit_events"}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func ownerDSN() string { return envOr("WAYSTONE_TEST_OWNER_DSN", defaultOwnerDSN) }
func appDSN() string   { return envOr("WAYSTONE_TEST_APP_DSN", defaultAppDSN) }

// testClock is a clock the tests can set, used as Store.Now. It only uses
// whole seconds, so a time read back from PostgreSQL (which keeps
// microseconds) compares equal to the one written.
type testClock struct{ now time.Time }

func (c *testClock) Now() time.Time          { return c.now }
func (c *testClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

// env is one freshly migrated and seeded database.
type env struct {
	store *Store
	owner *pgx.Conn // waystone_owner, for setup and for checking results
	clock *testClock
}

// setup wipes waystone_test, runs the real migrations and inserts the
// fixtures. Tests that use it share one database, so they must not run
// in parallel.
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
	mustExec(t, owner, `DROP SCHEMA IF EXISTS waystone CASCADE`)
	mustExec(t, owner, `DROP SCHEMA IF EXISTS waystone_meta CASCADE`)
	mustExec(t, owner, `CREATE SCHEMA waystone`)
	mustExec(t, owner, `CREATE SCHEMA waystone_meta`)

	if err := Migrate(ctx, ownerDSN()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	seed := []string{
		`INSERT INTO waystone.tenants (tenant_id, name) VALUES ('` + tenantA + `', 'A'), ('` + tenantB + `', 'B')`,
		`INSERT INTO waystone.memberships (tenant_id, actor_type, actor_id, role) VALUES
			('` + tenantA + `', 'local_user', '1000', 'admin'),
			('` + tenantB + `', 'local_user', '2000', 'admin'),
			('` + tenantA + `', 'local_user', '3000', 'admin'),
			('` + tenantB + `', 'local_user', '3000', 'admin'),
			('` + tenantA + `', 'node', '` + nodeA + `', 'node')`,
		`INSERT INTO waystone.nodes (tenant_id, node_id, name) VALUES
			('` + tenantA + `', '` + nodeA + `', 'node a'), ('` + tenantB + `', '` + nodeB + `', 'node b')`,
		`INSERT INTO waystone.servers (tenant_id, server_id, node_id, name, profile, runtime) VALUES
			('` + tenantA + `', '` + serverA1 + `', '` + nodeA + `', 'a1', 'minecraft-java', 'systemd'),
			('` + tenantA + `', '` + serverA2 + `', '` + nodeA + `', 'a2', 'minecraft-java', 'systemd'),
			('` + tenantB + `', '` + serverB1 + `', '` + nodeB + `', 'b1', 'minecraft-java', 'systemd')`,
	}
	for _, q := range seed {
		mustExec(t, owner, q)
	}

	s, err := Open(ctx, appDSN())
	if err != nil {
		t.Fatalf("Open as app: %v", err)
	}
	t.Cleanup(s.Close)
	clock := &testClock{now: time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)}
	s.Now = clock.Now
	return &env{store: s, owner: owner, clock: clock}
}

func mustExec(t *testing.T, conn *pgx.Conn, sql string, args ...any) {
	t.Helper()
	if _, err := conn.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// appConn opens a raw waystone_app connection, for tests that need to set
// (or deliberately not set) the session settings themselves.
func appConn(t *testing.T) *pgx.Conn {
	t.Helper()
	c, err := pgx.Connect(context.Background(), appDSN())
	if err != nil {
		t.Fatalf("connect as app: %v", err)
	}
	t.Cleanup(func() { c.Close(context.Background()) })
	return c
}

// count returns SELECT count(*) FROM <table> on conn.
func count(t *testing.T, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, table string) int {
	t.Helper()
	var n int
	if err := q.QueryRow(context.Background(), `SELECT count(*) FROM waystone.`+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// sqlState returns the PostgreSQL error code of err, or "".
func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// create runs CreateOperation in its own InTenantTx.
func (e *env) create(actor Actor, in CreateOperationInput) (Operation, bool, error) {
	var op Operation
	var replayed bool
	err := e.store.InTenantTx(context.Background(), actor, func(ctx context.Context, tx *Tx) error {
		var err error
		op, replayed, err = tx.CreateOperation(ctx, in)
		return err
	})
	return op, replayed, err
}

// setState moves an operation as the owner, with the override on: the
// history rules still apply, the per-actor rules don't.
func (e *env) setState(t *testing.T, opID string, states ...OperationState) {
	t.Helper()
	for _, s := range states {
		mustExec(t, e.owner, `UPDATE waystone.operations SET state = $1 WHERE operation_id = $2`, string(s), opID)
	}
}
