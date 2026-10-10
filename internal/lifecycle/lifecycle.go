// Package lifecycle is the agent's view of a game server's lifecycle.
// The dev harness (stage 3) and the agent's controller-facing service
// (stage 4) both call a Runtime, so the harness exercises the real path.
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"strings"

	helperv1 "github.com/sharddevs/waystone_panel/gen/waystone/helper/v1"
	"github.com/sharddevs/waystone_panel/internal/helper"
)

// Status is a server's run state plus the runtime's own description.
type Status struct {
	State  helperv1.RunState
	Native string
}

// Runtime starts, stops and inspects game servers.
type Runtime interface {
	Start(ctx context.Context, serverID, operationID string) error
	Stop(ctx context.Context, serverID, operationID string) error
	Status(ctx context.Context, serverID, operationID string) (Status, error)
}

// Errors returned for the helper's error codes.
var (
	ErrPermissionDenied = errors.New("helper refused: permission denied")
	ErrInvalidRequest   = errors.New("helper refused: invalid request")
	ErrNotFound         = errors.New("server not found on this machine")
	ErrFailed           = errors.New("action failed")
)

// HelperRuntime is the native systemd runtime. Every action goes through
// the privileged helper; the agent itself never touches systemd.
type HelperRuntime struct {
	Client helper.Client
}

var _ Runtime = HelperRuntime{}

// do sends one action to the helper and turns its error code into an error.
//   - A failed exchange: return an error that says the helper could not be reached.
//   - ERROR_CODE_UNSPECIFIED: success, return the response.
//   - PERMISSION_DENIED, INVALID_REQUEST, NOT_FOUND, FAILED: return an error
//     wrapping ErrPermissionDenied, ErrInvalidRequest, ErrNotFound or
//     ErrFailed, including the helper's error_message.
//   - Any code this agent does not know: ErrFailed. Never treat an unknown
//     answer as success.
func (h HelperRuntime) do(ctx context.Context, action helperv1.Action, serverID, operationID string) (*helperv1.Response, error) {
	req := &helperv1.Request{
		OperationId: operationID,
		ServerId:    serverID,
		Action:      action,
	}
	resp, err := h.Client.Do(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("could not reach helper: %w", err)
	}
	switch resp.GetErrorCode() {
	case helperv1.ErrorCode_ERROR_CODE_UNSPECIFIED:
		return resp, nil

	case helperv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED:
		return nil, fmt.Errorf("%w: %s", ErrPermissionDenied, resp.GetErrorMessage())

	case helperv1.ErrorCode_ERROR_CODE_INVALID_REQUEST:
		return nil, fmt.Errorf("%w: %s", ErrInvalidRequest,
			strings.TrimPrefix(resp.GetErrorMessage(), "invalid request: "))

	case helperv1.ErrorCode_ERROR_CODE_NOT_FOUND:
		return nil, fmt.Errorf("%w: %s", ErrNotFound, resp.GetErrorMessage())

	case helperv1.ErrorCode_ERROR_CODE_FAILED:
		return nil, fmt.Errorf("%w: %s", ErrFailed, resp.GetErrorMessage())

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
