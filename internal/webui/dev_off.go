// SPDX-License-Identifier: AGPL-3.0-only

//go:build !devweb

package webui

import (
	"errors"
	"io/fs"
)

// DevDir is refused in release builds: they only serve the embedded UI.
func DevDir(string) (fs.FS, error) {
	return nil, errors.New("-dev-ui-dir needs a controller built with -tags devweb")
}

const devBuild = false
