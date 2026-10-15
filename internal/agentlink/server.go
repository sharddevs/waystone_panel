// SPDX-License-Identifier: AGPL-3.0-only

// Package agentlink is the controller's end of the agent link (stage 4b-2,
// notes/stage4b-scope.md v2.1 and notes/stage4b2-scope.md): the gRPC
// server on the agent socket, its peer-uid handshake, the session map, and
// the handler that turns an agent's hello and reports into store calls.
//
// Who the agent is comes from the kernel (the handshake) and which node it
// speaks for comes from the controller's own configuration (R3). Nothing
// in a message can change either.
package agentlink

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"

	agentv1 "github.com/sharddevs/waystone_panel/gen/waystone/agent/v1"
	"github.com/sharddevs/waystone_panel/internal/ids"
	"github.com/sharddevs/waystone_panel/internal/peercred"
	"github.com/sharddevs/waystone_panel/internal/store"
)

// Limits from v2.1 (R7, R8).
const (
	// MaxMessageBytes caps every message, both ways (I12, R7).
	MaxMessageBytes = 64 << 10
	// HandshakeTimeout is gRPC's ConnectionTimeout: the peer check plus
	// the HTTP/2 setup.
	HandshakeTimeout = 5 * time.Second
	// HelloTimeout is how long a new stream has to send its Hello.
	HelloTimeout = 10 * time.Second
	// ReportInterval is what the HelloReply tells the agent.
	ReportInterval = 15 * time.Second
	// Keepalive: a ping every 30 s, and a connection silent for 90 s in
	// total is dropped.
	keepaliveTime    = 30 * time.Second
	keepaliveTimeout = 60 * time.Second
	// Agents may ping at most this often (the agent pings every 30 s).
	keepaliveMinTime = 15 * time.Second
)

// The status messages an agent can rely on. session_replaced is the one
// that matters: it switches the agent to the long backoff (R6).
const (
	MsgSessionReplaced = "session_replaced"
	MsgShuttingDown    = "shutting_down"
)

// Causes for cancelling a session's context.
var (
	errReplaced = errors.New(MsgSessionReplaced)
	errShutdown = errors.New(MsgShuttingDown)
)

// Config is what the controller hands the link.
type Config struct {
	Store *store.Store
	// NodeID is the node this controller's agent speaks for (-agent-node),
	// the only source of the node actor (R3).
	NodeID string
	// AgentUID is the only uid the handshake accepts (-agent-uid).
	AgentUID uint32

	// For tests; zero values mean the real thing.
	UIDOf          func(net.Conn) (uint32, error) // peercred.UID
	Log            *log.Logger                    // log.Default()
	Now            func() time.Time               // time.Now, for the churn window and log limits
	HelloTimeout   time.Duration                  // HelloTimeout
	ReportInterval time.Duration                  // ReportInterval; whole seconds

	// Crash-point hooks for tests (4c, F2 and S2). nil in production.
	// TestBeforeClaim runs inside the claim transaction; an error rolls
	// it back, like a failed claim. TestAfterClaim runs after the claim
	// commits and before the Send; an error stands for a Send that
	// failed, or a controller that died right there.
	TestBeforeClaim func() error
	TestAfterClaim  func(store.ClaimedOperation) error
}

// Server is the agent link: one gRPC server with one service.
type Server struct {
	agentv1.UnimplementedAgentLinkServiceServer

	store          *store.Store
	nodeID         string
	actor          store.Actor
	log            *log.Logger
	now            func() time.Time
	helloTimeout   time.Duration
	reportInterval time.Duration

	grpc *grpc.Server
	// base is cancelled by CancelSessions. Every stream watches it, so
	// shutdown also ends streams that haven't said hello yet.
	base       context.Context
	cancelBase context.CancelCauseFunc
	sessions   *sessions
	churn      *churn
	logLimit   *logLimit

	testBeforeClaim func() error
	testAfterClaim  func(store.ClaimedOperation) error
}

// New builds the link. It refuses a node ID that isn't a canonical UUID
// and a report interval that isn't whole seconds.
func New(cfg Config) (*Server, error) {
	if err := ids.Check(cfg.NodeID); err != nil {
		return nil, fmt.Errorf("agent node: %w", err)
	}
	if cfg.Store == nil {
		return nil, errors.New("agent link: no store")
	}
	s := &Server{
		store:          cfg.Store,
		nodeID:         cfg.NodeID,
		actor:          store.Actor{Type: store.ActorNode, ID: cfg.NodeID, Name: "node " + cfg.NodeID},
		log:            cfg.Log,
		now:            cfg.Now,
		helloTimeout:   cfg.HelloTimeout,
		reportInterval: cfg.ReportInterval,
		sessions:       newSessions(),
		churn:          newChurn(),

		testBeforeClaim: cfg.TestBeforeClaim,
		testAfterClaim:  cfg.TestAfterClaim,
	}
	s.base, s.cancelBase = context.WithCancelCause(context.Background())
	if s.log == nil {
		s.log = log.Default()
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.helloTimeout == 0 {
		s.helloTimeout = HelloTimeout
	}
	if s.reportInterval == 0 {
		s.reportInterval = ReportInterval
	}
	if s.reportInterval < time.Second || s.reportInterval%time.Second != 0 {
		return nil, errors.New("agent link: report interval must be whole seconds")
	}
	s.logLimit = newLogLimit(time.Minute, s.now)

	uidOf := cfg.UIDOf
	if uidOf == nil {
		uidOf = peercred.UID
	}
	creds := &serverCreds{
		agentUID: cfg.AgentUID,
		uidOf:    uidOf,
		refused: func(uid uint32, err error) {
			if s.logLimit.allow("refused-peer") {
				s.logf("agent_link=refused_peer uid=%d error=%q (at most one line a minute)", uid, err.Error())
			}
		},
	}
	s.grpc = grpc.NewServer(
		grpc.Creds(creds),
		grpc.ConnectionTimeout(HandshakeTimeout),
		grpc.MaxRecvMsgSize(MaxMessageBytes),
		grpc.MaxSendMsgSize(MaxMessageBytes),
		grpc.KeepaliveParams(keepalive.ServerParameters{Time: keepaliveTime, Timeout: keepaliveTimeout}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: keepaliveMinTime, PermitWithoutStream: true}),
	)
	agentv1.RegisterAgentLinkServiceServer(s.grpc, s)
	return s, nil
}

func (s *Server) logf(format string, args ...any) { s.log.Printf(format, args...) }

// Serve accepts agents on l, at most MaxConns at a time, until Stop.
func (s *Server) Serve(l net.Listener) error {
	return s.grpc.Serve(newLimitListener(l, MaxConns))
}

// Kick tells node's live session, if any, that it may have a new pending
// operation (Q7). The API calls it after an operation commits. It never
// blocks, and a kick for another node or with no session does nothing:
// the next hello or report claims anyway.
func (s *Server) Kick(nodeID string) {
	s.sessions.kick(nodeID)
}

// CancelSessions ends every stream, with or without a session yet (R10
// step 1), and every stream that opens after it. The agents see
// UNAVAILABLE and reconnect with their normal backoff.
func (s *Server) CancelSessions() {
	s.cancelBase(errShutdown)
}

// Stop waits for the streams to finish until ctx is done, then cuts them
// off (R10 steps 2 and 3). Call CancelSessions first: these streams never
// end on their own. It reports whether it had to cut them off.
func (s *Server) Stop(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		s.grpc.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		s.grpc.Stop()
		<-done
		return ctx.Err()
	}
}

// streamError is a status that ends the stream with a short reason.
func streamError(code codes.Code, reason string) error {
	return status.Error(code, reason)
}

// Connect is one agent's stream: a Hello first, then Reports until the
// agent leaves, the session is replaced, or something is wrong with what
// it sent.
func (s *Server) Connect(stream agentv1.AgentLinkService_ConnectServer) error {
	ctx, cancel := context.WithCancelCause(stream.Context())
	defer cancel(nil)
	stopWatching := context.AfterFunc(s.base, func() { cancel(errShutdown) })
	defer stopWatching()

	// One goroutine reads, so the handler can wait for a message, the
	// Hello deadline and its own cancellation at the same time. It ends
	// when the stream does, which happens when this handler returns.
	msgs := make(chan *agentv1.ConnectRequest)
	recvErr := make(chan error, 1)
	go func() {
		for {
			m, err := stream.Recv()
			if err != nil {
				recvErr <- err
				return
			}
			select {
			case msgs <- m:
			case <-ctx.Done():
				return
			}
		}
	}()

	// The Hello.
	timer := time.NewTimer(s.helloTimeout)
	defer timer.Stop()
	var first *agentv1.ConnectRequest
	select {
	case first = <-msgs:
	case err := <-recvErr:
		return endOfStream(err)
	case <-timer.C:
		s.logf("agent_link=refused reason=hello_timeout")
		return streamError(codes.DeadlineExceeded, "hello_timeout")
	case <-ctx.Done():
		return s.sessionEnded(ctx, 0)
	}
	if hasUnknownFields(first.ProtoReflect()) {
		return s.refuse(codes.InvalidArgument, "unknown_field")
	}
	hello := first.GetHello()
	if hello == nil {
		if first.GetReport() != nil {
			return s.refuse(codes.FailedPrecondition, "hello_required")
		}
		return s.refuse(codes.InvalidArgument, "unknown_message")
	}
	// Compared, never used (R3, F8).
	if hello.GetNodeId() != s.nodeID {
		return s.refuse(codes.FailedPrecondition, "node_mismatch")
	}

	var epoch int64
	var serverIDs []string
	var ports map[string]uint32
	err := s.store.InTenantTx(ctx, s.actor, func(ctx context.Context, tx *store.Tx) error {
		var err error
		epoch, serverIDs, ports, err = tx.NodeHello(ctx)
		return err
	})
	if errors.Is(err, store.ErrTooManyServers) {
		return s.refuse(codes.FailedPrecondition, "too_many_servers")
	}
	if err != nil {
		s.logf("agent_link=hello_failed node=%s error=%q", s.nodeID, err.Error())
		return streamError(codes.Unavailable, "hello_failed")
	}

	// R1: the session only goes live if no newer one beat it to the map.
	sessCtx, cancelSession := context.WithCancelCause(ctx)
	defer cancelSession(nil)
	won, replaced := s.sessions.install(s.nodeID, epoch, cancelSession)
	if !won {
		s.logf("agent_session=lost_race node=%s epoch=%d", s.nodeID, epoch)
		return streamError(codes.Aborted, MsgSessionReplaced)
	}
	defer s.sessions.remove(s.nodeID, epoch)
	if replaced {
		s.noteTakeover(ctx, epoch)
	}

	err = stream.Send(&agentv1.ConnectResponse{Msg: &agentv1.ConnectResponse_HelloReply{HelloReply: &agentv1.HelloReply{
		Epoch:                 epoch,
		ServerIds:             serverIDs,
		ReportIntervalSeconds: uint32(s.reportInterval / time.Second),
		GamePorts:             ports,
	}}})
	if err != nil {
		return err
	}
	s.logf("agent_session=started node=%s epoch=%d servers=%d agent_version=%q",
		s.nodeID, epoch, len(serverIDs), clip(hello.GetAgentVersion(), 64))

	// From here on only the dispatcher sends (S2). The handler waits for
	// it before returning, so nothing is sent on a finished stream.
	d := newDispatcher(s, epoch, stream)
	s.sessions.setKick(s.nodeID, epoch, d.signal)
	d.signal() // claim right after the hello (Q7)
	dispCtx, stopDispatch := context.WithCancel(sessCtx)
	dispErr := make(chan error, 1)
	var dispDone sync.WaitGroup
	dispDone.Go(func() { dispErr <- d.run(dispCtx) })
	defer dispDone.Wait()
	defer stopDispatch()

	var lastSeq int64
	for {
		select {
		case <-sessCtx.Done():
			return s.sessionEnded(sessCtx, epoch)
		case err := <-recvErr:
			s.logf("agent_session=ended node=%s epoch=%d", s.nodeID, epoch)
			return endOfStream(err)
		case err := <-dispErr:
			if sessCtx.Err() != nil {
				return s.sessionEnded(sessCtx, epoch)
			}
			if errors.Is(err, store.ErrSessionSuperseded) {
				s.logf("agent_session=superseded node=%s epoch=%d", s.nodeID, epoch)
				return streamError(codes.Aborted, MsgSessionReplaced)
			}
			return streamError(codes.Unavailable, "send_failed")
		case m := <-msgs:
			if err := s.message(sessCtx, d, &lastSeq, m); err != nil {
				if sessCtx.Err() != nil {
					return s.sessionEnded(sessCtx, epoch)
				}
				return err
			}
		}
	}
}

// message handles one message after the Hello. Any error ends the stream.
func (s *Server) message(ctx context.Context, d *dispatcher, lastSeq *int64, m *agentv1.ConnectRequest) error {
	if hasUnknownFields(m.ProtoReflect()) {
		return s.refuse(codes.InvalidArgument, "unknown_field")
	}
	switch {
	case m.GetReport() != nil:
		if err := s.report(ctx, d.epoch, lastSeq, m.GetReport()); err != nil {
			return err
		}
		d.signal() // the safety net (Q7)
		return nil
	case m.GetResult() != nil:
		return d.result(ctx, m.GetResult())
	case m.GetProgress() != nil:
		return d.progress(ctx, m.GetProgress())
	case m.GetHello() != nil:
		return s.refuse(codes.FailedPrecondition, "unexpected_hello")
	}
	return s.refuse(codes.InvalidArgument, "unknown_message")
}

// report handles one Report.
func (s *Server) report(ctx context.Context, epoch int64, lastSeq *int64, wire *agentv1.Report) error {
	r, err := toStoreReport(wire)
	if err != nil {
		return s.refuse(codes.InvalidArgument, "invalid_report")
	}
	if err := store.ValidateReport(r); err != nil {
		return s.refuse(codes.InvalidArgument, "invalid_report")
	}
	if r.Epoch != epoch {
		return s.refuse(codes.InvalidArgument, "wrong_epoch")
	}
	// C2: gaps are fine, repeats and going back are not.
	if r.Seq <= *lastSeq {
		return s.refuse(codes.InvalidArgument, "seq_not_increasing")
	}

	var accepted, rejected int
	err = s.store.InTenantTx(ctx, s.actor, func(ctx context.Context, tx *store.Tx) error {
		var err error
		accepted, rejected, err = tx.AcceptReport(ctx, r)
		return err
	})
	if errors.Is(err, store.ErrSessionSuperseded) {
		s.logf("agent_session=superseded node=%s epoch=%d", s.nodeID, epoch)
		return streamError(codes.Aborted, MsgSessionReplaced)
	}
	if err != nil {
		if ctx.Err() == nil {
			s.logf("agent_link=report_failed node=%s epoch=%d error=%q", s.nodeID, epoch, err.Error())
		}
		return streamError(codes.Unavailable, "report_failed")
	}
	*lastSeq = r.Seq

	if rejected > 0 && s.logLimit.allow("rejected") {
		s.logf("agent_report=entries_rejected node=%s epoch=%d seq=%d accepted=%d rejected=%d (at most one line a minute)",
			s.nodeID, epoch, r.Seq, accepted, rejected)
	}
	// native_state is only for this: saying why a server is unknown.
	for _, e := range wire.GetEntries() {
		if e.GetProcessState() == agentv1.ProcessState_PROCESS_STATE_UNKNOWN && s.logLimit.allow("unknown/"+e.GetServerId()) {
			s.logf("agent_report=unknown node=%s server=%s native_state=%q", s.nodeID, e.GetServerId(), e.GetNativeState())
		}
	}
	return nil
}

// refuse logs why a stream is being closed and returns its status.
func (s *Server) refuse(code codes.Code, reason string) error {
	s.logf("agent_link=refused node=%s reason=%s", s.nodeID, reason)
	return streamError(code, reason)
}

// sessionEnded turns the session's cancellation into the agent's status.
func (s *Server) sessionEnded(sessCtx context.Context, epoch int64) error {
	switch context.Cause(sessCtx) {
	case errReplaced:
		s.logf("agent_session=replaced node=%s epoch=%d", s.nodeID, epoch)
		return streamError(codes.Aborted, MsgSessionReplaced)
	case errShutdown:
		return streamError(codes.Unavailable, MsgShuttingDown)
	}
	return status.FromContextError(sessCtx.Err()).Err()
}

// noteTakeover counts a takeover and, the first time a node crosses the
// limit in a window, writes the audit row and a warning (C4).
func (s *Server) noteTakeover(ctx context.Context, epoch int64) {
	if !s.churn.note(s.nodeID, s.now()) {
		return
	}
	s.logf("WARNING agent_session=churn node=%s epoch=%d takeovers_over=%d window=%s",
		s.nodeID, epoch, churnLimit, churnWindow)
	err := s.store.InTenantTx(ctx, s.actor, func(ctx context.Context, tx *store.Tx) error {
		return tx.RecordChurn(ctx, epoch)
	})
	if err != nil {
		s.logf("agent_link=churn_audit_failed node=%s error=%q", s.nodeID, err.Error())
	}
}

// endOfStream: the agent closing its side is a normal end.
func endOfStream(err error) error {
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

// clip shortens s to at most n bytes for a log line; %q escapes the rest.
func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
