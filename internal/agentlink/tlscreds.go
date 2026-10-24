// SPDX-License-Identifier: AGPL-3.0-only

package agentlink

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"time"

	"google.golang.org/grpc/credentials"

	"github.com/sharddevs/waystone_panel/internal/agenttls"
	"github.com/sharddevs/waystone_panel/internal/store"
)

// pinLookupTimeout bounds the handshake's look at whether a key is a
// node's.
const pinLookupTimeout = 2 * time.Second

// TLSAuth is what the TCP handshake learned: the TLS state and the pin of
// the peer's one certificate. Whether that pin may do anything is decided
// per call, by the authorizer.
type TLSAuth struct {
	credentials.TLSInfo
	Pin agenttls.Pin
	// conn is the listener's connection, for Enroll's one call; nil if
	// the listener isn't the slot listener.
	conn *slotConn
	// revoked: at the handshake the key was a revoked node's.
	revoked bool
}

// tlsCreds is the controller's TLS on the agent listener (6a-2): gRPC's
// own TLS with agenttls.ServerConfig, then the pin, then the slot move
// (6H1): a key that is an active node's moves its connection to the
// pinned pool, any other to the enroll-only pool. This is only about
// room; what a connection may call is decided per call.
type tlsCreds struct {
	credentials.TransportCredentials
	lookup func(ctx context.Context, pin agenttls.Pin) (store.AgentNode, error)
	log    func(format string, args ...any)
}

func newTLSCreds(own tls.Certificate, lookup func(context.Context, agenttls.Pin) (store.AgentNode, error), log func(string, ...any)) *tlsCreds {
	return &tlsCreds{TransportCredentials: credentials.NewTLS(agenttls.ServerConfig(own)), lookup: lookup, log: log}
}

func (c *tlsCreds) ServerHandshake(raw net.Conn) (net.Conn, credentials.AuthInfo, error) {
	conn, info, err := c.TransportCredentials.ServerHandshake(raw)
	if err != nil {
		return nil, nil, err
	}
	ti, ok := info.(credentials.TLSInfo)
	if !ok {
		conn.Close()
		return nil, nil, fmt.Errorf("agent link: unexpected auth info %T", info)
	}
	pin, err := agenttls.PeerPin(ti.State)
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("agent link: %w", err)
	}
	sc, _ := raw.(*slotConn)
	revoked := false
	if sc != nil {
		ctx, cancel := context.WithTimeout(context.Background(), pinLookupTimeout)
		n, err := c.lookup(ctx, pin)
		cancel()
		pinned := err == nil && !n.Revoked
		revoked = err == nil && n.Revoked
		if !sc.settle(pinned) {
			conn.Close()
			c.log("agent_link=refused reason=no_slot pinned=%t", pinned)
			return nil, nil, fmt.Errorf("agent link: no connection slot")
		}
	}
	return conn, TLSAuth{TLSInfo: ti, Pin: pin, conn: sc, revoked: revoked}, nil
}

func (c *tlsCreds) Clone() credentials.TransportCredentials {
	return &tlsCreds{TransportCredentials: c.TransportCredentials.Clone(), lookup: c.lookup, log: c.log}
}
