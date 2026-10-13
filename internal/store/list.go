// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"time"

	"github.com/sharddevs/waystone_panel/internal/ids"
)

// Server is one row of the servers table as the API shows it. The three
// state dimensions each carry two timestamps: since (when the value last
// changed) and observed_at (when the agent last confirmed it). Both are
// nil until an agent reports (4b).
type Server struct {
	ID           string
	TenantID     string
	NodeID       string
	Name         string
	Profile      string
	DesiredState string // "running" or "stopped"

	ProcessState      string // stopped, starting, running, stopping, failed, unknown
	ProcessSince      *time.Time
	ProcessObservedAt *time.Time

	Readiness           string // ready, not_ready, unknown, not_supported
	ReadinessSince      *time.Time
	ReadinessObservedAt *time.Time
}

// MaxListLimit is the most rows ListOperations returns in one call.
const MaxListLimit = 100

// serverColumns is the SELECT list scanServer reads, in the same order.
const serverColumns = `server_id::text, tenant_id::text, node_id::text, name, profile, desired_state,
	process_state, process_since, process_observed_at,
	readiness, readiness_since, readiness_observed_at`

func scanServer(row interface{ Scan(...any) error }) (Server, error) {
	var s Server
	err := row.Scan(&s.ID, &s.TenantID, &s.NodeID, &s.Name, &s.Profile, &s.DesiredState,
		&s.ProcessState, &s.ProcessSince, &s.ProcessObservedAt,
		&s.Readiness, &s.ReadinessSince, &s.ReadinessObservedAt)
	return s, err
}

// ListServers returns every server in the transaction's tenant, ordered
// by name and then by ID, so the order is stable even when two servers
// share a name. RLS already limits the rows to the tenant; don't add a
// tenant filter of your own. An empty tenant gives an empty slice, not an
// error.
//
// TODO: SELECT serverColumns FROM waystone.servers ORDER BY name,
// server_id; scan each row with scanServer; check rows.Err().
func (t *Tx) ListServers(ctx context.Context) ([]Server, error) {
	rows, err := t.tx.Query(
		ctx,
		"SELECT "+serverColumns+" FROM waystone.servers ORDER BY name, server_id",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	servers := []Server{}

	for rows.Next() {
		server, err := scanServer(rows)
		if err != nil {
			return nil, err
		}

		servers = append(servers, server)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return servers, nil
}

// ListOperations returns up to limit operations from the transaction's
// tenant, newest first (requested_at, then operation_id, both
// descending). If serverID isn't "", only that server's operations come
// back. A server in another tenant simply has none, so it isn't an error.
//
// serverID must be "" or pass ids.Check, and limit must be 1 to
// MaxListLimit; otherwise ErrInvalidInput.
//
// TODO: validate, then one query with the server filter as an optional
// parameter, ORDER BY requested_at DESC, operation_id DESC LIMIT $n;
// scan with scanOperation. Return an empty slice, not nil, when there
// are no rows.
func (t *Tx) ListOperations(ctx context.Context, serverID string, limit int) ([]Operation, error) {
	if limit < 1 || limit > MaxListLimit {
		return nil, ErrInvalidInput
	}

	if serverID != "" {
		if err := ids.Check(serverID); err != nil {
			return nil, ErrInvalidInput
		}
	}

	query := "SELECT " + operationColumns + `
		FROM waystone.operations
		WHERE ($1::text = '' OR resource_id = $1::uuid)
		ORDER BY requested_at DESC, operation_id DESC
		LIMIT $2`

	rows, err := t.tx.Query(ctx, query, serverID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	operations := []Operation{}

	for rows.Next() {
		op, err := scanOperation(rows)
		if err != nil {
			return nil, err
		}

		operations = append(operations, op)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return operations, nil
}
