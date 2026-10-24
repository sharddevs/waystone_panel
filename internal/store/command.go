// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/sharddevs/waystone_panel/internal/console"
	"github.com/sharddevs/waystone_panel/internal/ids"
)

// CommandSpec is what a node's agent reads for a console command it is
// running (7b, decision N): the line the API stored with the operation.
type CommandSpec struct {
	ServerID string
	Command  string
}

// RunningCommand returns the line for operationID, which must be a
// server.command running on the transaction's node, for a server still on
// that node. Anything else is ErrNotFound, the same answer for every case,
// as RunningInstall. Only a node may ask (ErrNotNode).
func (t *Tx) RunningCommand(ctx context.Context, operationID string) (CommandSpec, error) {
	if err := t.requireNode(); err != nil {
		return CommandSpec{}, err
	}
	if ids.Check(operationID) != nil {
		return CommandSpec{}, ErrNotFound
	}
	var spec CommandSpec
	err := t.tx.QueryRow(ctx, `
		SELECT s.server_id::text, coalesce(o.config->>'command', '')
		FROM waystone.operations o
		JOIN waystone.servers s ON s.tenant_id = o.tenant_id AND s.server_id = o.resource_id
		WHERE o.operation_id = $1 AND o.node_id = $2 AND s.node_id = $2
		  AND o.resource_type = 'server' AND o.action = 'server.command' AND o.state = 'running'`,
		operationID, t.actor.ID).Scan(&spec.ServerID, &spec.Command)
	if errors.Is(err, pgx.ErrNoRows) {
		return CommandSpec{}, ErrNotFound
	}
	if err != nil {
		return CommandSpec{}, fmt.Errorf("read command: %w", err)
	}
	if !console.ValidLine(spec.Command) {
		return CommandSpec{}, ErrNotFound
	}
	return spec, nil
}
