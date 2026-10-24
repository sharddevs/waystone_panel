// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"errors"
	"fmt"
)

// The errors the store returns. The API maps each one to a stable public
// code; none of them carries database text, so nothing about the schema
// or the host leaks through them.
var (
	// ErrInvalidActor: the actor's type, ID or name is malformed. Checked
	// before any database work.
	ErrInvalidActor = errors.New("invalid actor")
	// ErrNoMembership: the actor belongs to no tenant.
	ErrNoMembership = errors.New("actor has no membership")
	// ErrAmbiguousMembership: the actor belongs to more than one tenant.
	// Stage 4 refuses that until the UI can choose an organization (I6).
	ErrAmbiguousMembership = errors.New("actor has more than one membership")
	// ErrNotAdmin: the transaction's actor isn't an admin of its tenant.
	ErrNotAdmin = errors.New("actor is not an admin")
	// ErrInvalidInput: a request field is malformed (bad UUID, unknown
	// action, bad idempotency key, note too long, ...).
	ErrInvalidInput = errors.New("invalid input")
	// ErrNotFound: no such row in the actor's tenant. A row in another
	// tenant is reported the same way, so its existence doesn't leak.
	ErrNotFound = errors.New("not found")
	// ErrOperationInProgress: the server already has a pending, running or
	// unresolved uncertain operation (I3). Returned as an *InProgressError.
	ErrOperationInProgress = errors.New("operation in progress")
	// ErrIdempotencyKeyReused: the key was used before for a different
	// request (C1).
	ErrIdempotencyKeyReused = errors.New("idempotency key reused for a different request")
	// ErrNotUncertain: only uncertain operations can be resolved.
	ErrNotUncertain = errors.New("operation is not uncertain")
	// ErrAlreadyResolved: an uncertain operation is resolved at most once.
	ErrAlreadyResolved = errors.New("operation already resolved")
	// ErrNodeUnreachable: the server's node hasn't been seen within
	// NodeStaleAfter, so a new operation is refused instead of queued (V1).
	ErrNodeUnreachable = errors.New("node unreachable")
	// ErrNodeRevoked: the server's node has been revoked, so it can't run
	// anything new (D5).
	ErrNodeRevoked = errors.New("node revoked")
	// ErrPortTaken: another server on the node has the port, or it is the
	// controller's own agent port (6c-1).
	ErrPortTaken = errors.New("port taken")
	// ErrServerRunning: an install needs the server stopped.
	ErrServerRunning = errors.New("server is not stopped")
	// ErrNotInstalled: the server hasn't been installed yet.
	ErrNotInstalled = errors.New("server not installed")
	// ErrEulaNotAccepted: start and restart wait for the Minecraft EULA.
	ErrEulaNotAccepted = errors.New("eula not accepted")
	// ErrAlreadyAccepted: the EULA was accepted already.
	ErrAlreadyAccepted = errors.New("eula already accepted")
)

// InProgressError says which operation blocks a new one. errors.Is(err,
// ErrOperationInProgress) is true for it.
type InProgressError struct {
	OperationID string
	State       OperationState
}

func (e *InProgressError) Error() string {
	return fmt.Sprintf("%v: %s is %s", ErrOperationInProgress, e.OperationID, e.State)
}

// Is makes errors.Is(err, ErrOperationInProgress) work.
func (e *InProgressError) Is(target error) bool {
	return target == ErrOperationInProgress
}
