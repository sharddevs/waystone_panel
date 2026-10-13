// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"
	"unicode"
	"unicode/utf8"

	"github.com/sharddevs/waystone_panel/internal/ids"
)

// Denial is one refused request, kept for the audit trail (decision H4).
// Accepted requests don't need one: their operation row already records
// who asked for what.
type Denial struct {
	Action       string // the named action, e.g. "server.start"
	ResourceType string // e.g. "server"
	ResourceID   string // exactly as the client sent it, so it may not be a real ID
	Reason       string // a fixed code such as "forbidden", never error text
	RequestID    string // the request's ID, to find the matching log lines
}

// RecordDenial adds one audit_events row for the transaction's actor and
// tenant, with outcome "denied" and the transaction's time.
//
// Every field must be 1 to 200 bytes of UTF-8 without control characters,
// or it's ErrInvalidInput. Some fields come straight from the client, and
// this keeps junk out of a log that people will read later.
//
// The row only stays if the transaction commits. So after recording a
// denial, return nil from the InTenantTx callback and report the refusal
// some other way. Returning an error would roll the audit row back too.
func (t *Tx) RecordDenial(ctx context.Context, d Denial) error {
	fields := []string{
		d.Action,
		d.ResourceType,
		d.ResourceID,
		d.Reason,
		d.RequestID,
	}

	for _, field := range fields {
		if len(field) == 0 || len(field) > 200 {
			return ErrInvalidInput
		}

		if !utf8.ValidString(field) {
			return ErrInvalidInput
		}

		for _, r := range field {
			if unicode.IsControl(r) {
				return ErrInvalidInput
			}
		}
	}

	eventID, err := ids.NewV7()
	if err != nil {
		return fmt.Errorf("generate audit event ID: %w", err)
	}

	_, err = t.tx.Exec(ctx, `
	INSERT INTO waystone.audit_events (
		tenant_id,
		event_id,
		actor_type,
		actor_id,
		actor_name,
		action,
		resource_type,
		resource_id,
		reason,
		request_id,
		outcome,
		at
	)
	VALUES (
		$1, $2, $3, $4, $5, $6,
		$7, $8, $9, $10, 'denied', $11
	)
`,
		t.TenantID(),
		eventID,
		t.Actor().Type,
		t.Actor().ID,
		t.Actor().Name,
		d.Action,
		d.ResourceType,
		d.ResourceID,
		d.Reason,
		d.RequestID,
		t.Now(),
	)
	if err != nil {
		return fmt.Errorf("record denial: %w", err)
	}

	return nil
}
