package helper

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// UnitFiles reads and writes server unit files in Dir
// (DefaultUnitDir in production, a temporary directory in tests).
type UnitFiles struct {
	Dir string
}

func (u UnitFiles) path(serverID string) string {
	return filepath.Join(u.Dir, UnitName(serverID))
}

// Check implements Units. A missing file is fine. Anything that isn't a
// regular file starting with UnitMarker was not written by us, so it is
// ErrConflict.
func (u UnitFiles) Check(serverID string) error {
	// Lstat doesn't follow symlinks, so a symlink shows up as one and is
	// refused below instead of us reading whatever it points at.
	info, err := os.Lstat(u.path(serverID))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: unit path is not a regular file", ErrConflict)
	}
	content, err := os.ReadFile(u.path(serverID))
	if err != nil {
		return err
	}
	if !bytes.HasPrefix(content, []byte(UnitMarker)) {
		return fmt.Errorf("%w: unit is not managed by Waystone", ErrConflict)
	}
	return nil
}

// Write implements Units. If the file already holds exactly content it
// changes nothing. Otherwise it writes a temp file and renames it into
// place, so systemd never sees half a unit file.
func (u UnitFiles) Write(serverID string, content []byte) (bool, error) {
	if err := u.Check(serverID); err != nil {
		return false, err
	}
	existing, err := os.ReadFile(u.path(serverID))
	if err == nil {
		if bytes.Equal(existing, content) {
			return false, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	// The temp file must be in the same directory (same file system),
	// otherwise the rename isn't atomic.
	tmp, err := os.CreateTemp(u.Dir, ".waystone-unit-*")
	if err != nil {
		return false, err
	}
	// Deferred calls run on every return. After a successful rename the
	// Remove just fails quietly, since the temp name is gone.
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if _, err := tmp.Write(content); err != nil {
		return false, err
	}
	// The helper runs with umask 0077, so set the mode ourselves.
	if err := tmp.Chmod(0644); err != nil {
		return false, err
	}
	if err := tmp.Sync(); err != nil {
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if err := os.Rename(tmp.Name(), u.path(serverID)); err != nil {
		return false, err
	}

	return true, nil
}
