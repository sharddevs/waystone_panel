// SPDX-License-Identifier: AGPL-3.0-only

//go:build devweb

package webui

import (
	"io/fs"
	"os"
)

// DevDir serves the UI from dir on disk, read again on every request, so
// `npm run build -- --watch` shows up on the next reload (Q13). Only in
// -tags devweb builds.
func DevDir(dir string) (fs.FS, error) { return os.DirFS(dir), nil }

const devBuild = true
