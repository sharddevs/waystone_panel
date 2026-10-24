package helper

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	helperv1 "github.com/sharddevs/waystone_panel/gen/waystone/helper/v1"
	"google.golang.org/protobuf/encoding/protowire"
)

func TestServerUser(t *testing.T) {
	// This must never change: a different name would orphan every existing server.
	if got, want := ServerUser(testServerID), "ws-16362f566387"; got != want {
		t.Errorf("ServerUser(%s) = %q, want %q", testServerID, got, want)
	}
	other := ServerUser("3f2504e0-4f89-41d3-9a0c-0305e82c3302")
	if !regexp.MustCompile(`^ws-[0-9a-f]{12}$`).MatchString(other) {
		t.Errorf("ServerUser = %q, want ws- and 12 lowercase hex digits", other)
	}
	if other == ServerUser(testServerID) {
		t.Error("two server IDs got the same user")
	}
}

func TestAccountComment(t *testing.T) {
	if got, want := AccountComment(testServerID), "waystone server "+testServerID; got != want {
		t.Errorf("AccountComment = %q, want %q", got, want)
	}
}

func validSpec() *helperv1.ProvisionSpec {
	return &helperv1.ProvisionSpec{Runtime: "java25", HeapMib: 2048}
}

func TestValidateSpecAccepts(t *testing.T) {
	for _, heap := range []uint32{MinHeapMiB, 2048, MaxHeapMiB} {
		got, err := ValidateSpec(&helperv1.ProvisionSpec{Runtime: "java25", HeapMib: heap})
		if err != nil {
			t.Errorf("heap %d: unexpected error %v", heap, err)
			continue
		}
		if got != (Spec{Runtime: "java25", HeapMiB: heap}) {
			t.Errorf("heap %d: got %+v", heap, got)
		}
	}
}

func TestValidateSpecRejects(t *testing.T) {
	unknownField := validSpec()
	unknownField.ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, 99, protowire.VarintType), 1))

	tests := map[string]*helperv1.ProvisionSpec{
		"nil spec":         nil,
		"unknown field":    unknownField,
		"empty runtime":    {HeapMib: 2048},
		"unknown runtime":  {Runtime: "java8", HeapMib: 2048},
		"path as runtime":  {Runtime: "/usr/bin/bash", HeapMib: 2048},
		"heap zero":        {Runtime: "java25"},
		"heap too small":   {Runtime: "java25", HeapMib: MinHeapMiB - 1},
		"heap too large":   {Runtime: "java25", HeapMib: MaxHeapMiB + 1},
		"heap 4 GiB MiB":   {Runtime: "java25", HeapMib: 1 << 31},
		"runtime w/ space": {Runtime: "java25 -XX:+Evil", HeapMib: 2048},
	}
	for name, spec := range tests {
		if _, err := ValidateSpec(spec); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: err = %v, want ErrInvalidRequest", name, err)
		}
	}
}

func TestValidateRequestProvision(t *testing.T) {
	req := validRequest(helperv1.Action_ACTION_PROVISION)
	req.Provision = validSpec()
	cmd, err := ValidateRequest(req)
	if err != nil {
		t.Fatalf("valid provision request refused: %v", err)
	}
	want := Command{OperationID: testOpID, ServerID: testServerID, Action: helperv1.Action_ACTION_PROVISION, Spec: Spec{Runtime: "java25", HeapMiB: 2048}}
	if cmd != want {
		t.Errorf("got %+v, want %+v", cmd, want)
	}

	noSpec := validRequest(helperv1.Action_ACTION_PROVISION)
	badSpec := validRequest(helperv1.Action_ACTION_PROVISION)
	badSpec.Provision = &helperv1.ProvisionSpec{Runtime: "/bin/sh", HeapMib: 2048}
	startWithSpec := validRequest(helperv1.Action_ACTION_START)
	startWithSpec.Provision = validSpec()

	for name, req := range map[string]*helperv1.Request{
		"provision without spec":  noSpec,
		"provision with bad spec": badSpec,
		"start with a spec":       startWithSpec,
	} {
		if _, err := ValidateRequest(req); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: err = %v, want ErrInvalidRequest", name, err)
		}
	}
}

// Fakes for the Provisioner. They all append to one shared log, so tests
// can check the order of side effects.

type fakeAccounts struct {
	log       *[]string
	accounts  map[string]Account
	createErr error
	nextUID   uint32
}

func (f *fakeAccounts) Lookup(name string) (Account, error) {
	*f.log = append(*f.log, "lookup "+name)
	a, ok := f.accounts[name]
	if !ok {
		return Account{}, ErrNoAccount
	}
	return a, nil
}

func (f *fakeAccounts) Create(_ context.Context, name, comment string) error {
	*f.log = append(*f.log, "create "+name+" "+comment)
	if f.createErr != nil {
		return f.createErr
	}
	f.accounts[name] = Account{Name: name, UID: f.nextUID, GID: f.nextUID, Comment: comment}
	return nil
}

type fakeUnits struct {
	log      *[]string
	checkErr error
	changed  bool
	// changedOnly, when set, decides changed per file instead.
	changedOnly map[string]bool
	content     map[string][]byte
}

func (f *fakeUnits) Check(name string) error {
	*f.log = append(*f.log, "check unit "+name)
	return f.checkErr
}

func (f *fakeUnits) Write(name string, content []byte) (bool, error) {
	*f.log = append(*f.log, "write unit "+name)
	if f.content == nil {
		f.content = map[string][]byte{}
	}
	f.content[name] = content
	if f.changedOnly != nil {
		return f.changedOnly[name], nil
	}
	return f.changed, nil
}

type provisionFixture struct {
	log      []string
	accounts *fakeAccounts
	units    *fakeUnits
	runner   *fakeRunner
	java     string
	p        Provisioner
}

// newFixture returns a Provisioner whose fakes all report "nothing to do"
// and whose java25 runtime is an existing temporary file.
func newFixture(t *testing.T) *provisionFixture {
	t.Helper()
	f := &provisionFixture{}
	f.java = filepath.Join(t.TempDir(), "java")
	if err := os.WriteFile(f.java, []byte("#!/bin/false\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.accounts = &fakeAccounts{log: &f.log, accounts: map[string]Account{}, nextUID: 990}
	f.units = &fakeUnits{log: &f.log}
	f.runner = &fakeRunner{}
	f.p = Provisioner{
		Accounts: f.accounts,
		Units:    f.units,
		Runner:   f.runner,
		Runtimes: map[string]string{"java25": f.java},
	}
	return f
}

var testSpec = Spec{Runtime: "java25", HeapMiB: 2048}

func TestProvisionFromScratch(t *testing.T) {
	f := newFixture(t)
	f.units.changed = true

	changed, err := f.p.Provision(context.Background(), testServerID, testSpec)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if !changed {
		t.Error("changed = false after creating everything")
	}
	user := ServerUser(testServerID)
	socket, service := ConsoleSocketName(testServerID), UnitName(testServerID)
	wantLog := []string{
		"check unit " + socket,
		"check unit " + service,
		"lookup " + user,
		"create " + user + " " + AccountComment(testServerID),
		"lookup " + user,
		// The socket first: the service names it.
		"write unit " + socket,
		"write unit " + service,
	}
	if !slices.Equal(f.log, wantLog) {
		t.Errorf("side effects:\n got %q\nwant %q", f.log, wantLog)
	}
	wantReload := [][]string{{"/usr/bin/systemctl", "--no-ask-password", "daemon-reload"}}
	if !slices.EqualFunc(f.runner.calls, wantReload, slices.Equal) {
		t.Errorf("runner calls %q, want %q", f.runner.calls, wantReload)
	}

	unit := string(f.units.content[service])
	for _, want := range []string{
		"User=" + user + "\n",
		"StateDirectory=waystone/servers/" + testServerID + "\n",
		"ExecStart=" + f.java + " -Xms2048M -Xmx2048M -jar server.jar nogui\n",
		"MemoryMax=3072M\n",
		"Sockets=" + socket + "\n",
		"StandardInput=socket\n",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit does not contain %q", want)
		}
	}
	if want, _ := RenderConsoleSocket(testServerID); !bytes.Equal(f.units.content[socket], want) {
		t.Errorf("console socket:\n%s", f.units.content[socket])
	}
}

func TestProvisionNothingToDo(t *testing.T) {
	f := newFixture(t)
	user := ServerUser(testServerID)
	f.accounts.accounts[user] = Account{Name: user, UID: 990, GID: 990, Comment: AccountComment(testServerID)}

	changed, err := f.p.Provision(context.Background(), testServerID, testSpec)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if changed {
		t.Error("changed = true although every fake reported no change")
	}
	if len(f.runner.calls) != 0 {
		t.Errorf("daemon-reload ran although the unit did not change: %q", f.runner.calls)
	}
	if slices.ContainsFunc(f.log, func(s string) bool { return strings.HasPrefix(s, "create") }) {
		t.Errorf("created an account that already existed: %q", f.log)
	}
}

// Either file changing is a change, and needs the reload: a server made
// before 7b gets only its socket written.
func TestProvisionEitherUnitChanged(t *testing.T) {
	for _, name := range []string{ConsoleSocketName(testServerID), UnitName(testServerID)} {
		f := newFixture(t)
		user := ServerUser(testServerID)
		f.accounts.accounts[user] = Account{Name: user, UID: 990, GID: 990, Comment: AccountComment(testServerID)}
		f.units.changedOnly = map[string]bool{name: true}
		changed, err := f.p.Provision(context.Background(), testServerID, testSpec)
		if err != nil || !changed {
			t.Errorf("%s: changed %v, %v", name, changed, err)
		}
		if len(f.runner.calls) != 1 {
			t.Errorf("%s: runner calls %q, want one daemon-reload", name, f.runner.calls)
		}
	}
}

func TestProvisionRefusesForeignAccounts(t *testing.T) {
	user := ServerUser(testServerID)
	tests := map[string]Account{
		"no comment":         {Name: user, UID: 990, GID: 990},
		"another server":     {Name: user, UID: 990, GID: 990, Comment: AccountComment("3f2504e0-4f89-41d3-9a0c-0305e82c3302")},
		"root":               {Name: user, UID: 0, GID: 0, Comment: AccountComment(testServerID)},
		"human account 1000": {Name: user, UID: 1000, GID: 1000, Comment: AccountComment(testServerID)},
	}
	for name, acct := range tests {
		f := newFixture(t)
		f.accounts.accounts[user] = acct
		_, err := f.p.Provision(context.Background(), testServerID, testSpec)
		if !errors.Is(err, ErrConflict) {
			t.Errorf("%s: err = %v, want ErrConflict", name, err)
		}
		if slices.ContainsFunc(f.log, func(s string) bool { return strings.HasPrefix(s, "write") }) {
			t.Errorf("%s: went on after the conflict: %q", name, f.log)
		}
	}
}

func TestProvisionStopsBeforeChangesOnUnitConflict(t *testing.T) {
	f := newFixture(t)
	f.units.checkErr = ErrConflict
	_, err := f.p.Provision(context.Background(), testServerID, testSpec)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	if want := []string{"check unit " + ConsoleSocketName(testServerID)}; !slices.Equal(f.log, want) {
		t.Errorf("side effects %q, want only %q", f.log, want)
	}
}

func TestProvisionRuntimeProblems(t *testing.T) {
	missing := newFixture(t)
	missing.p.Runtimes = map[string]string{"java25": filepath.Join(t.TempDir(), "no-such-java")}

	notFile := newFixture(t)
	notFile.p.Runtimes = map[string]string{"java25": t.TempDir()}

	notConfigured := newFixture(t)
	notConfigured.p.Runtimes = map[string]string{}

	for name, f := range map[string]*provisionFixture{
		"java missing":           missing,
		"java is a directory":    notFile,
		"runtime not configured": notConfigured,
	} {
		_, err := f.p.Provision(context.Background(), testServerID, testSpec)
		if err == nil {
			t.Errorf("%s: Provision succeeded", name)
		}
		if errors.Is(err, ErrConflict) {
			t.Errorf("%s: a missing runtime is not a conflict: %v", name, err)
		}
		if len(f.log) != 0 {
			t.Errorf("%s: changed things before checking the runtime: %q", name, f.log)
		}
	}
}

func TestProvisionErrorsStopTheSequence(t *testing.T) {
	boom := errors.New("boom")

	createFails := newFixture(t)
	createFails.accounts.createErr = boom
	_, err := createFails.p.Provision(context.Background(), testServerID, testSpec)
	if !errors.Is(err, boom) {
		t.Errorf("create failure: err = %v, want it wrapped", err)
	}
	if slices.ContainsFunc(createFails.log, func(s string) bool { return strings.HasPrefix(s, "write") }) {
		t.Errorf("wrote a unit without an account: %q", createFails.log)
	}

	reloadFails := newFixture(t)
	reloadFails.units.changed = true
	reloadFails.runner.err = boom
	if _, err := reloadFails.p.Provision(context.Background(), testServerID, testSpec); !errors.Is(err, boom) {
		t.Errorf("daemon-reload failure: err = %v, want it wrapped", err)
	}
}
