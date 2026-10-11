package helper

import (
	"context"
	"net"
	"time"

	helperv1 "github.com/sharddevs/waystone_panel/gen/waystone/helper/v1"
)

// DefaultSocketPath is where the helper's socket unit listens.
const DefaultSocketPath = "/run/waystone-helper/helper.sock"

// Client sends requests to the helper over its Unix socket.
type Client struct {
	SocketPath string
}

// Do sends one request and returns the helper's response. An error means
// the exchange itself failed; a refused or failed action is reported in
// the response's error code.
func (c Client) Do(ctx context.Context, req *helperv1.Request) (*helperv1.Response, error) {
	dialer := net.Dialer{}

	conn, err := dialer.DialContext(ctx, "unix", c.SocketPath)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(ConnTimeout)
	}

	if err := conn.SetDeadline(deadline); err != nil {
		return nil, err
	}
	// The helper refuses an unauthorized caller without reading its request
	// and closes the connection, so our write can fail with "broken pipe"
	// while the refusal is already waiting. That's why we read even after a
	// failed write, and only report the write error if nothing came back.
	writeErr := WriteFrame(conn, req)
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
