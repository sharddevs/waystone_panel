// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"io"
	"net"
	"net/http"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The I12 limits live on the http.Server, and ConnContext is what puts the
// caller's identity on every request. Without it every request is a 500.
func TestNewServerLimits(t *testing.T) {
	srv := newServer(http.NotFoundHandler())

	if srv.Handler == nil {
		t.Error("Handler is nil")
	}
	if srv.Addr != "" {
		t.Errorf("Addr = %q, want empty: the server only runs on a listener we hand it", srv.Addr)
	}
	for name, tc := range map[string]struct{ got, want time.Duration }{
		"ReadHeaderTimeout": {srv.ReadHeaderTimeout, 5 * time.Second},
		"ReadTimeout":       {srv.ReadTimeout, 10 * time.Second},
		"WriteTimeout":      {srv.WriteTimeout, 30 * time.Second},
		"IdleTimeout":       {srv.IdleTimeout, 60 * time.Second},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v", name, tc.got, tc.want)
		}
	}
	if srv.MaxHeaderBytes != 16<<10 {
		t.Errorf("MaxHeaderBytes = %d, want %d", srv.MaxHeaderBytes, 16<<10)
	}
	if srv.ConnContext == nil {
		t.Error("ConnContext is nil: requests would have no peer identity")
	}
}

// Without -api-socket the controller only runs under socket activation.
// It must never fall back to opening a socket (or a TCP port) of its own.
func TestAPIListenerNeedsActivation(t *testing.T) {
	noEnv := func(string) string { return "" }
	socks, err := listeners("", "", "", 4242, noEnv)
	if err == nil {
		socks.close()
		t.Fatal("listeners with no socket path and no activation succeeded, want an error")
	}
}

// Bad flags exit 2 before anything touches the database or a socket.
func TestRunBadFlags(t *testing.T) {
	for _, args := range [][]string{
		{"-nope"},
		{"extra"},
		{"-api-socket", "/tmp/x.sock", "extra"},
	} {
		var stderr strings.Builder
		if code := run(args, &stderr); code != 2 {
			t.Errorf("run(%q) = %d, want 2", args, code)
		}
		if stderr.Len() == 0 {
			t.Errorf("run(%q) printed nothing to stderr", args)
		}
	}
}

// A database that can't be reached is exit code 1, not a hang or a panic.
func TestRunDatabaseFailure(t *testing.T) {
	dir := t.TempDir()
	dsn := "host=" + dir + " dbname=nope user=nobody connect_timeout=2"
	code := run([]string{"-db", dsn, "-api-socket", dir + "/api.sock", "-web-socket", dir + "/web.sock",
		"-public-url", "https://panel.example.com", "-proxy-uid", "4242"}, io.Discard)
	if code != 1 {
		t.Errorf("run with an unreachable database = %d, want 1", code)
	}
}

// A bundle's address is the listener's own, an IPv4-mapped one unmapped,
// and nothing for a listener that isn't TCP.
func TestListenerAddr(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if got := listenerAddr(l); got.Addr() != netip.MustParseAddr("127.0.0.1") || got.Port() == 0 {
		t.Errorf("listenerAddr = %v", got)
	}
	// systemd's socket can hand over an IPv4 address in 16-byte form.
	mapped := addrListener{&net.TCPAddr{IP: net.ParseIP("10.0.20.80"), Port: 7443}}
	if got := listenerAddr(mapped); got != netip.MustParseAddrPort("10.0.20.80:7443") {
		t.Errorf("16-byte IPv4: listenerAddr = %v", got)
	}
	u, err := net.Listen("unix", filepath.Join(t.TempDir(), "s"))
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	if got := listenerAddr(u); got.IsValid() {
		t.Errorf("unix listener: %v", got)
	}
}

// addrListener is a listener that only has an address.
type addrListener struct{ addr net.Addr }

func (l addrListener) Accept() (net.Conn, error) { return nil, net.ErrClosed }
func (l addrListener) Close() error              { return nil }
func (l addrListener) Addr() net.Addr            { return l.addr }
