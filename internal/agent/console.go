// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"errors"
	"fmt"

	agentv1 "github.com/sharddevs/waystone_panel/gen/waystone/agent/v1"
	helperv1 "github.com/sharddevs/waystone_panel/gen/waystone/helper/v1"
	"github.com/sharddevs/waystone_panel/internal/console"
)

// Stage 7b (notes/stage7-scope.md D2): a console command writes one line
// to the FIFO systemd gives the game as its stdin. The command carries
// only IDs; the line comes from GetCommandSpec and is checked again here.
// systemd makes the FIFO for one run of the game and removes it when the
// game stops (7M1), and the agent writes only while the helper says the
// game is running. The agent never makes the FIFO or its directory.

// DefaultConsoleDir is where the console socket units put their FIFOs:
// root-owned, 0755, made by systemd (DirectoryMode=).
const DefaultConsoleDir = "/run/waystone-console"

// A console command's own failures. Each means nothing was written.
var (
	errNotRunning         = errors.New("server is not running")
	errConsoleUnavailable = errors.New("console unavailable")
	errStatusUnavailable  = errors.New("server status unavailable")
)

// consoleOutcome is installOutcome with the console's own failures added.
func consoleOutcome(err error) (agentv1.ResultOutcome, agentv1.ResultCode) {
	failed := agentv1.ResultOutcome_RESULT_OUTCOME_FAILED
	switch {
	case errors.Is(err, errNotRunning):
		return failed, agentv1.ResultCode_RESULT_CODE_NOT_RUNNING
	case errors.Is(err, errConsoleUnavailable):
		return failed, agentv1.ResultCode_RESULT_CODE_CONSOLE_UNAVAILABLE
	case errors.Is(err, errStatusUnavailable):
		return failed, agentv1.ResultCode_RESULT_CODE_HELPER_UNAVAILABLE
	}
	return installOutcome(err)
}

// runConsole runs one CONSOLE command to the end:
//
//  1. GetCommandSpec, and the line checked again (SPEC_UNAVAILABLE).
//  2. The helper's STATUS, just before the write: anything but RUNNING is
//     NOT_RUNNING (7M1), and no answer is HELPER_UNAVAILABLE.
//  3. One write of the line and a newline to the server's FIFO, after the
//     checks in writeConsole (CONSOLE_UNAVAILABLE).
//
// Only a write that was cut short leaves it unknown.
func (a *Agent) runConsole(cmd *agentv1.Command, cmds *commands) error {
	ctx, cancel := context.WithTimeout(context.Background(), a.commandCallTimeout)
	defer cancel()
	server, op := cmd.GetServerId(), cmd.GetOperationId()

	resp, err := cmds.link.GetCommandSpec(ctx, &agentv1.GetCommandSpecRequest{ServerId: server, OperationId: op})
	if err != nil {
		return fmt.Errorf("%w: %v", errSpecUnavailable, err)
	}
	if len(resp.ProtoReflect().GetUnknown()) != 0 || !console.ValidLine(resp.GetCommand()) {
		return errSpecUnavailable
	}

	st, err := a.helper.Do(ctx, &helperv1.Request{OperationId: op, ServerId: server, Action: helperv1.Action_ACTION_STATUS})
	switch {
	case err != nil:
		return fmt.Errorf("%w: %v", errStatusUnavailable, err)
	case st.GetErrorCode() == helperv1.ErrorCode_ERROR_CODE_NOT_FOUND:
		return errNotRunning
	case st.GetErrorCode() != helperv1.ErrorCode_ERROR_CODE_UNSPECIFIED:
		return fmt.Errorf("%w: status %s", errStatusUnavailable, st.GetErrorCode())
	case st.GetRunState() != helperv1.RunState_RUN_STATE_RUNNING:
		return errNotRunning
	}
	return writeConsole(a.consoleDir, a.consoleOwner, server, resp.GetCommand())
}
