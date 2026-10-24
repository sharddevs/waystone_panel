// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/sharddevs/waystone_panel/internal/password"
	"github.com/sharddevs/waystone_panel/internal/service"
	"github.com/sharddevs/waystone_panel/internal/session"
	"github.com/sharddevs/waystone_panel/internal/store"
	"github.com/sharddevs/waystone_panel/internal/web"
	"github.com/sharddevs/waystone_panel/internal/webui"
)

// C-W1 with the controller's real route set: every browser route the
// controller registers is behind the whole guard.
func TestWebRoutesCovered(t *testing.T) {
	o, err := web.ParsePublicURL("https://panel.example.com")
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := session.New(session.Config{Store: noStore{}, Params: password.Params{Memory: 8, Time: 1, Threads: 1}})
	if err != nil {
		t.Fatal(err)
	}
	// The service has no store: the coverage requests stop at the guard or
	// the session check.
	svc := service.New(service.Config{Log: log.New(io.Discard, "", 0)})
	ui, _, err := uiRoutes("")
	if err != nil {
		t.Fatal(err)
	}
	h, err := web.New(web.Config{Public: o, ProxyUID: 4242, Log: log.New(io.Discard, "", 0), Sessions: sessions}, webRoutes(sessions, svc, ui)...)
	if err != nil {
		t.Fatal(err)
	}
	if err := web.RouteCoverage(h); err != nil {
		t.Fatal(err)
	}
	// Every route, with the CSRF policy it must have and whether it needs
	// a session: the four session routes (5a-2), the browser API (5b,
	// scope §5, the Nodes API of 6b, create/install of 6c and the log of
	// 7a) and the UI
	// (5c, §6). Nothing else may be on web.sock.
	type policy struct {
		csrf    web.CSRFPolicy
		session bool
	}
	want := map[string]policy{
		"GET /healthz":                          {web.CSRFNotApplicable, false},
		"POST /api/v1/session":                  {web.CSRFPreSession, false},
		"GET /api/v1/session":                   {web.CSRFNotApplicable, true},
		"DELETE /api/v1/session":                {web.CSRFRequired, true},
		"PATCH /api/v1/session/preferences":     {web.CSRFRequired, true},
		"GET /api/v1/servers":                   {web.CSRFNotApplicable, true},
		"POST /api/v1/servers":                  {web.CSRFRequired, true},
		"POST /api/v1/servers/{id}/install":     {web.CSRFRequired, true},
		"POST /api/v1/servers/{id}/accept-eula": {web.CSRFRequired, true},
		"POST /api/v1/servers/{id}/command":     {web.CSRFRequired, true},
		"GET /api/v1/servers/{id}":              {web.CSRFNotApplicable, true},
		"GET /api/v1/servers/{id}/logs":         {web.CSRFNotApplicable, true},
		"POST /api/v1/servers/{id}/start":       {web.CSRFRequired, true},
		"POST /api/v1/servers/{id}/stop":        {web.CSRFRequired, true},
		"POST /api/v1/servers/{id}/restart":     {web.CSRFRequired, true},
		"GET /api/v1/operations":                {web.CSRFNotApplicable, true},
		"GET /api/v1/operations/{id}":           {web.CSRFNotApplicable, true},
		"POST /api/v1/operations/{id}/resolve":  {web.CSRFRequired, true},
		"GET /api/v1/nodes":                     {web.CSRFNotApplicable, true},
		"GET /api/v1/nodes/{id}":                {web.CSRFNotApplicable, true},
		"POST /api/v1/nodes/{id}/revoke":        {web.CSRFRequired, true},
		"GET /api/v1/nodes/enrollments":         {web.CSRFNotApplicable, true},
		"POST /api/v1/nodes/enrollments":        {web.CSRFRequired, true},
		"GET /assets/{file...}":                 {web.CSRFNotApplicable, false},
		"GET /{path...}":                        {web.CSRFNotApplicable, false},
	}
	for _, rt := range h.Routes() {
		key := rt.Method + " " + rt.Pattern
		p, ok := want[key]
		if !ok {
			t.Errorf("unexpected route %s", key)
			continue
		}
		if rt.CSRF != p.csrf || rt.Session != p.session {
			t.Errorf("%s: CSRF %v session %v, want %v %v", key, rt.CSRF, rt.Session, p.csrf, p.session)
		}
		delete(want, key)
	}
	for k := range want {
		t.Errorf("route %s missing", k)
	}
}

// noStore is a session store that is never reached: the coverage
// requests carry no cookie and no valid body.
type noStore struct{}

func (noStore) LookupLogin(context.Context, string) (store.LoginRecord, error) {
	return store.LoginRecord{}, errors.New("no store")
}

func (noStore) LookupSession(context.Context, [32]byte) (store.SessionRecord, error) {
	return store.SessionRecord{}, errors.New("no store")
}

func (noStore) InTenantTx(context.Context, store.Actor, func(context.Context, *store.Tx) error) error {
	return errors.New("no store")
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

// Q11 and Q13: without -tags ui the UI is the "UI not built" page and the
// log says so; -dev-ui-dir works only in a devweb build.
func TestUIRoutes(t *testing.T) {
	routes, source, err := uiRoutes("")
	if err != nil || len(routes) != 2 {
		t.Fatalf("uiRoutes: %d routes, %v", len(routes), err)
	}
	if webui.Embedded() == nil && !strings.Contains(source, "not built") {
		t.Errorf("source %q, want it to say the UI isn't built", source)
	}
	_, _, err = uiRoutes(t.TempDir())
	if devweb {
		// An empty directory has no index.html: refused, not served.
		if err == nil {
			t.Error("devweb: an empty -dev-ui-dir was accepted")
		}
	} else if err == nil || !strings.Contains(err.Error(), "devweb") {
		t.Errorf("release build: -dev-ui-dir gave %v, want a refusal naming devweb", err)
	}
}
