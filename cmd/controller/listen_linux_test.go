// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package main

import (
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"testing"
)

// The dev socket is 0600, so only our own user can connect, and closing
// the listener removes it again.
func TestListenDev(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api.sock")
	l, err := listenDev(path)
	if err != nil {
		t.Fatal(err)
	}
	if l.Addr().Network() != "unix" {
		t.Errorf("network = %q, want unix", l.Addr().Network())
	}
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Type() != fs.ModeSocket {
		t.Errorf("%s is %v, want a socket", path, fi.Mode().Type())
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("socket mode = %o, want 600", perm)
	}

	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial own socket: %v", err)
	}
	c.Close()

	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("after Close, Lstat = %v, want the socket gone", err)
	}
}

// listenDev never deletes or reuses what's already at the path.
func TestListenDevRefusesExistingPath(t *testing.T) {
	dir := t.TempDir()

	// A regular file stays exactly as it was.
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, []byte("not yours"), 0o644); err != nil {
		t.Fatal(err)
	}
	if l, err := listenDev(file); err == nil {
		l.Close()
		t.Fatal("listenDev over a regular file succeeded")
	}
	if b, err := os.ReadFile(file); err != nil || string(b) != "not yours" {
		t.Errorf("regular file changed: %q, %v", b, err)
	}

	// A live socket (another controller) is refused and keeps working.
	live := filepath.Join(dir, "live.sock")
	other, err := net.Listen("unix", live)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if l, err := listenDev(live); err == nil {
		l.Close()
		t.Fatal("listenDev over a live socket succeeded")
	}
	c, err := net.Dial("unix", live)
	if err != nil {
		t.Fatalf("the other socket stopped working: %v", err)
	}
	c.Close()

	// A dangling symlink counts as existing too.
	link := filepath.Join(dir, "link")
	if err := os.Symlink(filepath.Join(dir, "nowhere"), link); err != nil {
		t.Fatal(err)
	}
	if l, err := listenDev(link); err == nil {
		l.Close()
		t.Fatal("listenDev over a dangling symlink succeeded")
	}
}

// With -api-socket, apiListener is dev mode and ignores activation.
func TestAPIListenerDevMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api.sock")
	l, err := apiListener(path, 4242, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if got := l.Addr().String(); got != path {
		t.Errorf("listening on %q, want %q", got, path)
	}
}
