// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Stage 4c-1, Q5 and S4 (M4): migration 00004's operation guard, driven
// with raw SQL as the real roles, so it's the database that refuses and
// not the Go code in front of it.

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// A local user with a node-role membership in A and an admin one in B:
// a member of A, but not an admin there.
const forgedUser = "5000"

// asApp runs one statement as waystone_app with exactly the given
// session settings ("" leaves one unset) and returns the rows changed.
func asApp(t *testing.T, actorType, actorID, tenant, q string, args ...any) (int64, error) {
	t.Helper()
	ctx := context.Background()
	conn := appConn(t)
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT set_config('app.actor_type', $1, true),
		set_config('app.actor_id', $2, true), set_config('app.tenant_id', $3, true)`, actorType, actorID, tenant); err != nil {
		t.Fatal(err)
	}
	tag, err := tx.Exec(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), tx.Commit(ctx)
}

// guardEnv is a migrated database with node A in session 1 and the
// extra actors the guard tests need.
type guardEnv struct {
	*env
	epoch int64
}

func setupGuard(t *testing.T) *guardEnv {
	e := setup(t)
	addNodeA2(t, e)
	mustExec(t, e.owner, `INSERT INTO waystone.memberships (tenant_id, actor_type, actor_id, role) VALUES
		($1, 'local_user', $3, 'node'), ($2, 'local_user', $3, 'admin'), ($2, 'node', $4, 'admin')`,
		tenantA, tenantB, forgedUser, nodeA)
	// Node A now has two memberships, so InTenantTx would refuse it
	// (I6). These tests set node A's session settings by hand instead,
	// and give it session 1 directly.
	g := &guardEnv{env: e}
	mustExec(t, e.owner, `UPDATE waystone.nodes SET session_epoch = 1 WHERE node_id = $1`, nodeA)
	g.epoch = 1
	return g
}

// pending makes a fresh pending operation on server, retiring whatever
// the server had before.
func (g *guardEnv) pending(t *testing.T, server string, action Action) string {
	t.Helper()
	mustExec(t, g.owner, `UPDATE waystone.operations SET state = 'failed'
		WHERE resource_id = $1 AND state IN ('pending', 'running')`, server)
	mustExec(t, g.owner, `UPDATE waystone.operations SET resolved_at = now(), resolved_outcome = 'failed'
		WHERE resource_id = $1 AND state = 'uncertain' AND resolved_at IS NULL`, server)
	return g.mustCreate(t, server, action)
}

// running makes a fresh operation claimed under epoch (as the owner, so
// any epoch is possible).
func (g *guardEnv) running(t *testing.T, server string, action Action, epoch int64) string {
	t.Helper()
	id := g.pending(t, server, action)
	phase := any(nil)
	if action == ActionRestart {
		phase = "stopping"
	}
	mustExec(t, g.owner, `UPDATE waystone.operations SET state = 'running', claimed_epoch = $2, started_at = now(), phase = $3
		WHERE operation_id = $1`, id, epoch, phase)
	return id
}

func (g *guardEnv) uncertain(t *testing.T, server string) string {
	t.Helper()
	id := g.running(t, server, ActionStart, g.epoch)
	mustExec(t, g.owner, `UPDATE waystone.operations SET state = 'uncertain', finished_at = now(),
		error_code = 'no_answer', error_summary = 'x' WHERE operation_id = $1`, id)
	return id
}

type guardCase struct {
	name    string
	actor   [2]string // type, id
	tenant  string
	setup   func(t *testing.T, g *guardEnv) string // returns the operation ID
	sql     string                                 // $1 is the operation, $2 the epoch
	allowed bool
}

var (
	asNodeA  = [2]string{ActorNode, nodeA}
	asNodeA2 = [2]string{ActorNode, nodeA2}
	asAdmA   = [2]string{ActorLocalUser, "1000"}
	asAdmB   = [2]string{ActorLocalUser, "2000"}
	asForged = [2]string{ActorLocalUser, forgedUser}
	noActor  = [2]string{"", ""}
)

func pendingStart(t *testing.T, g *guardEnv) string { return g.pending(t, serverA1, ActionStart) }
func pendingRestart(t *testing.T, g *guardEnv) string {
	return g.pending(t, serverA1, ActionRestart)
}
func runningStart(t *testing.T, g *guardEnv) string {
	return g.running(t, serverA1, ActionStart, g.epoch)
}
func runningRestart(t *testing.T, g *guardEnv) string {
	return g.running(t, serverA1, ActionRestart, g.epoch)
}
func runningOld(t *testing.T, g *guardEnv) string {
	return g.running(t, serverA1, ActionRestart, g.epoch-1)
}
func runningUnclaimed(t *testing.T, g *guardEnv) string {
	id := g.pending(t, serverA1, ActionStart)
	mustExec(t, g.owner, `UPDATE waystone.operations SET state = 'running' WHERE operation_id = $1`, id)
	return id
}
func uncertainA(t *testing.T, g *guardEnv) string { return g.uncertain(t, serverA1) }

// runningStartStopping is a start that the owner put in phase stopping,
// a state no node could have made.
func runningStartStopping(t *testing.T, g *guardEnv) string {
	id := runningStart(t, g)
	mustExec(t, g.owner, `UPDATE waystone.operations SET phase = 'stopping' WHERE operation_id = $1`, id)
	return id
}

const (
	sqlClaim       = `UPDATE waystone.operations SET state = 'running', claimed_epoch = $2, started_at = now() WHERE operation_id = $1`
	sqlClaimPhase  = `UPDATE waystone.operations SET state = 'running', claimed_epoch = $2, started_at = now(), phase = 'stopping' WHERE operation_id = $1`
	sqlRefuse      = `UPDATE waystone.operations SET state = 'failed', finished_at = now(), error_code = 'not_found', error_summary = 'x' WHERE operation_id = $1 AND $2::bigint IS NOT NULL`
	sqlSucceed     = `UPDATE waystone.operations SET state = 'succeeded', finished_at = now() WHERE operation_id = $1 AND $2::bigint IS NOT NULL`
	sqlUncertain   = `UPDATE waystone.operations SET state = 'uncertain', finished_at = now(), error_code = 'session_replaced', error_summary = 'x' WHERE operation_id = $1 AND $2::bigint IS NOT NULL`
	sqlProgress    = `UPDATE waystone.operations SET phase = 'starting' WHERE operation_id = $1 AND $2::bigint IS NOT NULL`
	sqlResolveAdmA = `UPDATE waystone.operations SET resolved_at = now(), resolved_outcome = 'failed', resolved_by_type = 'local_user',
		resolved_by_id = '1000', resolved_by_name = 'admin a', resolution_note = 'checked' WHERE operation_id = $1 AND $2::bigint IS NOT NULL`
)

func TestOperationGuardByActor(t *testing.T) {
	g := setupGuard(t)
	e1 := `$2::bigint IS NOT NULL` // keeps $2 in use where the epoch doesn't matter

	cases := []guardCase{
		// The node, each allowed change (the S4 table).
		{"node claims", asNodeA, tenantA, pendingStart, sqlClaim, true},
		{"node claims a restart in phase stopping", asNodeA, tenantA, pendingRestart, sqlClaimPhase, true},
		{"node refuses an unsendable op (F10)", asNodeA, tenantA, pendingStart, sqlRefuse, true},
		{"node completes", asNodeA, tenantA, runningStart, sqlSucceed, true},
		{"node completes with a result phase", asNodeA, tenantA, runningRestart,
			`UPDATE waystone.operations SET state = 'failed', finished_at = now(), error_code = 'x', error_summary = 'y', phase = 'starting' WHERE operation_id = $1 AND ` + e1, true},
		{"node marks its op uncertain", asNodeA, tenantA, runningStart, sqlUncertain, true},
		{"node reports restart progress", asNodeA, tenantA, runningRestart, sqlProgress, true},
		{"hello marks an older session's op uncertain (I8)", asNodeA, tenantA, runningOld, sqlUncertain, true},
		{"hello marks an unclaimed running op uncertain", asNodeA, tenantA, runningUnclaimed, sqlUncertain, true},

		// The node, refused.
		{"claim with an old epoch", asNodeA, tenantA, pendingStart,
			`UPDATE waystone.operations SET state = 'running', claimed_epoch = $2 - 1, started_at = now() WHERE operation_id = $1`, false},
		{"claim with a future epoch", asNodeA, tenantA, pendingStart,
			`UPDATE waystone.operations SET state = 'running', claimed_epoch = $2 + 1, started_at = now() WHERE operation_id = $1`, false},
		{"claim without started_at", asNodeA, tenantA, pendingStart,
			`UPDATE waystone.operations SET state = 'running', claimed_epoch = $2 WHERE operation_id = $1`, false},
		{"claim a restart without phase", asNodeA, tenantA, pendingRestart, sqlClaim, false},
		{"claim a start with a phase", asNodeA, tenantA, pendingStart, sqlClaimPhase, false},
		{"claim that sets an error", asNodeA, tenantA, pendingStart,
			`UPDATE waystone.operations SET state = 'running', claimed_epoch = $2, started_at = now(), error_code = 'x' WHERE operation_id = $1`, false},
		{"refusal that claims too", asNodeA, tenantA, pendingStart,
			`UPDATE waystone.operations SET state = 'failed', finished_at = now(), error_code = 'x', error_summary = 'y', claimed_epoch = $2 WHERE operation_id = $1`, false},
		{"refusal without a code", asNodeA, tenantA, pendingStart,
			`UPDATE waystone.operations SET state = 'failed', finished_at = now() WHERE operation_id = $1 AND ` + e1, false},
		{"started_at after the claim", asNodeA, tenantA, runningStart,
			`UPDATE waystone.operations SET started_at = now() - interval '1 hour' WHERE operation_id = $1 AND ` + e1, false},
		{"claimed_epoch after the claim", asNodeA, tenantA, runningStart,
			`UPDATE waystone.operations SET claimed_epoch = $2 + 1 WHERE operation_id = $1`, false},
		{"result that rewrites claimed_epoch", asNodeA, tenantA, runningStart,
			`UPDATE waystone.operations SET state = 'succeeded', finished_at = now(), claimed_epoch = $2 + 1 WHERE operation_id = $1`, false},
		{"result without finished_at", asNodeA, tenantA, runningStart,
			`UPDATE waystone.operations SET state = 'succeeded' WHERE operation_id = $1 AND ` + e1, false},
		{"stale session completes", asNodeA, tenantA, runningOld, sqlSucceed, false},
		{"stale session fails it", asNodeA, tenantA, runningOld,
			`UPDATE waystone.operations SET state = 'failed', finished_at = now(), error_code = 'x', error_summary = 'y' WHERE operation_id = $1 AND ` + e1, false},
		{"I8 that changes the phase", asNodeA, tenantA, runningOld,
			`UPDATE waystone.operations SET state = 'uncertain', finished_at = now(), error_code = 'x', error_summary = 'y', phase = 'starting' WHERE operation_id = $1 AND ` + e1, false},
		{"progress on a start", asNodeA, tenantA, runningStart, sqlProgress, false},
		{"progress on a start in phase stopping", asNodeA, tenantA, runningStartStopping, sqlProgress, false},
		{"progress from a stale session", asNodeA, tenantA, runningOld, sqlProgress, false},
		{"progress backwards", asNodeA, tenantA, runningRestart,
			`UPDATE waystone.operations SET phase = NULL WHERE operation_id = $1 AND ` + e1, false},
		{"progress with an error", asNodeA, tenantA, runningRestart,
			`UPDATE waystone.operations SET phase = 'starting', error_code = 'x' WHERE operation_id = $1 AND ` + e1, false},
		{"no-op update", asNodeA, tenantA, runningStart,
			`UPDATE waystone.operations SET state = state WHERE operation_id = $1 AND ` + e1, false},
		{"no-op update on a pending op", asNodeA, tenantA, pendingStart,
			`UPDATE waystone.operations SET state = state WHERE operation_id = $1 AND ` + e1, false},
		{"claim columns without the state", asNodeA, tenantA, pendingStart,
			`UPDATE waystone.operations SET claimed_epoch = $2, started_at = now() WHERE operation_id = $1`, false},
		{"node resolves", asNodeA, tenantA, uncertainA, sqlResolveAdmA, false},
		{"node changes the resolution note", asNodeA, tenantA, uncertainA,
			`UPDATE waystone.operations SET resolution_note = 'x' WHERE operation_id = $1 AND ` + e1, false},
		{"node notes a resolution with its progress", asNodeA, tenantA, runningRestart,
			`UPDATE waystone.operations SET phase = 'starting', resolution_note = 'x' WHERE operation_id = $1 AND ` + e1, false},
		{"node notes a resolution with its result", asNodeA, tenantA, runningStart,
			`UPDATE waystone.operations SET state = 'succeeded', finished_at = now(), resolution_note = 'x' WHERE operation_id = $1 AND ` + e1, false},
		{"node changes the error on an uncertain op", asNodeA, tenantA, uncertainA,
			`UPDATE waystone.operations SET error_code = 'x' WHERE operation_id = $1 AND ` + e1, false},

		// Another node in the same tenant.
		{"other node claims", asNodeA2, tenantA, pendingStart, sqlClaim, false},
		{"other node completes", asNodeA2, tenantA, runningStart, sqlSucceed, false},
		{"other node refuses", asNodeA2, tenantA, pendingStart, sqlRefuse, false},

		// No actor at all: RLS shows nothing.
		{"no actor", noActor, tenantA, runningStart, sqlSucceed, false},

		// An admin may only resolve.
		{"admin resolves", asAdmA, tenantA, uncertainA, sqlResolveAdmA, true},
		{"admin claims", asAdmA, tenantA, pendingStart, sqlClaim, false},
		{"admin completes", asAdmA, tenantA, runningStart, sqlSucceed, false},
		{"admin fails a pending op", asAdmA, tenantA, pendingStart, sqlRefuse, false},
		{"admin marks running uncertain", asAdmA, tenantA, runningStart, sqlUncertain, false},
		{"admin changes the phase", asAdmA, tenantA, runningRestart, sqlProgress, false},
		{"admin resolves in someone else's name", asAdmA, tenantA, uncertainA,
			`UPDATE waystone.operations SET resolved_at = now(), resolved_outcome = 'failed', resolved_by_type = 'local_user',
			resolved_by_id = '2000', resolved_by_name = 'admin b' WHERE operation_id = $1 AND ` + e1, false},
		{"admin resolves and rewrites the error", asAdmA, tenantA, uncertainA,
			`UPDATE waystone.operations SET resolved_at = now(), resolved_outcome = 'failed', resolved_by_type = 'local_user',
			resolved_by_id = '1000', resolved_by_name = 'a', error_code = 'fine' WHERE operation_id = $1 AND ` + e1, false},
		{"admin of another tenant resolves", asAdmB, tenantB, uncertainA,
			`UPDATE waystone.operations SET resolved_at = now(), resolved_outcome = 'failed', resolved_by_type = 'local_user',
			resolved_by_id = '2000', resolved_by_name = 'b' WHERE operation_id = $1 AND ` + e1, false},
		{"admin of another tenant pointing at this one", asAdmB, tenantA, uncertainA,
			`UPDATE waystone.operations SET resolved_at = now(), resolved_outcome = 'failed', resolved_by_type = 'local_user',
			resolved_by_id = '2000', resolved_by_name = 'b' WHERE operation_id = $1 AND ` + e1, false},
		{"member of A, admin only of B, resolves in A", asForged, tenantA, uncertainA,
			`UPDATE waystone.operations SET resolved_at = now(), resolved_outcome = 'failed', resolved_by_type = 'local_user',
			resolved_by_id = '` + forgedUser + `', resolved_by_name = 'f' WHERE operation_id = $1 AND ` + e1, false},
	}

	ctx := context.Background()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id := c.setup(t, g)
			var before, after string
			snapshot := func(dst *string) {
				if err := g.owner.QueryRow(ctx, `SELECT row_to_json(o)::text FROM waystone.operations o WHERE operation_id = $1`, id).Scan(dst); err != nil {
					t.Fatal(err)
				}
			}
			snapshot(&before)
			n, err := asApp(t, c.actor[0], c.actor[1], c.tenant, c.sql, id, g.epoch)
			snapshot(&after)
			switch {
			case c.allowed && (err != nil || n != 1):
				t.Errorf("refused (%d rows, %v), want allowed", n, err)
			case !c.allowed && err == nil && n != 0:
				t.Errorf("allowed, want refused")
			case !c.allowed && before != after:
				t.Errorf("row changed although refused")
			case !c.allowed && err != nil && sqlState(err) != "P0001":
				// Refused by something other than the guard (a CHECK, a
				// grant): the guard itself wasn't tested.
				t.Errorf("refused with %v, want the guard's RAISE (P0001)", err)
			}
		})
	}
}

// The node with a forged admin membership elsewhere still can't resolve:
// the trigger looks at what the actor is, then at the row's tenant.
func TestOperationGuardForgedNodeAdmin(t *testing.T) {
	g := setupGuard(t)
	id := g.uncertain(t, serverA1)
	if n, err := asApp(t, ActorNode, nodeA, tenantA, `UPDATE waystone.operations SET resolved_at = now(), resolved_outcome = 'failed',
		resolved_by_type = 'node', resolved_by_id = $2, resolved_by_name = 'n' WHERE operation_id = $1`, id, nodeA); err == nil && n != 0 {
		t.Error("a node with an admin membership in another tenant resolved an operation")
	}
	// Pointed at tenant B, the operation in A isn't even visible.
	if n, err := asApp(t, ActorNode, nodeA, tenantB, sqlResolveAdmA, id, 0); err != nil || n != 0 {
		t.Errorf("node in tenant B: %d rows, %v; want 0 rows", n, err)
	}
}

// The owner without the override: only running -> uncertain, the
// recovery function's one change. The override never lets anyone, the
// owner included, rewrite identity columns or history.
func TestOperationGuardOwner(t *testing.T) {
	g := setupGuard(t)
	ctx := context.Background()
	// A second owner connection, without the fixtures' override.
	strict, err := pgx.Connect(ctx, ownerDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer strict.Close(ctx)

	refused := func(name, q, id string) {
		t.Helper()
		if _, err := strict.Exec(ctx, q, id); err == nil {
			t.Errorf("%s: allowed, want refused", name)
		}
	}
	id := pendingStart(t, g)
	refused("owner claims", `UPDATE waystone.operations SET state = 'running', claimed_epoch = 1, started_at = now() WHERE operation_id = $1`, id)
	refused("owner fails a pending op", `UPDATE waystone.operations SET state = 'failed', finished_at = now() WHERE operation_id = $1`, id)

	id = runningRestart(t, g)
	refused("owner completes", `UPDATE waystone.operations SET state = 'succeeded', finished_at = now() WHERE operation_id = $1`, id)
	refused("owner changes the phase", `UPDATE waystone.operations SET phase = 'starting' WHERE operation_id = $1`, id)
	refused("owner marks uncertain and changes the phase",
		`UPDATE waystone.operations SET state = 'uncertain', phase = 'starting' WHERE operation_id = $1`, id)
	if _, err := strict.Exec(ctx, `UPDATE waystone.operations SET state = 'uncertain', finished_at = now(),
		error_code = 'x', error_summary = 'y' WHERE operation_id = $1`, id); err != nil {
		t.Errorf("owner running -> uncertain: %v, want allowed", err)
	}
	refused("owner resolves", `UPDATE waystone.operations SET resolved_at = now(), resolved_outcome = 'failed' WHERE operation_id = $1`, id)

	// With the override: identity columns and history still hold. A
	// pending row, so it's layer 2 that refuses, not the history rules.
	open := pendingStart(t, g)
	for _, q := range []string{
		`UPDATE waystone.operations SET actor_name = 'someone else' WHERE operation_id = $1`,
		`UPDATE waystone.operations SET requested_at = now() WHERE operation_id = $1`,
		`UPDATE waystone.operations SET node_id = '` + nodeA2 + `' WHERE operation_id = $1`,
	} {
		if _, err := g.owner.Exec(ctx, q, open); err == nil || !strings.Contains(err.Error(), "identity columns") {
			t.Errorf("owner with override: %s gave %v, want the identity refusal", q, err)
		}
	}
	if _, err := g.owner.Exec(ctx, `UPDATE waystone.operations SET resolved_at = now(), resolved_outcome = 'failed' WHERE operation_id = $1`, id); err == nil {
		t.Error("owner with override resolved an operation twice")
	}
	// The override is the owner's alone: waystone_app setting it changes
	// nothing.
	run := runningStart(t, g)
	app := appConn(t)
	tx, err := app.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT set_config('waystone.owner_override', 'on', true),
		set_config('app.actor_type', 'local_user', true), set_config('app.actor_id', '1000', true),
		set_config('app.tenant_id', $1, true)`, tenantA); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE waystone.operations SET state = 'succeeded', finished_at = now() WHERE operation_id = $1`, run); err == nil {
		t.Error("waystone_app with the override set completed an operation as an admin")
	}
}

// I7 through the real role: waystone_app calls the recovery function with
// no session settings at all, and it changes only running rows.
func TestRecoveryAsApp(t *testing.T) {
	g := setupGuard(t)
	ctx := context.Background()
	run := runningStart(t, g)
	pend := g.pending(t, serverA2, ActionStart)
	var n int
	if err := appConn(t).QueryRow(ctx, `SELECT waystone.recover_running_operations()`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("recover as app = %d, %v; want 1", n, err)
	}
	if s, c := g.opState(t, run); s != "uncertain" || c != "controller_restarted" {
		t.Errorf("running op = %s %s", s, c)
	}
	if s, _ := g.opState(t, pend); s != "pending" {
		t.Errorf("pending op = %s", s)
	}
}
