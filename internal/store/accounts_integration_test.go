// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Stage 5a-1: the account commands, as waystone_owner (§4, S-Q6a, S-Q6b).

// fakeHash passes the store's check (an argon2id PHC prefix). The store
// never verifies a hash; internal/password does.
const fakeHash = "$argon2id$v=19$m=64,t=1,p=1$c2FsdHNhbHRzYWx0c2FsdA$a2V5a2V5a2V5a2V5a2V5a2V5a2V5a2V5a2V5a2U"

// cli is the local user running the account commands.
var cli = Actor{Type: ActorLocalUser, ID: "1000", Name: "trevor"}

func newUser(name string) NewUser {
	return NewUser{Username: name, DisplayName: "The " + name, PasswordHash: fakeHash}
}

// oneTenant is setup with tenant B and everything in it removed, so the
// install has exactly one organization, as v0.1 does.
func oneTenant(t *testing.T) *env {
	t.Helper()
	e := setup(t)
	for _, q := range []string{
		`DELETE FROM waystone.servers WHERE tenant_id = '` + tenantB + `'`,
		`DELETE FROM waystone.nodes WHERE tenant_id = '` + tenantB + `'`,
		`DELETE FROM waystone.memberships WHERE tenant_id = '` + tenantB + `'`,
		`DELETE FROM waystone.tenants WHERE tenant_id = '` + tenantB + `'`,
	} {
		mustExec(t, e.owner, q)
	}
	return e
}

// openAccounts opens the owner side and closes it at the end of the test.
func openAccounts(t *testing.T, e *env) *Accounts {
	t.Helper()
	a, err := OpenAccounts(context.Background(), ownerDSN())
	if err != nil {
		t.Fatalf("OpenAccounts: %v", err)
	}
	a.Now = e.clock.Now
	t.Cleanup(func() { a.Close(context.Background()) })
	return a
}

func countWhere(t *testing.T, conn *pgx.Conn, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// S-Q6a: only a real owner connection gets in, checked before any write.
func TestOpenAccountsNeedsOwner(t *testing.T) {
	setup(t)
	a, err := OpenAccounts(context.Background(), appDSN())
	if !errors.Is(err, ErrNotOwner) {
		if a != nil {
			a.Close(context.Background())
		}
		t.Fatalf("OpenAccounts(app) = %v, want ErrNotOwner", err)
	}
}

func TestBootstrap(t *testing.T) {
	e := oneTenant(t)
	a := openAccounts(t, e)
	ctx := context.Background()

	u, err := a.Bootstrap(ctx, cli, newUser("trevor"))
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if u.TenantID != tenantA || u.Role != RoleAdmin || u.Username != "trevor" || u.DisplayName != "The trevor" {
		t.Errorf("Bootstrap = %+v", u)
	}
	if err := ValidateActor(Actor{Type: ActorUser, ID: u.ID, Name: u.DisplayName}); err != nil {
		t.Errorf("user ID %q is not a valid actor ID: %v", u.ID, err)
	}

	// The membership, an admin one, in the only organization.
	if n := countWhere(t, e.owner, `SELECT count(*) FROM waystone.memberships
		WHERE tenant_id = $1 AND actor_type = 'user' AND actor_id = $2 AND role = 'admin'`, tenantA, u.ID); n != 1 {
		t.Errorf("admin memberships = %d, want 1", n)
	}
	// The account, with the default appearance and the given hash.
	var hash, theme, density, mode string
	var disabled bool
	if err := e.owner.QueryRow(ctx, `SELECT password_hash, theme, density, mode, disabled_at IS NOT NULL
		FROM waystone.users WHERE user_id = $1`, u.ID).Scan(&hash, &theme, &density, &mode, &disabled); err != nil {
		t.Fatal(err)
	}
	if hash != fakeHash || theme != "stone" || density != "comfortable" || mode != "dark" || disabled {
		t.Errorf("users row: hash ok=%v theme=%s density=%s mode=%s disabled=%v", hash == fakeHash, theme, density, mode, disabled)
	}
	// The audit row names the local user who ran it.
	if n := countWhere(t, e.owner, `SELECT count(*) FROM waystone.audit_events
		WHERE tenant_id = $1 AND outcome = 'recorded' AND action = 'user.bootstrap'
		  AND actor_type = 'local_user' AND actor_id = '1000' AND actor_name = 'trevor'
		  AND resource_type = 'user' AND resource_id = $2`, tenantA, u.ID); n != 1 {
		t.Errorf("bootstrap audit rows = %d, want 1", n)
	}

	// S-Q6b: a second bootstrap is refused and writes nothing.
	if _, err := a.Bootstrap(ctx, cli, newUser("mallory")); !errors.Is(err, ErrAlreadyBootstrapped) {
		t.Fatalf("second Bootstrap = %v, want ErrAlreadyBootstrapped", err)
	}
	if n := count(t, e.owner, "users"); n != 1 {
		t.Errorf("users after a refused bootstrap = %d, want 1", n)
	}
	if n := countWhere(t, e.owner, `SELECT count(*) FROM waystone.memberships WHERE actor_type = 'user'`); n != 1 {
		t.Errorf("user memberships after a refused bootstrap = %d, want 1", n)
	}
}

// A membership of type "user" without an account (a hand-made row) also
// counts as bootstrapped.
func TestBootstrapRefusedByStrayMembership(t *testing.T) {
	e := oneTenant(t)
	mustExec(t, e.owner, `INSERT INTO waystone.memberships (tenant_id, actor_type, actor_id, role)
		VALUES ($1, 'user', '0199e1a0-0000-7000-8000-0000000000f1', 'admin')`, tenantA)
	if _, err := openAccounts(t, e).Bootstrap(context.Background(), cli, newUser("trevor")); !errors.Is(err, ErrAlreadyBootstrapped) {
		t.Fatalf("Bootstrap = %v, want ErrAlreadyBootstrapped", err)
	}
	if n := count(t, e.owner, "users"); n != 0 {
		t.Errorf("users = %d, want 0", n)
	}
}

// S-Q6b: two bootstraps at the same time give exactly one account.
func TestBootstrapConcurrent(t *testing.T) {
	e := oneTenant(t)
	a1, a2 := openAccounts(t, e), openAccounts(t, e)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, a := range []*Accounts{a1, a2} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = a.Bootstrap(context.Background(), cli, newUser([]string{"first", "second"}[i]))
		}()
	}
	wg.Wait()
	ok, refused := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrAlreadyBootstrapped):
			refused++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if ok != 1 || refused != 1 {
		t.Fatalf("concurrent bootstraps: %d succeeded, %d refused; want 1 and 1", ok, refused)
	}
	if n := count(t, e.owner, "users"); n != 1 {
		t.Fatalf("users = %d, want 1", n)
	}
}

// With no organization yet, bootstrap creates the default one.
func TestBootstrapCreatesDefaultOrganization(t *testing.T) {
	e := setup(t)
	for _, q := range []string{
		`DELETE FROM waystone.servers`, `DELETE FROM waystone.nodes`,
		`DELETE FROM waystone.memberships`, `DELETE FROM waystone.tenants`,
	} {
		mustExec(t, e.owner, q)
	}
	u, err := openAccounts(t, e).Bootstrap(context.Background(), cli, newUser("trevor"))
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	var name string
	if err := e.owner.QueryRow(context.Background(), `SELECT name FROM waystone.tenants WHERE tenant_id = $1`, u.TenantID).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != DefaultTenantName || count(t, e.owner, "tenants") != 1 {
		t.Fatalf("organization %q (of %d), want one named %q", name, count(t, e.owner, "tenants"), DefaultTenantName)
	}
}

// More than one organization: v0.1 can't choose, so it refuses.
func TestBootstrapAmbiguousOrganization(t *testing.T) {
	e := setup(t) // tenants A and B
	if _, err := openAccounts(t, e).Bootstrap(context.Background(), cli, newUser("trevor")); !errors.Is(err, ErrAmbiguousTenant) {
		t.Fatalf("Bootstrap = %v, want ErrAmbiguousTenant", err)
	}
	if n := count(t, e.owner, "users"); n != 0 {
		t.Errorf("users = %d, want 0", n)
	}
}

func TestCreate(t *testing.T) {
	e := oneTenant(t)
	a := openAccounts(t, e)
	ctx := context.Background()

	// S-Q6b: create is not a way around bootstrap.
	if _, err := a.Create(ctx, cli, newUser("alice"), RoleAdmin); !errors.Is(err, ErrNotBootstrapped) {
		t.Fatalf("Create before bootstrap = %v, want ErrNotBootstrapped", err)
	}
	if n := count(t, e.owner, "users"); n != 0 {
		t.Fatalf("users = %d, want 0", n)
	}

	if _, err := a.Bootstrap(ctx, cli, newUser("trevor")); err != nil {
		t.Fatal(err)
	}
	u, err := a.Create(ctx, cli, newUser("alice"), RoleAdmin)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if u.TenantID != tenantA || u.Role != RoleAdmin {
		t.Errorf("Create = %+v", u)
	}
	if n := countWhere(t, e.owner, `SELECT count(*) FROM waystone.memberships
		WHERE actor_type = 'user' AND actor_id = $1 AND role = 'admin'`, u.ID); n != 1 {
		t.Errorf("memberships = %d, want 1", n)
	}
	if n := countWhere(t, e.owner, `SELECT count(*) FROM waystone.audit_events
		WHERE outcome = 'recorded' AND action = 'user.create' AND resource_id = $1 AND actor_id = '1000'`, u.ID); n != 1 {
		t.Errorf("create audit rows = %d, want 1", n)
	}

	if _, err := a.Create(ctx, cli, newUser("alice"), RoleAdmin); !errors.Is(err, ErrUsernameTaken) {
		t.Errorf("duplicate username = %v, want ErrUsernameTaken", err)
	}
	for _, role := range []string{"", "viewer", RoleNode, "Admin"} {
		if _, err := a.Create(ctx, cli, newUser("bob"), role); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("Create(role %q) = %v, want ErrInvalidInput", role, err)
		}
	}
	if n := count(t, e.owner, "users"); n != 2 {
		t.Errorf("users = %d, want 2", n)
	}
}

// Every refusal happens before any write.
func TestAccountInputRefused(t *testing.T) {
	e := oneTenant(t)
	a := openAccounts(t, e)
	ctx := context.Background()
	bad := map[string]NewUser{
		"empty username":     {Username: "", DisplayName: "x", PasswordHash: fakeHash},
		"uppercase":          {Username: "Trevor", DisplayName: "x", PasswordHash: fakeHash},
		"space":              {Username: "tre vor", DisplayName: "x", PasswordHash: fakeHash},
		"leading dot":        {Username: ".trevor", DisplayName: "x", PasswordHash: fakeHash},
		"33 characters":      {Username: strings.Repeat("a", 33), DisplayName: "x", PasswordHash: fakeHash},
		"empty display name": {Username: "trevor", DisplayName: "", PasswordHash: fakeHash},
		"control character":  {Username: "trevor", DisplayName: "bad\nname", PasswordHash: fakeHash},
		"long display name":  {Username: "trevor", DisplayName: strings.Repeat("a", 101), PasswordHash: fakeHash},
		"not argon2id":       {Username: "trevor", DisplayName: "x", PasswordHash: "$2b$10$bcrypthash"},
		"plain password":     {Username: "trevor", DisplayName: "x", PasswordHash: "hunter2hunter2"},
	}
	for name, u := range bad {
		if _, err := a.Bootstrap(ctx, cli, u); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s: Bootstrap = %v, want ErrInvalidInput", name, err)
		}
	}
	for _, by := range []Actor{
		{Type: ActorNode, ID: nodeA, Name: "node a"},
		{Type: ActorUser, ID: "0199e1a0-0000-7000-8000-0000000000f1", Name: "browser"},
		{Type: ActorLocalUser, ID: "x", Name: "bad"},
	} {
		if _, err := a.Bootstrap(ctx, by, newUser("trevor")); !errors.Is(err, ErrInvalidActor) {
			t.Errorf("Bootstrap by %+v = %v, want ErrInvalidActor", by, err)
		}
	}
	if n := count(t, e.owner, "users"); n != 0 {
		t.Errorf("users = %d, want 0", n)
	}
}

// insertSession adds a session row as the owner, for tests that need one
// without going through the app side.
func insertSession(t *testing.T, e *env, userID string, token byte) {
	t.Helper()
	tok := make([]byte, 32)
	tok[0] = token
	mustExec(t, e.owner, `INSERT INTO waystone.sessions (tenant_id, session_id, user_id, token_sha256, csrf_token,
		created_at, last_seen_at, expires_at, client_ip, user_agent)
		SELECT tenant_id, gen_random_uuid(), user_id, $2, $3, now(), now(), now() + interval '1 day', '192.0.2.1', ''
		FROM waystone.users WHERE user_id = $1`, userID, tok, strings.Repeat("c", 43))
}

func TestSetPassword(t *testing.T) {
	e := oneTenant(t)
	a := openAccounts(t, e)
	ctx := context.Background()
	u, err := a.Bootstrap(ctx, cli, newUser("trevor"))
	if err != nil {
		t.Fatal(err)
	}
	insertSession(t, e, u.ID, 1)
	insertSession(t, e, u.ID, 2)

	newHash := "$argon2id$v=19$m=64,t=1,p=1$bmV3c2FsdG5ld3NhbHRuZQ$bmV3a2V5bmV3a2V5bmV3a2V5bmV3a2V5bmV3a2U"
	if err := a.SetPassword(ctx, cli, "trevor", newHash); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	if n := countWhere(t, e.owner, `SELECT count(*) FROM waystone.users WHERE user_id = $1 AND password_hash = $2`, u.ID, newHash); n != 1 {
		t.Error("the hash didn't change")
	}
	if n := count(t, e.owner, "sessions"); n != 0 {
		t.Errorf("sessions after a password change = %d, want 0", n)
	}
	if n := countWhere(t, e.owner, `SELECT count(*) FROM waystone.audit_events WHERE action = 'user.set_password' AND resource_id = $1`, u.ID); n != 1 {
		t.Errorf("set_password audit rows = %d, want 1", n)
	}

	if err := a.SetPassword(ctx, cli, "nobody", newHash); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetPassword(unknown) = %v, want ErrNotFound", err)
	}
	if err := a.SetPassword(ctx, cli, "trevor", "plain"); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("SetPassword(not a hash) = %v, want ErrInvalidInput", err)
	}
}

func TestDisable(t *testing.T) {
	e := oneTenant(t)
	a := openAccounts(t, e)
	ctx := context.Background()
	u, err := a.Bootstrap(ctx, cli, newUser("trevor"))
	if err != nil {
		t.Fatal(err)
	}
	insertSession(t, e, u.ID, 1)

	if err := a.Disable(ctx, cli, "trevor"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if n := countWhere(t, e.owner, `SELECT count(*) FROM waystone.users WHERE user_id = $1 AND disabled_at IS NOT NULL`, u.ID); n != 1 {
		t.Error("not disabled")
	}
	if n := count(t, e.owner, "sessions"); n != 0 {
		t.Errorf("sessions after disable = %d, want 0", n)
	}
	if n := countWhere(t, e.owner, `SELECT count(*) FROM waystone.audit_events WHERE action = 'user.disable' AND resource_id = $1`, u.ID); n != 1 {
		t.Errorf("disable audit rows = %d, want 1", n)
	}
	// Disabling twice is fine and keeps the first time.
	if err := a.Disable(ctx, cli, "trevor"); err != nil {
		t.Errorf("second Disable: %v", err)
	}
	if err := a.Disable(ctx, cli, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Disable(unknown) = %v, want ErrNotFound", err)
	}
}
