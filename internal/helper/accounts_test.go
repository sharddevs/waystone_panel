package helper

import (
	"context"
	"errors"
	"slices"
	"testing"
)

// The whole command line, sandbox included, is part of the security review.
func TestSystemAccountsCreateArgv(t *testing.T) {
	runner := &fakeRunner{}
	a := SystemAccounts{Runner: runner}
	if err := a.Create(context.Background(), "ws-16362f566387", AccountComment(testServerID)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	want := []string{
		"/usr/bin/systemd-run", "--wait", "--collect", "--quiet", "--pipe", "--service-type=exec",
		"--unit=waystone-useradd-ws-16362f566387.service", "--setenv=LC_ALL=C",
		"-p", "ProtectSystem=strict",
		"-p", "ReadWritePaths=/etc",
		"-p", "ProtectHome=yes",
		"-p", "PrivateTmp=yes",
		"-p", "PrivateDevices=yes",
		"-p", "PrivateNetwork=yes",
		"-p", "RestrictAddressFamilies=AF_UNIX",
		"-p", "NoNewPrivileges=yes",
		"-p", "CapabilityBoundingSet=",
		"-p", "SupplementaryGroups=shadow",
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
		"--",
		"/usr/sbin/useradd", "--system", "--user-group", "--no-create-home",
		"--home-dir", "/nonexistent", "--shell", "/usr/sbin/nologin",
		"--comment", "waystone server " + testServerID,
		"--", "ws-16362f566387",
	}
	if len(runner.calls) != 1 || !slices.Equal(runner.calls[0], want) {
		t.Errorf("argv %q,\nwant %q", runner.calls, want)
	}
}

// The name ends up in a root unit's name and useradd's argv, so Create
// checks its shape itself instead of trusting its caller.
func TestSystemAccountsCreateRefusesOtherNames(t *testing.T) {
	for _, name := range []string{
		"", "root", "ws-", "ws-16362f56638", "ws-16362f5663870",
		"ws-16362F566387", "ws-16362f56638g", "xs-16362f566387",
		"ws-16362f566387 ", "-ws-16362f56638", "ws-16362f566387\n",
	} {
		runner := &fakeRunner{}
		err := SystemAccounts{Runner: runner}.Create(context.Background(), name, "c")
		if !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("Create(%q): err = %v, want ErrInvalidRequest", name, err)
		}
		if len(runner.calls) != 0 {
			t.Errorf("Create(%q) ran %q", name, runner.calls)
		}
	}
}

func TestSystemAccountsCreateError(t *testing.T) {
	boom := errors.New("boom")
	a := SystemAccounts{Runner: &fakeRunner{err: boom}}
	if err := a.Create(context.Background(), "ws-16362f566387", "c"); !errors.Is(err, boom) {
		t.Errorf("err = %v, want the runner's error", err)
	}
}
