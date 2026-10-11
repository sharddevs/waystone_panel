// SPDX-License-Identifier: AGPL-3.0-only

package helper

import (
	"crypto/sha256"
	"errors"
	"fmt"

	helperv1 "github.com/sharddevs/waystone_panel/gen/waystone/helper/v1"
	"github.com/sharddevs/waystone_panel/internal/worker"
)

// FileCommand is a FileSpec that passed validation. Content is a string,
// not a []byte, so Command stays comparable and == works on it.
type FileCommand struct {
	Name    string
	Content string
	// Expect is set only for ACTION_WRITE_FILE.
	Expect worker.Expect
}

// ErrChanged means a file is no longer what the agent read. The Router
// answers it with ERROR_CODE_CHANGED.
var ErrChanged = errors.New("file changed since it was read")

// ValidateFileSpec checks the file part of a READ_FILE or WRITE_FILE
// request. Errors wrap ErrInvalidRequest. A write must carry exactly one
// expectation (the file is absent, or has this SHA-256), so there is no
// unconditional write.
func ValidateFileSpec(action helperv1.Action, spec *helperv1.FileSpec) (FileCommand, error) {
	if spec == nil {
		return FileCommand{}, fmt.Errorf("%w: missing file spec", ErrInvalidRequest)
	}

	// ValidateRequest's unknown-field check doesn't look inside nested
	// messages, so the spec needs its own.
	if len(spec.ProtoReflect().GetUnknown()) != 0 {
		return FileCommand{}, fmt.Errorf("%w: unknown file fields", ErrInvalidRequest)
	}

	if err := worker.ValidName(spec.GetName()); err != nil {
		return FileCommand{}, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}

	cmd := FileCommand{
		Name: spec.GetName(),
	}

	switch action {
	case helperv1.Action_ACTION_READ_FILE:
		// A read that carries data is a confused or forged request.
		if len(spec.GetContent()) != 0 ||
			spec.GetExpectAbsent() ||
			len(spec.GetExpectSha256()) != 0 {
			return FileCommand{}, fmt.Errorf(
				"%w: read requests cannot include content or expectations",
				ErrInvalidRequest,
			)
		}

		return cmd, nil
	case helperv1.Action_ACTION_WRITE_FILE:
		if len(spec.GetContent()) > worker.MaxFileBytes {
			return FileCommand{}, fmt.Errorf(
				"%w: file content exceeds size limit",
				ErrInvalidRequest,
			)
		}

		absent := spec.GetExpectAbsent()
		hash := spec.GetExpectSha256()

		if absent {
			if len(hash) != 0 {
				return FileCommand{}, fmt.Errorf(
					"%w: conflicting file expectations",
					ErrInvalidRequest,
				)
			}

			cmd.Expect = worker.ExpectAbsent()
		} else {
			if len(hash) != sha256.Size {
				return FileCommand{}, fmt.Errorf(
					"%w: expected SHA-256 must be 32 bytes",
					ErrInvalidRequest,
				)
			}

			copy(cmd.Expect.SHA256[:], hash)
		}

		cmd.Content = string(spec.GetContent())
		return cmd, nil
	default:
		return FileCommand{}, fmt.Errorf(
			"%w: unsupported file action",
			ErrInvalidRequest,
		)
	}
}
