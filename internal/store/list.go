// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sharddevs/waystone_panel/internal/ids"
)

// Server is one row of the servers table as the API shows it, with its
// node's name and last_seen_at joined in. The three state dimensions each
// carry two timestamps: since (when the value last changed) and
// observed_at (when the agent last confirmed it). Both are nil until an
// agent reports (4b).
type Server struct {
	ID       string
	TenantID string
	NodeID   string
	Name     string
	Profile  string
	// ProfileVersion is config->>'version', nil when it isn't set. Nothing
	// writes it before the stage 6 install (V3a).
	ProfileVersion *string
	Runtime        string
	DesiredState   string // "running" or "stopped"

	NodeName       string
	NodeLastSeenAt *time.Time // nil until the node's agent first says hello

	ProcessState      string // stopped, starting, running, stopping, failed, unknown
	ProcessSince      *time.Time
	ProcessObservedAt *time.Time

	Readiness           string // ready, not_ready, unknown, not_supported
	ReadinessSince      *time.Time
	ReadinessObservedAt *time.Time

	// Blocking is the state of the operation that would refuse a new one:
	// "pending" or "running", "uncertain" for an unresolved uncertain one,
	// or "" when there is none. The unique index operations_one_unfinished
	// allows at most one.
	Blocking OperationState
}

// NodeStaleAfter is how old a node's last_seen_at may be before the node
// counts as unreachable (R5). Agents report every 15 s, so 45 s is three
// missed reports. It is compared with the controller's clock, which also
// wrote last_seen_at.
const NodeStaleAfter = 45 * time.Second

// NodeReachable reports whether a node last seen at lastSeen is reachable
// at now: seen at all, and no more than NodeStaleAfter ago.
func NodeReachable(lastSeen *time.Time, now time.Time) bool {
	return lastSeen != nil && now.Sub(*lastSeen) <= NodeStaleAfter
}

// MaxListLimit is the most rows ListOperations returns in one call.
const MaxListLimit = 100

// serverColumns is the SELECT list scanServer reads, in the same order,
// and serverFrom is the FROM clause it needs. The foreign key on
// (tenant_id, node_id) means every server has its node, and RLS shows a
// member both.
const serverColumns = `s.server_id::text, s.tenant_id::text, s.node_id::text, s.name, s.profile,
	nullif(s.config->>'version', ''), s.runtime, s.desired_state,
	n.name, n.last_seen_at,
	s.process_state, s.process_since, s.process_observed_at,
	s.readiness, s.readiness_since, s.readiness_observed_at,
	coalesce((SELECT o.state FROM waystone.operations o
	          WHERE o.tenant_id = s.tenant_id AND o.resource_type = 'server'
	            AND o.resource_id = s.server_id
	            AND (o.state IN ('pending', 'running')
	                 OR (o.state = 'uncertain' AND o.resolved_at IS NULL))
	          LIMIT 1), '')`

const serverFrom = ` FROM waystone.servers s
	JOIN waystone.nodes n ON n.tenant_id = s.tenant_id AND n.node_id = s.node_id`

func scanServer(row interface{ Scan(...any) error }) (Server, error) {
	var s Server
	err := row.Scan(&s.ID, &s.TenantID, &s.NodeID, &s.Name, &s.Profile,
		&s.ProfileVersion, &s.Runtime, &s.DesiredState,
		&s.NodeName, &s.NodeLastSeenAt,
		&s.ProcessState, &s.ProcessSince, &s.ProcessObservedAt,
		&s.Readiness, &s.ReadinessSince, &s.ReadinessObservedAt,
		&s.Blocking)
	return s, err
}

// ListServers returns every server in the transaction's tenant, ordered
// by name and then by ID, so the order is stable even when two servers
// share a name. RLS already limits the rows to the tenant; don't add a
// tenant filter of your own. An empty tenant gives an empty slice, not an
// error.
func (t *Tx) ListServers(ctx context.Context) ([]Server, error) {
	rows, err := t.tx.Query(
		ctx,
		"SELECT "+serverColumns+serverFrom+" ORDER BY s.name, s.server_id",
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

// GetServer returns one server from the transaction's tenant, in the same
// shape as ListServers. A malformed ID is ErrInvalidInput. A missing server
// is ErrNotFound, and so is one in another tenant, because RLS hides it.
func (t *Tx) GetServer(ctx context.Context, id string) (Server, error) {
	if err := ids.Check(id); err != nil {
		return Server{}, ErrInvalidInput
	}
	server, err := scanServer(t.tx.QueryRow(ctx,
		"SELECT "+serverColumns+serverFrom+" WHERE s.server_id = $1", id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Server{}, ErrNotFound
	}
	if err != nil {
		return Server{}, err
	}
	return server, nil
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
