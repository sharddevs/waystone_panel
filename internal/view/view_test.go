// SPDX-License-Identifier: AGPL-3.0-only

package view

import (
	"encoding/json"
	"reflect"
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
	if got := ActionsFor(all, s, true); len(got) != 5 {
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
		// Set up and stopped, so the server's own state refuses nothing
		// but accepting the EULA again.
		ready := store.Server{Blocking: tc.blocking, Installed: true, EulaAccepted: true, ProcessState: "stopped"}
		for _, a := range ActionsFor(all, ready, tc.reachable) {
			if a.Action == "accept_eula" {
				continue
			}
			if a.Reason != tc.reason || a.Allowed != (tc.reason == "") {
				t.Errorf("blocking %q reachable %v: %+v, want reason %q", tc.blocking, tc.reachable, a, tc.reason)
			}
		}
	}
}

// 6c: the server's own state comes after the node and the operations,
// and is the same rule CreateOperation applies.
func TestActionsServerState(t *testing.T) {
	reasons := func(s store.Server) map[string]string {
		out := map[string]string{}
		for _, a := range ActionsFor(all, s, true) {
			if a.Allowed != (a.Reason == "") {
				t.Errorf("%+v", a)
			}
			out[a.Action] = a.Reason
		}
		return out
	}
	for name, c := range map[string]struct {
		s    store.Server
		want map[string]string
	}{
		"new": {store.Server{ProcessState: "unknown"}, map[string]string{
			"start": "not_installed", "stop": "", "restart": "not_installed", "install": "", "accept_eula": "not_installed"}},
		"installed, unknown": {store.Server{Installed: true, ProcessState: "unknown"}, map[string]string{
			"start": "eula_not_accepted", "stop": "", "restart": "eula_not_accepted", "install": "server_running", "accept_eula": ""}},
		"new, stopped": {store.Server{ProcessState: "stopped"}, map[string]string{
			"start": "not_installed", "stop": "", "restart": "not_installed", "install": "", "accept_eula": "not_installed"}},
		"installed": {store.Server{Installed: true, ProcessState: "failed"}, map[string]string{
			"start": "eula_not_accepted", "stop": "", "restart": "eula_not_accepted", "install": "", "accept_eula": ""}},
		"running": {store.Server{Installed: true, EulaAccepted: true, ProcessState: "running"}, map[string]string{
			"start": "", "stop": "", "restart": "", "install": "server_running", "accept_eula": "already_accepted"}},
		"busy": {store.Server{Blocking: store.StatePending, ProcessState: "stopped"}, map[string]string{
			"start": "operation_in_progress", "stop": "operation_in_progress", "restart": "operation_in_progress",
			"install": "operation_in_progress", "accept_eula": "operation_in_progress"}},
	} {
		if got := reasons(c.s); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %v, want %v", name, got, c.want)
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

// A revoked node is unreachable whatever it last reported, and its revoke
// is refused; a role that may not revoke sees no revoke at all.
func TestNodeOf(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	seen := now.Add(-time.Second)
	yes := func(string) bool { return true }
	n := NodeOf(store.Node{LastSeenAt: &seen}, yes, now)
	if n.Reachability.State != "reachable" || n.Link != "local" || n.Capabilities == nil ||
		len(n.Actions) != 1 || !n.Actions[0].Allowed || n.Actions[0].Reason != "" {
		t.Errorf("active node: %+v", n)
	}
	n = NodeOf(store.Node{LastSeenAt: &seen, Pinned: true, RevokedAt: &now}, yes, now)
	if n.Reachability.State != "unreachable" || n.Link != "mtls" ||
		len(n.Actions) != 1 || n.Actions[0].Allowed || n.Actions[0].Reason != "already_revoked" {
		t.Errorf("revoked node: %+v", n)
	}
	old := now.Add(-time.Hour)
	if n = NodeOf(store.Node{LastSeenAt: &old}, yes, now); n.Reachability.State != "unreachable" {
		t.Errorf("stale node: %+v", n)
	}
	if n = NodeOf(store.Node{LastSeenAt: &seen}, func(string) bool { return false }, now); len(n.Actions) != 0 {
		t.Errorf("no permission: %+v", n.Actions)
	}
}

// A token is pending until its expiry, expired from then on, and used
// once redeemed, whatever the clock says.
func TestEnrollmentOf(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	e := store.Enrollment{ExpiresAt: now.Add(time.Nanosecond)}
	if got := EnrollmentOf(e, now); got.State != "pending" || got.NodeID != nil {
		t.Errorf("pending: %+v", got)
	}
	e.ExpiresAt = now
	if got := EnrollmentOf(e, now); got.State != "expired" {
		t.Errorf("at expiry: %+v", got)
	}
	e.UsedAt, e.NodeID = &now, "n"
	if got := EnrollmentOf(e, now.Add(time.Hour)); got.State != "used" || got.NodeID == nil || *got.NodeID != "n" {
		t.Errorf("used: %+v", got)
	}
}

// A revoked node blocks every action first, even ahead of an unreachable
// node or a blocking operation, and its servers read unreachable and
// unknown even if it reported a moment ago.
func TestActionsNodeRevoked(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	seen := now.Add(-time.Second)
	j := ServerOf(store.Server{NodeRevoked: true, NodeLastSeenAt: &seen, Readiness: "ready", ReadinessObservedAt: &seen}, all, now)
	if j.Node.Reachability.State != "unreachable" || j.Readiness.State != "unknown" {
		t.Errorf("server on a revoked node: %+v %+v", j.Node, j.Readiness)
	}
	for _, a := range ActionsFor(all, store.Server{NodeRevoked: true, Blocking: store.StateUncertain}, false) {
		if a.Allowed || a.Reason != "node_revoked" {
			t.Errorf("%s: %+v", a.Action, a)
		}
	}
}
