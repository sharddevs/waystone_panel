// SPDX-License-Identifier: AGPL-3.0-only

package agentlink

import (
	"net"
	"net/netip"
	"sync"
	"time"
)

// The TCP listener's connection limits (6H1). Every connection is in one
// of three pools, and the pools are separate, so a flood of TLS clients
// can't keep real nodes out:
//   - handshaking: accepted, handshake not done yet (at most
//     HandshakeTimeout);
//   - enroll-only: handshake done, but the key isn't an active node's,
//     so it can only enroll;
//   - pinned: the key is an active node's.
//
// One address (an IPv6 /64 counting as one, see addrKey) may hold at most
// MaxUnpinnedPerIP connections that aren't pinned. An enroll-only
// connection lives for one Enroll call or UnpinnedLifetime, whichever
// ends first (6a-H2); the deadline is fixed when the handshake ends, so
// keepalive pings never stretch it.
const (
	MaxUnpinnedPerIP = 4
	MaxHandshaking   = 128
	MaxEnrollOnly    = 16
	MaxPinned        = 64
	UnpinnedLifetime = 30 * time.Second
	// MaxStreamsPerConn caps the concurrent calls on one TCP connection.
	MaxStreamsPerConn = 4
	// enrollGrace is how long a connection stays after its Enroll call
	// returns, so the answer gets out.
	enrollGrace = time.Second
)

type pool int

const (
	poolHandshaking pool = iota
	poolEnrollOnly
	poolPinned
)

// slots counts the pools.
type slots struct {
	mu    sync.Mutex
	perIP map[string]int // connections not pinned, per addrKey
	count [3]int
	max   [3]int
	ipMax int
	// lifetime is UnpinnedLifetime but for tests.
	lifetime time.Duration
}

func newSlots(perIP, handshaking, enrollOnly, pinned int) *slots {
	return &slots{perIP: map[string]int{}, max: [3]int{handshaking, enrollOnly, pinned}, ipMax: perIP, lifetime: UnpinnedLifetime}
}

// addrKey is the per-address key, as the sign-in limiter's
// (internal/session): an IPv4-mapped address counts as its IPv4 address,
// and IPv6 counts by its /64, since one host usually has a whole /64.
func addrKey(ip netip.Addr) string {
	ip = ip.Unmap().WithZone("")
	if ip.Is6() {
		p, _ := ip.Prefix(64)
		return p.String()
	}
	return ip.String()
}

// take puts a new connection from ip in the handshaking pool, or reports
// there is no room.
func (s *slots) take(ip string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.count[poolHandshaking] >= s.max[poolHandshaking] || s.perIP[ip] >= s.ipMax {
		return false
	}
	s.count[poolHandshaking]++
	s.perIP[ip]++
	return true
}

// move takes a slot in pool to, then gives back the one in from. If to
// has no room nothing changes and it reports false.
func (s *slots) move(ip string, from, to pool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.count[to] >= s.max[to] {
		return false
	}
	s.count[to]++
	if to == poolPinned {
		s.dropIP(ip)
	}
	s.count[from]--
	return true
}

func (s *slots) release(ip string, p pool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.count[p]--
	if p != poolPinned {
		s.dropIP(ip)
	}
}

func (s *slots) dropIP(ip string) {
	if s.perIP[ip]--; s.perIP[ip] <= 0 {
		delete(s.perIP, ip)
	}
}

// slotListener hands out connections that hold a handshaking slot, and
// closes every other one as soon as it is accepted, before the handshake.
type slotListener struct {
	net.Listener
	slots *slots
}

func (l *slotListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		ip := addrKey(remoteIP(c))
		if !l.slots.take(ip) {
			c.Close()
			continue
		}
		return &slotConn{Conn: c, slots: l.slots, ip: ip}, nil
	}
}

// remoteIP is the peer's address, IPv4-mapped IPv6 folded into IPv4 so
// one host can't count as two. A connection without one (not TCP) counts
// as the zero address.
func remoteIP(c net.Conn) netip.Addr {
	if a, ok := c.RemoteAddr().(*net.TCPAddr); ok {
		if ip, ok := netip.AddrFromSlice(a.IP); ok {
			return ip.Unmap()
		}
	}
	return netip.Addr{}
}

// slotConn holds one slot until it is closed, exactly once.
type slotConn struct {
	net.Conn
	slots *slots
	ip    string // addrKey

	mu       sync.Mutex
	pool     pool
	closed   bool
	enrolled bool        // its one Enroll call has started
	timer    *time.Timer // closes an enroll-only connection
}

// settle moves a connection that finished its handshake to the pinned
// pool (its key is an active node's) or the enroll-only pool. It reports
// false if that pool is full; the caller then closes the connection.
func (c *slotConn) settle(pinned bool) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.pool != poolHandshaking {
		return false
	}
	to := poolEnrollOnly
	if pinned {
		to = poolPinned
	}
	if !c.slots.move(c.ip, poolHandshaking, to) {
		return false
	}
	c.pool = to
	if to == poolEnrollOnly {
		c.timer = time.AfterFunc(c.slots.lifetime, func() { c.Close() })
	}
	return true
}

// isPinned reports whether the connection's key was an active node's at
// the handshake.
func (c *slotConn) isPinned() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pool == poolPinned
}

// claimEnroll reports whether this connection may make its one Enroll
// call; any later call is refused.
func (c *slotConn) claimEnroll() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.enrolled {
		return false
	}
	c.enrolled = true
	return true
}

// enrollDone ends an enroll-only connection shortly after its Enroll
// call, whatever the answer. A key that just enrolled connects again,
// and is pinned this time.
func (c *slotConn) enrollDone() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.timer != nil && !c.closed {
		c.timer.Reset(enrollGrace)
	}
}

// closeSoon closes the connection in enrollGrace, so the answer that
// ends it gets out first.
func (c *slotConn) closeSoon() {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.closed:
	case c.timer != nil:
		c.timer.Reset(enrollGrace)
	default:
		c.timer = time.AfterFunc(enrollGrace, func() { c.Close() })
	}
}

func (c *slotConn) Close() error {
	err := c.Conn.Close()
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.closed = true
		c.slots.release(c.ip, c.pool)
		if c.timer != nil {
			c.timer.Stop()
		}
	}
	return err
}
