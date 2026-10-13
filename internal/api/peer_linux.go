// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package api

import (
	"errors"
	"net"
	"syscall"
)

// peerFromConn asks the kernel who opened c (SO_PEERCRED). It must be a
// *net.UnixConn; anything else is an error, never a guess.
//
// TODO: the same steps as peerCred in cmd/waystone-helper/peercred.go:
// SyscallConn, then Control with syscall.GetsockoptUcred(fd, SOL_SOCKET,
// SO_PEERCRED). Return both errors (Control's and getsockopt's).
func peerFromConn(c net.Conn) (Peer, error) {
	unixConn, ok := c.(*net.UnixConn)
	if !ok {
		return Peer{}, errors.New("peer credentials require a Unix connection")
	}

	rawConn, err := unixConn.SyscallConn()
	if err != nil {
		return Peer{}, err
	}

	var cred *syscall.Ucred
	var credErr error

	err = rawConn.Control(func(fd uintptr) {
		cred, credErr = syscall.GetsockoptUcred(
			int(fd),
			syscall.SOL_SOCKET,
			syscall.SO_PEERCRED,
		)
	})
	if err != nil {
		return Peer{}, err
	}

	if credErr != nil {
		return Peer{}, credErr
	}

	return Peer{
		UID: cred.Uid,
		GID: cred.Gid,
		PID: cred.Pid,
	}, nil
}
