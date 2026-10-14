// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"errors"
	"fmt"
	"net"

	"google.golang.org/grpc/credentials"
)

// controllerAuth is what the client handshake learned: the uid of the
// process that is listening, from the kernel.
type controllerAuth struct {
	credentials.CommonAuthInfo
	UID uint32
}

func (controllerAuth) AuthType() string { return "peercred" }

// clientCreds is the agent's side of R2. gRPC calls ClientHandshake right
// after connecting and before it sends anything, so the agent never talks
// to a listener that isn't the controller's user or root.
//
// Root has to be allowed: the kernel reports the uid that created the
// listening socket, and in production that is systemd (uid 0), which then
// hands it to the controller (waystone-controller-agent.socket). Root is
// trusted anyway, and only root can create a socket in the root-owned
// /run/waystone-controller, so any other user is still refused.
type clientCreds struct {
	controllerUID uint32
	uidOf         func(net.Conn) (uint32, error)
}

func (c *clientCreds) ClientHandshake(_ context.Context, _ string, conn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	uid, err := c.uidOf(conn)
	if err == nil && uid != c.controllerUID && uid != 0 {
		err = fmt.Errorf("listener uid %d is not the controller", uid)
	}
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("agent link: %w", err)
	}
	return conn, controllerAuth{
		CommonAuthInfo: credentials.CommonAuthInfo{SecurityLevel: credentials.PrivacyAndIntegrity},
		UID:            uid,
	}, nil
}

// ServerHandshake is never used: these credentials are client-only.
func (c *clientCreds) ServerHandshake(net.Conn) (net.Conn, credentials.AuthInfo, error) {
	return nil, nil, errors.New("agent link: client credentials used by a server")
}

func (c *clientCreds) Info() credentials.ProtocolInfo {
	return credentials.ProtocolInfo{SecurityProtocol: "peercred"}
}

func (c *clientCreds) Clone() credentials.TransportCredentials {
	clone := *c
	return &clone
}

func (c *clientCreds) OverrideServerName(string) error { return nil }
