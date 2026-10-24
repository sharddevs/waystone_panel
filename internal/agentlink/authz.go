// SPDX-License-Identifier: AGPL-3.0-only

package agentlink

import (
	"context"
	"errors"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/sharddevs/waystone_panel/internal/store"
)

// The authorizer (6M1, 6M2): every call is checked against a fixed table
// before its handler runs, and anything not in the table is refused. The
// node a call acts for is looked up afresh for each call, so a revoked
// node is refused at its next call, not only at its next handshake.

// The reasons an agent sees.
const (
	MsgNotPermitted = "not_permitted"
	MsgNodeRevoked  = "node_revoked"
)

const (
	methodConnect = "/waystone.agent.v1.AgentLinkService/Connect"
	methodEnroll  = "/waystone.agent.v1.EnrollService/Enroll"
)

// rule is who may make a call.
type rule int

const (
	ruleDeny   rule = iota // the zero value: an unlisted method
	ruleNode               // a node that isn't revoked
	ruleEnroll             // any TCP peer; the token is the authority
)

var methodRules = map[string]rule{
	methodConnect: ruleNode,
	methodEnroll:  ruleEnroll,
}

// nodeKey carries the call's node in its context. Only the authorizer
// sets it.
type nodeKey struct{}

func nodeFrom(ctx context.Context) (store.AgentNode, bool) {
	n, ok := ctx.Value(nodeKey{}).(store.AgentNode)
	return n, ok
}

var errNotPermitted = streamError(codes.PermissionDenied, MsgNotPermitted)

// resolveNode finds the node a call acts for, by how its connection was
// authenticated:
//   - agent.sock: the kernel proved the agent's uid (the handshake), and
//     the node is the configured one (R3), if it isn't revoked (6H6);
//   - TCP: the node whose key is pinned as the peer's, if it isn't
//     revoked.
func (s *Server) resolveNode(ctx context.Context) (store.AgentNode, error) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return store.AgentNode{}, errNotPermitted
	}
	var n store.AgentNode
	var err error
	switch auth := p.AuthInfo.(type) {
	case PeerAuth:
		if s.localNode == "" {
			return store.AgentNode{}, errNotPermitted
		}
		n, err = s.nodes.AgentNodeByID(ctx, s.localNode)
	case TLSAuth:
		n, err = s.nodes.AgentNodeByPin(ctx, auth.Pin)
	default:
		return store.AgentNode{}, errNotPermitted
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		return store.AgentNode{}, errNotPermitted
	case err != nil:
		s.logf("agent_link=lookup_failed error=%q", err.Error())
		return store.AgentNode{}, streamError(codes.Unavailable, "lookup_failed")
	case n.Revoked: // pin: revoked is refused
		return store.AgentNode{}, streamError(codes.PermissionDenied, MsgNodeRevoked)
	}
	return n, nil
}

func isTCP(ctx context.Context) bool {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return false
	}
	_, ok = p.AuthInfo.(TLSAuth)
	return ok
}

// unaryAuthz admits Enroll from a TCP peer, and nothing else.
func (s *Server) unaryAuthz(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	if methodRules[info.FullMethod] == ruleEnroll && isTCP(ctx) {
		return handler(ctx, req)
	}
	s.refusedCall(info.FullMethod)
	return nil, errNotPermitted
}

// streamAuthz admits Connect from a node, with the node in the context.
func (s *Server) streamAuthz(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	if methodRules[info.FullMethod] != ruleNode {
		s.refusedCall(info.FullMethod)
		return errNotPermitted
	}
	n, err := s.resolveNode(ss.Context())
	if err == nil {
		err = handler(srv, &nodeStream{ServerStream: ss, ctx: context.WithValue(ss.Context(), nodeKey{}, n)})
	} else if s.logLimit.allow("refused-call/" + info.FullMethod) {
		s.logf("agent_link=refused method=%s error=%q (at most one line a minute)", info.FullMethod, err.Error())
	}
	dropIfRevoked(ss.Context(), err)
	return err
}

// dropIfRevoked closes a TCP connection soon after its node is refused as
// revoked, so a revoked key doesn't keep a pinned slot for as long as it
// likes. If it connects again, it gets an enroll-only slot.
func dropIfRevoked(ctx context.Context, err error) {
	if st, ok := status.FromError(err); !ok || st.Code() != codes.PermissionDenied || st.Message() != MsgNodeRevoked {
		return
	}
	if p, ok := peer.FromContext(ctx); ok {
		if a, ok := p.AuthInfo.(TLSAuth); ok && a.conn != nil {
			a.conn.closeSoon()
		}
	}
}

func (s *Server) refusedCall(method string) {
	if s.logLimit.allow("refused-call/" + clip(method, 100)) {
		s.logf("agent_link=refused method=%q reason=%s (at most one line a minute)", clip(method, 100), MsgNotPermitted)
	}
}

// nodeStream is a stream whose context carries its node.
type nodeStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *nodeStream) Context() context.Context { return s.ctx }
