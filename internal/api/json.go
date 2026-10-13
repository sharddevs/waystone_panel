// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"time"

	"github.com/sharddevs/waystone_panel/internal/store"
)

// The JSON shapes the API sends. They are separate from the store's types
// on purpose: renaming a Go field must never change the public API, and a
// store field (say, TenantID) is never sent unless it's listed here.

type actorJSON struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Name string `json:"name"`
}

type resolutionJSON struct {
	At      time.Time `json:"at"`
	Outcome string    `json:"outcome"`
	By      actorJSON `json:"by"`
	Note    string    `json:"note"`
}

type operationJSON struct {
	ID           string          `json:"id"`
	ServerID     string          `json:"server_id"`
	NodeID       string          `json:"node_id"`
	Action       string          `json:"action"`
	State        string          `json:"state"`
	Phase        string          `json:"phase,omitempty"`
	Actor        actorJSON       `json:"actor"`
	ErrorCode    string          `json:"error_code,omitempty"`
	ErrorSummary string          `json:"error_summary,omitempty"`
	RequestedAt  time.Time       `json:"requested_at"`
	StartedAt    *time.Time      `json:"started_at"`
	FinishedAt   *time.Time      `json:"finished_at"`
	Resolution   *resolutionJSON `json:"resolution"`
}

type dimensionJSON struct {
	State      string     `json:"state"`
	Since      *time.Time `json:"since"`
	ObservedAt *time.Time `json:"observed_at"`
}

type serverJSON struct {
	ID           string        `json:"id"`
	NodeID       string        `json:"node_id"`
	Name         string        `json:"name"`
	Profile      string        `json:"profile"`
	DesiredState string        `json:"desired_state"`
	Process      dimensionJSON `json:"process"`
	Readiness    dimensionJSON `json:"readiness"`
}

func toActorJSON(a store.Actor) actorJSON {
	return actorJSON{Type: a.Type, ID: a.ID, Name: a.Name}
}

func toOperationJSON(op store.Operation) operationJSON {
	out := operationJSON{
		ID:           op.ID,
		ServerID:     op.ResourceID,
		NodeID:       op.NodeID,
		Action:       string(op.Action),
		State:        string(op.State),
		Phase:        op.Phase,
		Actor:        toActorJSON(op.Actor),
		ErrorCode:    op.ErrorCode,
		ErrorSummary: op.ErrorSummary,
		RequestedAt:  op.RequestedAt,
		StartedAt:    op.StartedAt,
		FinishedAt:   op.FinishedAt,
	}
	if r := op.Resolution; r != nil {
		out.Resolution = &resolutionJSON{At: r.At, Outcome: string(r.Outcome), By: toActorJSON(r.By), Note: r.Note}
	}
	return out
}

func toServerJSON(s store.Server) serverJSON {
	return serverJSON{
		ID:           s.ID,
		NodeID:       s.NodeID,
		Name:         s.Name,
		Profile:      s.Profile,
		DesiredState: s.DesiredState,
		Process:      dimensionJSON{State: s.ProcessState, Since: s.ProcessSince, ObservedAt: s.ProcessObservedAt},
		Readiness:    dimensionJSON{State: s.Readiness, Since: s.ReadinessSince, ObservedAt: s.ReadinessObservedAt},
	}
}
