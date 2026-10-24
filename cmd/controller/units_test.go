// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sharddevs/waystone_panel/internal/agentlink"
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
// refuses anything but these three names. A unit that drifts from the code
// would only show up on the VM, so this pins them together.
func TestSocketUnits(t *testing.T) {
	cases := []struct {
		file, name, path, group string
	}{
		{"waystone-controller-api.socket", apiSocketName, "/run/waystone-controller/api.sock", adminGroup},
		{"waystone-controller-agent.socket", agentSocketName, "/run/waystone-controller/agent.sock", "waystone-agent"},
		{"waystone-controller-web.socket", webSocketName, "/run/waystone-controller/web.sock", "waystone-web"},
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
			"Install.WantedBy":          "sockets.target",
		}
		for k, v := range want {
			if u[k] != v {
				t.Errorf("%s: %s = %q, want %q", c.file, k, u[k], v)
			}
		}
	}
}

// The service must name exactly the three sockets, run as its own user,
// and its ExecStart must be a command line the controller accepts, with
// each env value reaching the flag it's meant for.
func TestControllerServiceUnit(t *testing.T) {
	u := readUnit(t, "waystone-controller.service")
	want := map[string]string{
		"Service.User":            "waystone-controller",
		"Service.Group":           "waystone-controller",
		"Service.Sockets":         "waystone-controller-api.socket waystone-controller-web.socket waystone-controller-agent.socket",
		"Unit.Requires":           "waystone-controller-api.socket waystone-controller-web.socket waystone-controller-agent.socket",
		"Service.Environment":     "WAYSTONE_HSTS_MAX_AGE=300",
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
	// Fill in the env values with one value broken at a time. run then
	// stops at that value's own check, after every flag has parsed and
	// before anything connects, which proves the value reached the right
	// flag. An unknown flag would fail earlier, differently.
	good := map[string]string{
		"${WAYSTONE_AGENT_UID}":    "995",
		"${WAYSTONE_NODE_ID}":      "0199e1a0-0000-7000-8000-0000000000a1",
		"${WAYSTONE_PUBLIC_URL}":   "https://panel.example.com",
		"${WAYSTONE_PROXY_UID}":    "4242",
		"${WAYSTONE_HSTS_MAX_AGE}": "300",
	}
	for _, c := range []struct{ key, bad, prefix string }{
		{"${WAYSTONE_NODE_ID}", "not-a-uuid", "-agent-node:"},
		{"${WAYSTONE_PUBLIC_URL}", "http://panel.example.com", "-public-url:"},
		{"${WAYSTONE_PROXY_UID}", "0", "-proxy-uid 0"},
		{"${WAYSTONE_HSTS_MAX_AGE}", "0", "-hsts-max-age 0"},
	} {
		var pairs []string
		for k, v := range good {
			if k == c.key {
				v = c.bad
			}
			pairs = append(pairs, k, v)
		}
		args := strings.NewReplacer(pairs...).Replace(strings.Join(exec[1:], " "))
		if strings.Contains(args, "$") {
			t.Fatalf("ExecStart uses an env value this test doesn't know: %q", args)
		}
		var stderr bytes.Buffer
		if code := run(strings.Fields(args), &stderr); code != 2 || !strings.HasPrefix(stderr.String(), c.prefix) {
			t.Errorf("%s=%s: run(%q) = %d, stderr %q; want 2 from %q", c.key, c.bad, args, code, stderr.String(), c.prefix)
		}
	}
}

// 6a, 6M4: the TCP agent listener is its own socket unit, added by a
// drop-in, and the controller itself keeps no network of its own.
func TestAgentTCPUnits(t *testing.T) {
	s := readUnit(t, "waystone-controller-agent-tcp.socket")
	for k, v := range map[string]string{
		"Socket.FileDescriptorName": agentTCPSocketName,
		"Socket.Service":            "waystone-controller.service",
		"Socket.Accept":             "no",
		"Install.WantedBy":          "sockets.target",
	} {
		if s[k] != v {
			t.Errorf("tcp socket: %s = %q, want %q", k, s[k], v)
		}
	}
	if addr := s["Socket.ListenStream"]; !strings.Contains(addr, ":") || strings.HasPrefix(addr, "0.0.0.0") || strings.HasPrefix(addr, "[::]") || strings.HasPrefix(addr, "/") {
		t.Errorf("tcp socket: ListenStream = %q, want one LAN address and port", addr)
	}

	d := readUnit(t, "waystone-controller.service.d/agent-tcp.conf")
	for k, v := range map[string]string{
		"Service.Sockets": "waystone-controller-agent-tcp.socket",
		"Unit.Requires":   "waystone-controller-agent-tcp.socket",
		"Unit.After":      "waystone-controller-agent-tcp.socket",
	} {
		if d[k] != v {
			t.Errorf("drop-in: %s = %q, want %q", k, d[k], v)
		}
	}
	if len(d) != 3 {
		t.Errorf("drop-in sets %d keys, want only these 3: %v", len(d), d)
	}

	u := readUnit(t, "waystone-controller.service")
	for k, v := range map[string]string{
		"Service.PrivateNetwork":          "yes",
		"Service.RestrictAddressFamilies": "AF_UNIX",
		"Service.StateDirectory":          "waystone-controller waystone-controller/artifacts",
		"Service.StateDirectoryMode":      "0700",
		"Service.SupplementaryGroups":     "waystone-fetch",
		"Unit.Wants":                      "waystone-fetch.socket",
	} {
		if u[k] != v {
			t.Errorf("controller: %s = %q, want %q", k, u[k], v)
		}
	}
	if agentlink.DefaultArtifactDir != "/var/lib/waystone-controller/artifacts" {
		t.Errorf("the artifact cache %s isn't the unit's StateDirectory", agentlink.DefaultArtifactDir)
	}
}
