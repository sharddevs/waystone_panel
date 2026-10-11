// SPDX-License-Identifier: AGPL-3.0-only

package lifecycle

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"

	helperv1 "github.com/sharddevs/waystone_panel/gen/waystone/helper/v1"
	"github.com/sharddevs/waystone_panel/internal/worker"
)

// File is one small file in a server's directory, as the node read it.
// Pass it back to WriteFile as the base of a read-modify-write.
type File struct {
	Exists  bool
	Content []byte
}

// Expect is what WriteFile requires of the file on disk when f is the
// base: absent if f didn't exist, otherwise exactly f's content.
func (f File) Expect() worker.Expect {
	if !f.Exists {
		return worker.Expect{
			Absent: true,
		}
	}

	hash := sha256.Sum256(f.Content)

	return worker.Expect{
		SHA256: hash,
	}
}

// ErrChanged is returned for the helper's ERROR_CODE_CHANGED: the file is
// no longer what was read. Nothing was written. Read again and decide again.
var ErrChanged = errors.New("file changed since it was read")

// ReadFile implements Runtime. It sends ACTION_READ_FILE with a FileSpec
// holding only the name, through h.Provisioner (the worker is started by
// the provisioning service, never by the runtime helper), with the same
// error mapping as Provision (send). It returns the response's FileResult.
// A response without a FileResult is an error, never a missing file.
func (h HelperRuntime) ReadFile(ctx context.Context, serverID, operationID, name string) (File, error) {
	req := &helperv1.Request{
		OperationId: operationID,
		ServerId:    serverID,
		Action:      helperv1.Action_ACTION_READ_FILE,
		File: &helperv1.FileSpec{
			Name: name,
		},
	}

	resp, err := h.send(ctx, h.Provisioner, req)
	if err != nil {
		return File{}, err
	}

	result := resp.GetFile()
	if result == nil {
		return File{}, fmt.Errorf("%w: helper returned no file result", ErrFailed)
	}

	return File{
		Exists:  result.GetExists(),
		Content: result.GetContent(),
	}, nil
}

// WriteFile implements Runtime. It sends ACTION_WRITE_FILE through
// h.Provisioner, with content and base's expectation: expect_absent if
// base doesn't exist, otherwise expect_sha256 of base's content (the
// 32 bytes of base.Expect().SHA256). The agent never validates the name or
// the size itself: the helper and the worker do, and a refusal comes back
// as ErrInvalidRequest.
func (h HelperRuntime) WriteFile(
	ctx context.Context,
	serverID, operationID, name string,
	content []byte,
	base File,
) error {
	expect := base.Expect()

	spec := &helperv1.FileSpec{
		Name:    name,
		Content: content,
	}

	if expect.Absent {
		spec.ExpectAbsent = true
	} else {
		spec.ExpectSha256 = expect.SHA256[:]
	}

	req := &helperv1.Request{
		OperationId: operationID,
		ServerId:    serverID,
		Action:      helperv1.Action_ACTION_WRITE_FILE,
		File:        spec,
	}

	_, err := h.send(ctx, h.Provisioner, req)
	return err
}
