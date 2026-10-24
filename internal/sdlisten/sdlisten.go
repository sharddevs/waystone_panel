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
	"slices"
	"strconv"
	"strings"
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

// maxNamed bounds how many named sockets Listeners takes over.
const maxNamed = 8

// Listeners returns every socket systemd passed to this process, by the
// names in LISTEN_FDNAMES (each socket unit's FileDescriptorName=). It is
// for a service with more than one socket, like the controller's API and
// agent sockets. Every name must be present, non-empty and unique; anything
// off and nothing is returned (all descriptors closed).
//
// Each socket must be a Unix socket, except those named in tcp, which must
// be TCP (the controller's agent listener, stage 6a).
func Listeners(pid int, getenv func(string) string, tcp ...string) (map[string]net.Listener, error) {
	listenPID, err := strconv.Atoi(getenv("LISTEN_PID"))
	if err != nil {
		return nil, fmt.Errorf("not socket-activated (LISTEN_PID: %w)", err)
	}
	if listenPID != pid {
		return nil, fmt.Errorf("LISTEN_PID is %d, this process is %d", listenPID, pid)
	}
	n, err := strconv.Atoi(getenv("LISTEN_FDS"))
	if err != nil {
		return nil, fmt.Errorf("LISTEN_FDS: %w", err)
	}
	if n < 1 || n > maxNamed {
		return nil, fmt.Errorf("expected 1 to %d sockets from systemd, got %d", maxNamed, n)
	}
	names := strings.Split(getenv("LISTEN_FDNAMES"), ":")
	if len(names) != n {
		return nil, fmt.Errorf("LISTEN_FDNAMES has %d names for %d sockets", len(names), n)
	}

	out := make(map[string]net.Listener, n)
	closeAll := func() {
		for _, l := range out {
			l.Close()
		}
	}
	for i, name := range names {
		if name == "" || out[name] != nil {
			closeAll()
			return nil, fmt.Errorf("socket name %q is empty or repeated", name)
		}
		f := os.NewFile(uintptr(firstFD+i), name)
		if f == nil {
			closeAll()
			return nil, fmt.Errorf("%s: descriptor %d is not open", name, firstFD+i)
		}
		l, err := net.FileListener(f)
		f.Close()
		if err != nil {
			closeAll()
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if slices.Contains(tcp, name) {
			if _, ok := l.(*net.TCPListener); !ok {
				l.Close()
				closeAll()
				return nil, errors.New(name + ": the socket systemd passed is not a TCP socket")
			}
		} else if _, ok := l.(*net.UnixListener); !ok {
			l.Close()
			closeAll()
			return nil, errors.New(name + ": the socket systemd passed is not a Unix socket")
		}
		out[name] = l
	}
	return out, nil
}
