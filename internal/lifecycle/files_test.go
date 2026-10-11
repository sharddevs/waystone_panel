// SPDX-License-Identifier: AGPL-3.0-only

package lifecycle

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	helperv1 "github.com/sharddevs/waystone_panel/gen/waystone/helper/v1"
	"github.com/sharddevs/waystone_panel/internal/helper"
	"github.com/sharddevs/waystone_panel/internal/worker"
)

func TestFileExpect(t *testing.T) {
	if got := (File{}).Expect(); got != worker.ExpectAbsent() {
		t.Errorf("missing file: %+v, want absent", got)
	}
	if got := (File{Exists: true}).Expect(); got != worker.ExpectContent(nil) {
		t.Errorf("empty file: %+v, want the hash of nothing", got)
	}
	if got := (File{Exists: true, Content: []byte("old")}).Expect(); got != worker.ExpectContent([]byte("old")) {
		t.Errorf("file: %+v, want the hash of old", got)
	}
}

// These go through the real helper.Server, so they also check that the
// helper's ValidateRequest accepts what the agent sends.
func TestHelperRuntimeReadFile(t *testing.T) {
	var got []helper.Command
	rt := startHelper(t, func(_ context.Context, cmd helper.Command) *helperv1.Response {
		got = append(got, cmd)
		return &helperv1.Response{File: &helperv1.FileResult{Exists: true, Content: []byte("motd=hi\n")}}
	})
	rt.Provisioner = rt.Client
	var r Runtime = rt

	f, err := r.ReadFile(ctx(t), serverID, opID, "server.properties")
	if err != nil || !f.Exists || string(f.Content) != "motd=hi\n" {
		t.Errorf("ReadFile = %+v, %v", f, err)
	}
	want := helper.Command{OperationID: opID, ServerID: serverID, Action: helperv1.Action_ACTION_READ_FILE, File: helper.FileCommand{Name: "server.properties"}}
	if len(got) != 1 || got[0] != want {
		t.Errorf("helper received %+v, want %+v", got, want)
	}
}

func TestHelperRuntimeReadFileNeedsAnAnswer(t *testing.T) {
	rt := startHelper(t, func(context.Context, helper.Command) *helperv1.Response {
		return &helperv1.Response{}
	})
	rt.Provisioner = rt.Client
	if f, err := rt.ReadFile(ctx(t), serverID, opID, "f.txt"); err == nil {
		t.Errorf("a response without a FileResult read as %+v", f)
	}
}

func TestHelperRuntimeWriteFile(t *testing.T) {
	var got []helper.Command
	rt := startHelper(t, func(_ context.Context, cmd helper.Command) *helperv1.Response {
		got = append(got, cmd)
		return &helperv1.Response{}
	})
	rt.Provisioner = rt.Client
	var r Runtime = rt

	if err := r.WriteFile(ctx(t), serverID, opID, "eula.txt", []byte("eula=true\n"), File{}); err != nil {
		t.Errorf("WriteFile new: %v", err)
	}
	base := File{Exists: true, Content: []byte("old")}
	if err := r.WriteFile(ctx(t), serverID, opID, "f.txt", []byte("new"), base); err != nil {
		t.Errorf("WriteFile over: %v", err)
	}
	want := []helper.Command{
		{OperationID: opID, ServerID: serverID, Action: helperv1.Action_ACTION_WRITE_FILE,
			File: helper.FileCommand{Name: "eula.txt", Content: "eula=true\n", Expect: worker.ExpectAbsent()}},
		{OperationID: opID, ServerID: serverID, Action: helperv1.Action_ACTION_WRITE_FILE,
			File: helper.FileCommand{Name: "f.txt", Content: "new", Expect: worker.ExpectContent([]byte("old"))}},
	}
	if len(got) != len(want) {
		t.Fatalf("helper received %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("request %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestHelperRuntimeFileErrors(t *testing.T) {
	tests := map[helperv1.ErrorCode]error{
		helperv1.ErrorCode_ERROR_CODE_NOT_FOUND:       ErrNotFound,
		helperv1.ErrorCode_ERROR_CODE_CONFLICT:        ErrConflict,
		helperv1.ErrorCode_ERROR_CODE_CHANGED:         ErrChanged,
		helperv1.ErrorCode_ERROR_CODE_INVALID_REQUEST: ErrInvalidRequest,
		helperv1.ErrorCode_ERROR_CODE_FAILED:          ErrFailed,
		helperv1.ErrorCode(42):                        ErrFailed,
	}
	for code, want := range tests {
		rt := startHelper(t, func(context.Context, helper.Command) *helperv1.Response {
			return &helperv1.Response{ErrorCode: code, ErrorMessage: "detail", File: &helperv1.FileResult{Exists: true}}
		})
		rt.Provisioner = rt.Client
		if f, err := rt.ReadFile(ctx(t), serverID, opID, "f.txt"); !errors.Is(err, want) || f.Exists {
			t.Errorf("%v: ReadFile = %+v, %v; want nothing and %v", code, f, err, want)
		}
		if err := rt.WriteFile(ctx(t), serverID, opID, "f.txt", []byte("x"), File{}); !errors.Is(err, want) {
			t.Errorf("%v: WriteFile err = %v, want %v", code, err, want)
		}
	}
	// CHANGED must map for the other actions too.
	rt := startHelper(t, func(context.Context, helper.Command) *helperv1.Response {
		return &helperv1.Response{ErrorCode: helperv1.ErrorCode_ERROR_CODE_CHANGED, ErrorMessage: "detail"}
	})
	if err := rt.Start(ctx(t), serverID, opID); !errors.Is(err, ErrChanged) {
		t.Errorf("Start with CHANGED: err = %v, want ErrChanged", err)
	}
}

// The worker is started by the provisioning service. The runtime helper's
// socket must never see these actions.
func TestHelperRuntimeFilesUseProvisionerSocket(t *testing.T) {
	called := false
	rt := startHelper(t, func(context.Context, helper.Command) *helperv1.Response {
		called = true
		return &helperv1.Response{File: &helperv1.FileResult{}}
	})
	rt.Provisioner = helper.Client{SocketPath: filepath.Join(t.TempDir(), "missing.sock")}
	if _, err := rt.ReadFile(ctx(t), serverID, opID, "f.txt"); err == nil {
		t.Error("ReadFile succeeded with no provisioning service")
	}
	if err := rt.WriteFile(ctx(t), serverID, opID, "f.txt", []byte("x"), File{}); err == nil {
		t.Error("WriteFile succeeded with no provisioning service")
	}
	if called {
		t.Error("a file action went to the runtime helper's socket")
	}
}

func TestHelperRuntimeBadNamesNeverReachHandler(t *testing.T) {
	called := false
	rt := startHelper(t, func(context.Context, helper.Command) *helperv1.Response {
		called = true
		return &helperv1.Response{File: &helperv1.FileResult{}}
	})
	rt.Provisioner = rt.Client
	if _, err := rt.ReadFile(ctx(t), serverID, opID, "../../etc/shadow"); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("ReadFile err = %v, want ErrInvalidRequest", err)
	}
	if err := rt.WriteFile(ctx(t), serverID, opID, "a/b", []byte("x"), File{}); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("WriteFile err = %v, want ErrInvalidRequest", err)
	}
	if called {
		t.Error("the helper handled a path as a file name")
	}
}
