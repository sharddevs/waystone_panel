// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"errors"

	"strings"
	"testing"
)

// R2: the agent uid can't be root, the controller itself, or an admin.
func TestCheckAgentUID(t *testing.T) {
	notMember := func(int) (bool, error) { return false, nil }
	member := func(int) (bool, error) { return true, nil }
	unsure := func(int) (bool, error) { return false, errors.New("no such user") }
	cases := []struct {
		name   string
		uid    int
		member func(int) (bool, error)
		ok     bool
	}{
		{"agent", 995, notMember, true},
		{"root", 0, notMember, false},
		{"negative", -2, notMember, false},
		{"too big", 1 << 32, notMember, false},
		{"controller itself", 1000, notMember, false},
		{"admin group member", 995, member, false},
		{"membership unknown", 995, unsure, false},
	}
	for _, c := range cases {
		err := checkAgentUID(c.uid, 1000, c.member)
		if (err == nil) != c.ok {
			t.Errorf("%s: err = %v", c.name, err)
		}
	}
}

// Agent flags that can't work exit 2 before the database is touched.
func TestRunBadAgentFlags(t *testing.T) {
	const node = "0199e1a0-0000-7000-8000-0000000000a1"
	dir := t.TempDir()
	for _, args := range [][]string{
		{"-agent-uid", "995"},
		{"-agent-node", node},
		{"-agent-uid", "0", "-agent-node", node},
		{"-agent-uid", "995", "-agent-node", strings.ToUpper(node)},
		{"-agent-uid", "995", "-agent-node", "node-1"},
		{"-api-socket", dir + "/a.sock", "-agent-socket", dir + "/b.sock"},
	} {
		var stderr strings.Builder
		if code := run(append([]string{"-db", "host=" + dir + " dbname=nope"}, args...), &stderr); code != 2 {
			t.Errorf("run(%q) = %d, want 2", args, code)
		}
		if stderr.Len() == 0 {
			t.Errorf("run(%q) printed nothing", args)
		}
	}
}

// Without -api-socket, -agent-socket is refused: dev sockets come as a
// pair, and production gets both from systemd.
func TestAgentSocketNeedsDevMode(t *testing.T) {
	_, err := listeners("", "", "/tmp/agent.sock", 4242, func(string) string { return "" })
	if err == nil {
		t.Fatal("agent socket without -api-socket was accepted")
	}
}
