//go:build linux

package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
)

// listenFD is the first file descriptor systemd passes (SD_LISTEN_FDS_START).
const listenFD = 3

// activationListener returns the one socket systemd passed to this process.
//
// systemd's socket activation protocol: the socket is file descriptor 3, and
// the environment says LISTEN_PID=<our pid> and LISTEN_FDS=1. Check both
// before touching fd 3. If LISTEN_PID is another process, the descriptor is
// not ours. Then turn fd 3 into a listener (os.NewFile, net.FileListener),
// close the *os.File (FileListener made its own copy), and make sure the
// result is a *net.UnixListener.
//
// pid and getenv are parameters so tests can fake them.
func activationListener(pid int, getenv func(string) string) (net.Listener, error) {
	listenPID, err := strconv.Atoi(getenv("LISTEN_PID"))
	if err != nil {
		return nil, fmt.Errorf("invalid LISTEN_PID: %w", err)
	}

	if listenPID != pid {
		return nil, fmt.Errorf("LISTEN_PID mismatch: got %d, expected %d", listenPID, pid)
	}
	listenFDS, err := strconv.Atoi(getenv("LISTEN_FDS"))
	if err != nil {
		return nil, fmt.Errorf("invalid LISTEN_FDS: %w", err)
	}

	if listenFDS != 1 {
		return nil, fmt.Errorf("expected exactly 1 socket, got %d", listenFDS)
	}
	file := os.NewFile(uintptr(listenFD), "waystone-helper-socket")
	if file == nil {
		return nil, errors.New("could not wrap systemd socket descriptor")
	}
	listener, err := net.FileListener(file)
	file.Close()

	if err != nil {
		return nil, fmt.Errorf("could not create listener from systemd socket: %w", err)
	}
	unixListener, ok := listener.(*net.UnixListener)
	if !ok {
		listener.Close()
		return nil, errors.New("systemd socket is not a Unix listener")
	}

	return unixListener, nil
}
