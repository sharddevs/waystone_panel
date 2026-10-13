// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Stage 4b-2 tests for migration 00003 and RecordChurn (D1): the churn row
// goes in as waystone_app, through the node's own membership, with no new
// privileges.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pressly/goose/v3"
)

// The node writes its flagged row through InTenantTx, the same path the
// controller uses. An admin can't, through the function or around it.
func TestRecordChurn(t *testing.T) {
	e := setup(t)
	ctx := context.Background()

	if err := e.store.InTenantTx(ctx, nodeActA, func(ctx context.Context, tx *Tx) error {
		return tx.RecordChurn(ctx, 42)
	}); err != nil {
		t.Fatalf("RecordChurn as the node: %v", err)
	}
	var tenant, actorType, actorID, action, resType, resID, reason, req, outcome string
	var at time.Time
	if err := e.owner.QueryRow(ctx, `SELECT tenant_id::text, actor_type, actor_id, action, resource_type,
		resource_id, reason, request_id, outcome, at FROM waystone.audit_events`).Scan(
		&tenant, &actorType, &actorID, &action, &resType, &resID, &reason, &req, &outcome, &at); err != nil {
		t.Fatal(err)
	}
	if tenant != tenantA || actorType != "node" || actorID != nodeA || action != "node.session_churn" ||
		resType != "node" || resID != nodeA || reason != "too_many_takeovers" || req != "epoch-42" ||
		outcome != "flagged" || !at.Equal(e.clock.now) {
		t.Errorf("churn row = %s %s %s %s %s %s %s %s %s %v",
			tenant, actorType, actorID, action, resType, resID, reason, req, outcome, at)
	}

	// An admin calling the function.
	if err := e.store.InTenantTx(ctx, adminA, func(ctx context.Context, tx *Tx) error {
		return tx.RecordChurn(ctx, 43)
	}); !errors.Is(err, ErrNotNode) {
		t.Errorf("RecordChurn as admin: err = %v, want ErrNotNode", err)
	}
	// An admin inserting a flagged row directly: the CHECK refuses it.
	conn := appConn(t)
	tx := beginAs(t, conn, "local_user", "1000", tenantA)
	_, err := tx.Exec(ctx, `INSERT INTO waystone.audit_events (tenant_id, event_id, actor_type, actor_id,
		actor_name, action, resource_type, resource_id, reason, request_id, outcome, at)
		VALUES ($1, gen_random_uuid(), 'local_user', '1000', 'x', 'a', 'b', 'c', 'd', 'e', 'flagged', now())`, tenantA)
	tx.Rollback(ctx)
	if sqlState(err) != "23514" {
		t.Errorf("admin flagged row: err = %v, want a CHECK violation", err)
	}
	// The node can't write into a tenant it isn't a member of.
	tx = beginAs(t, conn, "node", nodeA, tenantB)
	_, err = tx.Exec(ctx, `INSERT INTO waystone.audit_events (tenant_id, event_id, actor_type, actor_id,
		actor_name, action, resource_type, resource_id, reason, request_id, outcome, at)
		VALUES ($1, gen_random_uuid(), 'node', $2, 'x', 'a', 'b', 'c', 'd', 'e', 'flagged', now())`, tenantB, nodeA)
	tx.Rollback(ctx)
	if sqlState(err) != "42501" {
		t.Errorf("node into tenant B: err = %v, want an RLS violation", err)
	}
	if n := count(t, e.owner, "audit_events"); n != 1 {
		t.Errorf("audit rows = %d, want 1", n)
	}
}

// Migration 00003 keeps existing denials, widens only the outcome CHECK,
// and leaves waystone_app's privileges on audit_events exactly as they
// were: SELECT and INSERT, no column grants, the same two policies.
func TestMigration00003(t *testing.T) {
	e := setup(t)
	ctx := context.Background()

	mustExec(t, e.owner, `DROP SCHEMA waystone CASCADE`)
	mustExec(t, e.owner, `DROP SCHEMA waystone_meta CASCADE`)
	mustExec(t, e.owner, `CREATE SCHEMA waystone`)
	mustExec(t, e.owner, `CREATE SCHEMA waystone_meta`)
	p := provider(t)
	if _, err := p.UpTo(ctx, 2); err != nil {
		t.Fatal(err)
	}
	mustExec(t, e.owner, `INSERT INTO waystone.tenants (tenant_id, name) VALUES ($1, 'A')`, tenantA)
	mustExec(t, e.owner, `INSERT INTO waystone.audit_events (tenant_id, event_id, actor_type, actor_id,
		actor_name, action, resource_type, resource_id, reason, request_id, outcome, at)
		VALUES ($1, gen_random_uuid(), 'local_user', '1000', 'x', 'a', 'b', 'c', 'd', 'e', 'denied', now())`, tenantA)
	before := auditPrivileges(t, e.owner)

	if err := Migrate(ctx, ownerDSN()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if n := count(t, e.owner, "audit_events"); n != 1 {
		t.Errorf("audit rows after migrate = %d, want 1", n)
	}
	after := auditPrivileges(t, e.owner)
	if before != after {
		t.Errorf("privileges changed:\nbefore %s\nafter  %s", before, after)
	}
	if !strings.Contains(after, "table=INSERT,SELECT") || !strings.Contains(after, "columns=;") {
		t.Errorf("privileges = %s, want table=INSERT,SELECT and no column grants", after)
	}
	var def string
	if err := e.owner.QueryRow(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint
		WHERE conname = 'audit_events_outcome_check'`).Scan(&def); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(def, "'flagged'") || !strings.Contains(def, "'node'") {
		t.Errorf("outcome check = %s", def)
	}
	// Anything other than the two outcomes is still refused, even from
	// the owner.
	_, err := e.owner.Exec(ctx, `INSERT INTO waystone.audit_events (tenant_id, event_id, actor_type, actor_id,
		actor_name, action, resource_type, resource_id, reason, request_id, outcome, at)
		VALUES ($1, gen_random_uuid(), 'node', 'n', 'x', 'a', 'b', 'c', 'd', 'e', 'allowed', now())`, tenantA)
	if sqlState(err) != "23514" {
		t.Errorf("outcome 'allowed': err = %v, want a CHECK violation", err)
	}
}

// auditPrivileges describes everything waystone_app may do on
// audit_events: table privileges, column privileges and policies.
func auditPrivileges(t *testing.T, owner *pgx.Conn) string {
	t.Helper()
	var table, columns, policies string
	if err := owner.QueryRow(context.Background(), `
		SELECT
		  (SELECT coalesce(string_agg(privilege_type, ',' ORDER BY privilege_type), '')
		     FROM information_schema.table_privileges
		    WHERE grantee = 'waystone_app' AND table_schema = 'waystone' AND table_name = 'audit_events'),
		  (SELECT coalesce(string_agg(column_name || ':' || privilege_type, ',' ORDER BY 1), '')
		     FROM information_schema.column_privileges c
		    WHERE grantee = 'waystone_app' AND table_schema = 'waystone' AND table_name = 'audit_events'
		      AND NOT EXISTS (SELECT 1 FROM information_schema.table_privileges tp
		                       WHERE tp.grantee = c.grantee AND tp.table_schema = c.table_schema
		                         AND tp.table_name = c.table_name AND tp.privilege_type = c.privilege_type)),
		  (SELECT coalesce(string_agg(policyname || ':' || cmd || ':' || roles::text || ':' ||
		          coalesce(qual, '') || ':' || coalesce(with_check, ''), ';' ORDER BY policyname), '')
		     FROM pg_policies WHERE schemaname = 'waystone' AND tablename = 'audit_events')`).Scan(
		&table, &columns, &policies); err != nil {
		t.Fatal(err)
	}
	return "table=" + table + " columns=" + columns + "; policies=" + policies
}

// provider is a goose provider on the test database, for tests that stop
// at an older version.
func provider(t *testing.T) *goose.Provider {
	t.Helper()
	db, err := goose.OpenDBWithDriver("pgx", ownerDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	p, err := goose.NewProvider(goose.DialectPostgres, db, Migrations(),
		goose.WithTableName(VersionTable), goose.WithDisableGlobalRegistry(true))
	if err != nil {
		t.Fatal(err)
	}
	return p
}
