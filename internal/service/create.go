// SPDX-License-Identifier: AGPL-3.0-only

package service

import (
	"context"
	"net/http"

	"github.com/sharddevs/waystone_panel/internal/ids"
	"github.com/sharddevs/waystone_panel/internal/profile/minecraft"
	"github.com/sharddevs/waystone_panel/internal/store"
	"github.com/sharddevs/waystone_panel/internal/view"
)

// Creating a server (6c-1, D7). Installing it again and accepting the
// EULA are actions like start (handleAction).

const actionServerCreate = "server.create"

// CreateServerRequest is the body of POST /api/v1/servers. The version
// is the pinned one, so it isn't asked for; motd may be left out.
type CreateServerRequest struct {
	NodeID  string `json:"node_id"`
	Name    string `json:"name"`
	Port    uint32 `json:"port"`
	HeapMiB uint32 `json:"heap_mib"`
	Motd    string `json:"motd"`
}

// CreatedServer is the answer to a create: the server and its first
// install operation.
type CreatedServer struct {
	Server    view.Server    `json:"server"`
	Operation view.Operation `json:"operation"`
}

const createRoute = "/api/v1/servers"

// handleCreateServer: POST /api/v1/servers → 201 with the server and its
// install operation, Location /api/v1/servers/<id>, after OnOperation
// has been told; a replay (same Idempotency-Key, same request) is 200
// with them as they are now. The body is checked here with the profile's
// own rules and again by the store and the database.
func (s *Service) handleCreateServer(w http.ResponseWriter, r *http.Request) {
	if err := checkQuery(r); err != nil {
		s.writeError(w, r, err)
		return
	}
	var body CreateServerRequest
	if err := decodeJSON(w, r, &body); err != nil {
		s.writeError(w, r, err)
		return
	}
	props := minecraft.Properties{ServerPort: body.Port, Motd: body.Motd, MaxPlayers: minecraft.DefaultMaxPlayers}
	if ids.Check(body.NodeID) != nil || !store.ValidServerName(body.Name) || props.Validate() != nil ||
		body.HeapMiB < store.MinHeapMiB || body.HeapMiB > store.MaxHeapMiB {
		s.writeError(w, r, newError(CodeBadRequest, nil))
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if key != "" && !store.ValidIdempotencyKey(key) {
		s.writeError(w, r, newError(CodeInvalidIdempotencyKey, nil))
		return
	}
	fingerprint, err := Fingerprint(r.Method, createRoute, "", body)
	if err != nil {
		s.writeError(w, r, newError(CodeInternal, err))
		return
	}
	in := store.NewServer{
		NodeID: body.NodeID, Name: body.Name, Version: minecraft.PinnedVersion,
		Port: body.Port, HeapMiB: body.HeapMiB, Motd: body.Motd,
		RequestID: requestIDFrom(r.Context()), IdempotencyKey: key, RequestSHA256: fingerprint,
	}
	if s.agentLink != nil {
		in.ReservedPort = uint32(s.agentLink.Addr.Port())
	}

	var server store.Server
	var op store.Operation
	var replayed bool
	var role string
	err = s.run(r, actionServerCreate, "node", body.NodeID, func(ctx context.Context, tx *store.Tx) error {
		var err error
		role = tx.Role()
		server, op, replayed, err = tx.CreateServer(ctx, in)
		return err
	})
	if err != nil {
		s.writeError(w, r, errorFor(err))
		return
	}
	status := http.StatusOK
	if !replayed {
		status = http.StatusCreated
		w.Header().Set("Location", "/api/v1/servers/"+server.ID)
		if s.onOperation != nil {
			s.onOperation(op)
		}
	}
	writeJSON(w, status, CreatedServer{Server: view.ServerOf(server, permits(role), s.now()), Operation: view.OperationOf(op)})
}
