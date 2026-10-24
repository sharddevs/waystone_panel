// SPDX-License-Identifier: AGPL-3.0-only

package view

import (
	"time"

	"github.com/sharddevs/waystone_panel/internal/store"
)

// NodeDetail is one node on the Nodes page (6b, D9).
type NodeDetail struct {
	ID           string       `json:"id"`
	Name         string       `json:"name"`
	Reachability Reachability `json:"reachability"`
	// Link is how the agent connects: "local" on agent.sock, "mtls" for
	// an enrolled node over TCP.
	Link         string     `json:"link"`
	AgentVersion *string    `json:"agent_version"`
	Capabilities []string   `json:"capabilities"`
	EnrolledAt   *time.Time `json:"enrolled_at"`
	RevokedAt    *time.Time `json:"revoked_at"`
	ServerCount  int        `json:"server_count"`
	Actions      []Action   `json:"actions"`
}

// The node actions, as the actions list names them, and their reasons.
const (
	ActionNodeRevoke     = "node.revoke"
	reasonAlreadyRevoked = "already_revoked"
)

// NodeOf converts a node at now. A revoked node is unreachable whatever
// it last reported: its stream is cut when it is revoked, and it can't
// connect again. allowed reports whether the actor may perform an action
// name; revoke is left out if it may not, and refused with
// already_revoked once the node is.
func NodeOf(n store.Node, allowed func(string) bool, now time.Time) NodeDetail {
	reach := Reachability{State: "reachable", LastSeenAt: n.LastSeenAt}
	if n.RevokedAt != nil || !store.NodeReachable(n.LastSeenAt, now) {
		reach.State = "unreachable"
	}
	link := "local"
	if n.Pinned {
		link = "mtls"
	}
	caps := n.Capabilities
	if caps == nil {
		caps = []string{}
	}
	actions := []Action{}
	if allowed(ActionNodeRevoke) {
		a := Action{Action: "revoke", Allowed: n.RevokedAt == nil}
		if !a.Allowed {
			a.Reason = reasonAlreadyRevoked
		}
		actions = append(actions, a)
	}
	return NodeDetail{
		ID: n.ID, Name: n.Name, Reachability: reach, Link: link,
		AgentVersion: n.AgentVersion, Capabilities: caps,
		EnrolledAt: n.EnrolledAt, RevokedAt: n.RevokedAt,
		ServerCount: n.ServerCount, Actions: actions,
	}
}

// Enrollment is a token as the list shows it: never its secret.
type Enrollment struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	CreatedBy Actor      `json:"created_by"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt time.Time  `json:"expires_at"`
	UsedAt    *time.Time `json:"used_at"`
	// NodeID is the node it became; null if unused, or if the key that
	// used it was already a node's.
	NodeID *string `json:"node_id"`
	// State is pending, used or expired.
	State string `json:"state"`
}

// EnrollmentOf converts a token at now. It is expired from its expiry
// time on, which is when enroll_node stops taking it.
func EnrollmentOf(e store.Enrollment, now time.Time) Enrollment {
	out := Enrollment{ID: e.ID, Name: e.Name, CreatedBy: ActorOf(e.CreatedBy), CreatedAt: e.CreatedAt,
		ExpiresAt: e.ExpiresAt, UsedAt: e.UsedAt, State: "pending"}
	if e.NodeID != "" {
		id := e.NodeID
		out.NodeID = &id
	}
	switch {
	case e.UsedAt != nil:
		out.State = "used"
	case !now.Before(e.ExpiresAt):
		out.State = "expired"
	}
	return out
}

// NewEnrollment is the answer to creating a token, the only time the
// bundle exists anywhere (D2): the bundle to paste and the command that
// asks for it.
type NewEnrollment struct {
	Enrollment Enrollment `json:"enrollment"`
	Bundle     string     `json:"bundle"`
	Command    string     `json:"command"`
}

// EnrollCommand is what runs on the new machine. The bundle is pasted at
// its prompt, never put on the command line (6H2).
const EnrollCommand = "sudo waystone-agent enroll"
