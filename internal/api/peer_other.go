// SPDX-License-Identifier: AGPL-3.0-only

//go:build !linux

package api

import (
	"errors"
	"net"
)

// peerFromConn needs SO_PEERCRED, which is Linux-only. On other systems
// every API request fails closed with an internal error.
func peerFromConn(net.Conn) (Peer, error) {
	return Peer{}, errors.New("peer credentials are only supported on Linux")
}
