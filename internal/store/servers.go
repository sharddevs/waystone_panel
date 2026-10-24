// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/sharddevs/waystone_panel/internal/ids"
)

// Creating a server (6c-1, D7): a database change, with its install
// operation recorded in the same transaction so the side effect is
// recorded first.

// The limits a new server's settings must keep. The database has the same
// CHECKs (00009).
const (
	MinGamePort        = 1024
	MaxGamePort        = 65535
	MinHeapMiB         = 512
	MaxHeapMiB         = 32768
	MaxServerNameBytes = 100
)

// NewServer is what CreateServer needs. Version and Motd are the
// profile's to check (minecraft.Properties); the store only checks their
// shape, as the database does.
type NewServer struct {
	NodeID  string
	Name    string
	Version string
	Port    uint32
	HeapMiB uint32
	Motd    string
	// ReservedPort, if not 0, is refused on the node without a pin: the
	// controller's own machine, where it is the agent listener's port.
	ReservedPort uint32
	// RequestID labels the server.create audit row.
	RequestID string
	// IdempotencyKey and RequestSHA256 work as for CreateOperation.
	IdempotencyKey string
	RequestSHA256  [32]byte
}

var (
	versionRE = regexp.MustCompile(`^[0-9A-Za-z.+-]{1,32}$`)
	motdRE    = regexp.MustCompile(`^[\x20-\x5b\x5d-\x7e]{0,64}$`)
)

// ValidServerName reports whether name can be a server's name: 1 to
// MaxServerNameBytes bytes of UTF-8 without control characters.
func ValidServerName(name string) bool {
	if len(name) < 1 || len(name) > MaxServerNameBytes || !utf8.ValidString(name) {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func (in NewServer) valid() bool {
	return ids.Check(in.NodeID) == nil && ValidServerName(in.Name) &&
		versionRE.MatchString(in.Version) && motdRE.MatchString(in.Motd) &&
		in.Port >= MinGamePort && in.Port <= MaxGamePort &&
		in.HeapMiB >= MinHeapMiB && in.HeapMiB <= MaxHeapMiB &&
		len(in.RequestID) > 0 &&
		(in.IdempotencyKey == "" || (ValidIdempotencyKey(in.IdempotencyKey) && in.RequestSHA256 != [32]byte{}))
}

// CreateServer creates a server on one of the tenant's nodes, with its
// first server.install operation and a server.create audit row, all in
// the caller's transaction:
//
//  1. Check the input (ErrInvalidInput). Only an admin may (ErrNotAdmin;
//     the database's guard checks again).
//  2. With a key, a retry returns the server and the install operation
//     it made, with replayed=true (as CreateOperation step 3).
//  3. Lock the node: a node the tenant doesn't have is ErrNotFound, a
//     revoked one ErrNodeRevoked, and one not seen within NodeStaleAfter
//     ErrNodeUnreachable, so an install never waits for a node that's
//     gone (V1).
//     A node that has MaxServersPerNode servers already is
//     ErrTooManyServers.
//  4. Insert the server. Its port on that node, or the reserved port on
//     the controller's own node, is ErrPortTaken.
//  5. Insert the install operation, the key, and the audit row.
func (t *Tx) CreateServer(ctx context.Context, in NewServer) (s Server, op Operation, replayed bool, err error) {
	if !in.valid() {
		return Server{}, Operation{}, false, ErrInvalidInput
	}
	if err := t.requireAdmin(); err != nil {
		return Server{}, Operation{}, false, err
	}

	existing, reuse, err := t.lookupKey(ctx, in.IdempotencyKey, in.RequestSHA256)
	if err != nil {
		return Server{}, Operation{}, false, err
	}
	if existing != nil {
		if existing.Action != ActionInstall {
			return Server{}, Operation{}, false, ErrIdempotencyKeyReused
		}
		s, err := t.GetServer(ctx, existing.ResourceID)
		if err != nil {
			return Server{}, Operation{}, false, fmt.Errorf("get idempotent server: %w", err)
		}
		return s, *existing, true, nil
	}

	// FOR SHARE: the node can't be revoked under us; two creates on it
	// still run side by side, and the unique index settles the port.
	var lastSeen *time.Time
	var revoked, pinned bool
	err = t.tx.QueryRow(ctx, `
		SELECT last_seen_at, revoked_at IS NOT NULL, agent_spki_sha256 IS NOT NULL
		FROM waystone.nodes
		WHERE node_id = $1
		FOR SHARE`, in.NodeID).Scan(&lastSeen, &revoked, &pinned)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Server{}, Operation{}, false, ErrNotFound
	case err != nil:
		return Server{}, Operation{}, false, fmt.Errorf("lock node: %w", err)
	case revoked:
		return Server{}, Operation{}, false, ErrNodeRevoked
	case !NodeReachable(lastSeen, t.Now()):
		return Server{}, Operation{}, false, ErrNodeUnreachable
	case !pinned && in.ReservedPort != 0 && in.Port == in.ReservedPort:
		return Server{}, Operation{}, false, ErrPortTaken
	}

	// A node's hello lists all its servers, and refuses more than
	// MaxServersPerNode, so one more would lock the node out. Creates on
	// one node take turns here, so two can't both see room for one.
	// The count is its own statement: under READ COMMITTED it then sees
	// every create that committed while this one waited for the lock.
	if _, err = t.tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('waystone.servers/' || $1, 0))`, in.NodeID); err != nil {
		return Server{}, Operation{}, false, fmt.Errorf("lock node servers: %w", err)
	}
	var onNode int
	if err = t.tx.QueryRow(ctx, `SELECT count(*) FROM waystone.servers WHERE node_id = $1`, in.NodeID).Scan(&onNode); err != nil {
		return Server{}, Operation{}, false, fmt.Errorf("count node servers: %w", err)
	}
	if onNode >= MaxServersPerNode {
		return Server{}, Operation{}, false, ErrTooManyServers
	}

	serverID, err := ids.NewV7()
	if err != nil {
		return Server{}, Operation{}, false, fmt.Errorf("generate server ID: %w", err)
	}
	_, err = t.tx.Exec(ctx, `
		INSERT INTO waystone.servers (tenant_id, server_id, node_id, name, profile, runtime, config)
		VALUES ($1, $2, $3, $4, 'minecraft-java', 'systemd',
		        jsonb_build_object('version', $5::text, 'port', $6::int, 'heap_mib', $7::int, 'motd', $8::text))`,
		t.TenantID(), serverID, in.NodeID, in.Name, in.Version, in.Port, in.HeapMiB, in.Motd)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "servers_node_port" {
			return Server{}, Operation{}, false, ErrPortTaken
		}
		return Server{}, Operation{}, false, fmt.Errorf("insert server: %w", err)
	}

	if op, err = t.insertOperation(ctx, serverID, in.NodeID, ActionInstall, ""); err != nil {
		return Server{}, Operation{}, false, err
	}
	if err := t.storeKey(ctx, in.IdempotencyKey, in.RequestSHA256, op.ID, reuse); err != nil {
		return Server{}, Operation{}, false, err
	}
	if err := t.insertAudit(ctx, Denial{
		Action:       "server.create",
		ResourceType: "server",
		ResourceID:   serverID,
		Reason:       "created",
		RequestID:    in.RequestID,
	}, "recorded"); err != nil {
		return Server{}, Operation{}, false, err
	}
	if s, err = t.GetServer(ctx, serverID); err != nil {
		return Server{}, Operation{}, false, fmt.Errorf("read new server: %w", err)
	}
	return s, op, false, nil
}
