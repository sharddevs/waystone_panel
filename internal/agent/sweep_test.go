// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	agentv1 "github.com/sharddevs/waystone_panel/gen/waystone/agent/v1"
	helperv1 "github.com/sharddevs/waystone_panel/gen/waystone/helper/v1"
)

const (
	srv1 = "0199e1a0-0000-7000-8000-000000000a01"
	srv2 = "0199e1a0-0000-7000-8000-000000000a02"
	srv3 = "0199e1a0-0000-7000-8000-000000000a03"
	srv4 = "0199e1a0-0000-7000-8000-000000000a04"
	srv5 = "0199e1a0-0000-7000-8000-000000000a05"
	srv6 = "0199e1a0-0000-7000-8000-000000000a06"
	srv7 = "0199e1a0-0000-7000-8000-000000000a07"
	srv8 = "0199e1a0-0000-7000-8000-000000000a08"
	node = "0199e1a0-0000-7000-8000-0000000000a1"
)

// fakeHelper answers STATUS from a function per server and records calls.
type fakeHelper struct {
	mu     sync.Mutex
	answer func(ctx context.Context, serverID string) (*helperv1.Response, error)
	calls  []*helperv1.Request
}

func (f *fakeHelper) Do(ctx context.Context, req *helperv1.Request) (*helperv1.Response, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	answer := f.answer
	f.mu.Unlock()
	return answer(ctx, req.GetServerId())
}

// set swaps the answers. Calls cut off by a deadline may still be running,
// so this takes the lock.
func (f *fakeHelper) set(answer func(ctx context.Context, serverID string) (*helperv1.Response, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answer = answer
}

func (f *fakeHelper) requests() []*helperv1.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*helperv1.Request(nil), f.calls...)
}

func (f *fakeHelper) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, r := range f.calls {
		out = append(out, r.GetServerId())
	}
	return out
}

func running(native string) *helperv1.Response {
	return &helperv1.Response{RunState: helperv1.RunState_RUN_STATE_RUNNING, NativeState: native}
}

func testAgent(t *testing.T, h Helper, call, sweep time.Duration) *Agent {
	t.Helper()
	a, err := New(Config{ControllerSocket: "/nonexistent", NodeID: node, Helper: h, CallTimeout: call, SweepTimeout: sweep})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

type wantEntry struct {
	state  agentv1.ProcessState
	native string
}

func checkEntries(t *testing.T, name string, got []*agentv1.ReportEntry, ids []string, want []wantEntry) {
	t.Helper()
	if len(got) != len(ids) {
		t.Fatalf("%s: %d entries for %d servers", name, len(got), len(ids))
	}
	for i, e := range got {
		if e.GetServerId() != ids[i] || e.GetProcessState() != want[i].state || e.GetNativeState() != want[i].native {
			t.Errorf("%s: entry %d = %s %v %q, want %s %v %q", name, i,
				e.GetServerId(), e.GetProcessState(), e.GetNativeState(), ids[i], want[i].state, want[i].native)
		}
	}
}

// R9: every kind of failure is UNKNOWN with its name, the others map one
// to one, and every request is a STATUS with the sweep's ID.
func TestSweepMapsEveryOutcome(t *testing.T) {
	const unknown = agentv1.ProcessState_PROCESS_STATE_UNKNOWN
	answers := map[string]func(ctx context.Context) (*helperv1.Response, error){
		srv1: func(context.Context) (*helperv1.Response, error) { return running("active/running"), nil },
		srv2: func(context.Context) (*helperv1.Response, error) {
			return &helperv1.Response{ErrorCode: helperv1.ErrorCode_ERROR_CODE_NOT_FOUND}, nil
		},
		srv3: func(context.Context) (*helperv1.Response, error) {
			return &helperv1.Response{ErrorCode: helperv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED}, nil
		},
		srv4: func(context.Context) (*helperv1.Response, error) {
			return &helperv1.Response{ErrorCode: helperv1.ErrorCode_ERROR_CODE_FAILED}, nil
		},
		srv5: func(context.Context) (*helperv1.Response, error) { return nil, errors.New("connection refused") },
		srv6: func(ctx context.Context) (*helperv1.Response, error) { <-ctx.Done(); return nil, ctx.Err() },
		srv7: func(context.Context) (*helperv1.Response, error) { return &helperv1.Response{}, nil },
		srv8: func(context.Context) (*helperv1.Response, error) {
			return &helperv1.Response{RunState: helperv1.RunState_RUN_STATE_STOPPED, NativeState: "inactive/dead"}, nil
		},
	}
	h := &fakeHelper{answer: func(ctx context.Context, id string) (*helperv1.Response, error) { return answers[id](ctx) }}
	a := testAgent(t, h, 50*time.Millisecond, 5*time.Second)
	ids := []string{srv1, srv2, srv3, srv4, srv5, srv6, srv7, srv8}
	const sweepID = "0199e1a0-0000-7000-8000-00000000c0de"
	got := a.sweep(context.Background(), sweepID, ids)
	checkEntries(t, "sweep", got, ids, []wantEntry{
		{agentv1.ProcessState_PROCESS_STATE_RUNNING, "active/running"},
		{unknown, "not_found"},
		{unknown, "permission_denied"},
		{unknown, "failed"},
		{unknown, "unavailable"},
		{unknown, "timeout"},
		{unknown, "no_run_state"},
		{agentv1.ProcessState_PROCESS_STATE_STOPPED, "inactive/dead"},
	})
	for _, r := range h.requests() {
		if r.GetAction() != helperv1.Action_ACTION_STATUS || r.GetOperationId() != sweepID {
			t.Errorf("request %+v, want STATUS with the sweep ID", r)
		}
	}
	// A helper that fails only after the deadline has passed is a
	// timeout, never "unavailable", whichever the agent notices first.
	h.set(func(ctx context.Context, _ string) (*helperv1.Response, error) {
		<-ctx.Done()
		return nil, errors.New("connection reset")
	})
	fast := testAgent(t, h, 2*time.Millisecond, 5*time.Second)
	many := make([]string, 50)
	for i := range many {
		many[i] = srv1
	}
	for _, e := range fast.sweep(context.Background(), sweepID, many) {
		if e.GetNativeState() != "timeout" {
			t.Fatalf("late failure reported as %q, want timeout", e.GetNativeState())
		}
	}

	// Each helper state maps to its own report state.
	for hs, want := range runStates {
		h.set(func(context.Context, string) (*helperv1.Response, error) {
			return &helperv1.Response{RunState: hs}, nil
		})
		if e := a.sweep(context.Background(), sweepID, []string{srv1}); e[0].GetProcessState() != want {
			t.Errorf("%v → %v, want %v", hs, e[0].GetProcessState(), want)
		}
	}
}

// Requirement 2 (R9): once the sweep's 10 s budget (shortened here) is
// spent, every server not yet observed is UNKNOWN with "timeout", the
// helper isn't even asked, and nothing from an earlier sweep comes back.
func TestSweepDeadlineNeverReusesStaleObservations(t *testing.T) {
	ids := []string{srv1, srv2, srv3, srv4, srv5, srv6}
	// First sweep: every server is running, quickly.
	h := &fakeHelper{answer: func(context.Context, string) (*helperv1.Response, error) { return running(""), nil }}
	a := testAgent(t, h, time.Second, 300*time.Millisecond)
	first := a.sweep(context.Background(), "0199e1a0-0000-7000-8000-000000000001", ids)
	for _, e := range first {
		if e.GetProcessState() != agentv1.ProcessState_PROCESS_STATE_RUNNING {
			t.Fatalf("first sweep: %v", e)
		}
	}

	// Second sweep: each call takes 120 ms, so only the first two fit
	// into 300 ms. The third is cut off by the sweep deadline mid-call,
	// and the rest are never asked.
	h.mu.Lock()
	h.calls = nil
	h.answer = func(ctx context.Context, id string) (*helperv1.Response, error) {
		select {
		case <-time.After(120 * time.Millisecond):
			return &helperv1.Response{RunState: helperv1.RunState_RUN_STATE_STOPPED}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	h.mu.Unlock()
	start := time.Now()
	second := a.sweep(context.Background(), "0199e1a0-0000-7000-8000-000000000002", ids)
	if took := time.Since(start); took > 600*time.Millisecond {
		t.Errorf("sweep took %s, budget 300 ms", took)
	}
	stopped := agentv1.ProcessState_PROCESS_STATE_STOPPED
	unknown := agentv1.ProcessState_PROCESS_STATE_UNKNOWN
	checkEntries(t, "second sweep", second, ids, []wantEntry{
		{stopped, ""}, {stopped, ""},
		{unknown, "timeout"}, {unknown, "timeout"}, {unknown, "timeout"}, {unknown, "timeout"},
	})
	// Give any stray call a moment to show up before counting.
	time.Sleep(100 * time.Millisecond)
	if got := h.called(); len(got) != 3 {
		t.Errorf("helper asked about %v after the deadline, want only the first 3", got)
	}

	// A helper client that ignores its context entirely still can't hold
	// the sweep past its deadline.
	release := make(chan struct{})
	defer close(release)
	h.mu.Lock()
	h.answer = func(context.Context, string) (*helperv1.Response, error) {
		<-release
		return running(""), nil
	}
	h.mu.Unlock()
	start = time.Now()
	third := a.sweep(context.Background(), "0199e1a0-0000-7000-8000-000000000003", ids)
	if took := time.Since(start); took > 600*time.Millisecond {
		t.Errorf("sweep with a stuck helper took %s, budget 300 ms", took)
	}
	for _, e := range third {
		if e.GetProcessState() != unknown || e.GetNativeState() != "timeout" {
			t.Errorf("stuck helper: %v, want UNKNOWN timeout", e)
		}
	}
}

// D3: the helper's text is made safe and cut to 32 bytes on a character
// boundary.
func TestCleanNativeState(t *testing.T) {
	cases := map[string]string{
		"active/running":        "active/running",
		strings.Repeat("x", 40): strings.Repeat("x", 32),
		strings.Repeat("€", 11): strings.Repeat("€", 10), // 33 bytes → 30
		"a\nb\x00c":             "a?b?c",
		"bad\xffutf8":           "bad?utf8",
		"​zero width is fine":   "​zero width is fine",
	}
	for in, want := range cases {
		got := cleanNativeState(in)
		if got != want {
			t.Errorf("clean(%q) = %q, want %q", in, got, want)
		}
		if len(got) > maxNativeState || !utf8.ValidString(got) {
			t.Errorf("clean(%q) = %q is not a valid report value", in, got)
		}
	}
	h := &fakeHelper{answer: func(context.Context, string) (*helperv1.Response, error) {
		return running(strings.Repeat("é", 20) + "\n"), nil
	}}
	e := testAgent(t, h, time.Second, time.Second).sweep(context.Background(), node, []string{srv1})
	if got := e[0].GetNativeState(); got != strings.Repeat("é", 16) {
		t.Errorf("helper native state cleaned to %q", got)
	}
}
