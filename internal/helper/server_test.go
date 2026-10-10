package helper

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	helperv1 "github.com/sharddevs/waystone_panel/gen/waystone/helper/v1"
)

const testAgentUID = 998

type recorder struct {
	mu   sync.Mutex
	cmds []Command
}

func (r *recorder) handle(_ context.Context, cmd Command) *helperv1.Response {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cmds = append(r.cmds, cmd)
	return &helperv1.Response{RunState: helperv1.RunState_RUN_STATE_RUNNING, NativeState: "fake"}
}

func (r *recorder) calls() []Command {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Command(nil), r.cmds...)
}

// startServer runs a Server on a real Unix socket. peer decides who the
// kernel says is calling, since tests cannot change their own uid.
func startServer(t *testing.T, peer func(net.Conn) (PeerCred, error)) (string, *recorder) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "h.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	rec := &recorder{}
	srv := &Server{AgentUID: testAgentUID, PeerCred: peer, Handle: rec.handle, Log: log.New(io.Discard, "", 0)}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(l) }()
	t.Cleanup(func() {
		l.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve returned %v after the listener closed, want nil", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Serve did not return after the listener closed")
		}
	})
	return path, rec
}

func asUID(uid uint32) func(net.Conn) (PeerCred, error) {
	return func(net.Conn) (PeerCred, error) { return PeerCred{UID: uid, PID: 1234}, nil }
}

func send(t *testing.T, path string, req *helperv1.Request) *helperv1.Response {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := Client{SocketPath: path}.Do(ctx, req)
	if err != nil {
		t.Fatalf("Client.Do: %v", err)
	}
	return resp
}

func TestServerHandlesAgentRequest(t *testing.T) {
	path, rec := startServer(t, asUID(testAgentUID))
	resp := send(t, path, validRequest(helperv1.Action_ACTION_STATUS))
	if resp.GetErrorCode() != helperv1.ErrorCode_ERROR_CODE_UNSPECIFIED {
		t.Fatalf("error code %v (%s), want success", resp.GetErrorCode(), resp.GetErrorMessage())
	}
	if resp.GetRunState() != helperv1.RunState_RUN_STATE_RUNNING || resp.GetNativeState() != "fake" {
		t.Errorf("response %v does not carry the handler's result", resp)
	}
	want := []Command{{OperationID: testOpID, ServerID: testServerID, Action: helperv1.Action_ACTION_STATUS}}
	if got := rec.calls(); len(got) != 1 || got[0] != want[0] {
		t.Errorf("handler got %+v, want %+v", got, want)
	}
}

func TestServerRejectsOtherUsers(t *testing.T) {
	for name, uid := range map[string]uint32{"root": 0, "another user": 1000} {
		path, rec := startServer(t, asUID(uid))
		resp := send(t, path, validRequest(helperv1.Action_ACTION_START))
		if resp.GetErrorCode() != helperv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED {
			t.Errorf("%s: error code %v, want PERMISSION_DENIED", name, resp.GetErrorCode())
		}
		if n := len(rec.calls()); n != 0 {
			t.Errorf("%s: handler ran %d times for an unauthorized caller", name, n)
		}
	}
}

func TestServerRejectsUnknownPeer(t *testing.T) {
	path, rec := startServer(t, func(net.Conn) (PeerCred, error) {
		return PeerCred{}, errors.New("no credentials")
	})
	resp := send(t, path, validRequest(helperv1.Action_ACTION_START))
	if resp.GetErrorCode() != helperv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED {
		t.Errorf("error code %v, want PERMISSION_DENIED", resp.GetErrorCode())
	}
	if n := len(rec.calls()); n != 0 {
		t.Errorf("handler ran %d times without known credentials", n)
	}
}

func TestServerRejectsInvalidRequest(t *testing.T) {
	path, rec := startServer(t, asUID(testAgentUID))
	resp := send(t, path, &helperv1.Request{OperationId: testOpID, ServerId: "sshd", Action: helperv1.Action_ACTION_STOP})
	if resp.GetErrorCode() != helperv1.ErrorCode_ERROR_CODE_INVALID_REQUEST {
		t.Errorf("error code %v, want INVALID_REQUEST", resp.GetErrorCode())
	}
	if n := len(rec.calls()); n != 0 {
		t.Errorf("handler ran %d times for an invalid request", n)
	}
}

func TestServerRejectsOversizedFrame(t *testing.T) {
	path, rec := startServer(t, asUID(testAgentUID))
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	// Claim a 4 GiB body and send nothing else.
	if _, err := conn.Write([]byte{0xff, 0xff, 0xff, 0xff}); err != nil {
		t.Fatal(err)
	}
	var resp helperv1.Response
	if err := ReadFrame(conn, &resp); err != nil {
		t.Fatalf("no response to an oversized frame: %v", err)
	}
	if resp.GetErrorCode() != helperv1.ErrorCode_ERROR_CODE_INVALID_REQUEST {
		t.Errorf("error code %v, want INVALID_REQUEST", resp.GetErrorCode())
	}
	if n := len(rec.calls()); n != 0 {
		t.Errorf("handler ran %d times", n)
	}
}

func TestClientMissingSocket(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := Client{SocketPath: filepath.Join(t.TempDir(), "missing.sock")}.Do(ctx, validRequest(helperv1.Action_ACTION_STATUS))
	if err == nil {
		t.Error("Do succeeded without a helper listening")
	}
}
