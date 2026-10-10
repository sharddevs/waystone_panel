package lifecycle

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	helperv1 "github.com/sharddevs/waystone_panel/gen/waystone/helper/v1"
	"github.com/sharddevs/waystone_panel/internal/helper"
)

// This goes through the real helper.Server, so it also checks that the
// helper's ValidateRequest accepts the provision request the agent sends.
func TestHelperRuntimeProvision(t *testing.T) {
	var got []helper.Command
	rt := startHelper(t, func(_ context.Context, cmd helper.Command) *helperv1.Response {
		got = append(got, cmd)
		return &helperv1.Response{Changed: true}
	})

	rt.Provisioner = rt.Client
	var r Runtime = rt
	changed, err := r.Provision(ctx(t), serverID, opID, ProvisionSpec{Runtime: "java25", HeapMiB: 2048})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if !changed {
		t.Error("changed = false, want the helper's true")
	}
	want := helper.Command{
		OperationID: opID,
		ServerID:    serverID,
		Action:      helperv1.Action_ACTION_PROVISION,
		Spec:        helper.Spec{Runtime: "java25", HeapMiB: 2048},
	}
	if len(got) != 1 || got[0] != want {
		t.Errorf("helper received %+v, want %+v", got, want)
	}
}

func TestHelperRuntimeProvisionErrors(t *testing.T) {
	tests := map[helperv1.ErrorCode]error{
		helperv1.ErrorCode_ERROR_CODE_CONFLICT:        ErrConflict,
		helperv1.ErrorCode_ERROR_CODE_INVALID_REQUEST: ErrInvalidRequest,
		helperv1.ErrorCode_ERROR_CODE_FAILED:          ErrFailed,
	}
	for code, want := range tests {
		rt := startHelper(t, func(context.Context, helper.Command) *helperv1.Response {
			return &helperv1.Response{ErrorCode: code, ErrorMessage: "detail", Changed: true}
		})
		rt.Provisioner = rt.Client
		changed, err := rt.Provision(ctx(t), serverID, opID, ProvisionSpec{Runtime: "java25", HeapMiB: 2048})
		if !errors.Is(err, want) {
			t.Errorf("%v: err = %v, want %v", code, err, want)
		}
		if changed {
			t.Errorf("%v: changed = true on an error", code)
		}
	}
	// CONFLICT must also work for the other actions.
	rt := startHelper(t, func(context.Context, helper.Command) *helperv1.Response {
		return &helperv1.Response{ErrorCode: helperv1.ErrorCode_ERROR_CODE_CONFLICT, ErrorMessage: "detail"}
	})
	if err := rt.Start(ctx(t), serverID, opID); !errors.Is(err, ErrConflict) {
		t.Errorf("Start with CONFLICT: err = %v, want ErrConflict", err)
	}
}

// Provision must use the provisioning service's socket, never the
// runtime helper's.
func TestHelperRuntimeProvisionUsesProvisionerSocket(t *testing.T) {
	called := false
	rt := startHelper(t, func(context.Context, helper.Command) *helperv1.Response {
		called = true
		return &helperv1.Response{}
	})
	rt.Provisioner = helper.Client{SocketPath: filepath.Join(t.TempDir(), "missing.sock")}
	if _, err := rt.Provision(ctx(t), serverID, opID, ProvisionSpec{Runtime: "java25", HeapMiB: 2048}); err == nil {
		t.Error("Provision succeeded with no provisioning service")
	}
	if called {
		t.Error("Provision went to the runtime helper's socket")
	}
}

func TestHelperRuntimeProvisionInvalidSpecNeverReachesHandler(t *testing.T) {
	called := false
	rt := startHelper(t, func(context.Context, helper.Command) *helperv1.Response {
		called = true
		return &helperv1.Response{}
	})
	rt.Provisioner = rt.Client
	_, err := rt.Provision(ctx(t), serverID, opID, ProvisionSpec{Runtime: "/bin/sh", HeapMiB: 2048})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("err = %v, want ErrInvalidRequest", err)
	}
	if called {
		t.Error("the helper handled a request with a path as runtime")
	}
}
