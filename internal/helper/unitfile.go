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

// path is where the server's unit file lives.
func (u UnitFiles) path(serverID string) string {
	return filepath.Join(u.Dir, UnitName(serverID))
}

// Check implements Units.
//   - Use os.Lstat, which does not follow symlinks. A missing file is fine
//     (return nil).
//   - Anything that is not a regular file (a symlink, a directory) is
//     ErrConflict.
//   - A regular file that does not start with UnitMarker is ErrConflict:
//     someone wrote it by hand, like your 3a copy of mclab.service.
func (u UnitFiles) Check(serverID string) error {
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

// Write implements Units.
//   - Run the same checks as Check first.
//   - If the file already holds exactly content, change nothing and
//     return false.
//   - Otherwise write atomically, so systemd never reads half a file:
//     os.CreateTemp in Dir (same file system, so rename is atomic), write,
//     Chmod 0644 (the helper's umask is 0077, so the mode must be set
//     explicitly), Sync, Close, then os.Rename over the unit's path.
//   - On any error remove the temporary file. Never leave one behind.
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
	tmp, err := os.CreateTemp(u.Dir, ".waystone-unit-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if _, err := tmp.Write(content); err != nil {
		return false, err
	}
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
