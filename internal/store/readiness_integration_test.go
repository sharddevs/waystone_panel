// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// Stage 4d-2 (D6): readiness travels with every report, and the hello
// hands the agent each server's game port.

type readinessRow struct {
	state    string
	since    *time.Time
	observed *time.Time
}

func (e *env) readiness(t *testing.T, server string) readinessRow {
	t.Helper()
	var r readinessRow
	err := e.owner.QueryRow(context.Background(), `
		SELECT readiness, readiness_since, readiness_observed_at
		FROM waystone.servers WHERE server_id = $1`, server).Scan(&r.state, &r.since, &r.observed)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestReportReadiness(t *testing.T) {
	e := setup(t)
	epoch, _, err := e.hello(nodeActA)
	if err != nil {
		t.Fatal(err)
	}
	report := func(seq int64, state ProcessState, r Readiness) {
		t.Helper()
		acc, rej, err := e.report(nodeActA, Report{Epoch: epoch, Seq: seq, AgentTime: agentTime(t), Entries: []ReportEntry{
			{ServerID: serverA1, State: state, Readiness: r},
		}})
		if err != nil || acc != 1 || rej != 0 {
			t.Fatalf("seq %d: %d/%d, %v", seq, acc, rej, err)
		}
	}

	// Before any report: unknown, never observed.
	if r := e.readiness(t, serverA1); r.state != "unknown" || r.since != nil || r.observed != nil {
		t.Fatalf("before = %+v", r)
	}

	report(1, ProcessRunning, ReadinessNotReady)
	first := e.readiness(t, serverA1)
	n := e.node(t, nodeA)
	if first.state != "not_ready" || first.observed == nil || !first.observed.Equal(*n.lastSeen) ||
		first.since == nil || !first.since.Equal(*first.observed) {
		t.Fatalf("first = %+v, node seen %v", first, n.lastSeen)
	}

	// Same readiness: observed moves, since stays.
	time.Sleep(10 * time.Millisecond)
	report(2, ProcessRunning, ReadinessNotReady)
	same := e.readiness(t, serverA1)
	if same.state != "not_ready" || !same.since.Equal(*first.since) || !same.observed.After(*first.observed) {
		t.Fatalf("same = %+v, first %+v", same, first)
	}

	// A change: both move.
	time.Sleep(10 * time.Millisecond)
	report(3, ProcessRunning, ReadinessReady)
	ready := e.readiness(t, serverA1)
	if ready.state != "ready" || !ready.since.Equal(*ready.observed) || !ready.since.After(*same.since) {
		t.Fatalf("ready = %+v", ready)
	}

	// A rejected entry (stale seq) leaves readiness alone.
	acc, rej, err := e.report(nodeActA, Report{Epoch: epoch, Seq: 3, AgentTime: agentTime(t), Entries: []ReportEntry{
		{ServerID: serverA1, State: ProcessStopped, Readiness: ReadinessNotReady},
	}})
	if err != nil || acc != 0 || rej != 1 {
		t.Fatalf("stale: %d/%d, %v", acc, rej, err)
	}
	if r := e.readiness(t, serverA1); !reflect.DeepEqual(r, ready) {
		t.Errorf("after stale = %+v, want %+v", r, ready)
	}

	// What the API reads back.
	s, err := getServer(e, adminA, serverA1)
	if err != nil {
		t.Fatal(err)
	}
	if s.Readiness != "ready" || s.ReadinessSince == nil || !s.ReadinessSince.Equal(*ready.since) ||
		s.ReadinessObservedAt == nil || !s.ReadinessObservedAt.Equal(*ready.observed) {
		t.Errorf("GetServer readiness = %q %v %v", s.Readiness, s.ReadinessSince, s.ReadinessObservedAt)
	}
}

func TestNodeHelloGamePorts(t *testing.T) {
	e := setup(t)
	// a1 has a port, a2 none; b1 is another node's. A value that isn't a
	// port can't be stored at all since 00009 (6H4), even by the owner.
	mustExec(t, e.owner, `UPDATE waystone.servers SET config = '{"port": 25565}' WHERE server_id = $1`, serverA1)
	if _, err := e.owner.Exec(context.Background(), `UPDATE waystone.servers SET config = '{"port": "abc"}' WHERE server_id = $1`, serverA2); err == nil {
		t.Error("the owner stored port \"abc\"")
	}
	mustExec(t, e.owner, `UPDATE waystone.servers SET config = '{"port": 25570}' WHERE server_id = $1`, serverB1)

	var ids []string
	var ports map[string]uint32
	err := e.store.InTenantTx(context.Background(), nodeActA, func(ctx context.Context, tx *Tx) error {
		var err error
		_, ids, ports, err = tx.NodeHello(ctx)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ids, []string{serverA1, serverA2}) {
		t.Errorf("ids = %v", ids)
	}
	if !reflect.DeepEqual(ports, map[string]uint32{serverA1: 25565}) {
		t.Errorf("ports = %v, want only a1's", ports)
	}

	// No config at all: an empty map, not nil, and no error.
	mustExec(t, e.owner, `UPDATE waystone.servers SET config = '{}'`)
	err = e.store.InTenantTx(context.Background(), nodeActA, func(ctx context.Context, tx *Tx) error {
		var err error
		_, _, ports, err = tx.NodeHello(ctx)
		return err
	})
	if err != nil || ports == nil || len(ports) != 0 {
		t.Errorf("no ports = %v, %v", ports, err)
	}
}
