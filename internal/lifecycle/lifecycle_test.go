package lifecycle

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"path/filepath"
	"testing"
	"time"

	helperv1 "github.com/sharddevs/waystone_panel/gen/waystone/helper/v1"
	"github.com/sharddevs/waystone_panel/internal/helper"
)

const (
	opID     = "0192a5c4-7d3e-7b1a-9c2f-3e4d5f6a7b8c"
	serverID = "3f2504e0-4f89-41d3-9a0c-0305e82c3301"
)

// startHelper runs the real helper.Server on a temp socket with a fake
// action handler, so these tests cover the agent side down to the socket.
// PeerCred is faked to always say "the agent" so we don't need real UIDs.
func startHelper(t *testing.T, handle func(context.Context, helper.Command) *helperv1.Response) HelperRuntime {
	t.Helper()
	path := filepath.Join(t.TempDir(), "h.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &helper.Server{
		AgentUID: 998,
		PeerCred: func(net.Conn) (helper.PeerCred, error) { return helper.PeerCred{UID: 998}, nil },
		Handle:   handle,
		Log:      log.New(io.Discard, "", 0),
	}
	go srv.Serve(l)
	t.Cleanup(func() { l.Close() })
	return HelperRuntime{Client: helper.Client{SocketPath: path}}
}

// ctx gives each call a timeout, so a hung helper fails the test instead
// of hanging it.
func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return c
}

func TestHelperRuntimeSendsActions(t *testing.T) {
	var got []helper.Command
	rt := startHelper(t, func(_ context.Context, cmd helper.Command) *helperv1.Response {
		got = append(got, cmd)
		return &helperv1.Response{RunState: helperv1.RunState_RUN_STATE_RUNNING, NativeState: "active/running"}
	})

	if err := rt.Start(ctx(t), serverID, opID); err != nil {
		t.Errorf("Start: %v", err)
	}
	if err := rt.Stop(ctx(t), serverID, opID); err != nil {
		t.Errorf("Stop: %v", err)
	}
	st, err := rt.Status(ctx(t), serverID, opID)
	if err != nil {
		t.Errorf("Status: %v", err)
	}
	if st != (Status{State: helperv1.RunState_RUN_STATE_RUNNING, Native: "active/running"}) {
		t.Errorf("Status = %+v", st)
	}

	want := []helperv1.Action{helperv1.Action_ACTION_START, helperv1.Action_ACTION_STOP, helperv1.Action_ACTION_STATUS}
	if len(got) != len(want) {
		t.Fatalf("helper received %d requests, want %d", len(got), len(want))
	}
	for i, cmd := range got {
		if cmd.Action != want[i] || cmd.ServerID != serverID || cmd.OperationID != opID {
			t.Errorf("request %d = %+v, want action %v for server %s, op %s", i, cmd, want[i], serverID, opID)
		}
	}
}

func TestHelperRuntimeErrors(t *testing.T) {
	tests := map[helperv1.ErrorCode]error{
		helperv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED: ErrPermissionDenied,
		helperv1.ErrorCode_ERROR_CODE_INVALID_REQUEST:   ErrInvalidRequest,
		helperv1.ErrorCode_ERROR_CODE_NOT_FOUND:         ErrNotFound,
		helperv1.ErrorCode_ERROR_CODE_FAILED:            ErrFailed,
		helperv1.ErrorCode(42):                          ErrFailed, // unknown codes are failures, never success
	}
	for code, want := range tests {
		rt := startHelper(t, func(context.Context, helper.Command) *helperv1.Response {
			return &helperv1.Response{ErrorCode: code, ErrorMessage: "detail"}
		})
		if err := rt.Start(ctx(t), serverID, opID); !errors.Is(err, want) {
			t.Errorf("%v: Start = %v, want %v", code, err, want)
		}
		if _, err := rt.Status(ctx(t), serverID, opID); !errors.Is(err, want) {
			t.Errorf("%v: Status = %v, want %v", code, err, want)
		}
	}
}

func TestHelperRuntimeNoHelper(t *testing.T) {
	rt := HelperRuntime{Client: helper.Client{SocketPath: filepath.Join(t.TempDir(), "missing.sock")}}
	if err := rt.Start(ctx(t), serverID, opID); err == nil {
		t.Error("Start succeeded with no helper running")
	}
}

// Stage 4c, S3: START and STOP carry the caller's deadline; STATUS
// carries none (the helper would refuse one).
func TestHelperRuntimeSendsDeadline(t *testing.T) {
	var got []helper.Command
	rt := startHelper(t, func(_ context.Context, cmd helper.Command) *helperv1.Response {
		got = append(got, cmd)
		return &helperv1.Response{RunState: helperv1.RunState_RUN_STATE_RUNNING}
	})
	deadline := time.Now().Add(7 * time.Second).Truncate(time.Millisecond)
	c, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	if err := rt.Start(c, serverID, opID); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := rt.Stop(c, serverID, opID); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := rt.Status(c, serverID, opID); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("helper received %d requests, want 3", len(got))
	}
	for _, cmd := range got[:2] {
		if !cmd.NotAfter.Equal(deadline) {
			t.Errorf("%v: NotAfter %v, want the caller's deadline %v", cmd.Action, cmd.NotAfter, deadline)
		}
	}
	if !got[2].NotAfter.IsZero() {
		t.Errorf("STATUS: NotAfter %v, want none", got[2].NotAfter)
	}
}

// S5: the helper client's "never sent" survives lifecycle's wrapping, so
// the agent can tell a request that can't have run from one that might.
func TestHelperRuntimeNotSent(t *testing.T) {
	rt := HelperRuntime{Client: helper.Client{SocketPath: filepath.Join(t.TempDir(), "missing.sock")}}
	if err := rt.Stop(ctx(t), serverID, opID); !errors.Is(err, helper.ErrNotSent) {
		t.Errorf("no helper: %v, want helper.ErrNotSent", err)
	}
}
