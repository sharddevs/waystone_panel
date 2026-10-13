// SPDX-License-Identifier: AGPL-3.0-only

package agentlink

import (
	"net"
	"path/filepath"
	"testing"
	"time"
)

// R8: with 8 connections open, the 9th is closed at accept. Closing one
// frees its slot, once, however often it's closed.
func TestLimitListener(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.sock")
	raw, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	l := newLimitListener(raw, MaxConns)
	defer l.Close()
	accepted := make(chan net.Conn, 16)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			accepted <- c
		}
	}()
	dial := func() net.Conn {
		c, err := net.Dial("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	var held []net.Conn
	for range MaxConns {
		dial()
		held = append(held, <-accepted)
	}
	ninth := dial()
	ninth.SetReadDeadline(time.Now().Add(2 * time.Second))
	if n, err := ninth.Read(make([]byte, 1)); err == nil {
		t.Fatalf("9th connection read %d bytes, want it closed", n)
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("9th connection is still open")
	}
	select {
	case <-accepted:
		t.Fatal("9th connection was handed out")
	case <-time.After(50 * time.Millisecond):
	}

	held[0].Close()
	held[0].Close()
	dial()
	select {
	case c := <-accepted:
		held = append(held, c)
	case <-time.After(2 * time.Second):
		t.Fatal("a freed slot wasn't reused")
	}
	// Still full: a double close didn't free two slots.
	extra := dial()
	extra.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := extra.Read(make([]byte, 1)); err == nil {
		t.Fatal("connection beyond the limit after a double close")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("double close freed two slots")
	}
	for _, c := range held {
		c.Close()
	}
}
