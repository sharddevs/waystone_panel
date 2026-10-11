package helper

import (
	"context"
	"errors"
	"fmt"
	"os/user"
	"strconv"
)

// Absolute paths of the programs Create runs. We never look them up in PATH.
const (
	SystemdRun = "/usr/bin/systemd-run"
	Useradd    = "/usr/sbin/useradd"
)

// UseraddProperties is the sandbox of the transient unit that runs useradd.
// useradd needs /etc writable for its lock, temporary and backup files, and
// the shadow group lets it keep /etc/shadow's group without CAP_CHOWN.
// This list is a security boundary, so it should only ever get stricter.
var UseraddProperties = []string{
	"ProtectSystem=strict",
	"ReadWritePaths=/etc",
	"ProtectHome=yes",
	"PrivateTmp=yes",
	"PrivateDevices=yes",
	"PrivateNetwork=yes",
	"RestrictAddressFamilies=AF_UNIX",
	"NoNewPrivileges=yes",
	"CapabilityBoundingSet=",
	"SupplementaryGroups=shadow",
	"ProtectKernelTunables=yes",
	"ProtectKernelModules=yes",
	"ProtectKernelLogs=yes",
	"ProtectControlGroups=yes",
	"ProtectClock=yes",
	"ProtectHostname=yes",
	"RestrictNamespaces=yes",
	"RestrictRealtime=yes",
	"RestrictSUIDSGID=yes",
	"LockPersonality=yes",
	"MemoryDenyWriteExecute=yes",
	"SystemCallArchitectures=native",
	"SystemCallFilter=@system-service",
}

// SystemAccounts implements Accounts with the system's user database
// (os/user) and useradd.
type SystemAccounts struct {
	Runner Runner
}

// Lookup implements Accounts. Comment is the user's Name field, which is
// the GECOS comment.
func (SystemAccounts) Lookup(name string) (Account, error) {
	u, err := user.Lookup(name)
	if err != nil {
		// errors.As checks whether err (or anything it wraps) is this type.
		var unknown user.UnknownUserError
		if errors.As(err, &unknown) {
			return Account{}, fmt.Errorf("%w: user %q does not exist", ErrNoAccount, name)
		}
		return Account{}, err
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return Account{}, err
	}
	gid, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		return Account{}, err
	}
	return Account{
		Name:    u.Username,
		UID:     uint32(uid),
		GID:     uint32(gid),
		Comment: u.Name,
	}, nil
}

// Create implements Accounts. The provisioning service itself can't write
// /etc, so instead of running useradd directly we ask systemd to run it
// once in a transient unit sandboxed with UseraddProperties, and wait.
// name must have exactly the shape ServerUser produces, or nothing runs.
func (a SystemAccounts) Create(ctx context.Context, name, comment string) error {
	if err := ValidateServerUser(name); err != nil {
		return err
	}
	// --wait and --pipe give us useradd's exit status and stderr, so a
	// failure says why. --collect unloads the unit even when it failed.
	args := []string{
		"--wait",
		"--collect",
		"--quiet",
		"--pipe",
		"--service-type=exec",
		"--unit=waystone-useradd-" + name + ".service",
		"--setenv=LC_ALL=C",
	}
	for _, property := range UseraddProperties {
		args = append(args, "-p", property)
	}
	args = append(args,
		"--",
		Useradd,
		"--system",
		"--user-group",
		"--no-create-home",
		"--home-dir", "/nonexistent",
		"--shell", "/usr/sbin/nologin",
		"--comment", comment,
		"--", name,
	)

	_, err := a.Runner.Run(ctx, append([]string{SystemdRun}, args...))
	if err != nil {
		return fmt.Errorf("create system account %q: %w", name, err)
	}

	return nil
}
