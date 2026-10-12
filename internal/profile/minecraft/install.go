// SPDX-License-Identifier: AGPL-3.0-only

package minecraft

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"

	"github.com/sharddevs/waystone_panel/internal/lifecycle"
)

// ServerJarFile is the jar the server's systemd unit runs.
const ServerJarFile = "server.jar"

// Installer is the one method InstallServerJar needs. Like Files, it is
// kept small so tests can fake it.
type Installer interface {
	InstallFile(ctx context.Context, serverID, operationID, name string, content *os.File, sum [32]byte) error
}

// The build fails here if lifecycle.Runtime stops satisfying Installer.
var _ Installer = lifecycle.Runtime(nil)

// InstallServerJar installs jar as the server's server.jar. sum is the
// SHA-256 the jar must have; the worker refuses anything else. This package
// only picks the file name, the Installer does the rest.
func InstallServerJar(
	ctx context.Context,
	i Installer,
	serverID, operationID string,
	jar *os.File,
	sum [sha256.Size]byte,
) error {
	if err := i.InstallFile(ctx, serverID, operationID, ServerJarFile, jar, sum); err != nil {
		return fmt.Errorf("install server.jar: %w", err)
	}

	return nil
}
