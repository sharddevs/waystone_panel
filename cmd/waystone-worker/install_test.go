// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sharddevs/waystone_panel/internal/worker"
)

// hashOf returns the hash argument install-file takes: the SHA-256 of s as
// 64 lowercase hex digits.
func hashOf(s string) string { return worker.ExpectContent([]byte(s)).String() }

func TestRunInstallFile(t *testing.T) {
	dir := t.TempDir()
	r := call(t, dir, 1000, "jar bytes", "install-file", "server.jar", hashOf("jar bytes"))
	if r.code != 0 {
		t.Fatalf("exit %d, stderr %q", r.code, r.stderr)
	}
	if got := decode[worker.InstallResult](t, r.stdout); got.Bytes != 9 {
		t.Errorf("result = %+v, want 9 bytes", got)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "server.jar")); err != nil || string(b) != "jar bytes" {
		t.Errorf("server.jar = %q, %v", b, err)
	}
}

// Unlike write-file's "changed" answer, a hash mismatch is a real failure:
// exit 1, nothing on stdout, and no file left behind.
func TestRunInstallFileMismatch(t *testing.T) {
	dir := t.TempDir()
	r := call(t, dir, 1000, "evil", "install-file", "server.jar", hashOf("good"))
	if r.code != 1 || r.stdout != "" || !strings.Contains(r.stderr, "does not match") {
		t.Errorf("exit %d, stdout %q, stderr %q; want 1, nothing, and \"does not match\"", r.code, r.stdout, r.stderr)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("a mismatch left files: %v", entries)
	}
}

func TestRunInstallFileRejectsBadInput(t *testing.T) {
	for _, tt := range []struct {
		args []string
		code int
	}{
		// A wrong argument count is a usage error (exit 2). A bad hash or
		// name is exit 1. "absent" only means something to write-file.
		{[]string{"install-file", "server.jar"}, 2},
		{[]string{"install-file", "server.jar", hashOf("x"), "extra"}, 2},
		{[]string{"install-file", "server.jar", "absent"}, 1},
		{[]string{"install-file", "server.jar", strings.ToUpper(hashOf("x"))}, 1},
		{[]string{"install-file", "../server.jar", hashOf("x")}, 1},
	} {
		dir := t.TempDir()
		r := call(t, dir, 1000, "x", tt.args...)
		if r.code != tt.code || r.stdout != "" || r.stderr == "" {
			t.Errorf("%q: exit %d, stdout %q, stderr %q; want %d, nothing, and a message", tt.args, r.code, r.stdout, r.stderr, tt.code)
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Errorf("%q created files: %v", tt.args, entries)
		}
	}
}

func TestRunInstallFileRefusesRoot(t *testing.T) {
	dir := t.TempDir()
	r := call(t, dir, 0, "x", "install-file", "server.jar", hashOf("x"))
	if r.code != 1 || !strings.Contains(r.stderr, "root") {
		t.Errorf("as root: exit %d, stderr %q; want 1 and a message about root", r.code, r.stderr)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("as root created files: %v", entries)
	}
}
