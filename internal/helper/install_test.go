// SPDX-License-Identifier: AGPL-3.0-only

package helper

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"testing"

	helperv1 "github.com/sharddevs/waystone_panel/gen/waystone/helper/v1"
	"github.com/sharddevs/waystone_panel/internal/worker"
)

// fileRunner is a fake InputRunner. It keeps the stdin it was given without
// reading it, so a test can check it got the *os.File itself.
type fileRunner struct {
	calls  [][]string
	stdins []io.Reader
	output string
	err    error
}

func (f *fileRunner) RunInput(_ context.Context, argv []string, stdin io.Reader) ([]byte, error) {
	f.calls = append(f.calls, slices.Clone(argv))
	f.stdins = append(f.stdins, stdin)
	return []byte(f.output), f.err
}

// installCommand returns a valid INSTALL_FILE command for server.jar with an
// empty temp file as its Input.
func installCommand(t *testing.T) Command {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "jar")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return Command{
		OperationID: testOpID, ServerID: testServerID, Action: helperv1.Action_ACTION_INSTALL_FILE,
		File:  FileCommand{Name: "server.jar", ContentSHA256: jarHash},
		Input: f,
	}
}

func TestWorkerInstallFile(t *testing.T) {
	runner := &fileRunner{output: `{"bytes":3}` + "\n"}
	w := Worker{Accounts: accountsWith(goodAccount()), Runner: runner}
	cmd := installCommand(t)
	if err := w.InstallFile(context.Background(), cmd); err != nil {
		t.Fatalf("InstallFile: %v", err)
	}
	want, err := WorkerCommand(testServerID, testOpID, testUser, "install-file", "server.jar", fmt.Sprintf("%x", jarHash))
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 || !slices.Equal(runner.calls[0], want) {
		t.Fatalf("ran %q, want %q", runner.calls, want)
	}
	// It has to be the same *os.File. exec.Cmd hands an *os.File straight to
	// the child; any other io.Reader would get copied through a pipe.
	if f, ok := runner.stdins[0].(*os.File); !ok || f != cmd.Input {
		t.Errorf("stdin = %T %v, want cmd.Input itself", runner.stdins[0], runner.stdins[0])
	}
}

func TestWorkerInstallFileNeedsInput(t *testing.T) {
	runner := &fileRunner{output: `{"bytes":3}`}
	w := Worker{Accounts: accountsWith(goodAccount()), Runner: runner}
	cmd := installCommand(t)
	cmd.Input = nil
	if err := w.InstallFile(context.Background(), cmd); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("no input: %v, want ErrInvalidRequest", err)
	}
	if len(runner.calls) != 0 {
		t.Errorf("worker ran without input: %q", runner.calls)
	}
}

func TestWorkerInstallFileNeverRunsWithoutItsAccount(t *testing.T) {
	for name, tt := range map[string]struct {
		accounts *fakeAccounts
		want     error
	}{
		"not provisioned": {accountsWith(), ErrNotProvisioned},
		"foreign account": {accountsWith(Account{Name: testUser, UID: 994, Comment: "x"}), ErrConflict},
	} {
		runner := &fileRunner{output: `{"bytes":3}`}
		err := Worker{Accounts: tt.accounts, Runner: runner}.InstallFile(context.Background(), installCommand(t))
		if !errors.Is(err, tt.want) {
			t.Errorf("%s: %v, want %v", name, err, tt.want)
		}
		if len(runner.calls) != 0 {
			t.Errorf("%s: worker ran: %q", name, runner.calls)
		}
	}
}

func TestWorkerInstallFileRejectsBadOutput(t *testing.T) {
	// Nothing a well-behaved worker prints: empty output, a size out of
	// range, an unknown field, two results, and write-file's result.
	for _, out := range []string{
		``,
		`{"bytes":-1}`,
		fmt.Sprintf(`{"bytes":%d}`, worker.MaxInstallBytes+1),
		`{"bytes":3,"extra":1}`,
		`{"bytes":3}{"bytes":3}`,
		`{"outcome":"written"}`,
	} {
		w := Worker{Accounts: accountsWith(goodAccount()), Runner: &fileRunner{output: out}}
		if err := w.InstallFile(context.Background(), installCommand(t)); err == nil {
			t.Errorf("output %q accepted", out)
		}
	}
}

// A worker that exits non-zero is a plain error, which the Router answers
// with FAILED.
func TestWorkerInstallFileFailure(t *testing.T) {
	runner := &fileRunner{err: errors.New("exit status 1: waystone-worker: content does not match sha256")}
	err := Worker{Accounts: accountsWith(goodAccount()), Runner: runner}.InstallFile(context.Background(), installCommand(t))
	if err == nil || errors.Is(err, ErrChanged) || errors.Is(err, ErrInvalidRequest) {
		t.Errorf("worker failure: %v, want a plain error (FAILED)", err)
	}
}
