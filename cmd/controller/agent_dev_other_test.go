// SPDX-License-Identifier: AGPL-3.0-only

//go:build !linux || !localnode

package main

import (
	"path/filepath"
	"testing"
)

// Decision F: without -tags localnode the controller refuses -agent-socket.
func TestNoDevAgentSocketWithoutTag(t *testing.T) {
	dir := t.TempDir()
	if _, err := listeners(filepath.Join(dir, "api.sock"), filepath.Join(dir, "web.sock"), filepath.Join(dir, "agent.sock"), 4242,
		func(string) string { return "" }); err == nil {
		t.Fatal("-agent-socket accepted in a build without localnode")
	}
}
