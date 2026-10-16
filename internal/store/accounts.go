// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/sharddevs/waystone_panel/internal/ids"
)

// The account commands ("controller user ...", 5a-1) work on browser
// accounts as waystone_owner, because only the owner may write
// memberships (stage 4 decision A). The controller itself, running as
// waystone_app, never creates an account or a membership (S-Q6a).

var (
	// ErrNotOwner: the connection isn't waystone_owner with the right to
	// insert memberships. Nothing was written.
	ErrNotOwner = errors.New("not connected as waystone_owner")
	// ErrAlreadyBootstrapped: a browser account exists already, so
	// bootstrap refuses (S-Q6b). Use Create.
	ErrAlreadyBootstrapped = errors.New("already bootstrapped; use \"user create\"")
	// ErrNotBootstrapped: Create needs a bootstrapped first admin.
	ErrNotBootstrapped = errors.New("no administrator yet; use \"user bootstrap\" first")
	// ErrUsernameTaken: another account has this username.
	ErrUsernameTaken = errors.New("username taken")
	// ErrAmbiguousTenant: the install has more than one organization, and
	// v0.1 has no way to choose one.
	ErrAmbiguousTenant = errors.New("more than one organization")
	// ErrNotUser: the method needs a browser user actor.
	ErrNotUser = errors.New("not a browser user")
)

// DefaultTenantName is the organization Bootstrap creates when the
// install has none yet.
const DefaultTenantName = "Default organization"

// bootstrapLock is the transaction-level advisory lock every account
// command takes, so two of them can't interleave (S-Q6b).
const bootstrapLock = 0x5761_7973_746f_6e65 // "Waystone"

var usernamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,31}$`)

// ValidUsername reports whether name can be a username: 1 to 32
// characters, lowercase ASCII letters, digits, '.', '_' and '-', starting
// with a letter or digit. The same rule is a CHECK on users.username.
func ValidUsername(name string) bool { return usernamePattern.MatchString(name) }

// validDisplayName is the actor name rule (ValidateActor), because the
// display name becomes the actor name in history: 1 to 100 bytes of
// UTF-8, no control characters.
func validDisplayName(name string) bool {
	if len(name) < 1 || len(name) > 100 || !utf8.ValidString(name) {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// NewUser is a browser account to create. PasswordHash is a PHC string
// from password.Hash; the store never sees the password itself.
type NewUser struct {
	Username     string
	DisplayName  string
	PasswordHash string
}

// User is a created account.
type User struct {
	ID          string
	TenantID    string
	Username    string
	DisplayName string
	Role        string
}

// Accounts is one waystone_owner connection for the account commands.
type Accounts struct {
	conn *pgx.Conn
	// Now is the clock for written timestamps. OpenAccounts sets it to
	// time.Now.
	Now func() time.Time
}

// OpenAccounts connects with dsn and checks, before anything else, that
// the connection really has the owner's privileges: current_user is
// waystone_owner and it may INSERT into waystone.memberships. Otherwise
// it closes the connection and returns ErrNotOwner (S-Q6a).
func OpenAccounts(ctx context.Context, dsn string) (*Accounts, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	var isOwner, canInsert bool
	err = conn.QueryRow(ctx, `
		SELECT current_user = 'waystone_owner',
		       has_table_privilege('waystone.memberships', 'INSERT')`).Scan(&isOwner, &canInsert)
	if err != nil || !isOwner || !canInsert {
		conn.Close(ctx)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrNotOwner, err)
		}
		return nil, ErrNotOwner
	}
	return &Accounts{conn: conn, Now: time.Now}, nil
}

// Close closes the connection.
func (a *Accounts) Close(ctx context.Context) error { return a.conn.Close(ctx) }

// Bootstrap creates the first administrator (W4, S-Q6b). In one
// transaction, under the advisory lock:
//
//  1. If any browser account or "user" membership exists, it's
//     ErrAlreadyBootstrapped, and nothing is written.
//  2. The organization: the only one there is; a new one named
//     DefaultTenantName if there is none; ErrAmbiguousTenant if there
//     are several.
//  3. The users row, its admin membership and a "recorded" audit row
//     for by (action user.bootstrap).
//
// by is the local user running the command, an ActorLocalUser.
func (a *Accounts) Bootstrap(ctx context.Context, by Actor, u NewUser) (User, error) {
	return a.createUser(ctx, by, u, RoleAdmin, true)
}

// Create adds a later account with role (v0.1: only RoleAdmin; anything
// else is ErrInvalidInput). Like Bootstrap, but it needs an existing
// browser admin (ErrNotBootstrapped otherwise), never creates an
// organization (ErrNotBootstrapped if there is none), and audits
// user.create.
func (a *Accounts) Create(ctx context.Context, by Actor, u NewUser, role string) (User, error) {
	if role != RoleAdmin {
		return User{}, fmt.Errorf("%w: role must be %q", ErrInvalidInput, RoleAdmin)
	}
	return a.createUser(ctx, by, u, role, false)
}

func (a *Accounts) createUser(ctx context.Context, by Actor, u NewUser, role string, bootstrap bool) (User, error) {
	if err := checkLocalActor(by); err != nil {
		return User{}, err
	}
	if !ValidUsername(u.Username) {
		return User{}, fmt.Errorf("%w: invalid username", ErrInvalidInput)
	}
	if !validDisplayName(u.DisplayName) {
		return User{}, fmt.Errorf("%w: invalid display name", ErrInvalidInput)
	}
	if !strings.HasPrefix(u.PasswordHash, "$argon2id$") {
		return User{}, fmt.Errorf("%w: not an argon2id hash", ErrInvalidInput)
	}

	var out User
	err := a.inLockedTx(ctx, func(tx pgx.Tx) error {
		// Under the lock, so two runs can't both see an empty install.
		var hasUsers bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM waystone.users)
			    OR EXISTS (SELECT 1 FROM waystone.memberships WHERE actor_type = 'user')`).Scan(&hasUsers); err != nil {
			return fmt.Errorf("check accounts: %w", err)
		}
		if bootstrap && hasUsers {
			return ErrAlreadyBootstrapped
		}
		if !bootstrap {
			var hasAdmin bool
			if err := tx.QueryRow(ctx, `
				SELECT EXISTS (SELECT 1 FROM waystone.memberships
				               WHERE actor_type = 'user' AND role = 'admin')`).Scan(&hasAdmin); err != nil {
				return fmt.Errorf("check accounts: %w", err)
			}
			if !hasAdmin {
				return ErrNotBootstrapped
			}
		}

		tenantID, err := a.theTenant(ctx, tx, bootstrap)
		if err != nil {
			return err
		}

		userID, err := ids.NewV7()
		if err != nil {
			return fmt.Errorf("generate user ID: %w", err)
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO waystone.users (tenant_id, user_id, username, display_name, password_hash, created_at)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			tenantID, userID, u.Username, u.DisplayName, u.PasswordHash, a.Now())
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return ErrUsernameTaken
			}
			return fmt.Errorf("insert user: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO waystone.memberships (tenant_id, actor_type, actor_id, role)
			VALUES ($1, 'user', $2, $3)`, tenantID, userID, role); err != nil {
			return fmt.Errorf("insert membership: %w", err)
		}
		action := "user.create"
		if bootstrap {
			action = "user.bootstrap"
		}
		if err := a.audit(ctx, tx, by, tenantID, action, userID); err != nil {
			return err
		}
		out = User{ID: userID, TenantID: tenantID, Username: u.Username, DisplayName: u.DisplayName, Role: role}
		return nil
	})
	return out, err
}

// SetPassword replaces username's password hash and deletes all of that
// account's sessions, in one transaction, with a user.set_password audit
// row. An unknown username is ErrNotFound.
func (a *Accounts) SetPassword(ctx context.Context, by Actor, username, hash string) error {
	if err := checkLocalActor(by); err != nil {
		return err
	}
	if !ValidUsername(username) {
		return fmt.Errorf("%w: invalid username", ErrInvalidInput)
	}
	if !strings.HasPrefix(hash, "$argon2id$") {
		return fmt.Errorf("%w: not an argon2id hash", ErrInvalidInput)
	}
	return a.inLockedTx(ctx, func(tx pgx.Tx) error {
		var userID, tenantID string
		err := tx.QueryRow(ctx, `
			UPDATE waystone.users SET password_hash = $2 WHERE username = $1
			RETURNING user_id::text, tenant_id::text`, username, hash).Scan(&userID, &tenantID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("update password: %w", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM waystone.sessions WHERE user_id = $1`, userID); err != nil {
			return fmt.Errorf("delete sessions: %w", err)
		}
		return a.audit(ctx, tx, by, tenantID, "user.set_password", userID)
	})
}

// Disable marks username disabled (if it isn't already) and deletes all
// of its sessions, with a user.disable audit row. A disabled account
// can't sign in, and session_lookup never returns its sessions. An
// unknown username is ErrNotFound.
func (a *Accounts) Disable(ctx context.Context, by Actor, username string) error {
	if err := checkLocalActor(by); err != nil {
		return err
	}
	if !ValidUsername(username) {
		return fmt.Errorf("%w: invalid username", ErrInvalidInput)
	}
	return a.inLockedTx(ctx, func(tx pgx.Tx) error {
		var userID, tenantID string
		err := tx.QueryRow(ctx, `
			UPDATE waystone.users SET disabled_at = COALESCE(disabled_at, $2) WHERE username = $1
			RETURNING user_id::text, tenant_id::text`, username, a.Now()).Scan(&userID, &tenantID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("disable user: %w", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM waystone.sessions WHERE user_id = $1`, userID); err != nil {
			return fmt.Errorf("delete sessions: %w", err)
		}
		return a.audit(ctx, tx, by, tenantID, "user.disable", userID)
	})
}

// inLockedTx runs fn in one transaction that holds the account lock.
func (a *Accounts) inLockedTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := a.conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(bootstrapLock)); err != nil {
		return fmt.Errorf("account lock: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// theTenant returns the install's only organization. With none, bootstrap
// creates DefaultTenantName and Create refuses with ErrNotBootstrapped.
func (a *Accounts) theTenant(ctx context.Context, tx pgx.Tx, create bool) (string, error) {
	rows, err := tx.Query(ctx, `SELECT tenant_id::text FROM waystone.tenants LIMIT 2`)
	if err != nil {
		return "", fmt.Errorf("query organizations: %w", err)
	}
	found, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return "", fmt.Errorf("read organizations: %w", err)
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		if !create {
			return "", ErrNotBootstrapped
		}
		id, err := ids.NewV7()
		if err != nil {
			return "", fmt.Errorf("generate organization ID: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO waystone.tenants (tenant_id, name) VALUES ($1, $2)`, id, DefaultTenantName); err != nil {
			return "", fmt.Errorf("insert organization: %w", err)
		}
		return id, nil
	default:
		return "", ErrAmbiguousTenant
	}
}

// audit writes the "recorded" row for an account command.
func (a *Accounts) audit(ctx context.Context, tx pgx.Tx, by Actor, tenantID, action, userID string) error {
	eventID, err := ids.NewV7()
	if err != nil {
		return fmt.Errorf("generate audit event ID: %w", err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO waystone.audit_events (tenant_id, event_id, actor_type, actor_id, actor_name,
			action, resource_type, resource_id, reason, request_id, outcome, at)
		VALUES ($1, $2, $3, $4, $5, $6, 'user', $7, 'cli', $8, 'recorded', $9)`,
		tenantID, eventID, by.Type, by.ID, by.Name, action, userID, eventID, a.Now())
	if err != nil {
		return fmt.Errorf("record audit event: %w", err)
	}
	return nil
}

// checkLocalActor: account commands are run by a local user, named in
// the audit row.
func checkLocalActor(by Actor) error {
	if err := ValidateActor(by); err != nil {
		return err
	}
	if by.Type != ActorLocalUser {
		return fmt.Errorf("%w: account commands are run by a local user", ErrInvalidActor)
	}
	return nil
}
