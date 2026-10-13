package helper

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

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
	// Busy tracks the servers with a START or STOP in progress. nil means
	// the one shared by the whole helper process, which is what makes the
	// exclusion hold across connections (stage 4c, S3).
	Busy *Busy
	// Now is the clock for request deadlines. nil means time.Now.
	Now func() time.Time
}

// Busy is the set of servers that have a START or STOP running in this
// helper. Every START and STOP passes through the helper, whatever agent
// session or agent process sent it, so this is where two actions on one
// server are kept apart.
type Busy struct {
	mu      sync.Mutex
	servers map[string]bool
}

// processBusy is the helper process's own set.
var processBusy = &Busy{}

// acquire marks serverID busy and returns the function that frees it, or
// false if it was already busy. It never waits: a second action is
// refused, not queued.
func (b *Busy) acquire(serverID string) (release func(), ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.servers[serverID] {
		return nil, false
	}
	if b.servers == nil {
		b.servers = make(map[string]bool)
	}
	b.servers[serverID] = true
	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		delete(b.servers, serverID)
	}, true
}

// Why a START or STOP was refused without running anything. They all
// become ERROR_CODE_CONFLICT.
var (
	ErrServerBusy     = errors.New("another action on this server is still running")
	ErrJobActive      = errors.New("systemd still has a job for this server")
	ErrRequestExpired = errors.New("the request's deadline has passed")
)

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

// jobActive reports whether systemd is still busy with the server's unit:
// it has a job queued or running (from any source, including a systemctl
// this helper killed at its deadline), or the unit is in the middle of a
// change. The one exception is exactly ActiveState "activating" with
// SubState "auto-restart" and no job: a crash loop waiting to restart
// isn't a change in progress, so it can still be stopped. Every other
// transitional state is busy, whatever its SubState. Anything it can't
// read counts as an error, never as "not busy".
func (s Systemd) jobActive(ctx context.Context, serverID string) (bool, error) {
	argv := []string{
		Systemctl,
		"--no-ask-password",
		"show",
		"--property=LoadState,ActiveState,SubState,Job",
		"--",
		UnitName(serverID),
	}
	output, err := s.Runner.Run(ctx, argv)
	if err != nil {
		return false, err
	}
	properties := make(map[string]string)
	for _, line := range strings.Split(string(output), "\n") {
		key, value, found := strings.Cut(line, "=")
		if found {
			properties[key] = value
		}
	}
	for _, key := range []string{"LoadState", "ActiveState", "SubState", "Job"} {
		if _, ok := properties[key]; !ok {
			return false, fmt.Errorf("systemctl show: no %s", key)
		}
	}
	if properties["LoadState"] == "not-found" {
		return false, ErrNotFound
	}
	if properties["Job"] != "" {
		return true, nil
	}
	switch properties["ActiveState"] {
	case "activating":
		return properties["SubState"] != "auto-restart", nil
	case "deactivating", "reloading", "refreshing":
		return true, nil
	}
	return false, nil
}

// change runs a START or STOP, in this order (S3):
//
//  1. Mark the server busy, or refuse if another START or STOP for it is
//     still running in this helper.
//  2. Refuse if systemd still has a job for the unit or it is mid-change.
//  3. Refuse if the request's deadline has passed, checked last so it is
//     as close as possible to the systemctl call.
//  4. Run systemctl. The server stays busy until it returns.
func (s Systemd) change(ctx context.Context, cmd Command) error {
	busy := s.Busy
	if busy == nil {
		busy = processBusy
	}
	release, ok := busy.acquire(cmd.ServerID)
	if !ok {
		return ErrServerBusy
	}
	defer release()

	active, err := s.jobActive(ctx, cmd.ServerID)
	if err != nil {
		return err
	}
	if active {
		return ErrJobActive
	}

	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	if !now().Before(cmd.NotAfter) {
		return ErrRequestExpired
	}

	if cmd.Action == helperv1.Action_ACTION_START {
		return s.Start(ctx, cmd.ServerID)
	}
	return s.Stop(ctx, cmd.ServerID)
}

// Handle performs a START, STOP or STATUS command and builds the response.
// A START or STOP refused by change becomes ERROR_CODE_CONFLICT,
// ErrNotFound becomes ERROR_CODE_NOT_FOUND, and any other error
// ERROR_CODE_FAILED.
func (s Systemd) Handle(ctx context.Context, cmd Command) *helperv1.Response {
	var err error

	switch cmd.Action {
	case helperv1.Action_ACTION_START, helperv1.Action_ACTION_STOP:
		err = s.change(ctx, cmd)
		if err == nil {
			return &helperv1.Response{
				ErrorCode: helperv1.ErrorCode_ERROR_CODE_UNSPECIFIED,
			}
		}
		if errors.Is(err, ErrServerBusy) || errors.Is(err, ErrJobActive) || errors.Is(err, ErrRequestExpired) {
			return &helperv1.Response{
				ErrorCode:    helperv1.ErrorCode_ERROR_CODE_CONFLICT,
				ErrorMessage: err.Error(),
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
