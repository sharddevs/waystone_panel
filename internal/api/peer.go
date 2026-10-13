// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"errors"
	"net"
)

// Peer is who is on the other end of an API connection, as the kernel
// reports it (SO_PEERCRED). The caller can't fake any of it.
type Peer struct {
	UID uint32
	GID uint32
	PID int32
}

type peerKey struct{}

// peerResult is what ConnContext stores: the peer, or why there isn't one.
type peerResult struct {
	peer Peer
	err  error
}

// ConnContext is meant for http.Server.ConnContext. It runs once per
// connection, asks the kernel who connected, and stores the answer in the
// connection's context, where every request on that connection finds it.
// Asking once per connection (not per request) is enough because a
// connection can't change hands.
func ConnContext(ctx context.Context, c net.Conn) context.Context {
	p, err := peerFromConn(c)
	return context.WithValue(ctx, peerKey{}, peerResult{peer: p, err: err})
}

// ContextWithPeer returns ctx with p as the peer. The tests use it to
// stand in for a real socket.
func ContextWithPeer(ctx context.Context, p Peer) context.Context {
	return context.WithValue(ctx, peerKey{}, peerResult{peer: p})
}

// errNoPeer means the request's context has no peer at all, which only
// happens if the handler is used without ConnContext.
var errNoPeer = errors.New("no peer credentials on this connection")

// peerFrom returns the peer stored in ctx, or why there is none.
func peerFrom(ctx context.Context) (Peer, error) {
	r, ok := ctx.Value(peerKey{}).(peerResult)
	if !ok {
		return Peer{}, errNoPeer
	}
	return r.peer, r.err
}
