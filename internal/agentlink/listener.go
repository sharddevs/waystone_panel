// SPDX-License-Identifier: AGPL-3.0-only

package agentlink

import (
	"errors"
	"net"
	"sync"
	"syscall"
)

// MaxConns is how many connections the agent socket keeps open at once
// (R8). One agent needs one; the rest is room for a takeover overlapping
// an old connection that hasn't noticed it's dead.
const MaxConns = 8

// limitListener closes every connection beyond its limit as soon as it is
// accepted, before the handshake (R8: the 9th is closed at accept).
type limitListener struct {
	net.Listener
	slots chan struct{}
}

func newLimitListener(l net.Listener, n int) net.Listener {
	return &limitListener{Listener: l, slots: make(chan struct{}, n)}
}

func (l *limitListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		select {
		case l.slots <- struct{}{}:
			return &limitConn{Conn: c, release: func() { <-l.slots }}, nil
		default:
			c.Close()
		}
	}
}

// limitConn gives its slot back exactly once, however often it's closed.
type limitConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *limitConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}

// SyscallConn passes the socket's descriptor through, so the handshake can
// still ask the kernel for the peer's uid (peercred.UID).
func (c *limitConn) SyscallConn() (syscall.RawConn, error) {
	sc, ok := c.Conn.(syscall.Conn)
	if !ok {
		return nil, errors.New("connection has no descriptor")
	}
	return sc.SyscallConn()
}
