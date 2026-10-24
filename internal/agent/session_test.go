// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"

	agentv1 "github.com/sharddevs/waystone_panel/gen/waystone/agent/v1"
)

// R2: the agent's handshake refuses a listener that isn't the
// controller's uid or root (systemd made the socket), closing the
// connection without writing anything.
func TestClientHandshake(t *testing.T) {
	for name, c := range map[string]struct {
		uid  uint32
		err  error
		pass bool
	}{
		"controller":         {uid: 1000, pass: true},
		"root (socket unit)": {uid: 0, pass: true},
		"other user":         {uid: 1001},
		"agent's own uid":    {uid: 995},
		"no peercred":        {err: errors.New("not unix")},
	} {
		a, b := net.Pipe()
		conn := &writeCounter{Conn: a}
		creds := &clientCreds{controllerUID: 1000, uidOf: func(net.Conn) (uint32, error) { return c.uid, c.err }}
		got, _, err := creds.ClientHandshake(t.Context(), "x", conn)
		if (err == nil) != c.pass || (got != nil) != c.pass {
			t.Errorf("%s: handshake = %v, %v", name, got, err)
		}
		if conn.writes.Load() != 0 {
			t.Errorf("%s: wrote before the check", name)
		}
		if !c.pass {
			if _, err := b.Write([]byte{1}); err == nil {
				t.Errorf("%s: connection left open", name)
			}
		}
		a.Close()
		b.Close()
	}
}

type writeCounter struct {
	net.Conn
	writes atomic.Int64
}

func (c *writeCounter) Write(p []byte) (int, error) {
	c.writes.Add(1)
	return c.Conn.Write(p)
}

func helloReply(mutate func(*agentv1.HelloReply)) *agentv1.ConnectResponse {
	r := &agentv1.HelloReply{Epoch: 3, ServerIds: []string{srv1, srv2}, ReportIntervalSeconds: 15}
	if mutate != nil {
		mutate(r)
	}
	return &agentv1.ConnectResponse{Msg: &agentv1.ConnectResponse_HelloReply{HelloReply: r}}
}

// R12 and sanity checks on what the controller sends back.
func TestCheckHelloReply(t *testing.T) {
	if _, err := checkHelloReply(helloReply(nil)); err != nil {
		t.Fatalf("good reply: %v", err)
	}
	many := make([]string, 257)
	for i := range many {
		many[i] = fmt.Sprintf("%s%03d", srv1[:len(srv1)-3], i)
	}
	bad := map[string]*agentv1.ConnectResponse{
		"empty":         {},
		"no epoch":      helloReply(func(r *agentv1.HelloReply) { r.Epoch = 0 }),
		"no interval":   helloReply(func(r *agentv1.HelloReply) { r.ReportIntervalSeconds = 0 }),
		"huge interval": helloReply(func(r *agentv1.HelloReply) { r.ReportIntervalSeconds = 3601 }),
		"257 servers":   helloReply(func(r *agentv1.HelloReply) { r.ServerIds = many }),
		"bad id":        helloReply(func(r *agentv1.HelloReply) { r.ServerIds = []string{"../etc"} }),
		"repeated id":   helloReply(func(r *agentv1.HelloReply) { r.ServerIds = []string{srv1, srv1} }),
	}
	unknownField := helloReply(nil)
	unknownField.GetHelloReply().ProtoReflect().SetUnknown(
		protowire.AppendVarint(protowire.AppendTag(nil, 99, protowire.VarintType), 1))
	bad["unknown field"] = unknownField
	for name, m := range bad {
		if _, err := checkHelloReply(m); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// R6, 6a: only these exact statuses get the long wait.
func TestIsLongWait(t *testing.T) {
	for _, err := range []error{
		status.Error(codes.Aborted, "session_replaced"),
		status.Error(codes.PermissionDenied, "node_revoked"),
		status.Error(codes.PermissionDenied, "not_permitted"),
		status.Error(codes.FailedPrecondition, "version_mismatch"),
	} {
		if !isLongWait(err) {
			t.Errorf("%v not recognised", err)
		}
	}
	for _, err := range []error{
		nil,
		errors.New("session_replaced"),
		status.Error(codes.Aborted, "something else"),
		status.Error(codes.Unavailable, "session_replaced"),
		status.Error(codes.Unavailable, "shutting_down"),
		status.Error(codes.PermissionDenied, "something else"),
		status.Error(codes.Unavailable, "node_revoked"),
		status.Error(codes.FailedPrecondition, "node_mismatch"),
	} {
		if isLongWait(err) {
			t.Errorf("%v counted as a long wait", err)
		}
	}
}
