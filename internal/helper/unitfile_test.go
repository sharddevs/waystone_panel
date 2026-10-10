package helper

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func unitContent(extra string) []byte {
	return []byte(UnitMarker + "[Unit]\nDescription=test " + extra + "\n")
}

// onlyUnit fails the test if dir holds anything besides the unit file,
// such as a leftover temporary file.
func onlyUnit(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != testUnit {
			t.Errorf("unexpected file %q left in the unit directory", e.Name())
		}
	}
}

func TestUnitFilesWriteNewAndRepeat(t *testing.T) {
	dir := t.TempDir()
	u := UnitFiles{Dir: dir}
	path := filepath.Join(dir, testUnit)

	if err := u.Check(testServerID); err != nil {
		t.Fatalf("Check on a missing file: %v", err)
	}
	changed, err := u.Write(testServerID, unitContent("a"))
	if err != nil || !changed {
		t.Fatalf("first Write = %v, %v; want true, nil", changed, err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(unitContent("a")) {
		t.Fatalf("file holds %q (%v)", got, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("mode %v, want 0644 (systemd and admins must be able to read it)", info.Mode().Perm())
	}

	changed, err = u.Write(testServerID, unitContent("a"))
	if err != nil || changed {
		t.Errorf("same content again = %v, %v; want false, nil", changed, err)
	}
	changed, err = u.Write(testServerID, unitContent("b"))
	if err != nil || !changed {
		t.Errorf("new content = %v, %v; want true, nil", changed, err)
	}
	if got, _ := os.ReadFile(path); string(got) != string(unitContent("b")) {
		t.Errorf("file holds %q after rewrite", got)
	}
	if err := u.Check(testServerID); err != nil {
		t.Errorf("Check on our own file: %v", err)
	}
	onlyUnit(t, dir)
}

func TestUnitFilesRefuseHandWrittenUnit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, testUnit)
	hand := []byte("[Unit]\nDescription=written by hand\n")
	if err := os.WriteFile(path, hand, 0o644); err != nil {
		t.Fatal(err)
	}
	u := UnitFiles{Dir: dir}
	if err := u.Check(testServerID); !errors.Is(err, ErrConflict) {
		t.Errorf("Check = %v, want ErrConflict", err)
	}
	if _, err := u.Write(testServerID, unitContent("a")); !errors.Is(err, ErrConflict) {
		t.Errorf("Write = %v, want ErrConflict", err)
	}
	if got, _ := os.ReadFile(path); string(got) != string(hand) {
		t.Errorf("hand-written unit was changed to %q", got)
	}
	onlyUnit(t, dir)
}

func TestUnitFilesRefuseSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(target, unitContent("target"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, testUnit)); err != nil {
		t.Fatal(err)
	}
	u := UnitFiles{Dir: dir}
	if err := u.Check(testServerID); !errors.Is(err, ErrConflict) {
		t.Errorf("Check = %v, want ErrConflict", err)
	}
	if _, err := u.Write(testServerID, unitContent("a")); !errors.Is(err, ErrConflict) {
		t.Errorf("Write = %v, want ErrConflict", err)
	}
	if got, _ := os.ReadFile(target); string(got) != string(unitContent("target")) {
		t.Errorf("symlink target was changed to %q", got)
	}
}
