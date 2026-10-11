package helper

import (
	"context"
	"errors"
	"fmt"

	helperv1 "github.com/sharddevs/waystone_panel/gen/waystone/helper/v1"
)

// Router sends each validated command to the code that performs it. A nil
// field means this service doesn't perform those actions at all: the
// runtime helper has no Provisioner or Worker, the provisioning service no
// Systemd.
type Router struct {
	Systemd     *Systemd
	Provisioner *Provisioner
	Worker      *Worker
}

// Handle has the signature of Server.Handle. An action this service
// doesn't perform is answered with ERROR_CODE_INVALID_REQUEST and nothing
// runs.
func (r Router) Handle(ctx context.Context, cmd Command) *helperv1.Response {
	if cmd.Action == helperv1.Action_ACTION_PROVISION {
		if r.Provisioner == nil {
			return &helperv1.Response{
				ErrorCode:    helperv1.ErrorCode_ERROR_CODE_INVALID_REQUEST,
				ErrorMessage: "provisioning is not supported by this helper",
			}
		}

		changed, err := r.Provisioner.Provision(ctx, cmd.ServerID, cmd.Spec)

		if err == nil {
			return &helperv1.Response{
				ErrorCode: helperv1.ErrorCode_ERROR_CODE_UNSPECIFIED,
				Changed:   changed,
			}
		}
		if errors.Is(err, ErrConflict) {
			return &helperv1.Response{
				ErrorCode:    helperv1.ErrorCode_ERROR_CODE_CONFLICT,
				ErrorMessage: err.Error(),
			}
		}

		if errors.Is(err, ErrInvalidRequest) {
			return &helperv1.Response{
				ErrorCode:    helperv1.ErrorCode_ERROR_CODE_INVALID_REQUEST,
				ErrorMessage: err.Error(),
			}
		}

		return &helperv1.Response{
			ErrorCode:    helperv1.ErrorCode_ERROR_CODE_FAILED,
			ErrorMessage: err.Error(),
		}
	}
	switch cmd.Action {
	case helperv1.Action_ACTION_READ_FILE:
		if r.Worker == nil {
			return &helperv1.Response{
				ErrorCode:    helperv1.ErrorCode_ERROR_CODE_INVALID_REQUEST,
				ErrorMessage: "file operations are not supported by this helper",
			}
		}

		result, err := r.Worker.ReadFile(ctx, cmd)
		if err != nil {
			return fileErrorResponse(err)
		}

		return &helperv1.Response{
			ErrorCode: helperv1.ErrorCode_ERROR_CODE_UNSPECIFIED,
			File: &helperv1.FileResult{
				Exists:  result.Exists,
				Content: result.Content,
			},
		}
	case helperv1.Action_ACTION_WRITE_FILE:
		if r.Worker == nil {
			return &helperv1.Response{
				ErrorCode:    helperv1.ErrorCode_ERROR_CODE_INVALID_REQUEST,
				ErrorMessage: "file operations are not supported by this helper",
			}
		}

		err := r.Worker.WriteFile(ctx, cmd)
		if err != nil {
			return fileErrorResponse(err)
		}

		return &helperv1.Response{}

	case helperv1.Action_ACTION_START,
		helperv1.Action_ACTION_STOP,
		helperv1.Action_ACTION_STATUS:

		if r.Systemd == nil {
			return &helperv1.Response{
				ErrorCode:    helperv1.ErrorCode_ERROR_CODE_INVALID_REQUEST,
				ErrorMessage: "lifecycle actions are not supported by this helper",
			}
		}

		return r.Systemd.Handle(ctx, cmd)

	default:
		return &helperv1.Response{
			ErrorCode:    helperv1.ErrorCode_ERROR_CODE_INVALID_REQUEST,
			ErrorMessage: "unsupported action",
		}
	}
}

// Roles of the waystone-helper binary, chosen with its -role flag. Each
// role runs as its own systemd service with its own socket and sandbox.
const (
	RoleRuntime   = "runtime"   // waystone-helper.service: START, STOP, STATUS
	RoleProvision = "provision" // waystone-provisioner.service: PROVISION, READ_FILE, WRITE_FILE
)

// NewRouter returns the production Router for a role. An unknown role is
// an error, and the helper should not start.
func NewRouter(role string, runner Runners) (Router, error) {
	switch role {
	case RoleRuntime:
		return Router{
			Systemd: &Systemd{
				Runner: runner,
			},
		}, nil
	case RoleProvision:
		return Router{
			Provisioner: &Provisioner{
				Accounts: SystemAccounts{
					Runner: runner,
				},
				Units: UnitFiles{
					Dir: DefaultUnitDir,
				},
				Runner:   runner,
				Runtimes: DefaultRuntimes,
			},
			Worker: &Worker{
				Accounts: SystemAccounts{
					Runner: runner,
				},
				Runner: runner,
			},
		}, nil

	}

	return Router{}, fmt.Errorf("unknown helper role %q", role)
}

// fileErrorResponse turns an error from the Worker into a response with
// the matching error code.
func fileErrorResponse(err error) *helperv1.Response {
	code := helperv1.ErrorCode_ERROR_CODE_FAILED

	switch {
	case errors.Is(err, ErrNotProvisioned):
		code = helperv1.ErrorCode_ERROR_CODE_NOT_FOUND
	case errors.Is(err, ErrChanged):
		code = helperv1.ErrorCode_ERROR_CODE_CHANGED
	case errors.Is(err, ErrConflict):
		code = helperv1.ErrorCode_ERROR_CODE_CONFLICT
	case errors.Is(err, ErrInvalidRequest):
		code = helperv1.ErrorCode_ERROR_CODE_INVALID_REQUEST
	}

	return &helperv1.Response{
		ErrorCode:    code,
		ErrorMessage: err.Error(),
	}
}
