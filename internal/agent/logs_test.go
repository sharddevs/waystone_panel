// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"errors"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	agentv1 "github.com/sharddevs/waystone_panel/gen/waystone/agent/v1"
	helperv1 "github.com/sharddevs/waystone_panel/gen/waystone/helper/v1"
)

// Stage 7a: log reads on the agent's side.

const (
	req1   = "0199e1a0-0000-7000-8000-0000000d0001"
	cursor = "s=0123456789abcdef0123456789abcdef;i=1a2;b=fedcba9876543210fedcba9876543210;m=9f;t=5f1e2d3c4b5a6;x=77aa"
)

func logReq(server string, mutate func(*agentv1.LogRequest)) *agentv1.LogRequest {
	r := &agentv1.LogRequest{Epoch: 3, RequestId: req1, ServerId: server, Limit: 200}
	if mutate != nil {
		mutate(r)
	}
	return r
}

// readsAgent is an agent with h as its helper, and a deliver that
// collects what it sends.
func readsAgent(h Helper) (*Agent, *commands, func(*agentv1.ConnectRequest) bool, chan *agentv1.LogChunk) {
	a := &Agent{helper: h, log: log.New(io.Discard, "", 0)}
	cmds := newCommands(3, []string{srv1, srv2}, nil)
	out := make(chan *agentv1.LogChunk, 16)
	deliver := func(m *agentv1.ConnectRequest) bool { out <- m.GetLogChunk(); return true }
	return a, cmds, deliver, out
}

func next(t *testing.T, out chan *agentv1.LogChunk) *agentv1.LogChunk {
	t.Helper()
	select {
	case c := <-out:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("no chunk")
		return nil
	}
}

func TestLogReadPassesTheHelpersAnswer(t *testing.T) {
	h := &fakeHelper{answer: func(context.Context, string) (*helperv1.Response, error) {
		return &helperv1.Response{Log: &helperv1.LogResult{
			Lines:  []*helperv1.LogLine{{TimeUnixMicro: 7, Priority: 4, Text: "[Server thread/INFO]: Done"}},
			Cursor: cursor, More: true,
		}}, nil
	}}
	a, cmds, deliver, out := readsAgent(h)
	if err := a.logRequest(logReq(srv1, func(r *agentv1.LogRequest) { r.AfterCursor = cursor; r.Limit = 50 }), cmds, deliver); err != nil {
		t.Fatal(err)
	}
	got := next(t, out)
	want := &agentv1.LogChunk{RequestId: req1, Lines: []*agentv1.LogLine{{TimeUnixMicro: 7, Priority: 4, Text: "[Server thread/INFO]: Done"}}, Cursor: cursor, More: true}
	if !proto.Equal(got, want) {
		t.Errorf("chunk %v, want %v", got, want)
	}
	reqs := h.requests()
	wantReq := &helperv1.Request{OperationId: req1, ServerId: srv1, Action: helperv1.Action_ACTION_READ_LOG,
		Log: &helperv1.LogSpec{AfterCursor: cursor, Limit: 50}}
	if len(reqs) != 1 || !proto.Equal(reqs[0], wantReq) {
		t.Errorf("helper got %v, want %v", reqs, wantReq)
	}
	if cmds.reads != 0 {
		t.Errorf("%d reads still counted", cmds.reads)
	}
}

// A server outside the inventory was never installed here: NO_LOG, and
// the helper isn't asked.
func TestLogReadUnknownServer(t *testing.T) {
	h := &fakeHelper{answer: func(context.Context, string) (*helperv1.Response, error) { return &helperv1.Response{}, nil }}
	a, cmds, deliver, out := readsAgent(h)
	if err := a.logRequest(logReq(srv3, nil), cmds, deliver); err != nil {
		t.Fatal(err)
	}
	if got := next(t, out); got.GetCode() != agentv1.LogCode_LOG_CODE_NO_LOG || got.GetRequestId() != req1 {
		t.Errorf("chunk %v, want NO_LOG", got)
	}
	if len(h.requests()) != 0 || cmds.reads != 0 {
		t.Errorf("helper asked %d times, %d reads counted", len(h.requests()), cmds.reads)
	}
}

func TestLogReadFailures(t *testing.T) {
	big := strings.Repeat("a", 4096)
	lines := make([]*helperv1.LogLine, 16)
	for i := range lines {
		lines[i] = &helperv1.LogLine{Text: big}
	}
	cases := map[string]struct {
		resp *helperv1.Response
		err  error
		want agentv1.LogCode
	}{
		"unreachable": {err: errors.New("dial: no such file"), want: agentv1.LogCode_LOG_CODE_UNAVAILABLE},
		"failed":      {resp: &helperv1.Response{ErrorCode: helperv1.ErrorCode_ERROR_CODE_FAILED}, want: agentv1.LogCode_LOG_CODE_UNAVAILABLE},
		"refused":     {resp: &helperv1.Response{ErrorCode: helperv1.ErrorCode_ERROR_CODE_INVALID_REQUEST}, want: agentv1.LogCode_LOG_CODE_UNAVAILABLE},
		"not found":   {resp: &helperv1.Response{ErrorCode: helperv1.ErrorCode_ERROR_CODE_NOT_FOUND}, want: agentv1.LogCode_LOG_CODE_NO_LOG},
		"too large":   {resp: &helperv1.Response{Log: &helperv1.LogResult{Lines: lines, Cursor: cursor}}, want: agentv1.LogCode_LOG_CODE_UNAVAILABLE},
	}
	for name, c := range cases {
		h := &fakeHelper{answer: func(context.Context, string) (*helperv1.Response, error) { return c.resp, c.err }}
		a, cmds, deliver, out := readsAgent(h)
		if err := a.logRequest(logReq(srv1, nil), cmds, deliver); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got := next(t, out)
		want := &agentv1.LogChunk{RequestId: req1, Code: c.want}
		if !proto.Equal(got, want) {
			t.Errorf("%s: %v, want %v", name, got, want)
		}
	}
}

// The helper call has its own deadline, inside the controller's 5 s.
func TestLogReadDeadline(t *testing.T) {
	var left time.Duration
	h := &fakeHelper{answer: func(ctx context.Context, _ string) (*helperv1.Response, error) {
		d, _ := ctx.Deadline()
		left = time.Until(d)
		return &helperv1.Response{}, nil
	}}
	a, cmds, deliver, out := readsAgent(h)
	if err := a.logRequest(logReq(srv1, nil), cmds, deliver); err != nil {
		t.Fatal(err)
	}
	next(t, out)
	if left <= 3*time.Second || left > logCallTimeout {
		t.Errorf("deadline %s away, want just under %s", left, logCallTimeout)
	}
}

func TestLogRequestProtocolErrors(t *testing.T) {
	cases := map[string]*agentv1.LogRequest{
		"other epoch":    logReq(srv1, func(r *agentv1.LogRequest) { r.Epoch = 2 }),
		"bad request id": logReq(srv1, func(r *agentv1.LogRequest) { r.RequestId = "x" }),
		"bad server id":  logReq("../etc", nil),
		"limit 0":        logReq(srv1, func(r *agentv1.LogRequest) { r.Limit = 0 }),
		"limit 201":      logReq(srv1, func(r *agentv1.LogRequest) { r.Limit = 201 }),
		"bad cursor":     logReq(srv1, func(r *agentv1.LogRequest) { r.AfterCursor = "--since=today" }),
		"uppercase":      logReq(srv1, func(r *agentv1.LogRequest) { r.AfterCursor = "s=ABC" }),
	}
	for name, r := range cases {
		h := &fakeHelper{answer: func(context.Context, string) (*helperv1.Response, error) { return &helperv1.Response{}, nil }}
		a, cmds, deliver, _ := readsAgent(h)
		if err := a.logRequest(r, cmds, deliver); !errors.Is(err, errProtocol) {
			t.Errorf("%s: %v, want a protocol error", name, err)
		}
		if len(h.requests()) != 0 {
			t.Errorf("%s: helper asked", name)
		}
	}
}

// Four reads at once is the most; a fifth ends the session. A finished
// read frees its place before its chunk goes out.
func TestLogReadLimit(t *testing.T) {
	gate := make(chan struct{})
	h := &fakeHelper{answer: func(context.Context, string) (*helperv1.Response, error) {
		<-gate
		return &helperv1.Response{}, nil
	}}
	a, cmds, deliver, out := readsAgent(h)
	for range 4 {
		if err := a.logRequest(logReq(srv1, nil), cmds, deliver); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.logRequest(logReq(srv2, nil), cmds, deliver); !errors.Is(err, errProtocol) {
		t.Fatalf("fifth read: %v, want a protocol error", err)
	}
	gate <- struct{}{}
	next(t, out)
	if err := a.logRequest(logReq(srv2, nil), cmds, deliver); err != nil {
		t.Errorf("after one finished: %v", err)
	}
	close(gate)
	for range 4 {
		next(t, out)
	}
}

// Through the session's reader: a LogRequest is handled, not refused as
// an unexpected message.
func TestCommandTakesLogRequests(t *testing.T) {
	h := &fakeHelper{answer: func(context.Context, string) (*helperv1.Response, error) { return &helperv1.Response{}, nil }}
	a, cmds, deliver, out := readsAgent(h)
	m := &agentv1.ConnectResponse{Msg: &agentv1.ConnectResponse_LogRequest{LogRequest: logReq(srv1, nil)}}
	if err := a.command(m, cmds, deliver); err != nil {
		t.Fatal(err)
	}
	if got := next(t, out); got.GetRequestId() != req1 || got.GetCode() != agentv1.LogCode_LOG_CODE_UNSPECIFIED {
		t.Errorf("chunk %v", got)
	}
}
