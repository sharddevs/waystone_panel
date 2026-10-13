// SPDX-License-Identifier: AGPL-3.0-only

//go:build !linux || !localnode

package main

import (
	"errors"
	"net"
)

// Without -tags localnode the controller has no self-made agent socket
// (decision F): production gets it from systemd.
func listenAgentDev(string) (net.Listener, error) {
	return nil, errors.New("-agent-socket needs a Linux controller built with -tags localnode")
}
