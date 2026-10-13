// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package peercred

import (
	"net"
	"syscall"
)

// UID returns the uid of the process on the other end of c. For an
// accepted connection that's whoever connected; for a dialled one it's
// whoever called listen(). c must be a Unix socket that gives access to
// its descriptor (syscall.Conn): a *net.UnixConn, or a wrapper that
// passes SyscallConn through.
func UID(c net.Conn) (uint32, error) {
	sc, ok := c.(syscall.Conn)
	if !ok || c.LocalAddr().Network() != "unix" {
		return 0, ErrUnsupported
	}
	rawConn, err := sc.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *syscall.Ucred
	var credErr error
	err = rawConn.Control(func(fd uintptr) {
		cred, credErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	})
	if err != nil {
		return 0, err
	}
	if credErr != nil {
		return 0, credErr
	}
	return cred.Uid, nil
}
