// SPDX-License-Identifier: AGPL-3.0-only

package worker

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// ErrRoot means the worker was started as root. It never does any work then.
var ErrRoot = errors.New("waystone-worker must not run as root")

// NotRoot returns ErrRoot if euid is 0, otherwise nil.
func NotRoot(euid int) error {
	if euid == 0 {
		return ErrRoot
	}

	return nil
}

// ReadResult is what read-file prints on stdout, as JSON. Content is
// base64 in JSON (encoding/json does that for []byte).
type ReadResult struct {
	Exists  bool   `json:"exists"`
	Content []byte `json:"content"`
}

// WriteResult is what write-file prints on stdout, as JSON. A changed file
// is a normal outcome, not a failure of the worker: the helper needs to
// tell it apart from errors.
type WriteResult struct {
	Outcome string `json:"outcome"`
}

// WriteResult outcomes.
const (
	OutcomeWritten = "written"
	OutcomeChanged = "changed"
)

// ReadFile reads dir/name.
//  1. ValidName(name). On error, return it.
//  2. os.Lstat. Doesn't exist: ReadResult{Exists: false} and no error.
//     Not a regular file: an error wrapping ErrUnsupported (never followed).
//  3. Open it, Stat the open file, and require os.SameFile with the Lstat
//     result. Otherwise it was swapped in between: ErrChanged.
//  4. Read at most MaxFileBytes+1 bytes. More than MaxFileBytes:
//     ErrUnsupported. Close the file.
//  5. ReadResult{Exists: true, Content: what was read}. An empty existing
//     file has Exists true and empty Content.
//
// If a FIFO is swapped in between steps 2 and 3, the open can block; the
// unit's RuntimeMaxSec= ends that.
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

	file, err := os.Open(destination)
	if err != nil {
		return ReadResult{}, err
	}
	defer file.Close()

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

// WriteFile replaces dir/name with data if it still matches expect.
//   - ValidName(name), and len(data) at most MaxFileBytes. Otherwise an
//     error wrapping ErrInvalid, and nothing is touched.
//   - Then ReplaceIfMatches. Its errors are returned as they are, so
//     ErrChanged stays detectable.
func WriteFile(dir, name string, data []byte, expect Expect) error {
	if err := ValidName(name); err != nil {
		return err
	}

	if len(data) > MaxFileBytes {
		return fmt.Errorf("%w: content exceeds size limit", ErrInvalid)
	}

	return ReplaceIfMatches(dir, name, data, expect)
}
