// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"io"
	"log"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/sharddevs/waystone_panel/internal/web"
)

// C-W1 with the controller's real route set: every browser route the
// controller registers is behind the whole guard.
func TestWebRoutesCovered(t *testing.T) {
	o, err := web.ParsePublicURL("https://panel.example.com")
	if err != nil {
		t.Fatal(err)
	}
	h, err := web.New(web.Config{Public: o, ProxyUID: 4242, Log: log.New(io.Discard, "", 0)}, webRoutes()...)
	if err != nil {
		t.Fatal(err)
	}
	if err := web.RouteCoverage(h); err != nil {
		t.Fatal(err)
	}
}

// W4, W5, W6: the browser-facing configuration is required and checked
// before the database or any socket is touched. Each case exits 2 with a
// message naming the flag.
func TestRunBadWebFlags(t *testing.T) {
	dir := t.TempDir()
	base := []string{"-db", "host=" + dir + " dbname=nope"}
	good := map[string]string{"-public-url": "https://panel.example.com", "-proxy-uid": "4242"}
	with := func(over map[string]string, drop string) []string {
		args := append([]string(nil), base...)
		for _, k := range []string{"-public-url", "-proxy-uid"} {
			if k == drop {
				continue
			}
			v := good[k]
			if o, ok := over[k]; ok {
				v = o
			}
			args = append(args, k, v)
		}
		for k, v := range over {
			if _, known := good[k]; !known {
				args = append(args, k, v)
			}
		}
		return args
	}
	cases := []struct {
		name   string
		args   []string
		prefix string
	}{
		{"no public URL", with(nil, "-public-url"), "-public-url:"},
		{"http public URL", with(map[string]string{"-public-url": "http://panel.example.com"}, ""), "-public-url:"},
		{"public URL with path", with(map[string]string{"-public-url": "https://panel.example.com/waystone"}, ""), "-public-url:"},
		{"malformed public URL", with(map[string]string{"-public-url": "https://pa nel"}, ""), "-public-url:"},
		{"public URL with user", with(map[string]string{"-public-url": "https://u@panel.example.com"}, ""), "-public-url:"},
		{"no proxy uid", with(nil, "-proxy-uid"), "-proxy-uid is required"},
		{"root proxy", with(map[string]string{"-proxy-uid": "0"}, ""), "-proxy-uid 0"},
		{"proxy is the agent", with(map[string]string{"-proxy-uid": "995", "-agent-uid": "995", "-agent-node": "0199e1a0-0000-7000-8000-0000000000a1"}, ""), "-proxy-uid 995 is the agent's"},
		{"proxy is the controller", with(map[string]string{"-proxy-uid": strconv.Itoa(os.Getuid())}, ""), "-proxy-uid " + strconv.Itoa(os.Getuid())},
		{"HSTS 0", with(map[string]string{"-hsts-max-age": "0"}, ""), "-hsts-max-age 0"},
		{"HSTS too long", with(map[string]string{"-hsts-max-age": "63072001"}, ""), "-hsts-max-age 63072001"},
		{"HSTS negative", with(map[string]string{"-hsts-max-age": "-5"}, ""), "-hsts-max-age -5"},
	}
	for _, c := range cases {
		var stderr strings.Builder
		if code := run(c.args, &stderr); code != 2 || !strings.HasPrefix(stderr.String(), c.prefix) {
			t.Errorf("%s: run(%q) = %d, stderr %q; want 2 starting %q", c.name, c.args, code, stderr.String(), c.prefix)
		}
	}
}

// A proxy uid in range passes; root, the agent's uid, the controller's own
// uid and out-of-range values don't.
func TestCheckProxyUID(t *testing.T) {
	for _, c := range []struct {
		uid, agent, self int
		ok               bool
	}{
		{4242, 995, 981, true}, {4242, -1, 981, true}, {0, 995, 981, false}, {-1, 995, 981, false},
		{995, 995, 981, false}, {981, 995, 981, false}, {1 << 32, 995, 981, false},
	} {
		if err := checkProxyUID(c.uid, c.agent, c.self); (err == nil) != c.ok {
			t.Errorf("checkProxyUID(%d, %d, %d) = %v", c.uid, c.agent, c.self, err)
		}
	}
}
