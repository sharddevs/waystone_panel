// SPDX-License-Identifier: AGPL-3.0-only

package agentlink

import (
	"errors"

	"google.golang.org/protobuf/reflect/protoreflect"

	agentv1 "github.com/sharddevs/waystone_panel/gen/waystone/agent/v1"
	"github.com/sharddevs/waystone_panel/internal/store"
)

// hasUnknownFields reports whether m, or any message inside it, carries
// fields this build doesn't know (R12). Go's protobuf keeps them quietly,
// so they have to be looked for.
func hasUnknownFields(m protoreflect.Message) bool {
	if len(m.GetUnknown()) > 0 {
		return true
	}
	found := false
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case fd.IsList() && fd.Message() != nil:
			list := v.List()
			for i := 0; i < list.Len() && !found; i++ {
				found = hasUnknownFields(list.Get(i).Message())
			}
		case fd.IsMap() && fd.MapValue().Message() != nil:
			v.Map().Range(func(_ protoreflect.MapKey, mv protoreflect.Value) bool {
				found = hasUnknownFields(mv.Message())
				return !found
			})
		case fd.Message() != nil && !fd.IsList() && !fd.IsMap():
			found = hasUnknownFields(v.Message())
		}
		return !found
	})
	return found
}

// processStates maps the wire enum onto the store's states. UNSPECIFIED
// and any number this build doesn't know are missing on purpose: proto3
// enums are open, so they decode without error and must be refused here
// (R5).
var processStates = map[agentv1.ProcessState]store.ProcessState{
	agentv1.ProcessState_PROCESS_STATE_STOPPED:  store.ProcessStopped,
	agentv1.ProcessState_PROCESS_STATE_STARTING: store.ProcessStarting,
	agentv1.ProcessState_PROCESS_STATE_RUNNING:  store.ProcessRunning,
	agentv1.ProcessState_PROCESS_STATE_STOPPING: store.ProcessStopping,
	agentv1.ProcessState_PROCESS_STATE_FAILED:   store.ProcessFailed,
	agentv1.ProcessState_PROCESS_STATE_UNKNOWN:  store.ProcessUnknown,
}

var errBadEntry = errors.New("bad report entry")

// toStoreReport converts a wire report, refusing any state the store has
// no name for and any native_state that couldn't go to the log as is. The
// rest of R5 is store.ValidateReport's job.
func toStoreReport(r *agentv1.Report) (store.Report, error) {
	out := store.Report{Epoch: r.GetEpoch(), Seq: r.GetSeq()}
	if t := r.GetAgentTime(); t != nil {
		if err := t.CheckValid(); err != nil {
			return store.Report{}, errBadEntry
		}
		out.AgentTime = t.AsTime()
	}
	out.Entries = make([]store.ReportEntry, 0, len(r.GetEntries()))
	for _, e := range r.GetEntries() {
		state, ok := processStates[e.GetProcessState()]
		if !ok {
			return store.Report{}, errBadEntry
		}
		if err := store.ValidateNativeState(e.GetNativeState()); err != nil {
			return store.Report{}, errBadEntry
		}
		out.Entries = append(out.Entries, store.ReportEntry{ServerID: e.GetServerId(), State: state})
	}
	return out, nil
}

// commandActions maps an operation's action onto the wire.
var commandActions = map[store.Action]agentv1.CommandAction{
	store.ActionStart:   agentv1.CommandAction_COMMAND_ACTION_START,
	store.ActionStop:    agentv1.CommandAction_COMMAND_ACTION_STOP,
	store.ActionRestart: agentv1.CommandAction_COMMAND_ACTION_RESTART,
}

// resultCodes is the controller's fixed table from an agent's result code
// to what goes in the operation row (decision L). The agent sends codes
// only, so none of its text ever reaches the history.
var resultCodes = map[agentv1.ResultCode]store.OperationResult{
	agentv1.ResultCode_RESULT_CODE_NOT_FOUND: {State: store.StateFailed, Code: "not_found",
		Summary: "The server's systemd unit was not found on the node."},
	agentv1.ResultCode_RESULT_CODE_PERMISSION_DENIED: {State: store.StateFailed, Code: "permission_denied",
		Summary: "The node's helper refused the request."},
	agentv1.ResultCode_RESULT_CODE_INVALID_REQUEST: {State: store.StateFailed, Code: "invalid_request",
		Summary: "The node's helper rejected the request as invalid."},
	agentv1.ResultCode_RESULT_CODE_ACTION_FAILED: {State: store.StateFailed, Code: "action_failed",
		Summary: "systemd reported that the action failed."},
	agentv1.ResultCode_RESULT_CODE_HELPER_UNAVAILABLE: {State: store.StateFailed, Code: "helper_unavailable",
		Summary: "The agent couldn't reach the node's helper, so nothing was done."},
	agentv1.ResultCode_RESULT_CODE_UNKNOWN_SERVER: {State: store.StateFailed, Code: "unknown_server",
		Summary: "The agent doesn't manage this server, so nothing was done."},
	agentv1.ResultCode_RESULT_CODE_SERVER_BUSY: {State: store.StateFailed, Code: "server_busy",
		Summary: "An earlier action on this server was still running, so nothing was done."},
	agentv1.ResultCode_RESULT_CODE_NO_ANSWER: {State: store.StateUncertain, Code: "no_answer",
		Summary: "The node's helper didn't answer, so nobody knows whether the action ran."},
}

// toStoreResult checks that a result's outcome and code belong together
// and turns them into the row's state, code and summary. SUCCEEDED comes
// with no code, UNKNOWN only with NO_ANSWER, and FAILED with any other
// known code. Everything else, unknown numbers included, is refused.
func toStoreResult(r *agentv1.Result) (store.OperationResult, bool) {
	switch r.GetOutcome() {
	case agentv1.ResultOutcome_RESULT_OUTCOME_SUCCEEDED:
		if r.GetCode() == agentv1.ResultCode_RESULT_CODE_UNSPECIFIED {
			return store.OperationResult{State: store.StateSucceeded}, true
		}
	case agentv1.ResultOutcome_RESULT_OUTCOME_FAILED:
		if m, ok := resultCodes[r.GetCode()]; ok && m.State == store.StateFailed {
			return m, true
		}
	case agentv1.ResultOutcome_RESULT_OUTCOME_UNKNOWN:
		if m, ok := resultCodes[r.GetCode()]; ok && m.State == store.StateUncertain {
			return m, true
		}
	}
	return store.OperationResult{}, false
}
