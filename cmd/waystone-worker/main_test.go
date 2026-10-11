// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sharddevs/waystone_panel/internal/worker"
)

type result struct {
	code   int
	stdout string
	stderr string
}

// call runs the worker in dir with the given euid, stdin and arguments.
func call(t *testing.T, dir string, euid int, stdin string, args ...string) result {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(args, strings.NewReader(stdin), &stdout, &stderr, euid, dir)
	return result{code, stdout.String(), stderr.String()}
}

func decode[T any](t *testing.T, s string) T {
	t.Helper()
	var v T
	dec := json.NewDecoder(strings.NewReader(s))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("stdout %q is not the expected JSON: %v", s, err)
	}
	return v
}

func TestRunRefusesRoot(t *testing.T) {
	for _, args := range [][]string{
		{"write-file", "f.txt", "absent"},
		{"read-file", "f.txt"},
		{},
	} {
		dir := t.TempDir()
		r := call(t, dir, 0, "data", args...)
		if r.code != 1 || !strings.Contains(r.stderr, "root") || r.stdout != "" {
			t.Errorf("%q as root: exit %d, stdout %q, stderr %q; want 1, nothing, and a message about root", args, r.code, r.stdout, r.stderr)
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Errorf("%q as root touched the directory: %v", args, entries)
		}
	}
}

func TestRunUsage(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"accept-eula", "x"},
		{"read-file"},
		{"read-file", "a", "b"},
		{"write-file", "f.txt"},
		{"write-file", "f.txt", "absent", "extra"},
	} {
		r := call(t, t.TempDir(), 1000, "", args...)
		if r.code != 2 || r.stderr == "" || r.stdout != "" {
			t.Errorf("%q: exit %d, stdout %q, stderr %q; want 2 and a usage line", args, r.code, r.stdout, r.stderr)
		}
	}
}

func TestRunReadFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("hello\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	r := call(t, dir, 1000, "ignored", "read-file", "f.txt")
	if r.code != 0 {
		t.Fatalf("exit %d, stderr %q", r.code, r.stderr)
	}
	if got := decode[worker.ReadResult](t, r.stdout); !got.Exists || string(got.Content) != "hello\n" {
		t.Errorf("read %+v", got)
	}

	r = call(t, dir, 1000, "", "read-file", "missing.txt")
	if r.code != 0 {
		t.Fatalf("missing file: exit %d, stderr %q", r.code, r.stderr)
	}
	if got := decode[worker.ReadResult](t, r.stdout); got.Exists || len(got.Content) != 0 {
		t.Errorf("missing file read as %+v", got)
	}
}

func TestRunWriteFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")

	r := call(t, dir, 1000, "one", "write-file", "f.txt", "absent")
	if r.code != 0 || decode[worker.WriteResult](t, r.stdout).Outcome != worker.OutcomeWritten {
		t.Fatalf("create: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	expect := worker.ExpectContent([]byte("one")).String()
	r = call(t, dir, 1000, "two", "write-file", "f.txt", expect)
	if r.code != 0 || decode[worker.WriteResult](t, r.stdout).Outcome != worker.OutcomeWritten {
		t.Fatalf("replace: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	if b, _ := os.ReadFile(path); string(b) != "two" {
		t.Errorf("content = %q, want two", b)
	}
}

// A stale expectation is an answer, not a failure: exit 0, outcome changed,
// file untouched.
func TestRunWriteFileChanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("admin edit"), 0o640); err != nil {
		t.Fatal(err)
	}
	for _, expect := range []string{"absent", worker.ExpectContent([]byte("old")).String()} {
		r := call(t, dir, 1000, "new", "write-file", "f.txt", expect)
		if r.code != 0 || decode[worker.WriteResult](t, r.stdout).Outcome != worker.OutcomeChanged {
			t.Errorf("expect %s: exit %d, stdout %q, stderr %q; want 0 and changed", expect, r.code, r.stdout, r.stderr)
		}
	}
	if b, _ := os.ReadFile(path); string(b) != "admin edit" {
		t.Errorf("content = %q, want the admin's edit", b)
	}
}

func TestRunRejectsBadInput(t *testing.T) {
	big := strings.Repeat("x", worker.MaxFileBytes+1)
	tests := map[string]struct {
		stdin string
		args  []string
	}{
		"bad name read":    {"", []string{"read-file", "../etc/passwd"}},
		"bad name write":   {"x", []string{"write-file", "a/b", "absent"}},
		"bad expect":       {"x", []string{"write-file", "f.txt", "whatever"}},
		"uppercase expect": {"x", []string{"write-file", "f.txt", strings.ToUpper(worker.ExpectContent(nil).String())}},
		"too much stdin":   {big, []string{"write-file", "f.txt", "absent"}},
	}
	for name, tt := range tests {
		dir := t.TempDir()
		r := call(t, dir, 1000, tt.stdin, tt.args...)
		if r.code != 1 || !strings.HasPrefix(r.stderr, "waystone-worker: ") || r.stdout != "" {
			t.Errorf("%s: exit %d, stdout %q, stderr %q; want 1 and a waystone-worker: message", name, r.code, r.stdout, r.stderr)
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Errorf("%s: wrote %v", name, entries)
		}
	}
}

func TestRunRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "target.txt"), []byte("keep"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target.txt", filepath.Join(dir, "eula.txt")); err != nil {
		t.Fatal(err)
	}
	if r := call(t, dir, 1000, "", "read-file", "eula.txt"); r.code != 1 || r.stdout != "" {
		t.Errorf("read through a link: exit %d, stdout %q", r.code, r.stdout)
	}
	expect := worker.ExpectContent([]byte("keep")).String()
	if r := call(t, dir, 1000, "eula=true\n", "write-file", "eula.txt", expect); r.code != 1 || r.stdout != "" {
		t.Errorf("write over a link: exit %d, stdout %q", r.code, r.stdout)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "target.txt")); string(b) != "keep" {
		t.Errorf("link target changed to %q", b)
	}
}
