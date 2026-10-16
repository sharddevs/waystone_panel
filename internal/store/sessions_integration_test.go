// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"
)

// Stage 5a-1: browser sign-in from the app side (waystone_app).

// accounts bootstraps "trevor" and creates "alice", both admins of
// tenant A, and returns them as browser actors.
func accounts(t *testing.T) (*env, Actor, Actor) {
	t.Helper()
	e := oneTenant(t)
	a := openAccounts(t, e)
	ctx := context.Background()
	tr, err := a.Bootstrap(ctx, cli, newUser("trevor"))
	if err != nil {
		t.Fatal(err)
	}
	al, err := a.Create(ctx, cli, newUser("alice"), RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	return e,
		Actor{Type: ActorUser, ID: tr.ID, Name: tr.DisplayName},
		Actor{Type: ActorUser, ID: al.ID, Name: al.DisplayName}
}

var csrf = strings.Repeat("A", 43)

func session(token string, e *env) NewSession {
	return NewSession{
		TokenSHA256: sha256.Sum256([]byte(token)),
		CSRFToken:   csrf,
		ExpiresAt:   e.clock.Now().Add(7 * 24 * time.Hour),
		ClientIP:    "192.0.2.10",
		UserAgent:   "Mozilla/5.0 (X11; Linux x86_64) Firefox/140.0",
	}
}

// createSession runs CreateSession for actor in its own transaction.
func (e *env) createSession(actor Actor, n NewSession) (string, error) {
	var id string
	err := e.store.InTenantTx(context.Background(), actor, func(ctx context.Context, tx *Tx) error {
		var err error
		id, err = tx.CreateSession(ctx, n)
		return err
	})
	return id, err
}

func (e *env) asActor(actor Actor, fn func(ctx context.Context, tx *Tx) error) error {
	return e.store.InTenantTx(context.Background(), actor, fn)
}

func TestLookupLogin(t *testing.T) {
	e, trevor, _ := accounts(t)
	ctx := context.Background()

	r, err := e.store.LookupLogin(ctx, "trevor")
	if err != nil {
		t.Fatalf("LookupLogin: %v", err)
	}
	if r.UserID != trevor.ID || r.TenantID != tenantA || r.DisplayName != trevor.Name || r.PasswordHash != fakeHash || r.Disabled {
		t.Errorf("LookupLogin = %+v", r)
	}
	if r.Actor() != trevor {
		t.Errorf("Actor() = %+v, want %+v", r.Actor(), trevor)
	}
	for _, name := range []string{"nobody", "Trevor", "", "' OR '1'='1", "trevor%", strings.Repeat("a", 33)} {
		if _, err := e.store.LookupLogin(ctx, name); !errors.Is(err, ErrNotFound) {
			t.Errorf("LookupLogin(%q) = %v, want ErrNotFound", name, err)
		}
	}

	if err := openAccounts(t, e).Disable(ctx, cli, "trevor"); err != nil {
		t.Fatal(err)
	}
	r, err = e.store.LookupLogin(ctx, "trevor")
	if err != nil || !r.Disabled {
		t.Errorf("LookupLogin(disabled) = %+v, %v; want Disabled", r, err)
	}
}

func TestSessionLifecycle(t *testing.T) {
	e, trevor, _ := accounts(t)
	ctx := context.Background()
	n := session("token-1", e)

	id, err := e.createSession(trevor, n)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	r, err := e.store.LookupSession(ctx, n.TokenSHA256)
	if err != nil {
		t.Fatalf("LookupSession: %v", err)
	}
	now := e.clock.Now()
	if r.SessionID != id || r.UserID != trevor.ID || r.TenantID != tenantA || r.DisplayName != trevor.Name ||
		r.CSRFToken != csrf || !r.CreatedAt.Equal(now) || !r.LastSeenAt.Equal(now) || !r.ExpiresAt.Equal(n.ExpiresAt) {
		t.Errorf("LookupSession = %+v", r)
	}
	if r.Actor() != trevor {
		t.Errorf("Actor() = %+v", r.Actor())
	}
	// Only the hash is stored, never the token.
	if c := countWhere(t, e.owner, `SELECT count(*) FROM waystone.sessions WHERE token_sha256 = $1`, n.TokenSHA256[:]); c != 1 {
		t.Errorf("rows with the token's hash = %d, want 1", c)
	}

	e.clock.Advance(time.Hour)
	if err := e.asActor(trevor, func(ctx context.Context, tx *Tx) error { return tx.TouchSession(ctx, id) }); err != nil {
		t.Fatalf("TouchSession: %v", err)
	}
	r, _ = e.store.LookupSession(ctx, n.TokenSHA256)
	if !r.LastSeenAt.Equal(e.clock.Now()) || !r.CreatedAt.Equal(now) {
		t.Errorf("after touch: created %v last seen %v", r.CreatedAt, r.LastSeenAt)
	}

	if err := e.asActor(trevor, func(ctx context.Context, tx *Tx) error { return tx.DeleteSession(ctx, id) }); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, err := e.store.LookupSession(ctx, n.TokenSHA256); !errors.Is(err, ErrNotFound) {
		t.Errorf("LookupSession after delete = %v, want ErrNotFound", err)
	}
	if err := e.asActor(trevor, func(ctx context.Context, tx *Tx) error { return tx.DeleteSession(ctx, id) }); !errors.Is(err, ErrNotFound) {
		t.Errorf("second DeleteSession = %v, want ErrNotFound", err)
	}
	if _, err := e.store.LookupSession(ctx, sha256.Sum256([]byte("never issued"))); !errors.Is(err, ErrNotFound) {
		t.Errorf("LookupSession(unknown) = %v, want ErrNotFound", err)
	}
}

// A disabled account's sessions are never found, even if a row survives.
func TestSessionOfDisabledUser(t *testing.T) {
	e, trevor, _ := accounts(t)
	n := session("token-1", e)
	if _, err := e.createSession(trevor, n); err != nil {
		t.Fatal(err)
	}
	mustExec(t, e.owner, `UPDATE waystone.users SET disabled_at = now() WHERE user_id = $1`, trevor.ID)
	if _, err := e.store.LookupSession(context.Background(), n.TokenSHA256); !errors.Is(err, ErrNotFound) {
		t.Fatalf("LookupSession(disabled user) = %v, want ErrNotFound", err)
	}
}

func TestCreateSessionRefused(t *testing.T) {
	e, trevor, _ := accounts(t)
	if _, err := e.createSession(adminA, session("x", e)); !errors.Is(err, ErrNotUser) {
		t.Errorf("CreateSession as local_user = %v, want ErrNotUser", err)
	}
	bad := map[string]func(*NewSession){
		"short CSRF token":  func(n *NewSession) { n.CSRFToken = "abc" },
		"CSRF token chars":  func(n *NewSession) { n.CSRFToken = strings.Repeat("+", 43) },
		"long CSRF token":   func(n *NewSession) { n.CSRFToken = strings.Repeat("A", 44) },
		"expires now":       func(n *NewSession) { n.ExpiresAt = e.clock.Now() },
		"expired":           func(n *NewSession) { n.ExpiresAt = e.clock.Now().Add(-time.Second) },
		"no client IP":      func(n *NewSession) { n.ClientIP = "" },
		"client IP too big": func(n *NewSession) { n.ClientIP = strings.Repeat("1", 46) },
	}
	for name, change := range bad {
		n := session("x-"+name, e)
		change(&n)
		if _, err := e.createSession(trevor, n); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s: CreateSession = %v, want ErrInvalidInput", name, err)
		}
	}
	if c := count(t, e.owner, "sessions"); c != 0 {
		t.Errorf("sessions = %d, want 0", c)
	}
	// A hostile User-Agent is cleaned, not refused.
	n := session("ua", e)
	n.UserAgent = "evil\nheader"
	if _, err := e.createSession(trevor, n); err != nil {
		t.Fatalf("CreateSession(control character in UA) = %v", err)
	}
	var ua string
	if err := e.owner.QueryRow(context.Background(), `SELECT user_agent FROM waystone.sessions`).Scan(&ua); err != nil || ua != "" {
		t.Errorf("stored user agent = %q, %v; want empty", ua, err)
	}
}

// A user sees, touches and deletes only their own sessions and their
// own account row.
func TestSessionsAreOwnOnly(t *testing.T) {
	e, trevor, alice := accounts(t)
	id, err := e.createSession(trevor, session("trevor", e))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.createSession(alice, session("alice", e)); err != nil {
		t.Fatal(err)
	}

	err = e.asActor(alice, func(ctx context.Context, tx *Tx) error {
		if err := tx.TouchSession(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("alice touching trevor's session = %v, want ErrNotFound", err)
		}
		if err := tx.DeleteSession(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("alice deleting trevor's session = %v, want ErrNotFound", err)
		}
		if n := count(t, tx.tx, "sessions"); n != 1 {
			t.Errorf("alice sees %d sessions, want 1 (her own)", n)
		}
		if n := count(t, tx.tx, "users"); n != 1 {
			t.Errorf("alice sees %d accounts, want 1 (her own)", n)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if c := count(t, e.owner, "sessions"); c != 2 {
		t.Errorf("sessions = %d, want 2", c)
	}
	// A local admin of the same tenant doesn't see browser sessions either.
	err = e.asActor(adminA, func(ctx context.Context, tx *Tx) error {
		if n := count(t, tx.tx, "sessions"); n != 0 {
			t.Errorf("local admin sees %d sessions, want 0", n)
		}
		if n := count(t, tx.tx, "users"); n != 0 {
			t.Errorf("local admin sees %d accounts, want 0", n)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPreferences(t *testing.T) {
	e, trevor, alice := accounts(t)
	get := func(a Actor) Preferences {
		t.Helper()
		var p Preferences
		if err := e.asActor(a, func(ctx context.Context, tx *Tx) error {
			var err error
			p, err = tx.Preferences(ctx)
			return err
		}); err != nil {
			t.Fatalf("Preferences: %v", err)
		}
		return p
	}
	set := func(a Actor, p Preferences) error {
		return e.asActor(a, func(ctx context.Context, tx *Tx) error { return tx.SetPreferences(ctx, p) })
	}

	if p := get(trevor); p != DefaultPreferences {
		t.Errorf("new account = %+v, want %+v", p, DefaultPreferences)
	}
	want := Preferences{Theme: "slate", Density: "compact", Mode: "system"}
	if err := set(trevor, want); err != nil {
		t.Fatalf("SetPreferences: %v", err)
	}
	if p := get(trevor); p != want {
		t.Errorf("after set = %+v, want %+v", p, want)
	}
	if p := get(alice); p != DefaultPreferences {
		t.Errorf("alice changed too: %+v", p)
	}
	for _, p := range []Preferences{
		{Theme: "pink", Density: "compact", Mode: "dark"},
		{Theme: "stone", Density: "cozy", Mode: "dark"},
		{Theme: "stone", Density: "compact", Mode: "auto"},
		{},
	} {
		if err := set(trevor, p); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("SetPreferences(%+v) = %v, want ErrInvalidInput", p, err)
		}
	}
	if err := set(adminA, want); !errors.Is(err, ErrNotUser) {
		t.Errorf("SetPreferences as local_user = %v, want ErrNotUser", err)
	}
}

// C4: a browser admin resolves an uncertain operation under their own
// name, exactly as a local admin can.
func TestUserAdminResolves(t *testing.T) {
	e, trevor, _ := accounts(t)
	op, _, err := e.create(adminA, CreateOperationInput{ServerID: serverA1, Action: ActionStart})
	if err != nil {
		t.Fatal(err)
	}
	e.setState(t, op.ID, StateRunning, StateUncertain)

	var got Operation
	err = e.asActor(trevor, func(ctx context.Context, tx *Tx) error {
		var err error
		got, err = tx.ResolveOperation(ctx, op.ID, StateFailed, "checked the console")
		return err
	})
	if err != nil {
		t.Fatalf("ResolveOperation as user: %v", err)
	}
	if got.State != StateUncertain || got.Resolution == nil || got.Resolution.By != trevor {
		t.Fatalf("resolved = %+v", got)
	}
}

// A browser user can also start operations; the actor snapshot is the
// user (R10).
func TestUserStartsOperation(t *testing.T) {
	e, trevor, _ := accounts(t)
	op, _, err := e.create(trevor, CreateOperationInput{ServerID: serverA1, Action: ActionStart})
	if err != nil {
		t.Fatalf("CreateOperation as user: %v", err)
	}
	if op.Actor != trevor {
		t.Errorf("operation actor = %+v, want %+v", op.Actor, trevor)
	}
}

// I4: purge_sessions deletes what can't be used any more, in every
// tenant, and nothing else. A5: no session may outlive 168 hours.
func TestPurgeSessionsAndLifetime(t *testing.T) {
	e, trevor, _ := accounts(t)
	ctx := context.Background()
	for _, tok := range []string{"live", "expired", "idle"} {
		if _, err := e.createSession(trevor, session(tok, e)); err != nil {
			t.Fatal(err)
		}
	}
	set := func(tok, sql string) {
		t.Helper()
		h := sha256.Sum256([]byte(tok))
		mustExec(t, e.owner, `UPDATE waystone.sessions SET `+sql+` WHERE token_sha256 = $1`, h[:])
	}
	set("live", `created_at = now() - interval '1 hour', last_seen_at = now() - interval '1 hour', expires_at = now() + interval '6 days'`)
	set("expired", `created_at = now() - interval '8 days', last_seen_at = now() - interval '1 hour', expires_at = now() - interval '1 day'`)
	set("idle", `created_at = now() - interval '13 hours', last_seen_at = now() - interval '12 hours 1 second', expires_at = now() + interval '6 days'`)

	n, err := e.store.PurgeSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("purged %d, want 2", n)
	}
	var left []byte
	if err := e.owner.QueryRow(ctx, `SELECT token_sha256 FROM waystone.sessions`).Scan(&left); err != nil {
		t.Fatalf("the live session: %v", err)
	}
	if h := sha256.Sum256([]byte("live")); string(left) != string(h[:]) {
		t.Error("the wrong session survived")
	}
	if n, err := e.store.PurgeSessions(ctx); err != nil || n != 0 {
		t.Errorf("second purge: %d %v, want 0", n, err)
	}

	// A5: the database refuses a lifetime over 168 hours, even from the
	// owner, and allows exactly 168.
	h := sha256.Sum256([]byte("live"))
	if _, err := e.owner.Exec(ctx, `UPDATE waystone.sessions SET expires_at = created_at + interval '168 hours 1 second' WHERE token_sha256 = $1`, h[:]); err == nil {
		t.Error("a session lifetime over 168 hours was accepted")
	}
	mustExec(t, e.owner, `UPDATE waystone.sessions SET expires_at = created_at + interval '168 hours' WHERE token_sha256 = $1`, h[:])
	long := session("long", e)
	long.ExpiresAt = e.clock.Now().Add(7*24*time.Hour + time.Second)
	if _, err := e.createSession(trevor, long); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("CreateSession with a lifetime over 168 hours: %v, want ErrInvalidInput", err)
	}
}

// A6: a denial can carry the client's address, and only a real one.
func TestDenialClientIP(t *testing.T) {
	e, trevor, _ := accounts(t)
	ctx := context.Background()
	record := func(ip string) error {
		return e.asActor(trevor, func(ctx context.Context, tx *Tx) error {
			return tx.RecordDenial(ctx, Denial{Action: "session.create", ResourceType: "user", ResourceID: trevor.ID,
				Reason: "wrong_password", RequestID: "req-ip", ClientIP: ip})
		})
	}
	for _, ip := range []string{"203.0.113.7", "2001:db8::1", ""} {
		if err := record(ip); err != nil {
			t.Errorf("%q: %v", ip, err)
		}
	}
	for _, bad := range []string{"not an ip", "203.0.113.7 ", "::ffff:203.0.113.007", "fe80::1%" + strings.Repeat("x", 50)} {
		if err := record(bad); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%q: %v, want ErrInvalidInput", bad, err)
		}
	}
	var withIP, without int
	if err := e.owner.QueryRow(ctx, `SELECT count(client_ip), count(*) - count(client_ip) FROM waystone.audit_events WHERE request_id = 'req-ip'`).Scan(&withIP, &without); err != nil {
		t.Fatal(err)
	}
	if withIP != 2 || without != 1 {
		t.Errorf("%d rows with an address and %d without, want 2 and 1", withIP, without)
	}
}
