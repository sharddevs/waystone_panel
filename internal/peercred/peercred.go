// SPDX-License-Identifier: AGPL-3.0-only

// Package peercred asks the kernel which user is on the other end of a
// connected Unix socket (SO_PEERCRED). It is the one identity check of the
// agent link (stage 4b-2, R2): both the controller and the agent use it
// on the connection itself, never on the socket file's owner.
//
// The API (internal/api) and the helper keep their own copies of the same
// syscall; this package only shares it between the two new callers.
package peercred

import "errors"

// ErrUnsupported: the connection isn't a Unix socket, or this system has
// no SO_PEERCRED. Callers must treat it as a refusal, never as a default.
var ErrUnsupported = errors.New("peer credentials need a Unix socket on Linux")
