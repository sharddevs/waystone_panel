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
		full[i] = ReportEntry{ServerID: fmt.Sprintf("0199e1a0-0000-7000-8000-%012x", i), State: ProcessRunning}
	}

	good := map[string]Report{
		"zero entries (heartbeat)": ok(),
		"every state": ok(
			ReportEntry{s1, ProcessStopped}, ReportEntry{s2, ProcessUnknown},
		),
		"256 entries": ok(full...),
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
		"257 entries":    ok(append(full, ReportEntry{s1, ProcessRunning})...),
		"duplicate":      ok(ReportEntry{s1, ProcessRunning}, ReportEntry{s1, ProcessStopped}),
		"bad uuid":       ok(ReportEntry{"not-a-uuid", ProcessRunning}),
		"uppercase uuid": ok(ReportEntry{strings.ToUpper(s1), ProcessRunning}),
		"empty state":    ok(ReportEntry{s1, ""}),
		"unknown state":  ok(ReportEntry{s1, "exploded"}),
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
