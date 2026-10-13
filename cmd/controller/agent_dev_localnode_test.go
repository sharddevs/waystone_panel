// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux && localnode

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// R2 dev socket: mode 0666, refused if anything is at the path, removed
// on close, and only together with the API's dev socket.
func TestListenAgentDev(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.sock")
	api, agent, err := listeners(filepath.Join(dir, "api.sock"), path, 4242, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o666 {
		t.Errorf("agent socket mode = %v, want a 0666 socket", info.Mode())
	}
	agent.Close()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Errorf("socket left behind after close: %v", err)
	}

	// Something already there: refused, left alone, and the API socket
	// that was already made is closed again.
	if err := os.WriteFile(path, []byte("not ours"), 0o600); err != nil {
		t.Fatal(err)
	}
	apiPath := filepath.Join(dir, "api2.sock")
	if _, _, err := listeners(apiPath, path, 4242, func(string) string { return "" }); err == nil {
		t.Fatal("existing path accepted")
	}
	if b, _ := os.ReadFile(path); string(b) != "not ours" {
		t.Error("existing file was touched")
	}
	if _, err := os.Lstat(apiPath); !os.IsNotExist(err) {
		t.Errorf("API socket left behind after the agent socket failed: %v", err)
	}
}
