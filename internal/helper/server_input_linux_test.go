// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package helper

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	helperv1 "github.com/sharddevs/waystone_panel/gen/waystone/helper/v1"
)

// inputRecorder is a Handle that reads cmd.Input right away. The Server
// closes the descriptor after Handle returns, so this is the only chance.
type inputRecorder struct {
	mu      sync.Mutex
	calls   int
	inputs  []*os.File
	content []string
}

func (r *inputRecorder) handle(_ context.Context, cmd Command) *helperv1.Response {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.inputs = append(r.inputs, cmd.Input)
	if cmd.Input == nil {
		r.content = append(r.content, "")
		return &helperv1.Response{}
	}
	b, err := io.ReadAll(cmd.Input)
	if err != nil {
		r.content = append(r.content, "read error: "+err.Error())
	} else {
		r.content = append(r.content, string(b))
	}
	return &helperv1.Response{}
}

// startInputServer runs a Server on a temp socket that treats every caller
// as uid, and returns a Client for it.
func startInputServer(t *testing.T, uid uint32) (Client, *inputRecorder) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "p.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	rec := &inputRecorder{}
	srv := &Server{AgentUID: testAgentUID, PeerCred: asUID(uid), Handle: rec.handle, Log: log.New(io.Discard, "", 0)}
	go srv.Serve(l)
	t.Cleanup(func() { l.Close() })
	return Client{SocketPath: path}, rec
}

func doInput(t *testing.T, c Client, req *helperv1.Request, input *os.File) *helperv1.Response {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := c.DoInput(ctx, req, input)
	if err != nil {
		t.Fatalf("DoInput: %v", err)
	}
	return resp
}

func TestServerPassesInstallInput(t *testing.T) {
	c, rec := startInputServer(t, testAgentUID)
	jar := contentFile(t, "jar bytes")
	resp := doInput(t, c, installRequest(), jar)
	if resp.GetErrorCode() != helperv1.ErrorCode_ERROR_CODE_UNSPECIFIED {
		t.Fatalf("response %v %q", resp.GetErrorCode(), resp.GetErrorMessage())
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.calls != 1 || rec.content[0] != "jar bytes" {
		t.Fatalf("Handle saw %d calls, content %q; want one call that reads \"jar bytes\"", rec.calls, rec.content)
	}
	// The Server closes what it received when serveConn returns, which can
	// be just after we get the answer, so we poll for a bit.
	closed := false
	for range 200 {
		if _, err := rec.inputs[0].Stat(); errors.Is(err, os.ErrClosed) {
			closed = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !closed {
		t.Error("the Server never closed the descriptor it received")
	}
	// The sender's file is a different descriptor and stays open.
	if _, err := jar.Stat(); err != nil {
		t.Errorf("sender's file: %v", err)
	}
}

func TestServerRefusesMisplacedInput(t *testing.T) {
	c, rec := startInputServer(t, testAgentUID)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	readReq := validRequest(helperv1.Action_ACTION_READ_FILE)
	readReq.File = &helperv1.FileSpec{Name: "f.txt"}

	tests := map[string]struct {
		req   *helperv1.Request
		input *os.File
	}{
		"install without a descriptor": {installRequest(), nil},
		"install with a pipe":          {installRequest(), r},
		"read with a descriptor":       {readReq, contentFile(t, "x")},
		"status with a descriptor":     {validRequest(helperv1.Action_ACTION_STATUS), contentFile(t, "x")},
	}
	for name, tt := range tests {
		resp := doInput(t, c, tt.req, tt.input)
		if resp.GetErrorCode() != helperv1.ErrorCode_ERROR_CODE_INVALID_REQUEST || resp.GetErrorMessage() == "" {
			t.Errorf("%s: %v %q, want INVALID_REQUEST with a message", name, resp.GetErrorCode(), resp.GetErrorMessage())
		}
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.calls != 0 {
		t.Errorf("Handle ran %d times for refused requests", rec.calls)
	}
}

func TestServerRefusesInputFromOtherUsers(t *testing.T) {
	c, rec := startInputServer(t, 1000)
	resp := doInput(t, c, installRequest(), contentFile(t, "x"))
	if resp.GetErrorCode() != helperv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED {
		t.Errorf("other user: %v %q, want PERMISSION_DENIED", resp.GetErrorCode(), resp.GetErrorMessage())
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.calls != 0 {
		t.Errorf("Handle ran for an unauthorized caller")
	}
}

// Without a descriptor DoInput works like Do.
func TestDoInputWithoutDescriptor(t *testing.T) {
	c, rec := startInputServer(t, testAgentUID)
	resp := doInput(t, c, validRequest(helperv1.Action_ACTION_STATUS), nil)
	if resp.GetErrorCode() != helperv1.ErrorCode_ERROR_CODE_UNSPECIFIED {
		t.Errorf("status: %v %q", resp.GetErrorCode(), resp.GetErrorMessage())
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.calls != 1 || rec.inputs[0] != nil {
		t.Errorf("Handle saw %d calls, inputs %v; want one call without input", rec.calls, rec.inputs)
	}
}

func TestDoInputMissingSocket(t *testing.T) {
	c := Client{SocketPath: filepath.Join(t.TempDir(), "missing.sock")}
	if _, err := c.DoInput(context.Background(), installRequest(), contentFile(t, "x")); err == nil {
		t.Error("DoInput to a missing socket succeeded")
	}
}
