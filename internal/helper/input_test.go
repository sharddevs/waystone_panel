// SPDX-License-Identifier: AGPL-3.0-only

package helper

import (
	"errors"
	"os"
	"testing"

	helperv1 "github.com/sharddevs/waystone_panel/gen/waystone/helper/v1"
)

// sizedFile returns an open file of the given size. Truncate makes a sparse
// file, so even a 256 MiB one takes no real disk space.
func sizedFile(t *testing.T, size int64) *os.File {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "input")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestCheckInputInstall(t *testing.T) {
	install := helperv1.Action_ACTION_INSTALL_FILE
	for _, size := range []int64{0, 3, MaxInstallBytes} {
		if err := CheckInput(install, sizedFile(t, size)); err != nil {
			t.Errorf("regular file of %d bytes: %v", size, err)
		}
	}

	dir, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()

	// Only a regular file has a size we can check before the worker runs,
	// so a pipe or a directory is refused like a missing descriptor.
	for name, f := range map[string]*os.File{
		"no descriptor": nil,
		"one byte over": sizedFile(t, MaxInstallBytes+1),
		"directory":     dir,
		"pipe":          r,
	} {
		if err := CheckInput(install, f); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: %v, want ErrInvalidRequest", name, err)
		}
	}
}

func TestCheckInputOtherActions(t *testing.T) {
	for _, action := range []helperv1.Action{
		helperv1.Action_ACTION_START,
		helperv1.Action_ACTION_STOP,
		helperv1.Action_ACTION_STATUS,
		helperv1.Action_ACTION_PROVISION,
		helperv1.Action_ACTION_READ_FILE,
		helperv1.Action_ACTION_WRITE_FILE,
	} {
		if err := CheckInput(action, nil); err != nil {
			t.Errorf("%v without a descriptor: %v", action, err)
		}
		if err := CheckInput(action, sizedFile(t, 3)); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%v with a descriptor: %v, want ErrInvalidRequest", action, err)
		}
	}
}

// CheckInput only calls Stat. It must not read the file or close it.
func TestCheckInputLeavesTheFileAlone(t *testing.T) {
	f := sizedFile(t, 3)
	if err := CheckInput(helperv1.Action_ACTION_INSTALL_FILE, f); err != nil {
		t.Fatal(err)
	}
	// Seek(0, 1) (1 is io.SeekCurrent) reports the offset without moving
	// it, and fails if the file was closed.
	if pos, err := f.Seek(0, 1); err != nil || pos != 0 {
		t.Errorf("offset after CheckInput = %d, %v; want 0, still open", pos, err)
	}
}
