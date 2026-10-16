// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/user"
	"strconv"
	"time"

	"golang.org/x/term"

	"github.com/sharddevs/waystone_panel/internal/password"
	"github.com/sharddevs/waystone_panel/internal/store"
)

// userTimeout bounds one "controller user" run, prompts included.
const userTimeout = 5 * time.Minute

// accountStore is what the user commands need from *store.Accounts, so
// the tests can use a fake.
type accountStore interface {
	Bootstrap(ctx context.Context, by store.Actor, u store.NewUser) (store.User, error)
	Create(ctx context.Context, by store.Actor, u store.NewUser, role string) (store.User, error)
	SetPassword(ctx context.Context, by store.Actor, username, hash string) error
	Disable(ctx context.Context, by store.Actor, username string) error
	Close(ctx context.Context) error
}

// userDeps are the outside world for runUser.
type userDeps struct {
	// open connects as the owner; store.OpenAccounts in the real program,
	// which refuses anything but waystone_owner (S-Q6a).
	open func(ctx context.Context, dsn string) (accountStore, error)
	// readPassword prompts on the terminal without echo.
	readPassword func(prompt string) (string, error)
	// hash is password.Hash with password.Default.
	hash func(pw string) (string, error)
	// self is the local user running the command, for the audit row.
	self func() (store.Actor, error)
}

func realUserDeps() userDeps {
	return userDeps{
		open: func(ctx context.Context, dsn string) (accountStore, error) {
			return store.OpenAccounts(ctx, dsn)
		},
		readPassword: readTerminalPassword,
		hash:         func(pw string) (string, error) { return password.Hash(pw, password.Default) },
		self:         localActor,
	}
}

const userUsage = `usage:
  controller user bootstrap -db <owner dsn> [-name <display name>] <username>
  controller user create -db <owner dsn> -role admin [-name <display name>] <username>
  controller user set-password -db <owner dsn> <username>
  controller user disable -db <owner dsn> <username>`

// runUser handles "controller user <command> ...". args is everything
// after "user", and the return value is the exit code:
//   - 2 for a usage problem: no or unknown command, an unknown flag, a
//     missing -db, a missing or invalid username, extra arguments, and
//     for create a -role other than "admin". The usage text goes to
//     stderr and nothing else happens.
//   - 1 for a failure, with one line on stderr: the database isn't the
//     owner, already or not yet bootstrapped, username taken or unknown,
//     a password problem, or anything else from the store.
//   - 0 on success, with one line on stdout.
//
// The order matters: arguments, then the owner connection (so nobody
// types a password only to find out it can't be used), then for
// bootstrap, create and set-password the password, asked twice, checked
// with password.Validate and hashed; then the store call. A password is
// never taken from the command line or the environment.
func runUser(args []string, stdout, stderr io.Writer, d userDeps) int {
	usage := func() int {
		fmt.Fprintln(stderr, userUsage)
		return 2
	}
	if len(args) == 0 {
		return usage()
	}
	cmd := args[0]
	switch cmd {
	case "bootstrap", "create", "set-password", "disable":
	default:
		return usage()
	}

	flags := flag.NewFlagSet("user "+cmd, flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { fmt.Fprintln(stderr, userUsage) }
	dsn := flags.String("db", "", "PostgreSQL connection string, as waystone_owner")
	var name, role *string
	if cmd == "bootstrap" || cmd == "create" {
		name = flags.String("name", "", "display name (default: the username)")
	}
	if cmd == "create" {
		role = flags.String("role", "", "the account's role: admin")
	}
	if err := flags.Parse(args[1:]); err != nil {
		return 2
	}
	if *dsn == "" || flags.NArg() != 1 || !store.ValidUsername(flags.Arg(0)) {
		return usage()
	}
	if role != nil && *role != store.RoleAdmin {
		return usage()
	}
	username := flags.Arg(0)
	displayName := username
	if name != nil && *name != "" {
		displayName = *name
	}

	fail := func(err error) int {
		fmt.Fprintln(stderr, "user "+cmd+": "+explain(err))
		return 1
	}

	by, err := d.self()
	if err != nil {
		return fail(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), userTimeout)
	defer cancel()

	// The owner check comes before the password prompt (S-Q6a).
	accounts, err := d.open(ctx, *dsn)
	if err != nil {
		return fail(err)
	}
	defer accounts.Close(context.Background())

	var hash string
	if cmd != "disable" {
		if hash, err = askPassword(d); err != nil {
			return fail(err)
		}
	}

	nu := store.NewUser{Username: username, DisplayName: displayName, PasswordHash: hash}
	switch cmd {
	case "bootstrap":
		if _, err := accounts.Bootstrap(ctx, by, nu); err != nil {
			return fail(err)
		}
		fmt.Fprintf(stdout, "created the first administrator %q\n", username)
	case "create":
		if _, err := accounts.Create(ctx, by, nu, *role); err != nil {
			return fail(err)
		}
		fmt.Fprintf(stdout, "created %s %q\n", *role, username)
	case "set-password":
		if err := accounts.SetPassword(ctx, by, username, hash); err != nil {
			return fail(err)
		}
		fmt.Fprintf(stdout, "password changed for %q; its sessions were signed out\n", username)
	case "disable":
		if err := accounts.Disable(ctx, by, username); err != nil {
			return fail(err)
		}
		fmt.Fprintf(stdout, "disabled %q; its sessions were signed out\n", username)
	}
	return 0
}

// errMismatch: the two password entries differ.
var errMismatch = errors.New("the passwords don't match")

// askPassword prompts twice, checks the entries match and are a valid
// password, and returns the hash.
func askPassword(d userDeps) (string, error) {
	pw, err := d.readPassword("Password: ")
	if err != nil {
		return "", err
	}
	again, err := d.readPassword("Repeat password: ")
	if err != nil {
		return "", err
	}
	if pw != again {
		return "", errMismatch
	}
	if err := password.Validate(pw); err != nil {
		return "", err
	}
	return d.hash(pw)
}

// explain turns the store's errors into a message for the person at the
// terminal.
func explain(err error) string {
	switch {
	case errors.Is(err, store.ErrNotOwner):
		return "this needs the database owner (waystone_owner), the same connection as \"controller migrate\""
	case errors.Is(err, store.ErrAlreadyBootstrapped):
		return "an account exists already, so bootstrap is closed; use \"controller user create\""
	case errors.Is(err, store.ErrNotBootstrapped):
		return "there is no administrator yet; run \"controller user bootstrap\" first"
	case errors.Is(err, store.ErrUsernameTaken):
		return "that username is taken"
	case errors.Is(err, store.ErrNotFound):
		return "no such account"
	case errors.Is(err, store.ErrAmbiguousTenant):
		return "this install has more than one organization, which v0.1 can't handle"
	}
	return err.Error()
}

// readTerminalPassword reads one line from the terminal without echo. It
// refuses when stdin isn't a terminal, so a password can't come from a
// pipe, a file or shell history.
func readTerminalPassword(prompt string) (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return "", errors.New("a password can only be typed at a terminal")
	}
	fmt.Fprint(os.Stderr, prompt)
	b, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	return string(b), nil
}

// localActor is the Unix user running the command: ActorLocalUser with
// the uid, named after the passwd entry, or "uid <n>" when there is none.
func localActor() (store.Actor, error) {
	uid := strconv.Itoa(os.Getuid())
	name := "uid " + uid
	if u, err := user.LookupId(uid); err == nil && u.Username != "" {
		name = u.Username
	}
	a := store.Actor{Type: store.ActorLocalUser, ID: uid, Name: name}
	if err := store.ValidateActor(a); err != nil {
		a.Name = "uid " + uid
	}
	return a, nil
}
