// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/sharddevs/waystone_panel/internal/ids"
)

// Action is what an operation does to its server.
type Action string

const (
	ActionStart   Action = "server.start"
	ActionStop    Action = "server.stop"
	ActionRestart Action = "server.restart"
)

// OperationState is where an operation is in its life. These are the
// v0.1 states; the database CHECK lists the same ones.
type OperationState string

const (
	StatePending   OperationState = "pending"
	StateRunning   OperationState = "running"
	StateSucceeded OperationState = "succeeded"
	StateFailed    OperationState = "failed"
	StateUncertain OperationState = "uncertain"
)

// KeyLifetime is how long an idempotency key keeps protecting a request
// (C1). After that the key may point at a new request, but only once its
// old operation is finished or resolved.
const KeyLifetime = 24 * time.Hour

// Operation is one row of operation history. Rows are never deleted, and
// the database only lets a few columns change after the insert.
type Operation struct {
	ID           string
	TenantID     string
	ResourceType string // always "server" in stage 4
	ResourceID   string
	NodeID       string // the server's node at request time, kept even if the server moves later
	Action       Action
	State        OperationState
	Phase        string // "", or "stopping"/"starting" during a restart
	Actor        Actor  // who asked, copied at request time
	ErrorCode    string
	ErrorSummary string
	RequestedAt  time.Time
	StartedAt    *time.Time
	FinishedAt   *time.Time
	Resolution   *Resolution // nil until someone resolves an uncertain operation
}

// Resolution is what a person decided about an uncertain operation after
// checking what really happened.
type Resolution struct {
	At      time.Time
	Outcome OperationState // StateSucceeded or StateFailed
	By      Actor
	Note    string
}

// CreateOperationInput asks for one action on one server.
type CreateOperationInput struct {
	ServerID string
	Action   Action
	// IdempotencyKey is optional. With a key, RequestSHA256 must be the
	// fingerprint of the whole request, which the API computes. That is
	// how a retry with the same key is told apart from a different request.
	IdempotencyKey string
	RequestSHA256  [32]byte
}

// ValidIdempotencyKey reports whether k is 1 to 64 characters from
// [A-Za-z0-9_-]. The database has the same CHECK, but catching it here
// turns a bad key into ErrInvalidInput without a round trip.
func ValidIdempotencyKey(k string) bool {
	if len(k) < 1 || len(k) > 64 {
		return false
	}

	for _, c := range k {
		if (c >= 'a' && c <= 'z') ||
			(c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') ||
			c == '_' || c == '-' {
			continue
		}

		return false
	}

	return true
}

// operationColumns is the SELECT list that scanOperation reads, in the same
// order. The coalesce calls turn NULL text into "" so it scans into plain
// strings.
const operationColumns = `operation_id::text, tenant_id::text, resource_type, resource_id::text,
	node_id::text, action, state, coalesce(phase, ''), actor_type, actor_id, actor_name,
	coalesce(error_code, ''), coalesce(error_summary, ''), requested_at, started_at, finished_at,
	resolved_at, coalesce(resolved_outcome, ''), coalesce(resolved_by_type, ''),
	coalesce(resolved_by_id, ''), coalesce(resolved_by_name, ''), coalesce(resolution_note, '')`

// scanOperation reads one row selected with operationColumns. If you
// change one, change the other to match.
func scanOperation(row pgx.Row) (Operation, error) {
	var op Operation
	var resolvedAt *time.Time
	var outcome, byType, byID, byName, note string
	err := row.Scan(&op.ID, &op.TenantID, &op.ResourceType, &op.ResourceID, &op.NodeID,
		&op.Action, &op.State, &op.Phase, &op.Actor.Type, &op.Actor.ID, &op.Actor.Name,
		&op.ErrorCode, &op.ErrorSummary, &op.RequestedAt, &op.StartedAt, &op.FinishedAt,
		&resolvedAt, &outcome, &byType, &byID, &byName, &note)
	if err != nil {
		return Operation{}, err
	}
	if resolvedAt != nil {
		op.Resolution = &Resolution{At: *resolvedAt, Outcome: OperationState(outcome),
			By: Actor{Type: byType, ID: byID, Name: byName}, Note: note}
	}
	return op, nil
}

// CreateOperation records a new pending operation for a server (I3, I4,
// C1). Everything happens in the caller's transaction, in this order:
//
//  1. Check the input. A bad server ID, action, key or empty fingerprint
//     is ErrInvalidInput.
//  2. Lock the server row with FOR UPDATE. Every create for this server
//     now waits its turn, so two requests can't both pass the checks
//     below. A missing server, or one in another tenant, is ErrNotFound.
//  3. With a key, look up and lock the key row:
//     - expired, and its operation is finished or resolved: the key may
//     be reused, and step 6 points it at the new operation;
//     - otherwise the same fingerprint means a retry, so we return the
//     original operation with replayed=true, whatever its state. A
//     different fingerprint is ErrIdempotencyKeyReused.
//     Then, if the server's node is revoked, ErrNodeRevoked; if it isn't
//     reachable (NodeReachable at t.Now()), ErrNodeUnreachable. Nothing
//     is written (V1, D5).
//  4. If the server already has a pending, running or unresolved
//     uncertain operation, return an *InProgressError naming it.
//  5. Insert the operation as pending and set the server's desired state.
//     The unique index operations_one_unfinished backs up step 4: if it
//     still fires (23505), that's ErrOperationInProgress too.
//  6. With a key, insert the key row, or update it when step 3 allowed
//     reuse. If the insert hits the primary key, a request with the same
//     key for another server got there first: ErrIdempotencyKeyReused.
//     Returning an error rolls back the operation from step 5 as well.
//
// A new operation comes back with replayed=false.
func (t *Tx) CreateOperation(ctx context.Context, in CreateOperationInput) (op Operation, replayed bool, err error) {
	// The server ID comes from the URL, so it gets the full UUID check.
	if err := ids.Check(in.ServerID); err != nil {
		return Operation{}, false, fmt.Errorf("%w: invalid server ID: %v", ErrInvalidInput, err)
	}

	switch in.Action {
	case ActionStart, ActionStop, ActionRestart:
		// One of ours.
	default:
		return Operation{}, false, fmt.Errorf("%w: invalid action", ErrInvalidInput)
	}

	// No key is fine. A key without a fingerprint is not, because we
	// could never tell a retry from a different request.
	if in.IdempotencyKey != "" {
		if !ValidIdempotencyKey(in.IdempotencyKey) {
			return Operation{}, false, fmt.Errorf("%w: invalid idempotency key", ErrInvalidInput)
		}

		if in.RequestSHA256 == ([32]byte{}) {
			return Operation{}, false, fmt.Errorf("%w: missing request fingerprint", ErrInvalidInput)
		}
	}

	// FOR UPDATE holds this row until we commit or roll back, so creates
	// for the same server run one at a time. RLS hides servers in other
	// tenants, so they look missing, the same as servers that don't exist.
	var nodeID string

	err = t.tx.QueryRow(ctx, `
    SELECT node_id::text
    FROM waystone.servers
    WHERE server_id = $1
    FOR UPDATE
`, in.ServerID).Scan(&nodeID)

	if errors.Is(err, pgx.ErrNoRows) {
		return Operation{}, false, ErrNotFound
	}
	if err != nil {
		return Operation{}, false, fmt.Errorf("lock server: %w", err)
	}
	reuseExpiredKey := false
	// Step 3. The key row is locked as well, so two requests with the same
	// expired key can't both decide to reuse it.
	if in.IdempotencyKey != "" {
		var existingOperationID string
		var existingFingerprint []byte
		var createdAt time.Time

		err := t.tx.QueryRow(ctx, `
        SELECT operation_id::text, request_sha256, created_at
        FROM waystone.idempotency_keys
        WHERE tenant_id = $1
          AND actor_type = $2
          AND actor_id = $3
          AND key = $4
        FOR UPDATE
    `,
			t.TenantID(),
			t.Actor().Type,
			t.Actor().ID,
			in.IdempotencyKey,
		).Scan(
			&existingOperationID,
			&existingFingerprint,
			&createdAt,
		)

		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return Operation{}, false, fmt.Errorf(
				"look up idempotency key: %w", err,
			)
		}

		if err == nil {
			existingOp, err := t.GetOperation(ctx, existingOperationID)
			if err != nil {
				return Operation{}, false, fmt.Errorf(
					"get idempotent operation: %w", err,
				)
			}

			// A key whose operation is still pending, running or unresolved
			// never expires. Otherwise the same request could run twice.
			finished := existingOp.State == StateSucceeded ||
				existingOp.State == StateFailed ||
				(existingOp.State == StateUncertain &&
					existingOp.Resolution != nil)

			expired := !t.Now().Before(createdAt.Add(KeyLifetime))

			if expired && finished {
				// The key gets pointed at the new operation in step 6.
				reuseExpiredKey = true
			} else {
				// A live key must belong to exactly the same request.
				if !bytes.Equal(existingFingerprint, in.RequestSHA256[:]) {
					return Operation{}, false, ErrIdempotencyKeyReused
				}

				// It's a retry, so return the original instead of a new one.
				return existingOp, true, nil
			}
		}
	}

	// A new request for a node that isn't reachable is refused rather than
	// queued (V1). This comes after the replay check, so a retry of a
	// request accepted earlier still gets its operation, and before the
	// blocking check, matching the order of the B8 reason codes.
	// A revoked node is refused first, whatever its last report (D5).
	var lastSeen *time.Time
	var revoked bool
	err = t.tx.QueryRow(ctx, `
    SELECT last_seen_at, revoked_at IS NOT NULL
    FROM waystone.nodes
    WHERE tenant_id = $1 AND node_id = $2
`, t.TenantID(), nodeID).Scan(&lastSeen, &revoked)
	if err != nil {
		// The foreign key on servers means the node row exists.
		return Operation{}, false, fmt.Errorf("check node: %w", err)
	}
	if revoked {
		return Operation{}, false, ErrNodeRevoked
	}
	if !NodeReachable(lastSeen, t.Now()) {
		return Operation{}, false, ErrNodeUnreachable
	}

	// Step 4. An unresolved uncertain operation blocks too: nobody knows
	// yet whether it actually ran, so starting something new could clash
	// with it.
	var existingID string
	var existingState OperationState

	err = t.tx.QueryRow(ctx, `
    SELECT operation_id::text, state
    FROM waystone.operations
    WHERE tenant_id = $1
      AND resource_type = 'server'
      AND resource_id = $2
      AND (
          state IN ('pending', 'running')
          OR (state = 'uncertain' AND resolved_at IS NULL)
      )
`, t.TenantID(), in.ServerID).Scan(&existingID, &existingState)

	if err == nil {
		return Operation{}, false, &InProgressError{
			OperationID: existingID,
			State:       existingState,
		}
	}

	if !errors.Is(err, pgx.ErrNoRows) {
		return Operation{}, false, fmt.Errorf(
			"check unfinished operation: %w", err,
		)
	}

	operationID, err := ids.NewV7()
	if err != nil {
		return Operation{}, false, fmt.Errorf(
			"generate operation ID: %w", err,
		)
	}

	// Step 5. node_id is copied from the server row we locked, so history
	// keeps the node the request was meant for.
	row := t.tx.QueryRow(ctx, `
    INSERT INTO waystone.operations (
        tenant_id,
        operation_id,
        resource_type,
        resource_id,
        node_id,
        action,
        state,
        actor_type,
        actor_id,
        actor_name,
        requested_at
    )
    VALUES (
        $1, $2, 'server', $3, $4, $5, 'pending',
        $6, $7, $8, $9
    )
    RETURNING `+operationColumns,
		t.TenantID(),
		operationID,
		in.ServerID,
		nodeID,
		in.Action,
		t.Actor().Type,
		t.Actor().ID,
		t.Actor().Name,
		t.Now(),
	)

	op, err = scanOperation(row)
	if err != nil {
		// The server lock makes this unreachable through CreateOperation
		// today. It's here for any future writer that skips the lock.
		var pgErr *pgconn.PgError

		if errors.As(err, &pgErr) &&
			pgErr.Code == "23505" &&
			pgErr.ConstraintName == "operations_one_unfinished" {
			return Operation{}, false, ErrOperationInProgress
		}

		return Operation{}, false, fmt.Errorf("insert operation: %w", err)
	}

	// Restart ends with the server running, the same as start.
	desiredState := "running"

	if in.Action == ActionStop {
		desiredState = "stopped"
	}

	_, err = t.tx.Exec(ctx, `
	UPDATE waystone.servers
	SET desired_state = $1
	WHERE server_id = $2
`, desiredState, in.ServerID)

	if err != nil {
		return Operation{}, false, fmt.Errorf(
			"update server desired state: %w", err,
		)
	}

	// Step 6: store the key next to the new operation.
	if in.IdempotencyKey != "" {
		if reuseExpiredKey {
			// Point the expired key at the new operation and start its
			// lifetime over.
			_, err = t.tx.Exec(ctx, `
			UPDATE waystone.idempotency_keys
			SET request_sha256 = $1,
			    operation_id = $2,
			    created_at = $3
			WHERE tenant_id = $4
			  AND actor_type = $5
			  AND actor_id = $6
			  AND key = $7
		`,
				in.RequestSHA256[:],
				op.ID,
				t.Now(),
				t.TenantID(),
				t.Actor().Type,
				t.Actor().ID,
				in.IdempotencyKey,
			)
		} else {
			_, err = t.tx.Exec(ctx, `
			INSERT INTO waystone.idempotency_keys (
				tenant_id,
				actor_type,
				actor_id,
				key,
				request_sha256,
				operation_id,
				created_at
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
		`,
				t.TenantID(),
				t.Actor().Type,
				t.Actor().ID,
				in.IdempotencyKey,
				in.RequestSHA256[:],
				op.ID,
				t.Now(),
			)
		}

		if err != nil {
			// Two requests with the same new key for different servers
			// don't block each other at the server lock. The second
			// insert waits for the first transaction and then fails here.
			var pgErr *pgconn.PgError

			if errors.As(err, &pgErr) &&
				pgErr.Code == "23505" &&
				pgErr.ConstraintName == "idempotency_keys_pkey" {
				return Operation{}, false, ErrIdempotencyKeyReused
			}

			return Operation{}, false, fmt.Errorf("save idempotency key: %w", err)
		}
	}

	return op, false, nil
}

// GetOperation returns one operation from the actor's tenant. A malformed
// ID is ErrInvalidInput. A missing operation is ErrNotFound, and so is one
// in another tenant, so nobody can probe for other tenants' IDs.
func (t *Tx) GetOperation(ctx context.Context, id string) (Operation, error) {
	if err := ids.Check(id); err != nil {
		return Operation{}, fmt.Errorf(
			"%w: invalid operation ID: %v",
			ErrInvalidInput, err,
		)
	}

	// No tenant filter here: RLS already limits this to our tenant.
	row := t.tx.QueryRow(ctx, `
		SELECT `+operationColumns+`
		FROM waystone.operations
		WHERE operation_id = $1
	`, id)

	op, err := scanOperation(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Operation{}, ErrNotFound
	}
	if err != nil {
		return Operation{}, fmt.Errorf("get operation: %w", err)
	}

	return op, nil
}

// ResolveOperation records what a person decided about an uncertain
// operation (decision O). The state stays uncertain, so history still shows
// that the system itself never found out. The decision, who made it, when,
// and an optional note go into the resolved_* columns.
//
// A bad ID, an outcome other than succeeded or failed, or a note that
// validNote refuses is ErrInvalidInput. A missing operation is
// ErrNotFound, one that isn't uncertain is ErrNotUncertain, and one that
// is already resolved is ErrAlreadyResolved.
//
// It returns the operation as it is after the update.
func (t *Tx) ResolveOperation(ctx context.Context, id string, outcome OperationState, note string) (Operation, error) {
	if err := ids.Check(id); err != nil {
		return Operation{}, fmt.Errorf(
			"invalid operation ID %q: %w",
			id,
			ErrInvalidInput,
		)
	}
	if outcome != StateSucceeded && outcome != StateFailed {
		return Operation{}, fmt.Errorf("%w: invalid resolution outcome", ErrInvalidInput)
	}
	if !validNote(note) {
		return Operation{}, fmt.Errorf("%w: invalid resolution note", ErrInvalidInput)
	}
	// Lock the row first, so two people resolving at the same moment can't
	// both pass the checks below. The trigger would refuse the second
	// resolve anyway, but this way it gets a clear error.
	var lockedID string

	err := t.tx.QueryRow(ctx, `
	SELECT operation_id::text
	FROM waystone.operations
	WHERE operation_id = $1
	FOR UPDATE
`, id).Scan(&lockedID)

	if errors.Is(err, pgx.ErrNoRows) {
		return Operation{}, ErrNotFound
	}
	if err != nil {
		return Operation{}, fmt.Errorf("lock operation: %w", err)
	}

	op, err := t.GetOperation(ctx, id)
	if err != nil {
		return Operation{}, err
	}

	if op.State != OperationState("uncertain") {
		return Operation{}, ErrNotUncertain
	}
	if op.Resolution != nil {
		return Operation{}, ErrAlreadyResolved
	}

	// state isn't in the SET list: resolving never changes it.
	_, err = t.tx.Exec(ctx, `
	UPDATE waystone.operations
	SET resolved_at = $1,
	    resolved_outcome = $2,
	    resolved_by_type = $3,
	    resolved_by_id = $4,
	    resolved_by_name = $5,
	    resolution_note = $6
	WHERE operation_id = $7
`,
		t.Now(),
		outcome,
		t.Actor().Type,
		t.Actor().ID,
		t.Actor().Name,
		note,
		id,
	)
	if err != nil {
		return Operation{}, fmt.Errorf("resolve operation: %w", err)
	}

	return t.GetOperation(ctx, id)
}

// validNote reports whether note may be a resolution note: at most 500
// bytes of valid UTF-8, with no control character but newline and no
// bidirectional formatting character (U+061C, U+200E-U+200F,
// U+202A-U+202E, U+2066-U+2069). Every admin reads these notes in the
// operation history, so one mustn't be able to reorder or hide what it
// shows (5b audit H2), the same idea as the rule for actor names.
func validNote(note string) bool {
	if len(note) > 500 || !utf8.ValidString(note) {
		return false
	}
	for _, r := range note {
		if (unicode.IsControl(r) && r != '\n') || unicode.Is(unicode.Bidi_Control, r) {
			return false
		}
	}
	return true
}
