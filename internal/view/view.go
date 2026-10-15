// SPDX-License-Identifier: AGPL-3.0-only

// Package view holds the public JSON shapes for servers and operations,
// and the rules that fill them (staleness, reachability, the actions
// list). Both sockets send exactly these shapes (E1): internal/api for
// api.sock now, internal/web for web.sock in stage 5. Only how the actor
// is identified differs, so nothing here knows about HTTP or actors.
//
// The shapes are separate from the store's types on purpose: renaming a
// Go field must never change the public API, and a store field (say,
// TenantID) is never sent unless it's listed here.
package view

import (
	"time"

	"github.com/sharddevs/waystone_panel/internal/store"
)

type Actor struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Name string `json:"name"`
}

type Resolution struct {
	At      time.Time `json:"at"`
	Outcome string    `json:"outcome"`
	By      Actor     `json:"by"`
	Note    string    `json:"note"`
}

type Operation struct {
	ID           string      `json:"id"`
	ServerID     string      `json:"server_id"`
	NodeID       string      `json:"node_id"`
	Action       string      `json:"action"`
	State        string      `json:"state"`
	Phase        string      `json:"phase,omitempty"`
	Actor        Actor       `json:"actor"`
	ErrorCode    string      `json:"error_code,omitempty"`
	ErrorSummary string      `json:"error_summary,omitempty"`
	RequestedAt  time.Time   `json:"requested_at"`
	StartedAt    *time.Time  `json:"started_at"`
	FinishedAt   *time.Time  `json:"finished_at"`
	Resolution   *Resolution `json:"resolution"`
}

type Dimension struct {
	State      string     `json:"state"`
	Since      *time.Time `json:"since"`
	ObservedAt *time.Time `json:"observed_at"`
	// LastReported is the stored state when State says "unknown" only
	// because the last report is too old (R13).
	LastReported string `json:"last_reported,omitempty"`
}

// StaleAfter is how old the last accepted observation may be before the
// API shows a state as unknown (decision M, R13). It is measured from the
// controller's receipt time, never from the agent's clock. It is the same
// threshold that makes a node unreachable.
const StaleAfter = store.NodeStaleAfter

type Reachability struct {
	State      string     `json:"state"` // reachable or unreachable
	LastSeenAt *time.Time `json:"last_seen_at"`
}

type Node struct {
	ID           string       `json:"id"`
	Name         string       `json:"name"`
	Reachability Reachability `json:"reachability"`
}

type Profile struct {
	ID      string  `json:"id"`
	Version *string `json:"version"`
}

// Action says whether the actor may start, stop or restart the server
// right now (B8). It is a hint for the UI; the POST still checks
// everything itself. Reason is set only when Allowed is false.
type Action struct {
	Action  string `json:"action"`
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason,omitempty"`
}

type Server struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Node         Node      `json:"node"`
	Profile      Profile   `json:"profile"`
	Runtime      string    `json:"runtime"`
	DesiredState string    `json:"desired_state"`
	Process      Dimension `json:"process"`
	Readiness    Dimension `json:"readiness"`
	Actions      []Action  `json:"actions"`
}

func ActorOf(a store.Actor) Actor {
	return Actor{Type: a.Type, ID: a.ID, Name: a.Name}
}

func OperationOf(op store.Operation) Operation {
	out := Operation{
		ID:           op.ID,
		ServerID:     op.ResourceID,
		NodeID:       op.NodeID,
		Action:       string(op.Action),
		State:        string(op.State),
		Phase:        op.Phase,
		Actor:        ActorOf(op.Actor),
		ErrorCode:    op.ErrorCode,
		ErrorSummary: op.ErrorSummary,
		RequestedAt:  op.RequestedAt,
		StartedAt:    op.StartedAt,
		FinishedAt:   op.FinishedAt,
	}
	if r := op.Resolution; r != nil {
		out.Resolution = &Resolution{At: r.At, Outcome: string(r.Outcome), By: ActorOf(r.By), Note: r.Note}
	}
	return out
}

// dimension builds one state dimension. When stale is true the state is
// shown as "unknown", with the stored value in last_reported (unless that
// was "unknown" already). The database keeps the real value; only the
// response says unknown.
func dimension(state string, since, observedAt *time.Time, stale bool) Dimension {
	d := Dimension{State: state, Since: since, ObservedAt: observedAt}
	if stale {
		if d.State != string(store.ProcessUnknown) {
			d.LastReported = d.State
		}
		d.State = string(store.ProcessUnknown)
	}
	return d
}

// isStale reports whether an observation at observedAt is missing or older
// than StaleAfter at now.
func isStale(observedAt *time.Time, now time.Time) bool {
	return observedAt == nil || now.Sub(*observedAt) > StaleAfter
}

// serverActions are the actions B8 lists, in order, with the name the
// JSON uses for each.
var serverActions = []struct {
	name   string
	action store.Action
}{
	{"start", store.ActionStart},
	{"stop", store.ActionStop},
	{"restart", store.ActionRestart},
}

// The B8 reason codes. Fixed strings the UI maps to text.
const (
	reasonNodeUnreachable     = "node_unreachable"
	reasonPreviousUncertain   = "previous_operation_uncertain"
	reasonOperationInProgress = "operation_in_progress"
)

// ActionsFor lists what the actor may do to s (B8). allowed reports
// whether the actor's role may perform an action at all; an action it
// may not is left out. The others carry allowed=false and the first
// reason that applies, in this order: node unreachable, an unresolved
// uncertain operation, an operation in progress. These are exactly the
// refusals CreateOperation makes, in the same order.
func ActionsFor(allowed func(store.Action) bool, s store.Server, reachable bool) []Action {
	reason := ""
	switch {
	case !reachable:
		reason = reasonNodeUnreachable
	case s.Blocking == store.StateUncertain:
		reason = reasonPreviousUncertain
	case s.Blocking != "":
		reason = reasonOperationInProgress
	}
	out := []Action{}
	for _, a := range serverActions {
		if !allowed(a.action) {
			continue
		}
		out = append(out, Action{Action: a.name, Allowed: reason == "", Reason: reason})
	}
	return out
}

// ServerOf converts a server for the API at now, for an actor whose
// permissions allowed describes (see ActionsFor).
//
//   - The node is unreachable when its last_seen_at is missing or older
//     than StaleAfter (B2).
//   - Process and readiness are shown as "unknown" when their last
//     accepted observation is missing or older than StaleAfter (B6, B7).
//     Readiness is also unknown while the node is unreachable (B7).
func ServerOf(s store.Server, allowed func(store.Action) bool, now time.Time) Server {
	reachable := store.NodeReachable(s.NodeLastSeenAt, now)
	reach := Reachability{State: "reachable", LastSeenAt: s.NodeLastSeenAt}
	if !reachable {
		reach.State = "unreachable"
	}
	return Server{
		ID:           s.ID,
		Name:         s.Name,
		Node:         Node{ID: s.NodeID, Name: s.NodeName, Reachability: reach},
		Profile:      Profile{ID: s.Profile, Version: s.ProfileVersion},
		Runtime:      s.Runtime,
		DesiredState: s.DesiredState,
		Process:      dimension(s.ProcessState, s.ProcessSince, s.ProcessObservedAt, isStale(s.ProcessObservedAt, now)),
		Readiness: dimension(s.Readiness, s.ReadinessSince, s.ReadinessObservedAt,
			isStale(s.ReadinessObservedAt, now) || !reachable),
		Actions: ActionsFor(allowed, s, reachable),
	}
}
