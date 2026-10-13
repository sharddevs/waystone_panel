// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"strings"
	"unicode"
	"unicode/utf8"

	agentv1 "github.com/sharddevs/waystone_panel/gen/waystone/agent/v1"
	helperv1 "github.com/sharddevs/waystone_panel/gen/waystone/helper/v1"
)

// maxNativeState is the controller's limit (R5). The agent cuts to it
// (D3); the controller still refuses anything longer.
const maxNativeState = 32

// Helper is the one call the agent makes in 4b: STATUS. helper.Client
// implements it.
type Helper interface {
	Do(ctx context.Context, req *helperv1.Request) (*helperv1.Response, error)
}

// runStates maps the helper's states onto the report's, one to one.
var runStates = map[helperv1.RunState]agentv1.ProcessState{
	helperv1.RunState_RUN_STATE_STOPPED:  agentv1.ProcessState_PROCESS_STATE_STOPPED,
	helperv1.RunState_RUN_STATE_STARTING: agentv1.ProcessState_PROCESS_STATE_STARTING,
	helperv1.RunState_RUN_STATE_RUNNING:  agentv1.ProcessState_PROCESS_STATE_RUNNING,
	helperv1.RunState_RUN_STATE_STOPPING: agentv1.ProcessState_PROCESS_STATE_STOPPING,
	helperv1.RunState_RUN_STATE_FAILED:   agentv1.ProcessState_PROCESS_STATE_FAILED,
}

// sweep asks the helper for each server's state, one after another, and
// returns one fresh entry per server, in order (R9). Every entry is built
// from this sweep's own answer: once the sweep's deadline has passed,
// every server not yet asked is UNKNOWN with "timeout". Nothing from an
// earlier sweep is ever reused.
//
// sweepID is sent as the request's operation ID, which the helper
// requires and logs, so its log lines can be matched to this sweep.
func (a *Agent) sweep(ctx context.Context, sweepID string, serverIDs []string) []*agentv1.ReportEntry {
	ctx, cancel := context.WithTimeout(ctx, a.sweepTimeout)
	defer cancel()
	entries := make([]*agentv1.ReportEntry, 0, len(serverIDs))
	for _, id := range serverIDs {
		entries = append(entries, a.status(ctx, sweepID, id))
	}
	return entries
}

// status is one STATUS call under its own 3 s deadline, inside the
// sweep's. A failure of any kind is UNKNOWN, with the reason as
// native_state: timeout, unavailable, or the helper's error code name
// (not_found, permission_denied, failed, ...).
func (a *Agent) status(sweepCtx context.Context, sweepID, serverID string) *agentv1.ReportEntry {
	unknown := func(why string) *agentv1.ReportEntry {
		return &agentv1.ReportEntry{
			ServerId:     serverID,
			ProcessState: agentv1.ProcessState_PROCESS_STATE_UNKNOWN,
			NativeState:  why,
		}
	}
	if sweepCtx.Err() != nil {
		return unknown("timeout")
	}
	ctx, cancel := context.WithTimeout(sweepCtx, a.callTimeout)
	defer cancel()

	// The call runs on its own goroutine so the deadline holds even if
	// the helper client were to ignore its context. The channel has room
	// for the late answer, which is then dropped.
	type result struct {
		resp *helperv1.Response
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := a.helper.Do(ctx, &helperv1.Request{
			OperationId: sweepID,
			ServerId:    serverID,
			Action:      helperv1.Action_ACTION_STATUS,
		})
		done <- result{resp, err}
	}()
	var r result
	select {
	case r = <-done:
	case <-ctx.Done():
	}
	// Past the deadline it's a timeout, whichever arrived first: the
	// answer, the helper's own error, or the deadline itself.
	if ctx.Err() != nil {
		return unknown("timeout")
	}
	if r.err != nil {
		return unknown("unavailable")
	}
	if code := r.resp.GetErrorCode(); code != helperv1.ErrorCode_ERROR_CODE_UNSPECIFIED {
		return unknown(errorCodeName(code))
	}
	state, ok := runStates[r.resp.GetRunState()]
	if !ok {
		return unknown("no_run_state")
	}
	return &agentv1.ReportEntry{
		ServerId:     serverID,
		ProcessState: state,
		NativeState:  cleanNativeState(r.resp.GetNativeState()),
	}
}

// errorCodeName turns ERROR_CODE_NOT_FOUND into not_found.
func errorCodeName(code helperv1.ErrorCode) string {
	return cleanNativeState(strings.ToLower(strings.TrimPrefix(code.String(), "ERROR_CODE_")))
}

// cleanNativeState makes the helper's free text fit the report (D3):
// valid UTF-8, control characters replaced with '?', and at most 32
// bytes, cut on a character boundary.
func cleanNativeState(s string) string {
	s = strings.ToValidUTF8(s, "?")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '?'
		}
		return r
	}, s)
	if len(s) <= maxNativeState {
		return s
	}
	cut := maxNativeState
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
