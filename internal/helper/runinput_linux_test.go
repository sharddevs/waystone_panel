//go:build linux

// SPDX-License-Identifier: AGPL-3.0-only

package helper

import (
	"context"
	"strings"
	"testing"
)

func TestExecRunnerRunInput(t *testing.T) {
	out, err := ExecRunner{}.RunInput(context.Background(), []string{"/bin/cat"}, strings.NewReader("hello\n"))
	if err != nil || string(out) != "hello\n" {
		t.Errorf("cat with stdin = %q, %v; want %q", out, err, "hello\n")
	}

	out, err = ExecRunner{}.RunInput(context.Background(), []string{"/bin/cat"}, nil)
	if err != nil || len(out) != 0 {
		t.Errorf("cat without stdin = %q, %v; want nothing", out, err)
	}
}

func TestExecRunnerRunInputEnvironment(t *testing.T) {
	t.Setenv("WAYSTONE_LEAK", "secret")
	out, err := ExecRunner{}.RunInput(context.Background(), []string{"/usr/bin/env"}, nil)
	if err != nil {
		t.Fatalf("env: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "LC_ALL=C" {
		t.Errorf("environment = %q, want only LC_ALL=C", got)
	}
}

func TestExecRunnerRunInputStderr(t *testing.T) {
	_, err := ExecRunner{}.RunInput(context.Background(), []string{"/bin/sh", "-c", "echo why >&2; exit 3"}, nil)
	if err == nil || !strings.Contains(err.Error(), "why") {
		t.Errorf("err = %v, want the program's stderr in it", err)
	}
}
