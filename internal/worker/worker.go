// SPDX-License-Identifier: AGPL-3.0-only

// Package worker holds what waystone-worker does inside one server's
// directory. It runs as that server's own user, never as root, and it
// treats every file already in the directory as untrusted: the game server
// can change any of them.
//
// The worker is game-neutral. It reads and replaces small files by name;
// what a file means (server.properties, eula.txt) is the agent's profile
// code, never the worker's or the helper's.
package worker

import "errors"

var (
	// ErrInvalid means the input to the worker failed validation.
	ErrInvalid = errors.New("invalid worker input")
	// ErrUnsupported means a file in the server directory has a shape the
	// worker refuses to read, follow or replace: a symlink, a directory or
	// another non-regular file, or a file larger than MaxFileBytes.
	// Nothing was changed.
	ErrUnsupported = errors.New("unsupported file")
	// ErrChanged means the file is no longer what the caller expected.
	// Nothing was replaced; reading again and deciding again is safe.
	ErrChanged = errors.New("file changed since it was read")
)

// Limits. MaxFileBytes keeps a whole file plus the request around it inside
// one helper frame (64 KiB); a real server.properties is about 1.5 KiB.
const (
	MaxFileBytes = 48 << 10
	MaxNameBytes = 64
)
