// SPDX-License-Identifier: AGPL-3.0-only

package worker

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// noTemps fails if any temporary file was left in dir.
func noTemps(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("temporary file left behind: %s", e.Name())
		}
	}
}

func TestWriteFileAtomicCreatesAndReplaces(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := WriteFileAtomic(dir, "f.txt", []byte("one")); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if got := readFile(t, path); got != "one" {
		t.Errorf("content = %q, want one", got)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != FileMode {
		t.Errorf("mode = %v, want a regular file with %v", info.Mode(), FileMode)
	}
	if err := WriteFileAtomic(dir, "f.txt", []byte("two")); err != nil {
		t.Fatalf("second write: %v", err)
	}
	if got := readFile(t, path); got != "two" {
		t.Errorf("content = %q, want two", got)
	}
	noTemps(t, dir)
}

// A game server can plant a symlink where the worker writes. The link must
// be replaced, and its target left alone.
func TestWriteFileAtomicReplacesSymlinkWithoutFollowing(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "elsewhere")
	writeFile(t, target, "secret")
	if err := os.Symlink(target, filepath.Join(dir, "eula.txt")); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(dir, "eula.txt", []byte("new")); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	info, err := os.Lstat(filepath.Join(dir, "eula.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() {
		t.Errorf("eula.txt is %v, want a regular file", info.Mode())
	}
	if got := readFile(t, target); got != "secret" {
		t.Errorf("symlink target changed to %q", got)
	}
}

func TestWriteFileAtomicRejectsBadNames(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"", ".", "..", "a/b", "../x", "/etc/passwd", ".hidden"} {
		if err := WriteFileAtomic(dir, name, []byte("x")); !errors.Is(err, ErrInvalid) {
			t.Errorf("name %q: err = %v, want ErrInvalid", name, err)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("files were created: %v", entries)
	}
}

// Renaming a file over a non-empty directory fails, after the temporary
// file already exists. It must be cleaned up.
func TestWriteFileAtomicCleansUpOnFailure(t *testing.T) {
	dir := t.TempDir()
	busy := filepath.Join(dir, "busy")
	if err := os.Mkdir(busy, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(busy, "inside"), "x")
	if err := WriteFileAtomic(dir, "busy", []byte("data")); err == nil {
		t.Fatal("renaming over a non-empty directory succeeded")
	}
	if info, err := os.Lstat(busy); err != nil || !info.IsDir() {
		t.Errorf("the directory was disturbed: %v %v", info, err)
	}
	noTemps(t, dir)
}

func TestReplaceIfMatchesNewFile(t *testing.T) {
	dir := t.TempDir()
	if err := ReplaceIfMatches(dir, "p", []byte("new"), ExpectAbsent()); err != nil {
		t.Fatalf("ReplaceIfMatches: %v", err)
	}
	if got := readFile(t, filepath.Join(dir, "p")); got != "new" {
		t.Errorf("content = %q", got)
	}
	noTemps(t, dir)
}

func TestReplaceIfMatchesUnchangedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "p")
	writeFile(t, path, "old")
	if err := ReplaceIfMatches(dir, "p", []byte("new"), ExpectContent([]byte("old"))); err != nil {
		t.Fatalf("ReplaceIfMatches: %v", err)
	}
	if got := readFile(t, path); got != "new" {
		t.Errorf("content = %q", got)
	}
	if info, _ := os.Lstat(path); info.Mode().Perm() != FileMode {
		t.Errorf("mode = %v, want %v", info.Mode().Perm(), FileMode)
	}
	noTemps(t, dir)
}

// An edit that was made and then undone leaves nothing to lose.
func TestReplaceIfMatchesComparesContentNotTimestamps(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "p")
	writeFile(t, path, "old")
	writeFile(t, path, "edited")
	writeFile(t, path, "old")
	if err := ReplaceIfMatches(dir, "p", []byte("new"), ExpectContent([]byte("old"))); err != nil {
		t.Fatalf("ReplaceIfMatches: %v", err)
	}
}

// Each case leaves the file different from what the caller expects; none
// may be overwritten.
func TestReplaceIfMatchesDetectsChanges(t *testing.T) {
	tests := map[string]struct {
		before string // "" = no file
		expect Expect
	}{
		"created meanwhile":    {"admin wrote this", ExpectAbsent()},
		"edited meanwhile":     {"edited by the admin", ExpectContent([]byte("old"))},
		"removed meanwhile":    {"", ExpectContent([]byte("old"))},
		"truncated meanwhile":  {"ol", ExpectContent([]byte("old"))},
		"zero Expect":          {"old", Expect{}},
		"zero Expect, no file": {"", Expect{}},
	}
	for name, tt := range tests {
		dir := t.TempDir()
		path := filepath.Join(dir, "p")
		if tt.before != "" {
			writeFile(t, path, tt.before)
		}
		err := ReplaceIfMatches(dir, "p", []byte("from the worker"), tt.expect)
		if !errors.Is(err, ErrChanged) {
			t.Errorf("%s: err = %v, want ErrChanged", name, err)
		}
		b, _ := os.ReadFile(path)
		if string(b) != tt.before {
			t.Errorf("%s: content = %q, want %q", name, b, tt.before)
		}
		noTemps(t, dir)
	}
}

func TestReplaceIfMatchesRefusesTooLargeFile(t *testing.T) {
	dir := t.TempDir()
	big := strings.Repeat("x", MaxFileBytes+1)
	writeFile(t, filepath.Join(dir, "p"), big)
	if err := ReplaceIfMatches(dir, "p", []byte("new"), ExpectContent([]byte(big))); !errors.Is(err, ErrChanged) {
		t.Errorf("err = %v, want ErrChanged: a file over the limit can't be one the caller read", err)
	}
	noTemps(t, dir)
}

// Conditional writes never replace a link or a directory, even when the
// link's target matches.
func TestReplaceIfMatchesRefusesNonRegular(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	writeFile(t, target, "old")
	if err := os.Symlink("target.txt", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"link", "sub"} {
		err := ReplaceIfMatches(dir, name, []byte("new"), ExpectContent([]byte("old")))
		if !errors.Is(err, ErrUnsupported) {
			t.Errorf("%s: err = %v, want ErrUnsupported", name, err)
		}
	}
	if got := readFile(t, target); got != "old" {
		t.Errorf("link target changed to %q", got)
	}
	if info, err := os.Lstat(filepath.Join(dir, "link")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the link was replaced: %v %v", info, err)
	}
	noTemps(t, dir)
}
