// SPDX-License-Identifier: AGPL-3.0-only

package worker

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// ErrRoot means the worker was started as root, which it refuses.
var ErrRoot = errors.New("waystone-worker must not run as root")

// NotRoot returns ErrRoot if euid is 0. The worker has to run as the
// server's own user so that even a planted symlink only reaches files that
// user could already get to.
func NotRoot(euid int) error {
	if euid == 0 {
		return ErrRoot
	}

	return nil
}

// ReadResult is what read-file prints on stdout as JSON. encoding/json
// turns a []byte into base64 on its own, so Content needs no extra work.
type ReadResult struct {
	Exists  bool   `json:"exists"`
	Content []byte `json:"content"`
}

// WriteResult is what write-file prints on stdout as JSON. A changed file
// is a normal answer, not a failure, so it gets its own outcome instead of
// an error.
type WriteResult struct {
	Outcome string `json:"outcome"`
}

// Values for WriteResult.Outcome.
const (
	OutcomeWritten = "written"
	OutcomeChanged = "changed"
)

// ReadFile reads dir/name. A missing file is not an error, it just comes
// back with Exists false. Symlinks and other non-regular files are refused
// with ErrUnsupported, and so is anything over MaxFileBytes.
func ReadFile(dir, name string) (ReadResult, error) {
	if err := ValidName(name); err != nil {
		return ReadResult{}, err
	}

	destination := filepath.Join(dir, name)

	info, err := os.Lstat(destination)
	if errors.Is(err, os.ErrNotExist) {
		return ReadResult{Exists: false}, nil
	}

	if err != nil {
		return ReadResult{}, err
	}

	if !info.Mode().IsRegular() {
		return ReadResult{}, fmt.Errorf("%w: not a regular file", ErrUnsupported)
	}

	// If a FIFO gets swapped in right here, Open can block. The systemd
	// unit's RuntimeMaxSec= stops the worker if that happens.
	file, err := os.Open(destination)
	if err != nil {
		return ReadResult{}, err
	}
	defer file.Close()

	// Make sure we opened the same file Lstat saw, not something swapped in.
	openedInfo, err := file.Stat()
	if err != nil {
		return ReadResult{}, err
	}

	if !os.SameFile(info, openedInfo) {
		return ReadResult{}, fmt.Errorf("%w: file changed while opening", ErrChanged)
	}

	content, err := io.ReadAll(io.LimitReader(file, MaxFileBytes+1))
	if err != nil {
		return ReadResult{}, err
	}

	if len(content) > MaxFileBytes {
		return ReadResult{}, fmt.Errorf("%w: file exceeds size limit", ErrUnsupported)
	}

	return ReadResult{
		Exists:  true,
		Content: content,
	}, nil
}

// WriteFile replaces dir/name with data if the file still matches expect.
// Errors from ReplaceIfMatches are returned as they are, so callers can
// check for ErrChanged with errors.Is.
func WriteFile(dir, name string, data []byte, expect Expect) error {
	if err := ValidName(name); err != nil {
		return err
	}

	if len(data) > MaxFileBytes {
		return fmt.Errorf("%w: content exceeds size limit", ErrInvalid)
	}

	return ReplaceIfMatches(dir, name, data, expect)
}
