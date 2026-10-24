// SPDX-License-Identifier: AGPL-3.0-only

package agentlink

import (
	"context"
	"errors"
	"time"

	"google.golang.org/grpc/codes"

	agentv1 "github.com/sharddevs/waystone_panel/gen/waystone/agent/v1"
	"github.com/sharddevs/waystone_panel/internal/ids"
	"github.com/sharddevs/waystone_panel/internal/serverlog"
)

// Stage 7a (notes/stage7-scope.md D1): reading a server's log through
// its node's live session. The API asks; the dispatcher, the only
// goroutine that sends on the stream (S2), sends the LogRequest; the
// agent's LogChunk comes back through message. Nothing is stored, and
// nothing here logs a line's text.

// LogTimeout is how long a read waits for its chunk.
const LogTimeout = 5 * time.Second

// LogGap is the least time between two reads of one server's log (audit
// H1): viewers polling the same server share reads, and no number of
// them makes the node run journalctl more than once a second for it.
const LogGap = time.Second

// logRead is one read sent to the agent, from the moment it is asked
// until its chunk arrives. done has room for the answer, so the
// dispatcher never waits for a flight that has given up.
type logRead struct {
	req    *agentv1.LogRequest
	flight *logFlight
	done   chan logAnswer
}

type logAnswer struct {
	page serverlog.Page
	err  error
}

// logFlight is a server's one read in progress (H1). Every viewer asking
// for the same lines while it flies shares its answer. done closes when
// the answer is in page and err; free closes when the agent no longer
// counts the read, which can be later, after a timeout.
type logFlight struct {
	after string
	limit int
	done  chan struct{}
	free  chan struct{}
	page  serverlog.Page
	err   error
}

// serverReads is one server's reads on this session, under mu.
type serverReads struct {
	flight *logFlight // nil when none is out
	last   time.Time  // when the last flight started
}

// ReadLog reads serverID's log through nodeID's live session: the newest
// limit lines when after is "", or the lines after that cursor. The
// caller has checked that the server is on that node and that the actor
// may read it. Errors are serverlog's: ErrUnreachable with no session,
// ErrBusy with MaxReads already out, ErrUnavailable when the agent
// couldn't read it or didn't answer within LogTimeout.
func (s *Server) ReadLog(ctx context.Context, nodeID, serverID, after string, limit int) (serverlog.Page, error) {
	read := s.sessions.logReader(nodeID)
	if read == nil {
		return serverlog.Page{}, serverlog.ErrUnreachable
	}
	return read(ctx, serverID, after, limit)
}

// readLog is ReadLog on this session. A server has at most one read out
// at a time, started at least LogGap after the one before; a viewer
// asking for what is already in flight shares it, and any other waits
// its turn, all within LogTimeout.
func (d *dispatcher) readLog(ctx context.Context, serverID, after string, limit int) (serverlog.Page, error) {
	if ids.Check(serverID) != nil || limit < 1 || limit > serverlog.MaxLines || (after != "" && !serverlog.ValidCursor(after)) {
		return serverlog.Page{}, errors.New("agentlink: bad log read")
	}
	timer := time.NewTimer(d.s.logTimeout)
	defer timer.Stop()
	for {
		f, wait, err := d.takeTurn(serverID, after, limit)
		if err != nil {
			return serverlog.Page{}, err
		}
		if f != nil {
			select {
			case <-f.done:
				return f.page, f.err
			case <-d.ended:
				return serverlog.Page{}, serverlog.ErrUnreachable
			case <-timer.C:
				return serverlog.Page{}, serverlog.ErrUnavailable
			case <-ctx.Done():
				return serverlog.Page{}, ctx.Err()
			}
		}
		select {
		case <-wait:
		case <-d.ended:
			return serverlog.Page{}, serverlog.ErrUnreachable
		case <-timer.C:
			return serverlog.Page{}, serverlog.ErrUnavailable
		case <-ctx.Done():
			return serverlog.Page{}, ctx.Err()
		}
	}
}

// takeTurn returns the flight to wait for: the one in progress when it
// is for the same lines, or a new one. When it is another's turn, it
// returns what to wait on before trying again.
func (d *dispatcher) takeTurn(serverID, after string, limit int) (*logFlight, <-chan struct{}, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	sr := d.servers[serverID]
	if sr == nil {
		sr = &serverReads{}
		d.servers[serverID] = sr
	}
	if f := sr.flight; f != nil {
		select {
		case <-f.done:
			// Answered but not free: a timed-out read the agent still
			// counts. Its answer is old; wait for it to clear.
			return nil, f.free, nil
		default:
		}
		if f.after == after && f.limit == limit {
			return f, nil, nil
		}
		return nil, f.free, nil
	}
	if gap := sr.last.Add(d.s.logGap).Sub(d.s.now()); gap > 0 {
		return nil, closeAfter(gap), nil
	}
	if len(d.reads) >= serverlog.MaxReads {
		return nil, nil, serverlog.ErrBusy
	}
	id, err := ids.NewV7()
	if err != nil {
		return nil, nil, err
	}
	f := &logFlight{after: after, limit: limit, done: make(chan struct{}), free: make(chan struct{})}
	r := &logRead{
		req:    &agentv1.LogRequest{Epoch: d.epoch, RequestId: id, ServerId: serverID, AfterCursor: after, Limit: uint32(limit)},
		flight: f,
		done:   make(chan logAnswer, 1),
	}
	d.reads[id] = r
	sr.flight = f
	sr.last = d.s.now()
	go d.fly(r)
	return f, nil, nil
}

// fly sends one read and waits for its chunk, then answers everyone
// sharing it. It doesn't stop for any one viewer going away.
func (d *dispatcher) fly(r *logRead) {
	f := r.flight
	defer close(f.done)
	timer := time.NewTimer(d.s.logTimeout)
	defer timer.Stop()
	select {
	case d.logOut <- r:
	case <-d.ended:
		f.err = serverlog.ErrUnreachable
		return
	case <-timer.C:
		d.release(r)
		f.err = serverlog.ErrUnavailable
		return
	}
	// Sent: from here on only its chunk frees its place, so the agent's
	// count and ours never differ.
	select {
	case a := <-r.done:
		f.page, f.err = a.page, a.err
		if a.err == nil && len(a.page.Lines) == 0 {
			f.page.Cursor = f.after
		}
	case <-d.ended:
		f.err = serverlog.ErrUnreachable
	case <-timer.C:
		f.err = serverlog.ErrUnavailable
	}
}

// release drops a read, once: its chunk came, or it was never sent. The
// server's next read can start.
func (d *dispatcher) release(r *logRead) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.reads[r.req.GetRequestId()] != r {
		return false
	}
	delete(d.reads, r.req.GetRequestId())
	if sr := d.servers[r.req.GetServerId()]; sr != nil && sr.flight == r.flight {
		sr.flight = nil
	}
	close(r.flight.free)
	return true
}

// closeAfter is a channel closed once d has passed.
func closeAfter(d time.Duration) <-chan struct{} {
	c := make(chan struct{})
	time.AfterFunc(d, func() { close(c) })
	return c
}

// sendLog sends one read. Only run calls it.
func (d *dispatcher) sendLog(r *logRead) error {
	err := d.stream.Send(&agentv1.ConnectResponse{Msg: &agentv1.ConnectResponse_LogRequest{LogRequest: r.req}})
	if err != nil {
		d.s.logf("agent_dispatch=send_failed node=%s epoch=%d log_request=%s", d.s.nodeID, d.epoch, r.req.GetRequestId())
		return errSendFailed
	}
	return nil
}

// logChunk handles a LogChunk. One for a read this session didn't send,
// or one that breaks the rules, ends the stream like any other bad
// message (R12).
func (d *dispatcher) logChunk(c *agentv1.LogChunk) error {
	d.mu.Lock()
	r := d.reads[c.GetRequestId()]
	d.mu.Unlock()
	if r == nil || !d.release(r) {
		return d.s.refuse(codes.InvalidArgument, "unknown_log_request")
	}
	page, err := pageOf(c, int(r.req.GetLimit()))
	if errors.Is(err, errBadChunk) {
		return d.s.refuse(codes.InvalidArgument, "invalid_log_chunk")
	}
	if err != nil && d.s.logLimit.allow("log-unavailable/"+r.req.GetServerId()) {
		d.s.logf("agent_log=unavailable node=%s server=%s (at most one line a minute)", d.s.nodeID, r.req.GetServerId())
	}
	r.done <- logAnswer{page, err}
	return nil
}

var errBadChunk = errors.New("bad log chunk")

// pageOf checks a chunk against the rules the helper and the agent
// already follow (serverlog), and turns it into a page.
func pageOf(c *agentv1.LogChunk, limit int) (serverlog.Page, error) {
	switch c.GetCode() {
	case agentv1.LogCode_LOG_CODE_UNSPECIFIED:
	case agentv1.LogCode_LOG_CODE_NO_LOG, agentv1.LogCode_LOG_CODE_UNAVAILABLE:
		if len(c.GetLines()) != 0 || c.GetCursor() != "" || c.GetMore() {
			return serverlog.Page{}, errBadChunk
		}
		if c.GetCode() == agentv1.LogCode_LOG_CODE_UNAVAILABLE {
			return serverlog.Page{}, serverlog.ErrUnavailable
		}
		return serverlog.Page{}, nil
	default:
		return serverlog.Page{}, errBadChunk
	}
	if len(c.GetLines()) > limit {
		return serverlog.Page{}, errBadChunk
	}
	if len(c.GetLines()) == 0 {
		if c.GetCursor() != "" || c.GetMore() {
			return serverlog.Page{}, errBadChunk
		}
		return serverlog.Page{}, nil
	}
	if !serverlog.ValidCursor(c.GetCursor()) {
		return serverlog.Page{}, errBadChunk
	}
	page := serverlog.Page{Lines: make([]serverlog.Line, 0, len(c.GetLines())), Cursor: c.GetCursor(), More: c.GetMore()}
	size := 0
	for _, l := range c.GetLines() {
		size += len(l.GetText())
		if !serverlog.CleanText(l.GetText()) || l.GetPriority() > 7 || l.GetTimeUnixMicro() < 0 || size > serverlog.MaxTextBytes {
			return serverlog.Page{}, errBadChunk
		}
		page.Lines = append(page.Lines, serverlog.Line{
			Time:     time.UnixMicro(l.GetTimeUnixMicro()).UTC(),
			Priority: l.GetPriority(),
			Text:     l.GetText(),
		})
	}
	return page, nil
}
