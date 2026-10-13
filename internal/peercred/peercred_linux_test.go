// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package peercred

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
)

// Both ends of a real Unix socket see our own uid.
func TestUIDBothEnds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, _ := l.Accept()
		accepted <- c
	}()
	dialled, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer dialled.Close()
	server := <-accepted
	if server == nil {
		t.Fatal("accept failed")
	}
	defer server.Close()

	for name, c := range map[string]net.Conn{"accepted": server, "dialled": dialled} {
		uid, err := UID(c)
		if err != nil || uid != uint32(os.Getuid()) {
			t.Errorf("%s: UID = %d, %v, want %d", name, uid, err, os.Getuid())
		}
	}
}

// A wrapper that passes SyscallConn through still works.
type wrapped struct{ *net.UnixConn }

func TestUIDThroughWrapper(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	uid, err := UID(wrapped{c.(*net.UnixConn)})
	if err != nil || uid != uint32(os.Getuid()) {
		t.Errorf("UID(wrapper) = %d, %v", uid, err)
	}
}

// Anything but a Unix socket is refused, not guessed: a pipe has no
// descriptor, and a TCP socket isn't Unix.
func TestUIDNotUnix(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	if _, err := UID(a); !errors.Is(err, ErrUnsupported) {
		t.Errorf("UID(pipe) err = %v, want ErrUnsupported", err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := UID(c); !errors.Is(err, ErrUnsupported) {
		t.Errorf("UID(tcp) err = %v, want ErrUnsupported", err)
	}
}
