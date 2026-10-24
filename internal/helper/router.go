// SPDX-License-Identifier: AGPL-3.0-only

package helper

import (
	"context"
	"errors"
	"fmt"

	helperv1 "github.com/sharddevs/waystone_panel/gen/waystone/helper/v1"
)

// Router passes each validated Command to whatever performs it. A nil field
// means this helper doesn't do those actions at all: the runtime helper only
// has Systemd and Journal, and the provisioning helper has neither.
type Router struct {
	Systemd     *Systemd
	Journal     *Journal
	Provisioner *Provisioner
	Worker      *Worker
}

// Handle runs cmd and turns the outcome into a response. It has the same
// signature as Server.Handle, so it can be plugged straight in. An action
// this helper doesn't do gets ERROR_CODE_INVALID_REQUEST and nothing runs.
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
	case helperv1.Action_ACTION_INSTALL_FILE:
		if r.Worker == nil {
			return &helperv1.Response{
				ErrorCode:    helperv1.ErrorCode_ERROR_CODE_INVALID_REQUEST,
				ErrorMessage: "file operations are not supported by this helper",
			}
		}

		if err := r.Worker.InstallFile(ctx, cmd); err != nil {
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

	case helperv1.Action_ACTION_READ_LOG:
		if r.Journal == nil {
			return &helperv1.Response{
				ErrorCode:    helperv1.ErrorCode_ERROR_CODE_INVALID_REQUEST,
				ErrorMessage: "log reads are not supported by this helper",
			}
		}

		log, err := r.Journal.Read(ctx, cmd)
		if err != nil {
			code := helperv1.ErrorCode_ERROR_CODE_FAILED
			switch {
			case errors.Is(err, ErrInvalidRequest):
				code = helperv1.ErrorCode_ERROR_CODE_INVALID_REQUEST
			case errors.Is(err, ErrTooManyReads):
				code = helperv1.ErrorCode_ERROR_CODE_CONFLICT
			}
			return &helperv1.Response{
				ErrorCode:    code,
				ErrorMessage: err.Error(),
			}
		}

		return &helperv1.Response{Log: log}

	default:
		return &helperv1.Response{
			ErrorCode:    helperv1.ErrorCode_ERROR_CODE_INVALID_REQUEST,
			ErrorMessage: "unsupported action",
		}
	}
}

// The roles waystone-helper can run in, picked with its -role flag. Each
// role is its own systemd service with its own socket.
const (
	RoleRuntime   = "runtime"   // waystone-helper.service: START, STOP, STATUS, READ_LOG
	RoleProvision = "provision" // waystone-provisioner.service: PROVISION, file actions
)

// NewRouter builds the real Router for a role. An unknown role is an error,
// and the helper shouldn't start with it.
func NewRouter(role string, runner Runners) (Router, error) {
	switch role {
	case RoleRuntime:
		return Router{
			Systemd: &Systemd{
				Runner: runner,
			},
			Journal: &Journal{
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

// fileErrorResponse maps an error from the Worker to the matching error
// code. Anything it doesn't recognize is ERROR_CODE_FAILED.
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
