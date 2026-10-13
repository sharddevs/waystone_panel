// SPDX-License-Identifier: AGPL-3.0-only

// Package lifecycle is how the agent runs game servers. It doesn't do the
// work itself; it asks the root helper and the provisioner over their sockets.
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	helperv1 "github.com/sharddevs/waystone_panel/gen/waystone/helper/v1"
	"github.com/sharddevs/waystone_panel/internal/helper"
)

// Status is a server's run state. Native is the runtime's own description
// of it, only meant for logs and debugging.
type Status struct {
	State  helperv1.RunState
	Native string
}

// Runtime is everything the agent can do to a game server. It's an
// interface so code that uses it can be tested with a fake.
type Runtime interface {
	Start(ctx context.Context, serverID, operationID string) error
	Stop(ctx context.Context, serverID, operationID string) error
	Status(ctx context.Context, serverID, operationID string) (Status, error)
	Provision(ctx context.Context, serverID, operationID string, spec ProvisionSpec) (bool, error)
	ReadFile(ctx context.Context, serverID, operationID, name string) (File, error)
	WriteFile(ctx context.Context, serverID, operationID, name string, content []byte, base File) error
	InstallFile(ctx context.Context, serverID, operationID, name string, content *os.File, sum [32]byte) error
}

// These errors match the helper's error codes. Returned errors wrap them,
// so check with errors.Is.
var (
	ErrPermissionDenied = errors.New("helper refused: permission denied")
	ErrInvalidRequest   = errors.New("helper refused: invalid request")
	ErrNotFound         = errors.New("server not found on this machine")
	ErrFailed           = errors.New("action failed")
)

// HelperRuntime is the real Runtime. Client talks to the root helper
// (start, stop, status) and Provisioner to the provisioner (provisioning
// and files).
type HelperRuntime struct {
	Client      helper.Client
	Provisioner helper.Client
}

// Assigning to _ makes the build fail if HelperRuntime ever stops
// satisfying Runtime.
var _ Runtime = HelperRuntime{}

// do sends a start, stop or status action to the root helper.
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
	// START and STOP carry the caller's deadline, so the helper never
	// begins one after the caller has given up on it (stage 4c, S3).
	// Without a deadline on ctx, the helper's own connection limit is the
	// most it would wait anyway.
	if action == helperv1.Action_ACTION_START || action == helperv1.Action_ACTION_STOP {
		deadline, ok := ctx.Deadline()
		if !ok {
			deadline = time.Now().Add(helper.ConnTimeout)
		}
		req.NotAfterUnixMs = deadline.UnixMilli()
	}

	return h.send(ctx, h.Client, req)
}
func (h HelperRuntime) send(
	ctx context.Context,
	client helper.Client,
	req *helperv1.Request,
) (*helperv1.Response, error) {
	return h.sendInput(ctx, client, req, nil)
}

// sendInput sends req to client, with input's descriptor attached if it
// isn't nil, and turns the helper's error code into one of our errors.
func (h HelperRuntime) sendInput(
	ctx context.Context,
	client helper.Client,
	req *helperv1.Request,
	input *os.File,
) (*helperv1.Response, error) {
	resp, err := client.DoInput(ctx, req, input)
	if err != nil {
		return nil, fmt.Errorf("could not reach helper: %w", err)
	}

	// %w keeps the sentinel error inside, so errors.Is still finds it.
	switch resp.GetErrorCode() {
	case helperv1.ErrorCode_ERROR_CODE_UNSPECIFIED:
		return resp, nil

	case helperv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED:
		return nil, fmt.Errorf("%w: %s", ErrPermissionDenied, resp.GetErrorMessage())

	case helperv1.ErrorCode_ERROR_CODE_INVALID_REQUEST:
		// The helper's message already starts with "invalid request: ",
		// which would just repeat our own error text.
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

	// A code we don't know about must never count as success.
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
