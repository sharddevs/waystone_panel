// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"io"
	"log"
)

func discardLogger() *log.Logger { return log.New(io.Discard, "", 0) }
