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
	"github.com/sharddevs/waystone_panel/internal/ids"
)

// Browser sign-in, seen from the database (5a-1). The web side (5a-2)
// decides when a session has expired and what a token looks like; this
// file only stores and finds them.

// LoginRecord is what login_lookup returns for one username.
type LoginRecord struct {
	UserID       string
	TenantID     string
	DisplayName  string
	PasswordHash string
	Disabled     bool
}

// Actor is the browser actor for this account.
func (r LoginRecord) Actor() Actor {
	return Actor{Type: ActorUser, ID: r.UserID, Name: r.DisplayName}
}

// LookupLogin finds a username through waystone.login_lookup, the one
// pre-actor path to users (Q4). An invalid or unknown username is
// ErrNotFound; an invalid one never reaches the database.
func (s *Store) LookupLogin(ctx context.Context, username string) (LoginRecord, error) {
	if !ValidUsername(username) {
		return LoginRecord{}, ErrNotFound
	}
	var r LoginRecord
	err := s.pool.QueryRow(ctx, `
		SELECT user_id::text, tenant_id::text, display_name, password_hash, disabled
		FROM waystone.login_lookup($1)`, username).
		Scan(&r.UserID, &r.TenantID, &r.DisplayName, &r.PasswordHash, &r.Disabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return LoginRecord{}, ErrNotFound
	}
	if err != nil {
		return LoginRecord{}, fmt.Errorf("login lookup: %w", err)
	}
	return r, nil
}

// SessionRecord is what session_lookup returns for one token.
type SessionRecord struct {
	SessionID   string
	UserID      string
	TenantID    string
	DisplayName string
	CSRFToken   string
	CreatedAt   time.Time
	LastSeenAt  time.Time
	ExpiresAt   time.Time
}

// Actor is the browser actor this session acts for.
func (r SessionRecord) Actor() Actor {
	return Actor{Type: ActorUser, ID: r.UserID, Name: r.DisplayName}
}

// LookupSession finds a session by the SHA-256 of its token through
// waystone.session_lookup (Q4). An unknown token, or one whose account is
// disabled, is ErrNotFound. Expiry is the caller's decision: the record
// carries CreatedAt, LastSeenAt and ExpiresAt.
func (s *Store) LookupSession(ctx context.Context, tokenSHA256 [32]byte) (SessionRecord, error) {
	var r SessionRecord
	err := s.pool.QueryRow(ctx, `
		SELECT session_id::text, user_id::text, tenant_id::text, display_name, csrf_token,
		       created_at, last_seen_at, expires_at
		FROM waystone.session_lookup($1)`, tokenSHA256[:]).
		Scan(&r.SessionID, &r.UserID, &r.TenantID, &r.DisplayName, &r.CSRFToken,
			&r.CreatedAt, &r.LastSeenAt, &r.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return SessionRecord{}, ErrNotFound
	}
	if err != nil {
		return SessionRecord{}, fmt.Errorf("session lookup: %w", err)
	}
	return r, nil
}

// NewSession is a session to store.
type NewSession struct {
	TokenSHA256 [32]byte
	CSRFToken   string // 43 characters of base64url (32 random bytes)
	ExpiresAt   time.Time
	ClientIP    string // as web.ClientIP formats it, 1 to 45 bytes
	UserAgent   string // stored after CleanUserAgent
}

var csrfPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

// requireUser: the method is for a browser user acting for themselves.
func (t *Tx) requireUser() error {
	if t.actor.Type != ActorUser {
		return ErrNotUser
	}
	return nil
}

// CreateSession stores a session for the transaction's own user and
// returns its new ID (a UUIDv7). created_at and last_seen_at are the
// transaction's time. The actor must be an ActorUser (ErrNotUser).
// ErrInvalidInput for: a CSRF token that isn't 43 base64url characters,
// ExpiresAt not after the transaction's time, a client IP that is empty
// or over 45 bytes. The user agent is cleaned with CleanUserAgent, never
// refused.
func (t *Tx) CreateSession(ctx context.Context, n NewSession) (string, error) {
	if err := t.requireUser(); err != nil {
		return "", err
	}
	if !csrfPattern.MatchString(n.CSRFToken) {
		return "", fmt.Errorf("%w: invalid CSRF token", ErrInvalidInput)
	}
	if !n.ExpiresAt.After(t.now) {
		return "", fmt.Errorf("%w: session already expired", ErrInvalidInput)
	}
	if len(n.ClientIP) < 1 || len(n.ClientIP) > 45 {
		return "", fmt.Errorf("%w: invalid client IP", ErrInvalidInput)
	}
	id, err := ids.NewV7()
	if err != nil {
		return "", fmt.Errorf("generate session ID: %w", err)
	}
	_, err = t.tx.Exec(ctx, `
		INSERT INTO waystone.sessions (tenant_id, session_id, user_id, token_sha256, csrf_token,
			created_at, last_seen_at, expires_at, client_ip, user_agent)
		VALUES ($1, $2, $3, $4, $5, $6, $6, $7, $8, $9)`,
		t.tenantID, id, t.actor.ID, n.TokenSHA256[:], n.CSRFToken, t.now, n.ExpiresAt,
		n.ClientIP, CleanUserAgent(n.UserAgent))
	if err != nil {
		return "", fmt.Errorf("insert session: %w", err)
	}
	return id, nil
}

// CleanUserAgent makes a client's User-Agent safe to store and show: an
// invalid UTF-8 or control-character value becomes "", and anything else
// is cut to at most 200 bytes on a character boundary.
func CleanUserAgent(ua string) string {
	if !utf8.ValidString(ua) {
		return ""
	}
	for _, r := range ua {
		if unicode.IsControl(r) {
			return ""
		}
	}
	if len(ua) <= 200 {
		return ua
	}
	cut := 200
	for !utf8.RuneStart(ua[cut]) {
		cut--
	}
	return ua[:cut]
}

// TouchSession sets one of the user's own sessions' last_seen_at to the
// transaction's time (the idle timeout counts from it). Another user's
// session, or a missing one, is ErrNotFound.
func (t *Tx) TouchSession(ctx context.Context, sessionID string) error {
	return t.ownSession(ctx, sessionID, `UPDATE waystone.sessions SET last_seen_at = $2 WHERE session_id = $1`, t.now)
}

// DeleteSession deletes one of the user's own sessions (sign-out, or an
// expired one). Another user's session, or a missing one, is ErrNotFound.
func (t *Tx) DeleteSession(ctx context.Context, sessionID string) error {
	return t.ownSession(ctx, sessionID, `DELETE FROM waystone.sessions WHERE session_id = $1`)
}

func (t *Tx) ownSession(ctx context.Context, sessionID, sql string, args ...any) error {
	if err := t.requireUser(); err != nil {
		return err
	}
	if err := ids.Check(sessionID); err != nil {
		return fmt.Errorf("%w: invalid session ID", ErrInvalidInput)
	}
	tag, err := t.tx.Exec(ctx, sql, append([]any{sessionID}, args...)...)
	if err != nil {
		return fmt.Errorf("session: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrNotFound
	}
	return nil
}

// Preferences are a user's appearance settings (design-tokens-v0.1.md).
type Preferences struct {
	Theme   string // "stone" or "slate"
	Density string // "comfortable" or "compact"
	Mode    string // "dark", "light" or "system"
}

// DefaultPreferences is what a new account starts with.
var DefaultPreferences = Preferences{Theme: "stone", Density: "comfortable", Mode: "dark"}

// Valid reports whether every field is one of its allowed values.
func (p Preferences) Valid() bool {
	return (p.Theme == "stone" || p.Theme == "slate") &&
		(p.Density == "comfortable" || p.Density == "compact") &&
		(p.Mode == "dark" || p.Mode == "light" || p.Mode == "system")
}

// Preferences returns the transaction's own user's settings.
func (t *Tx) Preferences(ctx context.Context) (Preferences, error) {
	if err := t.requireUser(); err != nil {
		return Preferences{}, err
	}
	var p Preferences
	err := t.tx.QueryRow(ctx, `SELECT theme, density, mode FROM waystone.users WHERE user_id = $1`, t.actor.ID).
		Scan(&p.Theme, &p.Density, &p.Mode)
	if errors.Is(err, pgx.ErrNoRows) {
		return Preferences{}, ErrNotFound
	}
	if err != nil {
		return Preferences{}, fmt.Errorf("read preferences: %w", err)
	}
	return p, nil
}

// SetPreferences replaces the transaction's own user's settings. An
// invalid value is ErrInvalidInput, before any database work.
func (t *Tx) SetPreferences(ctx context.Context, p Preferences) error {
	if err := t.requireUser(); err != nil {
		return err
	}
	if !p.Valid() {
		return fmt.Errorf("%w: invalid preferences", ErrInvalidInput)
	}
	tag, err := t.tx.Exec(ctx, `UPDATE waystone.users SET theme = $2, density = $3, mode = $4 WHERE user_id = $1`,
		t.actor.ID, p.Theme, p.Density, p.Mode)
	if err != nil {
		return fmt.Errorf("write preferences: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrNotFound
	}
	return nil
}
