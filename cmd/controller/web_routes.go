// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"github.com/sharddevs/waystone_panel/internal/browser"
	"github.com/sharddevs/waystone_panel/internal/service"
	"github.com/sharddevs/waystone_panel/internal/session"
	"github.com/sharddevs/waystone_panel/internal/web"
)

// webRoutes are the controller's browser routes, on top of web's built-in
// /healthz: sign-in and the session (5a-2), then the servers and
// operations (5b). Every route added here is checked against the whole
// guard by TestWebRoutesCovered (C-W1).
func webRoutes(sessions *session.Manager, svc *service.Service) []web.Route {
	return append(sessions.Routes(), browser.Routes(svc)...)
}
