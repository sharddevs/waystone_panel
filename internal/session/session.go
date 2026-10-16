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
	"sync"
	"time"

	"github.com/sharddevs/waystone_panel/internal/password"
	"github.com/sharddevs/waystone_panel/internal/store"
	"github.com/sharddevs/waystone_panel/internal/web"
)

// The session rules (Q7). Migration 00007 repeats both in SQL:
// purge_sessions uses '12 hours' and the sessions CHECK '168 hours', so
// change them together.
const (
	IdleTimeout     = 12 * time.Hour
	AbsoluteTimeout = store.MaxSessionLifetime
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

// Password hashing is the expensive part of a sign-in (64 MiB each), so
// it is bounded three ways (A2): at most maxHashes run at once, at most
// maxHashWaiters sign-ins may be hashing or waiting to, and none waits
// longer than hashWait. Past either bound the answer is 503
// controller_busy straight away, instead of a queue that grows until real
// sign-ins time out behind it.
const (
	maxHashes      = 2
	maxHashWaiters = 32
	hashWait       = 5 * time.Second
	// auditTimeout bounds a failed sign-in's audit write, which runs after
	// the response has gone (A3).
	auditTimeout = 10 * time.Second
)

// maxLookups is how many session lookups may run at once. Any request
// with a well-formed cookie costs one before anything else bounds it, so
// past this the answer is 503 at once instead of a queue for the
// database pool (5b audit note).
const maxLookups = 32

// maxLookupsPerAddr is how many of those may come from one address (by
// addrKey) at once, so one client flooding random cookies can't take
// every slot and lock signed-in users out (audit N1).
const maxLookupsPerAddr = 8

// errLookupBusy: too many session lookups are already running.
var errLookupBusy = errors.New("session: too many session lookups in progress")

// errHashBusy: too many sign-ins are already hashing or waiting.
var errHashBusy = errors.New("session: too many sign-ins in progress")

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
	hashing chan struct{} // a slot per running hash
	waiting chan struct{} // a slot per sign-in hashing or waiting to
	wait    time.Duration // hashWait; tests shorten it
	lookups chan struct{} // a slot per running ResolveSession
	perAddr inflight      // running ResolveSessions per address
	audits  sync.WaitGroup
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
		waiting: make(chan struct{}, maxHashWaiters),
		wait:    hashWait,
		lookups: make(chan struct{}, maxLookups),
		perAddr: inflight{n: map[string]int{}},
		decoy:   decoy,
	}, nil
}

// ResolveSession is the session rule (web.SessionResolver): the token's
// session exists, its account isn't disabled (session_lookup), it hasn't
// passed its absolute expiry, and it was used within IdleTimeout. An
// expired session is deleted on the way. A live one is touched, at most
// once a minute. At most maxLookups run at once, and at most
// maxLookupsPerAddr from one address; one more is errLookupBusy, which
// web answers with 503.
func (m *Manager) ResolveSession(ctx context.Context, token string) (web.Session, error) {
	key := "ip:-"
	if ip, ok := web.ClientIPFrom(ctx); ok {
		key = addrKey(ip)
	}
	if !m.perAddr.acquire(key, maxLookupsPerAddr) {
		return web.Session{}, errLookupBusy
	}
	defer m.perAddr.release(key)
	select {
	case m.lookups <- struct{}{}:
	default:
		return web.Session{}, errLookupBusy
	}
	defer func() { <-m.lookups }()
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
// a slot at most hashWait, and only if fewer than maxHashWaiters are
// already in line; otherwise it's errHashBusy.
func (m *Manager) verify(ctx context.Context, pw, phc string) (bool, error) {
	select {
	case m.waiting <- struct{}{}:
	default:
		return false, errHashBusy
	}
	defer func() { <-m.waiting }()
	t := time.NewTimer(m.wait)
	defer t.Stop()
	select {
	case m.hashing <- struct{}{}:
	case <-t.C:
		return false, errHashBusy
	case <-ctx.Done():
		return false, ctx.Err()
	}
	defer func() { <-m.hashing }()
	return password.Verify(pw, phc)
}

// Wait waits for the audit writes still running after their responses.
// The controller calls it on the way out.
func (m *Manager) Wait() { m.audits.Wait() }

// inflight counts running requests per key.
type inflight struct {
	mu sync.Mutex
	n  map[string]int
}

// acquire takes one of max places for key, or reports false at once.
func (f *inflight) acquire(key string, max int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.n[key] >= max {
		return false
	}
	f.n[key]++
	return true
}

// release gives back a place acquire took. A key with none left is
// dropped, so the map only holds addresses with requests running.
func (f *inflight) release(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.n[key]--; f.n[key] <= 0 {
		delete(f.n, key)
	}
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
