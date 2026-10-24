// SPDX-License-Identifier: AGPL-3.0-only

//go:build !unix

package agent

// writeConsole: the console is a systemd FIFO, so only on Linux.
func writeConsole(dir string, owner uint32, serverID, line string) error {
	return errConsoleUnavailable
}
