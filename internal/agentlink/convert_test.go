// SPDX-License-Identifier: AGPL-3.0-only

package agentlink

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/sharddevs/waystone_panel/gen/waystone/agent/v1"
	"github.com/sharddevs/waystone_panel/internal/store"
)

const testServer = "0199e1a0-0000-7000-8000-000000000a01"

// withUnknown adds field 99 to m, as a newer peer would send it.
func withUnknown(m protoreflect.ProtoMessage) {
	r := m.ProtoReflect()
	r.SetUnknown(protowire.AppendVarint(protowire.AppendTag(r.GetUnknown(), 99, protowire.VarintType), 1))
}

func reportMsg() *agentv1.ConnectRequest {
	return &agentv1.ConnectRequest{Msg: &agentv1.ConnectRequest_Report{Report: &agentv1.Report{
		Epoch: 1, Seq: 1, AgentTime: timestamppb.New(time.Now()),
		Entries: []*agentv1.ReportEntry{{ServerId: testServer, ProcessState: agentv1.ProcessState_PROCESS_STATE_RUNNING, Readiness: agentv1.Readiness_READINESS_READY}},
	}}}
}

// R12: unknown fields are found at every level, also after a round trip
// through the wire format.
func TestHasUnknownFields(t *testing.T) {
	if hasUnknownFields(reportMsg().ProtoReflect()) {
		t.Fatal("clean message has unknown fields")
	}
	cases := map[string]func(*agentv1.ConnectRequest){
		"top level": func(m *agentv1.ConnectRequest) { withUnknown(m) },
		"report":    func(m *agentv1.ConnectRequest) { withUnknown(m.GetReport()) },
		"entry":     func(m *agentv1.ConnectRequest) { withUnknown(m.GetReport().GetEntries()[0]) },
		"timestamp": func(m *agentv1.ConnectRequest) { withUnknown(m.GetReport().GetAgentTime()) },
	}
	for name, add := range cases {
		m := reportMsg()
		add(m)
		b, err := proto.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		var got agentv1.ConnectRequest
		if err := proto.Unmarshal(b, &got); err != nil {
			t.Fatal(err)
		}
		if !hasUnknownFields(got.ProtoReflect()) {
			t.Errorf("%s: unknown field not found", name)
		}
	}
}

// R5: states the store can't name, and native_state that can't go to the
// log, are refused. Everything else converts one to one.
func TestToStoreReport(t *testing.T) {
	r := reportMsg().GetReport()
	for wire, want := range processStates {
		r.Entries[0].ProcessState = wire
		got, err := toStoreReport(r)
		if err != nil || got.Entries[0].State != want || got.Epoch != 1 || got.Seq != 1 || got.AgentTime.IsZero() {
			t.Errorf("%v: %+v, %v", wire, got, err)
		}
	}
	if len(processStates) != 6 {
		t.Errorf("%d mapped states, want 6", len(processStates))
	}
	r = reportMsg().GetReport()
	for wire, want := range readinessStates {
		r.Entries[0].Readiness = wire
		got, err := toStoreReport(r)
		if err != nil || got.Entries[0].Readiness != want {
			t.Errorf("%v: %+v, %v", wire, got, err)
		}
	}
	if len(readinessStates) != 4 {
		t.Errorf("%d mapped readiness values, want 4", len(readinessStates))
	}
	bad := map[string]func(*agentv1.Report){
		"unspecified":       func(r *agentv1.Report) { r.Entries[0].ProcessState = agentv1.ProcessState_PROCESS_STATE_UNSPECIFIED },
		"unknown enum":      func(r *agentv1.Report) { r.Entries[0].ProcessState = 99 },
		"readiness unset":   func(r *agentv1.Report) { r.Entries[0].Readiness = agentv1.Readiness_READINESS_UNSPECIFIED },
		"readiness 99":      func(r *agentv1.Report) { r.Entries[0].Readiness = 99 },
		"native 33 bytes":   func(r *agentv1.Report) { r.Entries[0].NativeState = strings.Repeat("x", 33) },
		"native control":    func(r *agentv1.Report) { r.Entries[0].NativeState = "a\nb" },
		"native bad utf-8":  func(r *agentv1.Report) { r.Entries[0].NativeState = "\xff" },
		"invalid timestamp": func(r *agentv1.Report) { r.AgentTime = &timestamppb.Timestamp{Seconds: 1, Nanos: -1} },
	}
	for name, mutate := range bad {
		r := reportMsg().GetReport()
		mutate(r)
		if _, err := toStoreReport(r); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// 32 bytes is fine; a missing time converts to zero, which
	// store.ValidateReport then refuses.
	r = reportMsg().GetReport()
	r.Entries[0].NativeState = strings.Repeat("x", 32)
	r.AgentTime = nil
	got, err := toStoreReport(r)
	if err != nil || !got.AgentTime.IsZero() {
		t.Errorf("32 bytes, no time: %+v, %v", got, err)
	}
	if store.ValidateReport(got) == nil {
		t.Error("ValidateReport accepted a report without agent time")
	}
}
