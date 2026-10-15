// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package main

import (
	"bufio"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sharddevs/waystone_panel/internal/web"
)

// R10 for web.sock: shutdown stops the web server with the others, and a
// browser request that outlives the budget makes shutdown report failure.
// The web server only serves its proxy uid, so the test plays the proxy
// with its own uid, which root can't be.
func TestShutdownStopsWebServer(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("the proxy uid can't be root; run as a normal user")
	}
	o, err := web.ParsePublicURL("https://panel.example.com")
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}, 1), make(chan struct{})
	defer close(release)
	slow := web.Route{Method: http.MethodGet, Pattern: "/slow", CSRF: web.CSRFNotApplicable,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			entered <- struct{}{}
			<-release
		})}
	h, err := web.New(web.Config{Public: o, ProxyUID: uint32(os.Getuid()), Log: log.New(io.Discard, "", 0)}, slow)
	if err != nil {
		t.Fatal(err)
	}

	start := func() (*web.Server, string, chan error) {
		path := filepath.Join(t.TempDir(), "web.sock")
		l, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		srv := h.NewServer(l)
		done := make(chan error, 1)
		go func() { done <- srv.Serve() }()
		return srv, path, done
	}
	get := func(path, target string) net.Conn {
		c, err := net.Dial("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(c, "GET "+target+" HTTP/1.1\r\nHost: panel.example.com\r\nX-Forwarded-For: 203.0.113.7\r\n"+
			"X-Forwarded-Proto: https\r\nX-Forwarded-Host: panel.example.com\r\n\r\n")
		return c
	}

	// Idle: everything stops in time and Serve returns.
	srv, path, done := start()
	c := get(path, "/healthz")
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if line, err := bufio.NewReader(c).ReadString('\n'); err != nil || !strings.HasPrefix(line, "HTTP/1.1 200") {
		t.Fatalf("healthz through web.sock: %q %v", line, err)
	}
	c.Close()
	if !shutdown(newServer(http.NotFoundHandler()), srv, nil, 3*time.Second, io.Discard) {
		t.Error("shutdown of an idle web server ran out of budget")
	}
	select {
	case err := <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("web Serve returned %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown left the web server running")
	}

	// A request stuck past the budget: shutdown says so.
	srv, path, done = start()
	c = get(path, "/slow")
	defer c.Close()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the slow request never reached its handler")
	}
	var stderr strings.Builder
	if shutdown(newServer(http.NotFoundHandler()), srv, nil, 100*time.Millisecond, &stderr) {
		t.Error("shutdown reported success with a web request still running")
	}
	if !strings.Contains(stderr.String(), "web shutdown:") {
		t.Errorf("stderr %q doesn't name the web server", stderr.String())
	}
	<-done
}
