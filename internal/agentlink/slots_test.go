// SPDX-License-Identifier: AGPL-3.0-only

package agentlink

import (
	"net"
	"net/netip"
	"testing"
	"time"
)

func TestSlotPools(t *testing.T) {
	s := newSlots(2, 3, 1, 1)
	a, b := addrKey(netip.MustParseAddr("192.0.2.1")), addrKey(netip.MustParseAddr("192.0.2.2"))
	if !s.take(a) || !s.take(a) || s.take(a) {
		t.Fatal("per-address limit of 2 not kept")
	}
	if !s.take(b) || s.take(b) {
		t.Fatal("handshaking limit of 3 not kept")
	}
	// One of a's moves to pinned: a may then start another.
	if !s.move(a, poolHandshaking, poolPinned) || s.perIP[a] != 1 || !s.take(a) {
		t.Fatalf("pinned move: %+v", s)
	}
	// The pinned pool is full now.
	if s.move(b, poolHandshaking, poolPinned) {
		t.Fatal("second pinned slot")
	}
	// Enroll-only keeps the address's count.
	if !s.move(b, poolHandshaking, poolEnrollOnly) || s.perIP[b] != 1 {
		t.Fatalf("enroll-only move: %+v", s)
	}
	if s.move(a, poolHandshaking, poolEnrollOnly) {
		t.Fatal("second enroll-only slot")
	}
	s.release(b, poolEnrollOnly)
	s.release(a, poolPinned)
	s.release(a, poolHandshaking)
	s.release(a, poolHandshaking)
	if s.count != [3]int{0, 0, 0} || len(s.perIP) != 0 {
		t.Fatalf("after releasing everything: %+v", s)
	}
}

func TestSlotConnReleasesOnce(t *testing.T) {
	s := newSlots(4, 4, 4, 4)
	ip := addrKey(netip.MustParseAddr("192.0.2.1"))
	s.take(ip)
	c := &slotConn{Conn: nopConn{}, slots: s, ip: ip}
	if !c.settle(true) || c.settle(false) {
		t.Fatal("settle should work once")
	}
	c.Close()
	c.Close()
	if s.count != [3]int{0, 0, 0} {
		t.Fatalf("counts %v after two closes", s.count)
	}
	if c.settle(true) {
		t.Fatal("settled after close")
	}
}

// 6a-H2: one host's /64 is one address; v4-mapped is its IPv4 address.
func TestAddrKey(t *testing.T) {
	for in, want := range map[string]string{
		"192.0.2.1":                 "192.0.2.1",
		"::ffff:192.0.2.1":          "192.0.2.1",
		"2001:db8:1:2:3:4:5:6":      "2001:db8:1:2::/64",
		"2001:db8:1:2:ffff::1%eth0": "2001:db8:1:2::/64",
	} {
		if got := addrKey(netip.MustParseAddr(in)); got != want {
			t.Errorf("addrKey(%s) = %q, want %q", in, got, want)
		}
	}
}

// An enroll-only connection is closed after its lifetime, or soon after
// its one Enroll call; a pinned one is never timed.
func TestSlotConnLifetime(t *testing.T) {
	s := newSlots(4, 4, 4, 4)
	s.lifetime = 50 * time.Millisecond
	ip := addrKey(netip.MustParseAddr("192.0.2.1"))
	s.take(ip)
	idle := &slotConn{Conn: nopConn{}, slots: s, ip: ip}
	idle.settle(false)
	s.take(ip)
	pinned := &slotConn{Conn: nopConn{}, slots: s, ip: ip}
	pinned.settle(true)
	waitTrue(t, func() bool { idle.mu.Lock(); defer idle.mu.Unlock(); return idle.closed })
	time.Sleep(100 * time.Millisecond)
	if pinned.closed {
		t.Fatal("pinned connection timed out")
	}
	if !pinned.claimEnroll() || pinned.claimEnroll() {
		t.Fatal("one Enroll per connection, pinned too")
	}
	pinned.enrollDone()
	time.Sleep(100 * time.Millisecond)
	if pinned.closed {
		t.Fatal("pinned connection closed after Enroll")
	}
	if idle.claimEnroll() {
		t.Fatal("Enroll on a closed connection")
	}
}

func waitTrue(t *testing.T, f func() bool) {
	t.Helper()
	for range 200 {
		if f() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out")
}

// nopConn is a net.Conn that does nothing.
type nopConn struct{ net.Conn }

func (nopConn) Close() error { return nil }
