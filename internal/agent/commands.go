// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	agentv1 "github.com/sharddevs/waystone_panel/gen/waystone/agent/v1"
	"github.com/sharddevs/waystone_panel/internal/helper"
	"github.com/sharddevs/waystone_panel/internal/ids"
	"github.com/sharddevs/waystone_panel/internal/lifecycle"
)

// Stage 4c (notes/stage4c-scope.md v2.1, §5): the agent carries out the
// controller's commands through the helper and sends back one Result per
// command.

// Runner starts and stops servers. lifecycle.HelperRuntime is the real
// one; it sends each call to the helper with the operation's ID.
type Runner interface {
	Start(ctx context.Context, serverID, operationID string) error
	Stop(ctx context.Context, serverID, operationID string) error
}

var _ Runner = lifecycle.HelperRuntime{}

const (
	// maxInFlight matches the controller's limit (Q3). A fifth command is
	// a protocol error.
	maxInFlight = 4
	// commandCallTimeout is each helper call's deadline, and so the
	// request's not_after: 10 s more than the helper's own 90 s
	// connection limit (Q2).
	commandCallTimeout = 100 * time.Second
)

// commands is one session's view of the commands it was sent: what is
// still running, and the inventory from the hello (F14) and every
// ServerAdded since (6c-2).
type commands struct {
	epoch int64
	// link is this session's connection, for an install's own calls.
	link agentv1.AgentLinkServiceClient

	mu        sync.Mutex
	inventory []string          // in the hello's order, then as added
	ports     map[string]uint32 // the game port of each server that has one
	inflight  map[string]string // operation ID -> server ID
}

func newCommands(epoch int64, serverIDs []string, ports map[string]uint32) *commands {
	c := &commands{epoch: epoch, inventory: slices.Clone(serverIDs), ports: maps.Clone(ports), inflight: make(map[string]string)}
	if c.ports == nil {
		c.ports = map[string]uint32{}
	}
	return c
}

// add puts a ServerAdded's server in the inventory. Any error is a
// protocol error: the wrong epoch, a bad ID or port, a server it already
// has, or one more than the most a hello may list.
func (c *commands) add(m *agentv1.ServerAdded) error {
	if m.GetEpoch() != c.epoch {
		return protocolError("server added for another epoch")
	}
	if ids.Check(m.GetServerId()) != nil || m.GetGamePort() > 65535 {
		return protocolError("server added with a bad ID or port")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if slices.Contains(c.inventory, m.GetServerId()) {
		return protocolError("server added twice")
	}
	if len(c.inventory) >= maxServers {
		return protocolError("more servers than a hello may list")
	}
	c.inventory = append(c.inventory, m.GetServerId())
	if m.GetGamePort() != 0 {
		c.ports[m.GetServerId()] = m.GetGamePort()
	}
	return nil
}

// servers returns a copy of the inventory and its ports, for one report.
func (c *commands) servers() ([]string, map[string]uint32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.inventory), maps.Clone(c.ports)
}

// errProtocol: the controller sent something no correct controller
// sends. The session ends.
var errProtocol = errors.New("protocol error from the controller")

func protocolError(why string) error { return fmt.Errorf("%w: %s", errProtocol, why) }

// accept checks a command (§5 steps 1 to 3). known is false for a server
// outside the inventory, which gets FAILED UNKNOWN_SERVER without the
// helper being asked. Any error is a protocol error.
func (c *commands) accept(cmd *agentv1.Command) (known bool, err error) {
	if cmd.GetEpoch() != c.epoch {
		return false, protocolError("command for another epoch")
	}
	if ids.Check(cmd.GetOperationId()) != nil || ids.Check(cmd.GetServerId()) != nil {
		return false, protocolError("command with a bad ID")
	}
	switch cmd.GetAction() {
	case agentv1.CommandAction_COMMAND_ACTION_START, agentv1.CommandAction_COMMAND_ACTION_STOP,
		agentv1.CommandAction_COMMAND_ACTION_RESTART, agentv1.CommandAction_COMMAND_ACTION_INSTALL,
		agentv1.CommandAction_COMMAND_ACTION_ACCEPT_EULA:
	default:
		return false, protocolError("command with an unknown action")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, dup := c.inflight[cmd.GetOperationId()]; dup {
		return false, protocolError("operation sent twice")
	}
	for _, server := range c.inflight {
		if server == cmd.GetServerId() {
			return false, protocolError("second command for a busy server")
		}
	}
	if len(c.inflight) >= maxInFlight {
		return false, protocolError("more than 4 commands in flight")
	}
	if !slices.Contains(c.inventory, cmd.GetServerId()) {
		return false, nil
	}
	c.inflight[cmd.GetOperationId()] = cmd.GetServerId()
	return true, nil
}

func (c *commands) done(opID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.inflight, opID)
}

// result builds a Result message.
func result(epoch int64, opID string, outcome agentv1.ResultOutcome, code agentv1.ResultCode) *agentv1.ConnectRequest {
	return &agentv1.ConnectRequest{Msg: &agentv1.ConnectRequest_Result{Result: &agentv1.Result{
		Epoch: epoch, OperationId: opID, Outcome: outcome, Code: code,
	}}}
}

// outcomeOf turns a Runner error into the Result (Q4, S5):
//
//   - nil: SUCCEEDED.
//   - the helper answered with an error code: FAILED with that code,
//     CONFLICT being SERVER_BUSY (S3).
//   - helper.ErrNotSent, no write was ever attempted: FAILED
//     HELPER_UNAVAILABLE. Nothing can have run.
//   - anything else happened after a write was attempted: UNKNOWN
//     NO_ANSWER. The helper may have run it.
func outcomeOf(err error) (agentv1.ResultOutcome, agentv1.ResultCode) {
	failed := agentv1.ResultOutcome_RESULT_OUTCOME_FAILED
	switch {
	case err == nil:
		return agentv1.ResultOutcome_RESULT_OUTCOME_SUCCEEDED, agentv1.ResultCode_RESULT_CODE_UNSPECIFIED
	case errors.Is(err, helper.ErrNotSent):
		return failed, agentv1.ResultCode_RESULT_CODE_HELPER_UNAVAILABLE
	case errors.Is(err, lifecycle.ErrNotFound):
		return failed, agentv1.ResultCode_RESULT_CODE_NOT_FOUND
	case errors.Is(err, lifecycle.ErrPermissionDenied):
		return failed, agentv1.ResultCode_RESULT_CODE_PERMISSION_DENIED
	case errors.Is(err, lifecycle.ErrInvalidRequest):
		return failed, agentv1.ResultCode_RESULT_CODE_INVALID_REQUEST
	case errors.Is(err, lifecycle.ErrConflict):
		return failed, agentv1.ResultCode_RESULT_CODE_SERVER_BUSY
	case errors.Is(err, lifecycle.ErrFailed), errors.Is(err, lifecycle.ErrChanged):
		// ErrChanged never comes from START or STOP; if it ever did, the
		// helper still answered, so it's a definite failure.
		return failed, agentv1.ResultCode_RESULT_CODE_ACTION_FAILED
	}
	return agentv1.ResultOutcome_RESULT_OUTCOME_UNKNOWN, agentv1.ResultCode_RESULT_CODE_NO_ANSWER
}

// execute runs one accepted command to the end and hands its messages to
// send, in order. It never stops halfway because the session ended:
// systemctl can't safely be cut off, so the helper calls get their full
// deadline whatever happens to the stream (F3 to F5). send drops what it
// can no longer deliver on this session's own stream.
//
// The command leaves cmds' in-flight set just before its result goes out:
// the controller may send the server's next command the moment it has the
// result, and that must not look like a duplicate.
func (a *Agent) execute(cmd *agentv1.Command, cmds *commands, send func(*agentv1.ConnectRequest) bool) {
	call := func(f func(context.Context, string, string) error) error {
		ctx, cancel := context.WithTimeout(context.Background(), a.commandCallTimeout)
		defer cancel()
		return f(ctx, cmd.GetServerId(), cmd.GetOperationId())
	}
	finish := func(err error) {
		outcome, code := installOutcome(err)
		cmds.done(cmd.GetOperationId())
		if !send(result(cmd.GetEpoch(), cmd.GetOperationId(), outcome, code)) {
			a.logf("agent=result_dropped op=%s server=%s outcome=%s code=%s reason=session_ended",
				cmd.GetOperationId(), cmd.GetServerId(), outcome, code)
			return
		}
		// send only queued the result for this session's sender; the
		// controller hasn't acknowledged anything, so the log says no more.
		a.logf("agent=command_done op=%s server=%s outcome=%s code=%s result=queued_for_send",
			cmd.GetOperationId(), cmd.GetServerId(), outcome, code)
	}

	switch cmd.GetAction() {
	case agentv1.CommandAction_COMMAND_ACTION_START:
		finish(call(a.runner.Start))
	case agentv1.CommandAction_COMMAND_ACTION_STOP:
		finish(call(a.runner.Stop))
	case agentv1.CommandAction_COMMAND_ACTION_RESTART:
		// Decision N: STOP, then Progress(STARTING), then START. A failed
		// stop ends it in phase stopping; a failed start in starting.
		if err := call(a.runner.Stop); err != nil {
			finish(err)
			return
		}
		send(&agentv1.ConnectRequest{Msg: &agentv1.ConnectRequest_Progress{Progress: &agentv1.Progress{
			Epoch: cmd.GetEpoch(), OperationId: cmd.GetOperationId(), Phase: agentv1.OperationPhase_OPERATION_PHASE_STARTING,
		}}})
		finish(call(a.runner.Start))
	case agentv1.CommandAction_COMMAND_ACTION_INSTALL:
		finish(a.install(cmd, cmds, send))
	case agentv1.CommandAction_COMMAND_ACTION_ACCEPT_EULA:
		finish(a.acceptEula(cmd))
	}
}
