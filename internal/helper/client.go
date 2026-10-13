// SPDX-License-Identifier: AGPL-3.0-only

package helper

import (
	"context"
	"errors"
	"net"
	"os"

	"google.golang.org/protobuf/proto"

	helperv1 "github.com/sharddevs/waystone_panel/gen/waystone/helper/v1"
)

// DefaultSocketPath is the runtime helper's socket (waystone-helper.socket).
const DefaultSocketPath = "/run/waystone-helper/helper.sock"

// Client talks to a helper over its Unix socket, one connection per request.
type Client struct {
	SocketPath string

	// writeFrame sends the request. nil means WriteFrameFD; tests swap in
	// a write that fails partway.
	writeFrame func(conn *net.UnixConn, m proto.Message, f *os.File) error
	// afterDial, if set, sees the connection right after the dial; tests
	// close it there to make the steps before the write fail.
	afterDial func(net.Conn)
}

// ErrNotSent wraps the error of a request that failed before any write to
// the helper was attempted (a failed dial, for example). Only then is it
// certain the helper did nothing. Any error without it means a write was
// attempted, so the helper may have received the whole request and acted
// on it (stage 4c, S5 and B1).
var ErrNotSent = errors.New("helper request not sent")

// Do sends req and returns the helper's response. A returned error means the
// exchange itself broke; if the helper refused or the action failed, that
// shows up as the response's ErrorCode instead.
func (c Client) Do(ctx context.Context, req *helperv1.Request) (*helperv1.Response, error) {
	return c.DoInput(ctx, req, nil)
}
