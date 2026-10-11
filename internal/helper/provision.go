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
	// listens.
	DefaultProvisionerSocketPath = "/run/waystone-provisioner/provisioner.sock"
)

// DefaultRuntimes maps the runtime names an agent may ask for to the
// program the unit runs. The agent only sends a name, so it can never make
// root write an arbitrary program path into a unit.
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

// ValidateSpec checks a provisioning spec: a known runtime name and a heap
// from MinHeapMiB to MaxHeapMiB. Errors wrap ErrInvalidRequest.
func ValidateSpec(spec *helperv1.ProvisionSpec) (Spec, error) {
	if spec == nil {
		return Spec{}, fmt.Errorf("%w: missing provision spec", ErrInvalidRequest)
	}
	// Nested messages carry their own unknown fields, which
	// ValidateRequest's check doesn't see.
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

// ServerUser returns the Unix user of a server: "ws-" plus the first 12 hex
// digits of the SHA-256 of the server ID. serverID must already be valid.
//
// We hash because Linux user names max out at 32 characters, and a UUIDv7
// starts with a timestamp, so just cutting the ID short would give servers
// created close together the same name.
func ServerUser(serverID string) string {
	hash := sha256.Sum256([]byte(serverID))
	return fmt.Sprintf("ws-%x", hash[:6])
}

// AccountComment returns the comment (GECOS field) that marks an account
// as created by Waystone for this server.
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

// Accounts finds and creates Unix accounts. It's an interface so tests can
// swap in a fake.
type Accounts interface {
	// Lookup returns the account, or an error wrapping ErrNoAccount.
	Lookup(name string) (Account, error)
	// Create adds a system account with its own group.
	Create(ctx context.Context, name, comment string) error
}

// Units checks and writes a server's systemd unit file. Tests swap in a
// fake.
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

// Provision creates the server's account if it is missing, brings its unit
// file up to date and reports whether anything changed. Running it again
// with the same input changes nothing.
//
// The order matters: we check everything we can before changing anything,
// and create things in dependency order (account before the unit that
// names it), so a failure halfway never leaves something pointing at
// something missing. An existing account or unit that Waystone didn't make
// is ErrConflict and is never taken over.
func (p Provisioner) Provision(ctx context.Context, serverID string, spec Spec) (bool, error) {
	// A missing runtime means the machine is incomplete, so these are plain
	// errors, not ErrConflict.
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
	// Whether we just made it or it was already there, the account must be
	// ours: the right comment and a system UID that isn't root.
	if err := validateAccount(account, serverID); err != nil {
		return false, err
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
	// systemd only picks up the new unit file after a daemon-reload.
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
