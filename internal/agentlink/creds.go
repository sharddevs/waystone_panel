// SPDX-License-Identifier: AGPL-3.0-only

package agentlink

import (
	"context"
	"errors"
	"fmt"
	"net"

	"google.golang.org/grpc/credentials"
)

// PeerAuth is what the handshake learned about the agent: only its uid,
// straight from the kernel.
type PeerAuth struct {
	credentials.CommonAuthInfo
	UID uint32
}

// AuthType names the check, for gRPC's peer info.
func (PeerAuth) AuthType() string { return "peercred" }

// serverCreds is the controller's side of R2. gRPC calls ServerHandshake
// on every accepted connection before it reads a single byte, so a wrong
// uid is closed without the peer ever reaching HTTP/2 (F7).
type serverCreds struct {
	agentUID uint32
	uidOf    func(net.Conn) (uint32, error)
	refused  func(uid uint32, err error)
}

// ServerHandshake reads the peer's uid and refuses anything but the agent.
func (c *serverCreds) ServerHandshake(conn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	uid, err := c.uidOf(conn)
	if err == nil && uid != c.agentUID {
		err = fmt.Errorf("uid %d is not the agent", uid)
	}
	if err != nil {
		conn.Close()
		c.refused(uid, err)
		return nil, nil, fmt.Errorf("agent link: %w", err)
	}
	// A Unix socket never leaves the machine, so, like gRPC's own local
	// credentials, the connection counts as private.
	return conn, PeerAuth{
		CommonAuthInfo: credentials.CommonAuthInfo{SecurityLevel: credentials.PrivacyAndIntegrity},
		UID:            uid,
	}, nil
}

// ClientHandshake is never used: these credentials are server-only.
func (c *serverCreds) ClientHandshake(context.Context, string, net.Conn) (net.Conn, credentials.AuthInfo, error) {
	return nil, nil, errors.New("agent link: server credentials used by a client")
}

func (c *serverCreds) Info() credentials.ProtocolInfo {
	return credentials.ProtocolInfo{SecurityProtocol: "peercred"}
}

func (c *serverCreds) Clone() credentials.TransportCredentials {
	clone := *c
	return &clone
}

// OverrideServerName is deprecated in gRPC and means nothing here.
func (c *serverCreds) OverrideServerName(string) error { return nil }
