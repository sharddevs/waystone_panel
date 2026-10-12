// SPDX-License-Identifier: AGPL-3.0-only

package helper

import (
	"context"
	"errors"
	"testing"

	helperv1 "github.com/sharddevs/waystone_panel/gen/waystone/helper/v1"
)

func TestRouterInstallsFiles(t *testing.T) {
	runner := &fileRunner{output: `{"bytes":3}`}
	r := Router{Worker: &Worker{Accounts: accountsWith(goodAccount()), Runner: runner}}
	resp := r.Handle(context.Background(), installCommand(t))
	if resp.GetErrorCode() != helperv1.ErrorCode_ERROR_CODE_UNSPECIFIED || len(runner.calls) != 1 {
		t.Errorf("INSTALL_FILE: %v %q, %d runs", resp.GetErrorCode(), resp.GetErrorMessage(), len(runner.calls))
	}
	if resp.GetFile() != nil {
		t.Errorf("INSTALL_FILE answered with file content %v", resp.GetFile())
	}
}

func TestRouterInstallErrors(t *testing.T) {
	tests := map[string]struct {
		w    Worker
		want helperv1.ErrorCode
	}{
		"not provisioned": {
			Worker{Accounts: accountsWith(), Runner: &fileRunner{}},
			helperv1.ErrorCode_ERROR_CODE_NOT_FOUND,
		},
		"foreign account": {
			Worker{Accounts: accountsWith(Account{Name: testUser, UID: 994, Comment: "x"}), Runner: &fileRunner{}},
			helperv1.ErrorCode_ERROR_CODE_CONFLICT,
		},
		"hash mismatch": {
			Worker{Accounts: accountsWith(goodAccount()), Runner: &fileRunner{err: errors.New("exit status 1: waystone-worker: content does not match sha256")}},
			helperv1.ErrorCode_ERROR_CODE_FAILED,
		},
	}
	for name, tt := range tests {
		resp := Router{Worker: &tt.w}.Handle(context.Background(), installCommand(t))
		if resp.GetErrorCode() != tt.want || resp.GetErrorMessage() == "" {
			t.Errorf("%s: %v %q, want %v with a message", name, resp.GetErrorCode(), resp.GetErrorMessage(), tt.want)
		}
	}
}

// The runtime helper's Router has no Worker, so it refuses installs before
// running anything.
func TestRouterWithoutWorkerRefusesInstall(t *testing.T) {
	runner := &fakeRunner{}
	resp := Router{Systemd: &Systemd{Runner: runner}}.Handle(context.Background(), installCommand(t))
	if resp.GetErrorCode() != helperv1.ErrorCode_ERROR_CODE_INVALID_REQUEST || resp.GetErrorMessage() == "" {
		t.Errorf("%v %q, want INVALID_REQUEST with a message", resp.GetErrorCode(), resp.GetErrorMessage())
	}
	if len(runner.calls) != 0 {
		t.Errorf("runner called: %q", runner.calls)
	}
}
