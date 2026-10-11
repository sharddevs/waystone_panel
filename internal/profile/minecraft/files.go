// SPDX-License-Identifier: AGPL-3.0-only

package minecraft

import (
	"context"
	"fmt"

	"github.com/sharddevs/waystone_panel/internal/ids"
	"github.com/sharddevs/waystone_panel/internal/lifecycle"
)

// The files this profile manages in a server's directory.
const (
	EulaFile       = "eula.txt"
	PropertiesFile = "server.properties"
)

// Files is what the profile needs from the node: read a small file, and
// replace it if it is still what was read. lifecycle.Runtime is one; tests
// use a fake.
type Files interface {
	ReadFile(ctx context.Context, serverID, operationID, name string) (lifecycle.File, error)
	WriteFile(ctx context.Context, serverID, operationID, name string, content []byte, base lifecycle.File) error
}

var _ Files = lifecycle.Runtime(nil)

// EulaContent is the eula.txt that records an explicit acceptance, exactly:
//
//	"# Accepted through Waystone, operation <operationID>\neula=true\n"
//
// operationID must pass ids.Check; otherwise an error wrapping ErrInvalid.
func EulaContent(operationID string) ([]byte, error) {
	if err := ids.Check(operationID); err != nil {
		return nil, fmt.Errorf("%w: invalid operation ID: %v", ErrInvalid, err)
	}

	content := fmt.Sprintf(
		"# Accepted through Waystone, operation %s\neula=true\n",
		operationID,
	)

	return []byte(content), nil
}

// AcceptEula records that a person accepted the Minecraft EULA in this
// operation. Call it only for an explicit acceptance; nothing may call it on
// its own, and nothing infers acceptance from an existing eula.txt.
//  1. EulaContent(operationID). On error, return it before touching
//     anything.
//  2. ReadFile eula.txt, only to get the base for the write. Its content is
//     never looked at: "eula=true" already being there is not an
//     acceptance.
//  3. WriteFile eula.txt with the content and that base.
//
// Wrap errors from f with %w and say which step failed, so
// lifecycle.ErrChanged and the others stay detectable.
func AcceptEula(ctx context.Context, f Files, serverID, operationID string) error {
	content, err := EulaContent(operationID)
	if err != nil {
		return err
	}

	base, err := f.ReadFile(ctx, serverID, operationID, EulaFile)
	if err != nil {
		return fmt.Errorf("read eula.txt: %w", err)
	}

	if err := f.WriteFile(ctx, serverID, operationID, EulaFile, content, base); err != nil {
		return fmt.Errorf("write eula.txt: %w", err)
	}

	return nil
}

// WriteProperties sets the managed keys in server.properties and keeps
// every other line.
//  1. p.Validate(). On error, return it before touching anything.
//  2. ReadFile server.properties.
//  3. MergeProperties with its content (nil if it doesn't exist).
//  4. WriteFile the result with what was read as the base. If an
//     administrator changed the file in between, this fails with
//     lifecycle.ErrChanged and nothing is written. Don't retry here: the
//     operation fails, and running it again re-reads the new file.
//
// Wrap errors with %w as in AcceptEula.
func WriteProperties(
	ctx context.Context,
	f Files,
	serverID, operationID string,
	p Properties,
) error {
	if err := p.Validate(); err != nil {
		return err
	}

	base, err := f.ReadFile(ctx, serverID, operationID, PropertiesFile)
	if err != nil {
		return fmt.Errorf("read server.properties: %w", err)
	}

	var existing []byte
	if base.Exists {
		existing = base.Content
	}

	content, err := MergeProperties(existing, p)
	if err != nil {
		return fmt.Errorf("merge server.properties: %w", err)
	}

	if err := f.WriteFile(ctx, serverID, operationID, PropertiesFile, content, base); err != nil {
		return fmt.Errorf("write server.properties: %w", err)
	}

	return nil
}
