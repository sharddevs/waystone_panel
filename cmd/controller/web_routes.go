// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"github.com/sharddevs/waystone_panel/internal/browser"
	"github.com/sharddevs/waystone_panel/internal/service"
	"github.com/sharddevs/waystone_panel/internal/session"
	"github.com/sharddevs/waystone_panel/internal/web"
	"github.com/sharddevs/waystone_panel/internal/webui"
)

// webRoutes are the controller's browser routes, on top of web's built-in
// /healthz: sign-in and the session (5a-2), the servers and operations
// (5b), then the UI's files and pages (5c, webui.Routes). Every route
// added here is checked against the whole guard by TestWebRoutesCovered
// (C-W1).
func webRoutes(sessions *session.Manager, svc *service.Service, ui []web.Route) []web.Route {
	routes := append(sessions.Routes(), browser.Routes(svc)...)
	return append(routes, ui...)
}

// uiRoutes picks the web UI (5c, Q11, Q13): -dev-ui-dir in a devweb
// build, else the one built in with -tags ui, else the "UI not built"
// page. It also says which, for the startup log. A UI that is there but
// unusable (no index.html, or one the CSP would break) is an error, so
// the controller refuses to start rather than serve a broken page.
func uiRoutes(devDir string) ([]web.Route, string, error) {
	files, source := webui.Embedded(), "embedded"
	if devDir != "" {
		dev, err := webui.DevDir(devDir)
		if err != nil {
			return nil, "", err
		}
		files, source = dev, "from "+devDir+" (devweb)"
	}
	if files == nil {
		source = "not built into this binary (build with -tags ui; see deploy/README.md)"
	}
	routes, err := webui.Routes(files)
	if err != nil {
		return nil, "", err
	}
	return routes, source, nil
}
