// SPDX-License-Identifier: AGPL-3.0-only

package helper

import (
	"crypto/sha256"
	"errors"
	"fmt"

	helperv1 "github.com/sharddevs/waystone_panel/gen/waystone/helper/v1"
	"github.com/sharddevs/waystone_panel/internal/worker"
)

// FileCommand is the checked form of a FileSpec. Content is a string rather
// than a []byte so the struct stays comparable and tests can use == on it.
type FileCommand struct {
	// ContentSHA256 is the hash of the file to install (INSTALL_FILE only).
	ContentSHA256 [sha256.Size]byte
	Name          string
	Content       string
	// Expect is what the file must be right now (WRITE_FILE only).
	Expect worker.Expect
}

// ErrChanged means the file is no longer what the agent last read, so a
// write was refused. The Router reports it as ERROR_CODE_CHANGED.
var ErrChanged = errors.New("file changed since it was read")

// ValidateFileSpec checks the file part of a READ_FILE, WRITE_FILE or
// INSTALL_FILE request. Errors wrap ErrInvalidRequest. A write has to say
// what it expects to replace (no file, or a file with a given SHA-256), so
// the agent can never overwrite a file blindly.
func ValidateFileSpec(action helperv1.Action, spec *helperv1.FileSpec) (FileCommand, error) {
	if spec == nil {
		return FileCommand{}, fmt.Errorf("%w: missing file spec", ErrInvalidRequest)
	}

	// GetUnknown on the request only covers its own fields, not the ones
	// of nested messages, so the spec is checked separately.
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
		// A read has no business carrying data. If it does, something on
		// the other side is confused, so refuse it.
		if len(spec.GetContent()) != 0 ||
			spec.GetExpectAbsent() ||
			len(spec.GetExpectSha256()) != 0 ||
			len(spec.GetContentSha256()) != 0 {
			return FileCommand{}, fmt.Errorf(
				"%w: read requests cannot include content or expectations",
				ErrInvalidRequest,
			)
		}

		return cmd, nil
	case helperv1.Action_ACTION_WRITE_FILE:
		if len(spec.GetContentSha256()) != 0 {
			return FileCommand{}, fmt.Errorf(
				"%w: write requests cannot include installation hashes",
				ErrInvalidRequest,
			)
		}
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
	case helperv1.Action_ACTION_INSTALL_FILE:
		if len(spec.GetContentSha256()) != sha256.Size {
			return FileCommand{}, fmt.Errorf(
				"%w: installation SHA-256 must be 32 bytes",
				ErrInvalidRequest,
			)
		}

		if len(spec.GetContent()) != 0 ||
			spec.GetExpectAbsent() ||
			len(spec.GetExpectSha256()) != 0 {
			return FileCommand{}, fmt.Errorf(
				"%w: install requests cannot include content or expectations",
				ErrInvalidRequest,
			)
		}

		copy(cmd.ContentSHA256[:], spec.GetContentSha256())
		return cmd, nil
	default:
		return FileCommand{}, fmt.Errorf(
			"%w: unsupported file action",
			ErrInvalidRequest,
		)
	}
}
