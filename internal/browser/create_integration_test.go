// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package browser

// Creating, installing and the EULA (6c-1) on both sockets: the same
// JSON (E1), the refusals with no row left behind, and who may.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sharddevs/waystone_panel/internal/service"
	"github.com/sharddevs/waystone_panel/internal/store"
)

func createBody(name string, port int) string {
	b, _ := json.Marshal(map[string]any{"node_id": nodeA, "name": name, "port": port, "heap_mib": 2048})
	return string(b)
}

func created(t *testing.T, w *httptest.ResponseRecorder) service.CreatedServer {
	t.Helper()
	var c service.CreatedServer
	if err := json.Unmarshal(w.Body.Bytes(), &c); err != nil || c.Server.ID == "" || c.Operation.ID == "" {
		t.Fatalf("not a created server: %d %s", w.Code, w.Body.String())
	}
	return c
}

func TestCreateSameJSONOnBothSockets(t *testing.T) {
	var kicked []store.Operation
	e := setupWith(t, func(c *service.Config) {
		c.OnOperation = func(op store.Operation) { kicked = append(kicked, op) }
		c.AgentLink = &service.AgentLink{Addr: testLinkAddr, Pin: testLinkPin}
	})
	trevor := e.signIn(t, "trevor")
	tj := actorJSON(e.trevor)
	post := func(body string) req { return req{method: "POST", path: "/api/v1/servers", body: body} }
	for _, c := range []struct {
		name string
		q    req
	}{
		{"bad node ID", post(`{"node_id":"x","name":"a","port":25600,"heap_mib":2048}`)},
		{"other tenant's node", post(strings.Replace(createBody("a", 25600), nodeA, nodeB, 1))},
		{"missing node", post(strings.Replace(createBody("a", 25600), nodeA, missing, 1))},
		{"port 80", post(createBody("a", 80))},
		{"port 70000", post(createBody("a", 70000))},
		{"the agent port", post(createBody("a", 7443))},
		{"heap 100", post(`{"node_id":"` + nodeA + `","name":"a","port":25600,"heap_mib":100}`)},
		{"no name", post(createBody("", 25600))},
		{"motd backslash", post(`{"node_id":"` + nodeA + `","name":"a","port":25600,"heap_mib":2048,"motd":"a\\b"}`)},
		{"motd padded", post(`{"node_id":"` + nodeA + `","name":"a","port":25600,"heap_mib":2048,"motd":" a"}`)},
		{"unknown field", post(`{"node_id":"` + nodeA + `","name":"a","port":25600,"heap_mib":2048,"version":"1.8"}`)},
		{"port as a string", post(`{"node_id":"` + nodeA + `","name":"a","port":"25600","heap_mib":2048}`)},
		{"install the fixture (unknown state)", req{method: "POST", path: "/api/v1/servers/" + serverA1 + "/install"}},
		{"eula on a fixture (accepted)", req{method: "POST", path: "/api/v1/servers/" + serverA1 + "/accept-eula"}},
		{"install other tenant's", req{method: "POST", path: "/api/v1/servers/" + serverB1 + "/install"}},
	} {
		aw, ww := e.onAPI(1000, c.q), e.onWeb(trevor, c.q)
		same(t, c.name, aw, ww, tj)
		if aw.Code < 400 {
			t.Errorf("%s: %d %s", c.name, aw.Code, aw.Body.String())
		}
	}
	if n := e.count(t, `SELECT count(*) FROM waystone.servers`); n != 3 {
		t.Fatalf("%d servers after refusals, want 3", n)
	}
	if n := e.count(t, `SELECT count(*) FROM waystone.operations`); n != 0 || len(kicked) != 0 {
		t.Fatalf("%d operations, %d kicks after refusals", n, len(kicked))
	}

	// One create on each socket, on its own port.
	aw := e.onAPI(1000, req{method: "POST", path: "/api/v1/servers", body: createBody("new", 25600), key: "c-1"})
	ww := e.onWeb(trevor, req{method: "POST", path: "/api/v1/servers", body: createBody("new", 25601), key: "c-1"})
	if aw.Code != http.StatusCreated {
		t.Fatalf("api create: %d %s", aw.Code, aw.Body.String())
	}
	ac, wc := created(t, aw), created(t, ww)
	if len(kicked) != 2 || kicked[0].ID != ac.Operation.ID {
		t.Errorf("kicks %+v", kicked)
	}
	if aw.Header().Get("Location") != "/api/v1/servers/"+ac.Server.ID {
		t.Errorf("Location %q", aw.Header().Get("Location"))
	}
	s := ac.Server
	if s.Installed || s.EulaAccepted || s.Port == nil || *s.Port != 25600 || s.HeapMiB == nil || *s.HeapMiB != 2048 ||
		s.Profile.Version == nil || *s.Profile.Version != "26.3" || ac.Operation.Action != "server.install" || ac.Operation.State != "pending" {
		t.Errorf("created %s", aw.Body.String())
	}
	ids := []string{ac.Server.ID, "SERVER", wc.Server.ID, "SERVER", ac.Operation.ID, "OP", wc.Operation.ID, "OP", "25600", "PORT", "25601", "PORT"}
	same(t, "create", aw, ww, tj, ids...)
	// A replay is 200 with the same server, on both, and no second kick.
	same(t, "replay",
		e.onAPI(1000, req{method: "POST", path: "/api/v1/servers", body: createBody("new", 25600), key: "c-1"}),
		e.onWeb(trevor, req{method: "POST", path: "/api/v1/servers", body: createBody("new", 25601), key: "c-1"}), tj, ids...)
	if len(kicked) != 2 {
		t.Errorf("a replay kicked the agent")
	}
	// The same port again, on both.
	same(t, "port taken",
		e.onAPI(1000, req{method: "POST", path: "/api/v1/servers", body: createBody("dup", 25600)}),
		e.onWeb(trevor, req{method: "POST", path: "/api/v1/servers", body: createBody("dup", 25600)}), tj)
	// Start while installing, then once the install failed: not installed.
	same(t, "start while installing",
		e.onAPI(1000, req{method: "POST", path: "/api/v1/servers/" + ac.Server.ID + "/start"}),
		e.onWeb(trevor, req{method: "POST", path: "/api/v1/servers/" + wc.Server.ID + "/start"}), tj, ids...)
	e.setState(t, ac.Operation.ID, "running", "succeeded")
	e.setState(t, wc.Operation.ID, "running", "succeeded")
	aw = e.onAPI(1000, req{method: "POST", path: "/api/v1/servers/" + ac.Server.ID + "/start"})
	ww = e.onWeb(trevor, req{method: "POST", path: "/api/v1/servers/" + wc.Server.ID + "/start"})
	same(t, "start before the EULA", aw, ww, tj, ids...)
	if aw.Code != http.StatusConflict || !strings.Contains(aw.Body.String(), `"code":"eula_not_accepted"`) {
		t.Errorf("start before the EULA: %d %s", aw.Code, aw.Body.String())
	}
	aw = e.onAPI(1000, req{method: "POST", path: "/api/v1/servers/" + ac.Server.ID + "/accept-eula"})
	ww = e.onWeb(trevor, req{method: "POST", path: "/api/v1/servers/" + wc.Server.ID + "/accept-eula"})
	if aw.Code != http.StatusAccepted || !strings.Contains(ww.Body.String(), `"action":"server.accept_eula"`) {
		t.Errorf("accept: %d %s", aw.Code, aw.Body.String())
	}
	same(t, "accept", aw, ww, tj, append(ids, opID(t, aw), "OP2", opID(t, ww), "OP2")...)
	same(t, "get server",
		e.onAPI(1000, req{method: "GET", path: "/api/v1/servers/" + ac.Server.ID}),
		e.onWeb(trevor, req{method: "GET", path: "/api/v1/servers/" + wc.Server.ID}), tj, append(ids, opID(t, aw), "OP2", opID(t, ww), "OP2")...)
}

// The new routes are admin only, audited, and need CSRF on web.sock.
func TestCreateForbiddenAndCSRF(t *testing.T) {
	e := setup(t)
	sam := e.signIn(t, "sam")
	trevor := e.signIn(t, "trevor")
	qs := []req{
		{method: "POST", path: "/api/v1/servers", body: createBody("x", 25600)},
		{method: "POST", path: "/api/v1/servers/" + serverA1 + "/install"},
		{method: "POST", path: "/api/v1/servers/" + serverA1 + "/accept-eula"},
	}
	for _, q := range qs {
		aw, ww := e.onAPI(5000, q), e.onWeb(sam, q)
		if ww.Code != http.StatusForbidden {
			t.Errorf("%s: %d %s, want 403", q.path, ww.Code, ww.Body.String())
		}
		same(t, q.path, aw, ww, actorJSON(e.sam))
		if w := e.onWeb(browserUser{cookie: trevor.cookie}, q); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "csrf") {
			t.Errorf("%s without CSRF: %d %s", q.path, w.Code, w.Body.String())
		}
	}
	if n := e.count(t, `SELECT count(*) FROM waystone.audit_events WHERE actor_type = 'user' AND actor_id = $1
		AND outcome = 'denied' AND action IN ('server.create', 'server.install', 'server.accept_eula')`, e.sam.ID); n != 3 {
		t.Errorf("%d denied rows, want 3", n)
	}
	if n := e.count(t, `SELECT count(*) FROM waystone.servers`); n != 3 {
		t.Errorf("%d servers, want 3", n)
	}
}
