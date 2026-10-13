// SPDX-License-Identifier: AGPL-3.0-only

//go:build !linux

package main

import (
	"errors"
	"net"
)

// listenDev is Linux-only, like the rest of the API's identity checks.
func listenDev(string) (net.Listener, error) {
	return nil, errors.New("the API socket is only supported on Linux")
}
