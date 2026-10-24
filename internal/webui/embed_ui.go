// SPDX-License-Identifier: AGPL-3.0-only

//go:build ui

package webui

import (
	"embed"
	"io/fs"
)

// dist is the web build (cd web && npm ci && npm run build). A -tags ui
// build doesn't compile without it, so a release binary can't be made
// without the real UI (Q11).
//
//go:embed all:dist
var dist embed.FS

// Embedded returns the UI built into this binary.
func Embedded() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err) // "dist" is a valid path; Sub can't fail
	}
	return sub
}
