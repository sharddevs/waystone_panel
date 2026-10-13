// SPDX-License-Identifier: AGPL-3.0-only

package helper

// Stage 4c, S5 and B1: an error is ErrNotSent only if no write to the
// helper was ever attempted. Every failure after that, including a failed
// or partial write, may have delivered the whole request, so it isn't.

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	helperv1 "github.com/sharddevs/waystone_panel/gen/waystone/helper/v1"
)

// startServerWith runs a real helper Server, with the agent's uid, whose
// commands go to handle.
func startServerWith(t *testing.T, handle func(context.Context, Command) *helperv1.Response) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "h.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &Server{AgentUID: testAgentUID, PeerCred: asUID(testAgentUID), Handle: handle, Log: log.New(io.Discard, "", 0)}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { l.Close() })
	return path
}

// rawListener accepts one connection, reads one whole request frame from
// it, then does whatever after says.
func rawListener(t *testing.T, after func(net.Conn)) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "raw.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		if err := ReadFrame(conn, new(helperv1.Request)); err != nil {
			return
		}
		after(conn)
	}()
	return path
}

func shortCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	t.Cleanup(cancel)
	return ctx
}

func startRequest() *helperv1.Request { return validRequest(helperv1.Action_ACTION_START) }

func TestClientNotSentOnlyBeforeAnyWrite(t *testing.T) {
	// No socket: the dial fails, nothing can have reached a helper.
	_, err := Client{SocketPath: filepath.Join(t.TempDir(), "missing.sock")}.Do(shortCtx(t), startRequest())
	if !errors.Is(err, ErrNotSent) {
		t.Errorf("no socket: %v, want ErrNotSent", err)
	}

	// The connection fails before the write call (SetDeadline): nothing
	// was written, so nothing can have run.
	path := rawListener(t, func(net.Conn) { time.Sleep(time.Second) })
	wrote := false
	_, err = Client{
		SocketPath: path,
		afterDial:  func(c net.Conn) { c.Close() },
		writeFrame: func(*net.UnixConn, proto.Message, *os.File) error { wrote = true; return nil },
	}.Do(shortCtx(t), startRequest())
	if !errors.Is(err, ErrNotSent) || wrote {
		t.Errorf("failure before the write: %v (wrote %t), want ErrNotSent and no write", err, wrote)
	}

	// Every case below attempted a write. None may be ErrNotSent (B1).
	cases := map[string]struct {
		write func(*net.UnixConn, proto.Message, *os.File) error
		after func(net.Conn)
	}{
		"write fails before any byte": {
			write: func(*net.UnixConn, proto.Message, *os.File) error { return errors.New("broken pipe") },
		},
		"write fails midway": {
			write: func(c *net.UnixConn, m proto.Message, _ *os.File) error {
				frame, _ := marshalFrame(m)
				_, _ = c.Write(frame[:len(frame)/2])
				return errors.New("connection reset")
			},
		},
		"short write": {
			write: func(c *net.UnixConn, m proto.Message, _ *os.File) error {
				frame, _ := marshalFrame(m)
				_, _ = c.Write(frame[:len(frame)-1])
				return io.ErrShortWrite
			},
		},
		"whole frame, then the peer closes": {
			after: func(net.Conn) {},
		},
		"whole frame, then no reply": {
			after: func(net.Conn) { time.Sleep(time.Second) },
		},
		"whole frame, then a truncated reply": {
			after: func(c net.Conn) {
				frame, _ := marshalFrame(&helperv1.Response{ErrorMessage: "this reply is cut short"})
				_, _ = c.Write(frame[:len(frame)-3])
			},
		},
		"whole frame, then garbage": {
			after: func(c net.Conn) { _, _ = c.Write([]byte{0, 0, 0, 3, 0xff, 0xff, 0xff}) },
		},
	}
	for name, tt := range cases {
		after := tt.after
		if after == nil {
			after = func(net.Conn) { time.Sleep(time.Second) }
		}
		path := rawListener(t, after)
		_, err := Client{SocketPath: path, writeFrame: tt.write}.Do(shortCtx(t), startRequest())
		if err == nil {
			t.Errorf("%s: no error", name)
			continue
		}
		if errors.Is(err, ErrNotSent) {
			t.Errorf("%s: %v is ErrNotSent, but a write was attempted", name, err)
		}
	}
}

// A partial frame never runs anything on a real helper.
func TestClientPartialFrameRunsNothing(t *testing.T) {
	rec := &recorder{}
	path := startServerWith(t, rec.handle)
	half := func(c *net.UnixConn, m proto.Message, _ *os.File) error {
		frame, _ := marshalFrame(m)
		_, _ = c.Write(frame[:len(frame)/2])
		return errors.New("connection reset")
	}
	_, err := Client{SocketPath: path, writeFrame: half}.Do(shortCtx(t), startRequest())
	if err == nil || errors.Is(err, ErrNotSent) {
		t.Fatalf("partial write: %v, want an error that isn't ErrNotSent", err)
	}
	time.Sleep(100 * time.Millisecond) // the helper sees the close and gives up
	if n := len(rec.calls()); n != 0 {
		t.Errorf("the helper ran %d commands from half a frame", n)
	}
}

// A refusal the helper sent before reading still counts as its answer,
// even though our write failed.
func TestClientReadsRefusalAfterFailedWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	srv := &Server{AgentUID: testAgentUID, PeerCred: asUID(1), Handle: (&recorder{}).handle, Log: log.New(io.Discard, "", 0)}
	go func() { _ = srv.Serve(l) }()
	failing := func(*net.UnixConn, proto.Message, *os.File) error { return errors.New("broken pipe") }
	resp, err := Client{SocketPath: path, writeFrame: failing}.Do(shortCtx(t), startRequest())
	if err != nil {
		t.Fatalf("refusal after a failed write: %v, want the refusal", err)
	}
	if resp.GetErrorCode() != helperv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED {
		t.Errorf("got %v, want PERMISSION_DENIED", resp.GetErrorCode())
	}
}
