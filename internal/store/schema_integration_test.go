// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

import (
	"context"
	"errors"
	"testing"
)

// I1: every table in the waystone schema has tenant_id, RLS enabled and
// RLS forced, and the expected tables all exist.
func TestEveryTableHasTenantAndForcedRLS(t *testing.T) {
	e := setup(t)
	rows, err := e.owner.Query(context.Background(), `
		SELECT c.relname, c.relrowsecurity, c.relforcerowsecurity,
		       EXISTS (SELECT 1 FROM information_schema.columns col
		               WHERE col.table_schema = 'waystone' AND col.table_name = c.relname
		                 AND col.column_name = 'tenant_id' AND col.data_type = 'uuid' AND col.is_nullable = 'NO')
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'waystone' AND c.relkind IN ('r', 'p')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var name string
		var rls, force, tenant bool
		if err := rows.Scan(&name, &rls, &force, &tenant); err != nil {
			t.Fatal(err)
		}
		seen[name] = true
		if !rls || !force {
			t.Errorf("table %s: row security enabled=%v forced=%v, want both", name, rls, force)
		}
		if !tenant {
			t.Errorf("table %s: no NOT NULL uuid tenant_id column", name)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, want := range wantTables {
		if !seen[want] {
			t.Errorf("table %s missing", want)
		}
	}
}

// I7: the runtime role can't change schema, policies or privileges, and
// has nothing that would bypass RLS.
func TestAppRoleCannotChangeSecurity(t *testing.T) {
	e := setup(t)
	ctx := context.Background()

	var super, bypass bool
	if err := e.owner.QueryRow(ctx, `SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = 'waystone_app'`).Scan(&super, &bypass); err != nil {
		t.Fatal(err)
	}
	if super || bypass {
		t.Fatalf("waystone_app rolsuper=%v rolbypassrls=%v, want both false", super, bypass)
	}
	var owned int
	if err := e.owner.QueryRow(ctx, `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname IN ('waystone', 'waystone_meta') AND pg_get_userbyid(c.relowner) = 'waystone_app'`).Scan(&owned); err != nil {
		t.Fatal(err)
	}
	if owned != 0 {
		t.Fatalf("waystone_app owns %d relations, want 0", owned)
	}

	app := appConn(t)
	attempts := []string{
		`ALTER TABLE waystone.servers DISABLE ROW LEVEL SECURITY`,
		`ALTER TABLE waystone.servers NO FORCE ROW LEVEL SECURITY`,
		`DROP POLICY tenant_rows ON waystone.servers`,
		`CREATE POLICY sneaky ON waystone.servers TO waystone_app USING (true)`,
		`ALTER TABLE waystone.operations DISABLE TRIGGER ALL`,
		`CREATE TABLE waystone.sneaky (i int)`,
		`CREATE TABLE waystone_meta.sneaky (i int)`,
		`INSERT INTO waystone.memberships (tenant_id, actor_type, actor_id, role)
			VALUES ('` + tenantB + `', 'local_user', '1000', 'admin')`,
		`DELETE FROM waystone.operations`,
		`UPDATE waystone_meta.goose_db_version SET is_applied = false`,
		`SET ROLE waystone_owner`,
	}
	for _, q := range attempts {
		if _, err := app.Exec(ctx, q); err == nil {
			t.Errorf("waystone_app ran %q, want an error", q)
		}
	}
}

// I2: a transaction with no actor or tenant settings sees nothing and can
// write nothing, on a fresh connection and on a reused one (where the
// settings read as "" rather than NULL).
func TestNoSettingsSeesNothing(t *testing.T) {
	setup(t)
	ctx := context.Background()
	app := appConn(t)

	check := func(when string) {
		for _, table := range wantTables {
			if n := count(t, app, table); n != 0 {
				t.Errorf("%s: %s shows %d rows with no settings, want 0", when, table, n)
			}
		}
		_, err := app.Exec(ctx, `INSERT INTO waystone.operations
			(tenant_id, operation_id, resource_type, resource_id, node_id, action, actor_type, actor_id, actor_name, requested_at)
			VALUES ($1, '0199e1a0-0000-7000-8000-00000000eeee', 'server', $2, $3, 'server.start', 'local_user', '1000', 'x', now())`,
			tenantA, serverA1, nodeA)
		if err == nil {
			t.Errorf("%s: insert with no settings succeeded", when)
		}
	}
	check("fresh connection")

	// Use the connection properly once and commit. The settings were local
	// to that transaction, so the next one on this connection must not see
	// them.
	tx, err := app.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('app.actor_type', 'local_user', true),
		set_config('app.actor_id', '1000', true), set_config('app.tenant_id', $1, true)`, tenantA); err != nil {
		t.Fatal(err)
	}
	if n := count(t, tx, "servers"); n != 2 {
		t.Fatalf("with settings: servers = %d, want 2", n)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	check("reused connection")
}

// I5: setting the tenant isn't enough; the actor must also be a member.
func TestForgedTenantSeesNothing(t *testing.T) {
	setup(t)
	ctx := context.Background()
	app := appConn(t)

	look := func(actorType, actorID, tenant string) map[string]int {
		tx, err := app.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, `SELECT set_config('app.actor_type', $1, true),
			set_config('app.actor_id', $2, true), set_config('app.tenant_id', $3, true)`, actorType, actorID, tenant); err != nil {
			t.Fatal(err)
		}
		got := map[string]int{}
		for _, table := range []string{"tenants", "nodes", "servers"} {
			got[table] = count(t, tx, table)
		}
		return got
	}

	if got := look("local_user", "1000", tenantB); got["tenants"]+got["nodes"]+got["servers"] != 0 {
		t.Errorf("admin of A with tenant forged to B sees %v, want nothing", got)
	}
	if got := look("local_user", "4000", tenantA); got["tenants"]+got["nodes"]+got["servers"] != 0 {
		t.Errorf("non-member with tenant A sees %v, want nothing", got)
	}
	if got := look("local_user", "1000", tenantA); got["tenants"] != 1 || got["nodes"] != 1 || got["servers"] != 2 {
		t.Errorf("admin of A in A sees %v, want tenants 1, nodes 1, servers 2", got)
	}
	if got := look("local_user", "2000", tenantB); got["servers"] != 1 {
		t.Errorf("admin of B in B sees %v, want servers 1", got)
	}

	// WITH CHECK: a member of A can't insert a row into B.
	tx, err := app.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT set_config('app.actor_type', 'local_user', true),
		set_config('app.actor_id', '1000', true), set_config('app.tenant_id', $1, true)`, tenantA); err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO waystone.audit_events
		(tenant_id, event_id, actor_type, actor_id, actor_name, action, resource_type, resource_id, outcome, reason, request_id, at)
		VALUES ($1, '0199e1a0-0000-7000-8000-00000000dddd', 'local_user', '1000', 'x', 'server.start', 'server', 'x', 'denied', 'forbidden', 'r', now())`,
		tenantB)
	if sqlState(err) != "42501" {
		t.Errorf("insert into another tenant: err = %v, want SQLSTATE 42501 (RLS WITH CHECK)", err)
	}
}

// C5: the memberships policy depends only on the actor settings, so the
// lookup works before any tenant is set and shows only the actor's own
// rows.
func TestMembershipLookupIsActorOnly(t *testing.T) {
	setup(t)
	ctx := context.Background()
	app := appConn(t)
	tx, err := app.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT set_config('app.actor_type', 'local_user', true), set_config('app.actor_id', '3000', true)`); err != nil {
		t.Fatal(err)
	}
	if n := count(t, tx, "memberships"); n != 2 {
		t.Fatalf("actor 3000 sees %d memberships with no tenant set, want its own 2", n)
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('app.actor_id', '1000', true)`); err != nil {
		t.Fatal(err)
	}
	if n := count(t, tx, "memberships"); n != 1 {
		t.Fatalf("actor 1000 sees %d memberships, want 1", n)
	}
}

// C5 through the Go API.
func TestInTenantTx(t *testing.T) {
	e := setup(t)
	ctx := context.Background()

	var tenant, role string
	err := e.store.InTenantTx(ctx, adminA, func(ctx context.Context, tx *Tx) error {
		tenant, role = tx.TenantID(), tx.Role()
		if tx.Actor() != adminA {
			t.Errorf("tx.Actor() = %+v, want %+v", tx.Actor(), adminA)
		}
		if !tx.Now().Equal(e.clock.now) {
			t.Errorf("tx.Now() = %v, want the store clock %v", tx.Now(), e.clock.now)
		}
		return nil
	})
	if err != nil || tenant != tenantA || role != RoleAdmin {
		t.Fatalf("adminA: tenant %q role %q err %v, want %q admin nil", tenant, role, err, tenantA)
	}

	err = e.store.InTenantTx(ctx, nodeActA, func(ctx context.Context, tx *Tx) error {
		tenant, role = tx.TenantID(), tx.Role()
		return nil
	})
	if err != nil || tenant != tenantA || role != RoleNode {
		t.Fatalf("node A: tenant %q role %q err %v", tenant, role, err)
	}

	called := false
	fn := func(context.Context, *Tx) error { called = true; return nil }
	if err := e.store.InTenantTx(ctx, stranger, fn); !errors.Is(err, ErrNoMembership) {
		t.Errorf("stranger: err = %v, want ErrNoMembership", err)
	}
	if err := e.store.InTenantTx(ctx, adminAB, fn); !errors.Is(err, ErrAmbiguousMembership) {
		t.Errorf("admin of two tenants: err = %v, want ErrAmbiguousMembership", err)
	}
	if called {
		t.Error("fn ran for an actor without exactly one membership")
	}

	// An error from fn rolls everything back and comes back unchanged.
	sentinel := errors.New("sentinel")
	err = e.store.InTenantTx(ctx, adminA, func(ctx context.Context, tx *Tx) error {
		if _, _, err := tx.CreateOperation(ctx, CreateOperationInput{ServerID: serverA1, Action: ActionStart}); err != nil {
			t.Fatalf("CreateOperation: %v", err)
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the callback's error", err)
	}
	if n := count(t, e.owner, "operations"); n != 0 {
		t.Fatalf("operations = %d after a rolled-back transaction, want 0", n)
	}
}

// I11: history can't be rewritten.
func TestOperationHistoryProtected(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	op, _, err := e.create(adminA, CreateOperationInput{ServerID: serverA1, Action: ActionStart})
	if err != nil {
		t.Fatal(err)
	}

	app := appConn(t)
	asAdminA := func(q string, args ...any) error {
		tx, err := app.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, `SELECT set_config('app.actor_type', 'local_user', true),
			set_config('app.actor_id', '1000', true), set_config('app.tenant_id', $1, true)`, tenantA); err != nil {
			t.Fatal(err)
		}
		tag, err := tx.Exec(ctx, q, args...)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return errors.New("no row changed")
		}
		return tx.Commit(ctx)
	}

	// Snapshot columns are not updatable by the app role at all.
	for _, col := range []string{"actor_id", "actor_name", "tenant_id", "resource_id", "node_id", "action", "requested_at"} {
		err := asAdminA(`UPDATE waystone.operations SET `+col+` = `+col+` WHERE operation_id = $1`, op.ID)
		if sqlState(err) != "42501" {
			t.Errorf("update %s: err = %v, want SQLSTATE 42501", col, err)
		}
	}
	if err := asAdminA(`DELETE FROM waystone.operations WHERE operation_id = $1`, op.ID); sqlState(err) != "42501" {
		t.Errorf("delete: err = %v, want SQLSTATE 42501", err)
	}

	// The history rules bind everyone, so they're walked here as the
	// owner with the override on, which skips only the per-actor rules
	// (4c, S4). Those have their own tests in dispatch_integration_test.go.
	setState := func(s OperationState) error {
		_, err := e.owner.Exec(ctx, `UPDATE waystone.operations SET state = $1 WHERE operation_id = $2`, string(s), op.ID)
		return err
	}
	steps := []struct {
		to OperationState
		ok bool
	}{
		{StateSucceeded, false}, // pending -> succeeded skips running
		{StateUncertain, false}, // pending -> uncertain
		{StateRunning, true},
		{StatePending, false}, // no going back
		{StateUncertain, true},
		{StateSucceeded, false}, // uncertain changes only by resolve
		{StateRunning, false},
	}
	for _, s := range steps {
		err := setState(s.to)
		if s.ok && err != nil {
			t.Fatalf("-> %s: %v, want allowed", s.to, err)
		}
		if !s.ok && err == nil {
			t.Fatalf("-> %s allowed, want refused", s.to)
		}
	}

	// One resolve is allowed; nothing changes after it.
	if err := asAdminA(`UPDATE waystone.operations SET resolved_at = now(), resolved_outcome = 'failed',
		resolved_by_type = 'local_user', resolved_by_id = '1000', resolved_by_name = 'x' WHERE operation_id = $1`, op.ID); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	if err := asAdminA(`UPDATE waystone.operations SET resolved_outcome = 'succeeded' WHERE operation_id = $1`, op.ID); err == nil {
		t.Fatal("changing a resolution was allowed")
	}

	// The trigger binds the owner too: a finished operation is final.
	op2, _, err := e.create(adminA, CreateOperationInput{ServerID: serverA2, Action: ActionStop})
	if err != nil {
		t.Fatal(err)
	}
	e.setState(t, op2.ID, StateRunning, StateSucceeded)
	if _, err := e.owner.Exec(ctx, `UPDATE waystone.operations SET state = 'failed' WHERE operation_id = $1`, op2.ID); err == nil {
		t.Fatal("owner changed a succeeded operation")
	}
}
