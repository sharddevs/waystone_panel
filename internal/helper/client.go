// SPDX-License-Identifier: AGPL-3.0-only

package helper

import (
	"context"

	helperv1 "github.com/sharddevs/waystone_panel/gen/waystone/helper/v1"
)

// DefaultSocketPath is the runtime helper's socket (waystone-helper.socket).
const DefaultSocketPath = "/run/waystone-helper/helper.sock"

// Client talks to a helper over its Unix socket, one connection per request.
type Client struct {
	SocketPath string
}

// Do sends req and returns the helper's response. A returned error means the
// exchange itself broke; if the helper refused or the action failed, that
// shows up as the response's ErrorCode instead.
func (c Client) Do(ctx context.Context, req *helperv1.Request) (*helperv1.Response, error) {
	return c.DoInput(ctx, req, nil)
}
