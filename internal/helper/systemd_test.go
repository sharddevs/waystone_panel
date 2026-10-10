package helper

import (
	"context"
	"errors"
	"slices"
	"testing"

	helperv1 "github.com/sharddevs/waystone_panel/gen/waystone/helper/v1"
)

type fakeRunner struct {
	calls  [][]string
	output string
	err    error
}

func (f *fakeRunner) Run(_ context.Context, argv []string) ([]byte, error) {
	f.calls = append(f.calls, slices.Clone(argv))
	return []byte(f.output), f.err
}

const testUnit = "waystone-srv-3f2504e0-4f89-41d3-9a0c-0305e82c3301.service"

// The exact command lines are part of the security review: an absolute
// program path, no shell, no password prompts, and "--" so a unit name can
// never be read as an option.
func TestSystemdArgv(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name string
		call func(Systemd) error
		want []string
	}{
		{"start", func(s Systemd) error { return s.Start(ctx, testServerID) },
			[]string{"/usr/bin/systemctl", "--no-ask-password", "start", "--", testUnit}},
		{"stop", func(s Systemd) error { return s.Stop(ctx, testServerID) },
			[]string{"/usr/bin/systemctl", "--no-ask-password", "stop", "--", testUnit}},
		{"status", func(s Systemd) error { _, _, err := s.Status(ctx, testServerID); return err },
			[]string{"/usr/bin/systemctl", "--no-ask-password", "show", "--property=LoadState,ActiveState,SubState,Result", "--", testUnit}},
	}
	for _, tt := range tests {
		r := &fakeRunner{output: "LoadState=loaded\nActiveState=active\nSubState=running\nResult=success\n"}
		if err := tt.call(Systemd{Runner: r}); err != nil {
			t.Errorf("%s: unexpected error %v", tt.name, err)
		}
		if len(r.calls) != 1 || !slices.Equal(r.calls[0], tt.want) {
			t.Errorf("%s: ran %q, want exactly %q", tt.name, r.calls, tt.want)
		}
	}
}

func TestSystemdStatusStates(t *testing.T) {
	tests := []struct {
		output string
		want   helperv1.RunState
	}{
		{"LoadState=loaded\nActiveState=active\nSubState=running\nResult=success\n", helperv1.RunState_RUN_STATE_RUNNING},
		{"LoadState=loaded\nActiveState=inactive\nSubState=dead\nResult=success\n", helperv1.RunState_RUN_STATE_STOPPED},
		{"LoadState=loaded\nActiveState=failed\nSubState=failed\nResult=exit-code\n", helperv1.RunState_RUN_STATE_FAILED},
		{"LoadState=loaded\nActiveState=activating\nSubState=auto-restart\nResult=signal\n", helperv1.RunState_RUN_STATE_STARTING},
		{"LoadState=loaded\nActiveState=deactivating\nSubState=stop-sigterm\nResult=success\n", helperv1.RunState_RUN_STATE_STOPPING},
		// systemd does not promise property order.
		{"Result=success\nSubState=running\nActiveState=active\nLoadState=loaded\n", helperv1.RunState_RUN_STATE_RUNNING},
		// A state this code does not know must not be guessed.
		{"LoadState=loaded\nActiveState=maintenance\nSubState=x\nResult=success\n", helperv1.RunState_RUN_STATE_UNSPECIFIED},
	}
	for _, tt := range tests {
		s := Systemd{Runner: &fakeRunner{output: tt.output}}
		got, native, err := s.Status(context.Background(), testServerID)
		if err != nil {
			t.Errorf("%q: unexpected error %v", tt.output, err)
			continue
		}
		if got != tt.want {
			t.Errorf("%q: state %v, want %v", tt.output, got, tt.want)
		}
		if native == "" {
			t.Errorf("%q: native state is empty, want systemd's own description", tt.output)
		}
	}
}

func TestSystemdStatusNotFound(t *testing.T) {
	s := Systemd{Runner: &fakeRunner{output: "LoadState=not-found\nActiveState=inactive\nSubState=dead\nResult=success\n"}}
	if _, _, err := s.Status(context.Background(), testServerID); !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

func TestSystemdPropagatesRunnerErrors(t *testing.T) {
	boom := errors.New("boom")
	s := Systemd{Runner: &fakeRunner{err: boom}}
	ctx := context.Background()
	if err := s.Start(ctx, testServerID); !errors.Is(err, boom) {
		t.Errorf("Start: got %v, want the runner's error", err)
	}
	if err := s.Stop(ctx, testServerID); !errors.Is(err, boom) {
		t.Errorf("Stop: got %v, want the runner's error", err)
	}
	if _, _, err := s.Status(ctx, testServerID); !errors.Is(err, boom) {
		t.Errorf("Status: got %v, want the runner's error", err)
	}
}

func TestSystemdHandle(t *testing.T) {
	ctx := context.Background()
	running := "LoadState=loaded\nActiveState=active\nSubState=running\nResult=success\n"
	notFound := "LoadState=not-found\nActiveState=inactive\nSubState=dead\nResult=success\n"

	tests := []struct {
		name      string
		runner    *fakeRunner
		action    helperv1.Action
		wantCode  helperv1.ErrorCode
		wantState helperv1.RunState
	}{
		{"start ok", &fakeRunner{}, helperv1.Action_ACTION_START, helperv1.ErrorCode_ERROR_CODE_UNSPECIFIED, helperv1.RunState_RUN_STATE_UNSPECIFIED},
		{"stop ok", &fakeRunner{}, helperv1.Action_ACTION_STOP, helperv1.ErrorCode_ERROR_CODE_UNSPECIFIED, helperv1.RunState_RUN_STATE_UNSPECIFIED},
		{"start fails", &fakeRunner{err: errors.New("boom")}, helperv1.Action_ACTION_START, helperv1.ErrorCode_ERROR_CODE_FAILED, helperv1.RunState_RUN_STATE_UNSPECIFIED},
		{"status running", &fakeRunner{output: running}, helperv1.Action_ACTION_STATUS, helperv1.ErrorCode_ERROR_CODE_UNSPECIFIED, helperv1.RunState_RUN_STATE_RUNNING},
		{"status not found", &fakeRunner{output: notFound}, helperv1.Action_ACTION_STATUS, helperv1.ErrorCode_ERROR_CODE_NOT_FOUND, helperv1.RunState_RUN_STATE_UNSPECIFIED},
	}
	for _, tt := range tests {
		resp := Systemd{Runner: tt.runner}.Handle(ctx, Command{OperationID: testOpID, ServerID: testServerID, Action: tt.action})
		if resp.GetErrorCode() != tt.wantCode {
			t.Errorf("%s: error code %v, want %v (%s)", tt.name, resp.GetErrorCode(), tt.wantCode, resp.GetErrorMessage())
		}
		if resp.GetRunState() != tt.wantState {
			t.Errorf("%s: run state %v, want %v", tt.name, resp.GetRunState(), tt.wantState)
		}
		if tt.wantCode != helperv1.ErrorCode_ERROR_CODE_UNSPECIFIED && resp.GetErrorMessage() == "" {
			t.Errorf("%s: error without a message", tt.name)
		}
	}
}
