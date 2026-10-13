// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/sharddevs/waystone_panel/internal/ids"
	"github.com/sharddevs/waystone_panel/internal/store"
)

// The actions the API checks permissions for. The three server actions
// are the store's Action values; these are the rest.
const (
	actionServerList       = "server.list"
	actionOperationList    = "operation.list"
	actionOperationRead    = "operation.read"
	actionOperationResolve = "operation.resolve"
)

// dbTimeout bounds the database work of one request.
const dbTimeout = 10 * time.Second

// defaultListLimit is how many operations a list returns without ?limit=.
const defaultListLimit = 50

// actionRequest is the body of start, stop and restart. It has no fields:
// the only valid body is {} (or none at all). It still exists so unknown
// fields are refused and so the fingerprint has a canonical body.
type actionRequest struct{}

// resolveRequest is the body of POST /operations/{id}/resolve.
type resolveRequest struct {
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

// run is how every API handler touches the database. It opens an
// InTenantTx for the request's actor, checks authorize(tx.Role(), action),
// and only then calls fn.
//
// If the role isn't allowed, run records the denial (store.Denial with
// reason "forbidden", the request ID, resourceType and resourceID), lets
// the transaction COMMIT so the audit row stays, and then returns a
// forbidden *apiError. Careful: returning an error from the InTenantTx
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
func (a *API) run(r *http.Request, action, resourceType, resourceID string, fn func(ctx context.Context, tx *store.Tx) error) error {
	actor, ok := actorFrom(r.Context())
	if !ok {
		return newError(CodeInternal, errors.New("missing actor"))
	}

	ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
	defer cancel()

	denied := false

	err := a.store.InTenantTx(ctx, actor, func(ctx context.Context, tx *store.Tx) error {
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

		a.logf(
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
func (a *API) handleListServers(w http.ResponseWriter, r *http.Request) {
	var servers []store.Server

	err := a.run(r, actionServerList, "server", "*", func(ctx context.Context, tx *store.Tx) error {
		var err error
		servers, err = tx.ListServers(ctx)
		return err
	})
	if err != nil {
		a.writeError(w, r, errorFor(err))
		return
	}

	result := make([]serverJSON, 0, len(servers))

	for _, server := range servers {
		result = append(result, toServerJSON(server))
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"servers": result,
	})
}

// handleAction returns the handler for POST route, which creates an
// operation for action on the server {id}:
//
//  1. {id} must pass ids.Check, else bad_request. (Check it before run,
//     so only a real UUID can end up in an audit row.)
//  2. decodeJSON into an actionRequest.
//  3. An Idempotency-Key header, if present, must pass
//     store.ValidIdempotencyKey, else invalid_idempotency_key.
//  4. Fingerprint(r.Method, route, id, the decoded body).
//  5. run with action and ("server", id), calling tx.CreateOperation.
//  6. A new operation: 202 Accepted, a Location header
//     /api/v1/operations/<op id>, and the operation as JSON. A replay
//     (same key, same request): 200 with the operation as it is now.
//
// TODO
func (a *API) handleAction(action store.Action, route string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")

		if err := ids.Check(id); err != nil {
			a.writeError(w, r, newError(CodeBadRequest, err))
			return
		}

		var body actionRequest

		if err := decodeJSON(w, r, &body); err != nil {
			a.writeError(w, r, err)
			return
		}

		key := r.Header.Get("Idempotency-Key")

		if key != "" && !store.ValidIdempotencyKey(key) {
			a.writeError(w, r, newError(CodeInvalidIdempotencyKey, nil))
			return
		}

		fingerprint, err := Fingerprint(r.Method, route, id, body)
		if err != nil {
			a.writeError(w, r, newError(CodeInternal, err))
			return
		}

		var operation store.Operation
		var replayed bool

		err = a.run(r, string(action), "server", id, func(ctx context.Context, tx *store.Tx) error {
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
			a.writeError(w, r, errorFor(err))
			return
		}

		status := http.StatusAccepted

		if replayed {
			status = http.StatusOK
		} else {
			w.Header().Set("Location", "/api/v1/operations/"+operation.ID)
		}

		writeJSON(w, status, toOperationJSON(operation))
	}
}

// handleListOperations: GET /api/v1/operations?server_id=&limit= → 200
// {"operations": [...]}, newest first. server_id is optional. limit is
// optional (default defaultListLimit); a value that isn't a number is
// bad_request, and the store checks the range. The denial's resource is
// ("server", server_id), or ("server", "*") without one; a server_id that
// isn't a UUID is bad_request before run.
//
// TODO
func (a *API) handleListOperations(w http.ResponseWriter, r *http.Request) {
	serverID := r.URL.Query().Get("server_id")

	if serverID != "" {
		if err := ids.Check(serverID); err != nil {
			a.writeError(w, r, newError(CodeBadRequest, err))
			return
		}
	}

	limit := defaultListLimit

	if value := r.URL.Query().Get("limit"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 || n > store.MaxListLimit {
			a.writeError(w, r, newError(CodeBadRequest, nil))
			return
		}
		limit = n
	}

	var operations []store.Operation
	resourceID := "*"
	if serverID != "" {
		resourceID = serverID
	}
	err := a.run(r, actionOperationList, "server", resourceID, func(ctx context.Context, tx *store.Tx) error {
		var err error
		operations, err = tx.ListOperations(ctx, serverID, limit)
		return err
	})
	if err != nil {
		a.writeError(w, r, errorFor(err))
		return
	}

	result := make([]operationJSON, 0, len(operations))

	for _, operation := range operations {
		result = append(result, toOperationJSON(operation))
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
func (a *API) handleGetOperation(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	if err := ids.Check(id); err != nil {
		a.writeError(w, r, newError(CodeBadRequest, err))
		return
	}

	var operation store.Operation

	err := a.run(r, actionOperationRead, "operation", id, func(ctx context.Context, tx *store.Tx) error {
		var err error
		operation, err = tx.GetOperation(ctx, id)
		return err
	})
	if err != nil {
		a.writeError(w, r, errorFor(err))
		return
	}

	writeJSON(w, http.StatusOK, toOperationJSON(operation))
}

// handleResolve: POST /api/v1/operations/{id}/resolve with a
// resolveRequest → 200 with the updated operation (decision O). The store
// checks the outcome and note and the operation's state; this handler
// only validates the ID, decodes the body and maps the errors.
//
// TODO
func (a *API) handleResolve(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	if err := ids.Check(id); err != nil {
		a.writeError(w, r, newError(CodeBadRequest, err))
		return
	}

	var body resolveRequest

	if err := decodeJSON(w, r, &body); err != nil {
		a.writeError(w, r, err)
		return
	}

	if body.Outcome == "" {
		a.writeError(w, r, newError(CodeBadRequest, nil))
		return
	}

	var operation store.Operation

	err := a.run(r, actionOperationResolve, "operation", id, func(ctx context.Context, tx *store.Tx) error {
		var err error
		operation, err = tx.ResolveOperation(ctx, id, store.OperationState(body.Outcome), body.Note)
		return err
	})
	if err != nil {
		a.writeError(w, r, errorFor(err))
		return
	}

	writeJSON(w, http.StatusOK, toOperationJSON(operation))
}

// handleNotFound answers every path (and method) no route matches, so
// that even those errors have the usual JSON shape.
func (a *API) handleNotFound(w http.ResponseWriter, r *http.Request) {
	a.writeError(w, r, newError(CodeNotFound, nil))
}
