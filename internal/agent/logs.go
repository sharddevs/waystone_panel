// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"slices"
	"time"

	"google.golang.org/protobuf/proto"

	agentv1 "github.com/sharddevs/waystone_panel/gen/waystone/agent/v1"
	helperv1 "github.com/sharddevs/waystone_panel/gen/waystone/helper/v1"
	"github.com/sharddevs/waystone_panel/internal/ids"
	"github.com/sharddevs/waystone_panel/internal/serverlog"
)

// Stage 7a (notes/stage7-scope.md D1): the controller asks for a
// server's log, the agent asks the helper, and passes the answer back
// unchanged. A read changes nothing, so it isn't a command: it has no
// operation and no result, just one LogChunk.

// logCallTimeout is one read's helper call. The controller waits 5 s.
const logCallTimeout = 4 * time.Second

// maxChunkBytes keeps a chunk well inside one 64 KiB message. The helper
// already keeps its lines inside 48 KiB; anything bigger is refused here
// rather than ending the session when the send fails.
const maxChunkBytes = 60 << 10

// startRead counts a read for this session, refusing a fifth. known says
// whether the server is in the inventory.
func (c *commands) startRead(serverID string) (known, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.reads >= serverlog.MaxReads {
		return false, false
	}
	known = slices.Contains(c.inventory, serverID)
	if known {
		c.reads++
	}
	return known, true
}

func (c *commands) doneRead() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reads--
}

// logRequest checks one LogRequest. Anything no correct controller would
// send is a protocol error. A server outside the inventory has never
// been installed here, so it has no log: NO_LOG at once. Otherwise the
// read runs on its own goroutine.
func (a *Agent) logRequest(req *agentv1.LogRequest, cmds *commands, deliver func(*agentv1.ConnectRequest) bool) error {
	if req.GetEpoch() != cmds.epoch {
		return protocolError("log request for another epoch")
	}
	if ids.Check(req.GetRequestId()) != nil || ids.Check(req.GetServerId()) != nil {
		return protocolError("log request with a bad ID")
	}
	if req.GetLimit() < 1 || req.GetLimit() > serverlog.MaxLines {
		return protocolError("log request with a bad limit")
	}
	if c := req.GetAfterCursor(); c != "" && !serverlog.ValidCursor(c) {
		return protocolError("log request with a bad cursor")
	}
	known, ok := cmds.startRead(req.GetServerId())
	if !ok {
		return protocolError("more than 4 log reads in flight")
	}
	if !known {
		deliver(logChunk(&agentv1.LogChunk{RequestId: req.GetRequestId(), Code: agentv1.LogCode_LOG_CODE_NO_LOG}))
		return nil
	}
	go func() {
		chunk := a.readLog(req)
		// Freed before the send, like a command's slot: the controller
		// may send the next read the moment it has this one.
		cmds.doneRead()
		deliver(logChunk(chunk))
	}()
	return nil
}

// readLog asks the helper and turns its answer into a chunk.
func (a *Agent) readLog(req *agentv1.LogRequest) *agentv1.LogChunk {
	chunk := &agentv1.LogChunk{RequestId: req.GetRequestId()}
	unavailable := func(why string) *agentv1.LogChunk {
		a.logf("agent=log_unavailable server=%s request=%s reason=%s", req.GetServerId(), req.GetRequestId(), why)
		return &agentv1.LogChunk{RequestId: req.GetRequestId(), Code: agentv1.LogCode_LOG_CODE_UNAVAILABLE}
	}
	ctx, cancel := context.WithTimeout(context.Background(), logCallTimeout)
	defer cancel()
	resp, err := a.helper.Do(ctx, &helperv1.Request{
		OperationId: req.GetRequestId(),
		ServerId:    req.GetServerId(),
		Action:      helperv1.Action_ACTION_READ_LOG,
		Log:         &helperv1.LogSpec{AfterCursor: req.GetAfterCursor(), Limit: req.GetLimit()},
	})
	if err != nil {
		return unavailable("helper_unreachable")
	}
	switch resp.GetErrorCode() {
	case helperv1.ErrorCode_ERROR_CODE_UNSPECIFIED:
	case helperv1.ErrorCode_ERROR_CODE_NOT_FOUND:
		chunk.Code = agentv1.LogCode_LOG_CODE_NO_LOG
		return chunk
	default:
		return unavailable(errorCodeName(resp.GetErrorCode()))
	}
	for _, l := range resp.GetLog().GetLines() {
		chunk.Lines = append(chunk.Lines, &agentv1.LogLine{
			TimeUnixMicro: l.GetTimeUnixMicro(),
			Priority:      l.GetPriority(),
			Text:          l.GetText(),
		})
	}
	chunk.Cursor = resp.GetLog().GetCursor()
	chunk.More = resp.GetLog().GetMore()
	if proto.Size(chunk) > maxChunkBytes {
		return unavailable("too_large")
	}
	return chunk
}

func logChunk(c *agentv1.LogChunk) *agentv1.ConnectRequest {
	return &agentv1.ConnectRequest{Msg: &agentv1.ConnectRequest_LogChunk{LogChunk: c}}
}
