// SPDX-License-Identifier: AGPL-3.0-only

package agent

// Stage 6c-2, the agent's side of an install (skeletons/stage6/6c §2):
// ServerAdded, the spec checks, the jar's stream, and the steps in order,
// with a fake controller and a fake provisioner.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/sharddevs/waystone_panel/gen/waystone/agent/v1"
	helperv1 "github.com/sharddevs/waystone_panel/gen/waystone/helper/v1"
	"github.com/sharddevs/waystone_panel/internal/lifecycle"
	"github.com/sharddevs/waystone_panel/internal/profile/minecraft"
)

var jar = bytes.Repeat([]byte("jar "), 20000)

func jarArtifact() minecraft.Artifact {
	sum := sha256.Sum256(jar)
	return minecraft.Artifact{Version: "26.3", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(jar)), Runtime: "java25"}
}

func jarSum() []byte {
	sum := sha256.Sum256(jar)
	return sum[:]
}

func goodSpec() *agentv1.GetInstallSpecResponse {
	return &agentv1.GetInstallSpecResponse{Version: "26.3", ArtifactSha256: jarSum(), ArtifactSize: uint64(len(jar)),
		Port: 25600, HeapMib: 1024, Motd: "hi", MaxPlayers: 20}
}

// fakeLink is the controller's two install calls.
type fakeLink struct {
	agentv1.AgentLinkServiceClient
	spec    *agentv1.GetInstallSpecResponse
	specErr error
	chunks  [][]byte
	end     error // after the chunks; nil is a clean end
	gotReq  *agentv1.FetchArtifactRequest
	stream  *fakeChunks
	// A console command's line (7b), and the calls made, in order.
	cmdSpec *agentv1.GetCommandSpecResponse
	calls   []string
}

func (f *fakeLink) GetInstallSpec(context.Context, *agentv1.GetInstallSpecRequest, ...grpc.CallOption) (*agentv1.GetInstallSpecResponse, error) {
	return f.spec, f.specErr
}

func (f *fakeLink) FetchArtifact(_ context.Context, req *agentv1.FetchArtifactRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[agentv1.FetchArtifactResponse], error) {
	f.gotReq = req
	f.stream = &fakeChunks{chunks: slices.Clone(f.chunks), end: f.end}
	return f.stream, nil
}

type fakeChunks struct {
	grpc.ClientStream
	chunks [][]byte
	end    error
}

func (f *fakeChunks) Recv() (*agentv1.FetchArtifactResponse, error) {
	if len(f.chunks) == 0 {
		if f.end != nil {
			return nil, f.end
		}
		return nil, io.EOF
	}
	c := f.chunks[0]
	f.chunks = f.chunks[1:]
	return &agentv1.FetchArtifactResponse{Data: c}, nil
}

// split cuts b into 32 KiB chunks.
func split(b []byte) [][]byte {
	var out [][]byte
	for len(b) > 0 {
		n := min(len(b), 32<<10)
		out = append(out, b[:n])
		b = b[n:]
	}
	return out
}

// fakeProv is the helpers: no unit until PROVISION; files in memory.
type fakeProv struct {
	mu        sync.Mutex
	calls     []string
	state     helperv1.RunState
	unit      bool
	statusErr error
	installed []byte
	props     string
	eula      string
	stagedAt  []string // the staging directory's contents at INSTALL_FILE
	staging   string
}

func (f *fakeProv) log(s string) { f.mu.Lock(); f.calls = append(f.calls, s); f.mu.Unlock() }

func (f *fakeProv) Status(context.Context, string, string) (lifecycle.Status, error) {
	f.log("status")
	if f.statusErr != nil {
		return lifecycle.Status{}, f.statusErr
	}
	if !f.unit {
		return lifecycle.Status{}, lifecycle.ErrNotFound
	}
	return lifecycle.Status{State: f.state}, nil
}

func (f *fakeProv) Provision(_ context.Context, _, _ string, spec lifecycle.ProvisionSpec) (bool, error) {
	f.log("provision " + spec.Runtime)
	f.unit = true
	return true, nil
}

func (f *fakeProv) InstallFile(_ context.Context, _, _, name string, content *os.File, sum [32]byte) error {
	f.log("install " + name)
	entries, _ := os.ReadDir(f.staging)
	for _, e := range entries {
		f.stagedAt = append(f.stagedAt, e.Name())
	}
	// The descriptor is read-only.
	if _, err := content.Write([]byte("x")); err == nil {
		return errors.New("the staged jar was writable")
	}
	data, err := io.ReadAll(content)
	if err != nil {
		return err
	}
	if sha256.Sum256(data) != sum {
		return lifecycle.ErrFailed
	}
	f.installed = data
	return nil
}

func (f *fakeProv) ReadFile(_ context.Context, _, _, name string) (lifecycle.File, error) {
	return lifecycle.File{}, nil
}

func (f *fakeProv) WriteFile(_ context.Context, _, _, name string, content []byte, _ lifecycle.File) error {
	f.log("write " + name)
	if name == minecraft.EulaFile {
		f.eula = string(content)
	} else {
		f.props = string(content)
	}
	return nil
}

func installAgent(t *testing.T, p Provisioner) *Agent {
	t.Helper()
	a, err := New(Config{
		ControllerSocket: "/nonexistent", NodeID: node, Helper: &fakeHelper{}, Runner: &fakeRunner{}, Prober: fakeProber{},
		Provisioner: p, StagingDir: t.TempDir(), Artifacts: []minecraft.Artifact{jarArtifact()},
		Log: log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// runInstall runs one INSTALL and returns its result and the messages it
// sent, in order.
func runInstall(t *testing.T, a *Agent, link *fakeLink) (agentv1.ResultOutcome, agentv1.ResultCode, []string) {
	t.Helper()
	cmds := newCommands(3, []string{srv1}, nil)
	cmds.link = link
	cmd := command(3, op1, srv1, agentv1.CommandAction_COMMAND_ACTION_INSTALL)
	if known, err := cmds.accept(cmd); err != nil || !known {
		t.Fatalf("accept: %v %v", known, err)
	}
	var sent []string
	var res *agentv1.Result
	a.execute(cmd, cmds, func(m *agentv1.ConnectRequest) bool {
		switch {
		case m.GetProgress() != nil:
			sent = append(sent, m.GetProgress().GetPhase().String())
		case m.GetResult() != nil:
			res = m.GetResult()
			sent = append(sent, "result")
		}
		return true
	})
	if res == nil {
		t.Fatal("no result")
	}
	return res.GetOutcome(), res.GetCode(), sent
}

func TestInstallSteps(t *testing.T) {
	p := &fakeProv{}
	a := installAgent(t, p)
	p.staging = a.stagingDir
	link := &fakeLink{spec: goodSpec(), chunks: split(jar)}
	outcome, code, sent := runInstall(t, a, link)
	if outcome != succeeded || code != noCode {
		t.Fatalf("%v %v (calls %v)", outcome, code, p.calls)
	}
	if want := []string{"OPERATION_PHASE_FETCHING", "OPERATION_PHASE_CONFIGURING", "result"}; !slices.Equal(sent, want) {
		t.Errorf("sent %v, want %v", sent, want)
	}
	if want := []string{"status", "provision java25", "install server.jar", "write server.properties"}; !slices.Equal(p.calls, want) {
		t.Errorf("calls %v, want %v", p.calls, want)
	}
	if !bytes.Equal(p.installed, jar) {
		t.Error("installed the wrong bytes")
	}
	if len(p.stagedAt) != 1 || p.stagedAt[0] != op1+".jar" {
		t.Errorf("staged as %v", p.stagedAt)
	}
	if left, _ := os.ReadDir(a.stagingDir); len(left) != 0 {
		t.Errorf("staging not empty: %v", left)
	}
	if link.gotReq.GetOperationId() != op1 || !bytes.Equal(link.gotReq.GetSha256(), jarSum()) {
		t.Errorf("fetch request %v", link.gotReq)
	}
	for _, line := range []string{"server-port=25600\n", "motd=hi\n", "max-players=20\n", "enable-rcon=false\n"} {
		if !strings.Contains(p.props, line) {
			t.Errorf("server.properties lacks %q: %s", line, p.props)
		}
	}
}

func TestInstallRefusals(t *testing.T) {
	mismatch := agentv1.ResultCode_RESULT_CODE_ARTIFACT_MISMATCH
	unavailableArt := agentv1.ResultCode_RESULT_CODE_ARTIFACT_UNAVAILABLE
	noSpec := agentv1.ResultCode_RESULT_CODE_SPEC_UNAVAILABLE
	busy := agentv1.ResultCode_RESULT_CODE_SERVER_BUSY
	spec := func(f func(*agentv1.GetInstallSpecResponse)) *agentv1.GetInstallSpecResponse {
		s := goodSpec()
		f(s)
		return s
	}
	short := split(jar[:len(jar)-1])
	wrong := bytes.Clone(jar)
	wrong[7] ^= 1
	cases := []struct {
		name      string
		prov      *fakeProv
		link      *fakeLink
		code      agentv1.ResultCode
		wantCalls []string
	}{
		{"running", &fakeProv{unit: true, state: helperv1.RunState_RUN_STATE_RUNNING}, &fakeLink{spec: goodSpec()}, busy, []string{"status"}},
		{"starting", &fakeProv{unit: true, state: helperv1.RunState_RUN_STATE_STARTING}, &fakeLink{spec: goodSpec()}, busy, []string{"status"}},
		{"status unreachable", &fakeProv{statusErr: errors.New("io")}, &fakeLink{spec: goodSpec()}, noAnswer, []string{"status"}},
		{"no spec", &fakeProv{}, &fakeLink{specErr: status.Error(codes.PermissionDenied, "not_permitted")}, noSpec, []string{"status"}},
		{"unpinned version", &fakeProv{}, &fakeLink{spec: spec(func(s *agentv1.GetInstallSpecResponse) { s.Version = "26.2" })}, noSpec, []string{"status"}},
		{"another hash", &fakeProv{}, &fakeLink{spec: spec(func(s *agentv1.GetInstallSpecResponse) { s.ArtifactSha256 = make([]byte, 32) })}, noSpec, []string{"status"}},
		{"another size", &fakeProv{}, &fakeLink{spec: spec(func(s *agentv1.GetInstallSpecResponse) { s.ArtifactSize++ })}, noSpec, []string{"status"}},
		{"port 80", &fakeProv{}, &fakeLink{spec: spec(func(s *agentv1.GetInstallSpecResponse) { s.Port = 80 })}, noSpec, []string{"status"}},
		{"heap 64", &fakeProv{}, &fakeLink{spec: spec(func(s *agentv1.GetInstallSpecResponse) { s.HeapMib = 64 })}, noSpec, []string{"status"}},
		{"heap 64 GiB", &fakeProv{}, &fakeLink{spec: spec(func(s *agentv1.GetInstallSpecResponse) { s.HeapMib = 65536 })}, noSpec, []string{"status"}},
		{"motd newline", &fakeProv{}, &fakeLink{spec: spec(func(s *agentv1.GetInstallSpecResponse) { s.Motd = "a\nb" })}, noSpec, []string{"status"}},
		{"no max players", &fakeProv{}, &fakeLink{spec: spec(func(s *agentv1.GetInstallSpecResponse) { s.MaxPlayers = 0 })}, noSpec, []string{"status"}},
		{"short stream", &fakeProv{}, &fakeLink{spec: goodSpec(), chunks: short}, mismatch, []string{"status", "provision java25"}},
		{"long stream", &fakeProv{}, &fakeLink{spec: goodSpec(), chunks: append(split(jar), []byte("x"))}, mismatch, []string{"status", "provision java25"}},
		{"wrong bytes", &fakeProv{}, &fakeLink{spec: goodSpec(), chunks: split(wrong)}, mismatch, []string{"status", "provision java25"}},
		{"controller says mismatch", &fakeProv{}, &fakeLink{spec: goodSpec(), chunks: split(jar), end: status.Error(codes.DataLoss, "artifact_mismatch")}, mismatch, []string{"status", "provision java25"}},
		{"controller has none", &fakeProv{}, &fakeLink{spec: goodSpec(), end: status.Error(codes.NotFound, "artifact_unavailable")}, unavailableArt, []string{"status", "provision java25"}},
		{"stream cut", &fakeProv{}, &fakeLink{spec: goodSpec(), chunks: short, end: status.Error(codes.Unavailable, "gone")}, unavailableArt, []string{"status", "provision java25"}},
	}
	for _, c := range cases {
		a := installAgent(t, c.prov)
		c.prov.staging = a.stagingDir
		outcome, code, _ := runInstall(t, a, c.link)
		wantOutcome := failed
		if c.code == noAnswer {
			wantOutcome = unknown
		}
		if outcome != wantOutcome || code != c.code {
			t.Errorf("%s: %v %v, want %v %v", c.name, outcome, code, wantOutcome, c.code)
		}
		if !slices.Equal(c.prov.calls, c.wantCalls) {
			t.Errorf("%s: calls %v, want %v", c.name, c.prov.calls, c.wantCalls)
		}
		if left, _ := os.ReadDir(a.stagingDir); len(left) != 0 {
			t.Errorf("%s: staging not empty: %v", c.name, left)
		}
	}
}

// The agent stops reading once the stream is longer than the pin, so a
// controller can't fill the staging directory.
func TestInstallStopsAtThePinnedSize(t *testing.T) {
	a := installAgent(t, &fakeProv{})
	endless := split(jar)
	for range 100 {
		endless = append(endless, make([]byte, 32<<10))
	}
	link := &fakeLink{spec: goodSpec(), chunks: endless}
	if outcome, code, _ := runInstall(t, a, link); outcome != failed || code != agentv1.ResultCode_RESULT_CODE_ARTIFACT_MISMATCH {
		t.Errorf("%v %v", outcome, code)
	}
	if left := len(link.stream.chunks); left < 99 {
		t.Errorf("read on to %d chunks from the end", left)
	}
}

// A staged file left by an earlier run is never reused or overwritten.
func TestInstallStagingExclusive(t *testing.T) {
	p := &fakeProv{}
	a := installAgent(t, p)
	if err := os.WriteFile(a.stagingDir+"/"+op1+".jar", jar, 0o600); err != nil {
		t.Fatal(err)
	}
	outcome, code, _ := runInstall(t, a, &fakeLink{spec: goodSpec(), chunks: split(jar)})
	if outcome != failed || code != agentv1.ResultCode_RESULT_CODE_ARTIFACT_UNAVAILABLE {
		t.Errorf("%v %v", outcome, code)
	}
}

func TestNoProvisioner(t *testing.T) {
	a, err := New(Config{ControllerSocket: "/nonexistent", NodeID: node, Helper: &fakeHelper{}, Runner: &fakeRunner{}, Prober: fakeProber{}})
	if err != nil {
		t.Fatal(err)
	}
	if o, c := installOutcome(a.install(command(3, op1, srv1, agentv1.CommandAction_COMMAND_ACTION_INSTALL), nil, nil)); o != failed || c != unavailable {
		t.Errorf("install: %v %v", o, c)
	}
	if o, c := installOutcome(a.acceptEula(command(3, op1, srv1, agentv1.CommandAction_COMMAND_ACTION_ACCEPT_EULA))); o != failed || c != unavailable {
		t.Errorf("eula: %v %v", o, c)
	}
}

func TestAcceptEulaCommand(t *testing.T) {
	p := &fakeProv{}
	a := installAgent(t, p)
	if err := a.acceptEula(command(3, op1, srv1, agentv1.CommandAction_COMMAND_ACTION_ACCEPT_EULA)); err != nil {
		t.Fatal(err)
	}
	if p.eula != "# Accepted through Waystone, operation "+op1+"\neula=true\n" {
		t.Errorf("eula.txt %q", p.eula)
	}
}

func TestServerAdded(t *testing.T) {
	c := newCommands(3, []string{srv1}, map[string]uint32{srv1: 25565})
	added := func(epoch int64, id string, port uint32) *agentv1.ServerAdded {
		return &agentv1.ServerAdded{Epoch: epoch, ServerId: id, GamePort: port}
	}
	if err := c.add(added(3, srv2, 25566)); err != nil {
		t.Fatal(err)
	}
	if err := c.add(added(3, srv3, 0)); err != nil {
		t.Fatal(err)
	}
	ids, ports := c.servers()
	if !slices.Equal(ids, []string{srv1, srv2, srv3}) || len(ports) != 2 || ports[srv2] != 25566 {
		t.Errorf("%v %v", ids, ports)
	}
	if known, err := c.accept(command(3, op1, srv2, start)); err != nil || !known {
		t.Errorf("a command for the added server: %v %v", known, err)
	}
	for name, m := range map[string]*agentv1.ServerAdded{
		"another epoch": added(4, srv4, 0),
		"bad ID":        added(3, "x", 0),
		"bad port":      added(3, srv4, 70000),
		"twice":         added(3, srv2, 25566),
	} {
		if err := c.add(m); !errors.Is(err, errProtocol) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// No more than a hello may list.
	full := newCommands(3, nil, nil)
	for i := range maxServers {
		id := "0199e1a0-0000-7000-8000-" + strings.Repeat("0", 8) + hex.EncodeToString([]byte{byte(i >> 8), byte(i)})
		if err := full.add(added(3, id, 0)); err != nil {
			t.Fatalf("%d: %v", i, err)
		}
	}
	if err := full.add(added(3, srv5, 0)); !errors.Is(err, errProtocol) {
		t.Errorf("one more than %d: %v", maxServers, err)
	}
}

// 6c-I4: a jar a killed agent left behind is removed when the agent
// starts; anything else in the directory stays.
func TestRunClearsStaging(t *testing.T) {
	a := installAgent(t, &fakeProv{})
	stale := filepath.Join(a.stagingDir, "0199e1a0-0000-7000-8000-00000000dead.jar")
	other := filepath.Join(a.stagingDir, "keep.txt")
	for _, p := range []string{stale, other} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a.Run(ctx)
	if _, err := os.Lstat(stale); !os.IsNotExist(err) {
		t.Errorf("the stale jar is still there: %v", err)
	}
	if _, err := os.Lstat(other); err != nil {
		t.Errorf("another file was removed: %v", err)
	}
}
