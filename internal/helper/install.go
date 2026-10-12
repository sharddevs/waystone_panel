// SPDX-License-Identifier: AGPL-3.0-only

package helper

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/sharddevs/waystone_panel/internal/worker"
)

// InstallFile runs "install-file <name> <sha256>" for cmd.ServerID as the
// server's own user, with cmd.Input as the worker's stdin.
func (w Worker) InstallFile(ctx context.Context, cmd Command) error {
	if cmd.Input == nil {
		return fmt.Errorf("%w: missing installation input", ErrInvalidRequest)
	}

	// No valid Waystone account for this server means the worker never runs.
	account, err := ServerAccount(w.Accounts, cmd.ServerID)
	if err != nil {
		return err
	}

	hash := hex.EncodeToString(cmd.File.ContentSHA256[:])

	argv, err := WorkerCommand(
		cmd.ServerID,
		cmd.OperationID,
		account.Name,
		"install-file",
		cmd.File.Name,
		hash,
	)
	if err != nil {
		return err
	}

	// Pass the *os.File itself, not a wrapper around it. When Cmd.Stdin is
	// an *os.File, os/exec hands that descriptor straight to the child; any
	// other io.Reader would make Go copy everything through a pipe.
	output, err := w.Runner.RunInput(ctx, argv, cmd.Input)
	if err != nil {
		return fmt.Errorf("install file: %w", err)
	}

	var result worker.InstallResult
	if err := decodeWorkerResult(output, &result); err != nil {
		return fmt.Errorf("decode install result: %w", err)
	}

	// The worker is ours, but a size outside the limit still means
	// something went wrong, so don't report success.
	if result.Bytes < 0 || result.Bytes > MaxInstallBytes {
		return fmt.Errorf("invalid install result size: %d", result.Bytes)
	}

	return nil
}
