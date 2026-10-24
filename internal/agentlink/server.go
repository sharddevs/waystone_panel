// SPDX-License-Identifier: AGPL-3.0-only

// Package agentlink is the controller's end of the agent link (stage 4b-2,
// notes/stage4b-scope.md v2.1 and notes/stage4b2-scope.md): the gRPC
// server on the agent socket, its peer-uid handshake, the session map, and
// the handler that turns an agent's hello and reports into store calls.
//
// Since stage 6a it can also serve agents on other machines over TCP with
// mutual TLS and pinned keys (notes/stage6-scope.md D1-D5), and lets a new
// node enroll there.
//
// Who the agent is and which node it speaks for come from the transport:
// on agent.sock the kernel proves the uid and the node is the controller's
// own configuration (R3); over TCP the node is the one whose key the TLS
// handshake proved. Nothing in a message can change either.
package agentlink

import (
	"context"
	"crypto/tls"
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
	"github.com/sharddevs/waystone_panel/internal/agenttls"
	"github.com/sharddevs/waystone_panel/internal/ids"
	"github.com/sharddevs/waystone_panel/internal/peercred"
	"github.com/sharddevs/waystone_panel/internal/profile/minecraft"
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
	// MsgVersionMismatch: the agent's version isn't the controller's
	// (D4). The agent waits before trying again.
	MsgVersionMismatch = "version_mismatch"
)

// Causes for cancelling a session's context.
var (
	errReplaced = errors.New(MsgSessionReplaced)
	errShutdown = errors.New(MsgShuttingDown)
	errRevoked  = errors.New(MsgNodeRevoked)
)

// Config is what the controller hands the link. At least one of the two
// transports must be set up: agent.sock (NodeID and AgentUID) or TCP
// (TLS).
type Config struct {
	Store *store.Store
	// NodeID is the node this controller's own agent speaks for on
	// agent.sock (-agent-node), the only source of that node actor (R3).
	// Empty means no agent.sock.
	NodeID string
	// AgentUID is the only uid the agent.sock handshake accepts
	// (-agent-uid).
	AgentUID uint32
	// TLS is the controller's key for the TCP listener (6a). nil means
	// no TCP listener.
	TLS *tls.Certificate
	// Version is the controller's version. An agent must say hello with
	// exactly this one (D4).
	Version string
	// ArtifactDir is the cache FetchArtifact serves from (6c-2);
	// DefaultArtifactDir when empty. Artifacts is the pinned set;
	// minecraft.Pinned when nil (tests pin a small one).
	ArtifactDir string
	Artifacts   []minecraft.Artifact
	// Fetch asks for an artifact the cache doesn't have and returns where
	// the fetched copy waits (6c-3): fetch.Ask with the fetch service's
	// socket. nil means a missing jar is artifact_unavailable.
	Fetch func(ctx context.Context, art minecraft.Artifact) (string, error)

	// For tests; zero values mean the real thing.
	Nodes            NodeLookup                     // Store
	UIDOf            func(net.Conn) (uint32, error) // peercred.UID
	Log              *log.Logger                    // log.Default()
	Now              func() time.Time               // time.Now, for the churn window and log limits
	HelloTimeout     time.Duration                  // HelloTimeout
	ReportInterval   time.Duration                  // ReportInterval; whole seconds
	UnpinnedLifetime time.Duration                  // UnpinnedLifetime
	LogTimeout       time.Duration                  // LogTimeout
	LogGap           time.Duration                  // LogGap

	// Crash-point hooks for tests (4c, F2 and S2). nil in production.
	// TestBeforeClaim runs inside the claim transaction; an error rolls
	// it back, like a failed claim. TestAfterClaim runs after the claim
	// commits and before the Send; an error stands for a Send that
	// failed, or a controller that died right there.
	TestBeforeClaim func() error
	TestAfterClaim  func(store.ClaimedOperation) error
}

// NodeLookup finds a connection's node before any tenant is known
// (*store.Store).
type NodeLookup interface {
	AgentNodeByPin(ctx context.Context, pin [32]byte) (store.AgentNode, error)
	AgentNodeByID(ctx context.Context, nodeID string) (store.AgentNode, error)
}

// Server is the agent link: one gRPC server with one service.
type Server struct {
	agentv1.UnimplementedAgentLinkServiceServer

	store *store.Store
	nodes NodeLookup
	// localNode is the configured agent.sock node, or "".
	localNode string
	version   string
	// artifactDir and artifacts: FetchArtifact's cache and pins (6c-2).
	artifactDir string
	artifacts   []minecraft.Artifact
	fetch       func(ctx context.Context, art minecraft.Artifact) (string, error)
	// fillSem lets one fill run at a time; shared by every copy.
	fillSem chan struct{}
	// nodeID and actor are the node of one stream. They are set only on
	// the copy forNode makes for that stream, never on the shared Server.
	nodeID         string
	actor          store.Actor
	log            *log.Logger
	now            func() time.Time
	helloTimeout   time.Duration
	reportInterval time.Duration
	logTimeout     time.Duration
	logGap         time.Duration

	grpc    *grpc.Server // agent.sock; nil without one
	grpcTCP *grpc.Server // the TCP listener; nil without one
	slots   *slots
	// enrollPerIP is Enroll's per-address limit (D2, 6H7).
	enrollPerIP *window
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

// New builds the link. It refuses a node ID that isn't a canonical UUID,
// a config with neither transport, no version, and a report interval
// that isn't whole seconds.
func New(cfg Config) (*Server, error) {
	if cfg.NodeID != "" {
		if err := ids.Check(cfg.NodeID); err != nil {
			return nil, fmt.Errorf("agent node: %w", err)
		}
	}
	if cfg.NodeID == "" && cfg.TLS == nil {
		return nil, errors.New("agent link: neither agent.sock nor TCP is set up")
	}
	if cfg.Store == nil {
		return nil, errors.New("agent link: no store")
	}
	if cfg.Version == "" {
		return nil, errors.New("agent link: no version")
	}
	s := &Server{
		store:          cfg.Store,
		nodes:          cfg.Nodes,
		localNode:      cfg.NodeID,
		version:        cfg.Version,
		artifactDir:    cfg.ArtifactDir,
		artifacts:      cfg.Artifacts,
		fetch:          cfg.Fetch,
		fillSem:        make(chan struct{}, 1),
		log:            cfg.Log,
		now:            cfg.Now,
		helloTimeout:   cfg.HelloTimeout,
		reportInterval: cfg.ReportInterval,
		logTimeout:     cfg.LogTimeout,
		logGap:         cfg.LogGap,
		sessions:       newSessions(),
		churn:          newChurn(),
		slots:          newSlots(MaxUnpinnedPerIP, MaxHandshaking, MaxEnrollOnly, MaxPinned),
		enrollPerIP:    newWindow(enrollPerIP, enrollWindow),

		testBeforeClaim: cfg.TestBeforeClaim,
		testAfterClaim:  cfg.TestAfterClaim,
	}
	if cfg.UnpinnedLifetime > 0 {
		s.slots.lifetime = cfg.UnpinnedLifetime
	}
	s.base, s.cancelBase = context.WithCancelCause(context.Background())
	if s.log == nil {
		s.log = log.Default()
	}
	if s.nodes == nil {
		s.nodes = cfg.Store
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.artifactDir == "" {
		s.artifactDir = DefaultArtifactDir
	}
	if s.artifacts == nil {
		s.artifacts = minecraft.Pinned
	}
	if s.helloTimeout == 0 {
		s.helloTimeout = HelloTimeout
	}
	if s.reportInterval == 0 {
		s.reportInterval = ReportInterval
	}
	if s.logTimeout == 0 {
		s.logTimeout = LogTimeout
	}
	if s.logGap == 0 {
		s.logGap = LogGap
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
	// Both servers share every limit and the authorizer; only the
	// handshake differs. Only TCP serves Enroll.
	opts := func(c grpc.ServerOption) []grpc.ServerOption {
		return []grpc.ServerOption{
			c,
			grpc.ConnectionTimeout(HandshakeTimeout),
			grpc.MaxRecvMsgSize(MaxMessageBytes),
			grpc.MaxSendMsgSize(MaxMessageBytes),
			grpc.KeepaliveParams(keepalive.ServerParameters{Time: keepaliveTime, Timeout: keepaliveTimeout}),
			grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: keepaliveMinTime, PermitWithoutStream: true}),
			grpc.UnaryInterceptor(s.unaryAuthz),
			grpc.StreamInterceptor(s.streamAuthz),
		}
	}
	if cfg.NodeID != "" {
		s.grpc = grpc.NewServer(opts(grpc.Creds(creds))...)
		agentv1.RegisterAgentLinkServiceServer(s.grpc, s)
	}
	if cfg.TLS != nil {
		lookup := func(ctx context.Context, pin agenttls.Pin) (store.AgentNode, error) {
			return s.nodes.AgentNodeByPin(ctx, pin)
		}
		// A node needs one Connect stream; a few more cover a reconnect
		// racing the old stream's end (6a-H4).
		tcpOpts := append(opts(grpc.Creds(newTLSCreds(*cfg.TLS, lookup, s.logf))), grpc.MaxConcurrentStreams(MaxStreamsPerConn))
		s.grpcTCP = grpc.NewServer(tcpOpts...)
		agentv1.RegisterAgentLinkServiceServer(s.grpcTCP, s)
		agentv1.RegisterEnrollServiceServer(s.grpcTCP, &enrollServer{s: s})
	}
	return s, nil
}

func (s *Server) logf(format string, args ...any) { s.log.Printf(format, args...) }

// Serve accepts the local agent on agent.sock l, at most MaxConns at a
// time, until Stop.
func (s *Server) Serve(l net.Listener) error {
	if s.grpc == nil {
		return errors.New("agent link: agent.sock is not set up")
	}
	return s.grpc.Serve(newLimitListener(l, MaxConns))
}

// ServeTCP accepts agents on the TCP listener l, with the connection
// pools' limits (6H1), until Stop.
func (s *Server) ServeTCP(l net.Listener) error {
	if s.grpcTCP == nil {
		return errors.New("agent link: TCP is not set up")
	}
	return s.grpcTCP.Serve(&slotListener{Listener: l, slots: s.slots})
}

// Revoke ends nodeID's live session at once, if it has one (D5). The
// agent sees node_revoked. Call it after the revocation commits: any
// later call is refused by the authorizer, and any later write by the
// database (the node has no membership any more).
func (s *Server) Revoke(nodeID string) {
	s.sessions.end(nodeID, errRevoked)
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
	var servers []*grpc.Server
	for _, g := range []*grpc.Server{s.grpc, s.grpcTCP} {
		if g != nil {
			servers = append(servers, g)
		}
	}
	done := make(chan struct{})
	go func() {
		var wg sync.WaitGroup
		for _, g := range servers {
			wg.Go(g.GracefulStop)
		}
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		for _, g := range servers {
			g.Stop()
		}
		<-done
		return ctx.Err()
	}
}

// forNode is the Server as one stream's node sees it.
func (s *Server) forNode(n store.AgentNode) *Server {
	c := *s
	c.nodeID = n.NodeID
	c.actor = n.Actor()
	return &c
}

// streamError is a status that ends the stream with a short reason.
func streamError(code codes.Code, reason string) error {
	return status.Error(code, reason)
}

// Connect is one agent's stream: a Hello first, then Reports until the
// agent leaves, the session is replaced or revoked, or something is wrong
// with what it sent. The authorizer has already found its node.
func (s *Server) Connect(stream agentv1.AgentLinkService_ConnectServer) error {
	n, ok := nodeFrom(stream.Context())
	if !ok {
		return errNotPermitted
	}
	return s.forNode(n).connect(stream)
}

func (s *Server) connect(stream agentv1.AgentLinkService_ConnectServer) error {
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
	// D4: v0.1 agents upgrade together with their controller. Only a
	// proven node gets this far, so the version is no outsider's to see.
	if hello.GetAgentVersion() != s.version {
		s.logf("agent_link=refused node=%s reason=%s agent_version=%q", s.nodeID, MsgVersionMismatch, clip(hello.GetAgentVersion(), 64))
		return streamError(codes.FailedPrecondition, MsgVersionMismatch)
	}
	if !store.ValidAgentInfo(hello.GetAgentVersion(), hello.GetCapabilities()) {
		return s.refuse(codes.InvalidArgument, "invalid_hello")
	}

	var epoch int64
	var serverIDs []string
	var ports map[string]uint32
	err := s.store.InTenantTx(ctx, s.actor, func(ctx context.Context, tx *store.Tx) error {
		var err error
		epoch, serverIDs, ports, err = tx.NodeHello(ctx)
		if err != nil {
			return err
		}
		return tx.SetAgentInfo(ctx, hello.GetAgentVersion(), hello.GetCapabilities())
	})
	if errors.Is(err, store.ErrTooManyServers) {
		return s.refuse(codes.FailedPrecondition, "too_many_servers")
	}
	if errors.Is(err, store.ErrNoMembership) {
		// Revoked between the authorizer's look and here.
		return s.refuse(codes.PermissionDenied, MsgNodeRevoked)
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
	d := newDispatcher(s, epoch, stream, serverIDs)
	dispCtx, stopDispatch := context.WithCancel(sessCtx)
	d.ended = dispCtx.Done()
	s.sessions.setKick(s.nodeID, epoch, d.signal)
	s.sessions.setLogReader(s.nodeID, epoch, d.readLog)
	d.signal() // claim right after the hello (Q7)
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
	case m.GetLogChunk() != nil:
		return d.logChunk(m.GetLogChunk())
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
	if errors.Is(err, store.ErrNoMembership) {
		// Revoked: the node can't write any more (6M2).
		return s.refuse(codes.PermissionDenied, MsgNodeRevoked)
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
	case errRevoked:
		s.logf("agent_session=revoked node=%s epoch=%d", s.nodeID, epoch)
		return streamError(codes.PermissionDenied, MsgNodeRevoked)
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
