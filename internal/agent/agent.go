// SPDX-License-Identifier: AGPL-3.0-only

// Package agent is the node side of the agent link (stage 4b-2): it dials
// the controller, says hello, and every report interval asks the helper
// for each server's STATUS and reports it. In 4b it never starts or stops
// anything and sends nothing but Hello and Report.
package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/sharddevs/waystone_panel/gen/waystone/agent/v1"
	"github.com/sharddevs/waystone_panel/internal/ids"
	"github.com/sharddevs/waystone_panel/internal/peercred"
)

// Limits shared with the controller (R7, R8, R9).
const (
	maxMessageBytes  = 64 << 10
	maxServers       = 256
	keepaliveTime    = 30 * time.Second
	keepaliveTimeout = 60 * time.Second
	callTimeout      = 3 * time.Second
	sweepTimeout     = 10 * time.Second
	// The controller sends 15 s. Anything outside this range is a bug.
	maxReportInterval = time.Hour
)

// msgSessionReplaced must match the controller's status message.
const msgSessionReplaced = "session_replaced"

// Config is what cmd/waystone-agent hands the agent.
type Config struct {
	// ControllerSocket is the controller's agent socket (-controller).
	ControllerSocket string
	// ControllerUID is the only uid the agent will talk to (R2).
	ControllerUID uint32
	// NodeID is sent in the Hello, where the controller compares it with
	// its own configuration.
	NodeID  string
	Version string
	Helper  Helper

	// For tests; zero values mean the real thing.
	Dial         func(ctx context.Context) (net.Conn, error)      // a Unix dial of ControllerSocket
	UIDOf        func(net.Conn) (uint32, error)                   // peercred.UID
	Log          *log.Logger                                      // log.Default()
	Now          func() time.Time                                 // time.Now, for agent_time
	Sleep        func(ctx context.Context, d time.Duration) error // waits d or until ctx is done
	Rand         func() float64                                   // math/rand/v2's Float64, for jitter
	CallTimeout  time.Duration                                    // 3 s per STATUS call
	SweepTimeout time.Duration                                    // 10 s per sweep
}

// Agent runs the connect loop.
type Agent struct {
	controllerUID uint32
	nodeID        string
	version       string
	helper        Helper
	dial          func(ctx context.Context) (net.Conn, error)
	uidOf         func(net.Conn) (uint32, error)
	log           *log.Logger
	now           func() time.Time
	sleep         func(ctx context.Context, d time.Duration) error
	backoff       backoff
	callTimeout   time.Duration
	sweepTimeout  time.Duration
}

// New checks the configuration and builds the agent.
func New(cfg Config) (*Agent, error) {
	if err := ids.Check(cfg.NodeID); err != nil {
		return nil, fmt.Errorf("node: %w", err)
	}
	if cfg.Helper == nil {
		return nil, errors.New("agent: no helper")
	}
	if cfg.Dial == nil && cfg.ControllerSocket == "" {
		return nil, errors.New("agent: no controller socket")
	}
	a := &Agent{
		controllerUID: cfg.ControllerUID,
		nodeID:        cfg.NodeID,
		version:       cfg.Version,
		helper:        cfg.Helper,
		dial:          cfg.Dial,
		uidOf:         cfg.UIDOf,
		log:           cfg.Log,
		now:           cfg.Now,
		sleep:         cfg.Sleep,
		callTimeout:   cfg.CallTimeout,
		sweepTimeout:  cfg.SweepTimeout,
	}
	if a.dial == nil {
		path := cfg.ControllerSocket
		a.dial = func(ctx context.Context) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		}
	}
	if a.uidOf == nil {
		a.uidOf = peercred.UID
	}
	if a.log == nil {
		a.log = log.Default()
	}
	if a.now == nil {
		a.now = time.Now
	}
	if a.sleep == nil {
		a.sleep = sleep
	}
	a.backoff.rand = cfg.Rand
	if a.backoff.rand == nil {
		a.backoff.rand = rand.Float64
	}
	if a.callTimeout == 0 {
		a.callTimeout = callTimeout
	}
	if a.sweepTimeout == 0 {
		a.sweepTimeout = sweepTimeout
	}
	return a, nil
}

func (a *Agent) logf(format string, args ...any) { a.log.Printf(format, args...) }

// Run connects, reports, and reconnects with backoff until ctx is done.
func (a *Agent) Run(ctx context.Context) error {
	for {
		healthy, err := a.session(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if healthy {
			a.backoff.reset()
		}
		replaced := isReplaced(err)
		wait := a.backoff.next(replaced)
		a.logf("agent=disconnected replaced=%t error=%q retry_in=%s", replaced, errText(err), wait.Round(time.Millisecond))
		if err := a.sleep(ctx, wait); err != nil {
			return nil
		}
	}
}

// isReplaced reports whether the controller ended the session because
// another agent took over this node (R6).
func isReplaced(err error) bool {
	st, ok := status.FromError(err)
	return ok && st.Code() == codes.Aborted && st.Message() == msgSessionReplaced
}

// session is one connection: hello, then reports until the stream ends.
// healthy says it lasted a full report interval, which resets the backoff.
func (a *Agent) session(ctx context.Context) (healthy bool, err error) {
	cc, err := grpc.NewClient("passthrough:///waystone-controller",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return a.dial(ctx) }),
		grpc.WithTransportCredentials(&clientCreds{controllerUID: a.controllerUID, uidOf: a.uidOf}),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxMessageBytes), grpc.MaxCallSendMsgSize(maxMessageBytes)),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: keepaliveTime, Timeout: keepaliveTimeout, PermitWithoutStream: true}),
	)
	if err != nil {
		return false, err
	}
	defer cc.Close()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := agentv1.NewAgentLinkServiceClient(cc).Connect(ctx)
	if err != nil {
		return false, err
	}
	err = stream.Send(&agentv1.ConnectRequest{Msg: &agentv1.ConnectRequest_Hello{Hello: &agentv1.Hello{
		NodeId:       a.nodeID,
		AgentVersion: a.version,
	}}})
	if err != nil {
		return false, recvStatus(stream, err)
	}
	m, err := stream.Recv()
	if err != nil {
		return false, err
	}
	reply, err := checkHelloReply(m)
	if err != nil {
		return false, err
	}
	interval := time.Duration(reply.GetReportIntervalSeconds()) * time.Second
	serverIDs := reply.GetServerIds()
	a.logf("agent=connected epoch=%d servers=%d interval=%s", reply.GetEpoch(), len(serverIDs), interval)

	// Nothing else is expected from the controller in 4b. The reader only
	// waits for the stream to end and says how.
	ended := make(chan error, 1)
	go func() {
		m, err := stream.Recv()
		if err == nil {
			err = fmt.Errorf("unexpected message from the controller: %T", m.GetMsg())
			cancel()
		}
		ended <- err
	}()

	var seq int64
	send := func() error {
		seq++
		sweepID, err := ids.NewV7()
		if err != nil {
			return err
		}
		entries := a.sweep(ctx, sweepID, serverIDs)
		a.logUnknown(sweepID, entries)
		err = stream.Send(&agentv1.ConnectRequest{Msg: &agentv1.ConnectRequest_Report{Report: &agentv1.Report{
			Epoch:     reply.GetEpoch(),
			Seq:       seq,
			AgentTime: timestamppb.New(a.now()),
			Entries:   entries,
		}}})
		if errors.Is(err, io.EOF) {
			// The controller closed the stream; its status comes
			// with the receive side.
			return <-ended
		}
		return err
	}

	if err := send(); err != nil {
		return false, err
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := send(); err != nil {
				return healthy, err
			}
			healthy = true
		case err := <-ended:
			return healthy, err
		case <-ctx.Done():
			_ = stream.CloseSend()
			return healthy, ctx.Err()
		}
	}
}

// recvStatus: a failed Send only says io.EOF; the real reason is the
// status the receive side gets.
func recvStatus(stream agentv1.AgentLinkService_ConnectClient, err error) error {
	if !errors.Is(err, io.EOF) {
		return err
	}
	_, rerr := stream.Recv()
	if rerr == nil {
		return err
	}
	return rerr
}

// checkHelloReply refuses anything but a well-formed HelloReply (R12 on
// the agent's side too).
func checkHelloReply(m *agentv1.ConnectResponse) (*agentv1.HelloReply, error) {
	if hasUnknownFields(m.ProtoReflect()) {
		return nil, errors.New("hello reply has unknown fields")
	}
	reply := m.GetHelloReply()
	if reply == nil {
		return nil, errors.New("expected a hello reply")
	}
	if reply.GetEpoch() < 1 {
		return nil, errors.New("hello reply without an epoch")
	}
	interval := time.Duration(reply.GetReportIntervalSeconds()) * time.Second
	if interval < time.Second || interval > maxReportInterval {
		return nil, fmt.Errorf("report interval %s out of range", interval)
	}
	if len(reply.GetServerIds()) > maxServers {
		return nil, fmt.Errorf("%d servers, at most %d", len(reply.GetServerIds()), maxServers)
	}
	seen := make(map[string]bool, len(reply.GetServerIds()))
	for _, id := range reply.GetServerIds() {
		if err := ids.Check(id); err != nil || seen[id] {
			return nil, errors.New("hello reply has a bad or repeated server ID")
		}
		seen[id] = true
	}
	return reply, nil
}

// logUnknown writes one line per sweep that had failures (R9: the agent
// logs them), with the first one as an example.
func (a *Agent) logUnknown(sweepID string, entries []*agentv1.ReportEntry) {
	n := 0
	var first *agentv1.ReportEntry
	for _, e := range entries {
		if e.GetProcessState() == agentv1.ProcessState_PROCESS_STATE_UNKNOWN {
			if first == nil {
				first = e
			}
			n++
		}
	}
	if n > 0 {
		a.logf("agent=status_unknown sweep=%s servers=%d first=%s reason=%q", sweepID, n, first.GetServerId(), first.GetNativeState())
	}
}

// hasUnknownFields reports whether m, or any message inside it, carries
// fields this build doesn't know (R12). The controller has the same check.
func hasUnknownFields(m protoreflect.Message) bool {
	if len(m.GetUnknown()) > 0 {
		return true
	}
	found := false
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case fd.IsList() && fd.Message() != nil:
			list := v.List()
			for i := 0; i < list.Len() && !found; i++ {
				found = hasUnknownFields(list.Get(i).Message())
			}
		case fd.IsMap() && fd.MapValue().Message() != nil:
			v.Map().Range(func(_ protoreflect.MapKey, mv protoreflect.Value) bool {
				found = hasUnknownFields(mv.Message())
				return !found
			})
		case fd.Message() != nil && !fd.IsList() && !fd.IsMap():
			found = hasUnknownFields(v.Message())
		}
		return !found
	})
	return found
}

// sleep waits d, or less if ctx ends first.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func errText(err error) string {
	if err == nil {
		return "stream closed"
	}
	return err.Error()
}
