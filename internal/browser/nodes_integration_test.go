// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package browser

// The Nodes API (6b) on both sockets: the same JSON (E1), revocation and
// its hook, the token's one-time bundle, and who may do what.

import (
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"github.com/sharddevs/waystone_panel/internal/agenttls"
	"github.com/sharddevs/waystone_panel/internal/enrollbundle"
	"github.com/sharddevs/waystone_panel/internal/service"
	"github.com/sharddevs/waystone_panel/internal/view"
	"github.com/sharddevs/waystone_panel/internal/web"
)

var (
	testLinkAddr = netip.MustParseAddrPort("10.0.20.80:7443")
	testLinkPin  = agenttls.Pin{1, 2, 3, 4, 5, 6, 7, 8}
)

// revocations records OnNodeRevoked's calls.
type revocations struct {
	mu  sync.Mutex
	ids []string
}

func (r *revocations) add(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ids = append(r.ids, id)
}

func (r *revocations) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.ids...)
}

// setupNodes is setup with an agent listener and a revocation hook.
func setupNodes(t *testing.T, link *service.AgentLink) (*env, *revocations) {
	t.Helper()
	rev := &revocations{}
	e := setupWith(t, func(c *service.Config) {
		c.AgentLink = link
		c.OnNodeRevoked = rev.add
	})
	return e, rev
}

func nodesBody(t *testing.T, w *httptest.ResponseRecorder) []view.NodeDetail {
	t.Helper()
	var v struct{ Nodes []view.NodeDetail }
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("not a node list: %d %s", w.Code, w.Body.String())
	}
	return v.Nodes
}

func TestNodesSameJSONOnBothSockets(t *testing.T) {
	e, rev := setupNodes(t, &service.AgentLink{Addr: testLinkAddr, Pin: testLinkPin})
	trevor := e.signIn(t, "trevor")
	tj := actorJSON(e.trevor)
	both := func(name string, q req) (aw, ww *httptest.ResponseRecorder) {
		t.Helper()
		aw, ww = e.onAPI(1000, q), e.onWeb(trevor, q)
		same(t, name, aw, ww, tj)
		return aw, ww
	}
	for _, c := range []struct {
		name string
		q    req
	}{
		{"list nodes", req{method: "GET", path: "/api/v1/nodes"}},
		{"get node", req{method: "GET", path: "/api/v1/nodes/" + nodeA}},
		{"other tenant's node", req{method: "GET", path: "/api/v1/nodes/" + nodeB}},
		{"missing node", req{method: "GET", path: "/api/v1/nodes/" + missing}},
		{"bad node ID", req{method: "GET", path: "/api/v1/nodes/not-a-uuid"}},
		{"unknown query", req{method: "GET", path: "/api/v1/nodes?all=1"}},
		{"no tokens", req{method: "GET", path: "/api/v1/nodes/enrollments"}},
		{"revoke other tenant's", req{method: "POST", path: "/api/v1/nodes/" + nodeB + "/revoke"}},
		{"revoke missing", req{method: "POST", path: "/api/v1/nodes/" + missing + "/revoke"}},
		{"revoke bad ID", req{method: "POST", path: "/api/v1/nodes/nope/revoke"}},
		{"revoke unknown field", req{method: "POST", path: "/api/v1/nodes/" + nodeA + "/revoke", body: `{"force":true}`}},
		{"token without name", req{method: "POST", path: "/api/v1/nodes/enrollments", body: `{}`}},
		{"token bad name", req{method: "POST", path: "/api/v1/nodes/enrollments", body: `{"name":"a\nb"}`}},
		{"token long name", req{method: "POST", path: "/api/v1/nodes/enrollments", body: `{"name":"` + strings.Repeat("n", 65) + `"}`}},
		{"token unknown field", req{method: "POST", path: "/api/v1/nodes/enrollments", body: `{"name":"x","addr":"192.0.2.1:1"}`}},
	} {
		both(c.name, c.q)
	}
	_, ww := both("list nodes", req{method: "GET", path: "/api/v1/nodes"})
	if ns := nodesBody(t, ww); len(ns) != 1 || ns[0].ID != nodeA || ns[0].ServerCount != 2 || ns[0].Link != "local" ||
		ns[0].Reachability.State != "reachable" || len(ns[0].Actions) != 1 || !ns[0].Actions[0].Allowed {
		t.Errorf("nodes: %s", ww.Body.String())
	}
	if n := e.count(t, `SELECT count(*) FROM waystone.node_enrollments`); n != 0 {
		t.Fatalf("%d tokens after refusals, want 0", n)
	}
	if got := rev.list(); len(got) != 0 {
		t.Fatalf("revocations after refusals: %v", got)
	}

	// A token on each socket. The two differ in their IDs, times and
	// secrets, so compare their shape and what they say.
	tokens := map[string]view.NewEnrollment{}
	for name, w := range map[string]*httptest.ResponseRecorder{
		"api": e.onAPI(1000, req{method: "POST", path: "/api/v1/nodes/enrollments", body: `{"name":"pi-node"}`}),
		"web": e.onWeb(trevor, req{method: "POST", path: "/api/v1/nodes/enrollments", body: `{"name":"pi-node"}`}),
	} {
		if w.Code != http.StatusCreated || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Content-Type") != "application/json" {
			t.Fatalf("%s token: %d %v %s", name, w.Code, w.Header(), w.Body.String())
		}
		var keys map[string]json.RawMessage
		_ = json.Unmarshal(w.Body.Bytes(), &keys)
		if len(keys) != 3 {
			t.Errorf("%s token keys: %s", name, w.Body.String())
		}
		var ne view.NewEnrollment
		if err := json.Unmarshal(w.Body.Bytes(), &ne); err != nil {
			t.Fatal(err)
		}
		if ne.Command != "sudo waystone-agent enroll" || ne.Enrollment.State != "pending" || ne.Enrollment.Name != "pi-node" {
			t.Errorf("%s token: %+v", name, ne)
		}
		b, err := enrollbundle.Decode(ne.Bundle)
		if err != nil || b.Addr != testLinkAddr || b.Pin != testLinkPin {
			t.Fatalf("%s bundle: %+v %v", name, b, err)
		}
		// Only the secret's hash is kept, and it's this bundle's.
		sum := sha256.Sum256(b.Secret[:])
		if n := e.count(t, `SELECT count(*) FROM waystone.node_enrollments WHERE enrollment_id = $1 AND secret_sha256 = $2`,
			ne.Enrollment.ID, sum[:]); n != 1 {
			t.Errorf("%s: no token row with the bundle's secret hash", name)
		}
		if strings.Contains(e.logs.String(), ne.Bundle) {
			t.Errorf("%s: the bundle is in the log", name)
		}
		tokens[name] = ne
	}
	if !strings.Contains(string(mustJSON(t, tokens["web"].Enrollment.CreatedBy)), e.trevor.ID) ||
		tokens["api"].Enrollment.CreatedBy.Type != "local_user" {
		t.Errorf("creators: api %+v, web %+v", tokens["api"].Enrollment.CreatedBy, tokens["web"].Enrollment.CreatedBy)
	}
	// The list is the same data whoever asks, and never a secret.
	aw, ww := e.onAPI(1000, req{method: "GET", path: "/api/v1/nodes/enrollments"}), e.onWeb(trevor, req{method: "GET", path: "/api/v1/nodes/enrollments"})
	if aw.Code != http.StatusOK || aw.Body.String() != ww.Body.String() || strings.Count(aw.Body.String(), `"state":"pending"`) != 2 {
		t.Errorf("token lists\napi: %d %s\nweb: %d %s", aw.Code, aw.Body.String(), ww.Code, ww.Body.String())
	}
	for _, ne := range tokens {
		if strings.Contains(aw.Body.String(), ne.Bundle) || strings.Contains(aw.Body.String(), "secret") {
			t.Errorf("the list shows a secret: %s", aw.Body.String())
		}
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// D5: revoking cuts the node off once, is audited once, answers the same
// on a retry, and blocks its servers with node_revoked.
func TestRevokeNode(t *testing.T) {
	e, rev := setupNodes(t, nil)
	trevor := e.signIn(t, "trevor")
	tj := actorJSON(e.trevor)
	revoke := req{method: "POST", path: "/api/v1/nodes/" + nodeA + "/revoke"}

	aw := e.onAPI(1000, revoke)
	if aw.Code != http.StatusOK {
		t.Fatalf("revoke: %d %s", aw.Code, aw.Body.String())
	}
	var n view.NodeDetail
	if err := json.Unmarshal(aw.Body.Bytes(), &n); err != nil || n.ID != nodeA || n.RevokedAt == nil ||
		n.Reachability.State != "unreachable" || len(n.Actions) != 1 || n.Actions[0].Allowed || n.Actions[0].Reason != "already_revoked" {
		t.Errorf("revoked node: %s", aw.Body.String())
	}
	if got := rev.list(); len(got) != 1 || got[0] != nodeA {
		t.Errorf("OnNodeRevoked calls: %v, want one for %s", got, nodeA)
	}
	// Again, from the browser: the same answer, no second cut or audit.
	ww := e.onWeb(trevor, revoke)
	same(t, "revoke again", aw, ww, tj)
	if got := rev.list(); len(got) != 1 {
		t.Errorf("OnNodeRevoked calls after a retry: %v", got)
	}
	if c := e.count(t, `SELECT count(*) FROM waystone.audit_events WHERE action = 'node.revoke' AND resource_id = $1`, nodeA); c != 1 {
		t.Errorf("%d node.revoke audit rows, want 1", c)
	}
	if c := e.count(t, `SELECT count(*) FROM waystone.audit_events WHERE action = 'node.revoke' AND actor_type = 'local_user' AND outcome = 'recorded'`); c != 1 {
		t.Errorf("the audit row isn't the local admin's")
	}

	// Its servers: every action refused with node_revoked, and a POST is 409.
	ww = e.onWeb(trevor, req{method: "GET", path: "/api/v1/servers/" + serverA1})
	var s view.Server
	if err := json.Unmarshal(ww.Body.Bytes(), &s); err != nil {
		t.Fatal(err)
	}
	for _, a := range s.Actions {
		if a.Allowed || a.Reason != "node_revoked" {
			t.Errorf("server action %s: %+v", a.Action, a)
		}
	}
	if s.Node.Reachability.State != "unreachable" {
		t.Errorf("server's node: %+v", s.Node)
	}
	start := req{method: "POST", path: "/api/v1/servers/" + serverA1 + "/start"}
	aw, ww = e.onAPI(1000, start), e.onWeb(trevor, start)
	same(t, "start on a revoked node", aw, ww, tj)
	if aw.Code != http.StatusConflict || !strings.Contains(aw.Body.String(), `"code":"node_revoked"`) {
		t.Errorf("start on a revoked node: %d %s", aw.Code, aw.Body.String())
	}
	if c := e.count(t, `SELECT count(*) FROM waystone.operations`); c != 0 {
		t.Errorf("%d operations, want 0", c)
	}
	// The list shows it revoked too.
	if ns := nodesBody(t, e.onWeb(trevor, req{method: "GET", path: "/api/v1/nodes"})); len(ns) != 1 || ns[0].RevokedAt == nil {
		t.Errorf("list after revoke: %+v", ns)
	}
}

// A role that may do nothing gets 403 on every node route, audited, with
// no token made and nothing revoked; enrollment_unavailable is said only
// to an admin.
func TestNodesForbidden(t *testing.T) {
	e, rev := setupNodes(t, nil)
	sam := e.signIn(t, "sam")
	sj := actorJSON(e.sam)
	qs := []req{
		{method: "GET", path: "/api/v1/nodes"},
		{method: "GET", path: "/api/v1/nodes/" + nodeA},
		{method: "POST", path: "/api/v1/nodes/" + nodeA + "/revoke"},
		{method: "GET", path: "/api/v1/nodes/enrollments"},
		{method: "POST", path: "/api/v1/nodes/enrollments", body: `{"name":"x"}`},
	}
	for _, q := range qs {
		aw, ww := e.onAPI(5000, q), e.onWeb(sam, q)
		if ww.Code != http.StatusForbidden {
			t.Errorf("%s %s: %d %s, want 403", q.method, q.path, ww.Code, ww.Body.String())
		}
		same(t, q.method+" "+q.path, aw, ww, sj)
	}
	if n := e.count(t, `SELECT count(*) FROM waystone.audit_events
		WHERE actor_type = 'user' AND actor_id = $1 AND outcome = 'denied' AND reason = 'forbidden' AND action LIKE 'node.%'`, e.sam.ID); n != len(qs) {
		t.Errorf("%d denied node audit rows for the user, want %d", n, len(qs))
	}
	if n := e.count(t, `SELECT count(*) FROM waystone.node_enrollments`); n != 0 {
		t.Errorf("%d tokens, want 0", n)
	}
	if n := e.count(t, `SELECT count(*) FROM waystone.nodes WHERE revoked_at IS NOT NULL`); n != 0 || len(rev.list()) != 0 {
		t.Errorf("%d revoked nodes, %v calls", n, rev.list())
	}

	// No listener: an admin is told, and no token is made.
	trevor := e.signIn(t, "trevor")
	token := req{method: "POST", path: "/api/v1/nodes/enrollments", body: `{"name":"x"}`}
	aw, ww := e.onAPI(1000, token), e.onWeb(trevor, token)
	same(t, "no listener", aw, ww, actorJSON(e.trevor))
	if aw.Code != http.StatusConflict || !strings.Contains(aw.Body.String(), `"code":"enrollment_unavailable"`) {
		t.Errorf("no listener: %d %s", aw.Code, aw.Body.String())
	}
	if n := e.count(t, `SELECT count(*) FROM waystone.node_enrollments`); n != 0 {
		t.Errorf("%d tokens without a listener, want 0", n)
	}
}

// A listener on an address no node can dial makes no token either.
func TestEnrollmentUnusableAddr(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:7443", "[::]:7443", "169.254.1.1:7443", "10.0.20.80:0"} {
		t.Run(addr, func(t *testing.T) {
			e, _ := setupNodes(t, &service.AgentLink{Addr: netip.MustParseAddrPort(addr), Pin: testLinkPin})
			w := e.onAPI(1000, req{method: "POST", path: "/api/v1/nodes/enrollments", body: `{"name":"x"}`})
			if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"code":"enrollment_unavailable"`) {
				t.Errorf("%d %s", w.Code, w.Body.String())
			}
			if n := e.count(t, `SELECT count(*) FROM waystone.node_enrollments`); n != 0 {
				t.Errorf("%d tokens, want 0", n)
			}
		})
	}
}

// The node POSTs need the session's CSRF token like every other POST.
func TestNodesCSRF(t *testing.T) {
	e, rev := setupNodes(t, &service.AgentLink{Addr: testLinkAddr, Pin: testLinkPin})
	trevor := e.signIn(t, "trevor")
	for _, q := range []req{
		{method: "POST", path: "/api/v1/nodes/" + nodeA + "/revoke"},
		{method: "POST", path: "/api/v1/nodes/enrollments", body: `{"name":"x"}`},
	} {
		w := e.onWeb(browserUser{cookie: trevor.cookie}, q)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"code":"`+web.CodeCSRF+`"`) {
			t.Errorf("%s without CSRF: %d %s", q.path, w.Code, w.Body.String())
		}
	}
	if n := e.count(t, `SELECT count(*) FROM waystone.node_enrollments`); n != 0 || len(rev.list()) != 0 {
		t.Errorf("%d tokens, %v revocations", n, rev.list())
	}
}
