// SPDX-License-Identifier: AGPL-3.0-only

package service

import (
	"context"
	"net/http"
	"net/netip"

	"github.com/sharddevs/waystone_panel/internal/agenttls"
	"github.com/sharddevs/waystone_panel/internal/enrollbundle"
	"github.com/sharddevs/waystone_panel/internal/ids"
	"github.com/sharddevs/waystone_panel/internal/store"
	"github.com/sharddevs/waystone_panel/internal/view"
)

// The Nodes API (6b, D2, D5, D9): list and read nodes, revoke one, and
// make and list enrollment tokens. Admins only, like everything else.

const (
	actionNodeList             = "node.list"
	actionNodeRead             = "node.read"
	actionNodeRevoke           = view.ActionNodeRevoke
	actionNodeEnrollmentList   = "node.enrollment.list"
	actionNodeEnrollmentCreate = "node.enrollment.create"
)

// AgentLink is what a bundle tells a new node: the controller's agent
// listener and its key pin. The address is the TCP listener's own, from
// its socket unit, never from a request (6H3).
type AgentLink struct {
	Addr netip.AddrPort
	Pin  agenttls.Pin
}

// RevokeRequest is the body of POST /nodes/{id}/revoke: {} or none.
type RevokeRequest struct{}

// EnrollmentRequest is the body of POST /nodes/enrollments.
type EnrollmentRequest struct {
	Name string `json:"name"`
}

// nodePermits is authorize for one role, as view.NodeOf takes it.
func nodePermits(role string) func(string) bool {
	return func(action string) bool { return authorize(role, action) }
}

// handleListNodes: GET /api/v1/nodes → 200 {"nodes": [...]}.
func (s *Service) handleListNodes(w http.ResponseWriter, r *http.Request) {
	if err := checkQuery(r); err != nil {
		s.writeError(w, r, err)
		return
	}
	var nodes []store.Node
	var role string
	err := s.run(r, actionNodeList, "node", "*", func(ctx context.Context, tx *store.Tx) error {
		var err error
		role = tx.Role()
		nodes, err = tx.ListNodes(ctx)
		return err
	})
	if err != nil {
		s.writeError(w, r, errorFor(err))
		return
	}
	now := s.now()
	result := make([]view.NodeDetail, 0, len(nodes))
	for _, n := range nodes {
		result = append(result, view.NodeOf(n, nodePermits(role), now))
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": result})
}

// handleGetNode: GET /api/v1/nodes/{id} → 200 with the node, the same
// shape as in the list. Another tenant's node is not_found.
func (s *Service) handleGetNode(w http.ResponseWriter, r *http.Request) {
	if err := checkQuery(r); err != nil {
		s.writeError(w, r, err)
		return
	}
	id := r.PathValue("id")
	if err := ids.Check(id); err != nil {
		s.writeError(w, r, newError(CodeBadRequest, err))
		return
	}
	var node store.Node
	var role string
	err := s.run(r, actionNodeRead, "node", id, func(ctx context.Context, tx *store.Tx) error {
		var err error
		role = tx.Role()
		node, err = tx.GetNode(ctx, id)
		return err
	})
	if err != nil {
		s.writeError(w, r, errorFor(err))
		return
	}
	writeJSON(w, http.StatusOK, view.NodeOf(node, nodePermits(role), s.now()))
}

// handleRevokeNode: POST /api/v1/nodes/{id}/revoke → 200 with the node,
// now revoked (D5). The store records the audit row; once it has
// committed, OnNodeRevoked cuts the node's live stream. Revoking a node
// that already is changes nothing and answers the same, so a retry is
// safe without an Idempotency-Key.
func (s *Service) handleRevokeNode(w http.ResponseWriter, r *http.Request) {
	if err := checkQuery(r); err != nil {
		s.writeError(w, r, err)
		return
	}
	id := r.PathValue("id")
	if err := ids.Check(id); err != nil {
		s.writeError(w, r, newError(CodeBadRequest, err))
		return
	}
	var body RevokeRequest
	if err := decodeJSON(w, r, &body); err != nil {
		s.writeError(w, r, err)
		return
	}
	var node store.Node
	var role string
	var already bool
	err := s.run(r, actionNodeRevoke, "node", id, func(ctx context.Context, tx *store.Tx) error {
		var err error
		role = tx.Role()
		if already, err = tx.RevokeNode(ctx, id, requestIDFrom(r.Context())); err != nil {
			return err
		}
		node, err = tx.GetNode(ctx, id)
		return err
	})
	if err != nil {
		s.writeError(w, r, errorFor(err))
		return
	}
	if !already && s.onNodeRevoked != nil {
		s.onNodeRevoked(id)
	}
	writeJSON(w, http.StatusOK, view.NodeOf(node, nodePermits(role), s.now()))
}

// handleListEnrollments: GET /api/v1/nodes/enrollments → 200
// {"enrollments": [...]}, newest first, never a secret.
func (s *Service) handleListEnrollments(w http.ResponseWriter, r *http.Request) {
	if err := checkQuery(r); err != nil {
		s.writeError(w, r, err)
		return
	}
	var ens []store.Enrollment
	err := s.run(r, actionNodeEnrollmentList, "node_enrollment", "*", func(ctx context.Context, tx *store.Tx) error {
		var err error
		ens, err = tx.ListEnrollments(ctx)
		return err
	})
	if err != nil {
		s.writeError(w, r, errorFor(err))
		return
	}
	now := s.now()
	result := make([]view.Enrollment, 0, len(ens))
	for _, e := range ens {
		result = append(result, view.EnrollmentOf(e, now))
	}
	writeJSON(w, http.StatusOK, map[string]any{"enrollments": result})
}

// handleCreateEnrollment: POST /api/v1/nodes/enrollments {name} → 201
// with the token, its bundle and the command (D2). This answer is the
// only place the bundle ever exists: it is sent with no-store and never
// logged (6H2). Without a usable agent listener there is nothing for a
// node to dial, so no token is made: enrollment_unavailable.
func (s *Service) handleCreateEnrollment(w http.ResponseWriter, r *http.Request) {
	if err := checkQuery(r); err != nil {
		s.writeError(w, r, err)
		return
	}
	var body EnrollmentRequest
	if err := decodeJSON(w, r, &body); err != nil {
		s.writeError(w, r, err)
		return
	}
	if !store.ValidNodeName(body.Name) {
		s.writeError(w, r, newError(CodeBadRequest, nil))
		return
	}
	var out view.NewEnrollment
	err := s.run(r, actionNodeEnrollmentCreate, "node_enrollment", "*", func(ctx context.Context, tx *store.Tx) error {
		// Checked after authorize, so only an admin learns whether the
		// controller has a listener.
		if s.agentLink == nil || !enrollbundle.UsableAddr(s.agentLink.Addr) {
			return newError(CodeEnrollmentUnavailable, nil)
		}
		e, secret, err := tx.CreateEnrollment(ctx, body.Name, requestIDFrom(r.Context()))
		if err != nil {
			return err
		}
		// Encoded before the commit, so a bundle that can't be made
		// leaves no token behind.
		bundle, err := enrollbundle.Encode(enrollbundle.Bundle{Addr: s.agentLink.Addr, Pin: s.agentLink.Pin, Secret: secret})
		if err != nil {
			return newError(CodeEnrollmentUnavailable, err)
		}
		out = view.NewEnrollment{Enrollment: view.EnrollmentOf(e, s.now()), Bundle: bundle, Command: view.EnrollCommand}
		return nil
	})
	if err != nil {
		s.writeError(w, r, errorFor(err))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, out)
}
