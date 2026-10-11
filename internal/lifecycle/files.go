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

// File is a small file in a server's directory, as it was when we read it.
// Pass it back to WriteFile as the base of a read-modify-write.
type File struct {
	Exists  bool
	Content []byte
}

// Expect returns what the file on disk must look like for a write based on
// f to go through: absent if f didn't exist, otherwise f's exact content.
// It's the content's SHA-256 rather than a timestamp: if someone edits the
// file and then undoes it, there's nothing to lose by writing.
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

// ErrChanged means the file is no longer what was read, so nothing was
// written. The caller should read it again and decide again.
var ErrChanged = errors.New("file changed since it was read")

// ReadFile implements Runtime. File actions go to the provisioning service,
// because that is what starts the worker that actually reads the file.
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
	// No result at all is a broken answer. Reading it as "file doesn't
	// exist" could let a later write clobber a real file.
	if result == nil {
		return File{}, fmt.Errorf("%w: helper returned no file result", ErrFailed)
	}

	return File{
		Exists:  result.GetExists(),
		Content: result.GetContent(),
	}, nil
}

// WriteFile implements Runtime. It writes content only if the file still
// matches base, otherwise it returns ErrChanged. The name and size are
// checked by the helper and the worker, not here; a bad one comes back as
// ErrInvalidRequest.
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
