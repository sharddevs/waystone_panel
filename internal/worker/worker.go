// SPDX-License-Identifier: AGPL-3.0-only

// Package worker reads and replaces small files in one server's directory
// for waystone-worker. It runs as the server's own user and treats every
// file in that directory as untrusted, since the game server can change them.
// It knows nothing about any game.
package worker

import "errors"

var (
	// ErrInvalid means a name, hash or content given to the worker was bad.
	ErrInvalid = errors.New("invalid worker input")
	// ErrUnsupported means the file is something the worker won't touch:
	// a symlink, a directory or other non-regular file, or one over
	// MaxFileBytes. Nothing was changed.
	ErrUnsupported = errors.New("unsupported file")
	// ErrChanged means the file is no longer what the caller expected, so
	// nothing was replaced. Read it again and decide again.
	ErrChanged = errors.New("file changed since it was read")
)

// Size limits. MaxFileBytes leaves room for a whole file plus the rest of the
// request in one 64 KiB helper frame. A real server.properties is
// about 1.5 KiB.
const (
	MaxFileBytes = 48 << 10
	MaxNameBytes = 64
)
