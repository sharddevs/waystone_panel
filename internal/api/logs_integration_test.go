// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// Stage 7a: GET /api/v1/servers/{id}/logs (notes/stage7-scope.md D1, D4).

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sharddevs/waystone_panel/internal/serverlog"
)

const logCursor = "s=0123456789abcdef0123456789abcdef;i=1a2;b=fedcba9876543210fedcba9876543210;m=9f;t=5f1e2d3c4b5a6;x=77aa"

// fakeLogs records reads and answers with page and err.
type fakeLogs struct {
	mu    sync.Mutex
	calls []string
	page  serverlog.Page
	err   error
}

func (f *fakeLogs) read(_ context.Context, nodeID, serverID, after string, limit int) (serverlog.Page, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("%s %s %q %d", nodeID, serverID, after, limit))
	return f.page, f.err
}

func (f *fakeLogs) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func logsPath(server, query string) string {
	p := "/api/v1/servers/" + server + "/logs"
	if query != "" {
		p += "?" + query
	}
	return p
}

func TestServerLogs(t *testing.T) {
	f := &fakeLogs{page: serverlog.Page{Cursor: logCursor, More: true, Lines: []serverlog.Line{
		{Time: time.Date(2026, 10, 24, 12, 0, 1, 500000000, time.UTC), Priority: 6, Text: "[Server thread/INFO]: Done (3.2s)!"},
	}}}
	e := setupWith(t, f.read)

	rec := e.do(t, req{method: http.MethodGet, path: logsPath(serverA1, ""), uid: uidAdminA})
	var page struct {
		Lines []struct {
			Time     string `json:"time"`
			Priority int    `json:"priority"`
			Text     string `json:"text"`
		} `json:"lines"`
		Cursor *string `json:"cursor"`
		More   bool    `json:"more"`
	}
	wantOK(t, rec, http.StatusOK, &page)
	if len(page.Lines) != 1 || page.Lines[0].Time != "2026-10-24T12:00:01.5Z" || page.Lines[0].Priority != 6 ||
		page.Lines[0].Text != "[Server thread/INFO]: Done (3.2s)!" || page.Cursor == nil || *page.Cursor != logCursor || !page.More {
		t.Errorf("page %+v", page)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control %q, want no-store", cc)
	}
	rec = e.do(t, req{method: http.MethodGet, path: logsPath(serverA1, "after="+strings.ReplaceAll(logCursor, ";", "%3B")+"&limit=50"), uid: uidAdminA})
	if rec.Code != http.StatusOK {
		t.Fatalf("after and limit: %d %s", rec.Code, rec.Body.String())
	}
	want := []string{nodeA + " " + serverA1 + ` "" 200`, nodeA + " " + serverA1 + ` "` + logCursor + `" 50`}
	if got := f.got(); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("reads %q, want %q", got, want)
	}
	// The lines go nowhere else: not the API's log, not the database.
	if strings.Contains(e.logs.String(), "Done (3.2s)") {
		t.Error("a line is in the API's log")
	}
	var n int
	if err := e.owner.QueryRow(context.Background(), `SELECT count(*) FROM waystone.audit_events`).Scan(&n); err != nil || n != 0 {
		t.Errorf("%d audit rows (%v), want none for a read", n, err)
	}

	// Nothing logged yet: no lines, a null cursor.
	f.page = serverlog.Page{}
	rec = e.do(t, req{method: http.MethodGet, path: logsPath(serverA2, ""), uid: uidAdminA})
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"lines":[],"cursor":null,"more":false}` {
		t.Errorf("empty: %d %s", rec.Code, rec.Body.String())
	}
}

func TestServerLogsRefusals(t *testing.T) {
	f := &fakeLogs{}
	e := setupWith(t, f.read)
	bad := []string{
		"limit=0", "limit=201", "limit=x", "limit=", "limit=5&limit=6",
		"after=", "after=--since%3Dtoday", "after=S%3D1", "after=" + logCursor[:4] + "&after=" + logCursor[:4],
		"verbose=1",
	}
	for _, q := range bad {
		wantError(t, e.do(t, req{method: http.MethodGet, path: logsPath(serverA1, q), uid: uidAdminA}), http.StatusBadRequest, "bad_request")
	}
	wantError(t, e.do(t, req{method: http.MethodGet, path: "/api/v1/servers/nope/logs", uid: uidAdminA}), http.StatusBadRequest, "bad_request")
	// Another tenant's server is the same as a missing one.
	wantError(t, e.do(t, req{method: http.MethodGet, path: logsPath(serverB1, ""), uid: uidAdminA}), http.StatusNotFound, "not_found")
	wantError(t, e.do(t, req{method: http.MethodGet, path: logsPath(missingServer, ""), uid: uidAdminA}), http.StatusNotFound, "not_found")
	// Not an admin: forbidden, with an audit row.
	wantError(t, e.do(t, req{method: http.MethodGet, path: logsPath(serverA1, ""), uid: uidNodeRole}), http.StatusForbidden, "forbidden")
	var n int
	if err := e.owner.QueryRow(context.Background(),
		`SELECT count(*) FROM waystone.audit_events WHERE action = 'server.logs.read' AND outcome = 'denied' AND resource_type = 'server' AND resource_id = $1`,
		serverA1).Scan(&n); err != nil || n != 1 {
		t.Errorf("%d denial rows (%v), want 1", n, err)
	}
	wantError(t, e.do(t, req{method: http.MethodGet, path: logsPath(serverA1, ""), uid: uidStranger}), http.StatusForbidden, "forbidden")
	if ev := e.securityEvents(); len(ev) != 1 || !strings.Contains(ev[0], "action=server.logs.read") {
		t.Errorf("security events %q", ev)
	}
	// A POST isn't a route.
	if rec := e.do(t, req{method: http.MethodPost, path: logsPath(serverA1, ""), body: "{}", uid: uidAdminA}); rec.Code == http.StatusOK {
		t.Errorf("POST: %d", rec.Code)
	}
	if got := f.got(); len(got) != 0 {
		t.Errorf("reads %q, want none", got)
	}

	// A revoked node can't be asked.
	mustExec(t, e.owner, `UPDATE waystone.nodes SET revoked_at = now() WHERE node_id = $1`, nodeA)
	wantError(t, e.do(t, req{method: http.MethodGet, path: logsPath(serverA1, ""), uid: uidAdminA}), http.StatusConflict, "node_revoked")
	if got := f.got(); len(got) != 0 {
		t.Errorf("reads %q, want none", got)
	}
}

func TestServerLogsErrors(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{serverlog.ErrUnreachable, http.StatusConflict, "node_unreachable"},
		{fmt.Errorf("x: %w", serverlog.ErrBusy), http.StatusServiceUnavailable, "controller_busy"},
		{serverlog.ErrUnavailable, http.StatusServiceUnavailable, "logs_unavailable"},
		{context.DeadlineExceeded, http.StatusInternalServerError, "internal"},
	}
	for _, c := range cases {
		e := setupWith(t, (&fakeLogs{err: c.err}).read)
		wantError(t, e.do(t, req{method: http.MethodGet, path: logsPath(serverA1, ""), uid: uidAdminA}), c.status, c.code)
	}
	// Without an agent link, every node is unreachable.
	e := setup(t)
	wantError(t, e.do(t, req{method: http.MethodGet, path: logsPath(serverA1, ""), uid: uidAdminA}), http.StatusConflict, "node_unreachable")
}
