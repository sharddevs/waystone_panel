package helper

import (
	"context"
	"errors"
	"fmt"
	"os/user"
	"strconv"
)

// Absolute paths of the programs Create runs. Never looked up in PATH.
const (
	SystemdRun = "/usr/bin/systemd-run"
	Useradd    = "/usr/sbin/useradd"
)

// UseraddProperties is the sandbox of the transient unit that runs useradd
// (decision: notes/stage3b-privilege-decision.md, option C). useradd needs
// the directory /etc writable for its lock, temporary and backup files;
// group shadow lets it keep /etc/shadow's group without CAP_CHOWN.
// This list is part of the security review. Only ever make it stricter.
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

// Lookup implements Accounts.
//   - Use user.Lookup. A user.UnknownUserError means the account does not
//     exist: return an error wrapping ErrNoAccount. Any other error is
//     returned as it is.
//   - Parse Uid and Gid as 32-bit unsigned numbers (strconv.ParseUint).
//   - Comment is the user's Name field (the GECOS comment).
func (SystemAccounts) Lookup(name string) (Account, error) {
	u, err := user.Lookup(name)
	if err != nil {
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

// Create implements Accounts. It never runs useradd in its own process:
// the provisioning service cannot write /etc. It asks systemd to run
// useradd once, in a transient unit with UseraddProperties, and waits.
// Exact argv, in this order, with each UseraddProperties entry as its own
// "-p", "<entry>" pair:
//
//	/usr/bin/systemd-run --wait --collect --quiet --pipe --service-type=exec
//	    --unit=waystone-useradd-<name>.service --setenv=LC_ALL=C
//	    -p <entry> ...
//	    -- /usr/sbin/useradd --system --user-group --no-create-home
//	    --home-dir /nonexistent --shell /usr/sbin/nologin --comment <comment> -- <name>
//
// Rules:
//   - First check that name has exactly the shape ServerUser produces
//     ("ws-" and 12 lowercase hex digits). Anything else: an error wrapping
//     ErrInvalidRequest, and nothing runs.
//   - --wait and --pipe make systemd-run return useradd's exit status and
//     pass on its stderr, so a failure says why. --collect unloads the
//     transient unit even when it failed.
//   - Return the Runner's error wrapped with %w.
func (a SystemAccounts) Create(ctx context.Context, name, comment string) error {
	if len(name) != 15 || name[:3] != "ws-" {
		return fmt.Errorf("%w: invalid server username", ErrInvalidRequest)
	}
	for _, ch := range name[3:] {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return fmt.Errorf("%w: invalid server username", ErrInvalidRequest)
		}
	}
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
