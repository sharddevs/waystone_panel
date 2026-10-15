// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

import (
	"bytes"
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The stage 4a goal: a start creates a pending operation, recorded with
// who asked, and it can be read back.
func TestStartCreatesPendingOperation(t *testing.T) {
	e := setup(t)

	rec := e.do(t, req{method: "POST", path: start(serverA1), uid: uidAdminA})
	var op operationJSON
	wantOK(t, rec, http.StatusAccepted, &op)
	if op.State != "pending" || op.Action != "server.start" || op.ServerID != serverA1 || op.NodeID != nodeA {
		t.Errorf("operation = %+v", op)
	}
	if op.Actor != (actorJSON{Type: "local_user", ID: "1000", Name: "admin a"}) {
		t.Errorf("actor = %+v, want local_user 1000 \"admin a\"", op.Actor)
	}
	if !op.RequestedAt.Equal(e.now) || op.StartedAt != nil || op.FinishedAt != nil || op.Resolution != nil {
		t.Errorf("timestamps = %+v", op)
	}
	if loc := rec.Header().Get("Location"); loc != "/api/v1/operations/"+op.ID {
		t.Errorf("Location = %q, want /api/v1/operations/%s", loc, op.ID)
	}
	// The response must not carry fields outside the public shape.
	if strings.Contains(rec.Body.String(), "tenant") {
		t.Errorf("response mentions the tenant: %s", rec.Body.String())
	}

	var got operationJSON
	wantOK(t, e.do(t, req{method: "GET", path: "/api/v1/operations/" + op.ID, uid: uidAdminA}), http.StatusOK, &got)
	if got.ID != op.ID || got.State != "pending" || got.Actor.Name != "admin a" {
		t.Errorf("GET = %+v", got)
	}

	// An empty JSON object is the same as no body.
	wantOK(t, e.do(t, req{method: "POST", path: start(serverA2), uid: uidAdminA, body: "{}"}), http.StatusAccepted, &op)

	// An unknown uid still works; its name falls back to "uid <n>".
	mustExec(t, e.owner, `INSERT INTO waystone.memberships VALUES ($1, 'local_user', '1500', 'admin')`, tenantA)
	mustExec(t, e.owner, `UPDATE waystone.operations SET state = 'running' WHERE resource_id = $1`, serverA2)
	mustExec(t, e.owner, `UPDATE waystone.operations SET state = 'succeeded' WHERE resource_id = $1`, serverA2)
	wantOK(t, e.do(t, req{method: "POST", path: stop(serverA2), uid: 1500}), http.StatusAccepted, &op)
	if op.Actor.Name != "uid 1500" || op.Action != "server.stop" {
		t.Errorf("unknown uid's operation = %+v", op)
	}

	// Restart works too.
	mustExec(t, e.owner, `UPDATE waystone.operations SET state = 'running' WHERE operation_id = $1`, op.ID)
	mustExec(t, e.owner, `UPDATE waystone.operations SET state = 'failed' WHERE operation_id = $1`, op.ID)
	wantOK(t, e.do(t, req{method: "POST", path: "/api/v1/servers/" + serverA2 + "/restart", uid: uidAdminA}), http.StatusAccepted, &op)
	if op.Action != "server.restart" {
		t.Errorf("restart action = %q", op.Action)
	}
}

// F11 and F13: a second action while one is unfinished is refused, and
// says which operation is in the way.
func TestConflicts(t *testing.T) {
	e := setup(t)
	var first operationJSON
	wantOK(t, e.do(t, req{method: "POST", path: start(serverA1), uid: uidAdminA}), http.StatusAccepted, &first)

	er := wantError(t, e.do(t, req{method: "POST", path: stop(serverA1), uid: uidAdminA}), http.StatusConflict, CodeOperationInProgress)
	if er.OperationID != first.ID {
		t.Errorf("operation_id = %q, want %s", er.OperationID, first.ID)
	}
	e.setState(t, first.ID, "running")
	er = wantError(t, e.do(t, req{method: "POST", path: stop(serverA1), uid: uidAdminA}), http.StatusConflict, CodeOperationInProgress)
	if er.OperationID != first.ID {
		t.Errorf("while running, operation_id = %q", er.OperationID)
	}

	e.setState(t, first.ID, "uncertain")
	er = wantError(t, e.do(t, req{method: "POST", path: stop(serverA1), uid: uidAdminA}), http.StatusConflict, CodePreviousOperationUncertain)
	if er.OperationID != first.ID {
		t.Errorf("uncertain, operation_id = %q", er.OperationID)
	}
	// Other servers aren't affected.
	var other operationJSON
	wantOK(t, e.do(t, req{method: "POST", path: start(serverA2), uid: uidAdminA}), http.StatusAccepted, &other)
}

// Decision O through the API: resolve keeps the state uncertain, records
// who resolved it, unblocks the server, and works only once.
func TestResolve(t *testing.T) {
	e := setup(t)
	var op operationJSON
	wantOK(t, e.do(t, req{method: "POST", path: start(serverA1), uid: uidAdminA}), http.StatusAccepted, &op)
	resolve := "/api/v1/operations/" + op.ID + "/resolve"

	wantError(t, e.do(t, req{method: "POST", path: resolve, uid: uidAdminA, body: `{"outcome":"failed"}`}), http.StatusConflict, CodeNotUncertain)

	e.setState(t, op.ID, "running", "uncertain")
	for _, bad := range []string{``, `{}`, `{"outcome":"uncertain"}`, `{"outcome":"FAILED"}`,
		`{"outcome":"failed","note":"` + strings.Repeat("x", 501) + `"}`, `{"outcome":"failed","by":"me"}`} {
		wantError(t, e.do(t, req{method: "POST", path: resolve, uid: uidAdminA, body: bad}), http.StatusBadRequest, CodeBadRequest)
	}

	e.now = e.now.Add(time.Hour)
	var got operationJSON
	wantOK(t, e.do(t, req{method: "POST", path: resolve, uid: uidAdminA, body: `{"outcome":"failed","note":"checked by hand"}`}), http.StatusOK, &got)
	if got.State != "uncertain" || got.Resolution == nil {
		t.Fatalf("after resolve: %+v", got)
	}
	r := got.Resolution
	if r.Outcome != "failed" || r.Note != "checked by hand" || !r.At.Equal(e.now) ||
		r.By != (actorJSON{Type: "local_user", ID: "1000", Name: "admin a"}) {
		t.Errorf("resolution = %+v", r)
	}

	wantError(t, e.do(t, req{method: "POST", path: resolve, uid: uidAdminA, body: `{"outcome":"succeeded"}`}), http.StatusConflict, CodeAlreadyResolved)

	// The server is free again.
	wantOK(t, e.do(t, req{method: "POST", path: start(serverA1), uid: uidAdminA}), http.StatusAccepted, &got)

	// Resolving a missing operation, or another tenant's: not found.
	wantError(t, e.do(t, req{method: "POST", path: "/api/v1/operations/0199e1a0-0000-7000-8000-00000000dead/resolve", uid: uidAdminA, body: `{"outcome":"failed"}`}), http.StatusNotFound, CodeNotFound)
	wantError(t, e.do(t, req{method: "POST", path: resolve, uid: uidAdminB, body: `{"outcome":"failed"}`}), http.StatusNotFound, CodeNotFound)
	wantError(t, e.do(t, req{method: "POST", path: "/api/v1/operations/nope/resolve", uid: uidAdminA, body: `{"outcome":"failed"}`}), http.StatusBadRequest, CodeBadRequest)
}

// C1 and F12 through the API.
func TestIdempotencyKeys(t *testing.T) {
	e := setup(t)
	var op, again operationJSON
	wantOK(t, e.do(t, req{method: "POST", path: start(serverA1), uid: uidAdminA, key: "retry-1"}), http.StatusAccepted, &op)

	// A retry, even with a body that only differs in spacing, gets the
	// same operation back with 200 and no new row.
	wantOK(t, e.do(t, req{method: "POST", path: start(serverA1), uid: uidAdminA, key: "retry-1", body: " { } "}), http.StatusOK, &again)
	if again.ID != op.ID {
		t.Errorf("retry returned %s, want %s", again.ID, op.ID)
	}
	// It reflects the operation's state now.
	e.setState(t, op.ID, "running")
	wantOK(t, e.do(t, req{method: "POST", path: start(serverA1), uid: uidAdminA, key: "retry-1"}), http.StatusOK, &again)
	if again.State != "running" {
		t.Errorf("replay state = %q, want running", again.State)
	}

	// Same key, different request (another action, another server).
	wantError(t, e.do(t, req{method: "POST", path: stop(serverA1), uid: uidAdminA, key: "retry-1"}), http.StatusUnprocessableEntity, CodeIdempotencyKeyReused)
	wantError(t, e.do(t, req{method: "POST", path: start(serverA2), uid: uidAdminA, key: "retry-1"}), http.StatusUnprocessableEntity, CodeIdempotencyKeyReused)

	// The stored fingerprint is C1's formula with the route template
	// exactly as the scope note writes it, without the method in front.
	var stored []byte
	if err := e.owner.QueryRow(context.Background(), `SELECT request_sha256 FROM waystone.idempotency_keys WHERE key = 'retry-1'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if want, _ := Fingerprint("POST", "/api/v1/servers/{id}/start", serverA1, actionRequest{}); !bytes.Equal(stored, want[:]) {
		t.Errorf("stored fingerprint %x, want %x (route template \"/api/v1/servers/{id}/start\")", stored, want)
	}

	for _, bad := range []string{"has space", strings.Repeat("k", 65), "ümlaut", "semi;colon"} {
		wantError(t, e.do(t, req{method: "POST", path: start(serverA2), uid: uidAdminA, key: bad}), http.StatusBadRequest, CodeInvalidIdempotencyKey)
	}

	var n int
	if err := e.owner.QueryRow(context.Background(), `SELECT count(*) FROM waystone.operations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("%d operations exist, want 1", n)
	}
}

// I5, I6: other tenants' things look missing; no membership or several
// memberships are refused.
func TestTenantsAndMembership(t *testing.T) {
	e := setup(t)
	var op operationJSON
	wantOK(t, e.do(t, req{method: "POST", path: start(serverA1), uid: uidAdminA}), http.StatusAccepted, &op)

	wantError(t, e.do(t, req{method: "POST", path: start(serverA1), uid: uidAdminB}), http.StatusNotFound, CodeNotFound)
	wantError(t, e.do(t, req{method: "GET", path: "/api/v1/operations/" + op.ID, uid: uidAdminB}), http.StatusNotFound, CodeNotFound)
	wantError(t, e.do(t, req{method: "POST", path: start(missingServer), uid: uidAdminA}), http.StatusNotFound, CodeNotFound)

	for _, uid := range []uint32{uidStranger, uidAdminAB} {
		wantError(t, e.do(t, req{method: "GET", path: "/api/v1/servers", uid: uid}), http.StatusForbidden, CodeForbidden)
		wantError(t, e.do(t, req{method: "POST", path: start(serverA2), uid: uid}), http.StatusForbidden, CodeForbidden)
	}
	// Those refusals had no tenant to write an audit row into.
	var n int
	if err := e.owner.QueryRow(context.Background(), `SELECT count(*) FROM waystone.audit_events`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d audit rows, want 0", n)
	}

	// Instead, each one is a structured security event in the log, with
	// who, what and why, and nothing from the request itself. This is the
	// deliberate exception to I6 (approved 2026-10-13).
	events := e.securityEvents()
	if len(events) != 4 {
		t.Fatalf("security events = %q, want 4", events)
	}
	want := []struct{ actor, action, resource, reason string }{
		{"local_user:4000", "server.list", "server:*", "no_membership"},
		{"local_user:4000", "server.start", "server:" + serverA2, "no_membership"},
		{"local_user:3000", "server.list", "server:*", "ambiguous_membership"},
		{"local_user:3000", "server.start", "server:" + serverA2, "ambiguous_membership"},
	}
	for i, w := range want {
		ev := events[i]
		for _, field := range []string{
			"security_event=denied ",
			" actor=" + w.actor + " ",
			" action=" + w.action + " ",
			" resource=" + w.resource + " ",
			" reason=" + w.reason,
		} {
			if !strings.Contains(ev, field) {
				t.Errorf("event %d %q lacks %q", i, ev, field)
			}
		}
		if !regexp.MustCompile(` request_id=[0-9a-f-]{36} `).MatchString(ev) {
			t.Errorf("event %d %q lacks a request_id", i, ev)
		}
	}
}

// A member whose role may not use the API is refused with 403, and the
// refusal is recorded in audit_events (H4), even though nothing else
// happened.
func TestRoleDeniedIsAudited(t *testing.T) {
	e := setup(t)
	wantError(t, e.do(t, req{method: "POST", path: start(serverA1), uid: uidNodeRole}), http.StatusForbidden, CodeForbidden)
	rec := e.do(t, req{method: "GET", path: "/api/v1/servers", uid: uidNodeRole})
	er := wantError(t, rec, http.StatusForbidden, CodeForbidden)

	rows, err := e.owner.Query(context.Background(), `SELECT tenant_id::text, actor_type, actor_id, actor_name,
		action, resource_type, resource_id, reason, request_id, outcome FROM waystone.audit_events ORDER BY at, action`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type row struct{ tenant, atype, aid, aname, action, rtype, rid, reason, reqID, outcome string }
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.tenant, &r.atype, &r.aid, &r.aname, &r.action, &r.rtype, &r.rid, &r.reason, &r.reqID, &r.outcome); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	if len(got) != 2 {
		t.Fatalf("%d audit rows, want 2: %+v", len(got), got)
	}
	want := map[string]row{
		"server.start": {tenantA, "local_user", "5000", "not an admin", "server.start", "server", serverA1, "forbidden", "", "denied"},
		"server.list":  {tenantA, "local_user", "5000", "not an admin", "server.list", "server", "*", "forbidden", er.RequestID, "denied"},
	}
	for _, g := range got {
		w, ok := want[g.action]
		if !ok {
			t.Errorf("unexpected audit row %+v", g)
			continue
		}
		if w.reqID == "" {
			w.reqID = g.reqID // the first request's ID isn't known here; it must just be set
			if len(g.reqID) != 36 {
				t.Errorf("request_id %q, want a UUID", g.reqID)
			}
		}
		if g != w {
			t.Errorf("audit row = %+v, want %+v", g, w)
		}
	}
	// And no operation was created.
	var n int
	if err := e.owner.QueryRow(context.Background(), `SELECT count(*) FROM waystone.operations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d operations, want 0", n)
	}
}

func TestListServers(t *testing.T) {
	e := setup(t)
	// Observed 30 s before the clock, so it is still fresh (R13).
	seen := e.now.Add(-30 * time.Second)
	mustExec(t, e.owner, `UPDATE waystone.servers SET process_state = 'running', process_since = $1, process_observed_at = $1 WHERE server_id = $2`, seen, serverA2)

	var body struct {
		Servers []serverJSON `json:"servers"`
	}
	rec := e.do(t, req{method: "GET", path: "/api/v1/servers", uid: uidAdminA})
	wantOK(t, rec, http.StatusOK, &body)
	if len(body.Servers) != 2 || body.Servers[0].ID != serverA1 || body.Servers[1].ID != serverA2 {
		t.Fatalf("servers = %+v, want a1, a2", body.Servers)
	}
	s := body.Servers[1]
	if s.Name != "a2" || s.Node.ID != nodeA || s.Node.Name != "node a" || s.Node.Reachability.State != "reachable" ||
		s.Profile.ID != "minecraft-java" || s.Profile.Version != nil || s.Runtime != "systemd" || s.DesiredState != "stopped" ||
		s.Process.State != "running" || s.Process.Since == nil || !s.Process.Since.Equal(seen) ||
		s.Readiness.State != "unknown" || s.Readiness.Since != nil {
		t.Errorf("a2 = %+v", s)
	}
	if strings.Contains(rec.Body.String(), "tenant") {
		t.Errorf("response mentions the tenant: %s", rec.Body.String())
	}

	wantOK(t, e.do(t, req{method: "GET", path: "/api/v1/servers", uid: uidAdminB}), http.StatusOK, &body)
	if len(body.Servers) != 1 || body.Servers[0].ID != serverB1 {
		t.Errorf("tenant B sees %+v", body.Servers)
	}

	// A tenant without servers gets [], not null.
	mustExec(t, e.owner, `DELETE FROM waystone.servers WHERE tenant_id = $1`, tenantB)
	rec = e.do(t, req{method: "GET", path: "/api/v1/servers", uid: uidAdminB})
	if !strings.Contains(rec.Body.String(), `"servers":[]`) {
		t.Errorf("empty tenant: %s, want \"servers\":[]", rec.Body.String())
	}
}

func TestListOperations(t *testing.T) {
	e := setup(t)
	var a1, a2 operationJSON
	wantOK(t, e.do(t, req{method: "POST", path: start(serverA1), uid: uidAdminA}), http.StatusAccepted, &a1)
	e.now = e.now.Add(time.Second)
	wantOK(t, e.do(t, req{method: "POST", path: start(serverA2), uid: uidAdminA}), http.StatusAccepted, &a2)
	wantOK(t, e.do(t, req{method: "POST", path: start(serverB1), uid: uidAdminB}), http.StatusAccepted, &operationJSON{})

	var body struct {
		Operations []operationJSON `json:"operations"`
	}
	wantOK(t, e.do(t, req{method: "GET", path: "/api/v1/operations", uid: uidAdminA}), http.StatusOK, &body)
	if len(body.Operations) != 2 || body.Operations[0].ID != a2.ID || body.Operations[1].ID != a1.ID {
		t.Errorf("all = %+v, want a2 then a1", body.Operations)
	}
	wantOK(t, e.do(t, req{method: "GET", path: "/api/v1/operations?server_id=" + serverA1, uid: uidAdminA}), http.StatusOK, &body)
	if len(body.Operations) != 1 || body.Operations[0].ID != a1.ID {
		t.Errorf("server a1 = %+v", body.Operations)
	}
	wantOK(t, e.do(t, req{method: "GET", path: "/api/v1/operations?limit=1", uid: uidAdminA}), http.StatusOK, &body)
	if len(body.Operations) != 1 || body.Operations[0].ID != a2.ID {
		t.Errorf("limit 1 = %+v", body.Operations)
	}
	rec := e.do(t, req{method: "GET", path: "/api/v1/operations?server_id=" + serverB1, uid: uidAdminA})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"operations":[]`) {
		t.Errorf("another tenant's server: %d %s, want 200 and []", rec.Code, rec.Body.String())
	}
	for _, q := range []string{"?limit=0", "?limit=101", "?limit=ten", "?limit=-1", "?server_id=nope"} {
		wantError(t, e.do(t, req{method: "GET", path: "/api/v1/operations" + q, uid: uidAdminA}), http.StatusBadRequest, CodeBadRequest)
	}
	wantError(t, e.do(t, req{method: "GET", path: "/api/v1/operations/not-a-uuid", uid: uidAdminA}), http.StatusBadRequest, CodeBadRequest)

	// Without ?limit= a list stops at 50. Tenant A gets 52 operations: a1,
	// a2 and 50 more on a1, each finished so the next one may start.
	e.setState(t, a1.ID, "running", "succeeded")
	for i := 0; i < 50; i++ {
		e.now = e.now.Add(3 * time.Second) // stay under the rate limit
		var op operationJSON
		wantOK(t, e.do(t, req{method: "POST", path: start(serverA1), uid: uidAdminA}), http.StatusAccepted, &op)
		e.setState(t, op.ID, "running", "succeeded")
	}
	wantOK(t, e.do(t, req{method: "GET", path: "/api/v1/operations", uid: uidAdminA}), http.StatusOK, &body)
	if len(body.Operations) != 50 {
		t.Errorf("no limit: got %d operations, want the default 50", len(body.Operations))
	}
	wantOK(t, e.do(t, req{method: "GET", path: "/api/v1/operations?limit=100", uid: uidAdminA}), http.StatusOK, &body)
	if len(body.Operations) != 52 {
		t.Errorf("limit 100: got %d operations, want all 52", len(body.Operations))
	}
}

// Body rules on a real endpoint: nothing is created when the body is
// refused.
func TestBodyRules(t *testing.T) {
	e := setup(t)
	wantError(t, e.do(t, req{method: "POST", path: start(serverA1), uid: uidAdminA, body: `{"force":true}`}), http.StatusBadRequest, CodeBadRequest)
	wantError(t, e.do(t, req{method: "POST", path: start(serverA1), uid: uidAdminA, body: `{}`, contentType: "text/plain"}), http.StatusUnsupportedMediaType, CodeUnsupportedMediaType)
	wantError(t, e.do(t, req{method: "POST", path: start(serverA1), uid: uidAdminA, body: `{"x":"` + strings.Repeat("a", MaxBodyBytes) + `"}`}), http.StatusRequestEntityTooLarge, CodeBodyTooLarge)
	wantError(t, e.do(t, req{method: "POST", path: start("NOT-A-UUID"), uid: uidAdminA}), http.StatusBadRequest, CodeBadRequest)

	// Resolve has the same body rules, with the same codes: 415 and 413
	// stay 415 and 413, not a generic 400.
	resolve := "/api/v1/operations/" + missingServer + "/resolve"
	wantError(t, e.do(t, req{method: "POST", path: resolve, uid: uidAdminA, body: `{"outcome":"failed"}`, contentType: "text/plain"}), http.StatusUnsupportedMediaType, CodeUnsupportedMediaType)
	wantError(t, e.do(t, req{method: "POST", path: resolve, uid: uidAdminA, body: `{"note":"` + strings.Repeat("a", MaxBodyBytes) + `"}`}), http.StatusRequestEntityTooLarge, CodeBodyTooLarge)

	var n int
	if err := e.owner.QueryRow(context.Background(), `SELECT count(*) FROM waystone.operations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d operations after refused requests, want 0", n)
	}
}

// A refused list of operations is audited against the server it asked
// about, or ("server", "*") without a server_id.
func TestListDenialResource(t *testing.T) {
	e := setup(t)
	wantError(t, e.do(t, req{method: "GET", path: "/api/v1/operations?server_id=" + serverA1, uid: uidNodeRole}), http.StatusForbidden, CodeForbidden)
	wantError(t, e.do(t, req{method: "GET", path: "/api/v1/operations", uid: uidNodeRole}), http.StatusForbidden, CodeForbidden)

	rows, err := e.owner.Query(context.Background(), `SELECT action, resource_type, resource_id FROM waystone.audit_events ORDER BY at, resource_id DESC`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var action, rtype, rid string
		if err := rows.Scan(&action, &rtype, &rid); err != nil {
			t.Fatal(err)
		}
		got = append(got, action+" "+rtype+":"+rid)
	}
	want := []string{"operation.list server:" + serverA1, "operation.list server:*"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("audit rows = %q, want %q", got, want)
	}
}

// R13: a process state is shown as unknown once its last accepted
// observation is more than 45 s old (or missing), and the stored value is
// kept in last_reported. The database row is never rewritten.
func TestServerStaleness(t *testing.T) {
	e := setup(t)
	get := func() serverJSON {
		t.Helper()
		var body struct {
			Servers []serverJSON `json:"servers"`
		}
		wantOK(t, e.do(t, req{method: "GET", path: "/api/v1/servers", uid: uidAdminA}), http.StatusOK, &body)
		for _, s := range body.Servers {
			if s.ID == serverA2 {
				return s
			}
		}
		t.Fatalf("a2 missing from %+v", body.Servers)
		return serverJSON{}
	}
	set := func(observed time.Time) {
		t.Helper()
		mustExec(t, e.owner, `UPDATE waystone.servers SET process_state = 'running', process_since = $1, process_observed_at = $1 WHERE server_id = $2`, observed, serverA2)
	}

	// Never reported: unknown, and nothing to show as last reported.
	if s := get(); s.Process.State != "unknown" || s.Process.LastReported != "" {
		t.Errorf("never reported: %+v", s.Process)
	}

	// Exactly 45 s old is still fresh.
	set(e.now.Add(-45 * time.Second))
	if s := get(); s.Process.State != "running" || s.Process.LastReported != "" {
		t.Errorf("45 s old: %+v", s.Process)
	}

	// 46 s old is stale.
	set(e.now.Add(-46 * time.Second))
	s := get()
	if s.Process.State != "unknown" || s.Process.LastReported != "running" ||
		s.Process.ObservedAt == nil || !s.Process.ObservedAt.Equal(e.now.Add(-46*time.Second)) {
		t.Errorf("46 s old: %+v", s.Process)
	}
	var stored string
	if err := e.owner.QueryRow(context.Background(),
		`SELECT process_state FROM waystone.servers WHERE server_id = $1`, serverA2).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != "running" {
		t.Errorf("stored process_state = %q, want running (never rewritten)", stored)
	}
}
