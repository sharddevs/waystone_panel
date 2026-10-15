// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package web

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// rawGet sends the proxy's exact request over c and returns the status
// line, or an error if the server closed the connection first.
func rawGet(c net.Conn) (string, error) {
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	req := "GET /healthz HTTP/1.1\r\nHost: panel.example.com\r\nX-Forwarded-For: 203.0.113.7\r\n" +
		"X-Forwarded-Proto: https\r\nX-Forwarded-Host: panel.example.com\r\nConnection: close\r\n\r\n"
	if _, err := io.WriteString(c, req); err != nil {
		return "", err
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// W2 on a real socket: a connection from the proxy's uid is served; any
// other uid, or one whose credentials can't be read, is closed before a
// byte is read, and logged. The uid comes from a stand-in for the kernel
// so the test doesn't depend on which user runs it.
func TestServerOnlyServesProxy(t *testing.T) {
	logs := &syncBuffer{}
	h, err := New(Config{Public: mustOrigin(t, "https://panel.example.com"), ProxyUID: testProxyUID, Log: log.New(logs, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "web.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	var nextUID atomic.Int64
	srv := h.newServer(l, func(net.Conn) (uint32, error) {
		uid := nextUID.Load()
		if uid < 0 {
			return 0, errors.New("no credentials")
		}
		return uint32(uid), nil
	})
	done := make(chan error, 1)
	go func() { done <- srv.Serve() }()
	if srv.Addr().String() != path {
		t.Errorf("Addr %q", srv.Addr())
	}

	for _, c := range []struct {
		uid    int64
		served bool
	}{
		{testProxyUID, true},
		{0, false},
		{995, false},
		{1000, false},
		{-1, false},
		{testProxyUID, true},
	} {
		nextUID.Store(c.uid)
		conn, err := net.Dial("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		line, err := rawGet(conn)
		conn.Close()
		if c.served && line != "HTTP/1.1 200 OK" {
			t.Errorf("uid %d: %q %v, want 200", c.uid, line, err)
		}
		if !c.served && err == nil {
			t.Errorf("uid %d: got %q, want the connection closed", c.uid, line)
		}
	}
	for _, want := range []string{"from uid 0 ", "from uid 995 ", "from uid 1000 ", "no peer credentials"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log lacks %q: %s", want, logs)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, http.ErrServerClosed) {
		t.Errorf("Serve returned %v", err)
	}
	_ = srv.Close()
}

// The same through the real kernel check: the proxy uid is our own, so a
// connection from this process is served. Root can't be the proxy, so the
// test needs a non-root user (the VM runs it as trevor).
func TestServerRealPeerCred(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root, and root can't be the proxy uid")
	}
	h, err := New(Config{Public: mustOrigin(t, "https://panel.example.com"), ProxyUID: uint32(os.Getuid()), Log: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "web.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	srv := h.NewServer(l)
	go srv.Serve()
	defer srv.Close()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if line, err := rawGet(conn); line != "HTTP/1.1 200 OK" {
		t.Errorf("own uid as the proxy: %q %v", line, err)
	}

	other, err := New(Config{Public: mustOrigin(t, "https://panel.example.com"), ProxyUID: uint32(os.Getuid()) + 1, Log: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	path2 := filepath.Join(t.TempDir(), "web2.sock")
	l2, err := net.Listen("unix", path2)
	if err != nil {
		t.Fatal(err)
	}
	srv2 := other.NewServer(l2)
	go srv2.Serve()
	defer srv2.Close()
	conn2, err := net.Dial("unix", path2)
	if err != nil {
		t.Fatal(err)
	}
	defer conn2.Close()
	if line, err := rawGet(conn2); err == nil {
		t.Errorf("another uid's proxy socket answered us: %q", line)
	}
}

// The server has the local API's limits (I12).
func TestServerLimits(t *testing.T) {
	h, _ := newTestHandler(t)
	l, err := net.Listen("unix", filepath.Join(t.TempDir(), "s.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	s := h.NewServer(l).http
	if s.ReadHeaderTimeout != 5*time.Second || s.ReadTimeout != 10*time.Second || s.WriteTimeout != 30*time.Second ||
		s.IdleTimeout != 60*time.Second || s.MaxHeaderBytes != 16<<10 || s.Handler != h {
		t.Errorf("server limits %+v", s)
	}
}

// A refused connection is closed at once, not left open until a timeout:
// the caller sees the end of the stream right away.
func TestServerClosesRefusedConn(t *testing.T) {
	h, err := New(Config{Public: mustOrigin(t, "https://panel.example.com"), ProxyUID: testProxyUID, Log: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "web.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	srv := h.newServer(l, func(net.Conn) (uint32, error) { return 1000, nil })
	go srv.Serve()
	defer srv.Close()
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := c.Read(make([]byte, 1))
	var ne net.Error
	if n != 0 || err == nil || (errors.As(err, &ne) && ne.Timeout()) {
		t.Errorf("read on a refused connection: n=%d err=%v; want it closed at once", n, err)
	}
}
