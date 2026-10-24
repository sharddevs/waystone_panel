// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package agentlink

// Stage 7b (notes/stage7-scope.md D2): GetCommandSpec answers only for a
// server.command running on the calling node, for the server it names; a
// command reaches the agent as CONSOLE; and its two new result codes are
// recorded.

import (
	"context"
	"testing"

	agentv1 "github.com/sharddevs/waystone_panel/gen/waystone/agent/v1"
	"github.com/sharddevs/waystone_panel/internal/store"
)

// createCommand marks server running and records a console command for it.
func (e *env) createCommand(t *testing.T, server, line string) string {
	t.Helper()
	mustExec(t, e.owner, `UPDATE waystone.servers SET process_state = 'running', process_since = now(), process_observed_at = now()
		WHERE server_id = '`+server+`'`)
	mustExec(t, e.owner, `UPDATE waystone.nodes SET last_seen_at = now()
		WHERE node_id = (SELECT node_id FROM waystone.servers WHERE server_id = '`+server+`')`)
	var id string
	err := e.store.InTenantTx(context.Background(), adminA, func(ctx context.Context, tx *store.Tx) error {
		op, _, err := tx.CreateOperation(ctx, store.CreateOperationInput{ServerID: server, Action: store.ActionCommand, Command: line})
		id = op.ID
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestCommandSpec(t *testing.T) {
	e := setup(t, nil)
	c := e.client(t)
	s, epoch, r := e.session(t)

	op := e.createCommand(t, serverA1, "say hi é")
	// Pending, not yet claimed: nothing.
	_, err := c.GetCommandSpec(context.Background(), &agentv1.GetCommandSpecRequest{ServerId: serverA1, OperationId: op})
	wantNotPermitted(t, "pending", err)

	e.srv.Kick(nodeA)
	if cmd := r.command(t); cmd.GetOperationId() != op || cmd.GetServerId() != serverA1 ||
		cmd.GetAction() != agentv1.CommandAction_COMMAND_ACTION_CONSOLE {
		t.Fatalf("command %v", cmd)
	}
	spec, err := c.GetCommandSpec(context.Background(), &agentv1.GetCommandSpecRequest{ServerId: serverA1, OperationId: op})
	if err != nil || spec.GetCommand() != "say hi é" {
		t.Fatalf("spec %v %v", spec, err)
	}
	for name, req := range map[string]*agentv1.GetCommandSpecRequest{
		"another server": {ServerId: serverA2, OperationId: op},
		"no server":      {OperationId: op},
		"unknown op":     {ServerId: serverA1, OperationId: "0199e1a0-0000-7000-8000-00000000ffff"},
		"path as op":     {ServerId: serverA1, OperationId: "../x"},
		"no op":          {ServerId: serverA1},
	} {
		_, err := c.GetCommandSpec(context.Background(), req)
		wantNotPermitted(t, name, err)
	}

	// Finished: nothing any more. The console wasn't open.
	mustSend(t, s, resultMsg(epoch, op, outFailed, agentv1.ResultCode_RESULT_CODE_CONSOLE_UNAVAILABLE))
	waitFor(t, "failed", func() bool { return e.op(t, op).state == "failed" })
	if got := e.op(t, op); got.code != "console_unavailable" || got.summary == "" {
		t.Errorf("console_unavailable: %+v", got)
	}
	_, err = c.GetCommandSpec(context.Background(), &agentv1.GetCommandSpecRequest{ServerId: serverA1, OperationId: op})
	wantNotPermitted(t, "finished", err)

	// A running operation that isn't a command.
	start := e.freshOp(t, serverA2, store.ActionStart)
	e.srv.Kick(nodeA)
	if cmd := r.command(t); cmd.GetOperationId() != start {
		t.Fatalf("command %v", cmd)
	}
	_, err = c.GetCommandSpec(context.Background(), &agentv1.GetCommandSpecRequest{ServerId: serverA2, OperationId: start})
	wantNotPermitted(t, "a start", err)
	mustSend(t, s, resultMsg(epoch, start, outSucceeded, codeNone))
	waitFor(t, "start done", func() bool { return e.op(t, start).state == "succeeded" })

	// The server had stopped by the time the agent asked.
	op2 := e.createCommand(t, serverA1, "say again")
	e.srv.Kick(nodeA)
	if cmd := r.command(t); cmd.GetOperationId() != op2 {
		t.Fatalf("command %v", cmd)
	}
	mustSend(t, s, resultMsg(epoch, op2, outFailed, agentv1.ResultCode_RESULT_CODE_NOT_RUNNING))
	waitFor(t, "failed", func() bool { return e.op(t, op2).state == "failed" })
	if got := e.op(t, op2); got.code != "not_running" {
		t.Errorf("not_running: %+v", got)
	}
}
