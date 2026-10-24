// SPDX-License-Identifier: AGPL-3.0-only

package helper

import (
	"context"
	"io"
	"slices"
)

// fakeInputRunner records RunInput calls and what arrived on stdin.
type fakeInputRunner struct {
	calls  [][]string
	stdins []string
	output string
	err    error
}

func (f *fakeInputRunner) RunInput(_ context.Context, argv []string, stdin io.Reader) ([]byte, error) {
	f.calls = append(f.calls, slices.Clone(argv))
	in := ""
	if stdin != nil {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return nil, err
		}
		in = string(b)
	}
	f.stdins = append(f.stdins, in)
	return []byte(f.output), f.err
}

// fakeCappedRunner records RunCapped calls and returns output, capped
// at max like the real one.
type fakeCappedRunner struct {
	calls  [][]string
	maxes  []int
	output string
	err    error
}

func (f *fakeCappedRunner) RunCapped(_ context.Context, argv []string, max int) ([]byte, bool, error) {
	f.calls = append(f.calls, slices.Clone(argv))
	f.maxes = append(f.maxes, max)
	if f.err != nil {
		return nil, false, f.err
	}
	if len(f.output) > max {
		return []byte(f.output[:max]), true, nil
	}
	return []byte(f.output), false, nil
}

// fakeRunners embeds every fake, so it gets every method and can be
// passed to NewRouter as any kind of runner.
type fakeRunners struct {
	fakeRunner
	fakeInputRunner
	fakeCappedRunner
}
