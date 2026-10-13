// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sharddevs/waystone_panel/internal/ids"
)

func sum(s string) [32]byte { return sha256.Sum256([]byte(s)) }

// I4: a create records every snapshot and sets the desired state.
func TestCreateOperation(t *testing.T) {
	e := setup(t)
	op, replayed, err := e.create(adminA, CreateOperationInput{ServerID: serverA1, Action: ActionStart})
	if err != nil || replayed {
		t.Fatalf("create: replayed=%v err=%v", replayed, err)
	}
	if ids.Check(op.ID) != nil {
		t.Errorf("ID %q is not a canonical UUID", op.ID)
	}
	want := Operation{
		ID: op.ID, TenantID: tenantA, ResourceType: "server", ResourceID: serverA1, NodeID: nodeA,
		Action: ActionStart, State: StatePending, Actor: adminA, RequestedAt: e.clock.now,
	}
	if op.StartedAt != nil || op.FinishedAt != nil || op.Resolution != nil {
		t.Errorf("new operation has started/finished/resolution set: %+v", op)
	}
	op.StartedAt, op.FinishedAt, op.Resolution = nil, nil, nil
	if !op.RequestedAt.Equal(want.RequestedAt) {
		t.Errorf("RequestedAt = %v, want %v", op.RequestedAt, want.RequestedAt)
	}
	op.RequestedAt = want.RequestedAt
	if op != want {
		t.Errorf("operation = %+v\nwant        %+v", op, want)
	}

	var desired string
	if err := e.owner.QueryRow(context.Background(),
		`SELECT desired_state FROM waystone.servers WHERE server_id = $1`, serverA1).Scan(&desired); err != nil {
		t.Fatal(err)
	}
	if desired != "running" {
		t.Errorf("desired_state = %q after start, want running", desired)
	}

	// The same operation comes back through GetOperation.
	var got Operation
	err = e.store.InTenantTx(context.Background(), adminA, func(ctx context.Context, tx *Tx) error {
		var err error
		got, err = tx.GetOperation(ctx, op.ID)
		return err
	})
	if err != nil || got.ID != op.ID || got.State != StatePending || got.Actor != adminA {
		t.Fatalf("GetOperation = %+v, %v", got, err)
	}
}

func TestStopSetsDesiredStopped(t *testing.T) {
	e := setup(t)
	if _, _, err := e.create(adminA, CreateOperationInput{ServerID: serverA2, Action: ActionStop}); err != nil {
		t.Fatal(err)
	}
	var desired string
	if err := e.owner.QueryRow(context.Background(),
		`SELECT desired_state FROM waystone.servers WHERE server_id = $1`, serverA2).Scan(&desired); err != nil {
		t.Fatal(err)
	}
	if desired != "stopped" {
		t.Errorf("desired_state = %q after stop, want stopped", desired)
	}
}

func TestCreateOperationRefusals(t *testing.T) {
	e := setup(t)
	cases := []struct {
		name  string
		actor Actor
		in    CreateOperationInput
		want  error
	}{
		{"other tenant's server", adminA, CreateOperationInput{ServerID: serverB1, Action: ActionStart}, ErrNotFound},
		{"missing server", adminA, CreateOperationInput{ServerID: missingServer, Action: ActionStart}, ErrNotFound},
		{"bad server id", adminA, CreateOperationInput{ServerID: "../etc", Action: ActionStart}, ErrInvalidInput},
		{"uppercase id", adminA, CreateOperationInput{ServerID: strings.ToUpper(serverA1), Action: ActionStart}, ErrInvalidInput},
		{"unknown action", adminA, CreateOperationInput{ServerID: serverA1, Action: "server.delete"}, ErrInvalidInput},
		{"bad key", adminA, CreateOperationInput{ServerID: serverA1, Action: ActionStart, IdempotencyKey: "has space", RequestSHA256: sum("x")}, ErrInvalidInput},
		{"key without fingerprint", adminA, CreateOperationInput{ServerID: serverA1, Action: ActionStart, IdempotencyKey: "k"}, ErrInvalidInput},
	}
	for _, tc := range cases {
		if _, _, err := e.create(tc.actor, tc.in); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
	if n := count(t, e.owner, "operations"); n != 0 {
		t.Fatalf("refused requests left %d operations, want 0", n)
	}
	// Admin of B can act on B's server.
	if _, _, err := e.create(adminB, CreateOperationInput{ServerID: serverB1, Action: ActionStart}); err != nil {
		t.Fatalf("admin B on B: %v", err)
	}
}

// I3 and F11: one unfinished operation per server.
func TestOneUnfinishedOperationPerServer(t *testing.T) {
	e := setup(t)
	first, _, err := e.create(adminA, CreateOperationInput{ServerID: serverA1, Action: ActionStart})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = e.create(adminA, CreateOperationInput{ServerID: serverA1, Action: ActionStop})
	var ipe *InProgressError
	if !errors.As(err, &ipe) || ipe.OperationID != first.ID || ipe.State != StatePending {
		t.Fatalf("second create: err = %v, want InProgressError for %s (pending)", err, first.ID)
	}
	// Another server isn't affected.
	if _, _, err := e.create(adminA, CreateOperationInput{ServerID: serverA2, Action: ActionStart}); err != nil {
		t.Fatalf("other server: %v", err)
	}
	// A running operation still blocks; a finished one doesn't.
	e.setState(t, first.ID, StateRunning)
	if _, _, err := e.create(adminA, CreateOperationInput{ServerID: serverA1, Action: ActionStop}); !errors.Is(err, ErrOperationInProgress) {
		t.Fatalf("while running: err = %v, want ErrOperationInProgress", err)
	}
	e.setState(t, first.ID, StateSucceeded)
	if _, _, err := e.create(adminA, CreateOperationInput{ServerID: serverA1, Action: ActionStop}); err != nil {
		t.Fatalf("after succeeded: %v", err)
	}
}

// F11 with real concurrency: many simultaneous creates, exactly one wins.
func TestConcurrentCreatesOneWins(t *testing.T) {
	e := setup(t)
	const n = 10
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			action := ActionStart
			if i%2 == 1 {
				action = ActionStop
			}
			_, _, errs[i] = e.create(adminA, CreateOperationInput{ServerID: serverA2, Action: action})
		}()
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrOperationInProgress):
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d creates succeeded, want exactly 1", ok)
	}
	if c := count(t, e.owner, "operations"); c != 1 {
		t.Fatalf("operations = %d, want 1", c)
	}
}

// The unique index enforces I3 by itself, even for a writer that skips
// the application's check (here the owner, inserting directly).
func TestUniqueIndexIsTheBackstop(t *testing.T) {
	e := setup(t)
	first, _, err := e.create(adminA, CreateOperationInput{ServerID: serverA1, Action: ActionStart})
	if err != nil {
		t.Fatal(err)
	}
	insert := `INSERT INTO waystone.operations (tenant_id, operation_id, resource_type, resource_id, node_id,
		action, state, actor_type, actor_id, actor_name, requested_at)
		VALUES ($1, $2, 'server', $3, $4, 'server.stop', 'pending', 'local_user', '1000', 'x', now())`
	_, err = e.owner.Exec(context.Background(), insert, tenantA, "0199e1a0-0000-7000-8000-00000000c001", serverA1, nodeA)
	if sqlState(err) != "23505" {
		t.Fatalf("second pending insert: err = %v, want SQLSTATE 23505", err)
	}
	// Unresolved uncertain blocks too.
	e.setState(t, first.ID, StateRunning, StateUncertain)
	_, err = e.owner.Exec(context.Background(), insert, tenantA, "0199e1a0-0000-7000-8000-00000000c002", serverA1, nodeA)
	if sqlState(err) != "23505" {
		t.Fatalf("insert next to unresolved uncertain: err = %v, want SQLSTATE 23505", err)
	}
}

// F13 and decision O: an unresolved uncertain operation blocks the server
// until someone resolves it, and resolving keeps the state honest.
func TestUncertainBlocksUntilResolved(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	first, _, err := e.create(adminA, CreateOperationInput{ServerID: serverA1, Action: ActionRestart})
	if err != nil {
		t.Fatal(err)
	}
	resolve := func(id string, outcome OperationState, note string) (Operation, error) {
		var op Operation
		err := e.store.InTenantTx(ctx, adminA, func(ctx context.Context, tx *Tx) error {
			var err error
			op, err = tx.ResolveOperation(ctx, id, outcome, note)
			return err
		})
		return op, err
	}

	if _, err := resolve(first.ID, StateFailed, ""); !errors.Is(err, ErrNotUncertain) {
		t.Fatalf("resolve pending: err = %v, want ErrNotUncertain", err)
	}
	e.setState(t, first.ID, StateRunning, StateUncertain)

	_, _, err = e.create(adminA, CreateOperationInput{ServerID: serverA1, Action: ActionStart})
	var ipe *InProgressError
	if !errors.As(err, &ipe) || ipe.State != StateUncertain || ipe.OperationID != first.ID {
		t.Fatalf("create while uncertain: err = %v, want InProgressError (uncertain, %s)", err, first.ID)
	}

	for _, bad := range []struct {
		id      string
		outcome OperationState
		note    string
	}{
		{first.ID, StatePending, ""},
		{first.ID, StateUncertain, ""},
		{first.ID, StateFailed, strings.Repeat("n", 501)},
		{first.ID, StateFailed, "\xff"},
		{"not-a-uuid", StateFailed, ""},
	} {
		if _, err := resolve(bad.id, bad.outcome, bad.note); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("resolve(%q, %q, len %d): err = %v, want ErrInvalidInput", bad.id, bad.outcome, len(bad.note), err)
		}
	}
	if _, err := resolve(missingServer, StateFailed, ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("resolve missing: err = %v, want ErrNotFound", err)
	}

	e.clock.Advance(time.Hour)
	op, err := resolve(first.ID, StateFailed, "checked by hand: server was stopped")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if op.State != StateUncertain || op.Resolution == nil {
		t.Fatalf("after resolve: state %s, resolution %+v; want uncertain with a resolution", op.State, op.Resolution)
	}
	r := *op.Resolution
	if r.Outcome != StateFailed || r.By != adminA || !r.At.Equal(e.clock.now) || r.Note != "checked by hand: server was stopped" {
		t.Errorf("resolution = %+v", r)
	}
	if _, err := resolve(first.ID, StateSucceeded, ""); !errors.Is(err, ErrAlreadyResolved) {
		t.Errorf("second resolve: err = %v, want ErrAlreadyResolved", err)
	}
	if _, _, err := e.create(adminA, CreateOperationInput{ServerID: serverA1, Action: ActionStart}); err != nil {
		t.Fatalf("create after resolve: %v", err)
	}

	// Another tenant can't see or resolve it.
	err = e.store.InTenantTx(ctx, adminB, func(ctx context.Context, tx *Tx) error {
		_, err := tx.GetOperation(ctx, first.ID)
		return err
	})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("admin B GetOperation: err = %v, want ErrNotFound", err)
	}
}

// C1: replay, mismatch, and a key on another server.
func TestIdempotencyReplayAndMismatch(t *testing.T) {
	e := setup(t)
	in := CreateOperationInput{ServerID: serverA1, Action: ActionStart, IdempotencyKey: "retry-1", RequestSHA256: sum("start a1")}
	first, replayed, err := e.create(adminA, in)
	if err != nil || replayed {
		t.Fatalf("first: replayed=%v err=%v", replayed, err)
	}
	again, replayed, err := e.create(adminA, in)
	if err != nil || !replayed || again.ID != first.ID {
		t.Fatalf("replay: id %s replayed=%v err=%v, want %s replayed", again.ID, replayed, err, first.ID)
	}
	// The replay returns the current state, whatever it is.
	e.setState(t, first.ID, StateRunning, StateFailed)
	again, replayed, err = e.create(adminA, in)
	if err != nil || !replayed || again.State != StateFailed {
		t.Fatalf("replay after failure: state %s replayed=%v err=%v", again.State, replayed, err)
	}

	changed := in
	changed.Action = ActionStop
	changed.RequestSHA256 = sum("stop a1")
	if _, _, err := e.create(adminA, changed); !errors.Is(err, ErrIdempotencyKeyReused) {
		t.Fatalf("same key, different request: err = %v, want ErrIdempotencyKeyReused", err)
	}
	other := CreateOperationInput{ServerID: serverA2, Action: ActionStart, IdempotencyKey: "retry-1", RequestSHA256: sum("start a2")}
	if _, _, err := e.create(adminA, other); !errors.Is(err, ErrIdempotencyKeyReused) {
		t.Fatalf("same key, other server: err = %v, want ErrIdempotencyKeyReused", err)
	}
	if c := count(t, e.owner, "operations"); c != 1 {
		t.Fatalf("operations = %d, want 1", c)
	}

	// Keys belong to one actor in one tenant: B's identical key is separate.
	inB := CreateOperationInput{ServerID: serverB1, Action: ActionStart, IdempotencyKey: "retry-1", RequestSHA256: sum("start b1")}
	if _, replayed, err := e.create(adminB, inB); err != nil || replayed {
		t.Fatalf("admin B with the same key string: replayed=%v err=%v", replayed, err)
	}
}

// C1 under concurrency: parallel retries with one key create exactly one
// operation and all return it.
func TestIdempotencyConcurrentRetries(t *testing.T) {
	e := setup(t)
	in := CreateOperationInput{ServerID: serverA2, Action: ActionStart, IdempotencyKey: "burst", RequestSHA256: sum("start a2")}
	const n = 10
	type result struct {
		id       string
		replayed bool
		err      error
	}
	results := make([]result, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			op, replayed, err := e.create(adminA, in)
			results[i] = result{op.ID, replayed, err}
		}()
	}
	wg.Wait()
	created := 0
	for _, r := range results {
		if r.err != nil {
			t.Fatalf("retry failed: %v", r.err)
		}
		if r.id != results[0].id {
			t.Fatalf("retries returned different operations: %s and %s", r.id, results[0].id)
		}
		if !r.replayed {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("%d calls report a new operation, want 1", created)
	}
	if c := count(t, e.owner, "operations"); c != 1 {
		t.Fatalf("operations = %d, want 1", c)
	}
}

// Parallel requests with one key for two different servers: at most one
// may create anything (the loser is rolled back entirely).
func TestIdempotencyKeyRaceAcrossServers(t *testing.T) {
	e := setup(t)
	ins := []CreateOperationInput{
		{ServerID: serverA1, Action: ActionStart, IdempotencyKey: "race", RequestSHA256: sum("a1")},
		{ServerID: serverA2, Action: ActionStart, IdempotencyKey: "race", RequestSHA256: sum("a2")},
	}
	for round := range 20 {
		mustExec(t, e.owner, `DELETE FROM waystone.idempotency_keys`)
		// Operations can't be deleted, so finish the pending ones from
		// the last round instead; then neither server is blocked (I3).
		rows, err := e.owner.Query(context.Background(), `SELECT operation_id::text FROM waystone.operations WHERE state = 'pending'`)
		if err != nil {
			t.Fatal(err)
		}
		var pending []string
		for rows.Next() {
			var id string
			_ = rows.Scan(&id)
			pending = append(pending, id)
		}
		rows.Close()
		for _, id := range pending {
			e.setState(t, id, StateRunning, StateSucceeded)
		}
		before := count(t, e.owner, "operations")

		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i := range ins {
			wg.Add(1)
			go func() { defer wg.Done(); _, _, errs[i] = e.create(adminA, ins[i]) }()
		}
		wg.Wait()
		won := 0
		for _, err := range errs {
			switch {
			case err == nil:
				won++
			case errors.Is(err, ErrIdempotencyKeyReused):
			default:
				t.Fatalf("round %d: unexpected error %v", round, err)
			}
		}
		if won != 1 {
			t.Fatalf("round %d: %d requests won, want exactly 1", round, won)
		}
		if got := count(t, e.owner, "operations") - before; got != 1 {
			t.Fatalf("round %d: %d new operations, want 1", round, got)
		}
	}
}

// C1 expiry: a key never expires while its operation is unfinished or
// unresolved; once finished and older than 24 h it may be reused.
func TestIdempotencyExpiryNeverDuplicatesUnresolved(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	in := CreateOperationInput{ServerID: serverA1, Action: ActionStart, IdempotencyKey: "day", RequestSHA256: sum("start a1")}
	first, _, err := e.create(adminA, in)
	if err != nil {
		t.Fatal(err)
	}
	e.setState(t, first.ID, StateRunning, StateUncertain)
	e.clock.Advance(25 * time.Hour)

	again, replayed, err := e.create(adminA, in)
	if err != nil || !replayed || again.ID != first.ID {
		t.Fatalf("after 25 h, unresolved: id %s replayed=%v err=%v, want original replayed", again.ID, replayed, err)
	}
	changed := in
	changed.RequestSHA256 = sum("something else")
	if _, _, err := e.create(adminA, changed); !errors.Is(err, ErrIdempotencyKeyReused) {
		t.Fatalf("after 25 h, unresolved, different request: err = %v, want ErrIdempotencyKeyReused", err)
	}

	// Parallel retries against the unresolved operation: still no duplicate.
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); _, _, _ = e.create(adminA, in) }()
	}
	wg.Wait()
	if c := count(t, e.owner, "operations"); c != 1 {
		t.Fatalf("operations = %d after retries on an unresolved key, want 1", c)
	}

	if err := e.store.InTenantTx(ctx, adminA, func(ctx context.Context, tx *Tx) error {
		_, err := tx.ResolveOperation(ctx, first.ID, StateFailed, "")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// Now it's resolved, and the key is 25 hours old, past the 24 hour
	// lifetime, so the key may be reused.
	fresh, replayed, err := e.create(adminA, in)
	if err != nil || replayed || fresh.ID == first.ID {
		t.Fatalf("after resolve: id %s replayed=%v err=%v, want a new operation", fresh.ID, replayed, err)
	}
	// And the key now points at the new operation.
	again, replayed, err = e.create(adminA, in)
	if err != nil || !replayed || again.ID != fresh.ID {
		t.Fatalf("replay of reused key: id %s replayed=%v err=%v, want %s", again.ID, replayed, err, fresh.ID)
	}
}

// A finished operation's key still replays inside the 24 h window.
func TestIdempotencyWithinWindowReplaysFinished(t *testing.T) {
	e := setup(t)
	in := CreateOperationInput{ServerID: serverA1, Action: ActionStart, IdempotencyKey: "window", RequestSHA256: sum("start a1")}
	first, _, err := e.create(adminA, in)
	if err != nil {
		t.Fatal(err)
	}
	e.setState(t, first.ID, StateRunning, StateSucceeded)
	e.clock.Advance(23 * time.Hour)
	again, replayed, err := e.create(adminA, in)
	if err != nil || !replayed || again.ID != first.ID {
		t.Fatalf("within 24 h: id %s replayed=%v err=%v, want original", again.ID, replayed, err)
	}
}

func TestRecordDenial(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	d := Denial{Action: "server.start", ResourceType: "server", ResourceID: serverA1, Reason: "forbidden", RequestID: "req-1"}
	if err := e.store.InTenantTx(ctx, adminA, func(ctx context.Context, tx *Tx) error {
		return tx.RecordDenial(ctx, d)
	}); err != nil {
		t.Fatal(err)
	}
	var tenant, actorID, action, outcome, reason, req string
	var at time.Time
	if err := e.owner.QueryRow(ctx, `SELECT tenant_id::text, actor_id, action, outcome, reason, request_id, at
		FROM waystone.audit_events`).Scan(&tenant, &actorID, &action, &outcome, &reason, &req, &at); err != nil {
		t.Fatal(err)
	}
	if tenant != tenantA || actorID != "1000" || action != "server.start" || outcome != "denied" ||
		reason != "forbidden" || req != "req-1" || !at.Equal(e.clock.now) {
		t.Errorf("audit row = %s %s %s %s %s %s %v", tenant, actorID, action, outcome, reason, req, at)
	}
	bad := d
	bad.Reason = ""
	if err := e.store.InTenantTx(ctx, adminA, func(ctx context.Context, tx *Tx) error {
		return tx.RecordDenial(ctx, bad)
	}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("empty reason: err = %v, want ErrInvalidInput", err)
	}
}
