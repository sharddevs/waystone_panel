// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	agentv1 "github.com/sharddevs/waystone_panel/gen/waystone/agent/v1"
)

// fakeProber is ready for the ports in ready and not ready for the rest.
// calls, when set, counts the probes.
type fakeProber struct {
	ready map[uint32]bool
	calls *atomic.Int32
}

func (p fakeProber) Probe(_ context.Context, port uint32) error {
	if p.calls != nil {
		p.calls.Add(1)
	}
	if p.ready[port] {
		return nil
	}
	return errors.New("not ready")
}

// hangProber never answers before its deadline.
type hangProber struct{}

func (hangProber) Probe(ctx context.Context, _ uint32) error {
	<-ctx.Done()
	return ctx.Err()
}

// lateProber answers "ready" after its deadline has passed.
type lateProber struct{}

func (lateProber) Probe(ctx context.Context, _ uint32) error {
	<-ctx.Done()
	return nil
}

func probeAgent(t *testing.T, p Prober) *Agent {
	t.Helper()
	a, err := New(Config{ControllerSocket: "/nonexistent", NodeID: node, Helper: &fakeHelper{}, Runner: &fakeRunner{},
		Prober: p, ProbeTimeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

const (
	srvA = "0199e1a0-0000-7000-8000-00000000000a"
	srvB = "0199e1a0-0000-7000-8000-00000000000b"
	srvC = "0199e1a0-0000-7000-8000-00000000000c"
)

func entryOf(id string, s agentv1.ProcessState) *agentv1.ReportEntry {
	return &agentv1.ReportEntry{ServerId: id, ProcessState: s}
}

// D6: the probe decides only for a running server with a port. Every
// other state is decided without probing.
func TestReadinessByState(t *testing.T) {
	var calls atomic.Int32
	a := probeAgent(t, fakeProber{ready: map[uint32]bool{25565: true}, calls: &calls})
	ports := map[string]uint32{srvA: 25565, srvB: 25567}
	cases := []struct {
		id    string
		state agentv1.ProcessState
		want  agentv1.Readiness
	}{
		{srvA, agentv1.ProcessState_PROCESS_STATE_RUNNING, agentv1.Readiness_READINESS_READY},
		{srvB, agentv1.ProcessState_PROCESS_STATE_RUNNING, agentv1.Readiness_READINESS_NOT_READY},
		{srvC, agentv1.ProcessState_PROCESS_STATE_RUNNING, agentv1.Readiness_READINESS_UNKNOWN}, // no port
		{srvA, agentv1.ProcessState_PROCESS_STATE_UNKNOWN, agentv1.Readiness_READINESS_UNKNOWN},
		{srvA, agentv1.ProcessState_PROCESS_STATE_STARTING, agentv1.Readiness_READINESS_NOT_READY},
		{srvA, agentv1.ProcessState_PROCESS_STATE_STOPPING, agentv1.Readiness_READINESS_NOT_READY},
		{srvA, agentv1.ProcessState_PROCESS_STATE_STOPPED, agentv1.Readiness_READINESS_NOT_READY},
		{srvA, agentv1.ProcessState_PROCESS_STATE_FAILED, agentv1.Readiness_READINESS_NOT_READY},
	}
	for _, c := range cases {
		calls.Store(0)
		e := entryOf(c.id, c.state)
		a.readiness(context.Background(), []*agentv1.ReportEntry{e}, ports)
		if e.GetReadiness() != c.want {
			t.Errorf("%s %s: %s, want %s", c.id, c.state, e.GetReadiness(), c.want)
		}
		probed := c.state == agentv1.ProcessState_PROCESS_STATE_RUNNING && c.id != srvC
		if got := calls.Load() == 1; got != probed {
			t.Errorf("%s %s: probed %v, want %v", c.id, c.state, got, probed)
		}
	}
}

// A probe that hangs is NOT_READY at its own deadline, and doesn't hold
// up the others, which run at the same time.
func TestReadinessProbeDeadline(t *testing.T) {
	a := probeAgent(t, hangProber{})
	entries := []*agentv1.ReportEntry{
		entryOf(srvA, agentv1.ProcessState_PROCESS_STATE_RUNNING),
		entryOf(srvB, agentv1.ProcessState_PROCESS_STATE_RUNNING),
		entryOf(srvC, agentv1.ProcessState_PROCESS_STATE_RUNNING),
	}
	start := time.Now()
	a.readiness(context.Background(), entries, map[string]uint32{srvA: 1, srvB: 2, srvC: 3})
	if d := time.Since(start); d > 140*time.Millisecond {
		t.Errorf("three hanging probes took %s; they should run together", d)
	}
	for _, e := range entries {
		if e.GetReadiness() != agentv1.Readiness_READINESS_NOT_READY {
			t.Errorf("%s: %s, want NOT_READY", e.GetServerId(), e.GetReadiness())
		}
	}
}

// "Ready" that only arrives after the deadline doesn't count.
func TestReadinessLateAnswer(t *testing.T) {
	a := probeAgent(t, lateProber{})
	e := entryOf(srvA, agentv1.ProcessState_PROCESS_STATE_RUNNING)
	a.readiness(context.Background(), []*agentv1.ReportEntry{e}, map[string]uint32{srvA: 1})
	if e.GetReadiness() != agentv1.Readiness_READINESS_NOT_READY {
		t.Errorf("late ready: %s, want NOT_READY", e.GetReadiness())
	}
}

func TestNewNeedsProber(t *testing.T) {
	_, err := New(Config{ControllerSocket: "/nonexistent", NodeID: node, Helper: &fakeHelper{}, Runner: &fakeRunner{}})
	if err == nil {
		t.Fatal("New without a prober succeeded")
	}
	a := probeAgent(t, fakeProber{})
	if a.probeTimeout != 50*time.Millisecond {
		t.Errorf("probeTimeout = %s", a.probeTimeout)
	}
	b, err := New(Config{ControllerSocket: "/nonexistent", NodeID: node, Helper: &fakeHelper{}, Runner: &fakeRunner{}, Prober: fakeProber{}})
	if err != nil || b.probeTimeout != probeTimeout {
		t.Errorf("default probeTimeout = %v, %v", b.probeTimeout, err)
	}
}

// The hello reply's game ports: only for listed servers, only real ports.
func TestCheckHelloReplyGamePorts(t *testing.T) {
	reply := func(ports map[string]uint32) *agentv1.ConnectResponse {
		return &agentv1.ConnectResponse{Msg: &agentv1.ConnectResponse_HelloReply{HelloReply: &agentv1.HelloReply{
			Epoch: 1, ServerIds: []string{srvA, srvB}, ReportIntervalSeconds: 15, GamePorts: ports,
		}}}
	}
	for name, ports := range map[string]map[string]uint32{
		"none":         nil,
		"one":          {srvA: 25565},
		"both":         {srvA: 25565, srvB: 1},
		"highest port": {srvA: 65535},
	} {
		if _, err := checkHelloReply(reply(ports)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, ports := range map[string]map[string]uint32{
		"unlisted server": {srvC: 25565},
		"port 0":          {srvA: 0},
		"port 65536":      {srvA: 65536},
	} {
		if _, err := checkHelloReply(reply(ports)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
