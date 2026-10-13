// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

import (
	"context"
	"errors"
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
