// SPDX-License-Identifier: AGPL-3.0-only

package service

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/sharddevs/waystone_panel/internal/ids"
	"github.com/sharddevs/waystone_panel/internal/serverlog"
	"github.com/sharddevs/waystone_panel/internal/store"
	"github.com/sharddevs/waystone_panel/internal/view"
)

// Stage 7a (notes/stage7-scope.md D1, D4): a server's log, read through
// its node's agent. Admins only. The lines are passed through and never
// stored, logged or audited; only a refusal leaves an audit row.

const actionServerLogsRead = "server.logs.read"

// LogReader reads a server's log on its node (agentlink.Server.ReadLog).
// Its errors are serverlog's.
type LogReader func(ctx context.Context, nodeID, serverID, after string, limit int) (serverlog.Page, error)

// handleServerLogs: GET /api/v1/servers/{id}/logs?after=&limit= → 200
// with a view.LogPage. Without after, the newest lines; with it, the
// lines after that cursor. limit is 1 to 200, 200 without it. Each
// parameter at most once. Another tenant's server is not_found; a node
// that isn't connected is node_unreachable, one that couldn't read the
// log logs_unavailable.
func (s *Service) handleServerLogs(w http.ResponseWriter, r *http.Request) {
	if err := checkQuery(r, "after", "limit"); err != nil {
		s.writeError(w, r, err)
		return
	}
	q := r.URL.Query()
	for _, v := range q {
		if len(v) != 1 {
			s.writeError(w, r, newError(CodeBadRequest, errors.New("repeated query parameter")))
			return
		}
	}
	id := r.PathValue("id")
	if err := ids.Check(id); err != nil {
		s.writeError(w, r, newError(CodeBadRequest, err))
		return
	}
	after := q.Get("after")
	if q.Has("after") && !serverlog.ValidCursor(after) {
		s.writeError(w, r, newError(CodeBadRequest, errors.New("bad cursor")))
		return
	}
	limit := serverlog.DefaultLines
	if q.Has("limit") {
		n, err := strconv.Atoi(q.Get("limit"))
		if err != nil || n < 1 || n > serverlog.MaxLines {
			s.writeError(w, r, newError(CodeBadRequest, nil))
			return
		}
		limit = n
	}

	var server store.Server
	err := s.run(r, actionServerLogsRead, "server", id, func(ctx context.Context, tx *store.Tx) error {
		var err error
		server, err = tx.GetServer(ctx, id)
		if err == nil && server.NodeRevoked {
			return store.ErrNodeRevoked
		}
		return err
	})
	if err != nil {
		s.writeError(w, r, errorFor(err))
		return
	}
	if s.readLog == nil {
		s.writeError(w, r, newError(CodeNodeUnreachable, errors.New("no agent link")))
		return
	}
	page, err := s.readLog(r.Context(), server.NodeID, id, after, limit)
	if err != nil {
		s.writeError(w, r, logError(err))
		return
	}
	// Game logs hold player names, addresses and chat: nothing keeps them.
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, view.LogPageOf(page))
}

func logError(err error) *Error {
	switch {
	case errors.Is(err, serverlog.ErrUnreachable):
		return newError(CodeNodeUnreachable, err)
	case errors.Is(err, serverlog.ErrBusy):
		return newError(CodeControllerBusy, err)
	case errors.Is(err, serverlog.ErrUnavailable):
		return newError(CodeLogsUnavailable, err)
	}
	return newError(CodeInternal, err)
}
