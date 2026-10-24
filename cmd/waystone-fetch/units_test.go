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

	"github.com/sharddevs/waystone_panel/internal/fetch"
)

// readUnit returns a unit file's Key=Value lines, keyed by "Section.Key",
// as in cmd/controller.
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

// The socket is the controller's alone, one connection at a time; the
// service is the connection, with network and nothing else.
func TestFetchUnits(t *testing.T) {
	s := readUnit(t, "waystone-fetch.socket")
	for k, v := range map[string]string{
		"Socket.ListenStream":   fetch.DefaultSocket,
		"Socket.SocketUser":     "root",
		"Socket.SocketGroup":    "waystone-controller",
		"Socket.SocketMode":     "0660",
		"Socket.Accept":         "yes",
		"Socket.MaxConnections": "1",
	} {
		if s[k] != v {
			t.Errorf("socket: %s = %q, want %q", k, s[k], v)
		}
	}
	u := readUnit(t, "waystone-fetch@.service")
	for k, v := range map[string]string{
		"Service.User":                    "waystone-fetch",
		"Service.Group":                   "waystone-fetch",
		"Service.StandardInput":           "socket",
		"Service.StandardOutput":          "socket",
		"Service.StateDirectory":          "waystone-fetch",
		"Service.StateDirectoryMode":      "0750",
		"Service.UMask":                   "0027",
		"Service.RestrictAddressFamilies": "AF_INET AF_INET6 AF_UNIX",
		"Service.NoNewPrivileges":         "yes",
		"Service.CapabilityBoundingSet":   "",
		"Service.ProtectSystem":           "strict",
		"Service.IPAddressDeny":           "localhost link-local multicast",
		"Service.IPAddressAllow":          "127.0.0.53/32",
		"Service.ProtectProc":             "invisible",
		"Service.ProcSubset":              "pid",
		"Service.PrivateUsers":            "yes",
		"Service.RemoveIPC":               "yes",
		"Service.KeyringMode":             "private",
	} {
		if u[k] != v {
			t.Errorf("service: %s = %q, want %q", k, u[k], v)
		}
	}
	for _, k := range []string{"Service.EnvironmentFile", "Service.ReadWritePaths", "Service.PrivateNetwork"} {
		if _, ok := u[k]; ok {
			t.Errorf("service sets %s", k)
		}
	}
	exec := strings.Fields(u["Service.ExecStart"])
	if len(exec) != 3 || exec[0] != "/usr/local/bin/waystone-fetch" || exec[1] != "-dir" || exec[2] != fetch.DefaultDir ||
		"/var/lib/"+u["Service.StateDirectory"] != fetch.DefaultDir {
		t.Errorf("ExecStart %q, StateDirectory %q", u["Service.ExecStart"], u["Service.StateDirectory"])
	}
	if code := run(exec[1:], 1000, strings.NewReader("x\n"), io.Discard, io.Discard); code != 2 {
		t.Errorf("ExecStart's flags: exit %d", code)
	}
}
