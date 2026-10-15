// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"io"
	"net/http"
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
