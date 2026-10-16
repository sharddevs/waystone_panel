// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/sharddevs/waystone_panel/internal/password"
	"github.com/sharddevs/waystone_panel/internal/store"
)

// fakeAccounts records what runUser asked the store to do.
type fakeAccounts struct {
	calls  []string
	by     store.Actor
	user   store.NewUser
	role   string
	err    error // returned by every call
	closed bool
}

func (f *fakeAccounts) Bootstrap(_ context.Context, by store.Actor, u store.NewUser) (store.User, error) {
	f.calls, f.by, f.user = append(f.calls, "bootstrap"), by, u
	return store.User{Username: u.Username}, f.err
}
func (f *fakeAccounts) Create(_ context.Context, by store.Actor, u store.NewUser, role string) (store.User, error) {
	f.calls, f.by, f.user, f.role = append(f.calls, "create"), by, u, role
	return store.User{Username: u.Username}, f.err
}
func (f *fakeAccounts) SetPassword(_ context.Context, by store.Actor, username, hash string) error {
	f.calls, f.by, f.user = append(f.calls, "set-password"), by, store.NewUser{Username: username, PasswordHash: hash}
	return f.err
}
func (f *fakeAccounts) Disable(_ context.Context, by store.Actor, username string) error {
	f.calls, f.by, f.user = append(f.calls, "disable"), by, store.NewUser{Username: username}
	return f.err
}
func (f *fakeAccounts) Close(context.Context) error { f.closed = true; return nil }

var me = store.Actor{Type: store.ActorLocalUser, ID: "1000", Name: "trevor"}

// harness wires runUser to fakes. answers are what the password prompts
// return, in order.
type harness struct {
	acc     *fakeAccounts
	openErr error
	dsn     string
	prompts []string
	answers []string
	hashed  []string
	stdout  bytes.Buffer
	stderr  bytes.Buffer
}

func newHarness(answers ...string) *harness {
	return &harness{acc: &fakeAccounts{}, answers: answers}
}

func (h *harness) run(args ...string) int {
	return runUser(args, &h.stdout, &h.stderr, userDeps{
		open: func(_ context.Context, dsn string) (accountStore, error) {
			h.dsn = dsn
			if h.openErr != nil {
				return nil, h.openErr
			}
			return h.acc, nil
		},
		readPassword: func(prompt string) (string, error) {
			h.prompts = append(h.prompts, prompt)
			if len(h.answers) == 0 {
				return "", errors.New("no more answers")
			}
			a := h.answers[0]
			h.answers = h.answers[1:]
			return a, nil
		},
		hash: func(pw string) (string, error) {
			h.hashed = append(h.hashed, pw)
			return "$argon2id$hash-of-" + pw, nil
		},
		self: func() (store.Actor, error) { return me, nil },
	})
}

const pw = "a long enough password"

func TestUserBootstrap(t *testing.T) {
	h := newHarness(pw, pw)
	if code := h.run("bootstrap", "-db", "owner-dsn", "trevor"); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, h.stderr.String())
	}
	if h.dsn != "owner-dsn" {
		t.Errorf("opened %q, want the -db value", h.dsn)
	}
	if fmt.Sprint(h.acc.calls) != "[bootstrap]" || !h.acc.closed {
		t.Fatalf("calls %v closed=%v", h.acc.calls, h.acc.closed)
	}
	want := store.NewUser{Username: "trevor", DisplayName: "trevor", PasswordHash: "$argon2id$hash-of-" + pw}
	if h.acc.user != want || h.acc.by != me {
		t.Errorf("Bootstrap(%+v, %+v), want (%+v, %+v)", h.acc.by, h.acc.user, me, want)
	}
	if len(h.prompts) != 2 {
		t.Errorf("prompted %d times, want 2 (password, then again)", len(h.prompts))
	}
	if !strings.Contains(h.stdout.String(), "trevor") || h.stderr.Len() != 0 {
		t.Errorf("stdout %q stderr %q", h.stdout.String(), h.stderr.String())
	}
	// The password itself never reaches the output.
	if strings.Contains(h.stdout.String()+h.stderr.String(), pw) {
		t.Error("the password was printed")
	}
}

func TestUserBootstrapDisplayName(t *testing.T) {
	h := newHarness(pw, pw)
	if code := h.run("bootstrap", "-db", "d", "-name", "Trevor G", "trevor"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if h.acc.user.DisplayName != "Trevor G" {
		t.Errorf("display name %q", h.acc.user.DisplayName)
	}
}

func TestUserCreate(t *testing.T) {
	h := newHarness(pw, pw)
	if code := h.run("create", "-db", "d", "-role", "admin", "alice"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if fmt.Sprint(h.acc.calls) != "[create]" || h.acc.role != store.RoleAdmin || h.acc.user.Username != "alice" {
		t.Errorf("calls %v role %q user %+v", h.acc.calls, h.acc.role, h.acc.user)
	}
}

func TestUserSetPasswordAndDisable(t *testing.T) {
	h := newHarness(pw, pw)
	if code := h.run("set-password", "-db", "d", "alice"); code != 0 {
		t.Fatalf("set-password exit %d: %s", code, h.stderr.String())
	}
	if fmt.Sprint(h.acc.calls) != "[set-password]" || h.acc.user.PasswordHash != "$argon2id$hash-of-"+pw {
		t.Errorf("calls %v user %+v", h.acc.calls, h.acc.user)
	}

	h = newHarness()
	if code := h.run("disable", "-db", "d", "alice"); code != 0 {
		t.Fatalf("disable exit %d: %s", code, h.stderr.String())
	}
	if fmt.Sprint(h.acc.calls) != "[disable]" || len(h.prompts) != 0 {
		t.Errorf("calls %v, prompts %v (disable asks for no password)", h.acc.calls, h.prompts)
	}
}

// Usage problems are exit 2, print the usage, and never open the
// database or ask for a password.
func TestUserUsage(t *testing.T) {
	cases := [][]string{
		{},
		{"delete", "-db", "d", "trevor"},
		{"bootstrap"},
		{"bootstrap", "trevor"},             // no -db
		{"bootstrap", "-db", "", "trevor"},  // empty -db
		{"bootstrap", "-db", "d"},           // no username
		{"bootstrap", "-db", "d", "a", "b"}, // two usernames
		{"bootstrap", "-db", "d", "Trevor"}, // invalid username
		{"bootstrap", "-db", "d", "-password", "x", "trevor"}, // no such flag, ever
		{"bootstrap", "-db", "d", "-role", "admin", "trevor"}, // bootstrap has no -role
		{"create", "-db", "d", "alice"},                       // -role is required
		{"create", "-db", "d", "-role", "viewer", "alice"},
		{"create", "-db", "d", "-role", "node", "alice"},
		{"set-password", "-db", "d", "-name", "x", "alice"},
		{"disable", "-db", "d"},
	}
	for _, args := range cases {
		h := newHarness(pw, pw)
		code := h.run(args...)
		if code != 2 {
			t.Errorf("%q: exit %d, want 2", args, code)
		}
		if !strings.Contains(h.stderr.String(), "usage:") {
			t.Errorf("%q: no usage on stderr: %q", args, h.stderr.String())
		}
		if h.dsn != "" || len(h.prompts) != 0 || len(h.acc.calls) != 0 {
			t.Errorf("%q: opened %q, prompted %d times, calls %v; want nothing", args, h.dsn, len(h.prompts), h.acc.calls)
		}
	}
}

// S-Q6a: the owner check happens before the password is asked for.
func TestUserNotOwner(t *testing.T) {
	h := newHarness(pw, pw)
	h.openErr = store.ErrNotOwner
	if code := h.run("bootstrap", "-db", "app-dsn", "trevor"); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if len(h.prompts) != 0 {
		t.Errorf("asked for a password before the owner check")
	}
	if !strings.Contains(h.stderr.String(), "waystone_owner") {
		t.Errorf("stderr %q doesn't say the owner is needed", h.stderr.String())
	}
}

func TestUserPasswordProblems(t *testing.T) {
	for name, answers := range map[string][]string{
		"mismatch":  {pw, pw + "x"},
		"too short": {"short", "short"},
		"too long":  {strings.Repeat("a", password.MaxLength+1), strings.Repeat("a", password.MaxLength+1)},
		"no input":  {},
	} {
		h := newHarness(answers...)
		if code := h.run("bootstrap", "-db", "d", "trevor"); code != 1 {
			t.Errorf("%s: exit %d, want 1", name, code)
		}
		if len(h.acc.calls) != 0 || len(h.hashed) != 0 {
			t.Errorf("%s: calls %v hashed %d; want neither", name, h.acc.calls, len(h.hashed))
		}
		if h.stderr.Len() == 0 {
			t.Errorf("%s: nothing on stderr", name)
		}
	}
}

// The store's refusals are exit 1 with a readable line.
func TestUserStoreErrors(t *testing.T) {
	for err, want := range map[error]string{
		store.ErrAlreadyBootstrapped: "user create",
		store.ErrNotBootstrapped:     "user bootstrap",
		store.ErrUsernameTaken:       "taken",
		store.ErrNotFound:            "no such account",
		store.ErrAmbiguousTenant:     "more than one organization",
	} {
		h := newHarness(pw, pw)
		h.acc.err = err
		if code := h.run("bootstrap", "-db", "d", "trevor"); code != 1 {
			t.Errorf("%v: exit %d, want 1", err, code)
		}
		if !strings.Contains(h.stderr.String(), want) {
			t.Errorf("%v: stderr %q, want it to mention %q", err, h.stderr.String(), want)
		}
		if h.stdout.Len() != 0 {
			t.Errorf("%v: stdout %q, want nothing", err, h.stdout.String())
		}
	}
}

// The real wiring hashes with the approved parameters.
func TestRealUserDepsHash(t *testing.T) {
	if testing.Short() {
		t.Skip("hashes with 64 MiB")
	}
	h, err := realUserDeps().hash(pw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=65536,t=3,p=4$") {
		t.Fatalf("hash %q doesn't use password.Default", h)
	}
}

func TestLocalActor(t *testing.T) {
	a, err := localActor()
	if err != nil {
		t.Fatal(err)
	}
	if a.Type != store.ActorLocalUser || store.ValidateActor(a) != nil {
		t.Fatalf("localActor = %+v", a)
	}
}
