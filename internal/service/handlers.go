// SPDX-License-Identifier: AGPL-3.0-only

package service

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/sharddevs/waystone_panel/internal/ids"
	"github.com/sharddevs/waystone_panel/internal/store"
	"github.com/sharddevs/waystone_panel/internal/view"
)

// The actions the API checks permissions for. The three server actions
// are the store's Action values; these are the rest.
const (
	actionServerList       = "server.list"
	actionServerRead       = "server.read"
	actionOperationList    = "operation.list"
	actionOperationRead    = "operation.read"
	actionOperationResolve = "operation.resolve"
)

// dbTimeout bounds the database work of one request.
const dbTimeout = 10 * time.Second

// defaultListLimit is how many operations a list returns without ?limit=.
const defaultListLimit = 50

// ActionRequest is the body of start, stop and restart. It has no fields:
// the only valid body is {} (or none at all). It still exists so unknown
// fields are refused and so the fingerprint has a canonical body.
type ActionRequest struct{}

// ResolveRequest is the body of POST /operations/{id}/resolve.
type ResolveRequest struct {
	Outcome string `json:"outcome"`
	Note    string `json:"note"`
}

// authorize reports whether role may perform action. In stage 4 an admin
// may do everything the API offers, and no other role may do anything.
//
// TODO: one line, but keep it a function: stage 5 adds roles here.
func authorize(role, action string) bool {
	return role == "admin"
}

// permits is authorize for one role, in the form view.ServerOf takes:
// the actions list leaves out what the role may not do (B8).
func permits(role string) func(store.Action) bool {
	return func(action store.Action) bool { return authorize(role, string(action)) }
}

// run is how every API handler touches the database. It opens an
// InTenantTx for the request's actor, checks authorize(tx.Role(), action),
// and only then calls fn.
//
// If the role isn't allowed, run records the denial (store.Denial with
// reason "forbidden", the request ID, resourceType and resourceID), lets
// the transaction COMMIT so the audit row stays, and then returns a
// forbidden *Error. Careful: returning an error from the InTenantTx
// callback would roll the audit row back too. So the callback records the
// denial, sets a flag and returns nil; run checks the flag afterwards.
//
// An actor with no membership, or with several, has no tenant to write an
// audit row into. That is the one deliberate exception to I6: when
// InTenantTx fails with store.ErrNoMembership or
// store.ErrAmbiguousMembership, run logs one structured line instead,
// exactly in this form, and still returns the error:
//
//	security_event=denied request_id=<id> actor=<type>:<id> action=<action> resource=<type>:<id> reason=no_membership
//
// (reason=ambiguous_membership for several memberships). Nothing from the
// request body or headers goes into it. Never pick a tenant to audit into.
//
// The whole thing runs under dbTimeout. Any error comes back as it is;
// the caller passes it through errorFor.
//
// TODO: implement as described.
func (s *Service) run(r *http.Request, action, resourceType, resourceID string, fn func(ctx context.Context, tx *store.Tx) error) error {
	actor, ok := actorFrom(r.Context())
	if !ok {
		return newError(CodeInternal, errors.New("missing actor"))
	}

	ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
	defer cancel()

	denied := false

	err := s.store.InTenantTx(ctx, actor, func(ctx context.Context, tx *store.Tx) error {
		if !authorize(tx.Role(), action) {
			err := tx.RecordDenial(ctx, store.Denial{
				Action:       action,
				ResourceType: resourceType,
				ResourceID:   resourceID,
				Reason:       "forbidden",
				RequestID:    requestIDFrom(r.Context()),
			})
			if err != nil {
				return err
			}

			denied = true
			return nil
		}

		return fn(ctx, tx)
	})

	if errors.Is(err, store.ErrNoMembership) || errors.Is(err, store.ErrAmbiguousMembership) {
		reason := "no_membership"
		if errors.Is(err, store.ErrAmbiguousMembership) {
			reason = "ambiguous_membership"
		}

		s.logf(
			"security_event=denied request_id=%s actor=%s:%s action=%s resource=%s:%s reason=%s",
			requestIDFrom(r.Context()),
			actor.Type,
			actor.ID,
			action,
			resourceType,
			resourceID,
			reason,
		)
	}

	if err != nil {
		return err
	}

	if denied {
		return newError(CodeForbidden, nil)
	}

	return nil
}

// handleListServers: GET /api/v1/servers → 200 {"servers": [...]}.
// The denial's resource is ("server", "*") because no single server is
// involved.
//
// TODO
func (s *Service) handleListServers(w http.ResponseWriter, r *http.Request) {
	if err := checkQuery(r); err != nil {
		s.writeError(w, r, err)
		return
	}

	var servers []store.Server
	var role string

	err := s.run(r, actionServerList, "server", "*", func(ctx context.Context, tx *store.Tx) error {
		var err error
		role = tx.Role()
		servers, err = tx.ListServers(ctx)
		return err
	})
	if err != nil {
		s.writeError(w, r, errorFor(err))
		return
	}

	result := make([]view.Server, 0, len(servers))
	now := s.now()

	for _, server := range servers {
		result = append(result, view.ServerOf(server, permits(role), now))
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"servers": result,
	})
}

// handleGetServer: GET /api/v1/servers/{id} → 200 with the server, in the
// same shape as the list (A2). A bad ID is bad_request before run; a
// server in another tenant is not_found, the same as a missing one.
func (s *Service) handleGetServer(w http.ResponseWriter, r *http.Request) {
	if err := checkQuery(r); err != nil {
		s.writeError(w, r, err)
		return
	}

	id := r.PathValue("id")

	if err := ids.Check(id); err != nil {
		s.writeError(w, r, newError(CodeBadRequest, err))
		return
	}

	var server store.Server
	var role string

	err := s.run(r, actionServerRead, "server", id, func(ctx context.Context, tx *store.Tx) error {
		var err error
		role = tx.Role()
		server, err = tx.GetServer(ctx, id)
		return err
	})
	if err != nil {
		s.writeError(w, r, errorFor(err))
		return
	}

	writeJSON(w, http.StatusOK, view.ServerOf(server, permits(role), s.now()))
}

// handleAction returns the handler for POST route, which creates an
// operation for action on the server {id}:
//
//  1. {id} must pass ids.Check, else bad_request. (Check it before run,
//     so only a real UUID can end up in an audit row.)
//  2. decodeJSON into an ActionRequest.
//  3. An Idempotency-Key header, if present, must pass
//     store.ValidIdempotencyKey, else invalid_idempotency_key.
//  4. Fingerprint(r.Method, route, id, the decoded body).
//  5. run with action and ("server", id), calling tx.CreateOperation.
//  6. A new operation: 202 Accepted, a Location header
//     /api/v1/operations/<op id>, and the operation as JSON, after
//     OnOperation has been told about it. A replay (same key, same
//     request): 200 with the operation as it is now, and no OnOperation.
//
// TODO
func (s *Service) handleAction(action store.Action, route string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := checkQuery(r); err != nil {
			s.writeError(w, r, err)
			return
		}

		id := r.PathValue("id")

		if err := ids.Check(id); err != nil {
			s.writeError(w, r, newError(CodeBadRequest, err))
			return
		}

		var body ActionRequest

		if err := decodeJSON(w, r, &body); err != nil {
			s.writeError(w, r, err)
			return
		}

		key := r.Header.Get("Idempotency-Key")

		if key != "" && !store.ValidIdempotencyKey(key) {
			s.writeError(w, r, newError(CodeInvalidIdempotencyKey, nil))
			return
		}

		fingerprint, err := Fingerprint(r.Method, route, id, body)
		if err != nil {
			s.writeError(w, r, newError(CodeInternal, err))
			return
		}

		var operation store.Operation
		var replayed bool

		err = s.run(r, string(action), "server", id, func(ctx context.Context, tx *store.Tx) error {
			var err error

			operation, replayed, err = tx.CreateOperation(ctx, store.CreateOperationInput{
				ServerID:       id,
				Action:         action,
				IdempotencyKey: key,
				RequestSHA256:  fingerprint,
			})

			return err
		})
		if err != nil {
			s.writeError(w, r, errorFor(err))
			return
		}

		status := http.StatusAccepted

		if replayed {
			status = http.StatusOK
		} else {
			w.Header().Set("Location", "/api/v1/operations/"+operation.ID)
			// Committed by now, so the agent's claim can see it.
			if s.onOperation != nil {
				s.onOperation(operation)
			}
		}

		writeJSON(w, status, view.OperationOf(operation))
	}
}

// handleListOperations: GET /api/v1/operations?server_id=&limit= → 200
// {"operations": [...]}, newest first. server_id is optional. limit is
// optional (default defaultListLimit); a value that isn't a number is
// bad_request, and the store checks the range. The denial's resource is
// ("server", server_id), or ("server", "*") without one; a server_id that
// isn't a UUID is bad_request before run. Any other query parameter
// is bad_request too (V4).
//
// TODO
func (s *Service) handleListOperations(w http.ResponseWriter, r *http.Request) {
	if err := checkQuery(r, "server_id", "limit"); err != nil {
		s.writeError(w, r, err)
		return
	}

	serverID := r.URL.Query().Get("server_id")

	if serverID != "" {
		if err := ids.Check(serverID); err != nil {
			s.writeError(w, r, newError(CodeBadRequest, err))
			return
		}
	}

	limit := defaultListLimit

	if value := r.URL.Query().Get("limit"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 || n > store.MaxListLimit {
			s.writeError(w, r, newError(CodeBadRequest, nil))
			return
		}
		limit = n
	}

	var operations []store.Operation
	resourceID := "*"
	if serverID != "" {
		resourceID = serverID
	}
	err := s.run(r, actionOperationList, "server", resourceID, func(ctx context.Context, tx *store.Tx) error {
		var err error
		operations, err = tx.ListOperations(ctx, serverID, limit)
		return err
	})
	if err != nil {
		s.writeError(w, r, errorFor(err))
		return
	}

	result := make([]view.Operation, 0, len(operations))

	for _, operation := range operations {
		result = append(result, view.OperationOf(operation))
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"operations": result,
	})
}

// handleGetOperation: GET /api/v1/operations/{id} → 200 with the
// operation. A bad ID is bad_request; one in another tenant is not_found,
// the same as a missing one.
//
// TODO
func (s *Service) handleGetOperation(w http.ResponseWriter, r *http.Request) {
	if err := checkQuery(r); err != nil {
		s.writeError(w, r, err)
		return
	}

	id := r.PathValue("id")

	if err := ids.Check(id); err != nil {
		s.writeError(w, r, newError(CodeBadRequest, err))
		return
	}

	var operation store.Operation

	err := s.run(r, actionOperationRead, "operation", id, func(ctx context.Context, tx *store.Tx) error {
		var err error
		operation, err = tx.GetOperation(ctx, id)
		return err
	})
	if err != nil {
		s.writeError(w, r, errorFor(err))
		return
	}

	writeJSON(w, http.StatusOK, view.OperationOf(operation))
}

// handleResolve: POST /api/v1/operations/{id}/resolve with a
// ResolveRequest → 200 with the updated operation (decision O). The store
// checks the outcome and note and the operation's state; this handler
// only validates the ID, decodes the body and maps the errors.
//
// TODO
func (s *Service) handleResolve(w http.ResponseWriter, r *http.Request) {
	if err := checkQuery(r); err != nil {
		s.writeError(w, r, err)
		return
	}

	id := r.PathValue("id")

	if err := ids.Check(id); err != nil {
		s.writeError(w, r, newError(CodeBadRequest, err))
		return
	}

	var body ResolveRequest

	if err := decodeJSON(w, r, &body); err != nil {
		s.writeError(w, r, err)
		return
	}

	if body.Outcome == "" {
		s.writeError(w, r, newError(CodeBadRequest, nil))
		return
	}

	var operation store.Operation

	err := s.run(r, actionOperationResolve, "operation", id, func(ctx context.Context, tx *store.Tx) error {
		var err error
		operation, err = tx.ResolveOperation(ctx, id, store.OperationState(body.Outcome), body.Note)
		return err
	})
	if err != nil {
		s.writeError(w, r, errorFor(err))
		return
	}

	writeJSON(w, http.StatusOK, view.OperationOf(operation))
}
