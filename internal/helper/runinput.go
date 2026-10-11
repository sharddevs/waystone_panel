// SPDX-License-Identifier: AGPL-3.0-only

package helper

import (
	"context"
	"io"
)

// InputRunner runs a program like Runner does, with stdin connected to the
// given reader (nil means no input: stdin reads as empty).
type InputRunner interface {
	RunInput(ctx context.Context, argv []string, stdin io.Reader) ([]byte, error)
}

// Runners is what NewRouter needs: both kinds. ExecRunner is one.
type Runners interface {
	Runner
	InputRunner
}

var _ Runners = ExecRunner{}

// RunInput implements InputRunner exactly like Run (no shell, environment
// only LC_ALL=C, stderr in the error), plus cmd.Stdin = stdin. Share the
// code with Run instead of copying it.
func (ExecRunner) RunInput(ctx context.Context, argv []string, stdin io.Reader) ([]byte, error) {
	return runCommand(ctx, argv, stdin)
}
