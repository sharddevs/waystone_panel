package helper

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	helperv1 "github.com/sharddevs/waystone_panel/gen/waystone/helper/v1"
)

// Systemctl is the absolute path the helper runs. Never looked up in PATH.
const Systemctl = "/usr/bin/systemctl"

// ErrNotFound means systemd does not know the server's unit.
var ErrNotFound = errors.New("unit not found")

// Runner runs a program with exactly the given argv (argv[0] is the
// program) and returns its standard output. Tests swap in a fake.
type Runner interface {
	Run(ctx context.Context, argv []string) ([]byte, error)
}

// ExecRunner runs programs directly, without a shell.
type ExecRunner struct{}

// Run implements Runner with os/exec.
//   - Never go through a shell ("sh -c").
//   - Give the program a minimal environment: just LC_ALL=C, so output
//     is never translated and nothing from the helper's environment leaks in.
//   - On failure, include the program's stderr in the error; it says why.
func (ExecRunner) Run(ctx context.Context, argv []string) ([]byte, error) {
	if len(argv) == 0 {
		return nil, errors.New("empty command")
	}

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = []string{"LC_ALL=C"}
	output, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil, fmt.Errorf("command failed: %w: %s", err, exitErr.Stderr)
		}
		return nil, err
	}
	return output, nil
}

// Systemd performs server actions through systemctl.
type Systemd struct {
	Runner Runner
}

// Start starts the server's unit. Exact argv:
//
//	/usr/bin/systemctl --no-ask-password start -- waystone-srv-<id>.service
//
// "--" ends option parsing, so a unit name can never be read as an option.
func (s Systemd) Start(ctx context.Context, serverID string) error {
	argv := []string{
		Systemctl,
		"--no-ask-password",
		"start",
		"--",
		UnitName(serverID),
	}

	_, err := s.Runner.Run(ctx, argv)
	return err
}

// Stop stops the server's unit. Same argv as Start, with "stop".
func (s Systemd) Stop(ctx context.Context, serverID string) error {
	argv := []string{
		Systemctl,
		"--no-ask-password",
		"stop",
		"--",
		UnitName(serverID),
	}

	_, err := s.Runner.Run(ctx, argv)
	return err
}

// Status returns the server's run state and systemd's own description of
// it. Exact argv:
//
//	/usr/bin/systemctl --no-ask-password show --property=LoadState,ActiveState,SubState,Result -- waystone-srv-<id>.service
//
// The output is KEY=value lines, in no guaranteed order.
//   - LoadState=not-found: return ErrNotFound.
//   - Map ActiveState: active, reloading, refreshing -> RUNNING;
//     activating -> STARTING; deactivating -> STOPPING; inactive -> STOPPED;
//     failed -> FAILED; anything else -> UNSPECIFIED (never guess).
//   - The second return value is a non-empty description for logs, e.g.
//     "active/running result=success".
//   - Return the runner's error unchanged (wrapping is fine).
func (s Systemd) Status(ctx context.Context, serverID string) (helperv1.RunState, string, error) {
	argv := []string{
		Systemctl,
		"--no-ask-password",
		"show",
		"--property=LoadState,ActiveState,SubState,Result",
		"--",
		UnitName(serverID),
	}

	output, err := s.Runner.Run(ctx, argv)
	if err != nil {
		return helperv1.RunState_RUN_STATE_UNSPECIFIED, "", err
	}
	properties := make(map[string]string)
	for _, line := range strings.Split(string(output), "\n") {
		key, value, found := strings.Cut(line, "=")
		if found {
			properties[key] = value
		}
	}
	if properties["LoadState"] == "not-found" {
		return helperv1.RunState_RUN_STATE_UNSPECIFIED, "", ErrNotFound
	}
	active := properties["ActiveState"]
	sub := properties["SubState"]
	result := properties["Result"]

	native := active + "/" + sub + " result=" + result
	var state helperv1.RunState
	switch active {
	case "active", "reloading", "refreshing":
		state = helperv1.RunState_RUN_STATE_RUNNING
	case "activating":
		state = helperv1.RunState_RUN_STATE_STARTING
	case "deactivating":
		state = helperv1.RunState_RUN_STATE_STOPPING
	case "inactive":
		state = helperv1.RunState_RUN_STATE_STOPPED
	case "failed":
		state = helperv1.RunState_RUN_STATE_FAILED
	default:
		state = helperv1.RunState_RUN_STATE_UNSPECIFIED
	}

	return state, native, nil
}

// Handle performs a validated command and builds the response.
//   - Success: ERROR_CODE_UNSPECIFIED; for STATUS also run_state and native_state.
//   - ErrNotFound: ERROR_CODE_NOT_FOUND.
//   - Any other error: ERROR_CODE_FAILED.
//   - Every error response has a non-empty error_message.
func (s Systemd) Handle(ctx context.Context, cmd Command) *helperv1.Response {
	var err error

	switch cmd.Action {
	case helperv1.Action_ACTION_START:
		err = s.Start(ctx, cmd.ServerID)
		if err == nil {
			return &helperv1.Response{
				ErrorCode: helperv1.ErrorCode_ERROR_CODE_UNSPECIFIED,
			}
		}
	case helperv1.Action_ACTION_STOP:
		err = s.Stop(ctx, cmd.ServerID)
		if err == nil {
			return &helperv1.Response{
				ErrorCode: helperv1.ErrorCode_ERROR_CODE_UNSPECIFIED,
			}
		}

	case helperv1.Action_ACTION_STATUS:
		var state helperv1.RunState
		var native string

		state, native, err = s.Status(ctx, cmd.ServerID)

		if err == nil {
			return &helperv1.Response{
				ErrorCode:   helperv1.ErrorCode_ERROR_CODE_UNSPECIFIED,
				RunState:    state,
				NativeState: native,
			}
		}
	default:
		return &helperv1.Response{
			ErrorCode:    helperv1.ErrorCode_ERROR_CODE_FAILED,
			ErrorMessage: "unsupported action",
		}
	}
	if errors.Is(err, ErrNotFound) {
		return &helperv1.Response{
			ErrorCode:    helperv1.ErrorCode_ERROR_CODE_NOT_FOUND,
			ErrorMessage: err.Error(),
		}
	}
	return &helperv1.Response{
		ErrorCode:    helperv1.ErrorCode_ERROR_CODE_FAILED,
		ErrorMessage: err.Error(),
	}
}
