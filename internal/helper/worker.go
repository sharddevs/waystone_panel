// SPDX-License-Identifier: AGPL-3.0-only

package helper

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/sharddevs/waystone_panel/internal/ids"
	"github.com/sharddevs/waystone_panel/internal/worker"
)

// Where the worker binary is installed, and its limits.
const (
	WorkerPath = "/usr/local/libexec/waystone-worker"
	// WorkerRuntimeMaxSec lets systemd kill a hung worker. It stays below
	// ConnTimeout so the worker is stopped before the helper gives up on the
	// request, and the agent is told it failed.
	WorkerRuntimeMaxSec = 50
)

// WorkerProperties is the worker's sandbox, on top of the per-server
// properties WorkerCommand adds. It's stricter than the game's own unit: no
// network at all (its input comes on stdin) and no writable and executable
// memory. This list is a security boundary, so it should only get stricter.
var WorkerProperties = []string{
	"ProtectSystem=strict",
	"ProtectHome=yes",
	"PrivateTmp=yes",
	"PrivateDevices=yes",
	"PrivateNetwork=yes",
	"RestrictAddressFamilies=AF_UNIX",
	"NoNewPrivileges=yes",
	"CapabilityBoundingSet=",
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
	"UMask=0027",
}

// ErrNotProvisioned means the server has no account on this machine yet.
// The Router answers it with ERROR_CODE_NOT_FOUND.
var ErrNotProvisioned = errors.New("server is not provisioned on this machine")

// ValidateServerUser returns nil if name has exactly the shape ServerUser
// produces: "ws-" and 12 lowercase hex digits. Errors wrap
// ErrInvalidRequest.
func ValidateServerUser(name string) error {
	if len(name) != 15 || !strings.HasPrefix(name, "ws-") {
		return fmt.Errorf("%w: invalid server username", ErrInvalidRequest)
	}

	for _, c := range name[3:] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return fmt.Errorf("%w: invalid server username", ErrInvalidRequest)
		}
	}

	return nil
}

// WorkerCommand builds the systemd-run command line that runs the worker
// once, as user, inside server serverID's directory.
//
// Every input is validated first (errors wrap ErrInvalidRequest). That is
// what keeps the worker from ever running as root or in another server's
// directory.
func WorkerCommand(serverID, operationID, user string, args ...string) ([]string, error) {
	if err := ids.Check(serverID); err != nil {
		return nil, fmt.Errorf("%w: invalid server ID: %v", ErrInvalidRequest, err)
	}

	if err := ids.Check(operationID); err != nil {
		return nil, fmt.Errorf("%w: invalid operation ID: %v", ErrInvalidRequest, err)
	}

	if err := ValidateServerUser(user); err != nil {
		return nil, err
	}

	argv := []string{
		"/usr/bin/systemd-run",
		"--wait",
		"--collect",
		"--quiet",
		"--pipe",
		"--service-type=exec",
		"--unit=waystone-op-" + operationID + ".service",

		"-p", "User=" + user,
		"-p", "Group=" + user,
		// Same StateDirectory= as the game's unit, so systemd creates the
		// directory, owned by user, even if the game has never started.
		"-p", "StateDirectory=waystone/servers/" + serverID,
		"-p", "StateDirectoryMode=0750",
		"-p", "WorkingDirectory=/var/lib/waystone/servers/" + serverID,
		"-p", fmt.Sprintf("RuntimeMaxSec=%d", WorkerRuntimeMaxSec),
	}

	for _, property := range WorkerProperties {
		argv = append(argv, "-p", property)
	}

	argv = append(argv, "--", WorkerPath)
	argv = append(argv, args...)

	return argv, nil
}

// validateAccount checks that account is the one Waystone made for
// serverID: the right comment and a system UID (1 to 999, never root).
// Anything else is ErrConflict.
func validateAccount(account Account, serverID string) error {
	if account.Comment != AccountComment(serverID) {
		return fmt.Errorf(
			"%w: account ownership comment mismatch",
			ErrConflict,
		)
	}

	if account.UID == 0 || account.UID > 999 {
		return fmt.Errorf(
			"%w: account UID outside system range",
			ErrConflict,
		)
	}

	return nil
}

// ServerAccount returns the account provisioning made for serverID. A
// missing account wraps ErrNotProvisioned, and one Waystone didn't make
// wraps ErrConflict.
func ServerAccount(a Accounts, serverID string) (Account, error) {
	username := ServerUser(serverID)

	account, err := a.Lookup(username)
	if errors.Is(err, ErrNoAccount) {
		return Account{}, fmt.Errorf(
			"%w: account %q does not exist",
			ErrNotProvisioned,
			username,
		)
	}
	if err != nil {
		return Account{}, fmt.Errorf("lookup account: %w", err)
	}

	if err := validateAccount(account, serverID); err != nil {
		return Account{}, err
	}

	return account, nil
}

// Worker runs waystone-worker for the provisioning service, so file access
// happens as the server's own user and never as root.
type Worker struct {
	Accounts Accounts
	Runner   InputRunner
}

// decodeWorkerResult decodes exactly one JSON value from the worker's
// output into result. Unknown fields and trailing data are errors: the
// worker is ours, so odd output means something is badly wrong.
func decodeWorkerResult(output []byte, result any) error {
	decoder := json.NewDecoder(bytes.NewReader(output))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(result); err != nil {
		return fmt.Errorf("decode worker result: %w", err)
	}

	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("worker result contains trailing data or invalid JSON: %v", err)
	}

	return nil
}

// ReadFile runs "read-file <name>" for cmd.ServerID and returns what the
// worker found. If the server has no valid account the worker never runs.
func (w Worker) ReadFile(ctx context.Context, cmd Command) (worker.ReadResult, error) {
	account, err := ServerAccount(w.Accounts, cmd.ServerID)
	if err != nil {
		return worker.ReadResult{}, err
	}

	argv, err := WorkerCommand(
		cmd.ServerID,
		cmd.OperationID,
		account.Name,
		"read-file",
		cmd.File.Name,
	)
	if err != nil {
		return worker.ReadResult{}, err
	}

	output, err := w.Runner.RunInput(ctx, argv, nil)
	if err != nil {
		return worker.ReadResult{}, fmt.Errorf("read file: %w", err)
	}

	var result worker.ReadResult
	if err := decodeWorkerResult(output, &result); err != nil {
		return worker.ReadResult{}, fmt.Errorf("decode read result: %w", err)
	}

	// Check the result too, even though our own worker produced it.
	if len(result.Content) > worker.MaxFileBytes {
		return worker.ReadResult{}, fmt.Errorf(
			"read result exceeds maximum file size",
		)
	}

	if !result.Exists && len(result.Content) != 0 {
		return worker.ReadResult{}, fmt.Errorf(
			"missing file returned nonempty content",
		)
	}

	return result, nil
}

// WriteFile runs "write-file <name> <expect>" for cmd.ServerID, with the
// content on stdin. If the file no longer matches the expectation it
// returns an error wrapping ErrChanged.
func (w Worker) WriteFile(ctx context.Context, cmd Command) error {
	account, err := ServerAccount(w.Accounts, cmd.ServerID)
	if err != nil {
		return err
	}

	argv, err := WorkerCommand(
		cmd.ServerID,
		cmd.OperationID,
		account.Name,
		"write-file",
		cmd.File.Name,
		cmd.File.Expect.String(),
	)
	if err != nil {
		return err
	}

	output, err := w.Runner.RunInput(
		ctx,
		argv,
		strings.NewReader(cmd.File.Content),
	)
	if err != nil {
		return fmt.Errorf("write file: %w", err)
	}

	var result worker.WriteResult
	if err := decodeWorkerResult(output, &result); err != nil {
		return fmt.Errorf("decode write result: %w", err)
	}

	switch result.Outcome {
	case worker.OutcomeWritten:
		return nil

	case worker.OutcomeChanged:
		return fmt.Errorf(
			"%w: file %q changed since it was last read",
			ErrChanged,
			cmd.File.Name,
		)

	default:
		return fmt.Errorf(
			"unexpected write outcome %q",
			result.Outcome,
		)
	}
}
