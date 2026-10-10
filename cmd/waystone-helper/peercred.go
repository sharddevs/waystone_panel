//go:build linux

package main

import (
	"errors"
	"net"
	"syscall"

	"github.com/sharddevs/waystone_panel/internal/helper"
)

// peerCred asks the kernel (SO_PEERCRED) who opened the connection.
// The caller cannot fake this, unlike anything it sends in a request.
//
// Hint: conn must be a *net.UnixConn; use its SyscallConn().Control to get
// the file descriptor, and syscall.GetsockoptUcred(fd, syscall.SOL_SOCKET,
// syscall.SO_PEERCRED). Check both the Control error and the getsockopt error.
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
