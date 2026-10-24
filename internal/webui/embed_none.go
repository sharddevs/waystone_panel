// SPDX-License-Identifier: AGPL-3.0-only

//go:build !ui

package webui

import "io/fs"

// Embedded returns nil: this binary was built without -tags ui, so it
// serves the "UI not built" page (Q11). go build, go vet and the Windows
// cross-build work without Node.
func Embedded() fs.FS { return nil }
