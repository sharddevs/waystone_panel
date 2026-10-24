// SPDX-License-Identifier: AGPL-3.0-only

//go:build unix

package agent

import (
	"os"
	"syscall"
)

// openStaged reopens a staged jar read-only, never through a link.
func openStaged(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
}
