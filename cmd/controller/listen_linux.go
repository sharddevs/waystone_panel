// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
)

// listenDev creates the API socket at path for dev mode, readable and
// writable by our own user only (0600). Connecting to a Unix socket needs
// write permission on it, so nobody else can even connect.
//
// It refuses if anything already exists at path, rather than deleting it:
// that might be another controller's live socket, or a file that isn't
// ours. A stale socket from a crash has to be removed by hand. Closing
// the listener removes the socket file again.
//
// TODO: Lstat (any result other than fs.ErrNotExist → refuse), then
// net.Listen("unix", path), then os.Chmod(path, 0600); if the chmod
// fails, close the listener and return the error.
func listenDev(path string) (net.Listener, error) {
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

	if err := os.Chmod(path, 0o600); err != nil {
		listener.Close()
		return nil, fmt.Errorf("set socket permissions: %w", err)
	}

	return listener, nil
}
