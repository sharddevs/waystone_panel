package helper

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"

	helperv1 "github.com/sharddevs/waystone_panel/gen/waystone/helper/v1"
)

// Systemctl is the absolute path of systemctl. We never look it up in PATH.
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

// Run implements Runner with os/exec. There is no shell involved, and the
// program's stderr ends up in the error so a failure says why.
func (r ExecRunner) Run(ctx context.Context, argv []string) ([]byte, error) {
	return r.RunInput(ctx, argv, nil)
}

// runCommand is the shared code behind Run and RunInput.
func runCommand(ctx context.Context, argv []string, stdin io.Reader) ([]byte, error) {
	if len(argv) == 0 {
		return nil, errors.New("empty command")
	}

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	// Only LC_ALL=C: output is never translated, and nothing from the
	// helper's own environment leaks into the program.
	cmd.Env = []string{"LC_ALL=C"}
	cmd.Stdin = stdin

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

// Start starts the server's unit.
func (s Systemd) Start(ctx context.Context, serverID string) error {
	argv := []string{
		Systemctl,
		"--no-ask-password",
		"start",
		// "--" ends option parsing, so the unit name can never be read as
		// an option.
		"--",
		UnitName(serverID),
	}

	_, err := s.Runner.Run(ctx, argv)
	return err
}

// Stop stops the server's unit.
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
// it for logs, like "active/running result=success". It returns ErrNotFound
// if systemd doesn't know the unit.
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
	// The output is KEY=value lines, in no guaranteed order.
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
		// A state we don't recognize stays unspecified; we don't guess.
		state = helperv1.RunState_RUN_STATE_UNSPECIFIED
	}

	return state, native, nil
}

// Handle performs a START, STOP or STATUS command and builds the response.
// ErrNotFound becomes ERROR_CODE_NOT_FOUND and any other error
// ERROR_CODE_FAILED.
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
