// SPDX-License-Identifier: AGPL-3.0-only

package helper

// Stage 4c, S3 and B2: the helper never runs two STARTs or STOPs for one
// server at once, never begins one while systemd still has a job for the
// unit, and never begins one after its sender's deadline.

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	helperv1 "github.com/sharddevs/waystone_panel/gen/waystone/helper/v1"
)

const otherServerID = "9b1d6e2a-5c3f-4a8e-b7d4-0e2f1a3c5d7e"

// gateRunner is a fake systemctl. show prints showOutput (after calling
// onShow, if set). start and stop wait on the unit's gate, if it has one,
// and count how many run at once per unit.
type gateRunner struct {
	mu         sync.Mutex
	showOutput string
	onShow     func()
	gates      map[string]chan struct{}
	entered    chan string
	active     map[string]int
	maxActive  map[string]int
	changes    []string
}

func newGateRunner() *gateRunner {
	return &gateRunner{
		showOutput: idleUnit,
		gates:      make(map[string]chan struct{}),
		entered:    make(chan string, 16),
		active:     make(map[string]int),
		maxActive:  make(map[string]int),
	}
}

func (g *gateRunner) Run(ctx context.Context, argv []string) ([]byte, error) {
	verb, unit := argv[2], argv[len(argv)-1]
	if verb == "show" {
		g.mu.Lock()
		out, onShow := g.showOutput, g.onShow
		g.mu.Unlock()
		if onShow != nil {
			onShow()
		}
		return []byte(out), nil
	}
	g.mu.Lock()
	g.changes = append(g.changes, verb+" "+unit)
	g.active[unit]++
	if g.active[unit] > g.maxActive[unit] {
		g.maxActive[unit] = g.active[unit]
	}
	gate := g.gates[unit]
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		g.active[unit]--
		g.mu.Unlock()
	}()
	g.entered <- unit
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, nil
}

func (g *gateRunner) changeCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.changes)
}

func (g *gateRunner) max(unit string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.maxActive[unit]
}

func changeCommand(action helperv1.Action, serverID string) Command {
	return Command{OperationID: testOpID, ServerID: serverID, Action: action, NotAfter: time.UnixMilli(testNotAfter)}
}

func TestValidateRequestDeadline(t *testing.T) {
	for _, action := range []helperv1.Action{helperv1.Action_ACTION_START, helperv1.Action_ACTION_STOP} {
		req := validRequest(action)
		req.NotAfterUnixMs = 0
		if _, err := ValidateRequest(req); err == nil {
			t.Errorf("%v without a deadline: accepted, want refused", action)
		}
		req.NotAfterUnixMs = -1
		if _, err := ValidateRequest(req); err == nil {
			t.Errorf("%v with a negative deadline: accepted, want refused", action)
		}
	}
	status := validRequest(helperv1.Action_ACTION_STATUS)
	status.NotAfterUnixMs = testNotAfter
	if _, err := ValidateRequest(status); err == nil {
		t.Error("STATUS with a deadline: accepted, want refused")
	}
	read := &helperv1.Request{OperationId: testOpID, ServerId: testServerID, Action: helperv1.Action_ACTION_READ_FILE,
		File: &helperv1.FileSpec{Name: "server.properties"}, NotAfterUnixMs: testNotAfter}
	if _, err := ValidateRequest(read); err == nil {
		t.Error("READ_FILE with a deadline: accepted, want refused")
	}
}

// The forced interleaving from the scope note's S3 row, at the helper:
// an old STOP for A is stuck inside systemctl; a START for A from anyone
// is refused at once, a START for B runs, and once the STOP returns a
// START for A runs again.
func TestHelperExcludesActionsPerServer(t *testing.T) {
	g := newGateRunner()
	unitA, unitB := UnitName(testServerID), UnitName(otherServerID)
	gateA := make(chan struct{})
	g.gates[unitA] = gateA
	s := Systemd{Runner: g, Busy: &Busy{}}
	ctx := context.Background()

	stopDone := make(chan *helperv1.Response, 1)
	go func() { stopDone <- s.Handle(ctx, changeCommand(helperv1.Action_ACTION_STOP, testServerID)) }()
	if got := <-g.entered; got != unitA {
		t.Fatalf("entered %s, want %s", got, unitA)
	}

	// The same server, while its STOP is still inside systemctl.
	resp := s.Handle(ctx, changeCommand(helperv1.Action_ACTION_START, testServerID))
	if resp.GetErrorCode() != helperv1.ErrorCode_ERROR_CODE_CONFLICT || !strings.Contains(resp.GetErrorMessage(), "still running") {
		t.Errorf("START for a busy server: %v %q, want CONFLICT, still running", resp.GetErrorCode(), resp.GetErrorMessage())
	}
	if n := g.changeCount(); n != 1 {
		t.Errorf("%d systemctl start/stop calls, want 1: the refused START must not run", n)
	}

	// Another server isn't held up.
	if resp := s.Handle(ctx, changeCommand(helperv1.Action_ACTION_START, otherServerID)); resp.GetErrorCode() != helperv1.ErrorCode_ERROR_CODE_UNSPECIFIED {
		t.Errorf("START for another server: %v %q, want success", resp.GetErrorCode(), resp.GetErrorMessage())
	}
	<-g.entered

	close(gateA)
	if resp := <-stopDone; resp.GetErrorCode() != helperv1.ErrorCode_ERROR_CODE_UNSPECIFIED {
		t.Errorf("the old STOP: %v %q, want success", resp.GetErrorCode(), resp.GetErrorMessage())
	}
	if resp := s.Handle(ctx, changeCommand(helperv1.Action_ACTION_START, testServerID)); resp.GetErrorCode() != helperv1.ErrorCode_ERROR_CODE_UNSPECIFIED {
		t.Errorf("START after the STOP finished: %v %q, want success", resp.GetErrorCode(), resp.GetErrorMessage())
	}
	<-g.entered
	if m := g.max(unitA); m != 1 {
		t.Errorf("at most %d actions at once on A, want 1", m)
	}
	if m := g.max(unitB); m != 1 {
		t.Errorf("at most %d actions at once on B, want 1", m)
	}
}

// Two Systemd values with nil Busy share the helper process's set, which
// is what the real helper does: one Router per connection would still
// exclude across connections.
func TestHelperProcessWideExclusion(t *testing.T) {
	g := newGateRunner()
	gate := make(chan struct{})
	g.gates[UnitName(testServerID)] = gate
	first, second := Systemd{Runner: g}, Systemd{Runner: g}
	done := make(chan *helperv1.Response, 1)
	go func() {
		done <- first.Handle(context.Background(), changeCommand(helperv1.Action_ACTION_STOP, testServerID))
	}()
	<-g.entered
	resp := second.Handle(context.Background(), changeCommand(helperv1.Action_ACTION_START, testServerID))
	if resp.GetErrorCode() != helperv1.ErrorCode_ERROR_CODE_CONFLICT {
		t.Errorf("second Systemd value: %v, want CONFLICT", resp.GetErrorCode())
	}
	close(gate)
	<-done
}

// B2: a job systemd still runs for the unit, from anyone, blocks a new
// START or STOP; nothing runs. A crash loop waiting to restart has no job
// and can still be stopped.
func TestHelperRefusesLeftoverJob(t *testing.T) {
	cases := []struct {
		name string
		show string
		want helperv1.ErrorCode
		runs bool
	}{
		{"job running", "LoadState=loaded\nActiveState=deactivating\nSubState=stop-sigterm\nJob=4711\n", helperv1.ErrorCode_ERROR_CODE_CONFLICT, false},
		{"job queued, unit idle", "LoadState=loaded\nActiveState=inactive\nSubState=dead\nJob=4712\n", helperv1.ErrorCode_ERROR_CODE_CONFLICT, false},
		{"deactivating, no job", "LoadState=loaded\nActiveState=deactivating\nSubState=stop-sigterm\nJob=\n", helperv1.ErrorCode_ERROR_CODE_CONFLICT, false},
		{"activating, no job", "LoadState=loaded\nActiveState=activating\nSubState=start\nJob=\n", helperv1.ErrorCode_ERROR_CODE_CONFLICT, false},
		{"reloading", "LoadState=loaded\nActiveState=reloading\nSubState=reload\nJob=\n", helperv1.ErrorCode_ERROR_CODE_CONFLICT, false},
		{"auto-restart, no job", "LoadState=loaded\nActiveState=activating\nSubState=auto-restart\nJob=\n", helperv1.ErrorCode_ERROR_CODE_UNSPECIFIED, true},
		// The exemption is that exact pair with no job, nothing else.
		{"auto-restart with a job", "LoadState=loaded\nActiveState=activating\nSubState=auto-restart\nJob=4713\n", helperv1.ErrorCode_ERROR_CODE_CONFLICT, false},
		{"activating, auto-restart-queued", "LoadState=loaded\nActiveState=activating\nSubState=auto-restart-queued\nJob=\n", helperv1.ErrorCode_ERROR_CODE_CONFLICT, false},
		{"deactivating, auto-restart", "LoadState=loaded\nActiveState=deactivating\nSubState=auto-restart\nJob=\n", helperv1.ErrorCode_ERROR_CODE_CONFLICT, false},
		{"reloading, auto-restart", "LoadState=loaded\nActiveState=reloading\nSubState=auto-restart\nJob=\n", helperv1.ErrorCode_ERROR_CODE_CONFLICT, false},
		{"refreshing, auto-restart", "LoadState=loaded\nActiveState=refreshing\nSubState=auto-restart\nJob=\n", helperv1.ErrorCode_ERROR_CODE_CONFLICT, false},
		{"refreshing", "LoadState=loaded\nActiveState=refreshing\nSubState=running\nJob=\n", helperv1.ErrorCode_ERROR_CODE_CONFLICT, false},
		{"idle", idleUnit, helperv1.ErrorCode_ERROR_CODE_UNSPECIFIED, true},
		{"running, no job", "LoadState=loaded\nActiveState=active\nSubState=running\nJob=\n", helperv1.ErrorCode_ERROR_CODE_UNSPECIFIED, true},
		{"unit unknown", "LoadState=not-found\nActiveState=inactive\nSubState=dead\nJob=\n", helperv1.ErrorCode_ERROR_CODE_NOT_FOUND, false},
		{"no Job line", "LoadState=loaded\nActiveState=inactive\nSubState=dead\n", helperv1.ErrorCode_ERROR_CODE_FAILED, false},
		{"empty output", "", helperv1.ErrorCode_ERROR_CODE_FAILED, false},
	}
	for _, tt := range cases {
		for _, action := range []helperv1.Action{helperv1.Action_ACTION_START, helperv1.Action_ACTION_STOP} {
			g := newGateRunner()
			g.showOutput = tt.show
			resp := Systemd{Runner: g, Busy: &Busy{}}.Handle(context.Background(), changeCommand(action, testServerID))
			if resp.GetErrorCode() != tt.want {
				t.Errorf("%s, %v: %v %q, want %v", tt.name, action, resp.GetErrorCode(), resp.GetErrorMessage(), tt.want)
			}
			if ran := g.changeCount() > 0; ran != tt.runs {
				t.Errorf("%s, %v: systemctl ran = %t, want %t", tt.name, action, ran, tt.runs)
			}
			if tt.runs {
				<-g.entered
			}
		}
	}
}

// A leftover job also blocks after the old systemctl was killed at the
// helper's deadline: the lock is gone by then, and the job check is what
// still holds.
func TestHelperJobCheckOutlivesKilledCall(t *testing.T) {
	g := newGateRunner()
	unit := UnitName(testServerID)
	g.gates[unit] = make(chan struct{}) // never opens: the call is cut off
	s := Systemd{Runner: g, Busy: &Busy{}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *helperv1.Response, 1)
	go func() { done <- s.Handle(ctx, changeCommand(helperv1.Action_ACTION_STOP, testServerID)) }()
	<-g.entered
	cancel() // what the helper's ConnTimeout does to systemctl
	if resp := <-done; resp.GetErrorCode() != helperv1.ErrorCode_ERROR_CODE_FAILED {
		t.Fatalf("killed STOP: %v, want FAILED", resp.GetErrorCode())
	}
	// systemd carries on with the stop job.
	g.mu.Lock()
	g.showOutput = "LoadState=loaded\nActiveState=deactivating\nSubState=stop-sigterm\nJob=4711\n"
	g.mu.Unlock()
	resp := s.Handle(context.Background(), changeCommand(helperv1.Action_ACTION_START, testServerID))
	if resp.GetErrorCode() != helperv1.ErrorCode_ERROR_CODE_CONFLICT {
		t.Errorf("START while the old job runs: %v %q, want CONFLICT", resp.GetErrorCode(), resp.GetErrorMessage())
	}
	if n := g.changeCount(); n != 1 {
		t.Errorf("%d start/stop calls, want 1", n)
	}
	// The job finishes.
	g.mu.Lock()
	g.showOutput = idleUnit
	delete(g.gates, unit)
	g.mu.Unlock()
	if resp := s.Handle(context.Background(), changeCommand(helperv1.Action_ACTION_START, testServerID)); resp.GetErrorCode() != helperv1.ErrorCode_ERROR_CODE_UNSPECIFIED {
		t.Errorf("START after the job finished: %v %q, want success", resp.GetErrorCode(), resp.GetErrorMessage())
	}
}

// The deadline is checked after the job check, right before systemctl.
func TestHelperRefusesExpiredRequest(t *testing.T) {
	notAfter := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	cmd := Command{OperationID: testOpID, ServerID: testServerID, Action: helperv1.Action_ACTION_START, NotAfter: notAfter}

	// Already past.
	g := newGateRunner()
	s := Systemd{Runner: g, Busy: &Busy{}, Now: func() time.Time { return notAfter }}
	resp := s.Handle(context.Background(), cmd)
	if resp.GetErrorCode() != helperv1.ErrorCode_ERROR_CODE_CONFLICT || !strings.Contains(resp.GetErrorMessage(), "deadline") {
		t.Errorf("at the deadline: %v %q, want CONFLICT about the deadline", resp.GetErrorCode(), resp.GetErrorMessage())
	}
	if g.changeCount() != 0 {
		t.Error("systemctl ran for an expired request")
	}

	// Passes while the job check runs: still refused.
	g = newGateRunner()
	var mu sync.Mutex
	now := notAfter.Add(-time.Second)
	g.onShow = func() { mu.Lock(); now = notAfter.Add(time.Millisecond); mu.Unlock() }
	s = Systemd{Runner: g, Busy: &Busy{}, Now: func() time.Time { mu.Lock(); defer mu.Unlock(); return now }}
	resp = s.Handle(context.Background(), cmd)
	if resp.GetErrorCode() != helperv1.ErrorCode_ERROR_CODE_CONFLICT {
		t.Errorf("deadline passed during the job check: %v, want CONFLICT", resp.GetErrorCode())
	}
	if g.changeCount() != 0 {
		t.Error("systemctl ran after the deadline passed")
	}

	// Just in time.
	g = newGateRunner()
	s = Systemd{Runner: g, Busy: &Busy{}, Now: func() time.Time { return notAfter.Add(-time.Millisecond) }}
	if resp := s.Handle(context.Background(), cmd); resp.GetErrorCode() != helperv1.ErrorCode_ERROR_CODE_UNSPECIFIED {
		t.Errorf("before the deadline: %v %q, want success", resp.GetErrorCode(), resp.GetErrorMessage())
	}
}

// End to end through the real Server and Client: a request that reaches
// the helper after its deadline (as if it had waited in the accept queue)
// is refused and never runs.
func TestServerRefusesLateRequest(t *testing.T) {
	g := newGateRunner()
	sock := startServerWith(t, Systemd{Runner: g, Busy: &Busy{}}.Handle)
	req := validRequest(helperv1.Action_ACTION_START)
	req.NotAfterUnixMs = time.Now().Add(-time.Second).UnixMilli()
	resp := send(t, sock, req)
	if resp.GetErrorCode() != helperv1.ErrorCode_ERROR_CODE_CONFLICT {
		t.Errorf("late request: %v %q, want CONFLICT", resp.GetErrorCode(), resp.GetErrorMessage())
	}
	if g.changeCount() != 0 {
		t.Error("systemctl ran for a late request")
	}
}
