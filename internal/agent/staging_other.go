// SPDX-License-Identifier: AGPL-3.0-only

//go:build !unix

package agent

import (
	"errors"
	"os"
)

// openStaged: installs run on Linux only (v0.1).
func openStaged(string) (*os.File, error) {
	return nil, errors.New("installs run on Unix only")
}
