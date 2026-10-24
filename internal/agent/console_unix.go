// SPDX-License-Identifier: AGPL-3.0-only

//go:build unix

package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// writeConsole writes line and a newline to dir/serverID in one write.
// Every check failing, and every write error, is errConsoleUnavailable:
//
//   - dir is a directory, not a link, owned by owner (root in
//     production), mode 0755, so nobody else can swap the FIFO (H5);
//   - the FIFO is opened write-only, non-blocking and without following a
//     link, so a missing reader fails at once instead of hanging, and
//     fstat says it is a FIFO owned by owner that others can't write;
//   - the write is at most 257 bytes, under PIPE_BUF, so it is atomic: all
//     of it, or EAGAIN when the pipe is full.
//
// A missing FIFO is the usual case for a server provisioned before 7b
// that hasn't restarted since, or one that just stopped.
func writeConsole(dir string, owner uint32, serverID, line string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("%w: %v", errConsoleUnavailable, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || !fi.IsDir() || st.Uid != owner || fi.Mode().Perm() != 0o755 {
		return fmt.Errorf("%w: console directory failed its checks", errConsoleUnavailable)
	}
	// Raw syscalls: os.File would park a non-blocking write in the poller
	// instead of failing.
	fd, err := syscall.Open(filepath.Join(dir, serverID), syscall.O_WRONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("%w: %v", errConsoleUnavailable, err)
	}
	defer syscall.Close(fd)
	var fst syscall.Stat_t
	if err := syscall.Fstat(fd, &fst); err != nil {
		return fmt.Errorf("%w: %v", errConsoleUnavailable, err)
	}
	if fst.Mode&syscall.S_IFMT != syscall.S_IFIFO || fst.Uid != owner || fst.Mode&0o002 != 0 {
		return fmt.Errorf("%w: console FIFO failed its checks", errConsoleUnavailable)
	}
	b := []byte(line + "\n")
	n, err := syscall.Write(fd, b)
	if err != nil {
		return fmt.Errorf("%w: %v", errConsoleUnavailable, err)
	}
	if n != len(b) {
		// Can't happen under PIPE_BUF. If it did, part of the line is in
		// the pipe, and nobody knows what the game makes of it.
		return errors.New("console write cut short")
	}
	return nil
}
