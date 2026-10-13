// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package api

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// Over a real Unix socket the kernel reports our own process.
func TestPeerFromConn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	client, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	p, err := peerFromConn(server)
	if err != nil {
		t.Fatalf("peerFromConn: %v", err)
	}
	if p.UID != uint32(os.Getuid()) || p.GID != uint32(os.Getgid()) || p.PID != int32(os.Getpid()) {
		t.Errorf("peer = %+v, want uid %d gid %d pid %d", p, os.Getuid(), os.Getgid(), os.Getpid())
	}
}

// Anything that isn't a Unix connection is refused, never guessed.
func TestPeerFromConnRefusesOtherConns(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	if _, err := peerFromConn(a); err == nil {
		t.Error("net.Pipe: got a peer, want an error")
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
	s, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := peerFromConn(s); err == nil {
		t.Error("TCP: got a peer, want an error")
	}
}

// End to end: http.Server with ConnContext hands every request the
// caller's uid, taken from the socket.
func TestConnContextThroughServer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{
		ConnContext: ConnContext,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, err := peerFrom(r.Context())
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(p)
		}),
	}
	go srv.Serve(l)
	defer srv.Close()

	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		},
	}}
	resp, err := client.Get("http://waystone/whoami")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var p Peer
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		t.Fatalf("status %d: %v", resp.StatusCode, err)
	}
	if p.UID != uint32(os.Getuid()) {
		t.Errorf("uid = %d, want %d", p.UID, os.Getuid())
	}
}
