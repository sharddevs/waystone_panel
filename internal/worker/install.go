// SPDX-License-Identifier: AGPL-3.0-only

package worker

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// MaxInstallBytes is the largest file install-file accepts (256 MiB). The
// helper already checks the size of the file it is handed, but we check
// again while copying because only what really arrives on stdin counts.
const MaxInstallBytes = 256 << 20

// ErrMismatch means the content didn't have the requested SHA-256, so
// nothing was installed.
var ErrMismatch = errors.New("content does not match sha256")

// InstallResult is what install-file prints as JSON on stdout.
type InstallResult struct {
	Bytes int64 `json:"bytes"`
}

// ParseSHA256 parses a hash written as exactly 64 lowercase hex digits.
// Anything else, uppercase included, is an error wrapping ErrInvalid.
func ParseSHA256(s string) ([sha256.Size]byte, error) {
	var sum [sha256.Size]byte

	if len(s) != sha256.Size*2 {
		return sum, fmt.Errorf("%w: sha256 must be %d hex characters",
			ErrInvalid, sha256.Size*2)
	}

	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return sum, fmt.Errorf("%w: sha256 must use lowercase hexadecimal",
				ErrInvalid)
		}
	}

	decoded, err := hex.DecodeString(s)
	if err != nil {
		return sum, fmt.Errorf("%w: invalid sha256: %v", ErrInvalid, err)
	}

	copy(sum[:], decoded)
	return sum, nil
}

// InstallFile replaces dir/name with everything read from r, as long as
// it is at most MaxInstallBytes and has the SHA-256 want. It returns the
// number of bytes installed. Unlike WriteFile it doesn't care what dir/name
// held before.
//
// r is only ever read as a stream. systemd-run --pipe may give us a pipe
// instead of the original file, so we can't count on Seek or Stat.
func InstallFile(dir, name string, r io.Reader, want [sha256.Size]byte) (int64, error) {
	return installFile(dir, name, r, want, MaxInstallBytes)
}

// installFile is InstallFile with the size limit as a parameter, so tests
// can check the boundary without writing 256 MiB.
func installFile(dir, name string, r io.Reader, want [sha256.Size]byte, limit int64) (int64, error) {
	if err := ValidName(name); err != nil {
		return 0, err
	}

	// Same directory as the target, so the rename below is atomic.
	tmp, err := os.CreateTemp(dir, "."+name+".tmp-*")
	if err != nil {
		return 0, err
	}

	// defer runs on every return path, so a failed install never leaves a
	// temp file behind.
	renamed := false
	defer func() {
		tmp.Close()
		if !renamed {
			os.Remove(tmp.Name())
		}
	}()

	// Write to the file and the hasher at the same time, so the hash covers
	// exactly the bytes that end up on disk.
	hasher := sha256.New()
	writer := io.MultiWriter(tmp, hasher)

	// Reading limit+1 bytes is how we tell "too big" apart from "exactly
	// the limit", instead of cutting the file off without noticing.
	n, err := io.Copy(writer, io.LimitReader(r, limit+1))
	if err != nil {
		return 0, err
	}

	if n > limit {
		return 0, fmt.Errorf("%w: exceeds size limit", ErrInvalid)
	}

	var actual [sha256.Size]byte
	copy(actual[:], hasher.Sum(nil))

	if actual != want {
		return 0, ErrMismatch
	}

	// Set the mode ourselves so the umask doesn't decide it.
	if err := tmp.Chmod(FileMode); err != nil {
		return 0, err
	}
	if err := tmp.Sync(); err != nil {
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		return 0, err
	}

	// Rename replaces a symlink at dir/name instead of following it.
	if err := os.Rename(tmp.Name(), filepath.Join(dir, name)); err != nil {
		return 0, err
	}
	renamed = true

	// The rename is only durable once the directory is synced.
	if err := syncDir(dir); err != nil {
		return 0, err
	}

	return n, nil
}
