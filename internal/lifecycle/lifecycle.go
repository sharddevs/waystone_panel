// Package lifecycle is the agent's side of running a game server: it asks
// the root helper and the provisioner to start, stop, provision and edit files.
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"strings"

	helperv1 "github.com/sharddevs/waystone_panel/gen/waystone/helper/v1"
	"github.com/sharddevs/waystone_panel/internal/helper"
)

// Status is a server's run state plus systemd's own description of it
// (like "active/running result=success"), which is handy in logs.
type Status struct {
	State  helperv1.RunState
	Native string
}

// Runtime is everything the agent can do to a game server. It's an
// interface so callers can be tested with a fake.
type Runtime interface {
	Start(ctx context.Context, serverID, operationID string) error
	Stop(ctx context.Context, serverID, operationID string) error
	Status(ctx context.Context, serverID, operationID string) (Status, error)
	Provision(ctx context.Context, serverID, operationID string, spec ProvisionSpec) (bool, error)
	ReadFile(ctx context.Context, serverID, operationID, name string) (File, error)
	WriteFile(ctx context.Context, serverID, operationID, name string, content []byte, base File) error
}

// Errors for the helper's error codes. Check them with errors.Is.
var (
	ErrPermissionDenied = errors.New("helper refused: permission denied")
	ErrInvalidRequest   = errors.New("helper refused: invalid request")
	ErrNotFound         = errors.New("server not found on this machine")
	ErrFailed           = errors.New("action failed")
)

// HelperRuntime runs servers through the privileged helpers; the agent never
// touches systemd itself. Client talks to the root helper (start, stop,
// status), Provisioner to the provisioning service (provision, files).
type HelperRuntime struct {
	Client      helper.Client
	Provisioner helper.Client
}

// This line doesn't compile if HelperRuntime stops satisfying Runtime.
var _ Runtime = HelperRuntime{}

// do sends a start, stop or status action to the runtime helper. The error
// handling is in send, which the provisioner's actions use too.
func (h HelperRuntime) do(
	ctx context.Context,
	action helperv1.Action,
	serverID, operationID string,
) (*helperv1.Response, error) {
	req := &helperv1.Request{
		OperationId: operationID,
		ServerId:    serverID,
		Action:      action,
	}

	return h.send(ctx, h.Client, req)
}
func (h HelperRuntime) send(
	ctx context.Context,
	client helper.Client,
	req *helperv1.Request,
) (*helperv1.Response, error) {
	resp, err := client.Do(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("could not reach helper: %w", err)
	}

	// %w wraps our sentinel error so callers can still use errors.Is.
	switch resp.GetErrorCode() {
	case helperv1.ErrorCode_ERROR_CODE_UNSPECIFIED:
		return resp, nil

	case helperv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED:
		return nil, fmt.Errorf("%w: %s", ErrPermissionDenied, resp.GetErrorMessage())

	case helperv1.ErrorCode_ERROR_CODE_INVALID_REQUEST:
		// The helper's message already starts with "invalid request: ",
		// which would repeat our own error text.
		return nil, fmt.Errorf("%w: %s", ErrInvalidRequest,
			strings.TrimPrefix(resp.GetErrorMessage(), "invalid request: "))

	case helperv1.ErrorCode_ERROR_CODE_NOT_FOUND:
		return nil, fmt.Errorf("%w: %s", ErrNotFound, resp.GetErrorMessage())

	case helperv1.ErrorCode_ERROR_CODE_CONFLICT:
		return nil, fmt.Errorf("%w: %s", ErrConflict, resp.GetErrorMessage())

	case helperv1.ErrorCode_ERROR_CODE_CHANGED:
		return nil, fmt.Errorf("%w: %s", ErrChanged, resp.GetErrorMessage())

	case helperv1.ErrorCode_ERROR_CODE_FAILED:
		return nil, fmt.Errorf("%w: %s", ErrFailed, resp.GetErrorMessage())

	// A code we don't know is never treated as success.
	default:
		return nil, fmt.Errorf("%w: unknown helper error code %d: %s",
			ErrFailed, resp.GetErrorCode(), resp.GetErrorMessage())
	}
}

// Start implements Runtime.
func (h HelperRuntime) Start(ctx context.Context, serverID, operationID string) error {
	_, err := h.do(ctx, helperv1.Action_ACTION_START, serverID, operationID)
	return err
}

// Stop implements Runtime.
func (h HelperRuntime) Stop(ctx context.Context, serverID, operationID string) error {
	_, err := h.do(ctx, helperv1.Action_ACTION_STOP, serverID, operationID)
	return err
}

// Status implements Runtime.
func (h HelperRuntime) Status(ctx context.Context, serverID, operationID string) (Status, error) {
	resp, err := h.do(ctx, helperv1.Action_ACTION_STATUS, serverID, operationID)
	if err != nil {
		return Status{}, err
	}

	return Status{
		State:  resp.GetRunState(),
		Native: resp.GetNativeState(),
	}, nil
}
