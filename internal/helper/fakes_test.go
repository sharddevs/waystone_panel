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

// fakeRunners is both kinds of runner, for NewRouter.
type fakeRunners struct {
	fakeRunner
	fakeInputRunner
}
