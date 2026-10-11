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

// Runners is a runner of both kinds, which is what NewRouter needs.
type Runners interface {
	Runner
	InputRunner
}

// This line only compiles if ExecRunner has every method of Runners.
var _ Runners = ExecRunner{}

// RunInput implements InputRunner. It works exactly like Run, plus stdin.
func (ExecRunner) RunInput(ctx context.Context, argv []string, stdin io.Reader) ([]byte, error) {
	return runCommand(ctx, argv, stdin)
}
