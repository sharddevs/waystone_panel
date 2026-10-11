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

// Files is the small slice of the node the profile needs: read a file, and
// replace it only if it's still what was read. lifecycle.Runtime satisfies
// it, and keeping the interface this small makes it easy to fake in tests.
type Files interface {
	ReadFile(ctx context.Context, serverID, operationID, name string) (lifecycle.File, error)
	WriteFile(ctx context.Context, serverID, operationID, name string, content []byte, base lifecycle.File) error
}

// Compile-time check that lifecycle.Runtime still satisfies Files.
var _ Files = lifecycle.Runtime(nil)

// EulaContent returns the eula.txt we write for an explicit acceptance,
// with operationID in a comment. operationID must pass ids.Check, which also
// keeps it from sneaking extra lines into the file.
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

// AcceptEula writes eula.txt to record that a person accepted the Minecraft
// EULA in this operation. Only call it for an explicit acceptance. We never
// decide on our own that the EULA was accepted, even if eula.txt already
// says eula=true.
func AcceptEula(ctx context.Context, f Files, serverID, operationID string) error {
	content, err := EulaContent(operationID)
	if err != nil {
		return err
	}

	// We only read the file to get a base for the conditional write. Its
	// content is never looked at.
	base, err := f.ReadFile(ctx, serverID, operationID, EulaFile)
	if err != nil {
		// %w keeps the original error, so errors.Is still finds
		// lifecycle.ErrChanged and friends.
		return fmt.Errorf("read eula.txt: %w", err)
	}

	if err := f.WriteFile(ctx, serverID, operationID, EulaFile, content, base); err != nil {
		return fmt.Errorf("write eula.txt: %w", err)
	}

	return nil
}

// WriteProperties sets the managed keys in server.properties and keeps
// every other line. If an administrator edits the file between our read and
// our write, it fails with lifecycle.ErrChanged and writes nothing. We don't
// retry here: running the operation again reads the new file.
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
