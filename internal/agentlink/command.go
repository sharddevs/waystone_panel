// SPDX-License-Identifier: AGPL-3.0-only

package agentlink

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"

	agentv1 "github.com/sharddevs/waystone_panel/gen/waystone/agent/v1"
	"github.com/sharddevs/waystone_panel/internal/store"
)

// Stage 7b (notes/stage7-scope.md D2): an agent running a console command
// asks for its line. Like GetInstallSpec, it answers only for a
// server.command running on the calling node, for the server it names,
// and refuses everything else with the same not_permitted. The node comes
// from the authorizer, never from the request.

// GetCommandSpec answers with the line of a running console command.
func (s *Server) GetCommandSpec(ctx context.Context, req *agentv1.GetCommandSpecRequest) (*agentv1.GetCommandSpecResponse, error) {
	n, ok := nodeFrom(ctx)
	if !ok {
		return nil, errNotPermitted
	}
	if hasUnknownFields(req.ProtoReflect()) {
		return nil, streamError(codes.InvalidArgument, "unknown_field")
	}
	var spec store.CommandSpec
	err := s.store.InTenantTx(ctx, n.Actor(), func(ctx context.Context, tx *store.Tx) error {
		var err error
		spec, err = tx.RunningCommand(ctx, req.GetOperationId())
		return err
	})
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.refusedCall(methodGetCommandSpec)
		return nil, errNotPermitted
	case errors.Is(err, store.ErrNoMembership):
		return nil, streamError(codes.PermissionDenied, MsgNodeRevoked)
	case err != nil:
		if ctx.Err() == nil {
			s.logf("agent_link=command_lookup_failed node=%s error=%q", n.NodeID, err.Error())
		}
		return nil, streamError(codes.Unavailable, "lookup_failed")
	}
	if spec.ServerID != req.GetServerId() {
		s.refusedCall(methodGetCommandSpec)
		return nil, errNotPermitted
	}
	return &agentv1.GetCommandSpecResponse{Command: spec.Command}, nil
}
