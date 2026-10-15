// SPDX-License-Identifier: AGPL-3.0-only

package view

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sharddevs/waystone_panel/internal/store"
)

func all(store.Action) bool  { return true }
func none(store.Action) bool { return false }

// B8: an action the role may not do is left out; the list is [] and not
// null when nothing is left. Only the actions allowed says yes to remain.
func TestActionsForRole(t *testing.T) {
	s := store.Server{}
	if got := ActionsFor(all, s, true); len(got) != 3 {
		t.Errorf("admin: %+v", got)
	}
	onlyStop := func(a store.Action) bool { return a == store.ActionStop }
	if got := ActionsFor(onlyStop, s, true); len(got) != 1 || got[0].Action != "stop" {
		t.Errorf("only stop: %+v", got)
	}
	got := ActionsFor(none, s, true)
	if got == nil || len(got) != 0 {
		t.Errorf("viewer: %#v, want an empty list", got)
	}
	b, _ := json.Marshal(ServerOf(s, none, time.Now()))
	if !strings.Contains(string(b), `"actions":[]`) {
		t.Errorf("viewer JSON: %s", b)
	}
}

// B8 reason order, including the combinations.
func TestActionsReasonOrder(t *testing.T) {
	cases := []struct {
		blocking  store.OperationState
		reachable bool
		reason    string
	}{
		{"", true, ""},
		{store.StatePending, true, "operation_in_progress"},
		{store.StateRunning, true, "operation_in_progress"},
		{store.StateUncertain, true, "previous_operation_uncertain"},
		{"", false, "node_unreachable"},
		{store.StatePending, false, "node_unreachable"},
		{store.StateUncertain, false, "node_unreachable"},
	}
	for _, tc := range cases {
		for _, a := range ActionsFor(all, store.Server{Blocking: tc.blocking}, tc.reachable) {
			if a.Reason != tc.reason || a.Allowed != (tc.reason == "") {
				t.Errorf("blocking %q reachable %v: %+v, want reason %q", tc.blocking, tc.reachable, a, tc.reason)
			}
		}
	}
}

// B2, B6, B7 at the 45 s boundary, without a database.
func TestServerJSONStaleness(t *testing.T) {
	now := time.Date(2026, 10, 15, 3, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { t := now.Add(-d); return &t }
	s := store.Server{ProcessState: "running", ProcessObservedAt: at(45 * time.Second),
		Readiness: "ready", ReadinessObservedAt: at(45 * time.Second), NodeLastSeenAt: at(45 * time.Second)}
	j := ServerOf(s, all, now)
	if j.Node.Reachability.State != "reachable" || j.Process.State != "running" || j.Readiness.State != "ready" {
		t.Errorf("45 s: %+v", j)
	}
	s.NodeLastSeenAt = at(46 * time.Second)
	j = ServerOf(s, all, now)
	if j.Node.Reachability.State != "unreachable" || j.Readiness.State != "unknown" || j.Readiness.LastReported != "ready" {
		t.Errorf("node 46 s: %+v", j)
	}
	// Process has its own clock: fresh process stays fresh here.
	if j.Process.State != "running" {
		t.Errorf("process with a fresh observation: %+v", j.Process)
	}
	s.NodeLastSeenAt = at(0)
	s.ReadinessObservedAt = at(46 * time.Second)
	if j = ServerOf(s, all, now); j.Readiness.State != "unknown" || j.Readiness.LastReported != "ready" {
		t.Errorf("readiness 46 s: %+v", j.Readiness)
	}
	s.Readiness = "unknown"
	if j = ServerOf(s, all, now); j.Readiness.State != "unknown" || j.Readiness.LastReported != "" {
		t.Errorf("stored unknown: %+v", j.Readiness)
	}
}

// A value with no observation time at all is never shown as current,
// whatever the stored state says.
func TestNeverObservedIsUnknown(t *testing.T) {
	now := time.Date(2026, 10, 15, 3, 0, 0, 0, time.UTC)
	j := ServerOf(store.Server{ProcessState: "running", Readiness: "ready"}, all, now)
	if j.Process.State != "unknown" || j.Process.LastReported != "running" ||
		j.Readiness.State != "unknown" || j.Readiness.LastReported != "ready" {
		t.Errorf("never observed: %+v %+v", j.Process, j.Readiness)
	}
}
