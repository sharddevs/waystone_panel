// SPDX-License-Identifier: AGPL-3.0-only

package agent

// Stage 4c-1, the agent's side of commands (notes/stage4c-scope.md v2.1
// §5): checks, the result mapping (Q4, S5), restart, and what happens to
// a command whose session ends.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"

	agentv1 "github.com/sharddevs/waystone_panel/gen/waystone/agent/v1"
	helperv1 "github.com/sharddevs/waystone_panel/gen/waystone/helper/v1"
	"github.com/sharddevs/waystone_panel/internal/helper"
	"github.com/sharddevs/waystone_panel/internal/lifecycle"
)

// fakeRunner records Start and Stop calls. fail decides each call's
// error; gate, if set, holds every call until it's closed.
type fakeRunner struct {
	mu        sync.Mutex
	calls     []string // "start <server>" / "stop <server>"
	deadlines []time.Duration
	fail      func(action, serverID string) error
	gate      chan struct{}
	entered   chan string // if set, gets each call as it starts
}

func (f *fakeRunner) call(ctx context.Context, action, serverID string) error {
	f.mu.Lock()
	f.calls = append(f.calls, action+" "+serverID)
	if d, ok := ctx.Deadline(); ok {
		f.deadlines = append(f.deadlines, time.Until(d))
	}
	fail, gate, entered := f.fail, f.gate, f.entered
	f.mu.Unlock()
	if entered != nil {
		entered <- action + " " + serverID
	}
	if gate != nil {
		<-gate
	}
	if fail != nil {
		return fail(action, serverID)
	}
	return nil
}

func (f *fakeRunner) Start(ctx context.Context, serverID, _ string) error {
	return f.call(ctx, "start", serverID)
}

func (f *fakeRunner) Stop(ctx context.Context, serverID, _ string) error {
	return f.call(ctx, "stop", serverID)
}

func (f *fakeRunner) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

const (
	op1 = "0199e1a0-0000-7000-8000-0000000c0001"
	op2 = "0199e1a0-0000-7000-8000-0000000c0002"
	op3 = "0199e1a0-0000-7000-8000-0000000c0003"
	op4 = "0199e1a0-0000-7000-8000-0000000c0004"
	op5 = "0199e1a0-0000-7000-8000-0000000c0005"
)

var (
	succeeded   = agentv1.ResultOutcome_RESULT_OUTCOME_SUCCEEDED
	failed      = agentv1.ResultOutcome_RESULT_OUTCOME_FAILED
	unknown     = agentv1.ResultOutcome_RESULT_OUTCOME_UNKNOWN
	start       = agentv1.CommandAction_COMMAND_ACTION_START
	stopAction  = agentv1.CommandAction_COMMAND_ACTION_STOP
	restart     = agentv1.CommandAction_COMMAND_ACTION_RESTART
	noCode      = agentv1.ResultCode_RESULT_CODE_UNSPECIFIED
	noAnswer    = agentv1.ResultCode_RESULT_CODE_NO_ANSWER
	unavailable = agentv1.ResultCode_RESULT_CODE_HELPER_UNAVAILABLE
)

func command(epoch int64, opID, serverID string, action agentv1.CommandAction) *agentv1.Command {
	return &agentv1.Command{Epoch: epoch, OperationId: opID, ServerId: serverID, Action: action}
}

// Q4, S5: how each Runner error becomes a result.
func TestOutcomeOf(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		outcome agentv1.ResultOutcome
		code    agentv1.ResultCode
	}{
		{"ok", nil, succeeded, noCode},
		{"never sent", fmt.Errorf("could not reach helper: %w", fmt.Errorf("%w: dial: no such file", helper.ErrNotSent)), failed, unavailable},
		{"not found", fmt.Errorf("%w: x", lifecycle.ErrNotFound), failed, agentv1.ResultCode_RESULT_CODE_NOT_FOUND},
		{"permission", fmt.Errorf("%w: x", lifecycle.ErrPermissionDenied), failed, agentv1.ResultCode_RESULT_CODE_PERMISSION_DENIED},
		{"invalid", fmt.Errorf("%w: x", lifecycle.ErrInvalidRequest), failed, agentv1.ResultCode_RESULT_CODE_INVALID_REQUEST},
		{"conflict is busy", fmt.Errorf("%w: x", lifecycle.ErrConflict), failed, agentv1.ResultCode_RESULT_CODE_SERVER_BUSY},
		{"action failed", fmt.Errorf("%w: x", lifecycle.ErrFailed), failed, agentv1.ResultCode_RESULT_CODE_ACTION_FAILED},
		{"changed", fmt.Errorf("%w: x", lifecycle.ErrChanged), failed, agentv1.ResultCode_RESULT_CODE_ACTION_FAILED},
		{"after a write", fmt.Errorf("could not reach helper: %w", io.ErrUnexpectedEOF), unknown, noAnswer},
		{"timeout", fmt.Errorf("could not reach helper: %w", context.DeadlineExceeded), unknown, noAnswer},
		{"anything else", errors.New("?"), unknown, noAnswer},
	}
	for _, c := range cases {
		o, code := outcomeOf(c.err)
		if o != c.outcome || code != c.code {
			t.Errorf("%s: %v %v, want %v %v", c.name, o, code, c.outcome, c.code)
		}
	}
}

// Q4 through the real lifecycle.HelperRuntime and helper.Client: a helper
// that isn't there is FAILED HELPER_UNAVAILABLE; one that takes the whole
// request and never answers is UNKNOWN NO_ANSWER.
func TestOutcomeThroughRealHelperClient(t *testing.T) {
	dir := t.TempDir()
	rt := lifecycle.HelperRuntime{Client: helper.Client{SocketPath: filepath.Join(dir, "missing.sock")}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if o, c := outcomeOf(rt.Start(ctx, srv1, op1)); o != failed || c != unavailable {
		t.Errorf("no helper: %v %v, want FAILED HELPER_UNAVAILABLE", o, c)
	}

	path := filepath.Join(dir, "silent.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = helper.ReadFrame(conn, new(helperv1.Request))
		time.Sleep(2 * time.Second)
	}()
	rt = lifecycle.HelperRuntime{Client: helper.Client{SocketPath: path}}
	ctx, cancel = context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if o, c := outcomeOf(rt.Stop(ctx, srv1, op1)); o != unknown || c != noAnswer {
		t.Errorf("silent helper: %v %v, want UNKNOWN NO_ANSWER", o, c)
	}
}

// §5 steps 1 to 3: what a session accepts.
func TestCommandsAccept(t *testing.T) {
	c := newCommands(3, []string{srv1, srv2, srv3, srv4, srv5})
	bad := map[string]*agentv1.Command{
		"other epoch":    command(2, op1, srv1, start),
		"bad op ID":      command(3, "nope", srv1, start),
		"bad server ID":  command(3, op1, "0199E1A0-0000-7000-8000-000000000A01", start),
		"no action":      command(3, op1, srv1, agentv1.CommandAction_COMMAND_ACTION_UNSPECIFIED),
		"unknown action": command(3, op1, srv1, agentv1.CommandAction(99)),
	}
	for name, cmd := range bad {
		if _, err := c.accept(cmd); !errors.Is(err, errProtocol) {
			t.Errorf("%s: %v, want a protocol error", name, err)
		}
	}

	// F14: outside the inventory is not an error, just not known.
	if known, err := c.accept(command(3, op1, srv6, start)); known || err != nil {
		t.Errorf("unknown server: %v %v", known, err)
	}
	for i, op := range []string{op1, op2, op3, op4} {
		if known, err := c.accept(command(3, op, []string{srv1, srv2, srv3, srv4}[i], stopAction)); !known || err != nil {
			t.Fatalf("command %d: %v %v", i+1, known, err)
		}
	}
	if _, err := c.accept(command(3, op5, srv5, start)); !errors.Is(err, errProtocol) {
		t.Errorf("fifth command: %v, want a protocol error", err)
	}
	c.done(op4)
	if _, err := c.accept(command(3, op1, srv5, start)); !errors.Is(err, errProtocol) {
		t.Errorf("same operation twice: %v, want a protocol error", err)
	}
	if _, err := c.accept(command(3, op5, srv1, start)); !errors.Is(err, errProtocol) {
		t.Errorf("busy server: %v, want a protocol error", err)
	}
	if known, err := c.accept(command(3, op5, srv5, start)); !known || err != nil {
		t.Errorf("after a slot freed: %v %v", known, err)
	}
}

type sentMsgs struct {
	mu   sync.Mutex
	msgs []*agentv1.ConnectRequest
	ok   bool
}

func (s *sentMsgs) send(m *agentv1.ConnectRequest) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msgs = append(s.msgs, m)
	return s.ok
}

// describe turns progress and results into short strings.
func describe(msgs []*agentv1.ConnectRequest) []string {
	var out []string
	for _, m := range msgs {
		switch {
		case m.GetProgress() != nil:
			out = append(out, "progress "+m.GetProgress().GetPhase().String())
		case m.GetResult() != nil:
			out = append(out, fmt.Sprintf("result %s %s", m.GetResult().GetOutcome(), m.GetResult().GetCode()))
		}
	}
	return out
}

// Q2: the agent's call deadline outlasts the helper's own connection
// limit by 10 s, so the helper always answers first, even with a refusal.
func TestCallTimeouts(t *testing.T) {
	if helper.ConnTimeout != 90*time.Second {
		t.Errorf("helper.ConnTimeout = %s, want 90 s", helper.ConnTimeout)
	}
	if commandCallTimeout != helper.ConnTimeout+10*time.Second {
		t.Errorf("commandCallTimeout = %s, want ConnTimeout + 10 s", commandCallTimeout)
	}
}

// Restart is STOP, then Progress(STARTING), then START; a failed stop
// never starts. Every helper call gets the 100 s command deadline.
func TestExecute(t *testing.T) {
	failOn := func(which string) func(string, string) error {
		return func(action, _ string) error {
			if action == which {
				return fmt.Errorf("%w: systemctl said no", lifecycle.ErrFailed)
			}
			return nil
		}
	}
	cases := []struct {
		name   string
		action agentv1.CommandAction
		fail   func(string, string) error
		calls  []string
		msgs   []string
	}{
		{"start", start, nil, []string{"start " + srv1}, []string{"result RESULT_OUTCOME_SUCCEEDED RESULT_CODE_UNSPECIFIED"}},
		{"stop", stopAction, nil, []string{"stop " + srv1}, []string{"result RESULT_OUTCOME_SUCCEEDED RESULT_CODE_UNSPECIFIED"}},
		{"restart", restart, nil, []string{"stop " + srv1, "start " + srv1},
			[]string{"progress OPERATION_PHASE_STARTING", "result RESULT_OUTCOME_SUCCEEDED RESULT_CODE_UNSPECIFIED"}},
		{"restart, stop fails", restart, failOn("stop"), []string{"stop " + srv1},
			[]string{"result RESULT_OUTCOME_FAILED RESULT_CODE_ACTION_FAILED"}},
		{"restart, start fails", restart, failOn("start"), []string{"stop " + srv1, "start " + srv1},
			[]string{"progress OPERATION_PHASE_STARTING", "result RESULT_OUTCOME_FAILED RESULT_CODE_ACTION_FAILED"}},
	}
	for _, c := range cases {
		r := &fakeRunner{fail: c.fail}
		a := testAgent(t, &fakeHelper{}, time.Second, time.Second)
		a.runner = r
		cmds := newCommands(3, []string{srv1})
		cmd := command(3, op1, srv1, c.action)
		if known, err := cmds.accept(cmd); !known || err != nil {
			t.Fatal(known, err)
		}
		s := &sentMsgs{ok: true}
		var heldAtResult []int
		a.execute(cmd, cmds, func(m *agentv1.ConnectRequest) bool {
			if m.GetResult() != nil {
				cmds.mu.Lock()
				heldAtResult = append(heldAtResult, len(cmds.inflight))
				cmds.mu.Unlock()
			}
			return s.send(m)
		})
		if got := r.got(); !slices.Equal(got, c.calls) {
			t.Errorf("%s: calls %v, want %v", c.name, got, c.calls)
		}
		if got := describe(s.msgs); !slices.Equal(got, c.msgs) {
			t.Errorf("%s: sent %v, want %v", c.name, got, c.msgs)
		}
		if len(r.deadlines) != len(c.calls) {
			t.Errorf("%s: %d of %d helper calls had a deadline", c.name, len(r.deadlines), len(c.calls))
		}
		for _, d := range r.deadlines {
			if d < 99*time.Second || d > commandCallTimeout {
				t.Errorf("%s: helper call deadline %s, want 100 s", c.name, d)
			}
		}
		// The slot is free before the result goes out, so the controller's
		// next command for this server is never taken as a duplicate.
		if !slices.Equal(heldAtResult, []int{0}) {
			t.Errorf("%s: commands in flight when the result was sent: %v, want [0]", c.name, heldAtResult)
		}
		for _, m := range s.msgs {
			if e := m.GetResult().GetEpoch() + m.GetProgress().GetEpoch(); e != 3 {
				t.Errorf("%s: message with epoch %d", c.name, e)
			}
		}
	}
}

// F3 to F5: once the session is over, the command still runs to the end,
// but nothing it produces is delivered.
func TestExecuteAfterSessionEnded(t *testing.T) {
	r := &fakeRunner{}
	a := testAgent(t, &fakeHelper{}, time.Second, time.Second)
	a.runner = r
	cmds := newCommands(3, []string{srv1})
	cmd := command(3, op1, srv1, restart)
	cmds.accept(cmd)
	s := &sentMsgs{ok: false}
	a.execute(cmd, cmds, s.send)
	if got := r.got(); len(got) != 2 {
		t.Errorf("calls %v, want the whole restart", got)
	}
}

// ------------------------------------------------- against a fake controller

// fakeController is a scripted controller: it answers the hello, then
// runs script with the stream and the agent's messages.
type fakeController struct {
	agentv1.UnimplementedAgentLinkServiceServer
	servers []string
	script  func(stream agentv1.AgentLinkService_ConnectServer, in <-chan *agentv1.ConnectRequest) error
}

func (f *fakeController) Connect(stream agentv1.AgentLinkService_ConnectServer) error {
	if _, err := stream.Recv(); err != nil {
		return err
	}
	err := stream.Send(&agentv1.ConnectResponse{Msg: &agentv1.ConnectResponse_HelloReply{HelloReply: &agentv1.HelloReply{
		Epoch: 3, ServerIds: f.servers, ReportIntervalSeconds: 3600,
	}}})
	if err != nil {
		return err
	}
	in := make(chan *agentv1.ConnectRequest, 64)
	go func() {
		defer close(in)
		for {
			m, err := stream.Recv()
			if err != nil {
				return
			}
			in <- m
		}
	}()
	return f.script(stream, in)
}

// runAgainst runs a real agent session against f and returns the error
// the session ended with.
func runAgainst(t *testing.T, f *fakeController, r Runner) error {
	t.Helper()
	path := filepath.Join(t.TempDir(), "c.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.Creds(insecure.NewCredentials()))
	agentv1.RegisterAgentLinkServiceServer(srv, f)
	go srv.Serve(l)
	t.Cleanup(srv.Stop)

	a, err := New(Config{
		ControllerSocket: path,
		NodeID:           node,
		Helper:           &fakeHelper{answer: func(context.Context, string) (*helperv1.Response, error) { return running(""), nil }},
		Runner:           r,
		Prober:           fakeProber{},
		UIDOf:            func(net.Conn) (uint32, error) { return 0, nil },
		Log:              log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = a.session(ctx)
	return err
}

func sendCommand(stream agentv1.AgentLinkService_ConnectServer, cmd *agentv1.Command) error {
	return stream.Send(&agentv1.ConnectResponse{Msg: &agentv1.ConnectResponse_Command{Command: cmd}})
}

// nextResult skips reports and returns the next progress or result.
func nextResult(in <-chan *agentv1.ConnectRequest) (*agentv1.ConnectRequest, error) {
	timeout := time.After(5 * time.Second)
	for {
		select {
		case m, ok := <-in:
			if !ok {
				return nil, errors.New("stream ended")
			}
			if m.GetReport() == nil {
				return m, nil
			}
		case <-timeout:
			return nil, errors.New("no result")
		}
	}
}

var errDone = status.Error(codes.Canceled, "script done")

// F14 over a real stream: a command for a server outside the hello's
// inventory gets FAILED UNKNOWN_SERVER and the runner is never called.
// Commands for known servers run, and each result carries the session's
// epoch.
func TestSessionRunsCommands(t *testing.T) {
	r := &fakeRunner{}
	var results []string
	f := &fakeController{servers: []string{srv1, srv2}, script: func(stream agentv1.AgentLinkService_ConnectServer, in <-chan *agentv1.ConnectRequest) error {
		for _, cmd := range []*agentv1.Command{command(3, op1, srv7, start), command(3, op2, srv1, restart), command(3, op3, srv2, stopAction)} {
			if err := sendCommand(stream, cmd); err != nil {
				return err
			}
			for {
				m, err := nextResult(in)
				if err != nil {
					return err
				}
				if m.GetResult().GetEpoch()+m.GetProgress().GetEpoch() != 3 {
					return fmt.Errorf("wrong epoch in %v", m)
				}
				results = append(results, describe([]*agentv1.ConnectRequest{m})[0]+" "+m.GetResult().GetOperationId()+m.GetProgress().GetOperationId())
				if m.GetResult() != nil {
					break
				}
			}
		}
		return errDone
	}}
	if err := runAgainst(t, f, r); status.Code(err) != codes.Canceled {
		t.Fatalf("session ended with %v", err)
	}
	want := []string{
		"result RESULT_OUTCOME_FAILED RESULT_CODE_UNKNOWN_SERVER " + op1,
		"progress OPERATION_PHASE_STARTING " + op2,
		"result RESULT_OUTCOME_SUCCEEDED RESULT_CODE_UNSPECIFIED " + op2,
		"result RESULT_OUTCOME_SUCCEEDED RESULT_CODE_UNSPECIFIED " + op3,
	}
	if !slices.Equal(results, want) {
		t.Errorf("results:\n%v\nwant\n%v", results, want)
	}
	if got := r.got(); !slices.Equal(got, []string{"stop " + srv1, "start " + srv1, "stop " + srv2}) {
		t.Errorf("runner calls %v", got)
	}
}

// Q3, Q6 on the agent's side: a fifth command in flight, a second
// command for a busy server, a command for another epoch, or anything
// that isn't a command ends the session.
func TestSessionProtocolErrors(t *testing.T) {
	servers := []string{srv1, srv2, srv3, srv4, srv5}
	cases := map[string][]*agentv1.Command{
		"fifth in flight": {command(3, op1, srv1, start), command(3, op2, srv2, start), command(3, op3, srv3, start),
			command(3, op4, srv4, start), command(3, op5, srv5, start)},
		"busy server": {command(3, op1, srv1, start), command(3, op2, srv1, stopAction)},
		"same op":     {command(3, op1, srv1, start), command(3, op1, srv2, start)},
		"other epoch": {command(4, op1, srv1, start)},
		"no action":   {command(3, op1, srv1, agentv1.CommandAction_COMMAND_ACTION_UNSPECIFIED)},
		"unknown field": {func() *agentv1.Command {
			cmd := command(3, op1, srv1, start)
			cmd.ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, 99, protowire.VarintType), 1))
			return cmd
		}()},
		"hello reply": nil,
	}
	for name, cmds := range cases {
		r := &fakeRunner{gate: make(chan struct{})}
		ended := make(chan struct{})
		f := &fakeController{servers: servers, script: func(stream agentv1.AgentLinkService_ConnectServer, in <-chan *agentv1.ConnectRequest) error {
			if cmds == nil {
				_ = stream.Send(&agentv1.ConnectResponse{Msg: &agentv1.ConnectResponse_HelloReply{HelloReply: &agentv1.HelloReply{Epoch: 3}}})
			}
			for _, cmd := range cmds {
				if err := sendCommand(stream, cmd); err != nil {
					return err
				}
			}
			// The agent hangs up on us.
			for range in {
			}
			close(ended)
			return nil
		}}
		err := runAgainst(t, f, r)
		close(r.gate)
		if !errors.Is(err, errProtocol) {
			t.Errorf("%s: session ended with %v, want a protocol error", name, err)
		}
		select {
		case <-ended:
		case <-time.After(5 * time.Second):
			t.Errorf("%s: the stream stayed open", name)
		}
	}
}

// F5: a command still running when its session ends finishes, and its
// result is dropped. It never shows up on the next session.
func TestResultNeverCrossesSessions(t *testing.T) {
	r := &fakeRunner{gate: make(chan struct{}), entered: make(chan string, 4)}
	first := &fakeController{servers: []string{srv1}, script: func(stream agentv1.AgentLinkService_ConnectServer, in <-chan *agentv1.ConnectRequest) error {
		if err := sendCommand(stream, command(3, op1, srv1, start)); err != nil {
			return err
		}
		<-r.entered // the runner has it
		return status.Error(codes.Aborted, msgSessionReplaced)
	}}
	if err := runAgainst(t, first, r); !isLongWait(err) {
		t.Fatalf("first session: %v", err)
	}

	got := make(chan *agentv1.ConnectRequest, 8)
	second := &fakeController{servers: []string{srv1}, script: func(stream agentv1.AgentLinkService_ConnectServer, in <-chan *agentv1.ConnectRequest) error {
		close(r.gate) // the old command finishes now
		timeout := time.After(time.Second)
		for {
			select {
			case m, ok := <-in:
				if !ok {
					return nil
				}
				if m.GetReport() == nil {
					got <- m
				}
			case <-timeout:
				return errDone
			}
		}
	}}
	if err := runAgainst(t, second, r); status.Code(err) != codes.Canceled {
		t.Fatalf("second session: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("the old command's result reached the new session: %v", <-got)
	}
	if calls := r.got(); !slices.Equal(calls, []string{"start " + srv1}) {
		t.Errorf("runner calls %v, want the old start to have run once", calls)
	}
}
