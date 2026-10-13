// SPDX-License-Identifier: AGPL-3.0-only

package helper

import (
	"errors"
	"fmt"
	"os"
	"time"

	helperv1 "github.com/sharddevs/waystone_panel/gen/waystone/helper/v1"
	"github.com/sharddevs/waystone_panel/internal/ids"
)

// Command is a request that passed ValidateRequest, so the code that runs
// it can trust its fields.
type Command struct {
	// Input is the file sent along with an INSTALL_FILE request and nil for
	// every other action. The Server fills it in after CheckInput and
	// closes it once the request is finished.
	Input       *os.File
	OperationID string
	ServerID    string
	Action      helperv1.Action
	Spec        Spec
	File        FileCommand
	// NotAfter is the sender's deadline for START and STOP, zero for every
	// other action. The action must not begin after it.
	NotAfter time.Time
}

// ErrInvalidRequest is wrapped by every validation error, so callers can
// check for it with errors.Is.
var ErrInvalidRequest = errors.New("invalid request")

// ValidateRequest checks every field of req and turns it into a Command.
// Each action may carry only the spec that belongs to it.
func ValidateRequest(req *helperv1.Request) (Command, error) {
	if req == nil {
		return Command{}, fmt.Errorf("%w: nil request", ErrInvalidRequest)
	}
	// Protobuf silently keeps fields it doesn't know. At a root boundary
	// that's risky: a field from a newer agent might be a restriction we'd
	// ignore without noticing, so we refuse the request instead.
	if len(req.ProtoReflect().GetUnknown()) != 0 {
		return Command{}, fmt.Errorf("%w: unknown protobuf fields", ErrInvalidRequest)
	}
	if err := ids.Check(req.GetOperationId()); err != nil {
		return Command{}, fmt.Errorf("%w: invalid operation ID: %v", ErrInvalidRequest, err)
	}
	if err := ids.Check(req.GetServerId()); err != nil {
		return Command{}, fmt.Errorf("%w: invalid server ID: %v", ErrInvalidRequest, err)
	}
	// START and STOP must say when the sender gives up; nothing else may.
	var notAfter time.Time
	switch req.GetAction() {
	case helperv1.Action_ACTION_START, helperv1.Action_ACTION_STOP:
		if req.GetNotAfterUnixMs() <= 0 {
			return Command{}, fmt.Errorf("%w: missing deadline", ErrInvalidRequest)
		}
		notAfter = time.UnixMilli(req.GetNotAfterUnixMs())
	default:
		if req.GetNotAfterUnixMs() != 0 {
			return Command{}, fmt.Errorf("%w: unexpected deadline", ErrInvalidRequest)
		}
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
		helperv1.Action_ACTION_WRITE_FILE,
		helperv1.Action_ACTION_INSTALL_FILE:

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
		NotAfter:    notAfter,
	}, nil
}

// PeerCred is who the kernel says is on the other end of the socket.
type PeerCred struct {
	UID uint32
	GID uint32
	PID int32
}

// ErrPermissionDenied is wrapped by the error Authorize returns.
var ErrPermissionDenied = errors.New("permission denied")

// Authorize lets in only the agent's user, matched by uid; being in the
// right group isn't enough. An agentUID of 0 means the helper was set up
// wrong, so it lets in nobody instead of every root process.
func Authorize(peer PeerCred, agentUID uint32) error {
	if agentUID == 0 || peer.UID != agentUID {
		return fmt.Errorf("%w: unauthorized caller", ErrPermissionDenied)
	}

	return nil
}

// UnitName returns the systemd unit name for a server,
// "waystone-srv-<id>.service". serverID has to be validated already.
func UnitName(serverID string) string {
	return "waystone-srv-" + serverID + ".service"
}
