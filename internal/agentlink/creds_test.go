// SPDX-License-Identifier: AGPL-3.0-only

package agentlink

import (
	"errors"
	"net"
	"sync/atomic"
	"testing"
)

// countingConn counts reads and remembers whether it was closed.
type countingConn struct {
	net.Conn
	reads  atomic.Int64
	closed atomic.Bool
}

func (c *countingConn) Read(b []byte) (int, error) {
	c.reads.Add(1)
	return c.Conn.Read(b)
}

func (c *countingConn) Close() error {
	c.closed.Store(true)
	return c.Conn.Close()
}

// R2/F7: a wrong uid, or no uid at all, is closed with nothing read. The
// right one passes, with its uid in the auth info.
func TestServerHandshake(t *testing.T) {
	cases := map[string]struct {
		uid  uint32
		err  error
		pass bool
	}{
		"agent":       {uid: 995, pass: true},
		"other user":  {uid: 1000},
		"root":        {uid: 0},
		"no peercred": {err: errors.New("not a unix socket")},
		// An error wins even if the uid happens to match.
		"error, matching uid": {uid: 995, err: errors.New("getsockopt failed")},
	}
	for name, c := range cases {
		a, b := net.Pipe()
		conn := &countingConn{Conn: a}
		var refusedUID uint32
		refusedCalls := 0
		creds := &serverCreds{
			agentUID: 995,
			uidOf:    func(net.Conn) (uint32, error) { return c.uid, c.err },
			refused:  func(uid uint32, _ error) { refusedCalls++; refusedUID = uid },
		}
		got, info, err := creds.ServerHandshake(conn)
		if c.pass {
			auth, ok := info.(PeerAuth)
			if err != nil || got != conn || !ok || auth.UID != 995 {
				t.Errorf("%s: handshake = %v, %v, %v", name, got, info, err)
			}
			if conn.closed.Load() || refusedCalls != 0 {
				t.Errorf("%s: closed or logged as refused", name)
			}
		} else {
			if err == nil || got != nil {
				t.Errorf("%s: handshake passed", name)
			}
			if !conn.closed.Load() {
				t.Errorf("%s: connection left open", name)
			}
			if refusedCalls != 1 || refusedUID != c.uid {
				t.Errorf("%s: refused called %d times with uid %d", name, refusedCalls, refusedUID)
			}
		}
		if n := conn.reads.Load(); n != 0 {
			t.Errorf("%s: %d reads before the uid check, want 0", name, n)
		}
		a.Close()
		b.Close()
	}
}
