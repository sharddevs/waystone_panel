// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sharddevs/waystone_panel/internal/agent"
	"github.com/sharddevs/waystone_panel/internal/helper"
)

// readUnit returns a unit file's Key=Value lines, keyed by "Section.Key".
// The same small reader as in cmd/controller: no line continuations, and a
// key that appears twice fails the test.
func readUnit(t *testing.T, name string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "deploy", "systemd", name))
	if err != nil {
		t.Fatal(err)
	}
	unit := map[string]string{}
	section := ""
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "" || strings.HasPrefix(line, "#"):
		case strings.HasPrefix(line, "["):
			section = strings.Trim(line, "[]")
		default:
			k, v, ok := strings.Cut(line, "=")
			if !ok {
				t.Fatalf("%s: not Key=Value: %q", name, line)
			}
			key := section + "." + k
			if _, dup := unit[key]; dup {
				t.Fatalf("%s: %s set twice", name, key)
			}
			unit[key] = v
		}
	}
	return unit
}

// The installed agent must parse its own ExecStart, connect to the socket
// the controller's agent socket unit creates, and use the helper socket
// the helper's unit creates.
func TestAgentServiceUnit(t *testing.T) {
	u := readUnit(t, "waystone-agent.service")
	want := map[string]string{
		"Service.User":            "waystone-agent",
		"Service.Group":           "waystone-agent",
		"Service.EnvironmentFile": "/etc/waystone/agent.env",
		"Service.Restart":         "always",
	}
	for k, v := range want {
		if u[k] != v {
			t.Errorf("%s = %q, want %q", k, u[k], v)
		}
	}

	exec := strings.Fields(u["Service.ExecStart"])
	if len(exec) == 0 || exec[0] != "/usr/local/bin/waystone-agent" {
		t.Fatalf("ExecStart = %q", u["Service.ExecStart"])
	}
	args := strings.NewReplacer(
		"${WAYSTONE_CONTROLLER_UID}", "996",
		"${WAYSTONE_NODE_ID}", testNode,
	).Replace(strings.Join(exec[1:], " "))
	if strings.Contains(args, "$") {
		t.Fatalf("ExecStart uses an env value this test doesn't know: %q", args)
	}
	cfg, err := parse(strings.Fields(args), 995, io.Discard)
	if err != nil {
		t.Fatalf("parse(%q): %v", args, err)
	}

	agentSocket := readUnit(t, "waystone-controller-agent.socket")["Socket.ListenStream"]
	if cfg.controller != agentSocket {
		t.Errorf("-controller = %q, the agent socket unit listens on %q", cfg.controller, agentSocket)
	}
	if cfg.controllerUID != 996 || cfg.node != testNode {
		t.Errorf("parsed %+v", cfg)
	}
	provSocket := readUnit(t, "waystone-provisioner.socket")["Socket.ListenStream"]
	if cfg.provisioner != provSocket || helper.DefaultProvisionerSocketPath != provSocket {
		t.Errorf("provisioner socket %q (default %q), its unit listens on %q", cfg.provisioner, helper.DefaultProvisionerSocketPath, provSocket)
	}
	// The staging directory is the agent's default, and the unit makes it.
	if u["Service.StateDirectory"] != "waystone-agent waystone-agent/staging" || u["Service.StateDirectoryMode"] != "0700" ||
		agent.DefaultStagingDir != "/var/lib/waystone-agent/staging" {
		t.Errorf("StateDirectory %q mode %q, staging %q", u["Service.StateDirectory"], u["Service.StateDirectoryMode"], agent.DefaultStagingDir)
	}
	helperSocket := readUnit(t, "waystone-helper.socket")["Socket.ListenStream"]
	if cfg.helper != helperSocket || helper.DefaultSocketPath != helperSocket {
		t.Errorf("helper socket %q (default %q), the helper's unit listens on %q", cfg.helper, helper.DefaultSocketPath, helperSocket)
	}
}
