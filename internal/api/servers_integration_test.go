// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// getServer fetches one server through GET /api/v1/servers/{id}.
func (e *env) getServer(t *testing.T, id string) serverJSON {
	t.Helper()
	var s serverJSON
	wantOK(t, e.do(t, req{method: "GET", path: "/api/v1/servers/" + id, uid: uidAdminA}), http.StatusOK, &s)
	return s
}

// seen sets node A's last_seen_at; nil means never seen.
func (e *env) seen(t *testing.T, at *time.Time) {
	t.Helper()
	mustExec(t, e.owner, `UPDATE waystone.nodes SET last_seen_at = $1 WHERE node_id = $2`, at, nodeA)
}

func (e *env) countOperations(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.owner.QueryRow(context.Background(), `SELECT count(*) FROM waystone.operations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func ago(e *env, d time.Duration) *time.Time {
	at := e.now.Add(-d)
	return &at
}

// A2: one server, in exactly the shape the list uses.
func TestGetServer(t *testing.T) {
	e := setup(t)
	mustExec(t, e.owner, `UPDATE waystone.servers SET process_state = 'running', process_since = $1, process_observed_at = $1 WHERE server_id = $2`,
		e.now.Add(-10*time.Second), serverA2)

	rec := e.do(t, req{method: "GET", path: "/api/v1/servers/" + serverA2, uid: uidAdminA})
	var one map[string]any
	wantOK(t, rec, http.StatusOK, &one)
	var list struct {
		Servers []map[string]any `json:"servers"`
	}
	wantOK(t, e.do(t, req{method: "GET", path: "/api/v1/servers", uid: uidAdminA}), http.StatusOK, &list)
	a, _ := json.Marshal(one)
	b, _ := json.Marshal(list.Servers[1])
	if string(a) != string(b) {
		t.Errorf("GET one = %s\nlist item = %s", a, b)
	}
	if strings.Contains(rec.Body.String(), "tenant") {
		t.Errorf("response mentions the tenant: %s", rec.Body.String())
	}

	// Another tenant's server and a missing one look the same.
	wantError(t, e.do(t, req{method: "GET", path: "/api/v1/servers/" + serverB1, uid: uidAdminA}), http.StatusNotFound, CodeNotFound)
	wantError(t, e.do(t, req{method: "GET", path: "/api/v1/servers/" + missingServer, uid: uidAdminA}), http.StatusNotFound, CodeNotFound)
	wantError(t, e.do(t, req{method: "GET", path: "/api/v1/servers/nope", uid: uidAdminA}), http.StatusBadRequest, CodeBadRequest)
	// The ID is checked before authorization, so a bad one never reaches
	// an audit row.
	wantError(t, e.do(t, req{method: "GET", path: "/api/v1/servers/nope", uid: uidNodeRole}), http.StatusBadRequest, CodeBadRequest)
	// Not allowed: 403 with an audit row naming the server.
	wantError(t, e.do(t, req{method: "GET", path: "/api/v1/servers/" + serverA1, uid: uidNodeRole}), http.StatusForbidden, CodeForbidden)
	var n int
	if err := e.owner.QueryRow(context.Background(),
		`SELECT count(*) FROM waystone.audit_events WHERE action = 'server.read' AND resource_type = 'server' AND resource_id = $1`,
		serverA1).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("%d audit rows for the denied read, want 1", n)
	}
}

// B2, B3, B4: node, profile and runtime.
func TestServerNodeProfileRuntime(t *testing.T) {
	e := setup(t)

	s := e.getServer(t, serverA1)
	if s.Node.ID != nodeA || s.Node.Name != "node a" || s.Profile.ID != "minecraft-java" || s.Runtime != "systemd" {
		t.Errorf("server = %+v", s)
	}
	// No version yet: null, not missing and not "".
	rec := e.do(t, req{method: "GET", path: "/api/v1/servers/" + serverA1, uid: uidAdminA})
	if !strings.Contains(rec.Body.String(), `"profile":{"id":"minecraft-java","version":null}`) {
		t.Errorf("profile without a version: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"node_id"`) {
		t.Errorf("the flat node_id is gone (B2): %s", rec.Body.String())
	}

	mustExec(t, e.owner, `UPDATE waystone.servers SET config = '{"version": "26.3"}' WHERE server_id = $1`, serverA1)
	if v := e.getServer(t, serverA1).Profile.Version; v == nil || *v != "26.3" {
		t.Errorf("version = %v, want 26.3", v)
	}
	mustExec(t, e.owner, `UPDATE waystone.servers SET config = '{"version": ""}' WHERE server_id = $1`, serverA1)
	if v := e.getServer(t, serverA1).Profile.Version; v != nil {
		t.Errorf("empty version = %q, want null", *v)
	}

	// Reachability, at the 45 s boundary of the controller's clock.
	e.seen(t, nil)
	if r := e.getServer(t, serverA1).Node.Reachability; r.State != "unreachable" || r.LastSeenAt != nil {
		t.Errorf("never seen: %+v", r)
	}
	e.seen(t, ago(e, 45*time.Second))
	if r := e.getServer(t, serverA1).Node.Reachability; r.State != "reachable" || r.LastSeenAt == nil || !r.LastSeenAt.Equal(e.now.Add(-45*time.Second)) {
		t.Errorf("45 s ago: %+v", r)
	}
	e.seen(t, ago(e, 46*time.Second))
	if r := e.getServer(t, serverA1).Node.Reachability; r.State != "unreachable" || r.LastSeenAt == nil {
		t.Errorf("46 s ago: %+v", r)
	}
}

// B7: readiness follows the same staleness rule as process, and is also
// unknown while the node is unreachable.
func TestReadinessStaleness(t *testing.T) {
	e := setup(t)
	set := func(observed time.Time) {
		t.Helper()
		mustExec(t, e.owner, `UPDATE waystone.servers SET readiness = 'ready', readiness_since = $1, readiness_observed_at = $1 WHERE server_id = $2`, observed, serverA1)
	}

	if r := e.getServer(t, serverA1).Readiness; r.State != "unknown" || r.LastReported != "" {
		t.Errorf("never reported: %+v", r)
	}
	set(e.now.Add(-45 * time.Second))
	if r := e.getServer(t, serverA1).Readiness; r.State != "ready" || r.LastReported != "" {
		t.Errorf("45 s old: %+v", r)
	}
	set(e.now.Add(-46 * time.Second))
	if r := e.getServer(t, serverA1).Readiness; r.State != "unknown" || r.LastReported != "ready" ||
		r.ObservedAt == nil || !r.ObservedAt.Equal(e.now.Add(-46*time.Second)) {
		t.Errorf("46 s old: %+v", r)
	}

	// Fresh, but the node is gone.
	set(e.now)
	e.seen(t, nil)
	if r := e.getServer(t, serverA1).Readiness; r.State != "unknown" || r.LastReported != "ready" {
		t.Errorf("node unreachable: %+v", r)
	}
	var stored string
	if err := e.owner.QueryRow(context.Background(),
		`SELECT readiness FROM waystone.servers WHERE server_id = $1`, serverA1).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != "ready" {
		t.Errorf("stored readiness = %q, want ready (never rewritten)", stored)
	}
}

// D4 / V1, and gate G9b at the API level: a new action for an unreachable
// node is refused with no operation and no change to the server.
func TestNodeUnreachableRefusesNewOperations(t *testing.T) {
	e := setup(t)

	// Accepted while reachable, with a key.
	var first operationJSON
	wantOK(t, e.do(t, req{method: "POST", path: start(serverA1), uid: uidAdminA, key: "k1"}), http.StatusAccepted, &first)
	e.setState(t, first.ID, "running", "succeeded")

	e.seen(t, ago(e, 46*time.Second))
	before := e.countOperations(t)
	for _, path := range []string{start(serverA2), stop(serverA2), "/api/v1/servers/" + serverA2 + "/restart"} {
		er := wantError(t, e.do(t, req{method: "POST", path: path, uid: uidAdminA}), http.StatusConflict, CodeNodeUnreachable)
		if er.OperationID != "" {
			t.Errorf("%s: operation_id = %q, want none", path, er.OperationID)
		}
	}
	if n := e.countOperations(t); n != before {
		t.Errorf("%d operations after the refusals, want %d", n, before)
	}
	var desired string
	if err := e.owner.QueryRow(context.Background(),
		`SELECT desired_state FROM waystone.servers WHERE server_id = $1`, serverA2).Scan(&desired); err != nil {
		t.Fatal(err)
	}
	if desired != "stopped" {
		t.Errorf("desired_state = %q after a refused start, want stopped", desired)
	}

	// A refused request doesn't use up its key.
	wantError(t, e.do(t, req{method: "POST", path: start(serverA2), uid: uidAdminA, key: "k2"}), http.StatusConflict, CodeNodeUnreachable)

	// A retry of the request accepted earlier still gets its operation.
	var replay operationJSON
	wantOK(t, e.do(t, req{method: "POST", path: start(serverA1), uid: uidAdminA, key: "k1"}), http.StatusOK, &replay)
	if replay.ID != first.ID {
		t.Errorf("replay = %s, want %s", replay.ID, first.ID)
	}

	// Unreachable wins over a blocking operation, as in the B8 order.
	mustExec(t, e.owner, `UPDATE waystone.nodes SET last_seen_at = '2100-01-01' WHERE node_id = $1`, nodeA)
	var blocking operationJSON
	wantOK(t, e.do(t, req{method: "POST", path: start(serverA2), uid: uidAdminA}), http.StatusAccepted, &blocking)
	e.seen(t, nil)
	wantError(t, e.do(t, req{method: "POST", path: stop(serverA2), uid: uidAdminA}), http.StatusConflict, CodeNodeUnreachable)

	// Back within 45 s: accepted again, and k2 works now.
	e.seen(t, ago(e, 45*time.Second))
	e.setState(t, blocking.ID, "running", "succeeded")
	wantOK(t, e.do(t, req{method: "POST", path: start(serverA2), uid: uidAdminA, key: "k2"}), http.StatusAccepted, &blocking)

	// An operation accepted while reachable stays pending when the node
	// drops (G9a is unchanged).
	e.seen(t, nil)
	var got operationJSON
	wantOK(t, e.do(t, req{method: "GET", path: "/api/v1/operations/" + blocking.ID, uid: uidAdminA}), http.StatusOK, &got)
	if got.State != "pending" {
		t.Errorf("accepted operation is %s after the node dropped, want pending", got.State)
	}
}

// B8: the actions list, and that it never disagrees with the POST.
func TestServerActions(t *testing.T) {
	e := setup(t)
	check := func(name, wantReason string) {
		t.Helper()
		acts := e.getServer(t, serverA1).Actions
		if len(acts) != 3 || acts[0].Action != "start" || acts[1].Action != "stop" || acts[2].Action != "restart" {
			t.Fatalf("%s: actions = %+v", name, acts)
		}
		for _, a := range acts {
			if a.Allowed != (wantReason == "") || a.Reason != wantReason {
				t.Errorf("%s: %+v, want reason %q", name, a, wantReason)
			}
		}
		// The POST refuses with the same code, or accepts.
		rec := e.do(t, req{method: "POST", path: stop(serverA1), uid: uidAdminA})
		if wantReason == "" {
			var op operationJSON
			wantOK(t, rec, http.StatusAccepted, &op)
			e.setState(t, op.ID, "running", "succeeded")
		} else {
			wantError(t, rec, http.StatusConflict, wantReason)
		}
	}

	check("idle", "")
	rec := e.do(t, req{method: "GET", path: "/api/v1/servers/" + serverA1, uid: uidAdminA})
	if !strings.Contains(rec.Body.String(), `{"action":"start","allowed":true}`) {
		t.Errorf("an allowed action has no reason field: %s", rec.Body.String())
	}

	var op operationJSON
	wantOK(t, e.do(t, req{method: "POST", path: start(serverA1), uid: uidAdminA}), http.StatusAccepted, &op)
	check("pending", "operation_in_progress")
	e.setState(t, op.ID, "running")
	check("running", "operation_in_progress")
	e.setState(t, op.ID, "uncertain")
	check("uncertain", "previous_operation_uncertain")

	// Unreachable comes first.
	e.seen(t, nil)
	check("uncertain and unreachable", "node_unreachable")

	// Resolved: free again.
	e.seen(t, ago(e, 0))
	wantOK(t, e.do(t, req{method: "POST", path: "/api/v1/operations/" + op.ID + "/resolve", uid: uidAdminA, body: `{"outcome":"failed"}`}),
		http.StatusOK, &op)
	check("resolved", "")

	// Another server's operation doesn't block a1, and the list works too.
	var other operationJSON
	wantOK(t, e.do(t, req{method: "POST", path: start(serverA2), uid: uidAdminA}), http.StatusAccepted, &other)
	var list struct {
		Servers []serverJSON `json:"servers"`
	}
	wantOK(t, e.do(t, req{method: "GET", path: "/api/v1/servers", uid: uidAdminA}), http.StatusOK, &list)
	if a := list.Servers[0].Actions; !a[0].Allowed {
		t.Errorf("a1 in the list: %+v", a)
	}
	if a := list.Servers[1].Actions; a[0].Allowed || a[0].Reason != "operation_in_progress" {
		t.Errorf("a2 in the list: %+v", a)
	}
}

// E2 / V4: unknown query parameters are refused on every route, before
// anything happens.
func TestUnknownQueryParameters(t *testing.T) {
	e := setup(t)
	var op operationJSON
	wantOK(t, e.do(t, req{method: "POST", path: start(serverA1), uid: uidAdminA}), http.StatusAccepted, &op)
	e.setState(t, op.ID, "running", "uncertain")
	before := e.countOperations(t)

	for _, r := range []req{
		{method: "GET", path: "/api/v1/servers?x=1"},
		{method: "GET", path: "/api/v1/servers/" + serverA1 + "?verbose"},
		{method: "GET", path: "/api/v1/operations?serverid=" + serverA1},
		{method: "GET", path: "/api/v1/operations?server_id=" + serverA1 + "&limit=5&x="},
		{method: "GET", path: "/api/v1/operations?limit=%zz"},
		{method: "GET", path: "/api/v1/operations/" + op.ID + "?x=1"},
		{method: "POST", path: start(serverA2) + "?force=1"},
		{method: "POST", path: "/api/v1/operations/" + op.ID + "/resolve?x=1", body: `{"outcome":"failed"}`},
	} {
		r.uid = uidAdminA
		wantError(t, e.do(t, r), http.StatusBadRequest, CodeBadRequest)
	}
	if n := e.countOperations(t); n != before {
		t.Errorf("%d operations, want %d", n, before)
	}
	var got operationJSON
	wantOK(t, e.do(t, req{method: "GET", path: "/api/v1/operations/" + op.ID, uid: uidAdminA}), http.StatusOK, &got)
	if got.Resolution != nil {
		t.Errorf("the refused resolve went through: %+v", got.Resolution)
	}

	// The known ones still work.
	var ops struct {
		Operations []operationJSON `json:"operations"`
	}
	wantOK(t, e.do(t, req{method: "GET", path: "/api/v1/operations?server_id=" + serverA1 + "&limit=5", uid: uidAdminA}), http.StatusOK, &ops)
	if len(ops.Operations) != 1 {
		t.Errorf("operations = %+v", ops.Operations)
	}
}
