// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/sharddevs/waystone_panel/internal/ids"
)

// What the agent side of the store adds (stage 4b, notes/stage4b-scope.md
// v2.1). Both functions run inside an InTenantTx whose actor is the node.
// The controller builds that actor from its own config after checking the
// agent's uid (R3); nothing an agent sends ever picks the node.

var (
	// ErrNotNode: a node function was called by an actor that isn't a
	// node with role node. That's a bug in the controller, not the agent.
	ErrNotNode = errors.New("actor is not a node")

	// ErrSessionSuperseded: the report's epoch isn't the node's current
	// one, because a newer hello has taken over (C3).
	ErrSessionSuperseded = errors.New("node session superseded")

	// ErrTooManyServers: the node has more servers than one hello reply
	// or report may carry (R7).
	ErrTooManyServers = errors.New("node has too many servers")
)

// MaxServersPerNode is the v0.1 limit (R7). 256 report entries are about
// 20 KiB, well inside the 64 KiB message limit.
const MaxServersPerNode = 256

// MaxNativeStateBytes bounds the free-text native state an agent sends
// along with each entry. It only goes to the log.
const MaxNativeStateBytes = 32

// ProcessState is what systemd says a server's process is doing. The
// values match the CHECK on servers.process_state.
type ProcessState string

const (
	ProcessStopped  ProcessState = "stopped"
	ProcessStarting ProcessState = "starting"
	ProcessRunning  ProcessState = "running"
	ProcessStopping ProcessState = "stopping"
	ProcessFailed   ProcessState = "failed"
	ProcessUnknown  ProcessState = "unknown"
)

// ValidProcessState reports whether s is one of the values above.
func ValidProcessState(s ProcessState) bool {
	switch s {
	case ProcessStopped, ProcessStarting, ProcessRunning, ProcessStopping, ProcessFailed, ProcessUnknown:
		return true
	}
	return false
}

// Readiness is whether the game answers its players (D6). The values match
// the CHECK on servers.readiness.
type Readiness string

const (
	ReadinessReady        Readiness = "ready"
	ReadinessNotReady     Readiness = "not_ready"
	ReadinessUnknown      Readiness = "unknown"
	ReadinessNotSupported Readiness = "not_supported"
)

// ValidReadiness reports whether r is one of the values above.
func ValidReadiness(r Readiness) bool {
	switch r {
	case ReadinessReady, ReadinessNotReady, ReadinessUnknown, ReadinessNotSupported:
		return true
	}
	return false
}

// ReportEntry is one server in a report.
type ReportEntry struct {
	ServerID  string
	State     ProcessState
	Readiness Readiness
}

// Report is one batch from an agent: its session epoch, a sequence number
// that only goes up within the session, the agent's own clock (kept for
// diagnostics, never used for ordering or staleness), and the entries.
type Report struct {
	Epoch     int64
	Seq       int64
	AgentTime time.Time
	Entries   []ReportEntry
}

// ValidateNativeState checks the free text that goes to the log with an
// entry: at most MaxNativeStateBytes of UTF-8, no control characters.
func ValidateNativeState(s string) error {
	if len(s) > MaxNativeStateBytes || !utf8.ValidString(s) {
		return ErrInvalidInput
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return ErrInvalidInput
		}
	}
	return nil
}

// ValidateReport checks a report's shape before it reaches the database:
// epoch and seq at least 1, an agent time, at most MaxServersPerNode
// entries, each with a canonical UUID, a known state and readiness, ready
// only while running (D6), and no server twice. Anything else is
// ErrInvalidInput (R5: the whole report goes).
func ValidateReport(r Report) error {
	if r.Epoch < 1 || r.Seq < 1 || r.AgentTime.IsZero() {
		return fmt.Errorf("%w: epoch, seq and agent time are required", ErrInvalidInput)
	}
	if len(r.Entries) > MaxServersPerNode {
		return fmt.Errorf("%w: %d entries, at most %d", ErrInvalidInput, len(r.Entries), MaxServersPerNode)
	}
	seen := make(map[string]bool, len(r.Entries))
	for _, e := range r.Entries {
		if err := ids.Check(e.ServerID); err != nil {
			return fmt.Errorf("%w: server ID: %v", ErrInvalidInput, err)
		}
		if !ValidProcessState(e.State) {
			return fmt.Errorf("%w: unknown process state", ErrInvalidInput)
		}
		if !ValidReadiness(e.Readiness) {
			return fmt.Errorf("%w: unknown readiness", ErrInvalidInput)
		}
		if e.Readiness == ReadinessReady && e.State != ProcessRunning {
			return fmt.Errorf("%w: ready without a running process", ErrInvalidInput)
		}
		if seen[e.ServerID] {
			return fmt.Errorf("%w: server %s listed twice", ErrInvalidInput, e.ServerID)
		}
		seen[e.ServerID] = true
	}
	return nil
}

// requireNode refuses a transaction that doesn't belong to a node.
func (t *Tx) requireNode() error {
	if t.actor.Type != ActorNode || t.role != RoleNode {
		return ErrNotNode
	}
	return nil
}

// NodeHello starts a new agent session for the transaction's node and
// returns the new epoch, the node's server IDs (sorted), and the game port
// of each server that has one (see GamePort). In order:
//
//  1. Read the node's servers. More than MaxServersPerNode is
//     ErrTooManyServers, before anything is written (v2.1 point 3).
//  2. Raise session_epoch by one and set last_seen_at. The row lock this
//     takes serializes the hello against reports, which lock the same row
//     FOR SHARE (C3).
//  3. Every running operation of the node becomes uncertain with error
//     code session_replaced (I8): nobody knows whether the old session
//     finished it.
//
// The caller's InTenantTx rolls everything back if any step fails.
func (t *Tx) NodeHello(ctx context.Context) (epoch int64, serverIDs []string, ports map[string]uint32, err error) {
	if err := t.requireNode(); err != nil {
		return 0, nil, nil, err
	}
	nodeID := t.actor.ID

	rows, err := t.tx.Query(ctx, `
		SELECT server_id::text, config->>'port' FROM waystone.servers
		WHERE node_id = $1
		ORDER BY server_id
		LIMIT $2`, nodeID, MaxServersPerNode+1)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("list node servers: %w", err)
	}
	serverIDs = []string{}
	ports = map[string]uint32{}
	for rows.Next() {
		var id string
		var port *string
		if err := rows.Scan(&id, &port); err != nil {
			rows.Close()
			return 0, nil, nil, fmt.Errorf("scan node server: %w", err)
		}
		serverIDs = append(serverIDs, id)
		if p, ok := GamePort(port); ok {
			ports[id] = p
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, nil, nil, fmt.Errorf("list node servers: %w", err)
	}
	if len(serverIDs) > MaxServersPerNode {
		return 0, nil, nil, ErrTooManyServers
	}

	err = t.tx.QueryRow(ctx, `
		UPDATE waystone.nodes
		SET session_epoch = session_epoch + 1, last_seen_at = now()
		WHERE node_id = $1
		RETURNING session_epoch`, nodeID).Scan(&epoch)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("bump session epoch: %w", err)
	}

	_, err = t.tx.Exec(ctx, `
		UPDATE waystone.operations
		SET state = 'uncertain', finished_at = $2,
		    error_code = 'session_replaced',
		    error_summary = 'The agent session was replaced while this operation was running.'
		WHERE node_id = $1 AND state = 'running'`, nodeID, t.Now())
	if err != nil {
		return 0, nil, nil, fmt.Errorf("mark running operations uncertain: %w", err)
	}

	return epoch, serverIDs, ports, nil
}

// GamePort reads a server's game port from config->>'port' (D6): a
// decimal integer from 1 to 65535. Anything else, including a missing
// key, means no port, so the agent doesn't probe and reports unknown; it
// never falls back to a default. The dev seed sets the key until the
// stage 6 install writes it.
func GamePort(s *string) (uint32, bool) {
	if s == nil {
		return 0, false
	}
	p, err := strconv.ParseUint(*s, 10, 16)
	if err != nil || p == 0 {
		return 0, false
	}
	return uint32(p), true
}

// AcceptReport stores a report for the transaction's node:
//
//  1. Lock the node row FOR SHARE and check that the report's epoch is
//     the current one; otherwise ErrSessionSuperseded and nothing is
//     written (C3).
//  2. One conditional UPDATE per entry. It only matches a server on this
//     node whose stored (epoch, seq) is older (C2), so an entry for
//     another node's server (F9), a server that moved away, or a stale
//     seq matches nothing and counts as rejected. Process state and
//     readiness are written together; the observation time is the
//     database's now(), the controller's receipt time.
//  3. Set last_seen_at (R11), even if every entry was rejected.
//
// The report must already have passed ValidateReport; it is checked again
// here so the store never trusts its caller on that.
func (t *Tx) AcceptReport(ctx context.Context, r Report) (accepted, rejected int, err error) {
	if err := t.requireNode(); err != nil {
		return 0, 0, err
	}
	if err := ValidateReport(r); err != nil {
		return 0, 0, err
	}
	nodeID := t.actor.ID

	var current int64
	err = t.tx.QueryRow(ctx, `
		SELECT session_epoch FROM waystone.nodes
		WHERE node_id = $1
		FOR SHARE`, nodeID).Scan(&current)
	if err != nil {
		return 0, 0, fmt.Errorf("lock node: %w", err)
	}
	if current != r.Epoch {
		return 0, 0, ErrSessionSuperseded
	}

	for _, e := range r.Entries {
		tag, err := t.tx.Exec(ctx, `
			UPDATE waystone.servers
			SET process_state = $3,
			    process_since = CASE WHEN process_state IS DISTINCT FROM $3 THEN now() ELSE process_since END,
			    process_observed_at = now(),
			    readiness = $7,
			    readiness_since = CASE WHEN readiness IS DISTINCT FROM $7 THEN now() ELSE readiness_since END,
			    readiness_observed_at = now(),
			    report_epoch = $4,
			    report_seq = $5,
			    agent_reported_at = $6
			WHERE server_id = $1 AND node_id = $2
			  AND (report_epoch, report_seq) < ($4::bigint, $5::bigint)`,
			e.ServerID, nodeID, string(e.State), r.Epoch, r.Seq, r.AgentTime, string(e.Readiness))
		if err != nil {
			return 0, 0, fmt.Errorf("store report entry: %w", err)
		}
		if tag.RowsAffected() == 1 {
			accepted++
		} else {
			rejected++
		}
	}

	_, err = t.tx.Exec(ctx, `
		UPDATE waystone.nodes SET last_seen_at = now()
		WHERE node_id = $1`, nodeID)
	if err != nil {
		return 0, 0, fmt.Errorf("refresh last_seen_at: %w", err)
	}

	return accepted, rejected, nil
}
