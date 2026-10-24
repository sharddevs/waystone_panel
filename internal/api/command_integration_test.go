// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// Stage 7b: POST /api/v1/servers/{id}/command (notes/stage7-scope.md D2,
// D4) and migration 00010's rules for the line.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func command(server string) string { return "/api/v1/servers/" + server + "/command" }

func commandBody(line string) string {
	b, _ := json.Marshal(map[string]string{"command": line})
	return string(b)
}

// running marks a server running, as a fresh report would.
func (e *env) running(t *testing.T, server string) {
	t.Helper()
	mustExec(t, e.owner, `UPDATE waystone.servers SET process_state = 'running', process_since = $1, process_observed_at = $1 WHERE server_id = $2`,
		e.now.Add(-time.Second), server)
}

func (e *env) operationCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.owner.QueryRow(context.Background(), `SELECT count(*) FROM waystone.operations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCommand(t *testing.T) {
	e := setup(t)
	e.running(t, serverA1)
	var op operationJSON
	rec := e.do(t, req{method: http.MethodPost, path: command(serverA1), body: commandBody("say hi é"), uid: uidAdminA, key: "k1"})
	wantOK(t, rec, http.StatusAccepted, &op)
	if op.Action != "server.command" || op.State != "pending" || op.Command != "say hi é" || op.ServerID != serverA1 {
		t.Fatalf("operation %+v", op)
	}
	var config string
	if err := e.owner.QueryRow(context.Background(), `SELECT config::text FROM waystone.operations WHERE operation_id = $1`, op.ID).
		Scan(&config); err != nil || config != `{"command": "say hi é"}` {
		t.Errorf("config %q (%v)", config, err)
	}
	// It reads back the same, alone and in the list.
	var got operationJSON
	wantOK(t, e.do(t, req{method: http.MethodGet, path: "/api/v1/operations/" + op.ID, uid: uidAdminA}), http.StatusOK, &got)
	if got.Command != "say hi é" {
		t.Errorf("GET: %+v", got)
	}
	// A replay gets the same operation; the same key for another line
	// doesn't.
	var again operationJSON
	wantOK(t, e.do(t, req{method: http.MethodPost, path: command(serverA1), body: commandBody("say hi é"), uid: uidAdminA, key: "k1"}),
		http.StatusOK, &again)
	if again.ID != op.ID {
		t.Errorf("replay made %s", again.ID)
	}
	wantError(t, e.do(t, req{method: http.MethodPost, path: command(serverA1), body: commandBody("say bye"), uid: uidAdminA, key: "k1"}),
		http.StatusUnprocessableEntity, "idempotency_key_reused")
	// One at a time, like every operation.
	wantError(t, e.do(t, req{method: http.MethodPost, path: command(serverA1), body: commandBody("say bye"), uid: uidAdminA}),
		http.StatusConflict, "operation_in_progress")
	// Other actions never carry a line.
	e.setState(t, op.ID, "running", "succeeded")
	var stopOp operationJSON
	wantOK(t, e.do(t, req{method: http.MethodPost, path: stop(serverA1), uid: uidAdminA}), http.StatusAccepted, &stopOp)
	if stopOp.Command != "" || strings.Contains(e.do(t, req{method: http.MethodGet, path: "/api/v1/operations/" + stopOp.ID, uid: uidAdminA}).Body.String(), `"command"`) {
		t.Errorf("stop has a command: %+v", stopOp)
	}
	// The line never reaches the API's log.
	if strings.Contains(e.logs.String(), "say hi") {
		t.Error("the line is in the API's log")
	}
}

func TestCommandRefusals(t *testing.T) {
	e := setup(t)
	e.running(t, serverA1)
	for _, line := range []string{"", "/stop", "say a\nstop", "say\x00", "say \u202eboo", "say\x1b[2J", "say\u0085", strings.Repeat("a", 257)} {
		wantError(t, e.do(t, req{method: http.MethodPost, path: command(serverA1), body: commandBody(line), uid: uidAdminA}),
			http.StatusBadRequest, "invalid_command")
	}
	for _, body := range []string{`{}`, `{"command": 5}`, `{"command": "say hi", "as": "root"}`, `[]`} {
		rec := e.do(t, req{method: http.MethodPost, path: command(serverA1), body: body, uid: uidAdminA})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", body, rec.Code)
		}
	}
	wantError(t, e.do(t, req{method: http.MethodPost, path: "/api/v1/servers/nope/command", body: commandBody("say hi"), uid: uidAdminA}),
		http.StatusBadRequest, "bad_request")
	wantError(t, e.do(t, req{method: http.MethodPost, path: command(serverB1), body: commandBody("say hi"), uid: uidAdminA}),
		http.StatusNotFound, "not_found")
	// Not running: refused before anything is recorded.
	wantError(t, e.do(t, req{method: http.MethodPost, path: command(serverA2), body: commandBody("say hi"), uid: uidAdminA}),
		http.StatusConflict, "server_not_running")
	// Not an admin: forbidden, with an audit row that holds no line.
	wantError(t, e.do(t, req{method: http.MethodPost, path: command(serverA1), body: commandBody("say secret"), uid: uidNodeRole}),
		http.StatusForbidden, "forbidden")
	var n int
	if err := e.owner.QueryRow(context.Background(),
		`SELECT count(*) FROM waystone.audit_events WHERE action = 'server.command' AND outcome = 'denied' AND resource_id = $1
		   AND NOT (to_jsonb(audit_events)::text LIKE '%secret%')`, serverA1).Scan(&n); err != nil || n != 1 {
		t.Errorf("%d denial rows (%v), want 1", n, err)
	}
	if got := e.operationCount(t); got != 0 {
		t.Errorf("%d operations, want none", got)
	}
}

// 00010: the CHECK holds for the owner too, config never changes, and
// the app can't put config on another action.
func TestCommandSchema(t *testing.T) {
	e := setup(t)
	e.running(t, serverA1)
	var op operationJSON
	wantOK(t, e.do(t, req{method: http.MethodPost, path: command(serverA1), body: commandBody("say hi"), uid: uidAdminA}), http.StatusAccepted, &op)

	ctx := context.Background()
	insert := `INSERT INTO waystone.operations (tenant_id, operation_id, resource_type, resource_id, node_id, action, state,
		actor_type, actor_id, actor_name, requested_at, config)
		VALUES ($1, gen_random_uuid(), 'server', $2, $3, $4, 'failed', 'system', 'test', 'test', now(), $5)`
	for name, c := range map[string]struct{ action, config string }{
		"newline":       {"server.command", `{"command": "say a\nstop"}`},
		"slash":         {"server.command", `{"command": "/stop"}`},
		"bidi":          {"server.command", `{"command": "say \u202e"}`},
		"long":          {"server.command", `{"command": "` + strings.Repeat("a", 257) + `"}`},
		"empty":         {"server.command", `{"command": ""}`},
		"number":        {"server.command", `{"command": 5}`},
		"extra key":     {"server.command", `{"command": "say hi", "as": "root"}`},
		"no config":     {"server.command", ""},
		"stop with one": {"server.stop", `{"command": "say hi"}`},
	} {
		var config any
		if c.config != "" {
			config = c.config
		}
		if _, err := e.owner.Exec(ctx, insert, tenantA, serverA2, nodeA, c.action, config); err == nil {
			t.Errorf("%s: inserted", name)
		}
	}
	// Not even the owner with the override changes a line.
	tx, err := e.owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SET LOCAL waystone.owner_override = 'on'`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE waystone.operations SET config = '{"command": "op Mallory"}' WHERE operation_id = $1`, op.ID); err == nil ||
		!strings.Contains(err.Error(), "identity columns") {
		t.Errorf("owner update: %v", err)
	}
	tx.Rollback(ctx)
	// The app has no UPDATE grant on it, and INSERT only on the listed
	// columns.
	var update, insertConfig, insertClaim bool
	if err := e.owner.QueryRow(ctx, `SELECT has_column_privilege('waystone_app', 'waystone.operations', 'config', 'UPDATE'),
		has_column_privilege('waystone_app', 'waystone.operations', 'config', 'INSERT'),
		has_column_privilege('waystone_app', 'waystone.operations', 'claimed_epoch', 'INSERT')`).Scan(&update, &insertConfig, &insertClaim); err != nil {
		t.Fatal(err)
	}
	if update || !insertConfig || insertClaim {
		t.Errorf("grants: update config %v, insert config %v, insert claimed_epoch %v", update, insertConfig, insertClaim)
	}
}
