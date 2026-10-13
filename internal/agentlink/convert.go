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
