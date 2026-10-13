// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package agentlink

// Stage 4b-2 tests over a real stream (notes/stage4b2-scope.md §7).

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	agentv1 "github.com/sharddevs/waystone_panel/gen/waystone/agent/v1"
)

const (
	running = agentv1.ProcessState_PROCESS_STATE_RUNNING
	stopped = agentv1.ProcessState_PROCESS_STATE_STOPPED
)

// The happy path, and R11: last_seen_at moves on the hello and on an
// accepted report, never on a refused one.
func TestHelloAndReports(t *testing.T) {
	e := setup(t, nil)
	s := e.stream(t, e.client(t))
	reply := hello(t, s)
	if reply.GetEpoch() != 1 || strings.Join(reply.GetServerIds(), ",") != serverA1+","+serverA2 ||
		reply.GetReportIntervalSeconds() != 15 {
		t.Fatalf("hello reply = %v", reply)
	}
	afterHello := e.node(t)
	if afterHello.epoch != 1 || afterHello.lastSeen == nil {
		t.Fatalf("node after hello = %+v", afterHello)
	}

	if err := s.Send(report(1, 1, entry(serverA1, running), entry(serverA2, stopped))); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "seq 1 stored", func() bool { _, seq := e.serverState(t, serverA2); return seq == 1 })
	if st, _ := e.serverState(t, serverA1); st != "running" {
		t.Errorf("a1 = %s, want running", st)
	}
	afterReport := e.node(t)
	if !afterReport.lastSeen.After(*afterHello.lastSeen) {
		t.Errorf("last_seen_at %v didn't move past %v on an accepted report", afterReport.lastSeen, afterHello.lastSeen)
	}

	// A zero-entry report is the heartbeat: accepted, and it moves
	// last_seen_at too.
	if err := s.Send(report(1, 2)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "heartbeat", func() bool { return e.node(t).lastSeen.After(*afterReport.lastSeen) })
	beforeRefusal := e.node(t)
	cols := e.reportCols(t)

	// A refused report closes the stream and touches nothing.
	if err := s.Send(report(1, 2, entry(serverA1, stopped))); err != nil {
		t.Fatal(err)
	}
	wantClosed(t, "repeated seq", s, codes.InvalidArgument, "seq_not_increasing")
	if n := e.node(t); n.epoch != 1 || !n.lastSeen.Equal(*beforeRefusal.lastSeen) {
		t.Errorf("node after a refused report = %+v, want %+v", n, beforeRefusal)
	}
	if got := e.reportCols(t); got != cols {
		t.Errorf("report columns changed by a refused report:\n%s\n%s", cols, got)
	}
}

// R3/F8: a Hello for another node is refused, and nothing changes.
func TestWrongNode(t *testing.T) {
	e := setup(t, nil)
	for _, node := range []string{nodeB, strings.ToUpper(nodeA), "", "not-a-uuid"} {
		s := e.stream(t, e.client(t))
		if err := s.Send(helloMsg(node)); err != nil {
			t.Fatal(err)
		}
		wantClosed(t, "node "+node, s, codes.FailedPrecondition, "node_mismatch")
	}
	if n := e.node(t); n.epoch != 0 || n.lastSeen != nil {
		t.Errorf("node A after refused hellos = %+v", n)
	}
	var epochB int64
	if err := e.owner.QueryRow(context.Background(),
		`SELECT session_epoch FROM waystone.nodes WHERE node_id = $1`, nodeB).Scan(&epochB); err != nil {
		t.Fatal(err)
	}
	if epochB != 0 {
		t.Errorf("node B's epoch = %d, want 0", epochB)
	}
}

// R2/F7 over a real socket: a peer with the wrong uid never gets a
// stream, and the database never hears of it.
func TestWrongPeerUID(t *testing.T) {
	e := setup(t, func(c *Config) {
		c.UIDOf = func(net.Conn) (uint32, error) { return 4242, nil }
	})
	s, err := e.client(t).Connect(t.Context())
	if err == nil {
		err = s.Send(helloMsg(nodeA))
		if err == nil {
			_, err = s.Recv()
		}
	}
	if status.Code(err) != codes.Unavailable {
		t.Errorf("wrong uid: err = %v, want UNAVAILABLE", err)
	}
	if n := e.node(t); n.epoch != 0 || n.lastSeen != nil {
		t.Errorf("node after a refused peer = %+v", n)
	}
	waitFor(t, "refusal logged", func() bool { return strings.Contains(e.logs.String(), "agent_link=refused_peer uid=4242") })
}

// unknownField returns m with field 99 added at the level pick chooses.
func unknownField(m *agentv1.ConnectRequest, pick func(*agentv1.ConnectRequest) protoreflect.ProtoMessage) *agentv1.ConnectRequest {
	r := pick(m).ProtoReflect()
	r.SetUnknown(protowire.AppendVarint(protowire.AppendTag(r.GetUnknown(), 99, protowire.VarintType), 1))
	return m
}

// R5/R12: every whole-report rule, each on its own session. Each closes
// the stream with its code and leaves the database exactly as it was.
func TestReportRules(t *testing.T) {
	e := setup(t, nil)
	many := make([]*agentv1.ReportEntry, 257)
	for i := range many {
		many[i] = entry(serverA1, running)
	}
	top := func(m *agentv1.ConnectRequest) protoreflect.ProtoMessage { return m }
	nested := func(m *agentv1.ConnectRequest) protoreflect.ProtoMessage { return m.GetReport().GetEntries()[0] }

	cases := []struct {
		name   string
		before bool // send it before the Hello instead of after
		msg    func(epoch int64) *agentv1.ConnectRequest
		code   codes.Code
		reason string
	}{
		{"stale epoch", false, func(ep int64) *agentv1.ConnectRequest { return report(ep-1, 5, entry(serverA1, running)) }, codes.InvalidArgument, "wrong_epoch"},
		{"future epoch", false, func(ep int64) *agentv1.ConnectRequest { return report(ep+1, 5, entry(serverA1, running)) }, codes.InvalidArgument, "wrong_epoch"},
		{"repeated seq", false, func(ep int64) *agentv1.ConnectRequest { return report(ep, 1, entry(serverA1, running)) }, codes.InvalidArgument, "seq_not_increasing"},
		{"257 entries", false, func(ep int64) *agentv1.ConnectRequest { return report(ep, 5, many...) }, codes.InvalidArgument, "invalid_report"},
		{"duplicate server", false, func(ep int64) *agentv1.ConnectRequest {
			return report(ep, 5, entry(serverA1, running), entry(serverA1, stopped))
		}, codes.InvalidArgument, "invalid_report"},
		{"bad UUID", false, func(ep int64) *agentv1.ConnectRequest { return report(ep, 5, entry("a1", running)) }, codes.InvalidArgument, "invalid_report"},
		{"unknown enum", false, func(ep int64) *agentv1.ConnectRequest { return report(ep, 5, entry(serverA1, 99)) }, codes.InvalidArgument, "invalid_report"},
		{"unspecified", false, func(ep int64) *agentv1.ConnectRequest {
			return report(ep, 5, entry(serverA1, agentv1.ProcessState_PROCESS_STATE_UNSPECIFIED))
		}, codes.InvalidArgument, "invalid_report"},
		{"33-byte native_state", false, func(ep int64) *agentv1.ConnectRequest {
			en := entry(serverA1, running)
			en.NativeState = strings.Repeat("x", 33)
			return report(ep, 5, en)
		}, codes.InvalidArgument, "invalid_report"},
		{"no agent time", false, func(ep int64) *agentv1.ConnectRequest {
			m := report(ep, 5, entry(serverA1, running))
			m.GetReport().AgentTime = nil
			return m
		}, codes.InvalidArgument, "invalid_report"},
		{"unknown field", false, func(ep int64) *agentv1.ConnectRequest {
			return unknownField(report(ep, 5, entry(serverA1, running)), top)
		}, codes.InvalidArgument, "unknown_field"},
		{"unknown field, nested", false, func(ep int64) *agentv1.ConnectRequest {
			return unknownField(report(ep, 5, entry(serverA1, running)), nested)
		}, codes.InvalidArgument, "unknown_field"},
		{"second hello", false, func(int64) *agentv1.ConnectRequest { return helloMsg(nodeA) }, codes.FailedPrecondition, "unexpected_hello"},
		{"no message", false, func(int64) *agentv1.ConnectRequest { return &agentv1.ConnectRequest{} }, codes.InvalidArgument, "unknown_message"},
		{"report before hello", true, func(int64) *agentv1.ConnectRequest { return report(1, 1, entry(serverA1, running)) }, codes.FailedPrecondition, "hello_required"},
		{"nothing before hello", true, func(int64) *agentv1.ConnectRequest { return &agentv1.ConnectRequest{} }, codes.InvalidArgument, "unknown_message"},
		{"unknown field in hello", true, func(int64) *agentv1.ConnectRequest {
			return unknownField(helloMsg(nodeA), func(m *agentv1.ConnectRequest) protoreflect.ProtoMessage { return m.GetHello() })
		}, codes.InvalidArgument, "unknown_field"},
	}
	c := e.client(t)
	// One session first, so the first case's epoch is 2 and "stale" (1)
	// is a real old epoch rather than an invalid 0.
	hello(t, e.stream(t, c))
	for _, tc := range cases {
		s := e.stream(t, c)
		var epoch int64
		if !tc.before {
			epoch = hello(t, s).GetEpoch()
			// A good report first, so "repeated seq" has something to
			// repeat and the baseline includes a real report.
			if err := s.Send(report(epoch, 1, entry(serverA2, stopped))); err != nil {
				t.Fatal(err)
			}
			waitFor(t, tc.name+": baseline", func() bool {
				var reportEpoch int64
				if err := e.owner.QueryRow(context.Background(),
					`SELECT report_epoch FROM waystone.servers WHERE server_id = $1`, serverA2).Scan(&reportEpoch); err != nil {
					t.Fatal(err)
				}
				return reportEpoch == epoch
			})
		}
		node, cols := e.node(t), e.reportCols(t)
		if err := s.Send(tc.msg(epoch)); err != nil {
			t.Fatalf("%s: send: %v", tc.name, err)
		}
		wantClosed(t, tc.name, s, tc.code, tc.reason)
		after := e.node(t)
		if after.epoch != node.epoch || (after.lastSeen == nil) != (node.lastSeen == nil) ||
			(after.lastSeen != nil && !after.lastSeen.Equal(*node.lastSeen)) {
			t.Errorf("%s: node changed: %+v → %+v", tc.name, node, after)
		}
		if got := e.reportCols(t); got != cols {
			t.Errorf("%s: report columns changed", tc.name)
		}
	}
}

// R7: 256 servers fit in a HelloReply, 257 refuse the hello, and a
// message over 64 KiB is refused by gRPC itself.
func TestCapacity(t *testing.T) {
	e := setup(t, nil)
	// a1, a2 and 254 more: 256.
	mustExec(t, e.owner, `INSERT INTO waystone.servers (tenant_id, server_id, node_id, name, profile, runtime)
		SELECT $1, ('0199e1a0-0000-7000-8000-' || lpad(to_hex(4096 + g), 12, '0'))::uuid, $2, 'bulk ' || g, 'minecraft-java', 'systemd'
		FROM generate_series(1, 254) g`, tenantA, nodeA)
	c := e.client(t)
	if got := len(hello(t, e.stream(t, c)).GetServerIds()); got != 256 {
		t.Errorf("256 servers: hello reply has %d", got)
	}
	mustExec(t, e.owner, `INSERT INTO waystone.servers (tenant_id, server_id, node_id, name, profile, runtime)
		VALUES ($1, '0199e1a0-0000-7000-8000-00000000ffff', $2, 'one too many', 'minecraft-java', 'systemd')`, tenantA, nodeA)
	before := e.node(t)
	s := e.stream(t, c)
	if err := s.Send(helloMsg(nodeA)); err != nil {
		t.Fatal(err)
	}
	wantClosed(t, "257 servers", s, codes.FailedPrecondition, "too_many_servers")
	if n := e.node(t); n.epoch != before.epoch {
		t.Errorf("257 servers: epoch %d → %d", before.epoch, n.epoch)
	}

	// 65 KiB: the agent version is only logged, so it's the easy field
	// to make big.
	big := helloMsg(nodeA)
	big.GetHello().AgentVersion = strings.Repeat("v", 65<<10)
	if proto.Size(big) <= MaxMessageBytes {
		t.Fatal("test message isn't over the limit")
	}
	s = e.stream(t, c)
	if err := s.Send(big); err != nil {
		t.Fatal(err)
	}
	_, err := s.Recv()
	if status.Code(err) != codes.ResourceExhausted {
		t.Errorf("65 KiB message: err = %v, want RESOURCE_EXHAUSTED", err)
	}
	if n := e.node(t); n.epoch != before.epoch {
		t.Errorf("65 KiB message: epoch %d → %d", before.epoch, n.epoch)
	}
}

// R8: a stream that doesn't say hello in time is closed.
func TestHelloDeadline(t *testing.T) {
	e := setup(t, func(c *Config) { c.HelloTimeout = 200 * time.Millisecond })
	s := e.stream(t, e.client(t))
	start := time.Now()
	wantClosed(t, "silent stream", s, codes.DeadlineExceeded, "hello_timeout")
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("closed after %s", took)
	}
	if n := e.node(t); n.epoch != 0 {
		t.Errorf("epoch = %d after a silent stream", n.epoch)
	}
}

// Requirement 1, I8/F6: a second hello for the node takes over, and the
// first agent sees exactly ABORTED session_replaced on its own stream.
// Its late report can't land.
func TestTakeoverAbortsOldStream(t *testing.T) {
	e := setup(t, nil)
	first := e.stream(t, e.client(t))
	if ep := hello(t, first).GetEpoch(); ep != 1 {
		t.Fatalf("first epoch = %d", ep)
	}
	second := e.stream(t, e.client(t))
	if ep := hello(t, second).GetEpoch(); ep != 2 {
		t.Fatalf("second epoch = %d", ep)
	}
	_, err := first.Recv()
	st, _ := status.FromError(err)
	if st.Code() != codes.Aborted || st.Message() != MsgSessionReplaced {
		t.Fatalf("first stream ended with %v %q, want ABORTED %q", st.Code(), st.Message(), MsgSessionReplaced)
	}
	cols := e.reportCols(t)
	_ = first.Send(report(1, 1, entry(serverA1, running)))
	// The new session still works and the old report never landed.
	if err := second.Send(report(2, 1, entry(serverA2, stopped))); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "second session's report", func() bool { _, seq := e.serverState(t, serverA2); return seq == 1 })
	if st, seq := e.serverState(t, serverA1); st != "unknown" || seq != 0 {
		t.Errorf("a1 = %s seq %d: the replaced session's report landed (before: %s)", st, seq, cols)
	}
	if e.srv.sessions.epoch(nodeA) != 2 {
		t.Errorf("live epoch = %d, want 2", e.srv.sessions.epoch(nodeA))
	}
}

// C4 (requirement 3 through the real path): the 6th takeover inside the
// window writes exactly one flagged audit row as the node, the 7th
// doesn't add another.
func TestChurnAudit(t *testing.T) {
	e := setup(t, nil)
	c := e.client(t)
	auditRows := func() int {
		var n int
		if err := e.owner.QueryRow(context.Background(), `SELECT count(*) FROM waystone.audit_events`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	var prev agentv1.AgentLinkService_ConnectClient
	for i := 0; i <= 7; i++ { // the first hello isn't a takeover
		s := e.stream(t, c)
		hello(t, s)
		if prev != nil {
			wantClosed(t, "replaced", prev, codes.Aborted, MsgSessionReplaced)
		}
		prev = s
		want := 0
		if i >= 6 {
			want = 1
		}
		if got := auditRows(); got != want {
			t.Errorf("after %d takeovers: %d audit rows, want %d", i, got, want)
		}
	}
	var actorType, actorID, action, outcome, tenant string
	if err := e.owner.QueryRow(context.Background(),
		`SELECT actor_type, actor_id, action, outcome, tenant_id::text FROM waystone.audit_events`).Scan(
		&actorType, &actorID, &action, &outcome, &tenant); err != nil {
		t.Fatal(err)
	}
	if actorType != "node" || actorID != nodeA || action != "node.session_churn" || outcome != "flagged" || tenant != tenantA {
		t.Errorf("audit row = %s %s %s %s %s", actorType, actorID, action, outcome, tenant)
	}
	if !strings.Contains(e.logs.String(), "WARNING agent_session=churn") {
		t.Error("no churn warning logged")
	}
}

// R1 with the database: 50 hellos at once over one connection. Whatever
// order they reach the map in, the live session is the database's final
// epoch.
func TestConcurrentHellos(t *testing.T) {
	e := setup(t, nil)
	c := e.client(t)
	const n = 50
	var wg sync.WaitGroup
	for range n {
		s := e.stream(t, c)
		wg.Go(func() {
			if err := s.Send(helloMsg(nodeA)); err != nil {
				t.Error(err)
				return
			}
			// A reply or an ABORTED: either way this hello has been
			// through the map.
			s.Recv()
		})
	}
	wg.Wait()
	final := e.node(t).epoch
	if final != n {
		t.Fatalf("database epoch = %d, want %d", final, n)
	}
	if got := e.srv.sessions.epoch(nodeA); got != final {
		t.Errorf("live epoch = %d, database says %d", got, final)
	}
}

// R10: with a live session and a stream that hasn't said hello yet,
// shutdown finishes well inside its budget, and the agent hears
// UNAVAILABLE shutting_down.
func TestShutdownWithLiveStreams(t *testing.T) {
	e := setup(t, nil)
	c := e.client(t)
	live := e.stream(t, c)
	hello(t, live)
	silent := e.stream(t, c)
	_ = silent

	start := time.Now()
	e.srv.CancelSessions()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.srv.Stop(ctx); err != nil {
		t.Errorf("Stop = %v, want a graceful stop", err)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("shutdown took %s", took)
	}
	wantClosed(t, "live session", live, codes.Unavailable, MsgShuttingDown)
}

// Requirement 1, the database path: a newer hello that this controller's
// map never saw (say, from a restarted controller) bumps the epoch. The
// live session's next report fails the locked epoch check (C3), and the
// agent still gets exactly ABORTED session_replaced.
//
// Since 4c the session's dispatcher claims right after the hello and
// checks the same epoch, so it could meet the bump first. Claims are
// switched off here to keep this about the report; the claim's own path
// is TestSupersededClaim.
func TestSupersededInDatabase(t *testing.T) {
	e := setup(t, func(c *Config) {
		c.TestBeforeClaim = func() error { return errors.New("claims off in this test") }
	})
	s := e.stream(t, e.client(t))
	epoch := hello(t, s).GetEpoch()
	mustExec(t, e.owner, `UPDATE waystone.nodes SET session_epoch = session_epoch + 1 WHERE node_id = $1`, nodeA)
	cols := e.reportCols(t)
	if err := s.Send(report(epoch, 1, entry(serverA1, running))); err != nil {
		t.Fatal(err)
	}
	wantClosed(t, "superseded report", s, codes.Aborted, MsgSessionReplaced)
	if got := e.reportCols(t); got != cols {
		t.Error("a superseded report changed the servers")
	}
}
