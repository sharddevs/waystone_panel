// SPDX-License-Identifier: AGPL-3.0-only

package worker

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNotRoot(t *testing.T) {
	if err := NotRoot(0); !errors.Is(err, ErrRoot) {
		t.Errorf("NotRoot(0) = %v, want ErrRoot", err)
	}
	for _, euid := range []int{1, 994, 65534} {
		if err := NotRoot(euid); err != nil {
			t.Errorf("NotRoot(%d) = %v", euid, err)
		}
	}
}

func TestReadFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "server.properties"), "motd=hi\n")
	writeFile(t, filepath.Join(dir, "empty.txt"), "")

	got, err := ReadFile(dir, "server.properties")
	if err != nil || !got.Exists || string(got.Content) != "motd=hi\n" {
		t.Errorf("existing file: %+v, %v", got, err)
	}
	got, err = ReadFile(dir, "empty.txt")
	if err != nil || !got.Exists || len(got.Content) != 0 {
		t.Errorf("empty file: %+v, %v; want exists with no content", got, err)
	}
	got, err = ReadFile(dir, "missing.txt")
	if err != nil || got.Exists || len(got.Content) != 0 {
		t.Errorf("missing file: %+v, %v; want not exists, no error", got, err)
	}
}

func TestReadFileSizeLimitIsInclusive(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "max"), strings.Repeat("x", MaxFileBytes))
	writeFile(t, filepath.Join(dir, "over"), strings.Repeat("x", MaxFileBytes+1))
	if got, err := ReadFile(dir, "max"); err != nil || len(got.Content) != MaxFileBytes {
		t.Errorf("file of exactly MaxFileBytes: %d bytes, %v", len(got.Content), err)
	}
	if _, err := ReadFile(dir, "over"); !errors.Is(err, ErrUnsupported) {
		t.Errorf("file over MaxFileBytes: err = %v, want ErrUnsupported", err)
	}
}

// A game server can plant links and directories. The worker never reads
// through them.
func TestReadFileRefuses(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(t.TempDir(), "secret")
	writeFile(t, secret, "not yours")
	if err := os.Symlink(secret, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	tests := map[string]error{
		"link":       ErrUnsupported,
		"sub":        ErrUnsupported,
		"../secret":  ErrInvalid,
		".hidden":    ErrInvalid,
		"":           ErrInvalid,
		"a/b":        ErrInvalid,
		"/etc/hosts": ErrInvalid,
	}
	for name, want := range tests {
		got, err := ReadFile(dir, name)
		if !errors.Is(err, want) {
			t.Errorf("ReadFile(%q) err = %v, want %v", name, err, want)
		}
		if len(got.Content) != 0 {
			t.Errorf("ReadFile(%q) returned content %q", name, got.Content)
		}
	}
}

// The read-modify-write cycle the profile code runs, at the worker level.
func TestReadThenWriteFile(t *testing.T) {
	dir := t.TempDir()
	if err := WriteFile(dir, "f.txt", []byte("one"), ExpectAbsent()); err != nil {
		t.Fatalf("create: %v", err)
	}
	read, err := ReadFile(dir, "f.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(dir, "f.txt", []byte("two"), ExpectContent(read.Content)); err != nil {
		t.Fatalf("replace: %v", err)
	}
	// The same expectation again is stale now.
	if err := WriteFile(dir, "f.txt", []byte("three"), ExpectContent(read.Content)); !errors.Is(err, ErrChanged) {
		t.Errorf("stale write: err = %v, want ErrChanged", err)
	}
	if got := readFile(t, filepath.Join(dir, "f.txt")); got != "two" {
		t.Errorf("content = %q, want two", got)
	}
}

func TestWriteFileRefusesBadInput(t *testing.T) {
	dir := t.TempDir()
	tests := map[string]struct {
		name string
		data []byte
	}{
		"bad name":  {"../x", []byte("x")},
		"temp name": {".f.txt.tmp-1", []byte("x")},
		"too large": {"f.txt", []byte(strings.Repeat("x", MaxFileBytes+1))},
	}
	for label, tt := range tests {
		if err := WriteFile(dir, tt.name, tt.data, ExpectAbsent()); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", label, err)
		}
	}
	if err := WriteFile(dir, "max", []byte(strings.Repeat("x", MaxFileBytes)), ExpectAbsent()); err != nil {
		t.Errorf("exactly MaxFileBytes: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("directory holds %d entries, want only max", len(entries))
	}
}
