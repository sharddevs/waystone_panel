// SPDX-License-Identifier: AGPL-3.0-only

package helper

import (
	"fmt"
	"net"
	"os"

	helperv1 "github.com/sharddevs/waystone_panel/gen/waystone/helper/v1"
	"github.com/sharddevs/waystone_panel/internal/worker"
)

// MaxInstallBytes is the biggest file INSTALL_FILE accepts. The worker
// checks the same limit again while it copies, because the file can still
// change after our Stat and the worker may only see a pipe.
const MaxInstallBytes = worker.MaxInstallBytes

// CheckInput decides whether the descriptor that came with a request (nil if
// none did) is allowed. INSTALL_FILE needs a regular file no bigger than
// MaxInstallBytes; every other action must come without one. Errors wrap
// ErrInvalidRequest.
//
// It only calls Stat. It never reads or closes input; closing it is the
// Server's job.
func CheckInput(action helperv1.Action, input *os.File) error {
	if action != helperv1.Action_ACTION_INSTALL_FILE {
		if input != nil {
			return fmt.Errorf("%w: unexpected input descriptor", ErrInvalidRequest)
		}
		return nil
	}

	if input == nil {
		return fmt.Errorf("%w: missing input descriptor", ErrInvalidRequest)
	}

	info, err := input.Stat()
	if err != nil {
		return fmt.Errorf("%w: stat input descriptor: %v", ErrInvalidRequest, err)
	}

	// Pipes, sockets, directories and devices are refused: only a regular file
	// has a size we can check before the worker runs.
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: input must be a regular file", ErrInvalidRequest)
	}

	if info.Size() > MaxInstallBytes {
		return fmt.Errorf("%w: input exceeds size limit", ErrInvalidRequest)
	}

	return nil
}

// readRequest reads one request into req. On a Unix socket it also returns
// the descriptor that came with it, if any. Other kinds of net.Conn can't
// carry a descriptor, so for them it's always nil.
func readRequest(conn net.Conn, req *helperv1.Request) (*os.File, error) {
	if unixConn, ok := conn.(*net.UnixConn); ok {
		return ReadFrameFD(unixConn, req)
	}

	if err := ReadFrame(conn, req); err != nil {
		return nil, err
	}

	return nil, nil
}
