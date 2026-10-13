// SPDX-License-Identifier: AGPL-3.0-only

package helper

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"

	helperv1 "github.com/sharddevs/waystone_panel/gen/waystone/helper/v1"
)

// DoInput is Do with an optional file whose descriptor travels with the
// request. input can be nil. DoInput never closes it; that is the caller's job.
func (c Client) DoInput(ctx context.Context, req *helperv1.Request, input *os.File) (*helperv1.Response, error) {
	dialer := net.Dialer{}

	conn, err := dialer.DialContext(ctx, "unix", c.SocketPath)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNotSent, err)
	}
	defer conn.Close()
	if c.afterDial != nil {
		c.afterDial(conn)
	}

	// Without a deadline on ctx we use ConnTimeout, the same limit the
	// helper puts on us, so a stuck helper can't hang the caller forever.
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(ConnTimeout)
	}

	if err := conn.SetDeadline(deadline); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNotSent, err)
	}

	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return nil, fmt.Errorf("%w: expected Unix socket connection", ErrNotSent)
	}

	// From here on a write has been attempted, so no error below is
	// ErrNotSent: even a failed write may have delivered the whole frame.
	writeFrame := c.writeFrame
	if writeFrame == nil {
		writeFrame = WriteFrameFD
	}

	// The helper can refuse us before it reads the request, and then our
	// write may fail. We still try to read, because the refusal it sent
	// says more than a broken pipe does.
	writeErr := writeFrame(unixConn, req, input)

	resp := new(helperv1.Response)
	readErr := ReadFrame(conn, resp)
	if readErr == nil {
		return resp, nil
	}

	if writeErr != nil {
		return nil, writeErr
	}

	return nil, readErr
}
