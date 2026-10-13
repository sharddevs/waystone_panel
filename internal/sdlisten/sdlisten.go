// SPDX-License-Identifier: AGPL-3.0-only

// Package sdlisten takes over a listening Unix socket that systemd created
// and passed to this process (socket activation). systemd sets the
// socket's owner, group and mode before the service starts, so the socket
// is never reachable by anyone it shouldn't be, not even for a moment.
//
// This is the helper's activation code from stage 3a with the name made a
// parameter, so the controller can use it too.
package sdlisten

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
)

// firstFD is the first descriptor systemd passes (SD_LISTEN_FDS_START).
const firstFD = 3

// Listener returns the one Unix socket systemd passed to this process.
// pid is this process's ID and getenv reads the environment; both are
// parameters so tests can fake them. name only shows up in error
// messages and in the descriptor's name.
func Listener(name string, pid int, getenv func(string) string) (net.Listener, error) {
	listenPID, err := strconv.Atoi(getenv("LISTEN_PID"))
	if err != nil {
		return nil, fmt.Errorf("%s: not socket-activated (LISTEN_PID: %w)", name, err)
	}
	// LISTEN_PID names the process the sockets are meant for. If it's
	// another one (say, our parent), descriptor 3 isn't ours.
	if listenPID != pid {
		return nil, fmt.Errorf("%s: LISTEN_PID is %d, this process is %d", name, listenPID, pid)
	}
	n, err := strconv.Atoi(getenv("LISTEN_FDS"))
	if err != nil {
		return nil, fmt.Errorf("%s: LISTEN_FDS: %w", name, err)
	}
	if n != 1 {
		return nil, fmt.Errorf("%s: expected exactly 1 socket from systemd, got %d", name, n)
	}

	f := os.NewFile(uintptr(firstFD), name)
	if f == nil {
		return nil, errors.New(name + ": descriptor 3 is not open")
	}
	// FileListener duplicates the descriptor, so ours can be closed.
	l, err := net.FileListener(f)
	f.Close()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if _, ok := l.(*net.UnixListener); !ok {
		l.Close()
		return nil, errors.New(name + ": the socket systemd passed is not a Unix socket")
	}
	return l, nil
}
