package helper

import (
	"context"
	"errors"
	"log"
	"net"
	"time"

	helperv1 "github.com/sharddevs/waystone_panel/gen/waystone/helper/v1"
)

// ConnTimeout bounds how long one connection may take, so a stuck or
// malicious client cannot hold the helper.
const ConnTimeout = 60 * time.Second

// Server answers one request per connection.
type Server struct {
	// AgentUID is the only user allowed to make requests.
	AgentUID uint32
	// PeerCred asks the kernel who is on the other end of conn.
	PeerCred func(conn net.Conn) (PeerCred, error)
	// Handle performs a validated command.
	Handle func(ctx context.Context, cmd Command) *helperv1.Response
	// Log receives one line per request. Nil means the standard logger.
	Log *log.Logger
}

func (s *Server) logf(format string, args ...any) {
	if s.Log != nil {
		s.Log.Printf(format, args...)
		return
	}

	log.Printf(format, args...)
}

// Serve accepts connections until l is closed, serving each one in its own
// goroutine. It returns nil once l is closed (errors.Is(err, net.ErrClosed))
// and any other accept error otherwise.
func (s *Server) Serve(l net.Listener) error {
	sem := make(chan struct{}, 16) // limit concurrent connections
	for {
		conn, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}

		sem <- struct{}{}

		go func() {
			defer func() { <-sem }()
			s.serveConn(conn)
		}()
	}
}

// serveConn handles one connection. Steps, in this order:
//
//  1. Close the connection when done, and set a deadline of ConnTimeout.
//  2. Ask s.PeerCred who is calling. If that fails, or Authorize refuses,
//     reply PERMISSION_DENIED and stop. Do not read the request first:
//     never parse input from a caller you have not authorized.
//  3. ReadFrame the request. On error reply INVALID_REQUEST.
//  4. ValidateRequest. On error reply INVALID_REQUEST.
//  5. Call s.Handle with a context limited to ConnTimeout, and reply with
//     its response.
//  6. Log one line per request. For handled requests include the operation
//     id, server id, action, caller uid and result: this line is the audit
//     trail in journald. For refusals include the caller's uid and pid.
//
// Replies are written with WriteFrame. A failed reply write is only logged.
func (s *Server) serveConn(conn net.Conn) {
	defer conn.Close()

	if err := conn.SetDeadline(time.Now().Add(ConnTimeout)); err != nil {
		s.logf("failed to set connection deadline: %v", err)
		return
	}
	peer, err := s.PeerCred(conn)
	if err != nil {
		resp := &helperv1.Response{
			ErrorCode:    helperv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED,
			ErrorMessage: "could not verify peer credentials",
		}

		if writeErr := WriteFrame(conn, resp); writeErr != nil {
			s.logf("failed to send permission denial: %v", writeErr)
		}

		s.logf("peer credential lookup failed: %v", err)
		return
	}
	if err := Authorize(peer, s.AgentUID); err != nil {
		resp := &helperv1.Response{
			ErrorCode:    helperv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED,
			ErrorMessage: "unauthorized caller",
		}

		if writeErr := WriteFrame(conn, resp); writeErr != nil {
			s.logf("failed to send permission denial: %v", writeErr)
		}

		s.logf(
			"unauthorized connection: uid=%d pid=%d: %v",
			peer.UID,
			peer.PID,
			err,
		)
		return
	}
	req := new(helperv1.Request)

	if err := ReadFrame(conn, req); err != nil {
		resp := &helperv1.Response{
			ErrorCode:    helperv1.ErrorCode_ERROR_CODE_INVALID_REQUEST,
			ErrorMessage: "invalid request frame",
		}

		if writeErr := WriteFrame(conn, resp); writeErr != nil {
			s.logf("failed to send invalid request response: %v", writeErr)
		}

		s.logf(
			"invalid request frame: uid=%d pid=%d: %v",
			peer.UID,
			peer.PID,
			err,
		)
		return
	}
	cmd, err := ValidateRequest(req)
	if err != nil {
		resp := &helperv1.Response{
			ErrorCode:    helperv1.ErrorCode_ERROR_CODE_INVALID_REQUEST,
			ErrorMessage: "invalid request",
		}

		if writeErr := WriteFrame(conn, resp); writeErr != nil {
			s.logf("failed to send invalid request response: %v", writeErr)
		}

		s.logf(
			"invalid request: uid=%d pid=%d: %v",
			peer.UID,
			peer.PID,
			err,
		)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), ConnTimeout)
	defer cancel()

	s.logf(
		"received op=%s server=%s action=%s uid=%d",
		cmd.OperationID,
		cmd.ServerID,
		cmd.Action,
		peer.UID,
	)
	resp := s.Handle(ctx, cmd)
	if resp == nil {
		resp = &helperv1.Response{
			ErrorCode:    helperv1.ErrorCode_ERROR_CODE_FAILED,
			ErrorMessage: "handler returned nil response",
		}
	}
	if err := WriteFrame(conn, resp); err != nil {
		s.logf("failed to send response: %v", err)
	}

	s.logf(
		"operation=%s server=%s action=%s uid=%d result=%s error=%q",
		cmd.OperationID,
		cmd.ServerID,
		cmd.Action,
		peer.UID,
		resp.GetErrorCode(),
		resp.GetErrorMessage(),
	)
}
