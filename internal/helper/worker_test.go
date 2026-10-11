// SPDX-License-Identifier: AGPL-3.0-only

package helper

import (
	"context"
	"encoding/base64"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	helperv1 "github.com/sharddevs/waystone_panel/gen/waystone/helper/v1"
	"github.com/sharddevs/waystone_panel/internal/worker"
)

const testUser = "ws-16362f566387" // ServerUser(testServerID)

func TestValidateServerUser(t *testing.T) {
	if err := ValidateServerUser(testUser); err != nil {
		t.Errorf("ValidateServerUser(%q): %v", testUser, err)
	}
	for _, name := range []string{
		"", "root", "waystone-agent", "ws-", "ws-16362f56638", "ws-16362f5663870",
		"ws-16362F566387", "ws-16362f56638g", "xs-16362f566387", "ws-16362f566387 ",
	} {
		if err := ValidateServerUser(name); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("ValidateServerUser(%q) = %v, want ErrInvalidRequest", name, err)
		}
	}
}

// We compare the worker's whole command line, sandbox included, so any
// change to it shows up here.
func TestWorkerCommandArgv(t *testing.T) {
	got, err := WorkerCommand(testServerID, testOpID, testUser, "read-file", "server.properties")
	if err != nil {
		t.Fatalf("WorkerCommand: %v", err)
	}
	want := []string{
		"/usr/bin/systemd-run", "--wait", "--collect", "--quiet", "--pipe", "--service-type=exec",
		"--unit=waystone-op-" + testOpID + ".service",
		"-p", "User=ws-16362f566387",
		"-p", "Group=ws-16362f566387",
		"-p", "StateDirectory=waystone/servers/" + testServerID,
		"-p", "StateDirectoryMode=0750",
		"-p", "WorkingDirectory=/var/lib/waystone/servers/" + testServerID,
		"-p", "RuntimeMaxSec=50",
		"-p", "ProtectSystem=strict",
		"-p", "ProtectHome=yes",
		"-p", "PrivateTmp=yes",
		"-p", "PrivateDevices=yes",
		"-p", "PrivateNetwork=yes",
		"-p", "RestrictAddressFamilies=AF_UNIX",
		"-p", "NoNewPrivileges=yes",
		"-p", "CapabilityBoundingSet=",
		"-p", "ProtectKernelTunables=yes",
		"-p", "ProtectKernelModules=yes",
		"-p", "ProtectKernelLogs=yes",
		"-p", "ProtectControlGroups=yes",
		"-p", "ProtectClock=yes",
		"-p", "ProtectHostname=yes",
		"-p", "RestrictNamespaces=yes",
		"-p", "RestrictRealtime=yes",
		"-p", "RestrictSUIDSGID=yes",
		"-p", "LockPersonality=yes",
		"-p", "MemoryDenyWriteExecute=yes",
		"-p", "SystemCallArchitectures=native",
		"-p", "SystemCallFilter=@system-service",
		"-p", "UMask=0027",
		"--", "/usr/local/libexec/waystone-worker", "read-file", "server.properties",
	}
	if !slices.Equal(got, want) {
		t.Errorf("argv\n%q\nwant\n%q", got, want)
	}
}

// A hung worker must be stopped by systemd before the helper's own
// deadline, never left running after the agent was told it failed.
func TestWorkerStopsBeforeTheHelperGivesUp(t *testing.T) {
	if time.Duration(WorkerRuntimeMaxSec)*time.Second >= ConnTimeout {
		t.Errorf("WorkerRuntimeMaxSec %ds is not below ConnTimeout %v", WorkerRuntimeMaxSec, ConnTimeout)
	}
}

// Nothing but a ws- user, a real server ID and a real operation ID gets a
// command: the worker can never be pointed at root or at another path.
func TestWorkerCommandRefuses(t *testing.T) {
	tests := map[string][3]string{
		"root user":         {testServerID, testOpID, "root"},
		"agent user":        {testServerID, testOpID, "waystone-agent"},
		"empty user":        {testServerID, testOpID, ""},
		"bad server ID":     {"../../etc", testOpID, testUser},
		"bad operation ID":  {testServerID, "x.service", testUser},
		"uppercase op ID":   {testServerID, "0192A5C4-7D3E-7B1A-9C2F-3E4D5F6A7B8C", testUser},
		"server ID newline": {testServerID + "\n", testOpID, testUser},
	}
	for name, in := range tests {
		argv, err := WorkerCommand(in[0], in[1], in[2], "read-file", "server.properties")
		if !errors.Is(err, ErrInvalidRequest) || argv != nil {
			t.Errorf("%s: %q, %v; want no command and ErrInvalidRequest", name, argv, err)
		}
	}
}

func goodAccount() Account {
	return Account{Name: testUser, UID: 994, GID: 981, Comment: AccountComment(testServerID)}
}

func accountsWith(accounts ...Account) *fakeAccounts {
	log := []string{}
	f := &fakeAccounts{log: &log, accounts: map[string]Account{}}
	for _, a := range accounts {
		f.accounts[a.Name] = a
	}
	return f
}

func TestServerAccount(t *testing.T) {
	got, err := ServerAccount(accountsWith(goodAccount()), testServerID)
	if err != nil || got != goodAccount() {
		t.Errorf("ServerAccount = %+v, %v; want %+v", got, err, goodAccount())
	}
	if _, err := ServerAccount(accountsWith(), testServerID); !errors.Is(err, ErrNotProvisioned) {
		t.Errorf("missing account: err = %v, want ErrNotProvisioned", err)
	}
	foreign := map[string]Account{
		"other comment": {Name: testUser, UID: 994, GID: 981, Comment: "someone else"},
		"uid 0":         {Name: testUser, UID: 0, GID: 0, Comment: AccountComment(testServerID)},
		"uid 1000":      {Name: testUser, UID: 1000, GID: 1000, Comment: AccountComment(testServerID)},
	}
	for name, a := range foreign {
		if _, err := ServerAccount(accountsWith(a), testServerID); !errors.Is(err, ErrConflict) {
			t.Errorf("%s: err = %v, want ErrConflict", name, err)
		}
	}
}

func readCommand(name string) Command {
	return Command{OperationID: testOpID, ServerID: testServerID, Action: helperv1.Action_ACTION_READ_FILE, File: FileCommand{Name: name}}
}

func writeCommand(name, content string, expect worker.Expect) Command {
	return Command{
		OperationID: testOpID, ServerID: testServerID, Action: helperv1.Action_ACTION_WRITE_FILE,
		File: FileCommand{Name: name, Content: content, Expect: expect},
	}
}

func TestWorkerReadFile(t *testing.T) {
	runner := &fakeInputRunner{output: `{"exists":true,"content":"bW90ZD1oaQo="}` + "\n"}
	w := Worker{Accounts: accountsWith(goodAccount()), Runner: runner}
	got, err := w.ReadFile(context.Background(), readCommand("server.properties"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !got.Exists || string(got.Content) != "motd=hi\n" {
		t.Errorf("ReadFile = %+v", got)
	}
	want, _ := WorkerCommand(testServerID, testOpID, testUser, "read-file", "server.properties")
	if len(runner.calls) != 1 || !slices.Equal(runner.calls[0], want) {
		t.Errorf("ran %q, want %q", runner.calls, want)
	}
	if runner.stdins[0] != "" {
		t.Errorf("stdin = %q, want nothing", runner.stdins[0])
	}
}

func TestWorkerReadFileMissing(t *testing.T) {
	w := Worker{Accounts: accountsWith(goodAccount()), Runner: &fakeInputRunner{output: `{"exists":false,"content":null}`}}
	got, err := w.ReadFile(context.Background(), readCommand("eula.txt"))
	if err != nil || got.Exists || len(got.Content) != 0 {
		t.Errorf("ReadFile = %+v, %v; want a missing file and no error", got, err)
	}
}

// The worker's output is ours, so anything unexpected in it is an error,
// never something to interpret.
func TestWorkerReadFileRejectsBadOutput(t *testing.T) {
	big := base64.StdEncoding.EncodeToString(make([]byte, worker.MaxFileBytes+1))
	for name, out := range map[string]string{
		"empty":                 "",
		"not JSON":              "motd=hi",
		"unknown field":         `{"exists":true,"content":"","mode":"0777"}`,
		"two values":            `{"exists":true,"content":""}{"exists":false}`,
		"content but no file":   `{"exists":false,"content":"eA=="}`,
		"content over the size": `{"exists":true,"content":"` + big + `"}`,
	} {
		w := Worker{Accounts: accountsWith(goodAccount()), Runner: &fakeInputRunner{output: out}}
		if _, err := w.ReadFile(context.Background(), readCommand("f.txt")); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestWorkerWriteFile(t *testing.T) {
	expect := worker.ExpectContent([]byte("old"))
	runner := &fakeInputRunner{output: `{"outcome":"written"}`}
	w := Worker{Accounts: accountsWith(goodAccount()), Runner: runner}
	if err := w.WriteFile(context.Background(), writeCommand("server.properties", "motd=new\n", expect)); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	want, _ := WorkerCommand(testServerID, testOpID, testUser, "write-file", "server.properties", expect.String())
	if len(runner.calls) != 1 || !slices.Equal(runner.calls[0], want) {
		t.Fatalf("ran %q, want %q", runner.calls, want)
	}
	if runner.stdins[0] != "motd=new\n" {
		t.Errorf("stdin = %q, want the content", runner.stdins[0])
	}

	runner = &fakeInputRunner{output: `{"outcome":"written"}`}
	w.Runner = runner
	if err := w.WriteFile(context.Background(), writeCommand("eula.txt", "eula=true\n", worker.ExpectAbsent())); err != nil {
		t.Fatalf("WriteFile absent: %v", err)
	}
	if got := runner.calls[0][len(runner.calls[0])-1]; got != "absent" {
		t.Errorf("last argument = %q, want absent", got)
	}
}

func TestWorkerWriteFileOutcomes(t *testing.T) {
	cmd := writeCommand("f.txt", "x", worker.ExpectAbsent())
	w := Worker{Accounts: accountsWith(goodAccount()), Runner: &fakeInputRunner{output: `{"outcome":"changed"}`}}
	err := w.WriteFile(context.Background(), cmd)
	if !errors.Is(err, ErrChanged) || !strings.Contains(err.Error(), "f.txt") {
		t.Errorf("changed: err = %v, want ErrChanged naming the file", err)
	}
	for name, out := range map[string]string{
		"empty":           "",
		"unknown outcome": `{"outcome":"maybe"}`,
		"unknown field":   `{"outcome":"written","extra":1}`,
		"trailing junk":   `{"outcome":"written"} x`,
	} {
		w := Worker{Accounts: accountsWith(goodAccount()), Runner: &fakeInputRunner{output: out}}
		if err := w.WriteFile(context.Background(), cmd); err == nil || errors.Is(err, ErrChanged) {
			t.Errorf("%s: err = %v, want a plain error", name, err)
		}
	}
}

func TestWorkerNeverRunsWithoutItsAccount(t *testing.T) {
	cases := map[string]struct {
		accounts *fakeAccounts
		want     error
	}{
		"not provisioned": {accountsWith(), ErrNotProvisioned},
		"foreign account": {accountsWith(Account{Name: testUser, UID: 994, Comment: "x"}), ErrConflict},
	}
	for name, c := range cases {
		runner := &fakeInputRunner{output: `{"outcome":"written"}`}
		w := Worker{Accounts: c.accounts, Runner: runner}
		if _, err := w.ReadFile(context.Background(), readCommand("f.txt")); !errors.Is(err, c.want) {
			t.Errorf("%s: ReadFile err = %v, want %v", name, err, c.want)
		}
		if err := w.WriteFile(context.Background(), writeCommand("f.txt", "x", worker.ExpectAbsent())); !errors.Is(err, c.want) {
			t.Errorf("%s: WriteFile err = %v, want %v", name, err, c.want)
		}
		if len(runner.calls) != 0 {
			t.Errorf("%s: the worker ran: %q", name, runner.calls)
		}
	}
}

func TestWorkerRunErrors(t *testing.T) {
	boom := errors.New("boom")
	w := Worker{Accounts: accountsWith(goodAccount()), Runner: &fakeInputRunner{err: boom}}
	if _, err := w.ReadFile(context.Background(), readCommand("f.txt")); !errors.Is(err, boom) {
		t.Errorf("ReadFile err = %v, want the runner's error", err)
	}
	if err := w.WriteFile(context.Background(), writeCommand("f.txt", "x", worker.ExpectAbsent())); !errors.Is(err, boom) {
		t.Errorf("WriteFile err = %v, want the runner's error", err)
	}
}
