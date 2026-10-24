// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package agent

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"google.golang.org/grpc"

	agentv1 "github.com/sharddevs/waystone_panel/gen/waystone/agent/v1"
	helperv1 "github.com/sharddevs/waystone_panel/gen/waystone/helper/v1"
)

// Stage 7b: a console command, against a real FIFO in a temporary
// directory that the test owns instead of root.

func (f *fakeLink) GetCommandSpec(context.Context, *agentv1.GetCommandSpecRequest, ...grpc.CallOption) (*agentv1.GetCommandSpecResponse, error) {
	f.calls = append(f.calls, "spec")
	return f.cmdSpec, f.specErr
}

// consoleDir makes a 0755 directory with a FIFO for srv1, open for
// reading the way systemd holds it. It returns the directory and the
// reading end.
func consoleDir(t *testing.T) (string, int) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "console")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, srv1)
	if err := syscall.Mkfifo(fifo, 0o620); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(fifo, 0o620); err != nil {
		t.Fatal(err)
	}
	r, err := syscall.Open(fifo, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Close(r) })
	return dir, r
}

func readAll(t *testing.T, fd int) string {
	t.Helper()
	b := make([]byte, 1<<16)
	n, err := syscall.Read(fd, b)
	if errors.Is(err, syscall.EAGAIN) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(b[:n])
}

func consoleAgent(t *testing.T, h Helper, dir string) *Agent {
	t.Helper()
	a, err := New(Config{
		ControllerSocket: "/nonexistent", NodeID: node, Helper: h, Runner: &fakeRunner{}, Prober: fakeProber{},
		ConsoleDir: dir, ConsoleOwner: uint32(os.Getuid()), Log: log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// runCommand runs one CONSOLE command and returns its result.
func runCommand(t *testing.T, a *Agent, link *fakeLink) (agentv1.ResultOutcome, agentv1.ResultCode) {
	t.Helper()
	cmds := newCommands(3, []string{srv1}, nil)
	cmds.link = link
	cmd := command(3, op1, srv1, agentv1.CommandAction_COMMAND_ACTION_CONSOLE)
	if known, err := cmds.accept(cmd); err != nil || !known {
		t.Fatalf("accept: %v %v", known, err)
	}
	var res *agentv1.Result
	a.execute(cmd, cmds, func(m *agentv1.ConnectRequest) bool {
		res = m.GetResult()
		return true
	})
	if res == nil {
		t.Fatal("no result")
	}
	return res.GetOutcome(), res.GetCode()
}

// statusHelper answers STATUS with resp (or err), and logs the call to
// link, so the order of the two calls can be checked.
func statusHelper(link *fakeLink, resp *helperv1.Response, err error) *fakeHelper {
	return &fakeHelper{answer: func(context.Context, string) (*helperv1.Response, error) {
		link.calls = append(link.calls, "status")
		return resp, err
	}}
}

func TestConsoleWrites(t *testing.T) {
	dir, r := consoleDir(t)
	link := &fakeLink{cmdSpec: &agentv1.GetCommandSpecResponse{Command: "say hi é"}}
	h := statusHelper(link, running("active/running"), nil)
	o, c := runCommand(t, consoleAgent(t, h, dir), link)
	if o != agentv1.ResultOutcome_RESULT_OUTCOME_SUCCEEDED || c != agentv1.ResultCode_RESULT_CODE_UNSPECIFIED {
		t.Fatalf("result %s %s", o, c)
	}
	if got := readAll(t, r); got != "say hi é\n" {
		t.Errorf("FIFO got %q", got)
	}
	// The status is asked right before the write, after the spec (7M1).
	if strings.Join(link.calls, ",") != "spec,status" {
		t.Errorf("calls %v", link.calls)
	}
	req := h.requests()[0]
	if req.GetAction() != helperv1.Action_ACTION_STATUS || req.GetServerId() != srv1 || req.GetOperationId() != op1 {
		t.Errorf("helper request %v", req)
	}
}

func TestConsoleRefusals(t *testing.T) {
	failed := agentv1.ResultOutcome_RESULT_OUTCOME_FAILED
	spec := func(line string) *agentv1.GetCommandSpecResponse {
		return &agentv1.GetCommandSpecResponse{Command: line}
	}
	stopped := &helperv1.Response{RunState: helperv1.RunState_RUN_STATE_STOPPED}
	notFound := &helperv1.Response{ErrorCode: helperv1.ErrorCode_ERROR_CODE_NOT_FOUND}
	denied := &helperv1.Response{ErrorCode: helperv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED}
	for _, tc := range []struct {
		name   string
		link   *fakeLink
		status *helperv1.Response
		err    error
		setup  func(t *testing.T, dir string)
		code   agentv1.ResultCode
		calls  string
	}{
		{name: "no spec", link: &fakeLink{specErr: errors.New("not_permitted")}, code: agentv1.ResultCode_RESULT_CODE_SPEC_UNAVAILABLE, calls: "spec"},
		{name: "two lines", link: &fakeLink{cmdSpec: spec("say a\nstop")}, code: agentv1.ResultCode_RESULT_CODE_SPEC_UNAVAILABLE, calls: "spec"},
		{name: "slash", link: &fakeLink{cmdSpec: spec("/stop")}, code: agentv1.ResultCode_RESULT_CODE_SPEC_UNAVAILABLE, calls: "spec"},
		{name: "empty", link: &fakeLink{cmdSpec: spec("")}, code: agentv1.ResultCode_RESULT_CODE_SPEC_UNAVAILABLE, calls: "spec"},
		{name: "stopped", status: stopped, code: agentv1.ResultCode_RESULT_CODE_NOT_RUNNING},
		{name: "starting", status: &helperv1.Response{RunState: helperv1.RunState_RUN_STATE_STARTING}, code: agentv1.ResultCode_RESULT_CODE_NOT_RUNNING},
		{name: "no unit", status: notFound, code: agentv1.ResultCode_RESULT_CODE_NOT_RUNNING},
		{name: "helper refused", status: denied, code: agentv1.ResultCode_RESULT_CODE_HELPER_UNAVAILABLE},
		{name: "no helper", err: errors.New("dial"), code: agentv1.ResultCode_RESULT_CODE_HELPER_UNAVAILABLE},
		{name: "no FIFO", setup: func(t *testing.T, dir string) { os.Remove(filepath.Join(dir, srv1)) },
			code: agentv1.ResultCode_RESULT_CODE_CONSOLE_UNAVAILABLE},
		{name: "FIFO is a file", setup: func(t *testing.T, dir string) {
			os.Remove(filepath.Join(dir, srv1))
			os.WriteFile(filepath.Join(dir, srv1), nil, 0o620)
		}, code: agentv1.ResultCode_RESULT_CODE_CONSOLE_UNAVAILABLE},
		{name: "FIFO is a link", setup: func(t *testing.T, dir string) {
			os.Rename(filepath.Join(dir, srv1), filepath.Join(dir, "real"))
			os.Symlink(filepath.Join(dir, "real"), filepath.Join(dir, srv1))
		}, code: agentv1.ResultCode_RESULT_CODE_CONSOLE_UNAVAILABLE},
		{name: "FIFO others may write", setup: func(t *testing.T, dir string) { os.Chmod(filepath.Join(dir, srv1), 0o622) },
			code: agentv1.ResultCode_RESULT_CODE_CONSOLE_UNAVAILABLE},
		{name: "dir group-writable", setup: func(t *testing.T, dir string) { os.Chmod(dir, 0o775) },
			code: agentv1.ResultCode_RESULT_CODE_CONSOLE_UNAVAILABLE},
		{name: "dir 0700", setup: func(t *testing.T, dir string) { os.Chmod(dir, 0o700) },
			code: agentv1.ResultCode_RESULT_CODE_CONSOLE_UNAVAILABLE},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, r := consoleDir(t)
			if tc.setup != nil {
				tc.setup(t, dir)
			}
			link := tc.link
			if link == nil {
				link = &fakeLink{cmdSpec: spec("say hi")}
			}
			status := tc.status
			if status == nil && tc.err == nil {
				status = running("active/running")
			}
			o, c := runCommand(t, consoleAgent(t, statusHelper(link, status, tc.err), dir), link)
			if o != failed || c != tc.code {
				t.Errorf("result %s %s, want FAILED %s", o, c, tc.code)
			}
			if got := readAll(t, r); got != "" {
				t.Errorf("FIFO got %q", got)
			}
			want := tc.calls
			if want == "" {
				want = "spec,status"
			}
			if strings.Join(link.calls, ",") != want {
				t.Errorf("calls %v, want %s", link.calls, want)
			}
		})
	}
}

// The directory must be the expected owner's, and a link to one is not
// one.
func TestConsoleDirOwnerAndLink(t *testing.T) {
	dir, _ := consoleDir(t)
	link := &fakeLink{cmdSpec: &agentv1.GetCommandSpecResponse{Command: "say hi"}}
	a := consoleAgent(t, statusHelper(link, running(""), nil), dir)
	a.consoleOwner++
	if _, c := runCommand(t, a, link); c != agentv1.ResultCode_RESULT_CODE_CONSOLE_UNAVAILABLE {
		t.Errorf("another owner: %s", c)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(dir, alias); err != nil {
		t.Fatal(err)
	}
	a = consoleAgent(t, statusHelper(link, running(""), nil), alias)
	if _, c := runCommand(t, a, link); c != agentv1.ResultCode_RESULT_CODE_CONSOLE_UNAVAILABLE {
		t.Errorf("linked directory: %s", c)
	}
}

// A FIFO or directory someone else owns is not the console. Changing its owner needs root, as the container's tests run.
func TestConsoleFIFOOwner(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("needs root to chown the FIFO")
	}
	dir, r := consoleDir(t)
	if err := os.Lchown(filepath.Join(dir, srv1), 65534, -1); err != nil {
		t.Fatal(err)
	}
	link := &fakeLink{cmdSpec: &agentv1.GetCommandSpecResponse{Command: "say hi"}}
	if _, c := runCommand(t, consoleAgent(t, statusHelper(link, running(""), nil), dir), link); c != agentv1.ResultCode_RESULT_CODE_CONSOLE_UNAVAILABLE {
		t.Errorf("another owner's FIFO: %s", c)
	}
	if got := readAll(t, r); got != "" {
		t.Errorf("FIFO got %q", got)
	}

	// The right FIFO in someone else's directory.
	dir, r = consoleDir(t)
	if err := os.Lchown(dir, 65534, -1); err != nil {
		t.Fatal(err)
	}
	if _, c := runCommand(t, consoleAgent(t, statusHelper(link, running(""), nil), dir), link); c != agentv1.ResultCode_RESULT_CODE_CONSOLE_UNAVAILABLE {
		t.Errorf("another owner's directory: %s", c)
	}
	if got := readAll(t, r); got != "" {
		t.Errorf("FIFO got %q", got)
	}
}

// With nobody reading, the open fails at once (ENXIO); with the pipe
// full, the write does (EAGAIN). Neither waits.
func TestConsoleNoReaderOrFull(t *testing.T) {
	dir, r := consoleDir(t)
	link := &fakeLink{cmdSpec: &agentv1.GetCommandSpecResponse{Command: "say hi"}}
	a := consoleAgent(t, statusHelper(link, running(""), nil), dir)

	w, err := syscall.Open(filepath.Join(dir, srv1), syscall.O_WRONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(w)
	for {
		if _, err := syscall.Write(w, make([]byte, 4096)); err != nil {
			break
		}
	}
	if _, c := runCommand(t, a, link); c != agentv1.ResultCode_RESULT_CODE_CONSOLE_UNAVAILABLE {
		t.Errorf("full pipe: %s", c)
	}

	syscall.Close(r)
	syscall.Close(w)
	dir2, r2 := consoleDir(t)
	syscall.Close(r2)
	a = consoleAgent(t, statusHelper(link, running(""), nil), dir2)
	if _, c := runCommand(t, a, link); c != agentv1.ResultCode_RESULT_CODE_CONSOLE_UNAVAILABLE {
		t.Errorf("no reader: %s", c)
	}
}
