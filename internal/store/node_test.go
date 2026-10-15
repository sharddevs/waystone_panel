// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// R5's shape checks, which need no database.

func TestValidateReport(t *testing.T) {
	const s1 = "0199e1a0-0000-7000-8000-000000000a01"
	const s2 = "0199e1a0-0000-7000-8000-000000000a02"
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	ok := func(entries ...ReportEntry) Report {
		return Report{Epoch: 1, Seq: 1, AgentTime: now, Entries: entries}
	}
	full := make([]ReportEntry, MaxServersPerNode)
	for i := range full {
		full[i] = ReportEntry{ServerID: fmt.Sprintf("0199e1a0-0000-7000-8000-%012x", i), State: ProcessRunning, Readiness: ReadinessReady}
	}

	good := map[string]Report{
		"zero entries (heartbeat)": ok(),
		"every state": ok(
			ReportEntry{s1, ProcessStopped, ReadinessUnknown}, ReportEntry{s2, ProcessUnknown, ReadinessUnknown},
		),
		"256 entries": ok(full...),
		"every readiness": ok(
			ReportEntry{s1, ProcessRunning, ReadinessNotReady}, ReportEntry{s2, ProcessStopped, ReadinessNotSupported},
		),
	}
	for name, r := range good {
		if err := ValidateReport(r); err != nil {
			t.Errorf("%s: %v, want nil", name, err)
		}
	}

	bad := map[string]Report{
		"epoch 0":        {Epoch: 0, Seq: 1, AgentTime: now},
		"seq 0":          {Epoch: 1, Seq: 0, AgentTime: now},
		"negative seq":   {Epoch: 1, Seq: -1, AgentTime: now},
		"no agent time":  {Epoch: 1, Seq: 1},
		"257 entries":    ok(append(full, ReportEntry{s1, ProcessRunning, ReadinessUnknown})...),
		"duplicate":      ok(ReportEntry{s1, ProcessRunning, ReadinessUnknown}, ReportEntry{s1, ProcessStopped, ReadinessUnknown}),
		"bad uuid":       ok(ReportEntry{"not-a-uuid", ProcessRunning, ReadinessUnknown}),
		"uppercase uuid": ok(ReportEntry{strings.ToUpper(s1), ProcessRunning, ReadinessUnknown}),
		"empty state":    ok(ReportEntry{s1, "", ReadinessUnknown}),
		"unknown state":  ok(ReportEntry{s1, "exploded", ReadinessUnknown}),
		// D6: readiness is required, known, and ready only while running.
		"empty readiness":      ok(ReportEntry{s1, ProcessRunning, ""}),
		"unknown readiness":    ok(ReportEntry{s1, ProcessRunning, "online"}),
		"ready while stopped":  ok(ReportEntry{s1, ProcessStopped, ReadinessReady}),
		"ready while unknown":  ok(ReportEntry{s1, ProcessUnknown, ReadinessReady}),
		"ready while starting": ok(ReportEntry{s1, ProcessStarting, ReadinessReady}),
	}
	for name, r := range bad {
		if err := ValidateReport(r); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s: %v, want ErrInvalidInput", name, err)
		}
	}
}

func TestValidateNativeState(t *testing.T) {
	for _, s := range []string{"", "active/running", "deadline_exceeded", strings.Repeat("a", 32), strings.Repeat("é", 16)} {
		if err := ValidateNativeState(s); err != nil {
			t.Errorf("ValidateNativeState(%q) = %v, want nil", s, err)
		}
	}
	for _, s := range []string{strings.Repeat("a", 33), strings.Repeat("é", 17), "a\nb", "a\x00", "\xff", "tab\there"} {
		if err := ValidateNativeState(s); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("ValidateNativeState(%q) = %v, want ErrInvalidInput", s, err)
		}
	}
}

// D6: the port comes from config->>'port' and is a decimal 1..65535, or
// there is none.
func TestGamePort(t *testing.T) {
	str := func(s string) *string { return &s }
	good := map[string]uint32{"25565": 25565, "1": 1, "65535": 65535, "025565": 25565}
	for in, want := range good {
		if got, ok := GamePort(str(in)); !ok || got != want {
			t.Errorf("GamePort(%q) = %d, %v; want %d", in, got, ok, want)
		}
	}
	if _, ok := GamePort(nil); ok {
		t.Error("GamePort(nil) has a port")
	}
	for _, in := range []string{"", "0", "65536", "-1", "+25565", "25565.0", "2.5e4", " 25565", "25565 ", "port", "0x63dd"} {
		if got, ok := GamePort(str(in)); ok {
			t.Errorf("GamePort(%q) = %d, want none", in, got)
		}
	}
}
