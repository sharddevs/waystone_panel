// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/sharddevs/waystone_panel/internal/ids"
)

// What stage 4c adds (notes/stage4c-scope.md v2.1): operations leave the
// database for the agent and their results come back. Claim, result and
// progress run inside an InTenantTx whose actor is the node, and each
// one first checks that the caller's session is still the node's current
// one. Migration 00004's trigger checks the same things again, so a bug
// here can't move an operation the wrong way.

// ErrNotClaimed: a result or progress names an operation that isn't
// running under this node's session. The session's own bookkeeping
// should have caught that first, so it means a bug or a takeover that
// happened in between.
var ErrNotClaimed = errors.New("operation is not running under this session")

// The fixed error codes the claim writes itself (F10). The codes for
// results come from the controller's table in agentlink.
const (
	CodeServerNotFound   = "not_found"
	CodeServerReassigned = "reassigned"
)

// ClaimedOperation is one operation that is now running under a session
// and has to be sent to its agent.
type ClaimedOperation struct {
	OperationID string
	ServerID    string
	Action      Action
	Epoch       int64
}

// lockSession locks the node row FOR SHARE and checks that epoch is the
// node's current session. A newer hello has to wait for this transaction,
// and one that already committed makes it ErrSessionSuperseded (C3).
func (t *Tx) lockSession(ctx context.Context, epoch int64) error {
	if err := t.requireNode(); err != nil {
		return err
	}
	var current int64
	err := t.tx.QueryRow(ctx, `
		SELECT session_epoch FROM waystone.nodes
		WHERE node_id = $1
		FOR SHARE`, t.actor.ID).Scan(&current)
	if err != nil {
		return fmt.Errorf("lock node: %w", err)
	}
	if current != epoch {
		return ErrSessionSuperseded
	}
	return nil
}

// ClaimNext takes the node's oldest pending operation and marks it
// running under session epoch (I9). ok is false when there is nothing to
// send. In order:
//
//  1. Lock the node and check the epoch (lockSession).
//  2. Take the oldest pending operation FOR UPDATE SKIP LOCKED.
//  3. Look at the live server row. If it's gone, or now on another node,
//     the operation fails with not_found or reassigned and is never sent
//     (F10). Then go back to step 2.
//  4. Mark it running with claimed_epoch, started_at, and phase
//     "stopping" for a restart.
//
// I3 already allows only one unfinished operation per server, and this
// moves that one row, so a claim can never make two operations run on
// one server (S1). The caller sends the operation only after the
// transaction commits, and never sends it twice.
func (t *Tx) ClaimNext(ctx context.Context, epoch int64) (op ClaimedOperation, ok bool, err error) {
	if err := t.lockSession(ctx, epoch); err != nil {
		return ClaimedOperation{}, false, err
	}
	nodeID := t.actor.ID

	for {
		var opID, serverID string
		var action Action
		err := t.tx.QueryRow(ctx, `
			SELECT operation_id::text, resource_id::text, action
			FROM waystone.operations
			WHERE node_id = $1 AND state = 'pending'
			ORDER BY requested_at, operation_id
			LIMIT 1
			FOR UPDATE SKIP LOCKED`, nodeID).Scan(&opID, &serverID, &action)
		if errors.Is(err, pgx.ErrNoRows) {
			return ClaimedOperation{}, false, nil
		}
		if err != nil {
			return ClaimedOperation{}, false, fmt.Errorf("find pending operation: %w", err)
		}

		// RLS hides servers outside the node's tenant, so a server that
		// moved to another tenant looks deleted, which is what it is to us.
		var liveNode string
		err = t.tx.QueryRow(ctx, `
			SELECT node_id::text FROM waystone.servers
			WHERE server_id = $1`, serverID).Scan(&liveNode)
		refusal := ""
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			refusal = CodeServerNotFound
		case err != nil:
			return ClaimedOperation{}, false, fmt.Errorf("check server: %w", err)
		case liveNode != nodeID:
			refusal = CodeServerReassigned
		}
		if refusal != "" {
			summary := "The server no longer exists, so the operation was not sent."
			if refusal == CodeServerReassigned {
				summary = "The server moved to another node, so the operation was not sent."
			}
			_, err := t.tx.Exec(ctx, `
				UPDATE waystone.operations
				SET state = 'failed', finished_at = $2, error_code = $3, error_summary = $4
				WHERE operation_id = $1 AND state = 'pending'`,
				opID, t.Now(), refusal, summary)
			if err != nil {
				return ClaimedOperation{}, false, fmt.Errorf("refuse operation: %w", err)
			}
			continue
		}

		var phase *string
		if action == ActionRestart {
			stopping := "stopping"
			phase = &stopping
		}
		tag, err := t.tx.Exec(ctx, `
			UPDATE waystone.operations
			SET state = 'running', claimed_epoch = $2, started_at = $3, phase = $4
			WHERE operation_id = $1 AND state = 'pending'`,
			opID, epoch, t.Now(), phase)
		if err != nil {
			return ClaimedOperation{}, false, fmt.Errorf("claim operation: %w", err)
		}
		if tag.RowsAffected() != 1 {
			// The row is locked, so this can't happen; say so if it does.
			return ClaimedOperation{}, false, fmt.Errorf("claim operation %s: no row changed", opID)
		}
		return ClaimedOperation{OperationID: opID, ServerID: serverID, Action: action, Epoch: epoch}, true, nil
	}
}

// OperationResult is how an operation ended, as the controller records
// it. Code and Summary are fixed strings chosen by the controller, never
// text from the agent (decision L).
type OperationResult struct {
	State   OperationState // succeeded, failed or uncertain
	Code    string         // empty for succeeded, required otherwise
	Summary string         // empty for succeeded, required otherwise
}

func validResult(r OperationResult) bool {
	switch r.State {
	case StateSucceeded:
		return r.Code == "" && r.Summary == ""
	case StateFailed, StateUncertain:
		return r.Code != "" && len(r.Code) <= 64 && r.Summary != "" && len(r.Summary) <= 200
	}
	return false
}

// CompleteOperation records the result of an operation that session
// epoch claimed (C3, I10). The UPDATE only matches a row of this node
// that is still running under exactly this epoch, so a replayed result,
// another node's operation, or a superseded session's result changes
// nothing. A superseded session is ErrSessionSuperseded; any other row
// that doesn't match is ErrNotClaimed.
func (t *Tx) CompleteOperation(ctx context.Context, epoch int64, operationID string, r OperationResult) error {
	if err := ids.Check(operationID); err != nil {
		return fmt.Errorf("%w: invalid operation ID: %v", ErrInvalidInput, err)
	}
	if !validResult(r) {
		return fmt.Errorf("%w: invalid result", ErrInvalidInput)
	}
	if err := t.lockSession(ctx, epoch); err != nil {
		return err
	}
	var code, summary *string
	if r.State != StateSucceeded {
		code, summary = &r.Code, &r.Summary
	}
	tag, err := t.tx.Exec(ctx, `
		UPDATE waystone.operations
		SET state = $4, finished_at = $5, error_code = $6, error_summary = $7
		WHERE operation_id = $1 AND node_id = $2
		  AND state = 'running' AND claimed_epoch = $3`,
		operationID, t.actor.ID, epoch, string(r.State), t.Now(), code, summary)
	if err != nil {
		return fmt.Errorf("store result: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrNotClaimed
	}
	return nil
}

// RestartStarting records that a restart this session claimed has
// stopped the server and is now starting it again: phase stopping
// becomes starting. Anything else is ErrNotClaimed, or
// ErrSessionSuperseded after a takeover.
func (t *Tx) RestartStarting(ctx context.Context, epoch int64, operationID string) error {
	if err := ids.Check(operationID); err != nil {
		return fmt.Errorf("%w: invalid operation ID: %v", ErrInvalidInput, err)
	}
	if err := t.lockSession(ctx, epoch); err != nil {
		return err
	}
	tag, err := t.tx.Exec(ctx, `
		UPDATE waystone.operations
		SET phase = 'starting'
		WHERE operation_id = $1 AND node_id = $2
		  AND state = 'running' AND claimed_epoch = $3
		  AND action = 'server.restart' AND phase = 'stopping'`,
		operationID, t.actor.ID, epoch)
	if err != nil {
		return fmt.Errorf("store progress: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrNotClaimed
	}
	return nil
}

// RecoverRunningOperations is the controller's startup recovery (I7,
// F2): every running operation, in every tenant, becomes uncertain with
// controller_restarted, and the count comes back. It goes through the
// database function, which runs as the owner and can do nothing else.
// Call it once, before anything is listening.
func (s *Store) RecoverRunningOperations(ctx context.Context) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT waystone.recover_running_operations()`).Scan(&n); err != nil {
		return 0, fmt.Errorf("recover running operations: %w", err)
	}
	return n, nil
}
