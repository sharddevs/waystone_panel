// SPDX-License-Identifier: AGPL-3.0-only

// Package session is browser sign-in (stage 5a-2): the four
// /api/v1/session routes, the session rules (idle and absolute expiry),
// and the sign-in rate limit. It plugs into internal/web, which owns the
// cookie, the CSRF check and the guard, and into internal/store, which
// owns the rows. It never sees a raw token in the database: only its
// SHA-256 is stored.
package session

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/sharddevs/waystone_panel/internal/password"
	"github.com/sharddevs/waystone_panel/internal/store"
	"github.com/sharddevs/waystone_panel/internal/web"
)

// The session rules (Q7).
const (
	IdleTimeout     = 12 * time.Hour
	AbsoluteTimeout = 7 * 24 * time.Hour
	// touchEvery: last_seen_at is written at most this often per session,
	// so a page polling every 5 s doesn't write on every request.
	touchEvery = time.Minute
)

// The sign-in rate limit: this many failures in limitWindow, per username
// and per client address, then 429 until the oldest drops out.
const (
	limitWindow  = 15 * time.Minute
	limitPerUser = 5
	limitPerIP   = 20
)

// maxHashes is how many password hashes may run at once (64 MiB each).
const maxHashes = 2

// Store is what the manager needs from internal/store; *store.Store is
// the real one.
type Store interface {
	LookupLogin(ctx context.Context, username string) (store.LoginRecord, error)
	LookupSession(ctx context.Context, tokenSHA256 [32]byte) (store.SessionRecord, error)
	InTenantTx(ctx context.Context, actor store.Actor, fn func(ctx context.Context, tx *store.Tx) error) error
}

// Config is what New needs. Store is required.
type Config struct {
	Store Store
	// Params are the argon2id costs of the stand-in hash an unknown
	// username is checked against. Zero means password.Default, which is
	// what the user command writes. Tests use cheaper ones.
	Params password.Params
	Now    func() time.Time // nil means time.Now
	Log    *log.Logger      // nil means log.Default()
}

// Manager implements web.SessionResolver and serves the session routes.
type Manager struct {
	store   Store
	now     func() time.Time
	log     *log.Logger
	limits  *limiter
	hashing chan struct{}
	// decoy is a real hash of a random password: an unknown username is
	// verified against it, so it costs what a known one does.
	decoy string
}

// New builds the manager. It hashes one random password up front for the
// unknown-username path, so it takes as long as one sign-in.
func New(cfg Config) (*Manager, error) {
	if cfg.Store == nil {
		return nil, errors.New("session: a store is required")
	}
	if cfg.Params == (password.Params{}) {
		cfg.Params = password.Default
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = log.Default()
	}
	decoyPW, err := newToken()
	if err != nil {
		return nil, err
	}
	decoy, err := password.Hash(decoyPW, cfg.Params)
	if err != nil {
		return nil, fmt.Errorf("session: stand-in hash: %w", err)
	}
	return &Manager{
		store:   cfg.Store,
		now:     cfg.Now,
		log:     cfg.Log,
		limits:  newLimiter(limitWindow),
		hashing: make(chan struct{}, maxHashes),
		decoy:   decoy,
	}, nil
}

// ResolveSession is the session rule (web.SessionResolver): the token's
// session exists, its account isn't disabled (session_lookup), it hasn't
// passed its absolute expiry, and it was used within IdleTimeout. An
// expired session is deleted on the way. A live one is touched, at most
// once a minute.
func (m *Manager) ResolveSession(ctx context.Context, token string) (web.Session, error) {
	rec, err := m.store.LookupSession(ctx, hashToken(token))
	if errors.Is(err, store.ErrNotFound) {
		return web.Session{}, web.ErrNoSession
	}
	if err != nil {
		return web.Session{}, err
	}
	now := m.now()
	if !now.Before(rec.ExpiresAt) || now.Sub(rec.LastSeenAt) >= IdleTimeout {
		err := m.store.InTenantTx(ctx, rec.Actor(), func(ctx context.Context, tx *store.Tx) error {
			return tx.DeleteSession(ctx, rec.SessionID)
		})
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			m.log.Printf("session %s expired, and deleting it failed: %v", rec.SessionID, err)
		}
		return web.Session{}, web.ErrNoSession
	}
	if now.Sub(rec.LastSeenAt) >= touchEvery {
		err := m.store.InTenantTx(ctx, rec.Actor(), func(ctx context.Context, tx *store.Tx) error {
			return tx.TouchSession(ctx, rec.SessionID)
		})
		if err != nil {
			return web.Session{}, err
		}
	}
	return web.Session{
		ID:          rec.SessionID,
		UserID:      rec.UserID,
		TenantID:    rec.TenantID,
		DisplayName: rec.DisplayName,
		CSRFToken:   rec.CSRFToken,
		CreatedAt:   rec.CreatedAt,
		ExpiresAt:   rec.ExpiresAt,
	}, nil
}

// verify checks pw against phc, at most maxHashes at once. It waits for
// a slot as long as ctx allows.
func (m *Manager) verify(ctx context.Context, pw, phc string) (bool, error) {
	select {
	case m.hashing <- struct{}{}:
	case <-ctx.Done():
		return false, ctx.Err()
	}
	defer func() { <-m.hashing }()
	return password.Verify(pw, phc)
}

// newToken is 32 random bytes in unpadded base64url: a session token or a
// CSRF token, web.TokenLength characters.
func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("session: random token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func hashToken(token string) [32]byte { return sha256.Sum256([]byte(token)) }
