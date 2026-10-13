// SPDX-License-Identifier: AGPL-3.0-only

//go:build !linux

package peercred

import "net"

// UID always fails outside Linux, so every connection is refused.
func UID(net.Conn) (uint32, error) {
	return 0, ErrUnsupported
}
