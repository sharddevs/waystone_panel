// SPDX-License-Identifier: AGPL-3.0-only

// Package store is the controller's PostgreSQL layer. Every query goes
// through InTenantTx, which first works out who is asking and which tenant
// they belong to. Row-level security in the database then has the context
// it needs, and without that context it shows nothing at all.
package store

import (
	"context"
	"fmt"
	"strconv"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sharddevs/waystone_panel/internal/ids"
)

// The actor types. They must match the CHECK on memberships.actor_type.
const (
	ActorLocalUser = "local_user" // a Unix user on the API socket; the ID is their uid in decimal
	ActorNode      = "node"       // an agent; the ID is its node UUID
	ActorUser      = "user"       // a browser account; the ID is its user UUID (5a-1, C4)
)

// The roles a membership can have.
const (
	RoleAdmin = "admin"
	RoleNode  = "node"
)

// Actor is whoever made the request. Type and ID only ever come from the
// kernel (the socket's peer credentials) or from the controller's own
// config, never from the request itself, so a client can't claim to be
// someone else. Name is just for display and gets copied into history.
type Actor struct {
	Type string
	ID   string
	Name string
}

// ValidateActor checks that a is well formed before it gets anywhere near
// the database. Any problem comes back wrapped in ErrInvalidActor.
//
// A browser user's ID, like a node's, must be a valid UUID (5a-1).
//
// A local user's ID has to be a plain decimal uid that fits in 32 bits:
// digits only, no sign, and no leading zero, so "1000" and "01000" can't
// end up as two different actors. A node's ID must be a valid UUID. The
// name must be 1 to 100 bytes of UTF-8 without control characters, because
// it ends up in history that people read.
func ValidateActor(a Actor) error {
	if !utf8.ValidString(a.Name) ||
		len(a.Name) < 1 ||
		len(a.Name) > 100 {
		return fmt.Errorf("%w: invalid actor name", ErrInvalidActor)
	}

	for _, r := range a.Name {
		if unicode.IsControl(r) {
			return fmt.Errorf("%w: control character in actor name", ErrInvalidActor)
		}
	}

	switch a.Type {
	case ActorLocalUser:
		if a.ID == "" || (len(a.ID) > 1 && a.ID[0] == '0') {
			return fmt.Errorf("%w: invalid UID", ErrInvalidActor)
		}

		// ParseUint below would refuse these too. Checking each digit
		// here just makes the rule plain to see.
		for _, c := range a.ID {
			if c < '0' || c > '9' {
				return fmt.Errorf("%w: invalid UID", ErrInvalidActor)
			}
		}

		if _, err := strconv.ParseUint(a.ID, 10, 32); err != nil {
			return fmt.Errorf("%w: invalid UID: %v", ErrInvalidActor, err)
		}

	case ActorNode:
		if err := ids.Check(a.ID); err != nil {
			return fmt.Errorf("%w: invalid node ID: %v", ErrInvalidActor, err)
		}

	case ActorUser:
		if err := ids.Check(a.ID); err != nil {
			return fmt.Errorf("%w: invalid user ID: %v", ErrInvalidActor, err)
		}

	default:
		return fmt.Errorf("%w: unknown actor type", ErrInvalidActor)
	}

	return nil
}

// Store holds a pool of connections that log in as waystone_app.
type Store struct {
	pool *pgxpool.Pool
	// Now is the clock for every timestamp the store writes. Open sets it
	// to time.Now; tests swap in a fixed clock.
	Now func() time.Time
}

// Open connects to PostgreSQL. The dsn must log in as waystone_app, the
// runtime role that row-level security applies to.
func Open(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	// pgxpool.New doesn't connect yet, so ping to find a bad dsn now and
	// not on the first request.
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("open database: %w", err)
	}
	return &Store{pool: pool, Now: time.Now}, nil
}

// Close closes the pool.
func (s *Store) Close() { s.pool.Close() }

// Tx is one transaction whose actor, tenant and role are already known
// and already set in the database session.
type Tx struct {
	tx       pgx.Tx
	actor    Actor
	tenantID string
	role     string
	now      time.Time
}

// Actor returns who this transaction acts for.
func (t *Tx) Actor() Actor { return t.actor }

// TenantID returns the tenant this transaction is limited to.
func (t *Tx) TenantID() string { return t.tenantID }

// Role returns the actor's role in that tenant.
func (t *Tx) Role() string { return t.role }

// Now returns the time the transaction started. Everything written in one
// transaction uses this same time, so related rows always agree.
func (t *Tx) Now() time.Time { return t.now }

// InTenantTx runs fn inside one transaction that is set up for actor
// (contract C5):
//
//  1. The actor is validated before anything touches the database.
//  2. The actor's type and ID go into transaction-local settings, which
//     the memberships policy reads.
//  3. The membership lookup can now only see this actor's rows. There
//     must be exactly one: none is ErrNoMembership, more than one is
//     ErrAmbiguousMembership.
//  4. The tenant from that membership goes into a third setting, which
//     turns on the tenant policies for every other table.
//  5. fn runs. If it returns nil we commit; otherwise we roll back and
//     hand back fn's error as it is.
//
// The tenant comes from the database and never from the request. Setting
// app.tenant_id by hand isn't enough either, because every policy also
// checks the membership (I5).
func (s *Store) InTenantTx(ctx context.Context, actor Actor, fn func(ctx context.Context, tx *Tx) error) error {
	if err := ValidateActor(actor); err != nil {
		return err
	}

	// Read the clock once, before the transaction, so Tx.Now is the same
	// value for everything fn writes.
	now := s.Now()

	dbtx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tenant transaction: %w", err)
	}

	// Every early return below rolls back. After a successful Commit,
	// Rollback does nothing.
	defer dbtx.Rollback(ctx)

	// The last argument, true, makes the settings local to this
	// transaction. With false they would stay on the pooled connection,
	// and the next request to use it would run as this actor.
	_, err = dbtx.Exec(ctx, `
    SELECT
        set_config('app.actor_type', $1, true),
        set_config('app.actor_id', $2, true)
`, actor.Type, actor.ID)
	if err != nil {
		return fmt.Errorf("set actor context: %w", err)
	}

	// No WHERE clause needed: the memberships policy already limits this
	// to the actor's own rows.
	rows, err := dbtx.Query(ctx, `
    SELECT tenant_id::text, role
    FROM waystone.memberships
`)
	if err != nil {
		return fmt.Errorf("query actor memberships: %w", err)
	}
	defer rows.Close()

	var tenantID, role string
	membershipCount := 0

	// Count every row rather than stopping at the first, so we notice
	// when there is more than one.
	for rows.Next() {
		var foundTenantID, foundRole string

		if err := rows.Scan(&foundTenantID, &foundRole); err != nil {
			return fmt.Errorf("scan actor membership: %w", err)
		}

		membershipCount++
		if membershipCount == 1 {
			tenantID = foundTenantID
			role = foundRole
		}
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("read actor memberships: %w", err)
	}

	switch membershipCount {
	case 0:
		return ErrNoMembership
	case 1:
		// Exactly one, which is what we want.
	default:
		// Stage 4 has no way to pick an organization yet, so guessing
		// would be wrong (I6).
		return ErrAmbiguousMembership
	}

	// The tenant setting goes in only now, once the membership is proven.
	_, err = dbtx.Exec(ctx, `
    SELECT set_config('app.tenant_id', $1, true)
`, tenantID)
	if err != nil {
		return fmt.Errorf("set tenant context: %w", err)
	}

	tx := &Tx{
		tx:       dbtx,
		actor:    actor,
		tenantID: tenantID,
		role:     role,
		now:      now,
	}

	// fn's error goes back unwrapped, so callers can still use errors.Is
	// on things like ErrNotFound.
	if err := fn(ctx, tx); err != nil {
		return err
	}

	if err := dbtx.Commit(ctx); err != nil {
		return fmt.Errorf("commit tenant transaction: %w", err)
	}

	return nil
}
