// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux && localnode

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
)

// listenAgentDev creates the dev agent socket (R2, decision 4): like
// listenDev, it refuses anything already at path, but the mode is 0666.
// The agent runs as another user, and our user can't give a socket to
// group waystone-agent, which must stay empty. Anyone may connect; the
// handshake then refuses every uid but -agent-uid before reading a byte.
//
// Only in -tags localnode builds (decision F). Closing the listener
// removes the socket file.
func listenAgentDev(path string) (net.Listener, error) {
	_, err := os.Lstat(path)
	if err == nil {
		return nil, fmt.Errorf("socket path already exists: %s", path)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("check socket path: %w", err)
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on Unix socket: %w", err)
	}
	if err := os.Chmod(path, 0o666); err != nil {
		listener.Close()
		return nil, fmt.Errorf("set socket permissions: %w", err)
	}
	return listener, nil
}
