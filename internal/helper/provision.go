package helper

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"

	helperv1 "github.com/sharddevs/waystone_panel/gen/waystone/helper/v1"
)

// Where provisioning puts things on a Linux node.
const (
	DefaultUnitDir = "/etc/systemd/system"
	// DefaultProvisionerSocketPath is where waystone-provisioner.socket
	// listens (option C, approved 2026-10-10).
	DefaultProvisionerSocketPath = "/run/waystone-provisioner/provisioner.sock"
)

// DefaultRuntimes maps the runtime names an agent may ask for to the
// program the unit runs. The agent only ever sends a name, so it can never
// make root write an arbitrary program path into a unit.
var DefaultRuntimes = map[string]string{
	"java25": "/usr/lib/jvm/java-25-openjdk-amd64/bin/java",
}

// Heap limits in MiB, and the room the JVM needs on top of its heap
// (metaspace, threads, buffers). The unit's MemoryMax= is heap + overhead.
const (
	MinHeapMiB     = 512
	MaxHeapMiB     = 32768
	JVMOverheadMiB = 1024
)

// Spec is a provisioning request that passed validation.
type Spec struct {
	Runtime string
	HeapMiB uint32
}

// ErrConflict means something already exists that Waystone did not create
// for this server. Provisioning never takes over such things.
var ErrConflict = errors.New("conflict")

// ValidateSpec checks a provisioning spec. Errors wrap ErrInvalidRequest.
//
// Refuse:
//   - a nil spec;
//   - unknown fields inside the spec. ValidateRequest's check only covers
//     the outer message; nested messages carry their own unknown fields;
//   - a runtime that is not a key of DefaultRuntimes (this also refuses
//     anything that looks like a path);
//   - a heap outside MinHeapMiB..MaxHeapMiB (both ends allowed).
func ValidateSpec(spec *helperv1.ProvisionSpec) (Spec, error) {
	if spec == nil {
		return Spec{}, fmt.Errorf("%w: missing provision spec", ErrInvalidRequest)
	}
	if len(spec.ProtoReflect().GetUnknown()) != 0 {
		return Spec{}, fmt.Errorf("%w: unknown provision spec fields", ErrInvalidRequest)
	}
	if _, ok := DefaultRuntimes[spec.GetRuntime()]; !ok {
		return Spec{}, fmt.Errorf("%w: unknown runtime", ErrInvalidRequest)
	}
	if spec.GetHeapMib() < MinHeapMiB || spec.GetHeapMib() > MaxHeapMiB {
		return Spec{}, fmt.Errorf("%w: heap size out of range", ErrInvalidRequest)
	}
	return Spec{
		Runtime: spec.GetRuntime(),
		HeapMiB: spec.GetHeapMib(),
	}, nil
}

// ServerUser is the Unix user of a server: "ws-" plus the first 12 hex
// digits of the SHA-256 of the server ID. serverID must already be valid.
//
// Why a hash: Linux user names are limited to 32 characters, and a UUIDv7
// starts with a timestamp, so cutting the ID short would give servers
// created close together the same prefix.
func ServerUser(serverID string) string {
	hash := sha256.Sum256([]byte(serverID))
	return fmt.Sprintf("ws-%x", hash[:6])
}

// AccountComment is the comment (GECOS field) that marks an account as
// created by Waystone for this server.
func AccountComment(serverID string) string {
	return "waystone server " + serverID
}

// Account is the part of a Unix account provisioning cares about.
type Account struct {
	Name    string
	UID     uint32
	GID     uint32
	Comment string
}

// ErrNoAccount is returned by Accounts.Lookup when the user does not exist.
var ErrNoAccount = errors.New("no such account")

// Accounts finds and creates Unix accounts. Tests swap in a fake.
type Accounts interface {
	// Lookup returns the account, or an error wrapping ErrNoAccount.
	Lookup(name string) (Account, error)
	// Create adds a system account with its own group.
	Create(ctx context.Context, name, comment string) error
}

// Units writes a server's systemd unit file. Tests swap in a fake.
type Units interface {
	// Check returns an error wrapping ErrConflict if a unit file for the
	// server exists and was not written by Waystone. Nil if it is missing
	// or ours.
	Check(serverID string) error
	// Write puts content in place and reports whether the file changed.
	Write(serverID string, content []byte) (changed bool, err error)
}

// Provisioner sets up the root-owned things a server needs: its account
// and its unit. The server's directory is not created here: the unit's
// StateDirectory= makes systemd create it, owned by the server's user, on
// first start.
type Provisioner struct {
	Accounts Accounts
	Units    Units
	// Runner runs "systemctl daemon-reload" after a unit changed.
	Runner Runner
	// Runtimes maps runtime names to programs. Production uses
	// DefaultRuntimes; tests point a name at a temporary file.
	Runtimes map[string]string
}

// Provision creates whatever is missing and reports whether anything
// changed. Running it again with the same input changes nothing.
//
// Order matters. Check everything that can be checked before changing
// anything, and create things in dependency order, so a failure halfway
// leaves nothing that points at something missing:
//
//  1. The runtime: spec.Runtime must be in p.Runtimes, and that path must
//     be an existing regular file. Otherwise return an error (not
//     ErrConflict: the machine is missing a runtime).
//  2. p.Units.Check: a unit file Waystone did not write means stop now.
//  3. The account ServerUser(serverID). If it does not exist, Create it
//     with AccountComment(serverID), then Lookup again. If it exists (or
//     after creating it), it must carry exactly AccountComment(serverID)
//     and a UID from 1 to 999 (a system account, never root). Anything
//     else is ErrConflict: an account Waystone did not make is never
//     taken over.
//  4. RenderUnit with the server ID, the account's name, the runtime's
//     path, the heap and MemoryMaxMiB = heap + JVMOverheadMiB, then
//     p.Units.Write.
//  5. Only if the unit changed: run exactly
//     /usr/bin/systemctl --no-ask-password daemon-reload
//
// Return true if the account or the unit changed. Wrap
// errors with what was being done ("create account: %w"), keeping
// ErrConflict detectable with errors.Is.
func (p Provisioner) Provision(ctx context.Context, serverID string, spec Spec) (bool, error) {
	runtimePath, ok := p.Runtimes[spec.Runtime]
	if !ok {
		return false, fmt.Errorf("runtime %q is not configured", spec.Runtime)
	}
	info, err := os.Stat(runtimePath)
	if err != nil {
		return false, fmt.Errorf("stat runtime %q: %w", runtimePath, err)
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("runtime %q is not a regular file", runtimePath)
	}
	if err := p.Units.Check(serverID); err != nil {
		return false, fmt.Errorf("check unit: %w", err)
	}
	username := ServerUser(serverID)
	comment := AccountComment(serverID)

	account, err := p.Accounts.Lookup(username)
	if err != nil && !errors.Is(err, ErrNoAccount) {
		return false, fmt.Errorf("lookup account: %w", err)
	}
	accountCreated := false

	if errors.Is(err, ErrNoAccount) {
		if err := p.Accounts.Create(ctx, username, comment); err != nil {
			return false, fmt.Errorf("create account: %w", err)
		}

		accountCreated = true

		account, err = p.Accounts.Lookup(username)
		if err != nil {
			return false, fmt.Errorf("lookup created account: %w", err)
		}
	}
	if account.Comment != comment {
		return false, fmt.Errorf("%w: account ownership comment mismatch", ErrConflict)
	}
	if account.UID == 0 || account.UID > 999 {
		return false, fmt.Errorf("%w: account UID outside system range", ErrConflict)
	}
	params := UnitParams{
		ServerID:     serverID,
		User:         account.Name,
		Java:         runtimePath,
		HeapMiB:      spec.HeapMiB,
		MemoryMaxMiB: spec.HeapMiB + JVMOverheadMiB,
	}
	content, err := RenderUnit(params)
	if err != nil {
		return false, fmt.Errorf("render unit: %w", err)
	}
	unitChanged, err := p.Units.Write(serverID, content)
	if err != nil {
		return false, fmt.Errorf("write unit: %w", err)
	}
	if unitChanged {
		_, err := p.Runner.Run(ctx, []string{
			"/usr/bin/systemctl",
			"--no-ask-password",
			"daemon-reload",
		})
		if err != nil {
			return false, fmt.Errorf("daemon-reload: %w", err)
		}
	}
	return accountCreated || unitChanged, nil
}
