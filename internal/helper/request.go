// Package helper holds the privileged helper's protocol handling: framing,
// request validation, caller authorization and the systemd actions.
// The code here is OS-neutral; only cmd/waystone-helper is Linux-only.
package helper

import (
	"errors"
	"fmt"

	helperv1 "github.com/sharddevs/waystone_panel/gen/waystone/helper/v1"
	"github.com/sharddevs/waystone_panel/internal/ids"
)

// Command is a request that passed validation. Only ValidateRequest
// creates one, so code that receives a Command can trust its fields.
type Command struct {
	OperationID string
	ServerID    string
	Action      helperv1.Action
	Spec        Spec
	File        FileCommand
}

// ErrInvalidRequest wraps every validation failure.
var ErrInvalidRequest = errors.New("invalid request")

// ValidateRequest checks every field of req and returns the Command it asks for.
//
// Refuse (with an error that wraps ErrInvalidRequest; use fmt.Errorf with %w):
//   - a nil request;
//   - unknown fields. Protobuf normally keeps fields it does not know, which
//     is right for data but wrong for a security boundary: a newer agent's
//     extra field could be a restriction this helper would silently drop.
//     Look at req.ProtoReflect().GetUnknown();
//   - an operation_id or server_id that fails ids.Check;
//   - any action other than START, STOP or STATUS.
func ValidateRequest(req *helperv1.Request) (Command, error) {
	if req == nil {
		return Command{}, fmt.Errorf("%w: nil request", ErrInvalidRequest)
	}
	if len(req.ProtoReflect().GetUnknown()) != 0 {
		return Command{}, fmt.Errorf("%w: unknown protobuf fields", ErrInvalidRequest)
	}
	if err := ids.Check(req.GetOperationId()); err != nil {
		return Command{}, fmt.Errorf("%w: invalid operation ID: %v", ErrInvalidRequest, err)
	}
	if err := ids.Check(req.GetServerId()); err != nil {
		return Command{}, fmt.Errorf("%w: invalid server ID: %v", ErrInvalidRequest, err)
	}
	var spec Spec
	var file FileCommand
	switch req.GetAction() {
	case helperv1.Action_ACTION_START,
		helperv1.Action_ACTION_STOP,
		helperv1.Action_ACTION_STATUS:
		if req.GetProvision() != nil {
			return Command{}, fmt.Errorf("%w: unexpected provision spec", ErrInvalidRequest)
		}
		if req.GetFile() != nil {
			return Command{}, fmt.Errorf("%w: unexpected file spec", ErrInvalidRequest)
		}
	case helperv1.Action_ACTION_PROVISION:
		if req.GetProvision() == nil {
			return Command{}, fmt.Errorf("%w: missing provision spec", ErrInvalidRequest)
		}
		if req.GetFile() != nil {
			return Command{}, fmt.Errorf("%w: unexpected file spec", ErrInvalidRequest)
		}
		var err error
		spec, err = ValidateSpec(req.GetProvision())
		if err != nil {
			return Command{}, err
		}
	case helperv1.Action_ACTION_READ_FILE,
		helperv1.Action_ACTION_WRITE_FILE:

		if req.GetProvision() != nil {
			return Command{}, fmt.Errorf(
				"%w: unexpected provision spec",
				ErrInvalidRequest,
			)
		}

		var err error
		file, err = ValidateFileSpec(req.GetAction(), req.GetFile())
		if err != nil {
			return Command{}, err
		}
	default:
		return Command{}, fmt.Errorf("%w: unsupported action", ErrInvalidRequest)
	}
	return Command{
		OperationID: req.GetOperationId(),
		ServerID:    req.GetServerId(),
		Action:      req.GetAction(),
		Spec:        spec,
		File:        file,
	}, nil
}

// PeerCred is the identity of the process on the other end of the socket,
// as reported by the kernel.
type PeerCred struct {
	UID uint32
	GID uint32
	PID int32
}

// ErrPermissionDenied is returned by Authorize.
var ErrPermissionDenied = errors.New("permission denied")

// Authorize allows only the agent's user, by uid. Group membership does
// not count. A zero agentUID is a misconfiguration and must allow nobody,
// not every root process. Errors wrap ErrPermissionDenied.
func Authorize(peer PeerCred, agentUID uint32) error {
	if agentUID == 0 || peer.UID != agentUID {
		return fmt.Errorf("%w: unauthorized caller", ErrPermissionDenied)
	}

	return nil
}

// UnitName is the systemd unit of a server: "waystone-srv-<id>.service".
// serverID must already have passed validation.
func UnitName(serverID string) string {
	return "waystone-srv-" + serverID + ".service"
}
