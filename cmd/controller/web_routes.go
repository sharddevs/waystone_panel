// SPDX-License-Identifier: AGPL-3.0-only

package main

import "github.com/sharddevs/waystone_panel/internal/web"

// webRoutes are the controller's browser routes, on top of web's built-in
// /healthz. There are none before stage 5. Every route added here is
// checked against the whole guard by TestWebRoutesCovered (C-W1), and a
// state-changing one can't be registered until stage 5 builds CSRF tokens.
func webRoutes() []web.Route { return nil }
