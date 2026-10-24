// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/sharddevs/waystone_panel/internal/ids"
)

// InstallSpec is what a node's agent reads for an install it is running
// (6c-2, 6M5): the server's settings as the API stored them.
type InstallSpec struct {
	ServerID string
	Version  string
	Port     uint32
	HeapMiB  uint32
	Motd     string
}

// RunningInstall returns the settings for operationID, which must be a
// server.install running on the transaction's node, for a server still on
// that node. Anything else, a finished install or another node's
// included, is ErrNotFound, so the caller can give the same answer for
// every case. Only a node may ask (ErrNotNode).
func (t *Tx) RunningInstall(ctx context.Context, operationID string) (InstallSpec, error) {
	if err := t.requireNode(); err != nil {
		return InstallSpec{}, err
	}
	if ids.Check(operationID) != nil {
		return InstallSpec{}, ErrNotFound
	}
	var spec InstallSpec
	var port, heap *string
	err := t.tx.QueryRow(ctx, `
		SELECT s.server_id::text, coalesce(s.config->>'version', ''), s.config->>'port',
		       s.config->>'heap_mib', coalesce(s.config->>'motd', '')
		FROM waystone.operations o
		JOIN waystone.servers s ON s.tenant_id = o.tenant_id AND s.server_id = o.resource_id
		WHERE o.operation_id = $1 AND o.node_id = $2 AND s.node_id = $2
		  AND o.resource_type = 'server' AND o.action = 'server.install' AND o.state = 'running'`,
		operationID, t.actor.ID).Scan(&spec.ServerID, &spec.Version, &port, &heap, &spec.Motd)
	if errors.Is(err, pgx.ErrNoRows) {
		return InstallSpec{}, ErrNotFound
	}
	if err != nil {
		return InstallSpec{}, fmt.Errorf("read install: %w", err)
	}
	var ok bool
	if spec.Port, ok = GamePort(port); !ok {
		return InstallSpec{}, ErrNotFound
	}
	if spec.HeapMiB, ok = GamePort(heap); !ok {
		return InstallSpec{}, ErrNotFound
	}
	return spec, nil
}
