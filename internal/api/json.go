// SPDX-License-Identifier: AGPL-3.0-only

package api

import "github.com/sharddevs/waystone_panel/internal/view"

// The JSON shapes the API sends live in internal/view, shared with the
// browser socket (E1). These names keep the handlers and tests short.
type (
	actorJSON     = view.Actor
	operationJSON = view.Operation
	serverJSON    = view.Server
)
