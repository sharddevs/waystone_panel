// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// Stage 4c, Q7: a new operation wakes the node's agent session, once it
// has committed. A replay, a conflict or a refusal wakes nothing.

import (
	"context"
	"io"
	"log"
	"net/http"
	"testing"

	"github.com/sharddevs/waystone_panel/internal/store"
)

func TestOnOperation(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	st, err := store.Open(ctx, appDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	type kick struct {
		node, op  string
		committed bool
	}
	var kicks []kick
	e.h = New(Config{Version: "test", Store: st, Lookup: fakeLookup, Log: log.New(io.Discard, "", 0),
		OnOperation: func(op store.Operation) {
			// The owner's own connection only sees committed rows.
			var n int
			if err := e.owner.QueryRow(ctx, `SELECT count(*) FROM waystone.operations WHERE operation_id = $1`, op.ID).Scan(&n); err != nil {
				t.Error(err)
			}
			kicks = append(kicks, kick{node: op.NodeID, op: op.ID, committed: n == 1})
		}})

	var op operationJSON
	wantOK(t, e.do(t, req{method: "POST", path: start(serverA1), uid: uidAdminA, body: "{}", key: "k1"}), http.StatusAccepted, &op)
	if len(kicks) != 1 || kicks[0] != (kick{node: nodeA, op: op.ID, committed: true}) {
		t.Fatalf("kicks = %+v, want one for node A, after the commit", kicks)
	}

	// A replay, a conflict and a refusal: no more kicks.
	wantOK(t, e.do(t, req{method: "POST", path: start(serverA1), uid: uidAdminA, body: "{}", key: "k1"}), http.StatusOK, &op)
	wantError(t, e.do(t, req{method: "POST", path: stop(serverA1), uid: uidAdminA, body: "{}"}), http.StatusConflict, CodeOperationInProgress)
	wantError(t, e.do(t, req{method: "POST", path: start(serverA2), uid: uidNodeRole, body: "{}"}), http.StatusForbidden, CodeForbidden)
	if len(kicks) != 1 {
		t.Errorf("kicks = %+v, want still one", kicks)
	}
}
