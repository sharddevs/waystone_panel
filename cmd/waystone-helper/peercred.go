//go:build linux

package main

import (
	"errors"
	"net"
	"syscall"

	"github.com/sharddevs/waystone_panel/internal/helper"
)

// peerCred asks the kernel (SO_PEERCRED) who opened the connection.
// Unlike anything in the request, the caller can't fake this.
func peerCred(conn net.Conn) (helper.PeerCred, error) {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return helper.PeerCred{}, errors.New("peer credentials require a Unix connection")
	}
	rawConn, err := unixConn.SyscallConn()
	if err != nil {
		return helper.PeerCred{}, err
	}
	var cred *syscall.Ucred
	var credErr error

	// Control lends us the raw fd only inside the callback, so the
	// getsockopt result has to be passed out through these variables.
	err = rawConn.Control(func(fd uintptr) {
		cred, credErr = syscall.GetsockoptUcred(
			int(fd),
			syscall.SOL_SOCKET,
			syscall.SO_PEERCRED,
		)
	})
	if err != nil {
		return helper.PeerCred{}, err
	}

	if credErr != nil {
		return helper.PeerCred{}, credErr
	}
	return helper.PeerCred{
		UID: cred.Uid,
		GID: cred.Gid,
		PID: cred.Pid,
	}, nil
}
