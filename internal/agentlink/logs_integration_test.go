// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package agentlink

// Stage 7a: log reads through a live session. The test is the agent.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	agentv1 "github.com/sharddevs/waystone_panel/gen/waystone/agent/v1"
	"github.com/sharddevs/waystone_panel/internal/ids"
	"github.com/sharddevs/waystone_panel/internal/serverlog"
)

const logCursor = "s=0123456789abcdef0123456789abcdef;i=1a2;b=fedcba9876543210fedcba9876543210;m=9f;t=5f1e2d3c4b5a6;x=77aa"

type readResult struct {
	page serverlog.Page
	err  error
}

// read starts a ReadLog and returns where its result will arrive.
func (e *env) read(server, after string, limit int) chan readResult {
	out := make(chan readResult, 1)
	go func() {
		p, err := e.srv.ReadLog(context.Background(), nodeA, server, after, limit)
		out <- readResult{p, err}
	}()
	return out
}

// logRequest receives the next message, which must be a LogRequest.
func logRequest(t *testing.T, s agentv1.AgentLinkService_ConnectClient) *agentv1.LogRequest {
	t.Helper()
	m, err := s.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if m.GetLogRequest() == nil {
		t.Fatalf("got %v, want a log request", m)
	}
	return m.GetLogRequest()
}

func sendChunk(t *testing.T, s agentv1.AgentLinkService_ConnectClient, c *agentv1.LogChunk) {
	t.Helper()
	if err := s.Send(&agentv1.ConnectRequest{Msg: &agentv1.ConnectRequest_LogChunk{LogChunk: c}}); err != nil {
		t.Fatal(err)
	}
}

func result(t *testing.T, out chan readResult) readResult {
	t.Helper()
	select {
	case r := <-out:
		return r
	case <-time.After(10 * time.Second):
		t.Fatal("ReadLog never returned")
		return readResult{}
	}
}

// waitSession waits until node A's session takes reads.
func (e *env) waitSession(t *testing.T) {
	waitFor(t, "the session", func() bool { return e.srv.sessions.logReader(nodeA) != nil })
}

// fast leaves almost no gap between reads of one server, for tests that
// aren't about it.
func fast(c *Config) { c.LogGap = time.Millisecond }

func TestReadLog(t *testing.T) {
	e := setup(t, fast)
	s := e.stream(t, e.client(t))
	reply := hello(t, s)
	e.waitSession(t)

	out := e.read(serverA1, "", 200)
	req := logRequest(t, s)
	if req.GetEpoch() != reply.GetEpoch() || req.GetServerId() != serverA1 || req.GetAfterCursor() != "" || req.GetLimit() != 200 || ids.Check(req.GetRequestId()) != nil {
		t.Fatalf("request %v", req)
	}
	sendChunk(t, s, &agentv1.LogChunk{RequestId: req.GetRequestId(), Cursor: logCursor, More: true, Lines: []*agentv1.LogLine{
		{TimeUnixMicro: 1792843201000000, Priority: 6, Text: "[Server thread/INFO]: Done (3.2s)!"},
		{TimeUnixMicro: 1792843202000000, Priority: 3, Text: "tab\tok"},
	}})
	r := result(t, out)
	if r.err != nil {
		t.Fatal(r.err)
	}
	want := serverlog.Page{Cursor: logCursor, More: true, Lines: []serverlog.Line{
		{Time: time.UnixMicro(1792843201000000).UTC(), Priority: 6, Text: "[Server thread/INFO]: Done (3.2s)!"},
		{Time: time.UnixMicro(1792843202000000).UTC(), Priority: 3, Text: "tab\tok"},
	}}
	if len(r.page.Lines) != 2 || r.page.Lines[0] != want.Lines[0] || r.page.Lines[1] != want.Lines[1] || r.page.Cursor != want.Cursor || !r.page.More {
		t.Errorf("page %+v, want %+v", r.page, want)
	}
	// No line's text reaches the controller's log.
	if strings.Contains(e.logs.String(), "Done (3.2s)") {
		t.Error("a log line's text is in the controller's log")
	}

	// After a cursor with nothing new: no lines, and the same cursor back.
	out = e.read(serverA1, logCursor, 50)
	req = logRequest(t, s)
	if req.GetAfterCursor() != logCursor || req.GetLimit() != 50 {
		t.Fatalf("request %v", req)
	}
	sendChunk(t, s, &agentv1.LogChunk{RequestId: req.GetRequestId()})
	if r := result(t, out); r.err != nil || len(r.page.Lines) != 0 || r.page.Cursor != logCursor || r.page.More {
		t.Errorf("nothing new: %+v %v", r.page, r.err)
	}

	// NO_LOG is an empty page; UNAVAILABLE an error. The session goes on.
	out = e.read(serverA2, "", 10)
	sendChunk(t, s, &agentv1.LogChunk{RequestId: logRequest(t, s).GetRequestId(), Code: agentv1.LogCode_LOG_CODE_NO_LOG})
	if r := result(t, out); r.err != nil || len(r.page.Lines) != 0 || r.page.Cursor != "" {
		t.Errorf("no log: %+v %v", r.page, r.err)
	}
	out = e.read(serverA2, "", 10)
	sendChunk(t, s, &agentv1.LogChunk{RequestId: logRequest(t, s).GetRequestId(), Code: agentv1.LogCode_LOG_CODE_UNAVAILABLE})
	if r := result(t, out); !errors.Is(r.err, serverlog.ErrUnavailable) {
		t.Errorf("unavailable: %v", r.err)
	}
	if err := s.Send(report(reply.GetEpoch(), 1)); err != nil {
		t.Errorf("the session ended: %v", err)
	}
}

func TestReadLogWithoutSession(t *testing.T) {
	e := setup(t, fast)
	if _, err := e.srv.ReadLog(context.Background(), nodeA, serverA1, "", 10); !errors.Is(err, serverlog.ErrUnreachable) {
		t.Errorf("no session: %v", err)
	}
	s := e.stream(t, e.client(t))
	hello(t, s)
	e.waitSession(t)
	// Bad arguments are refused before anything is sent.
	for _, c := range []struct {
		server, after string
		limit         int
	}{{"x", "", 10}, {serverA1, "", 0}, {serverA1, "", 201}, {serverA1, "--since=today", 10}} {
		if _, err := e.srv.ReadLog(context.Background(), nodeA, c.server, c.after, c.limit); err == nil {
			t.Errorf("%+v: no error", c)
		}
	}
	// Nothing was sent for them: the next request the agent sees is this.
	out := e.read(serverA2, "", 7)
	if req := logRequest(t, s); req.GetServerId() != serverA2 || req.GetLimit() != 7 {
		t.Fatalf("a bad read was sent: %v", req)
	} else {
		sendChunk(t, s, &agentv1.LogChunk{RequestId: req.GetRequestId()})
	}
	result(t, out)
	// When the session ends, a read waiting on it ends too.
	out = e.read(serverA1, "", 10)
	logRequest(t, s)
	e.srv.Revoke(nodeA)
	if r := result(t, out); !errors.Is(r.err, serverlog.ErrUnreachable) {
		t.Errorf("session ended: %v", r.err)
	}
}

// A read nobody answers gives up after LogTimeout but keeps its place
// until its chunk comes: the agent still counts it. Four out is the most,
// and a server with one still out waits for it.
func TestReadLogTimeoutAndLimit(t *testing.T) {
	e := setup(t, func(c *Config) { c.LogTimeout = 500 * time.Millisecond; fast(c) })
	s := e.stream(t, e.client(t))
	reply := hello(t, s)
	e.waitSession(t)

	servers := []string{serverA1, serverA2, serverB1, "0199e1a0-0000-7000-8000-000000000a03"}
	var reqs []*agentv1.LogRequest
	for _, id := range servers {
		out := e.read(id, "", 10)
		reqs = append(reqs, logRequest(t, s))
		if r := result(t, out); !errors.Is(r.err, serverlog.ErrUnavailable) {
			t.Fatalf("unanswered: %v", r.err)
		}
	}
	if _, err := e.srv.ReadLog(context.Background(), nodeA, "0199e1a0-0000-7000-8000-000000000a04", "", 10); !errors.Is(err, serverlog.ErrBusy) {
		t.Fatalf("fifth: %v, want ErrBusy", err)
	}
	// serverA1's read is still out: another for it sends nothing and
	// gives up.
	if _, err := e.srv.ReadLog(context.Background(), nodeA, serverA1, "", 10); !errors.Is(err, serverlog.ErrUnavailable) {
		t.Fatalf("while one is out: %v, want ErrUnavailable", err)
	}
	// One for the same lines doesn't take the old answer: it waits, and
	// once the late chunk frees the place, it is sent and answered.
	out := e.read(serverA1, "", 10)
	time.Sleep(50 * time.Millisecond)
	sendChunk(t, s, &agentv1.LogChunk{RequestId: reqs[0].GetRequestId()})
	sendChunk(t, s, &agentv1.LogChunk{RequestId: logRequest(t, s).GetRequestId()})
	if r := result(t, out); r.err != nil {
		t.Errorf("after a late chunk: %v", r.err)
	}
	if err := s.Send(report(reply.GetEpoch(), 1, entry(serverA1, agentv1.ProcessState_PROCESS_STATE_RUNNING))); err != nil {
		t.Fatal(err)
	}
	// Three are still out: one more fits, a fifth doesn't.
	out = e.read(serverA1, "", 10)
	logRequest(t, s)
	result(t, out)
	if _, err := e.srv.ReadLog(context.Background(), nodeA, "0199e1a0-0000-7000-8000-000000000a04", "", 10); !errors.Is(err, serverlog.ErrBusy) {
		t.Errorf("four out again: %v, want ErrBusy", err)
	}
}

// H1: viewers of one server share its read; other reads of it wait their
// turn, LogGap apart, and a viewer going away doesn't end the read.
func TestReadLogShared(t *testing.T) {
	const gap = 300 * time.Millisecond
	e := setup(t, func(c *Config) { c.LogGap = gap })
	s := e.stream(t, e.client(t))
	hello(t, s)
	e.waitSession(t)

	ctx, cancel := context.WithCancel(context.Background())
	started := time.Now()
	first := make(chan readResult, 1)
	go func() {
		p, err := e.srv.ReadLog(ctx, nodeA, serverA1, "", 10)
		first <- readResult{p, err}
	}()
	req := logRequest(t, s)
	shared := e.read(serverA1, "", 10)
	other := e.read(serverA1, logCursor, 10)
	time.Sleep(100 * time.Millisecond)
	cancel()
	if r := result(t, first); !errors.Is(r.err, context.Canceled) {
		t.Fatalf("cancelled viewer: %v", r.err)
	}
	sendChunk(t, s, &agentv1.LogChunk{RequestId: req.GetRequestId(), Cursor: logCursor, Lines: []*agentv1.LogLine{{TimeUnixMicro: 1, Priority: 6, Text: "one"}}})
	if r := result(t, shared); r.err != nil || len(r.page.Lines) != 1 || r.page.Lines[0].Text != "one" {
		t.Fatalf("shared: %+v %v", r.page, r.err)
	}
	// The other read went out only after the first was answered, and not
	// before the gap.
	next := logRequest(t, s)
	if since := time.Since(started); since < gap {
		t.Errorf("second read after %v, want at least %v", since, gap)
	}
	if next.GetAfterCursor() != logCursor {
		t.Fatalf("second read %v", next)
	}
	sendChunk(t, s, &agentv1.LogChunk{RequestId: next.GetRequestId()})
	if r := result(t, other); r.err != nil || r.page.Cursor != logCursor {
		t.Errorf("other: %+v %v", r.page, r.err)
	}
	// Another server isn't held up by this one's gap.
	out := e.read(serverA2, "", 10)
	sendChunk(t, s, &agentv1.LogChunk{RequestId: logRequest(t, s).GetRequestId()})
	if r := result(t, out); r.err != nil {
		t.Errorf("another server: %v", r.err)
	}
}

func TestReadLogRefusesBadChunks(t *testing.T) {
	big := strings.Repeat("a", serverlog.MaxLineBytes)
	line := func(text string) *agentv1.LogLine { return &agentv1.LogLine{TimeUnixMicro: 1, Priority: 6, Text: text} }
	tooMuch := make([]*agentv1.LogLine, serverlog.MaxTextBytes/serverlog.MaxLineBytes+1)
	for i := range tooMuch {
		tooMuch[i] = line(big)
	}
	cases := map[string]func(id string) *agentv1.LogChunk{
		"unknown request": func(string) *agentv1.LogChunk { return &agentv1.LogChunk{RequestId: serverA2} },
		"over the limit": func(id string) *agentv1.LogChunk {
			return &agentv1.LogChunk{RequestId: id, Cursor: logCursor, Lines: []*agentv1.LogLine{line("1"), line("2"), line("3")}}
		},
		"escape": func(id string) *agentv1.LogChunk {
			return &agentv1.LogChunk{RequestId: id, Cursor: logCursor, Lines: []*agentv1.LogLine{line("\x1b[2J")}}
		},
		"bidi": func(id string) *agentv1.LogChunk {
			return &agentv1.LogChunk{RequestId: id, Cursor: logCursor, Lines: []*agentv1.LogLine{line("a\u202eb")}}
		},
		"long line": func(id string) *agentv1.LogChunk {
			return &agentv1.LogChunk{RequestId: id, Cursor: logCursor, Lines: []*agentv1.LogLine{line(big + "a")}}
		},
		"priority 8": func(id string) *agentv1.LogChunk {
			return &agentv1.LogChunk{RequestId: id, Cursor: logCursor, Lines: []*agentv1.LogLine{{Priority: 8, Text: "x"}}}
		},
		"negative time": func(id string) *agentv1.LogChunk {
			return &agentv1.LogChunk{RequestId: id, Cursor: logCursor, Lines: []*agentv1.LogLine{{TimeUnixMicro: -1, Text: "x"}}}
		},
		"no cursor": func(id string) *agentv1.LogChunk {
			return &agentv1.LogChunk{RequestId: id, Lines: []*agentv1.LogLine{line("x")}}
		},
		"bad cursor": func(id string) *agentv1.LogChunk {
			return &agentv1.LogChunk{RequestId: id, Cursor: "--since=x", Lines: []*agentv1.LogLine{line("x")}}
		},
		"cursor without lines": func(id string) *agentv1.LogChunk { return &agentv1.LogChunk{RequestId: id, Cursor: logCursor} },
		"more without lines":   func(id string) *agentv1.LogChunk { return &agentv1.LogChunk{RequestId: id, More: true} },
		"lines with an error": func(id string) *agentv1.LogChunk {
			return &agentv1.LogChunk{RequestId: id, Code: agentv1.LogCode_LOG_CODE_UNAVAILABLE, Lines: []*agentv1.LogLine{line("x")}}
		},
		"cursor with no log": func(id string) *agentv1.LogChunk {
			return &agentv1.LogChunk{RequestId: id, Code: agentv1.LogCode_LOG_CODE_NO_LOG, Cursor: logCursor}
		},
		"unknown code": func(id string) *agentv1.LogChunk { return &agentv1.LogChunk{RequestId: id, Code: 9} },
		"too much text": func(id string) *agentv1.LogChunk {
			return &agentv1.LogChunk{RequestId: id, Cursor: logCursor, Lines: tooMuch}
		},
	}
	for name, chunk := range cases {
		t.Run(name, func(t *testing.T) {
			e := setup(t, fast)
			s := e.stream(t, e.client(t))
			hello(t, s)
			e.waitSession(t)
			limit := 2
			if name == "too much text" {
				limit = 200
			}
			out := e.read(serverA1, "", limit)
			req := logRequest(t, s)
			sendChunk(t, s, chunk(req.GetRequestId()))
			reason := "invalid_log_chunk"
			if name == "unknown request" {
				reason = "unknown_log_request"
			}
			wantClosed(t, name, s, codes.InvalidArgument, reason)
			if r := result(t, out); r.err == nil {
				t.Errorf("read: %+v, want an error", r.page)
			}
		})
	}
}
