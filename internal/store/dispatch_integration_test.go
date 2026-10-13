// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Stage 4c-1 tests (notes/stage4c-scope.md v2.1): the claim, results,
// progress, startup recovery and migration 00004's per-actor guard, on
// real PostgreSQL with the real roles.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func (e *env) claim(actor Actor, epoch int64) (ClaimedOperation, bool, error) {
	var op ClaimedOperation
	var ok bool
	err := e.store.InTenantTx(context.Background(), actor, func(ctx context.Context, tx *Tx) error {
		var err error
		op, ok, err = tx.ClaimNext(ctx, epoch)
		return err
	})
	return op, ok, err
}

func (e *env) complete(actor Actor, epoch int64, opID string, r OperationResult) error {
	return e.store.InTenantTx(context.Background(), actor, func(ctx context.Context, tx *Tx) error {
		return tx.CompleteOperation(ctx, epoch, opID, r)
	})
}

func (e *env) restartStarting(actor Actor, epoch int64, opID string) error {
	return e.store.InTenantTx(context.Background(), actor, func(ctx context.Context, tx *Tx) error {
		return tx.RestartStarting(ctx, epoch, opID)
	})
}

type opRow struct {
	state, phase, code, summary string
	claimedEpoch                *int64
	startedAt, finishedAt       *time.Time
}

func (e *env) op(t *testing.T, id string) opRow {
	t.Helper()
	var r opRow
	if err := e.owner.QueryRow(context.Background(), `
		SELECT state, coalesce(phase, ''), coalesce(error_code, ''), coalesce(error_summary, ''),
		       claimed_epoch, started_at, finished_at
		FROM waystone.operations WHERE operation_id = $1`, id).
		Scan(&r.state, &r.phase, &r.code, &r.summary, &r.claimedEpoch, &r.startedAt, &r.finishedAt); err != nil {
		t.Fatal(err)
	}
	return r
}

func (e *env) mustCreate(t *testing.T, server string, action Action) string {
	t.Helper()
	op, _, err := e.create(adminA, CreateOperationInput{ServerID: server, Action: action})
	if err != nil {
		t.Fatal(err)
	}
	return op.ID
}

func (e *env) mustHello(t *testing.T, actor Actor) int64 {
	t.Helper()
	epoch, _, err := e.hello(actor)
	if err != nil {
		t.Fatal(err)
	}
	return epoch
}

var succeeded = OperationResult{State: StateSucceeded}

// ----------------------------------------------------------------- claim

// I9, F10: the oldest pending operation goes first, a restart starts in
// phase stopping, and a deleted or moved server's operation fails without
// being handed out.
func TestClaimNext(t *testing.T) {
	e := setup(t)
	addNodeA2(t, e)
	epoch := e.mustHello(t, nodeActA)

	if _, ok, err := e.claim(nodeActA, epoch); ok || err != nil {
		t.Fatalf("claim with nothing pending = %v, %v", ok, err)
	}

	first := e.mustCreate(t, serverA1, ActionRestart)
	e.clock.Advance(time.Second)
	second := e.mustCreate(t, serverA2, ActionStart)

	got, ok, err := e.claim(nodeActA, epoch)
	if err != nil || !ok {
		t.Fatalf("claim = %v, %v", ok, err)
	}
	want := ClaimedOperation{OperationID: first, ServerID: serverA1, Action: ActionRestart, Epoch: epoch}
	if got != want {
		t.Errorf("claimed %+v, want %+v", got, want)
	}
	r := e.op(t, first)
	if r.state != "running" || r.phase != "stopping" || r.claimedEpoch == nil || *r.claimedEpoch != epoch ||
		r.startedAt == nil || !r.startedAt.Equal(e.clock.now) || r.finishedAt != nil {
		t.Errorf("claimed restart row = %+v", r)
	}

	got, ok, err = e.claim(nodeActA, epoch)
	if err != nil || !ok || got.OperationID != second || got.Action != ActionStart {
		t.Fatalf("second claim = %+v %v %v", got, ok, err)
	}
	if r := e.op(t, second); r.phase != "" {
		t.Errorf("a start has phase %q", r.phase)
	}
	if _, ok, _ := e.claim(nodeActA, epoch); ok {
		t.Error("claimed a third operation from two")
	}

	// Another node never gets node A's operations.
	third := e.mustCreate(t, serverOnA2, ActionStop)
	if _, ok, err := e.claim(nodeActA, epoch); ok || err != nil {
		t.Errorf("node A claimed node A2's operation: %v %v", ok, err)
	}
	if s, _ := e.opState(t, third); s != "pending" {
		t.Errorf("A2's op = %s", s)
	}

	// Only a node may claim.
	if _, _, err := e.claim(adminA, epoch); !errors.Is(err, ErrNotNode) {
		t.Errorf("claim as admin: %v, want ErrNotNode", err)
	}
}

// F10: the server is gone, or on another node, by the time it's claimed.
// The operation fails with a fixed code and the claim moves on.
func TestClaimRefusesGoneOrMovedServers(t *testing.T) {
	e := setup(t)
	addNodeA2(t, e)
	epoch := e.mustHello(t, nodeActA)

	gone := e.mustCreate(t, serverA1, ActionStart)
	e.clock.Advance(time.Second)
	moved := e.mustCreate(t, serverA2, ActionStop)
	e.clock.Advance(time.Second)
	mustExec(t, e.owner, `INSERT INTO waystone.servers (tenant_id, server_id, node_id, name, profile, runtime)
		VALUES ($1, '0199e1a0-0000-7000-8000-000000000a03', $2, 'a3', 'minecraft-java', 'systemd')`, tenantA, nodeA)
	fine := e.mustCreate(t, "0199e1a0-0000-7000-8000-000000000a03", ActionStart)

	mustExec(t, e.owner, `DELETE FROM waystone.servers WHERE server_id = $1`, serverA1)
	mustExec(t, e.owner, `UPDATE waystone.servers SET node_id = $1 WHERE server_id = $2`, nodeA2, serverA2)

	got, ok, err := e.claim(nodeActA, epoch)
	if err != nil || !ok || got.OperationID != fine {
		t.Fatalf("claim = %+v %v %v, want the a3 operation", got, ok, err)
	}
	for id, code := range map[string]string{gone: CodeServerNotFound, moved: CodeServerReassigned} {
		r := e.op(t, id)
		if r.state != "failed" || r.code != code || r.summary == "" || r.finishedAt == nil ||
			r.claimedEpoch != nil || r.startedAt != nil {
			t.Errorf("%s: %+v, want failed, never claimed", code, r)
		}
	}
}

// C3 for the claim: an old session can't claim.
func TestClaimNeedsCurrentEpoch(t *testing.T) {
	e := setup(t)
	old := e.mustHello(t, nodeActA)
	current := e.mustHello(t, nodeActA)
	id := e.mustCreate(t, serverA1, ActionStart)

	if _, _, err := e.claim(nodeActA, old); !errors.Is(err, ErrSessionSuperseded) {
		t.Fatalf("claim with the old epoch: %v, want ErrSessionSuperseded", err)
	}
	if s, _ := e.opState(t, id); s != "pending" {
		t.Fatalf("op = %s after a refused claim", s)
	}
	if _, ok, err := e.claim(nodeActA, current); !ok || err != nil {
		t.Fatalf("claim with the current epoch: %v %v", ok, err)
	}
}

// I9, forced interleaving: a hello holds the node row while the old
// session tries to claim. The old claim waits, then sees the new epoch and
// commits nothing; the new session claims the operation exactly once.
func TestClaimRacesTakeover(t *testing.T) {
	e := setup(t)
	old := e.mustHello(t, nodeActA)
	id := e.mustCreate(t, serverA1, ActionStart)

	inHello := make(chan struct{})
	finishHello := make(chan struct{})
	helloDone := make(chan error, 1)
	var current int64
	go func() {
		helloDone <- e.store.InTenantTx(context.Background(), nodeActA, func(ctx context.Context, tx *Tx) error {
			var err error
			current, _, err = tx.NodeHello(ctx)
			close(inHello)
			<-finishHello
			return err
		})
	}()
	<-inHello

	claimDone := make(chan error, 1)
	go func() {
		_, _, err := e.claim(nodeActA, old)
		claimDone <- err
	}()
	select {
	case err := <-claimDone:
		t.Fatalf("the old claim didn't wait for the hello: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(finishHello)
	if err := <-helloDone; err != nil {
		t.Fatal(err)
	}
	if err := <-claimDone; !errors.Is(err, ErrSessionSuperseded) {
		t.Fatalf("old claim after the takeover: %v, want ErrSessionSuperseded", err)
	}
	if s, _ := e.opState(t, id); s != "pending" {
		t.Fatalf("op = %s, want still pending", s)
	}

	got, ok, err := e.claim(nodeActA, current)
	if err != nil || !ok || got.OperationID != id || got.Epoch != current {
		t.Fatalf("new session's claim = %+v %v %v", got, ok, err)
	}
	if _, ok, _ := e.claim(nodeActA, current); ok {
		t.Error("the operation was claimed twice")
	}
}

// Concurrent claims by the same session (two transactions) hand out each
// operation once: SKIP LOCKED, never a double claim.
func TestConcurrentClaimsNeverDouble(t *testing.T) {
	e := setup(t)
	epoch := e.mustHello(t, nodeActA)
	ids := map[string]bool{}
	for _, s := range []string{serverA1, serverA2} {
		ids[e.mustCreate(t, s, ActionStart)] = true
	}

	var mu sync.Mutex
	got := map[string]int{}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			op, ok, err := e.claim(nodeActA, epoch)
			if err != nil {
				t.Error(err)
				return
			}
			if ok {
				mu.Lock()
				got[op.OperationID]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(got) != 2 {
		t.Errorf("claimed %v, want both operations", got)
	}
	for id, n := range got {
		if n != 1 || !ids[id] {
			t.Errorf("%s claimed %d times", id, n)
		}
	}
}

// I9: a claim skips a pending row someone else holds locked and takes the
// next one at once, rather than waiting behind the lock.
func TestClaimSkipsLockedRows(t *testing.T) {
	e := setup(t)
	epoch := e.mustHello(t, nodeActA)
	first := e.mustCreate(t, serverA1, ActionStart)
	e.clock.Advance(time.Second)
	second := e.mustCreate(t, serverA2, ActionStart)

	ctx := context.Background()
	holder, err := e.owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx)
	if _, err := holder.Exec(ctx, `SELECT 1 FROM waystone.operations WHERE operation_id = $1 FOR UPDATE`, first); err != nil {
		t.Fatal(err)
	}

	type claimed struct {
		op  ClaimedOperation
		ok  bool
		err error
	}
	done := make(chan claimed, 1)
	go func() {
		op, ok, err := e.claim(nodeActA, epoch)
		done <- claimed{op, ok, err}
	}()
	select {
	case c := <-done:
		if c.err != nil || !c.ok || c.op.OperationID != second {
			t.Errorf("claim = %+v, want %s", c, second)
		}
	case <-time.After(3 * time.Second):
		holder.Rollback(ctx)
		<-done
		t.Fatal("the claim waited behind the locked row")
	}
}

// S1: one active operation per server holds at the claim boundary too.
// Even the owner, with the per-actor rules off, can't make a second
// operation run on a server: the I3 index refuses it.
func TestOneActiveOperationPerServer(t *testing.T) {
	e := setup(t)
	id := e.runningOp(t, serverA1)
	_, err := e.owner.Exec(context.Background(), `
		INSERT INTO waystone.operations (tenant_id, operation_id, resource_type, resource_id, node_id,
			action, state, actor_type, actor_id, actor_name, requested_at)
		SELECT tenant_id, '0199e1a0-0000-7000-8000-0000000c0de1', resource_type, resource_id, node_id,
			action, 'running', actor_type, actor_id, actor_name, requested_at
		FROM waystone.operations WHERE operation_id = $1`, id)
	if sqlState(err) != "23505" || !strings.Contains(err.Error(), "operations_one_unfinished") {
		t.Fatalf("second running operation: %v, want the operations_one_unfinished unique violation", err)
	}
}

// ---------------------------------------------------------------- result

// C3, I10: a result lands only on a running operation of this node,
// claimed by this exact session, and only once.
func TestCompleteOperation(t *testing.T) {
	e := setup(t)
	addNodeA2(t, e)
	epoch := e.mustHello(t, nodeActA)
	e.mustHello(t, nodeActA2)
	id := e.mustCreate(t, serverA1, ActionStart)
	if _, ok, err := e.claim(nodeActA, epoch); !ok || err != nil {
		t.Fatal(ok, err)
	}

	// Malformed results never reach the database.
	for _, r := range []OperationResult{
		{State: StateSucceeded, Code: "x"},
		{State: StateFailed},
		{State: StateFailed, Code: "x"},
		{State: StateUncertain, Summary: "x"},
		{State: StatePending, Code: "x", Summary: "y"},
		{State: StateRunning},
		{State: StateFailed, Code: strings.Repeat("x", 65), Summary: "y"},
	} {
		if err := e.complete(nodeActA, epoch, id, r); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("result %+v: %v, want ErrInvalidInput", r, err)
		}
	}
	if err := e.complete(nodeActA, epoch, "nope", succeeded); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("bad ID: %v", err)
	}

	// Another node can't finish it, even inside the same tenant.
	a2epoch := e.node(t, nodeA2).epoch
	if err := e.complete(nodeActA2, a2epoch, id, succeeded); !errors.Is(err, ErrNotClaimed) {
		t.Errorf("node A2 completing A's op: %v, want ErrNotClaimed", err)
	}
	// Nor can an admin.
	if err := e.complete(adminA, epoch, id, succeeded); !errors.Is(err, ErrNotNode) {
		t.Errorf("admin completing: %v, want ErrNotNode", err)
	}

	e.clock.Advance(3 * time.Second)
	if err := e.complete(nodeActA, epoch, id, succeeded); err != nil {
		t.Fatal(err)
	}
	r := e.op(t, id)
	if r.state != "succeeded" || r.code != "" || r.summary != "" || r.finishedAt == nil || !r.finishedAt.Equal(e.clock.now) {
		t.Errorf("row = %+v", r)
	}
	// A replay changes nothing.
	if err := e.complete(nodeActA, epoch, id, OperationResult{State: StateFailed, Code: "x", Summary: "y"}); !errors.Is(err, ErrNotClaimed) {
		t.Errorf("replayed result: %v, want ErrNotClaimed", err)
	}
	if r := e.op(t, id); r.state != "succeeded" {
		t.Errorf("replay changed the row: %+v", r)
	}

	// Failed and uncertain keep their fixed code and summary.
	for _, want := range []OperationResult{
		{State: StateFailed, Code: "action_failed", Summary: "The action failed."},
		{State: StateUncertain, Code: "no_answer", Summary: "Nobody knows."},
	} {
		id := e.mustCreate(t, serverA2, ActionStop)
		if _, ok, err := e.claim(nodeActA, epoch); !ok || err != nil {
			t.Fatal(ok, err)
		}
		if err := e.complete(nodeActA, epoch, id, want); err != nil {
			t.Fatal(err)
		}
		if r := e.op(t, id); r.state != string(want.State) || r.code != want.Code || r.summary != want.Summary {
			t.Errorf("row = %+v, want %+v", r, want)
		}
		if want.State == StateUncertain {
			mustExec(t, e.owner, `UPDATE waystone.operations SET resolved_at = now(), resolved_outcome = 'failed' WHERE operation_id = $1`, id)
		}
	}

	// Running, but no session claimed it (claimed_epoch NULL): the
	// current session can't finish it either.
	unclaimed := e.runningOp(t, serverA2)
	if err := e.complete(nodeActA, epoch, unclaimed, succeeded); !errors.Is(err, ErrNotClaimed) {
		t.Errorf("unclaimed running op: %v, want ErrNotClaimed", err)
	}
	if r := e.op(t, unclaimed); r.state != "running" {
		t.Errorf("unclaimed op changed: %+v", r)
	}

	// A pending operation was never claimed, so it can't be finished.
	pending := e.mustCreate(t, serverA1, ActionStop)
	if err := e.complete(nodeActA, epoch, pending, succeeded); !errors.Is(err, ErrNotClaimed) {
		t.Errorf("pending op: %v, want ErrNotClaimed", err)
	}
}

// C3: after a takeover, the old session's result is refused and the
// operation is left as the hello made it.
func TestCompleteAfterTakeover(t *testing.T) {
	e := setup(t)
	old := e.mustHello(t, nodeActA)
	id := e.mustCreate(t, serverA1, ActionStart)
	if _, ok, err := e.claim(nodeActA, old); !ok || err != nil {
		t.Fatal(ok, err)
	}
	current := e.mustHello(t, nodeActA)

	if err := e.complete(nodeActA, old, id, succeeded); !errors.Is(err, ErrSessionSuperseded) {
		t.Errorf("old session's result: %v, want ErrSessionSuperseded", err)
	}
	// The new session never claimed it either.
	if err := e.complete(nodeActA, current, id, succeeded); !errors.Is(err, ErrNotClaimed) {
		t.Errorf("new session's result for the old claim: %v, want ErrNotClaimed", err)
	}
	if s, c := e.opState(t, id); s != "uncertain" || c != "session_replaced" {
		t.Errorf("op = %s %s, want uncertain session_replaced", s, c)
	}
}

// C3, racing: a result and a takeover on two connections, over and over.
// Each round ends as exactly one of succeeded (the result won) or
// uncertain (the hello won, and the result was refused).
func TestResultRacesTakeover(t *testing.T) {
	e := setup(t)
	epoch := e.mustHello(t, nodeActA)
	sawSucceeded, sawUncertain := false, false
	for i := range 20 {
		id := e.mustCreate(t, serverA1, ActionStart)
		if _, ok, err := e.claim(nodeActA, epoch); !ok || err != nil {
			t.Fatal(ok, err)
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		var resultErr, helloErr error
		var next int64
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			resultErr = e.complete(nodeActA, epoch, id, succeeded)
		}()
		go func() {
			defer wg.Done()
			<-start
			next, _, helloErr = e.hello(nodeActA)
		}()
		close(start)
		wg.Wait()
		if helloErr != nil {
			t.Fatalf("round %d: hello: %v", i, helloErr)
		}
		s, c := e.opState(t, id)
		switch {
		case resultErr == nil && s == "succeeded":
			sawSucceeded = true
		case errors.Is(resultErr, ErrSessionSuperseded) && s == "uncertain" && c == "session_replaced":
			sawUncertain = true
			mustExec(t, e.owner, `UPDATE waystone.operations SET resolved_at = now(), resolved_outcome = 'failed' WHERE operation_id = $1`, id)
		default:
			t.Fatalf("round %d: result %v, op %s %s: neither outcome", i, resultErr, s, c)
		}
		epoch = next
	}
	t.Logf("succeeded seen: %v, uncertain seen: %v", sawSucceeded, sawUncertain)
}

// --------------------------------------------------------------- progress

func TestRestartStarting(t *testing.T) {
	e := setup(t)
	epoch := e.mustHello(t, nodeActA)
	restart := e.mustCreate(t, serverA1, ActionRestart)
	start := e.mustCreate(t, serverA2, ActionStart)
	for range 2 {
		if _, ok, err := e.claim(nodeActA, epoch); !ok || err != nil {
			t.Fatal(ok, err)
		}
	}

	if err := e.restartStarting(nodeActA, epoch, start); !errors.Is(err, ErrNotClaimed) {
		t.Errorf("progress on a start: %v, want ErrNotClaimed", err)
	}
	if err := e.restartStarting(nodeActA, epoch+1, restart); !errors.Is(err, ErrSessionSuperseded) {
		t.Errorf("progress with a wrong epoch: %v, want ErrSessionSuperseded", err)
	}
	if err := e.restartStarting(nodeActA, epoch, restart); err != nil {
		t.Fatal(err)
	}
	if r := e.op(t, restart); r.phase != "starting" || r.state != "running" {
		t.Errorf("row = %+v", r)
	}
	if err := e.restartStarting(nodeActA, epoch, restart); !errors.Is(err, ErrNotClaimed) {
		t.Errorf("second progress: %v, want ErrNotClaimed", err)
	}
	// A result keeps the phase, so a failed start reads "stopped, then
	// didn't start".
	if err := e.complete(nodeActA, epoch, restart, OperationResult{State: StateFailed, Code: "action_failed", Summary: "x"}); err != nil {
		t.Fatal(err)
	}
	if r := e.op(t, restart); r.phase != "starting" || r.state != "failed" {
		t.Errorf("row = %+v", r)
	}
}

// --------------------------------------------------------------- recovery

// I7, F2: every running operation, in every tenant, becomes uncertain
// with controller_restarted. Nothing else changes.
func TestRecoverRunningOperations(t *testing.T) {
	e := setup(t)
	runA := e.runningOp(t, serverA1)
	pendingA := e.mustCreate(t, serverA2, ActionStart)
	opB, _, err := e.create(adminB, CreateOperationInput{ServerID: serverB1, Action: ActionStop})
	if err != nil {
		t.Fatal(err)
	}
	e.setState(t, opB.ID, StateRunning)

	n, err := e.store.RecoverRunningOperations(context.Background())
	if err != nil || n != 2 {
		t.Fatalf("recover = %d, %v; want 2", n, err)
	}
	for _, id := range []string{runA, opB.ID} {
		r := e.op(t, id)
		if r.state != "uncertain" || r.code != "controller_restarted" || r.summary == "" || r.finishedAt == nil {
			t.Errorf("%s: %+v", id, r)
		}
	}
	if s, _ := e.opState(t, pendingA); s != "pending" {
		t.Errorf("pending op = %s", s)
	}
	if n, err := e.store.RecoverRunningOperations(context.Background()); n != 0 || err != nil {
		t.Errorf("second recover = %d, %v", n, err)
	}
}

// I7: the function is pinned. Its body, owner, SECURITY DEFINER, its
// search_path and its EXECUTE grants are exactly as reviewed.
func TestRecoveryFunctionPinned(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	var owner, src string
	var secdef bool
	var nargs int
	var config []string
	if err := e.owner.QueryRow(ctx, `
		SELECT pg_get_userbyid(p.proowner), p.prosecdef, p.pronargs, coalesce(p.proconfig, '{}'), p.prosrc
		FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname = 'waystone' AND p.proname = 'recover_running_operations'`).
		Scan(&owner, &secdef, &nargs, &config, &src); err != nil {
		t.Fatal(err)
	}
	if owner != "waystone_owner" || !secdef || nargs != 0 ||
		strings.Join(config, ";") != "search_path=pg_catalog, waystone" {
		t.Errorf("owner %s, security definer %v, %d args, config %v", owner, secdef, nargs, config)
	}
	const wantBody = `DECLARE n integer; BEGIN UPDATE waystone.operations SET state = 'uncertain', finished_at = now(), ` +
		`error_code = 'controller_restarted', error_summary = 'The controller restarted while this operation was running.' ` +
		`WHERE state = 'running'; GET DIAGNOSTICS n = ROW_COUNT; RETURN n; END;`
	if got := strings.Join(strings.Fields(src), " "); got != wantBody {
		t.Errorf("function body changed:\n got %s\nwant %s", got, wantBody)
	}

	rows, err := e.owner.Query(ctx, `
		SELECT CASE WHEN a.grantee = 0 THEN 'PUBLIC' ELSE pg_get_userbyid(a.grantee) END, a.privilege_type
		FROM pg_proc p, aclexplode(p.proacl) a
		WHERE p.oid = 'waystone.recover_running_operations()'::regprocedure
		ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (string, error) {
		var who, priv string
		err := r.Scan(&who, &priv)
		return who + ":" + priv, err
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "waystone_app:EXECUTE,waystone_owner:EXECUTE" {
		t.Errorf("grants = %v, want EXECUTE for waystone_app and its owner only", got)
	}
}
