// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readUnit returns a unit file's Key=Value lines, keyed by "Section.Key".
// It's only as much of the systemd format as the files in deploy/ use: no
// line continuations, and a key that appears twice fails the test.
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

// The socket units are what systemd hands the controller, and listeners()
// refuses anything but these two names. A unit that drifts from the code
// would only show up on the VM, so this pins them together.
func TestSocketUnits(t *testing.T) {
	cases := []struct {
		file, name, path, group string
	}{
		{"waystone-controller-api.socket", apiSocketName, "/run/waystone-controller/api.sock", adminGroup},
		{"waystone-controller-agent.socket", agentSocketName, "/run/waystone-controller/agent.sock", "waystone-agent"},
	}
	for _, c := range cases {
		u := readUnit(t, c.file)
		want := map[string]string{
			"Socket.ListenStream":       c.path,
			"Socket.FileDescriptorName": c.name,
			"Socket.Service":            "waystone-controller.service",
			"Socket.SocketUser":         "root",
			"Socket.SocketGroup":        c.group,
			"Socket.SocketMode":         "0660",
			"Socket.DirectoryMode":      "0755",
			"Socket.Accept":             "no",
		}
		for k, v := range want {
			if u[k] != v {
				t.Errorf("%s: %s = %q, want %q", c.file, k, u[k], v)
			}
		}
	}
}

// The service must name exactly the two sockets, run as its own user, and
// its ExecStart must be a command line the controller accepts.
func TestControllerServiceUnit(t *testing.T) {
	u := readUnit(t, "waystone-controller.service")
	want := map[string]string{
		"Service.User":            "waystone-controller",
		"Service.Group":           "waystone-controller",
		"Service.Sockets":         "waystone-controller-api.socket waystone-controller-agent.socket",
		"Service.EnvironmentFile": "/etc/waystone/controller.env",
		"Service.Restart":         "on-failure",
	}
	for k, v := range want {
		if u[k] != v {
			t.Errorf("%s = %q, want %q", k, u[k], v)
		}
	}

	exec := strings.Fields(u["Service.ExecStart"])
	if len(exec) == 0 || exec[0] != "/usr/local/bin/waystone-controller" {
		t.Fatalf("ExecStart = %q", u["Service.ExecStart"])
	}
	// Fill in the env values, with a node ID that isn't a UUID: run then
	// stops at the node check, after every flag has parsed and before
	// anything connects. An unknown flag would fail earlier, differently.
	args := strings.NewReplacer(
		"${WAYSTONE_AGENT_UID}", "995",
		"${WAYSTONE_NODE_ID}", "not-a-uuid",
	).Replace(strings.Join(exec[1:], " "))
	if strings.Contains(args, "$") {
		t.Fatalf("ExecStart uses an env value this test doesn't know: %q", args)
	}
	var stderr bytes.Buffer
	if code := run(strings.Fields(args), &stderr); code != 2 || !strings.HasPrefix(stderr.String(), "-agent-node:") {
		t.Fatalf("run(%q) = %d, stderr %q; want 2 from the node check", args, code, stderr.String())
	}
}
