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
// pid and getenv are parameters so tests can fake them.
func activationListener(pid int, getenv func(string) string) (net.Listener, error) {
	listenPID, err := strconv.Atoi(getenv("LISTEN_PID"))
	if err != nil {
		return nil, fmt.Errorf("invalid LISTEN_PID: %w", err)
	}

	// If LISTEN_PID names another process, fd 3 isn't meant for us.
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
	// FileListener dups the descriptor, so our *os.File can be closed right away.
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
