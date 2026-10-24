// SPDX-License-Identifier: AGPL-3.0-only

package helper

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
)

// InputRunner runs a program like Runner does, with stdin connected to the
// given reader (nil means no input: stdin reads as empty).
type InputRunner interface {
	RunInput(ctx context.Context, argv []string, stdin io.Reader) ([]byte, error)
}

// Runners is a runner of every kind, which is what NewRouter needs.
type Runners interface {
	Runner
	InputRunner
	CappedRunner
}

// This line only compiles if ExecRunner has every method of Runners.
var _ Runners = ExecRunner{}

// RunInput implements InputRunner. It works exactly like Run, plus stdin.
func (ExecRunner) RunInput(ctx context.Context, argv []string, stdin io.Reader) ([]byte, error) {
	return runCommand(ctx, argv, stdin)
}

// RunCapped implements CappedRunner. stderr is kept for the error, at
// most 4 KiB of it.
func (ExecRunner) RunCapped(ctx context.Context, argv []string, max int) ([]byte, bool, error) {
	if len(argv) == 0 {
		return nil, false, errors.New("empty command")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = []string{"LC_ALL=C"}
	stderr := &capBuffer{max: 4 << 10}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, false, err
	}
	if err := cmd.Start(); err != nil {
		return nil, false, err
	}
	out, readErr := io.ReadAll(io.LimitReader(stdout, int64(max)+1))
	capped := len(out) > max
	if capped {
		out = out[:max]
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	switch {
	case capped:
		return out, true, nil
	case readErr != nil:
		return nil, false, readErr
	case waitErr != nil:
		return nil, false, fmt.Errorf("command failed: %w: %s", waitErr, stderr.Bytes())
	}
	return out, false, nil
}

// capBuffer keeps the first max bytes written to it and drops the rest
// without an error, so the program never blocks on a full pipe.
type capBuffer struct {
	bytes.Buffer
	max int
}

func (b *capBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.Len(); room > 0 {
		b.Buffer.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}
