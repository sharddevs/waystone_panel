// SPDX-License-Identifier: AGPL-3.0-only

package helper

import (
	"context"
	"errors"
	"testing"

	helperv1 "github.com/sharddevs/waystone_panel/gen/waystone/helper/v1"
	"github.com/sharddevs/waystone_panel/internal/worker"
)

func TestRouterReadsFiles(t *testing.T) {
	runner := &fakeInputRunner{output: `{"exists":true,"content":"aGk="}`}
	r := Router{Worker: &Worker{Accounts: accountsWith(goodAccount()), Runner: runner}}
	resp := r.Handle(context.Background(), readCommand("f.txt"))
	if resp.GetErrorCode() != helperv1.ErrorCode_ERROR_CODE_UNSPECIFIED {
		t.Fatalf("READ_FILE: %v %s", resp.GetErrorCode(), resp.GetErrorMessage())
	}
	if !resp.GetFile().GetExists() || string(resp.GetFile().GetContent()) != "hi" {
		t.Errorf("file = %v, want exists with content hi", resp.GetFile())
	}

	runner.output = `{"exists":false,"content":null}`
	resp = r.Handle(context.Background(), readCommand("f.txt"))
	if resp.GetErrorCode() != helperv1.ErrorCode_ERROR_CODE_UNSPECIFIED || resp.GetFile() == nil || resp.GetFile().GetExists() {
		t.Errorf("missing file: %v %s, file %v; want success with a FileResult that says it doesn't exist",
			resp.GetErrorCode(), resp.GetErrorMessage(), resp.GetFile())
	}
}

func TestRouterWritesFiles(t *testing.T) {
	runner := &fakeInputRunner{output: `{"outcome":"written"}`}
	r := Router{Worker: &Worker{Accounts: accountsWith(goodAccount()), Runner: runner}}
	resp := r.Handle(context.Background(), writeCommand("f.txt", "x", worker.ExpectAbsent()))
	if resp.GetErrorCode() != helperv1.ErrorCode_ERROR_CODE_UNSPECIFIED || len(runner.calls) != 1 {
		t.Errorf("WRITE_FILE: %v %s, %d runs", resp.GetErrorCode(), resp.GetErrorMessage(), len(runner.calls))
	}
	if resp.GetFile() != nil {
		t.Errorf("WRITE_FILE answered with file content %v", resp.GetFile())
	}
}

func TestRouterWorkerErrors(t *testing.T) {
	tests := map[string]struct {
		w    Worker
		want helperv1.ErrorCode
	}{
		"not provisioned": {
			Worker{Accounts: accountsWith(), Runner: &fakeInputRunner{}},
			helperv1.ErrorCode_ERROR_CODE_NOT_FOUND,
		},
		"foreign account": {
			Worker{Accounts: accountsWith(Account{Name: testUser, UID: 994, Comment: "x"}), Runner: &fakeInputRunner{}},
			helperv1.ErrorCode_ERROR_CODE_CONFLICT,
		},
		"worker failed": {
			Worker{Accounts: accountsWith(goodAccount()), Runner: &fakeInputRunner{err: errors.New("exit status 1: waystone-worker: unsupported file")}},
			helperv1.ErrorCode_ERROR_CODE_FAILED,
		},
	}
	for name, tt := range tests {
		for _, cmd := range []Command{readCommand("f.txt"), writeCommand("f.txt", "x", worker.ExpectAbsent())} {
			resp := Router{Worker: &tt.w}.Handle(context.Background(), cmd)
			if resp.GetErrorCode() != tt.want {
				t.Errorf("%s, %v: code %v, want %v", name, cmd.Action, resp.GetErrorCode(), tt.want)
			}
			if resp.GetErrorMessage() == "" {
				t.Errorf("%s, %v: empty error message", name, cmd.Action)
			}
			if resp.GetFile() != nil {
				t.Errorf("%s, %v: file content on an error", name, cmd.Action)
			}
		}
	}
}

func TestRouterWriteFileChanged(t *testing.T) {
	r := Router{Worker: &Worker{Accounts: accountsWith(goodAccount()), Runner: &fakeInputRunner{output: `{"outcome":"changed"}`}}}
	resp := r.Handle(context.Background(), writeCommand("f.txt", "x", worker.ExpectAbsent()))
	if resp.GetErrorCode() != helperv1.ErrorCode_ERROR_CODE_CHANGED || resp.GetErrorMessage() == "" {
		t.Errorf("%v %q, want CHANGED with a message", resp.GetErrorCode(), resp.GetErrorMessage())
	}
}

// The runtime helper has no Worker: it never starts one, and says so.
func TestRouterWithoutWorkerRefuses(t *testing.T) {
	runner := &fakeRunner{}
	r := Router{Systemd: &Systemd{Runner: runner}}
	for _, cmd := range []Command{readCommand("f.txt"), writeCommand("f.txt", "x", worker.ExpectAbsent())} {
		resp := r.Handle(context.Background(), cmd)
		if resp.GetErrorCode() != helperv1.ErrorCode_ERROR_CODE_INVALID_REQUEST || resp.GetErrorMessage() == "" {
			t.Errorf("%v: %v %q, want INVALID_REQUEST with a message", cmd.Action, resp.GetErrorCode(), resp.GetErrorMessage())
		}
	}
	if len(runner.calls) != 0 {
		t.Errorf("runner called: %q", runner.calls)
	}
}
