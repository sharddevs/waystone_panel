package helper

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	helperv1 "github.com/sharddevs/waystone_panel/gen/waystone/helper/v1"
)

func startCommand() Command {
	return Command{OperationID: testOpID, ServerID: testServerID, Action: helperv1.Action_ACTION_START, NotAfter: time.UnixMilli(testNotAfter)}
}

func provisionCommand() Command {
	return Command{OperationID: testOpID, ServerID: testServerID, Action: helperv1.Action_ACTION_PROVISION, Spec: testSpec}
}

func TestRouterSendsLifecycleActionsToSystemd(t *testing.T) {
	runner := &fakeRunner{output: idleUnit}
	r := Router{Systemd: &Systemd{Runner: runner, Busy: &Busy{}}}
	resp := r.Handle(context.Background(), startCommand())
	if resp.GetErrorCode() != helperv1.ErrorCode_ERROR_CODE_UNSPECIFIED {
		t.Fatalf("START: %v %s", resp.GetErrorCode(), resp.GetErrorMessage())
	}
	// Since stage 4c the job check (S3) comes first, then the start.
	want := []string{"/usr/bin/systemctl", "--no-ask-password", "start", "--", testUnit}
	if len(runner.calls) != 2 || !slices.Equal(runner.calls[0], jobCheckArgv) || !slices.Equal(runner.calls[1], want) {
		t.Errorf("runner calls %q, want %q then %q", runner.calls, jobCheckArgv, want)
	}
}

// The runtime helper keeps its narrow sandbox, so it must never provision,
// and the provisioning service must never start or stop anything.
func TestRouterRefusesActionsItDoesNotServe(t *testing.T) {
	runtimeOnly := Router{Systemd: &Systemd{Runner: &fakeRunner{}}}
	resp := runtimeOnly.Handle(context.Background(), provisionCommand())
	if resp.GetErrorCode() != helperv1.ErrorCode_ERROR_CODE_INVALID_REQUEST || resp.GetErrorMessage() == "" {
		t.Errorf("runtime helper on PROVISION: %v %q, want INVALID_REQUEST with a message", resp.GetErrorCode(), resp.GetErrorMessage())
	}

	f := newFixture(t)
	provisionOnly := Router{Provisioner: &f.p}
	for _, action := range []helperv1.Action{helperv1.Action_ACTION_START, helperv1.Action_ACTION_STOP, helperv1.Action_ACTION_STATUS} {
		cmd := startCommand()
		cmd.Action = action
		resp := provisionOnly.Handle(context.Background(), cmd)
		if resp.GetErrorCode() != helperv1.ErrorCode_ERROR_CODE_INVALID_REQUEST || resp.GetErrorMessage() == "" {
			t.Errorf("provisioner on %v: %v %q, want INVALID_REQUEST with a message", action, resp.GetErrorCode(), resp.GetErrorMessage())
		}
	}
	if len(f.log) != 0 || len(f.runner.calls) != 0 {
		t.Errorf("the provisioner did something for a lifecycle action: %q %q", f.log, f.runner.calls)
	}
}

func TestRouterProvisions(t *testing.T) {
	f := newFixture(t)
	f.units.changed = true
	resp := Router{Provisioner: &f.p}.Handle(context.Background(), provisionCommand())
	if resp.GetErrorCode() != helperv1.ErrorCode_ERROR_CODE_UNSPECIFIED {
		t.Fatalf("PROVISION: %v %s", resp.GetErrorCode(), resp.GetErrorMessage())
	}
	if !resp.GetChanged() {
		t.Error("changed = false, want true")
	}
}

func TestRouterProvisionErrors(t *testing.T) {
	conflict := newFixture(t)
	conflict.units.checkErr = ErrConflict

	invalid := newFixture(t)
	invalid.units.checkErr = ErrInvalidRequest

	failed := newFixture(t)
	failed.accounts.createErr = errors.New("useradd: cannot lock /etc/passwd")

	tests := map[string]struct {
		f    *provisionFixture
		want helperv1.ErrorCode
	}{
		"conflict": {conflict, helperv1.ErrorCode_ERROR_CODE_CONFLICT},
		"invalid":  {invalid, helperv1.ErrorCode_ERROR_CODE_INVALID_REQUEST},
		"failed":   {failed, helperv1.ErrorCode_ERROR_CODE_FAILED},
	}
	for name, tt := range tests {
		resp := Router{Provisioner: &tt.f.p}.Handle(context.Background(), provisionCommand())
		if resp.GetErrorCode() != tt.want {
			t.Errorf("%s: code %v, want %v", name, resp.GetErrorCode(), tt.want)
		}
		if resp.GetErrorMessage() == "" {
			t.Errorf("%s: empty error message", name)
		}
	}
}

func TestNewRouterRuntime(t *testing.T) {
	runners := &fakeRunners{}
	r, err := NewRouter(RoleRuntime, runners)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	if r.Provisioner != nil || r.Worker != nil {
		t.Error("the runtime role must not provision or run the worker")
	}
	if r.Systemd == nil || r.Systemd.Runner != Runner(runners) {
		t.Errorf("Systemd = %+v, want one using the given runner", r.Systemd)
	}
}

func TestNewRouterProvision(t *testing.T) {
	runners := &fakeRunners{}
	r, err := NewRouter(RoleProvision, runners)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	if r.Systemd != nil {
		t.Error("the provisioning role must not start, stop or inspect servers")
	}
	p := r.Provisioner
	if p == nil {
		t.Fatal("Provisioner is nil")
	}
	if p.Accounts != Accounts(SystemAccounts{Runner: runners}) {
		t.Errorf("Accounts = %#v", p.Accounts)
	}
	if p.Units != Units(UnitFiles{Dir: DefaultUnitDir}) {
		t.Errorf("Units = %#v", p.Units)
	}
	if p.Runner != Runner(runners) {
		t.Errorf("Runner = %#v", p.Runner)
	}
	if len(p.Runtimes) != len(DefaultRuntimes) || p.Runtimes["java25"] != DefaultRuntimes["java25"] {
		t.Errorf("Runtimes = %v, want DefaultRuntimes", p.Runtimes)
	}
	want := Worker{Accounts: SystemAccounts{Runner: runners}, Runner: runners}
	if r.Worker == nil || *r.Worker != want {
		t.Errorf("Worker = %#v, want %#v", r.Worker, want)
	}
}

func TestNewRouterUnknownRole(t *testing.T) {
	for _, role := range []string{"", "Runtime", "provisioner", "both"} {
		if _, err := NewRouter(role, &fakeRunners{}); err == nil {
			t.Errorf("NewRouter(%q): no error", role)
		}
	}
}
