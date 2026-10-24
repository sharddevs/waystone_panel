// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func listServers(t *testing.T, e *env, actor Actor) []Server {
	t.Helper()
	var out []Server
	err := e.store.InTenantTx(context.Background(), actor, func(ctx context.Context, tx *Tx) error {
		var err error
		out, err = tx.ListServers(ctx)
		return err
	})
	if err != nil {
		t.Fatalf("ListServers as %s: %v", actor.ID, err)
	}
	return out
}

func listOps(e *env, actor Actor, serverID string, limit int) ([]Operation, error) {
	var out []Operation
	err := e.store.InTenantTx(context.Background(), actor, func(ctx context.Context, tx *Tx) error {
		var err error
		out, err = tx.ListOperations(ctx, serverID, limit)
		return err
	})
	return out, err
}

// ListServers shows the tenant's servers only, in a stable order, with the
// state columns the API needs.
func TestListServers(t *testing.T) {
	e := setup(t)

	a := listServers(t, e, adminA)
	if len(a) != 2 || a[0].ID != serverA1 || a[1].ID != serverA2 {
		t.Fatalf("tenant A servers = %+v, want a1 then a2", a)
	}
	s := a[0]
	if s.TenantID != tenantA || s.NodeID != nodeA || s.Name != "a1" || s.Profile != "minecraft-java" {
		t.Errorf("a1 = %+v", s)
	}
	if s.DesiredState != "stopped" || s.ProcessState != "unknown" || s.Readiness != "unknown" {
		t.Errorf("a1 states = %q/%q/%q, want the defaults stopped/unknown/unknown", s.DesiredState, s.ProcessState, s.Readiness)
	}
	if s.ProcessSince != nil || s.ProcessObservedAt != nil || s.ReadinessSince != nil || s.ReadinessObservedAt != nil {
		t.Errorf("a1 timestamps should all be nil before any report: %+v", s)
	}

	// Timestamps come through when they're set.
	seen := time.Date(2026, 10, 12, 11, 0, 0, 0, time.UTC)
	mustExec(t, e.owner, `UPDATE waystone.servers SET process_state = 'running', process_since = $1,
		process_observed_at = $1, readiness = 'ready', readiness_since = $1, readiness_observed_at = $1
		WHERE server_id = $2`, seen, serverA2)
	a = listServers(t, e, adminA)
	got := a[1]
	if got.ProcessState != "running" || got.Readiness != "ready" ||
		got.ProcessSince == nil || !got.ProcessSince.Equal(seen) ||
		got.ReadinessObservedAt == nil || !got.ReadinessObservedAt.Equal(seen) {
		t.Errorf("a2 after report = %+v", got)
	}

	// The order is by name, then ID: rename a2 so it sorts first.
	mustExec(t, e.owner, `UPDATE waystone.servers SET name = 'a0' WHERE server_id = $1`, serverA2)
	a = listServers(t, e, adminA)
	if a[0].ID != serverA2 {
		t.Errorf("after rename, first = %s, want a2", a[0].ID)
	}

	b := listServers(t, e, adminB)
	if len(b) != 1 || b[0].ID != serverB1 {
		t.Errorf("tenant B servers = %+v, want only b1", b)
	}

	// A tenant with no servers gets an empty list, not nil.
	mustExec(t, e.owner, `DELETE FROM waystone.servers WHERE tenant_id = $1`, tenantB)
	b = listServers(t, e, adminB)
	if b == nil || len(b) != 0 {
		t.Errorf("empty tenant: got %#v, want an empty non-nil slice", b)
	}
}

// ListOperations: newest first, optional server filter, limit, and no
// rows from other tenants.
func TestListOperations(t *testing.T) {
	e := setup(t)

	// Three finished operations on a1. The clock moves on after the first
	// one only, so the last three operations below share one requested_at
	// and their IDs (UUIDv7, increasing) have to break the tie.
	var ids []string // oldest first
	for i := 0; i < 3; i++ {
		op, _, err := e.create(adminA, CreateOperationInput{ServerID: serverA1, Action: ActionStart})
		if err != nil {
			t.Fatal(err)
		}
		e.setState(t, op.ID, StateRunning, StateSucceeded)
		ids = append(ids, op.ID)
		if i == 0 {
			e.clock.Advance(time.Minute)
		}
	}
	other, _, err := e.create(adminA, CreateOperationInput{ServerID: serverA2, Action: ActionStop})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.create(adminB, CreateOperationInput{ServerID: serverB1, Action: ActionStart}); err != nil {
		t.Fatal(err)
	}

	all, err := listOps(e, adminA, "", 50)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{other.ID, ids[2], ids[1], ids[0]}
	if len(all) != len(want) {
		t.Fatalf("got %d operations, want %d (tenant B's must not show)", len(all), len(want))
	}
	for i := range want {
		if all[i].ID != want[i] {
			t.Errorf("position %d = %s, want %s (newest first)", i, all[i].ID, want[i])
		}
	}
	if all[1].State != StateSucceeded || all[1].Actor != adminA {
		t.Errorf("row not fully scanned: %+v", all[1])
	}

	a1, err := listOps(e, adminA, serverA1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(a1) != 2 || a1[0].ID != ids[2] || a1[1].ID != ids[1] {
		t.Errorf("server a1, limit 2 = %v, want the two newest a1 operations", opIDs(a1))
	}

	// Another tenant's server: no rows, no error.
	b, err := listOps(e, adminA, serverB1, 50)
	if err != nil || b == nil || len(b) != 0 {
		t.Errorf("other tenant's server: %v, %v; want an empty non-nil slice", b, err)
	}

	for name, tc := range map[string]struct {
		server string
		limit  int
	}{
		"limit 0":    {"", 0},
		"limit 101":  {"", MaxListLimit + 1},
		"negative":   {"", -1},
		"bad server": {"not-a-uuid", 10},
		"upper uuid": {"0199E1A0-0000-7000-8000-000000000A01", 10},
	} {
		if _, err := listOps(e, adminA, tc.server, tc.limit); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s: err = %v, want ErrInvalidInput", name, err)
		}
	}
	if _, err := listOps(e, adminA, "", MaxListLimit); err != nil {
		t.Errorf("limit = MaxListLimit: %v", err)
	}
}

func opIDs(ops []Operation) []string {
	out := make([]string, len(ops))
	for i, op := range ops {
		out[i] = op.ID
	}
	return out
}

func getServer(e *env, actor Actor, id string) (Server, error) {
	var out Server
	err := e.store.InTenantTx(context.Background(), actor, func(ctx context.Context, tx *Tx) error {
		var err error
		out, err = tx.GetServer(ctx, id)
		return err
	})
	return out, err
}

// The node, profile and runtime columns, the blocking operation, and
// GetServer.
func TestServerDetails(t *testing.T) {
	e := setup(t)

	s, err := getServer(e, adminA, serverA1)
	if err != nil {
		t.Fatal(err)
	}
	if s.ID != serverA1 || s.NodeName != "node a" || s.NodeLastSeenAt == nil || s.Runtime != "systemd" ||
		s.ProfileVersion != nil || s.Blocking != "" {
		t.Errorf("a1 = %+v", s)
	}
	if l := listServers(t, e, adminA); !reflect.DeepEqual(l[0], s) {
		t.Errorf("GetServer = %+v\nlist item = %+v", s, l[0])
	}

	mustExec(t, e.owner, `UPDATE waystone.servers SET config = '{"version": "26.3"}' WHERE server_id = $1`, serverA1)
	mustExec(t, e.owner, `UPDATE waystone.nodes SET last_seen_at = NULL WHERE node_id = $1`, nodeA)
	if s, _ := getServer(e, adminA, serverA1); s.ProfileVersion == nil || *s.ProfileVersion != "26.3" || s.NodeLastSeenAt != nil {
		t.Errorf("after update: %+v", s)
	}
	mustExec(t, e.owner, `UPDATE waystone.nodes SET last_seen_at = '2100-01-01' WHERE node_id = $1`, nodeA)

	// Blocking: pending, running, unresolved uncertain; nothing once it is
	// finished or resolved. Only this server's operations count.
	op, _, err := e.create(adminA, CreateOperationInput{ServerID: serverA1, Action: ActionStart})
	if err != nil {
		t.Fatal(err)
	}
	want := func(name string, state OperationState) {
		t.Helper()
		l := listServers(t, e, adminA)
		if l[0].Blocking != state || l[1].Blocking != "" {
			t.Errorf("%s: blocking = %q / %q, want %q / \"\"", name, l[0].Blocking, l[1].Blocking, state)
		}
	}
	want("pending", StatePending)
	e.setState(t, op.ID, StateRunning)
	want("running", StateRunning)
	e.setState(t, op.ID, StateUncertain)
	want("uncertain", StateUncertain)
	mustExec(t, e.owner, `UPDATE waystone.operations SET resolved_at = now(), resolved_outcome = 'failed',
		resolved_by_type = 'local_user', resolved_by_id = '1000', resolved_by_name = 'admin a' WHERE operation_id = $1`, op.ID)
	want("resolved", "")

	// Missing, another tenant's, malformed.
	if _, err := getServer(e, adminA, missingServer); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
	if _, err := getServer(e, adminA, serverB1); !errors.Is(err, ErrNotFound) {
		t.Errorf("other tenant: %v", err)
	}
	if _, err := getServer(e, adminA, "nope"); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("malformed: %v", err)
	}
}

// V1: CreateOperation refuses a node not seen within NodeStaleAfter of the
// store's clock, writes nothing, and still replays an accepted request.
func TestCreateOperationNodeUnreachable(t *testing.T) {
	e := setup(t)
	seen := func(at any) {
		t.Helper()
		mustExec(t, e.owner, `UPDATE waystone.nodes SET last_seen_at = $1 WHERE node_id = $2`, at, nodeA)
	}
	count := func() (ops, keys int) {
		t.Helper()
		if err := e.owner.QueryRow(context.Background(),
			`SELECT (SELECT count(*) FROM waystone.operations), (SELECT count(*) FROM waystone.idempotency_keys)`).Scan(&ops, &keys); err != nil {
			t.Fatal(err)
		}
		return ops, keys
	}
	fp := [32]byte{1}

	first, _, err := e.create(adminA, CreateOperationInput{ServerID: serverA1, Action: ActionStart, IdempotencyKey: "k1", RequestSHA256: fp})
	if err != nil {
		t.Fatal(err)
	}

	seen(nil)
	ops, keys := count()
	if _, _, err := e.create(adminA, CreateOperationInput{ServerID: serverA2, Action: ActionStart, IdempotencyKey: "k2", RequestSHA256: fp}); !errors.Is(err, ErrNodeUnreachable) {
		t.Fatalf("never seen: %v", err)
	}
	if o, k := count(); o != ops || k != keys {
		t.Errorf("refusal wrote rows: %d/%d -> %d/%d", ops, keys, o, k)
	}
	if s, _ := getServer(e, adminA, serverA2); s.DesiredState != "stopped" {
		t.Errorf("desired_state = %q, want stopped", s.DesiredState)
	}
	// Unreachable comes before the blocking check.
	if _, _, err := e.create(adminA, CreateOperationInput{ServerID: serverA1, Action: ActionStop}); !errors.Is(err, ErrNodeUnreachable) {
		t.Errorf("blocked and unreachable: %v", err)
	}
	// A retry of the accepted request still gets it.
	again, replayed, err := e.create(adminA, CreateOperationInput{ServerID: serverA1, Action: ActionStart, IdempotencyKey: "k1", RequestSHA256: fp})
	if err != nil || !replayed || again.ID != first.ID {
		t.Errorf("replay = %v %v %v", again.ID, replayed, err)
	}

	seen(e.clock.now.Add(-NodeStaleAfter - time.Second))
	if _, _, err := e.create(adminA, CreateOperationInput{ServerID: serverA2, Action: ActionStart}); !errors.Is(err, ErrNodeUnreachable) {
		t.Errorf("46 s ago: %v", err)
	}
	seen(e.clock.now.Add(-NodeStaleAfter))
	if _, _, err := e.create(adminA, CreateOperationInput{ServerID: serverA2, Action: ActionStart}); err != nil {
		t.Errorf("45 s ago: %v", err)
	}
}

// D5: a revoked node's servers refuse new operations with
// ErrNodeRevoked, before the reachability check, and say so in the list.
func TestCreateOperationNodeRevoked(t *testing.T) {
	e := setup(t)
	mustExec(t, e.owner, `UPDATE waystone.nodes SET revoked_at = now() WHERE node_id = $1`, nodeA)
	if _, _, err := e.create(adminA, CreateOperationInput{ServerID: serverA1, Action: ActionStart}); !errors.Is(err, ErrNodeRevoked) {
		t.Fatalf("revoked, still seen: %v", err)
	}
	mustExec(t, e.owner, `UPDATE waystone.nodes SET last_seen_at = NULL WHERE node_id = $1`, nodeA)
	if _, _, err := e.create(adminA, CreateOperationInput{ServerID: serverA1, Action: ActionStart}); !errors.Is(err, ErrNodeRevoked) {
		t.Fatalf("revoked and unreachable: %v", err)
	}
	if s, _ := getServer(e, adminA, serverA1); !s.NodeRevoked {
		t.Error("server doesn't say its node is revoked")
	}
	if s, _ := getServer(e, adminB, serverB1); s.NodeRevoked {
		t.Error("another node reads as revoked")
	}
}

func TestListNodes(t *testing.T) {
	e := setup(t)
	pin := pinOf(7)
	mustExec(t, e.owner, `INSERT INTO waystone.nodes (tenant_id, node_id, name, agent_spki_sha256, enrolled_at, agent_version, capabilities)
		VALUES ($1, '0199e1a0-0000-7000-8000-0000000000a2', 'a remote', $2, now(), '0.1.0', '{systemd,minecraft-java}')`, tenantA, pin[:])
	var nodes []Node
	err := e.store.InTenantTx(context.Background(), adminA, func(ctx context.Context, tx *Tx) error {
		var err error
		nodes, err = tx.ListNodes(ctx)
		return err
	})
	if err != nil || len(nodes) != 2 {
		t.Fatalf("ListNodes = %+v, %v", nodes, err)
	}
	local, remote := nodes[1], nodes[0] // "a remote" sorts first
	if local.ID != nodeA || local.Pinned || local.ServerCount != 2 || local.EnrolledAt != nil || local.AgentVersion != nil || len(local.Capabilities) != 0 {
		t.Errorf("local node %+v", local)
	}
	if !remote.Pinned || remote.ServerCount != 0 || remote.EnrolledAt == nil || *remote.AgentVersion != "0.1.0" || len(remote.Capabilities) != 2 || remote.RevokedAt != nil {
		t.Errorf("remote node %+v", remote)
	}
	err = e.store.InTenantTx(context.Background(), adminA, func(ctx context.Context, tx *Tx) error {
		if n, err := tx.GetNode(ctx, nodeA); err != nil || n.Name != "node a" {
			t.Errorf("GetNode = %+v, %v", n, err)
		}
		if _, err := tx.GetNode(ctx, nodeB); !errors.Is(err, ErrNotFound) {
			t.Errorf("other tenant's node: %v", err)
		}
		if _, err := tx.GetNode(ctx, "x"); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("bad ID: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
