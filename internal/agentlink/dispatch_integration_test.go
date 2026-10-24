// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package agentlink

// Stage 4c-1 (notes/stage4c-scope.md v2.1): claims, commands, results and
// progress over the real link, with real PostgreSQL. Raw gRPC clients
// play the agent where a test needs to misbehave; the real agent plays it
// everywhere else.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/sharddevs/waystone_panel/gen/waystone/agent/v1"
	"github.com/sharddevs/waystone_panel/internal/agent"
	"github.com/sharddevs/waystone_panel/internal/store"
)

var adminA = store.Actor{Type: store.ActorLocalUser, ID: "1000", Name: "admin a"}

// fakeRunner stands in for lifecycle.HelperRuntime under the real agent.
// gate, if set, holds every call; entered gets each call as it starts.
type fakeRunner struct {
	mu      sync.Mutex
	calls   []string
	gate    chan struct{}
	entered chan string
	fail    func(action, serverID string) error
}

func (f *fakeRunner) call(action, serverID string) error {
	f.mu.Lock()
	gate, entered, fail := f.gate, f.entered, f.fail
	f.mu.Unlock()
	if entered != nil {
		entered <- action + " " + serverID
	}
	if gate != nil {
		<-gate
	}
	f.mu.Lock()
	f.calls = append(f.calls, action+" "+serverID)
	f.mu.Unlock()
	if fail != nil {
		return fail(action, serverID)
	}
	return nil
}

func (f *fakeRunner) Start(_ context.Context, serverID, _ string) error {
	return f.call("start", serverID)
}

func (f *fakeRunner) Stop(_ context.Context, serverID, _ string) error {
	return f.call("stop", serverID)
}

func (f *fakeRunner) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// createOp records an operation as admin A, as the API would. The API
// only accepts one while the node is reachable (V1), so the node is marked
// as seen first, whether or not an agent is connected yet.
func (e *env) createOp(t *testing.T, server string, action store.Action) string {
	t.Helper()
	mustExec(t, e.owner, `UPDATE waystone.nodes SET last_seen_at = now()
		WHERE node_id = (SELECT node_id FROM waystone.servers WHERE server_id = '`+server+`')`)
	var id string
	err := e.store.InTenantTx(context.Background(), adminA, func(ctx context.Context, tx *store.Tx) error {
		op, _, err := tx.CreateOperation(ctx, store.CreateOperationInput{ServerID: server, Action: action})
		id = op.ID
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// freshOp clears whatever the server has unfinished (as the owner) and
// records a new operation for it.
func (e *env) freshOp(t *testing.T, server string, action store.Action) string {
	t.Helper()
	mustExec(t, e.owner, `UPDATE waystone.operations SET state = 'failed'
		WHERE resource_id = $1 AND state IN ('pending', 'running')`, server)
	mustExec(t, e.owner, `UPDATE waystone.operations SET resolved_at = now(), resolved_outcome = 'failed'
		WHERE resource_id = $1 AND state = 'uncertain' AND resolved_at IS NULL`, server)
	return e.createOp(t, server, action)
}

type opRow struct{ state, phase, code, summary string }

func (e *env) op(t *testing.T, id string) opRow {
	t.Helper()
	var r opRow
	if err := e.owner.QueryRow(context.Background(), `
		SELECT state, coalesce(phase, ''), coalesce(error_code, ''), coalesce(error_summary, '')
		FROM waystone.operations WHERE operation_id = $1`, id).Scan(&r.state, &r.phase, &r.code, &r.summary); err != nil {
		t.Fatal(err)
	}
	return r
}

// addServers puts n more servers on node A and returns their IDs.
func (e *env) addServers(t *testing.T, n int) []string {
	t.Helper()
	var out []string
	for i := range n {
		id := fmt.Sprintf("0199e1a0-0000-7000-8000-000000000c%02d", i)
		mustExec(t, e.owner, `INSERT INTO waystone.servers (tenant_id, server_id, node_id, name, profile, runtime)
			VALUES ($1, $2, $3, $4, 'minecraft-java', 'systemd')`, tenantA, id, nodeA, "extra "+id[len(id)-2:])
		out = append(out, id)
	}
	return out
}

// recv reads the stream's messages on a goroutine, so a test can wait
// for a command with a timeout, or check that none comes.
type recv struct {
	msgs chan *agentv1.ConnectResponse
	err  chan error
}

func receive(s agentv1.AgentLinkService_ConnectClient) *recv {
	r := &recv{msgs: make(chan *agentv1.ConnectResponse, 64), err: make(chan error, 1)}
	go func() {
		for {
			m, err := s.Recv()
			if err != nil {
				r.err <- err
				return
			}
			r.msgs <- m
		}
	}()
	return r
}

func (r *recv) command(t *testing.T) *agentv1.Command {
	t.Helper()
	select {
	case m := <-r.msgs:
		if m.GetCommand() == nil {
			t.Fatalf("got %v, want a command", m)
		}
		return m.GetCommand()
	case err := <-r.err:
		t.Fatalf("stream ended waiting for a command: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("no command")
	}
	return nil
}

// none checks that nothing arrives for a while.
func (r *recv) none(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case m := <-r.msgs:
		t.Fatalf("got %v, want nothing", m)
	case <-time.After(d):
	}
}

// statusOf is a stream's end as a code and message; a clean end is OK.
func statusOf(err error) (codes.Code, string) {
	if errors.Is(err, io.EOF) {
		return codes.OK, ""
	}
	st, _ := status.FromError(err)
	return st.Code(), st.Message()
}

// closed waits for the stream's end and checks its status.
func (r *recv) closed(t *testing.T, name string, code codes.Code, msg string) {
	t.Helper()
	for {
		select {
		case <-r.msgs:
			continue
		case err := <-r.err:
			c, m := statusOf(err)
			if c != code || m != msg {
				t.Errorf("%s: stream ended with %v %q, want %v %q", name, c, m, code, msg)
			}
			return
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: stream still open", name)
		}
	}
}

// session says hello as node A on a new stream. All of an env's
// sessions share one connection, because the link allows only eight.
func (e *env) session(t *testing.T) (agentv1.AgentLinkService_ConnectClient, int64, *recv) {
	t.Helper()
	if e.cc == nil {
		e.cc = e.client(t)
	}
	s := e.stream(t, e.cc)
	reply := hello(t, s)
	return s, reply.GetEpoch(), receive(s)
}

func resultMsg(epoch int64, op string, outcome agentv1.ResultOutcome, code agentv1.ResultCode) *agentv1.ConnectRequest {
	return &agentv1.ConnectRequest{Msg: &agentv1.ConnectRequest_Result{Result: &agentv1.Result{
		Epoch: epoch, OperationId: op, Outcome: outcome, Code: code}}}
}

func progressMsg(epoch int64, op string) *agentv1.ConnectRequest {
	return &agentv1.ConnectRequest{Msg: &agentv1.ConnectRequest_Progress{Progress: &agentv1.Progress{
		Epoch: epoch, OperationId: op, Phase: agentv1.OperationPhase_OPERATION_PHASE_STARTING}}}
}

var (
	outSucceeded = agentv1.ResultOutcome_RESULT_OUTCOME_SUCCEEDED
	outFailed    = agentv1.ResultOutcome_RESULT_OUTCOME_FAILED
	outUnknown   = agentv1.ResultOutcome_RESULT_OUTCOME_UNKNOWN
	codeNone     = agentv1.ResultCode_RESULT_CODE_UNSPECIFIED
)

func mustSend(t *testing.T, s agentv1.AgentLinkService_ConnectClient, m *agentv1.ConnectRequest) {
	t.Helper()
	if err := s.Send(m); err != nil {
		t.Fatal(err)
	}
}

// ------------------------------------------------------------ the basics

// I9, Q7: an operation waiting at hello time is claimed and sent right
// after it; one created later is sent on a kick; one created without a
// kick goes out after the next report. Each is claimed under the session
// and finished by its result (C3).
func TestCommandsFlow(t *testing.T) {
	e := setup(t, nil)
	first := e.createOp(t, serverA1, store.ActionStart)
	s, epoch, r := e.session(t)

	cmd := r.command(t)
	if cmd.GetEpoch() != epoch || cmd.GetOperationId() != first || cmd.GetServerId() != serverA1 ||
		cmd.GetAction() != agentv1.CommandAction_COMMAND_ACTION_START {
		t.Fatalf("command = %v", cmd)
	}
	if got := e.op(t, first); got.state != "running" {
		t.Errorf("sent op = %+v, want running", got)
	}
	mustSend(t, s, resultMsg(epoch, first, outSucceeded, codeNone))
	waitFor(t, "succeeded", func() bool { return e.op(t, first).state == "succeeded" })

	// A kick.
	second := e.createOp(t, serverA2, store.ActionStop)
	r.none(t, 200*time.Millisecond)
	e.srv.Kick(nodeA)
	if cmd := r.command(t); cmd.GetOperationId() != second || cmd.GetAction() != agentv1.CommandAction_COMMAND_ACTION_STOP {
		t.Fatalf("after kick: %v", cmd)
	}
	// A kick with nothing pending sends nothing.
	e.srv.Kick(nodeA)
	r.none(t, 200*time.Millisecond)

	// A kick for another node doesn't reach this session; the next
	// report sends what's pending.
	third := e.createOp(t, serverA1, store.ActionRestart)
	e.srv.Kick(nodeB)
	r.none(t, 200*time.Millisecond)
	mustSend(t, s, report(epoch, 1))
	if cmd := r.command(t); cmd.GetOperationId() != third || cmd.GetAction() != agentv1.CommandAction_COMMAND_ACTION_RESTART {
		t.Fatalf("after report: %v", cmd)
	}
	if got := e.op(t, third); got.phase != "stopping" {
		t.Errorf("restart claimed with phase %q", got.phase)
	}
}

// Decision L: every code becomes the controller's own fixed code and
// summary. UNKNOWN NO_ANSWER makes the operation uncertain.
func TestResultCodes(t *testing.T) {
	e := setup(t, nil)
	s, epoch, r := e.session(t)
	cases := []struct {
		outcome agentv1.ResultOutcome
		code    agentv1.ResultCode
		state   string
		errCode string
	}{
		{outSucceeded, codeNone, "succeeded", ""},
		{outFailed, agentv1.ResultCode_RESULT_CODE_NOT_FOUND, "failed", "not_found"},
		{outFailed, agentv1.ResultCode_RESULT_CODE_PERMISSION_DENIED, "failed", "permission_denied"},
		{outFailed, agentv1.ResultCode_RESULT_CODE_INVALID_REQUEST, "failed", "invalid_request"},
		{outFailed, agentv1.ResultCode_RESULT_CODE_ACTION_FAILED, "failed", "action_failed"},
		{outFailed, agentv1.ResultCode_RESULT_CODE_HELPER_UNAVAILABLE, "failed", "helper_unavailable"},
		{outFailed, agentv1.ResultCode_RESULT_CODE_UNKNOWN_SERVER, "failed", "unknown_server"},
		{outFailed, agentv1.ResultCode_RESULT_CODE_SERVER_BUSY, "failed", "server_busy"},
		{outUnknown, agentv1.ResultCode_RESULT_CODE_NO_ANSWER, "uncertain", "no_answer"},
	}
	for _, c := range cases {
		id := e.freshOp(t, serverA1, store.ActionStart)
		e.srv.Kick(nodeA)
		if cmd := r.command(t); cmd.GetOperationId() != id {
			t.Fatalf("command %v, want %s", cmd, id)
		}
		mustSend(t, s, resultMsg(epoch, id, c.outcome, c.code))
		waitFor(t, c.state, func() bool { return e.op(t, id).state == c.state })
		got := e.op(t, id)
		if got.code != c.errCode || (c.errCode == "") != (got.summary == "") {
			t.Errorf("%v %v: row %+v, want code %q", c.outcome, c.code, got, c.errCode)
		}
	}
}

// Q6 and decision L: a result or progress for anything this session
// didn't send, or a result whose outcome and code don't belong together,
// ends the stream. The operation stays running until the next hello.
func TestBadResultsEndTheStream(t *testing.T) {
	e := setup(t, nil)
	cases := []struct {
		name   string
		action store.Action
		msgs   func(epoch int64, op string) []*agentv1.ConnectRequest
		code   codes.Code
		reason string
	}{
		{"never sent", store.ActionStart, func(epoch int64, _ string) []*agentv1.ConnectRequest {
			return []*agentv1.ConnectRequest{resultMsg(epoch, "0199e1a0-0000-7000-8000-00000000dead", outSucceeded, codeNone)}
		}, codes.InvalidArgument, "unknown_operation"},
		{"other epoch", store.ActionStart, func(epoch int64, op string) []*agentv1.ConnectRequest {
			return []*agentv1.ConnectRequest{resultMsg(epoch-1, op, outSucceeded, codeNone)}
		}, codes.InvalidArgument, "unknown_operation"},
		{"replayed", store.ActionStart, func(epoch int64, op string) []*agentv1.ConnectRequest {
			r := resultMsg(epoch, op, outSucceeded, codeNone)
			return []*agentv1.ConnectRequest{r, r}
		}, codes.InvalidArgument, "unknown_operation"},
		{"succeeded with a code", store.ActionStart, func(epoch int64, op string) []*agentv1.ConnectRequest {
			return []*agentv1.ConnectRequest{resultMsg(epoch, op, outSucceeded, agentv1.ResultCode_RESULT_CODE_ACTION_FAILED)}
		}, codes.InvalidArgument, "invalid_result"},
		{"failed without a code", store.ActionStart, func(epoch int64, op string) []*agentv1.ConnectRequest {
			return []*agentv1.ConnectRequest{resultMsg(epoch, op, outFailed, codeNone)}
		}, codes.InvalidArgument, "invalid_result"},
		{"failed with no_answer", store.ActionStart, func(epoch int64, op string) []*agentv1.ConnectRequest {
			return []*agentv1.ConnectRequest{resultMsg(epoch, op, outFailed, agentv1.ResultCode_RESULT_CODE_NO_ANSWER)}
		}, codes.InvalidArgument, "invalid_result"},
		{"unknown with another code", store.ActionStart, func(epoch int64, op string) []*agentv1.ConnectRequest {
			return []*agentv1.ConnectRequest{resultMsg(epoch, op, outUnknown, agentv1.ResultCode_RESULT_CODE_ACTION_FAILED)}
		}, codes.InvalidArgument, "invalid_result"},
		{"unknown outcome number", store.ActionStart, func(epoch int64, op string) []*agentv1.ConnectRequest {
			return []*agentv1.ConnectRequest{resultMsg(epoch, op, agentv1.ResultOutcome(9), codeNone)}
		}, codes.InvalidArgument, "invalid_result"},
		{"unknown code number", store.ActionStart, func(epoch int64, op string) []*agentv1.ConnectRequest {
			return []*agentv1.ConnectRequest{resultMsg(epoch, op, outFailed, agentv1.ResultCode(99))}
		}, codes.InvalidArgument, "invalid_result"},
		{"progress on a start", store.ActionStart, func(epoch int64, op string) []*agentv1.ConnectRequest {
			return []*agentv1.ConnectRequest{progressMsg(epoch, op)}
		}, codes.InvalidArgument, "invalid_progress"},
		{"progress twice", store.ActionRestart, func(epoch int64, op string) []*agentv1.ConnectRequest {
			return []*agentv1.ConnectRequest{progressMsg(epoch, op), progressMsg(epoch, op)}
		}, codes.InvalidArgument, "invalid_progress"},
		{"progress without a phase", store.ActionRestart, func(epoch int64, op string) []*agentv1.ConnectRequest {
			return []*agentv1.ConnectRequest{{Msg: &agentv1.ConnectRequest_Progress{Progress: &agentv1.Progress{Epoch: epoch, OperationId: op}}}}
		}, codes.InvalidArgument, "invalid_progress"},
		{"progress other epoch", store.ActionRestart, func(epoch int64, op string) []*agentv1.ConnectRequest {
			return []*agentv1.ConnectRequest{progressMsg(epoch-1, op)}
		}, codes.InvalidArgument, "unknown_operation"},
		{"progress never sent", store.ActionRestart, func(epoch int64, _ string) []*agentv1.ConnectRequest {
			return []*agentv1.ConnectRequest{progressMsg(epoch, "0199e1a0-0000-7000-8000-00000000dead")}
		}, codes.InvalidArgument, "unknown_operation"},
	}
	for _, c := range cases {
		id := e.freshOp(t, serverA1, c.action)
		s, epoch, r := e.session(t)
		if cmd := r.command(t); cmd.GetOperationId() != id {
			t.Fatalf("%s: command %v", c.name, cmd)
		}
		msgs := c.msgs(epoch, id)
		for _, m := range msgs {
			mustSend(t, s, m)
		}
		r.closed(t, c.name, c.code, c.reason)
		want := "running"
		if c.name == "replayed" {
			want = "succeeded"
		}
		if got := e.op(t, id); got.state != want {
			t.Errorf("%s: op %+v, want %s", c.name, got, want)
		}
	}
	// The next hello makes the last one uncertain, as I8 says.
	_, _, _ = e.session(t)
	var n int
	if err := e.owner.QueryRow(context.Background(),
		`SELECT count(*) FROM waystone.operations WHERE state = 'running'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("%d operations still running after a hello (%v)", n, err)
	}
}

// C3 for the claim over the link: a hello this link never saw bumped the
// epoch. The session's next claim fails the epoch check, commits nothing,
// and the agent gets ABORTED session_replaced.
func TestSupersededClaim(t *testing.T) {
	e := setup(t, nil)
	_, _, r := e.session(t)
	r.none(t, 100*time.Millisecond)
	mustExec(t, e.owner, `UPDATE waystone.nodes SET session_epoch = session_epoch + 1 WHERE node_id = $1`, nodeA)
	id := e.createOp(t, serverA1, store.ActionStart)
	e.srv.Kick(nodeA)
	r.closed(t, "superseded claim", codes.Aborted, MsgSessionReplaced)
	if got := e.op(t, id); got.state != "pending" {
		t.Errorf("op = %+v, want still pending", got)
	}
}

// C3 for the result over the link: a hello this link never saw bumped
// the epoch while a command was out. Its result commits nothing, and the
// agent gets ABORTED session_replaced.
func TestSupersededResult(t *testing.T) {
	e := setup(t, nil)
	id := e.createOp(t, serverA1, store.ActionStart)
	s, epoch, r := e.session(t)
	r.command(t)
	mustExec(t, e.owner, `UPDATE waystone.nodes SET session_epoch = session_epoch + 1 WHERE node_id = $1`, nodeA)
	mustSend(t, s, resultMsg(epoch, id, outSucceeded, codeNone))
	r.closed(t, "superseded result", codes.Aborted, MsgSessionReplaced)
	if got := e.op(t, id); got.state != "running" {
		t.Errorf("op = %+v, want still running", got)
	}
}

// Restart: phase stopping, then starting after the Progress, and the
// result keeps it.
func TestRestartPhases(t *testing.T) {
	e := setup(t, nil)
	id := e.createOp(t, serverA1, store.ActionRestart)
	s, epoch, r := e.session(t)
	r.command(t)
	if got := e.op(t, id); got.phase != "stopping" {
		t.Fatalf("phase %q", got.phase)
	}
	mustSend(t, s, progressMsg(epoch, id))
	waitFor(t, "phase starting", func() bool { return e.op(t, id).phase == "starting" })
	mustSend(t, s, resultMsg(epoch, id, outSucceeded, codeNone))
	waitFor(t, "succeeded", func() bool { return e.op(t, id).state == "succeeded" })
	if got := e.op(t, id); got.phase != "starting" {
		t.Errorf("phase after the result: %q", got.phase)
	}
}

// F10 over the link: the server was deleted, or moved to another node,
// after the operation was created. The operation fails and nothing is
// sent.
func TestGoneServerNeverSent(t *testing.T) {
	e := setup(t, nil)
	gone := e.createOp(t, serverA1, store.ActionStart)
	moved := e.createOp(t, serverA2, store.ActionStart)
	mustExec(t, e.owner, `INSERT INTO waystone.nodes (tenant_id, node_id, name) VALUES ($1, '0199e1a0-0000-7000-8000-0000000000a2', 'a2')`, tenantA)
	mustExec(t, e.owner, `DELETE FROM waystone.servers WHERE server_id = $1`, serverA1)
	mustExec(t, e.owner, `UPDATE waystone.servers SET node_id = '0199e1a0-0000-7000-8000-0000000000a2' WHERE server_id = $1`, serverA2)
	_, _, r := e.session(t)
	r.none(t, 300*time.Millisecond)
	if got := e.op(t, gone); got.state != "failed" || got.code != "not_found" {
		t.Errorf("deleted server's op: %+v", got)
	}
	if got := e.op(t, moved); got.state != "failed" || got.code != "reassigned" {
		t.Errorf("moved server's op: %+v", got)
	}
}

// ------------------------------------------------------------ S2 slots

// S2, the kick storm: ten pending operations, fifty concurrent kicks and
// a stream of reports. Never more than four commands out at once, and
// every operation sent exactly once.
func TestKickStorm(t *testing.T) {
	e := setup(t, nil)
	servers := e.addServers(t, 8)
	servers = append(servers, serverA1, serverA2)
	want := map[string]bool{}
	for _, srv := range servers {
		want[e.createOp(t, srv, store.ActionStart)] = true
	}
	s, epoch, r := e.session(t)

	var seq atomic.Int64
	storm := func() {
		var wg sync.WaitGroup
		for range 50 {
			wg.Go(func() { e.srv.Kick(nodeA) })
		}
		wg.Wait()
	}
	sent := map[string]int{}
	outstanding := []string{}
	for len(sent) < len(want) || len(outstanding) > 0 {
		storm()
		mustSend(t, s, report(epoch, seq.Add(1)))
		// Take everything that arrives within a moment.
		deadline := time.After(300 * time.Millisecond)
	drain:
		for {
			select {
			case m := <-r.msgs:
				id := m.GetCommand().GetOperationId()
				sent[id]++
				outstanding = append(outstanding, id)
			case err := <-r.err:
				t.Fatalf("stream ended: %v", err)
			case <-deadline:
				break drain
			}
		}
		if len(outstanding) > MaxInFlight {
			t.Fatalf("%d commands out at once, at most %d allowed", len(outstanding), MaxInFlight)
		}
		if len(outstanding) == 0 {
			t.Fatalf("nothing outstanding, %d of %d sent", len(sent), len(want))
		}
		// Finish one; a slot frees.
		mustSend(t, s, resultMsg(epoch, outstanding[0], outSucceeded, codeNone))
		outstanding = outstanding[1:]
		if len(sent) == len(want) && len(outstanding) == 0 {
			break
		}
	}
	for id, n := range sent {
		if !want[id] || n != 1 {
			t.Errorf("%s sent %d times", id, n)
		}
	}
	for id := range want {
		waitFor(t, "all succeeded", func() bool { return e.op(t, id).state == "succeeded" })
	}
}

// S2: a result frees a slot and claims the next pending operation by
// itself, without a report or a kick.
func TestResultFreesSlot(t *testing.T) {
	e := setup(t, nil)
	servers := append(e.addServers(t, 3), serverA1, serverA2)
	var ops []string
	for _, srv := range servers {
		ops = append(ops, e.createOp(t, srv, store.ActionStart))
	}
	s, epoch, r := e.session(t)
	var sent []string
	for range MaxInFlight {
		sent = append(sent, r.command(t).GetOperationId())
	}
	r.none(t, 200*time.Millisecond)
	mustSend(t, s, resultMsg(epoch, sent[0], outSucceeded, codeNone))
	last := r.command(t).GetOperationId()
	if slices.Contains(sent, last) || !slices.Contains(ops, last) {
		t.Errorf("after a result: %s, already sent %v", last, sent)
	}
}

// S2: a claim transaction that fails releases its slot and commits
// nothing; the next signal claims normally.
func TestFailedClaimReleasesSlot(t *testing.T) {
	var broken atomic.Bool
	broken.Store(true)
	e := setup(t, func(c *Config) {
		c.TestBeforeClaim = func() error {
			if broken.Load() {
				return errors.New("injected claim failure")
			}
			return nil
		}
	})
	ids := []string{e.createOp(t, serverA1, store.ActionStart), e.createOp(t, serverA2, store.ActionStart)}
	s, epoch, r := e.session(t)
	r.none(t, 300*time.Millisecond)
	for _, id := range ids {
		if got := e.op(t, id); got.state != "pending" {
			t.Fatalf("op after a failed claim: %+v", got)
		}
	}
	if !strings.Contains(e.logs.String(), "agent_dispatch=claim_failed") {
		t.Error("the failed claim wasn't logged")
	}
	broken.Store(false)
	mustSend(t, s, report(epoch, 1))
	got := []string{r.command(t).GetOperationId(), r.command(t).GetOperationId()}
	slices.Sort(got)
	slices.Sort(ids)
	if !slices.Equal(got, ids) {
		t.Errorf("sent %v, want %v", got, ids)
	}
}

// S2, I9: a Send that fails leaves the operation running and it is never
// sent again. After the reconnect it's uncertain and the new session gets
// nothing for it.
func TestFailedSendNeverResent(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	e := setup(t, func(c *Config) {
		c.TestAfterClaim = func(store.ClaimedOperation) error {
			if fail.Load() {
				return errors.New("injected send failure")
			}
			return nil
		}
	})
	id := e.createOp(t, serverA1, store.ActionStart)
	_, _, r := e.session(t)
	r.closed(t, "failed send", codes.Unavailable, "send_failed")
	if got := e.op(t, id); got.state != "running" {
		t.Fatalf("op after the failed send: %+v", got)
	}
	fail.Store(false)
	s, epoch, r := e.session(t)
	if got := e.op(t, id); got.state != "uncertain" || got.code != "session_replaced" {
		t.Errorf("op after the reconnect: %+v", got)
	}
	mustSend(t, s, report(epoch, 1))
	r.none(t, 300*time.Millisecond)
}

// S2: a stream that ends with four in flight leaks no slot into the next
// session, which starts from zero.
func TestStreamEndDropsSlots(t *testing.T) {
	e := setup(t, nil)
	servers := append(e.addServers(t, 3), serverA1, serverA2)
	for _, srv := range servers[:4] {
		e.createOp(t, srv, store.ActionStart)
	}
	s, _, r := e.session(t)
	for range 4 {
		r.command(t)
	}
	_ = s.CloseSend()
	r.closed(t, "client closed", codes.OK, "")

	last := e.createOp(t, servers[4], store.ActionStart)
	_, _, r = e.session(t)
	if cmd := r.command(t); cmd.GetOperationId() != last {
		t.Fatalf("new session's first command: %v", cmd)
	}
}

// ------------------------------------------------- crash points (F1–F5)

// startLink runs another link (a "new controller") on the same database.
func (e *env) startLink(t *testing.T, mod func(*Config)) (*Server, string) {
	t.Helper()
	cfg := Config{Store: e.store, NodeID: nodeA, AgentUID: uint32(os.Getuid()), Version: "test", Log: log.New(e.logs, "", 0)}
	if mod != nil {
		mod(&cfg)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "agent2.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan struct{})
	go func() {
		defer close(served)
		srv.Serve(l)
	}()
	t.Cleanup(func() {
		srv.CancelSessions()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Stop(ctx)
		<-served
	})
	return srv, path
}

func stopLink(srv *Server) {
	srv.CancelSessions()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Stop(ctx)
}

// runRealAgent runs the real agent against path with r as its runner.
func runRealAgent(t *testing.T, path string, r agent.Runner, h agent.Helper) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	a, err := agent.New(agent.Config{
		ControllerSocket: path,
		ControllerUID:    uint32(os.Getuid()),
		NodeID:           nodeA,
		Version:          "test",
		Helper:           h,
		Runner:           r,
		Prober:           fakeProber{},
		Log:              log.New(io.Discard, "", 0),
		Sleep: func(ctx context.Context, _ time.Duration) error {
			// Reconnect quickly, but not in a tight loop.
			select {
			case <-time.After(50 * time.Millisecond):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.Run(ctx)
	}()
	t.Cleanup(func() { cancel(); <-done })
	return cancel
}

// F1: the operation is created while no agent is connected; the
// controller stops; a new controller starts on the same database; the
// agent connects to it, and the operation runs and succeeds.
func TestF1OperationWaitsForAgent(t *testing.T) {
	e := setup(t, nil)
	id := e.createOp(t, serverA1, store.ActionStart)
	stopLink(e.srv)
	if n, err := e.store.RecoverRunningOperations(context.Background()); err != nil || n != 0 {
		t.Fatalf("recovery = %d, %v", n, err)
	}
	_, path := e.startLink(t, nil)
	r := &fakeRunner{}
	runRealAgent(t, path, r, &fakeHelper{})
	waitFor(t, "succeeded", func() bool { return e.op(t, id).state == "succeeded" })
	if got := r.got(); !slices.Equal(got, []string{"start " + serverA1}) {
		t.Errorf("runner calls %v", got)
	}
}

// F2: the controller dies after the claim commits and before the Send.
// The startup recovery makes the operation uncertain, controller_restarted,
// and the reconnected agent never receives it.
func TestF2DiesBetweenClaimAndSend(t *testing.T) {
	claimed := make(chan struct{}, 1)
	e := setup(t, func(c *Config) {
		c.TestAfterClaim = func(store.ClaimedOperation) error {
			claimed <- struct{}{}
			return errors.New("the controller died here")
		}
	})
	id := e.createOp(t, serverA1, store.ActionStart)
	e.session(t)
	<-claimed
	stopLink(e.srv)

	if n, err := e.store.RecoverRunningOperations(context.Background()); err != nil || n != 1 {
		t.Fatalf("recovery = %d, %v; want 1", n, err)
	}
	if got := e.op(t, id); got.state != "uncertain" || got.code != "controller_restarted" {
		t.Fatalf("op after recovery: %+v", got)
	}
	_, path := e.startLink(t, nil)
	r := &fakeRunner{}
	runRealAgent(t, path, r, &fakeHelper{})
	time.Sleep(500 * time.Millisecond)
	if got := r.got(); len(got) != 0 {
		t.Errorf("the reconnected agent ran %v", got)
	}
	if got := e.op(t, id); got.state != "uncertain" || got.code != "controller_restarted" {
		t.Errorf("op later: %+v", got)
	}
}

// F3 and F4: the agent's session is cut while its runner is blocked
// before (F3) or after (F4) the helper call. The reconnect makes the
// operation uncertain; the old command's result never lands.
func TestF3F4SessionCutMidCommand(t *testing.T) {
	for _, after := range []bool{false, true} {
		name := map[bool]string{false: "F3", true: "F4"}[after]
		t.Run(name, func(t *testing.T) {
			e := setup(t, nil)
			gate := make(chan struct{})
			entered := make(chan string, 4)
			r := &fakeRunner{entered: entered}
			if after {
				// F4: the "helper call" happened; the result is held back.
				r.fail = func(string, string) error { <-gate; return nil }
			} else {
				r.gate = gate
			}
			id := e.createOp(t, serverA1, store.ActionStart)
			runRealAgent(t, e.path, r, &fakeHelper{})
			<-entered
			if got := e.op(t, id); got.state != "running" {
				t.Fatalf("op = %+v", got)
			}
			before := e.node(t).epoch
			// Cut the session: the agent reconnects with a new hello.
			stopLink(e.srv)
			_, path := e.startLink(t, nil)
			if got := e.op(t, id); got.state != "running" {
				t.Fatalf("op before any reconnect = %+v", got)
			}
			if !after && len(r.got()) != 0 {
				t.Fatalf("F3: the runner recorded %v before its call", r.got())
			}
			// The agent needs the new socket; a second agent stands in
			// for the reconnect, as the real one would after its backoff.
			runRealAgent(t, path, &fakeRunner{}, &fakeHelper{})
			waitFor(t, "uncertain", func() bool { return e.op(t, id).state == "uncertain" })
			if e.node(t).epoch <= before {
				t.Error("no new session")
			}
			close(gate)
			time.Sleep(300 * time.Millisecond)
			if got := e.op(t, id); got.state != "uncertain" || got.code != "session_replaced" {
				t.Errorf("op after the old command finished: %+v", got)
			}
		})
	}
}

// F5 with a raw client: after a takeover, a result carrying the old epoch
// on the new stream is refused, and changes nothing.
func TestF5OldEpochResultRefused(t *testing.T) {
	e := setup(t, nil)
	id := e.createOp(t, serverA1, store.ActionStart)
	_, oldEpoch, r1 := e.session(t)
	r1.command(t)
	s2, _, r2 := e.session(t)
	r1.closed(t, "old session", codes.Aborted, MsgSessionReplaced)
	mustSend(t, s2, resultMsg(oldEpoch, id, outSucceeded, codeNone))
	r2.closed(t, "old-epoch result", codes.InvalidArgument, "unknown_operation")
	if got := e.op(t, id); got.state != "uncertain" || got.code != "session_replaced" {
		t.Errorf("op = %+v", got)
	}
}

// F14 with the real agent: a server added after the hello is in the
// database but not in the agent's inventory. Its command comes back
// FAILED UNKNOWN_SERVER and the runner is never called.
func TestF14UnknownServer(t *testing.T) {
	e := setup(t, nil)
	r := &fakeRunner{}
	runRealAgent(t, e.path, r, &fakeHelper{})
	waitFor(t, "session", func() bool { return e.srv.sessions.epoch(nodeA) > 0 })
	late := e.addServers(t, 1)[0]
	id := e.createOp(t, late, store.ActionStart)
	e.srv.Kick(nodeA)
	waitFor(t, "failed", func() bool { return e.op(t, id).state == "failed" })
	if got := e.op(t, id); got.code != "unknown_server" {
		t.Errorf("op = %+v", got)
	}
	if got := r.got(); len(got) != 0 {
		t.Errorf("runner called: %v", got)
	}
}
