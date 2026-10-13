// SPDX-License-Identifier: AGPL-3.0-only

// Package helper is the root helper the agent talks to over a Unix socket:
// the wire format, request checks, who may call, and the systemd,
// provisioning and file actions. Only descriptor passing needs a Linux-only
// file here; peer credentials and socket activation live in
// cmd/waystone-helper.
package helper

import (
	"context"
	"errors"
	"log"
	"net"
	"time"

	helperv1 "github.com/sharddevs/waystone_panel/gen/waystone/helper/v1"
)

// ConnTimeout limits how long one connection may last, so a slow or hostile
// client can't hold on to the helper. It is longer than a server unit's
// TimeoutStopSec (60 s), so a stop that needs the whole minute to save the
// world still gets its answer back (stage 4c, Q2).
const ConnTimeout = 90 * time.Second

// Server reads one request per connection, checks it and hands it to Handle.
type Server struct {
	// AgentUID is the only uid allowed to send requests.
	AgentUID uint32
	// PeerCred looks up the caller's credentials. It's a func field rather
	// than a method so tests can pass in a fake.
	PeerCred func(conn net.Conn) (PeerCred, error)
	// Handle runs a command that passed every check (Router.Handle in
	// production).
	Handle func(ctx context.Context, cmd Command) *helperv1.Response
	// Log is where log lines go. If it's nil the standard logger is used.
	Log *log.Logger
}

func (s *Server) logf(format string, args ...any) {
	if s.Log != nil {
		s.Log.Printf(format, args...)
		return
	}

	log.Printf(format, args...)
}

// Serve accepts connections and serves each one in its own goroutine. It
// returns nil when l is closed and the error for any other Accept failure.
func (s *Server) Serve(l net.Listener) error {
	// A buffered channel used as a semaphore: at most 16 connections are
	// served at once. When it's full, the loop waits after Accept until one
	// of them finishes.
	sem := make(chan struct{}, 16)
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

// serveConn handles one connection from start to finish. A refusal is sent
// back as a response and also logged. If writing a response fails we can
// only log it.
func (s *Server) serveConn(conn net.Conn) {
	defer conn.Close()

	if err := conn.SetDeadline(time.Now().Add(ConnTimeout)); err != nil {
		s.logf("failed to set connection deadline: %v", err)
		return
	}
	// Check who is calling before reading anything, so we never parse
	// bytes from a caller we haven't authorized.
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

	input, err := readRequest(conn, req)
	if err != nil {
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
	// The deferred Close runs whenever serveConn returns, so the received
	// descriptor is closed on every path below, refusals included.
	if input != nil {
		defer input.Close()
	}
	if err != nil {
		resp := &helperv1.Response{
			ErrorCode:    helperv1.ErrorCode_ERROR_CODE_INVALID_REQUEST,
			ErrorMessage: err.Error(),
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
	if err := CheckInput(cmd.Action, input); err != nil {
		resp := &helperv1.Response{
			ErrorCode:    helperv1.ErrorCode_ERROR_CODE_INVALID_REQUEST,
			ErrorMessage: err.Error(),
		}

		if writeErr := WriteFrame(conn, resp); writeErr != nil {
			s.logf("failed to send invalid input response: %v", writeErr)
		}

		s.logf(
			"invalid input: uid=%d pid=%d: %v",
			peer.UID,
			peer.PID,
			err,
		)
		return
	}

	cmd.Input = input
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

	// This line is the audit record that ends up in journald.
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
