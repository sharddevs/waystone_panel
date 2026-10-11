// SPDX-License-Identifier: AGPL-3.0-only

package worker

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
)

// FileMode is the mode of every file the worker writes: the server's user
// reads and writes, its group reads, nobody else.
const FileMode os.FileMode = 0o640

// WriteFileAtomic replaces dir/name with data, whatever is there now. It is
// the building block of ReplaceIfMatches; the worker's verbs never write
// unconditionally.
//   - ValidName(name) first. Otherwise its error, and nothing is created.
//   - Create the temporary file with os.CreateTemp in dir itself (same
//     file system, so the rename is atomic), named "." + name + ".tmp-*".
//   - Write data, Chmod to FileMode (umask must not decide the mode), Sync,
//     Close, then os.Rename onto dir/name. A symlink at dir/name is
//     replaced by the new file, never followed.
//   - Then sync the directory (syncDir) so the rename itself is durable.
//   - On any error before the rename succeeds, remove the temporary file.
//     No ".tmp-" file may be left behind.
func WriteFileAtomic(dir, name string, data []byte) error {
	return writeFileAtomicChecked(dir, name, data, nil)
}

func writeFileAtomicChecked(dir, name string, data []byte, check func() error) error {
	if err := ValidName(name); err != nil {
		return err
	}

	temp, err := os.CreateTemp(dir, "."+name+".tmp-*")
	if err != nil {
		return err
	}

	tempName := temp.Name()
	defer os.Remove(tempName)

	n, err := temp.Write(data)
	if err != nil {
		temp.Close()
		return err
	}

	if n != len(data) {
		temp.Close()
		return io.ErrShortWrite
	}

	if err := temp.Chmod(FileMode); err != nil {
		temp.Close()
		return err
	}

	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}

	if err := temp.Close(); err != nil {
		return err
	}

	destination := filepath.Join(dir, name)

	if check != nil {
		if err := check(); err != nil {
			return err
		}
	}

	if err := os.Rename(tempName, destination); err != nil {
		return err
	}

	return syncDir(dir)
}

// ReplaceIfMatches is WriteFileAtomic for read-modify-write: it replaces
// dir/name only if the file is still what the caller read.
//
// Immediately before the rename, look at dir/name again with os.Lstat:
//   - It doesn't exist: fine only if expect.Absent.
//   - It exists but isn't a regular file (a symlink, a directory, ...):
//     an error wrapping ErrUnsupported. It is never followed or replaced.
//   - A regular file and expect.Absent: ErrChanged.
//   - A regular file otherwise: open it, Stat the open file and require
//     os.SameFile with the Lstat result (else ErrChanged), then hash at most
//     MaxFileBytes+1 bytes of it. More than MaxFileBytes, or a different
//     SHA-256 than expect.SHA256: ErrChanged.
//
// On any refusal, remove the temporary file and leave dir/name as it is.
// Everything else (name check, temp file, mode, sync, cleanup) exactly as
// WriteFileAtomic. Share the code; don't copy it.
//
// Comparing content, not timestamps, is deliberate: an edit that was undone
// leaves nothing to lose. A tiny window between the check and the rename
// remains. No lock can close it against an administrator's editor; the
// check turns the common case into an error instead of a silent overwrite.
func ReplaceIfMatches(dir, name string, data []byte, expect Expect) error {
	return writeFileAtomicChecked(dir, name, data, func() error {
		destination := filepath.Join(dir, name)

		info, err := os.Lstat(destination)
		if errors.Is(err, os.ErrNotExist) {
			if expect.Absent {
				return nil
			}

			return fmt.Errorf("%w: file no longer exists", ErrChanged)
		}

		if err != nil {
			return err
		}

		if !info.Mode().IsRegular() {
			return fmt.Errorf("%w: destination is not a regular file", ErrUnsupported)
		}

		if expect.Absent {
			return fmt.Errorf("%w: file already exists", ErrChanged)
		}

		file, err := os.Open(destination)
		if err != nil {
			return err
		}
		defer file.Close()

		openedInfo, err := file.Stat()
		if err != nil {
			return err
		}

		if !os.SameFile(info, openedInfo) {
			return fmt.Errorf("%w: file changed while opening", ErrChanged)
		}

		hasher := sha256.New()

		n, err := io.Copy(hasher, io.LimitReader(file, MaxFileBytes+1))
		if err != nil {
			return err
		}

		if n > MaxFileBytes {
			return fmt.Errorf("%w: existing file exceeds size limit", ErrChanged)
		}

		var actualHash [sha256.Size]byte
		copy(actualHash[:], hasher.Sum(nil))

		if actualHash != expect.SHA256 {
			return fmt.Errorf("%w: file contents changed", ErrChanged)
		}

		return nil
	})
}

// syncDir fsyncs a directory so a rename inside it survives a crash. Open
// the directory, Sync, Close. Windows can't sync a directory: there, return
// nil without trying (runtime.GOOS).
func syncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}

	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()

	return f.Sync()
}
