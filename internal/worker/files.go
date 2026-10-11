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

// FileMode is the mode of every file the worker writes: owner read/write,
// group read, nothing for others.
const FileMode os.FileMode = 0o640

// WriteFileAtomic replaces dir/name with data no matter what is there now.
// The worker's verbs don't call it directly; they go through ReplaceIfMatches.
func WriteFileAtomic(dir, name string, data []byte) error {
	return writeFileAtomicChecked(dir, name, data, nil)
}

// writeFileAtomicChecked writes data to a temp file next to dir/name and
// renames it into place. If check is not nil it runs right before the rename,
// so the caller gets one last chance to say no.
func writeFileAtomicChecked(dir, name string, data []byte, check func() error) error {
	if err := ValidName(name); err != nil {
		return err
	}

	// The temp file has to live in the same directory, otherwise the
	// rename isn't atomic (it could cross file systems).
	temp, err := os.CreateTemp(dir, "."+name+".tmp-*")
	if err != nil {
		return err
	}

	tempName := temp.Name()
	renamed := false

	// defer runs on every return, so a failed write never leaves a
	// .tmp file behind. After the rename there's nothing to clean up.
	defer func() {
		if !renamed {
			_ = os.Remove(tempName)
		}
	}()

	n, err := temp.Write(data)
	if err != nil {
		temp.Close()
		return err
	}

	if n != len(data) {
		temp.Close()
		return io.ErrShortWrite
	}

	// Set the mode ourselves so the umask doesn't decide it.
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

	// Rename replaces a symlink at destination instead of following it.
	if err := os.Rename(tempName, destination); err != nil {
		return err
	}

	renamed = true
	// The rename is only durable once the directory itself is synced.
	return syncDir(dir)
}

// ReplaceIfMatches is WriteFileAtomic for read-modify-write: it replaces
// dir/name only if the file is still what the caller read (see Expect).
// Otherwise it returns ErrChanged, or ErrUnsupported for a symlink or other
// non-regular file, and leaves dir/name alone.
//
// We compare content, not timestamps, so an edit that was undone doesn't
// count as a change. There's still a tiny window between the check and the
// rename. No lock could close it against an admin's editor anyway; the check
// just turns the common case into an error instead of a silent overwrite.
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

		// Lstat doesn't follow symlinks, so a link the game server planted
		// shows up here as non-regular and is refused, never replaced.
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

		// Something could have been swapped in between Lstat and Open, so
		// make sure the file we opened is the one we looked at.
		openedInfo, err := file.Stat()
		if err != nil {
			return err
		}

		if !os.SameFile(info, openedInfo) {
			return fmt.Errorf("%w: file changed while opening", ErrChanged)
		}

		hasher := sha256.New()

		// Reading one byte past the limit is how we tell "too big" apart
		// from "exactly MaxFileBytes". A file that big can't be one the
		// caller read, so it counts as changed.
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

// syncDir fsyncs a directory so a rename inside it survives a crash.
// Windows can't sync a directory, so there it does nothing.
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
