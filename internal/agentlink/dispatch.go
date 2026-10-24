// SPDX-License-Identifier: AGPL-3.0-only

package agentlink

import (
	"context"
	"errors"
	"sync"

	"google.golang.org/grpc/codes"

	agentv1 "github.com/sharddevs/waystone_panel/gen/waystone/agent/v1"
	"github.com/sharddevs/waystone_panel/internal/store"
)

// Stage 4c (notes/stage4c-scope.md v2.1, S2): each session has exactly
// one dispatcher goroutine, and it is the only code that claims an
// operation or sends a command. Everything else only wakes it up.

// MaxInFlight is how many commands one session may have out at once (Q3).
const MaxInFlight = 4

// sentOp is one command this session sent and hasn't had a result for.
type sentOp struct {
	serverID string
	action   store.Action
	starting bool // a restart that has reported Progress(STARTING)
	// phase is an install's last phase: claimed in provisioning, then
	// fetching, then configuring (6c-2).
	phase string
}

// dispatcher belongs to one session.
type dispatcher struct {
	s      *Server
	epoch  int64
	stream agentv1.AgentLinkService_ConnectServer

	// wake holds at most one signal. A signal that finds one already
	// waiting is dropped: the dispatcher claims until there's nothing
	// left or no slot free, so one signal covers any number of triggers.
	wake chan struct{}

	// The in-flight set. Only the dispatcher adds, before Send; only a
	// result removes. Its size is the number of slots in use.
	mu       sync.Mutex
	inflight map[string]*sentOp

	// known is the agent's inventory: the hello's servers, and every
	// ServerAdded since (6c-2). Only the dispatcher uses it.
	known map[string]bool
}

func newDispatcher(s *Server, epoch int64, stream agentv1.AgentLinkService_ConnectServer, serverIDs []string) *dispatcher {
	d := &dispatcher{
		s:        s,
		epoch:    epoch,
		stream:   stream,
		wake:     make(chan struct{}, 1),
		inflight: make(map[string]*sentOp),
		known:    make(map[string]bool, len(serverIDs)),
	}
	for _, id := range serverIDs {
		d.known[id] = true
	}
	return d
}

// signal wakes the dispatcher. It never blocks.
func (d *dispatcher) signal() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// errSendFailed: the stream broke under a Send. The operation stays
// running, and the next hello or the startup recovery makes it uncertain.
var errSendFailed = errors.New("send failed")

// run waits for signals and claims until ctx ends. Its error ends the
// session: a superseded session, or a broken stream.
func (d *dispatcher) run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-d.wake:
		}
		if err := d.drain(ctx); err != nil {
			return err
		}
	}
}

func (d *dispatcher) inUse() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.inflight)
}

// drain claims and sends while a slot is free and something is pending.
func (d *dispatcher) drain(ctx context.Context) error {
	for ctx.Err() == nil && d.inUse() < MaxInFlight {
		// The slot is reserved simply by being the only goroutine that
		// adds to the set: nobody else can take it while we claim.
		var op store.ClaimedOperation
		var ok bool
		err := d.s.store.InTenantTx(ctx, d.s.actor, func(ctx context.Context, tx *store.Tx) error {
			if d.s.testBeforeClaim != nil {
				if err := d.s.testBeforeClaim(); err != nil {
					return err
				}
			}
			var err error
			op, ok, err = tx.ClaimNext(ctx, d.epoch)
			return err
		})
		if errors.Is(err, store.ErrSessionSuperseded) {
			return store.ErrSessionSuperseded
		}
		if err != nil {
			// Nothing was committed, so the slot is free again. Wait for
			// the next signal (a report brings one within 15 s) instead
			// of retrying in a tight loop.
			if ctx.Err() == nil {
				d.s.logf("agent_dispatch=claim_failed node=%s epoch=%d error=%q", d.s.nodeID, d.epoch, err.Error())
			}
			return nil
		}
		if !ok {
			return nil
		}

		// In the set before Send: a result can arrive the moment the
		// command is out.
		d.mu.Lock()
		sent := &sentOp{serverID: op.ServerID, action: op.Action}
		if op.Action == store.ActionInstall {
			sent.phase = "provisioning"
		}
		d.inflight[op.OperationID] = sent
		d.mu.Unlock()

		if d.s.testAfterClaim != nil {
			if err := d.s.testAfterClaim(op); err != nil {
				return errSendFailed
			}
		}
		// A server made after the hello (6c-2): the agent learns of it
		// first, on the same stream, so the command finds it.
		if !d.known[op.ServerID] {
			err = d.stream.Send(&agentv1.ConnectResponse{Msg: &agentv1.ConnectResponse_ServerAdded{ServerAdded: &agentv1.ServerAdded{
				Epoch: d.epoch, ServerId: op.ServerID, GamePort: op.GamePort,
			}}})
			if err != nil {
				d.s.logf("agent_dispatch=send_failed node=%s epoch=%d op=%s", d.s.nodeID, d.epoch, op.OperationID)
				return errSendFailed
			}
			d.known[op.ServerID] = true
		}
		err = d.stream.Send(&agentv1.ConnectResponse{Msg: &agentv1.ConnectResponse_Command{Command: &agentv1.Command{
			Epoch:       d.epoch,
			OperationId: op.OperationID,
			ServerId:    op.ServerID,
			Action:      commandActions[op.Action],
		}}})
		if err != nil {
			// Never resent, on this session or any other (I9).
			d.s.logf("agent_dispatch=send_failed node=%s epoch=%d op=%s", d.s.nodeID, d.epoch, op.OperationID)
			return errSendFailed
		}
		d.s.logf("agent_dispatch=sent node=%s epoch=%d op=%s server=%s action=%s",
			d.s.nodeID, d.epoch, op.OperationID, op.ServerID, op.Action)
	}
	return nil
}

// sent returns this session's entry for an operation, if it has one.
func (d *dispatcher) sent(opID string) (*sentOp, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	op, ok := d.inflight[opID]
	return op, ok
}

// result handles a Result (Q6, C3). Any error ends the stream.
func (d *dispatcher) result(ctx context.Context, r *agentv1.Result) error {
	s := d.s
	if r.GetEpoch() != d.epoch {
		return s.refuse(codes.InvalidArgument, "unknown_operation")
	}
	if _, ok := d.sent(r.GetOperationId()); !ok {
		return s.refuse(codes.InvalidArgument, "unknown_operation")
	}
	res, ok := toStoreResult(r)
	if !ok {
		return s.refuse(codes.InvalidArgument, "invalid_result")
	}
	err := s.store.InTenantTx(ctx, s.actor, func(ctx context.Context, tx *store.Tx) error {
		return tx.CompleteOperation(ctx, d.epoch, r.GetOperationId(), res)
	})
	if err := d.storeError(ctx, "result", r.GetOperationId(), err); err != nil {
		return err
	}
	s.logf("agent_op=finished node=%s epoch=%d op=%s state=%s code=%s",
		s.nodeID, d.epoch, r.GetOperationId(), res.State, orOK(res.Code))

	d.mu.Lock()
	delete(d.inflight, r.GetOperationId())
	d.mu.Unlock()
	d.signal()
	return nil
}

// installPhases is the one step an install may take from each phase.
var installPhases = map[string]agentv1.OperationPhase{
	"provisioning": agentv1.OperationPhase_OPERATION_PHASE_FETCHING,
	"fetching":     agentv1.OperationPhase_OPERATION_PHASE_CONFIGURING,
}

var phaseNames = map[agentv1.OperationPhase]string{
	agentv1.OperationPhase_OPERATION_PHASE_FETCHING:    "fetching",
	agentv1.OperationPhase_OPERATION_PHASE_CONFIGURING: "configuring",
}

// progress handles a Progress: STARTING once for a restart this session
// sent, or an install's next phase.
func (d *dispatcher) progress(ctx context.Context, p *agentv1.Progress) error {
	s := d.s
	if p.GetEpoch() != d.epoch {
		return s.refuse(codes.InvalidArgument, "unknown_operation")
	}
	op, ok := d.sent(p.GetOperationId())
	if !ok {
		return s.refuse(codes.InvalidArgument, "unknown_operation")
	}
	if op.action == store.ActionInstall {
		return d.installProgress(ctx, op, p)
	}
	if p.GetPhase() != agentv1.OperationPhase_OPERATION_PHASE_STARTING || op.action != store.ActionRestart || op.starting {
		return s.refuse(codes.InvalidArgument, "invalid_progress")
	}
	err := s.store.InTenantTx(ctx, s.actor, func(ctx context.Context, tx *store.Tx) error {
		return tx.RestartStarting(ctx, d.epoch, p.GetOperationId())
	})
	if err := d.storeError(ctx, "progress", p.GetOperationId(), err); err != nil {
		return err
	}
	d.mu.Lock()
	op.starting = true
	d.mu.Unlock()
	return nil
}

// installProgress moves an install one phase on, never back or past one.
func (d *dispatcher) installProgress(ctx context.Context, op *sentOp, p *agentv1.Progress) error {
	s := d.s
	d.mu.Lock()
	from := op.phase
	d.mu.Unlock()
	if next, ok := installPhases[from]; !ok || p.GetPhase() != next {
		return s.refuse(codes.InvalidArgument, "invalid_progress")
	}
	to := phaseNames[p.GetPhase()]
	err := s.store.InTenantTx(ctx, s.actor, func(ctx context.Context, tx *store.Tx) error {
		return tx.InstallPhase(ctx, d.epoch, p.GetOperationId(), to)
	})
	if err := d.storeError(ctx, "progress", p.GetOperationId(), err); err != nil {
		return err
	}
	d.mu.Lock()
	op.phase = to
	d.mu.Unlock()
	return nil
}

// storeError turns a failed result or progress write into the stream's
// status. The operation stays running either way, and the next hello
// makes it uncertain.
func (d *dispatcher) storeError(ctx context.Context, what, opID string, err error) error {
	s := d.s
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrSessionSuperseded):
		s.logf("agent_session=superseded node=%s epoch=%d", s.nodeID, d.epoch)
		return streamError(codes.Aborted, MsgSessionReplaced)
	case errors.Is(err, store.ErrNotClaimed):
		// The set said this session sent it and it's unfinished, so the
		// database disagreeing is a bug on our side, not the agent's.
		s.logf("agent_link=%s_not_stored node=%s epoch=%d op=%s error=%q", what, s.nodeID, d.epoch, opID, err.Error())
		return streamError(codes.Internal, what+"_not_stored")
	default:
		if ctx.Err() == nil {
			s.logf("agent_link=%s_failed node=%s epoch=%d op=%s error=%q", what, s.nodeID, d.epoch, opID, err.Error())
		}
		return streamError(codes.Unavailable, what+"_failed")
	}
}

func orOK(code string) string {
	if code == "" {
		return "ok"
	}
	return code
}
