// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package agentlink

// Stage 6c-2 (skeletons/stage6/6c §2, §4): GetInstallSpec and
// FetchArtifact answer only for a running install on the calling node
// (6M5, N16); a cached jar that isn't the pin is refused and removed (N7);
// a new server reaches the agent with ServerAdded; an install moves
// through its phases one at a time; and the real agent installs a server
// and accepts its EULA through a fake provisioner.

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/sharddevs/waystone_panel/gen/waystone/agent/v1"
	helperv1 "github.com/sharddevs/waystone_panel/gen/waystone/helper/v1"
	"github.com/sharddevs/waystone_panel/internal/agent"
	"github.com/sharddevs/waystone_panel/internal/lifecycle"
	"github.com/sharddevs/waystone_panel/internal/profile/minecraft"
	"github.com/sharddevs/waystone_panel/internal/store"
)

// testJar is a small stand-in for the pinned jar: more than three chunks.
var testJar = bytes.Repeat([]byte("waystone test jar\n"), 6000)

func testArtifact() minecraft.Artifact {
	sum := sha256.Sum256(testJar)
	s1 := sha1.Sum(testJar)
	return minecraft.Artifact{Version: "26.3", SHA1: hex.EncodeToString(s1[:]), SHA256: hex.EncodeToString(sum[:]),
		Size: int64(len(testJar)), Runtime: "java25"}
}

// setupInstall is setup with the test pin and a cache holding its jar.
func setupInstall(t *testing.T) (*env, string) {
	t.Helper()
	dir := t.TempDir()
	art := testArtifact()
	if err := os.WriteFile(filepath.Join(dir, art.SHA256), testJar, 0o600); err != nil {
		t.Fatal(err)
	}
	e := setup(t, func(c *Config) {
		c.ArtifactDir = dir
		c.Artifacts = []minecraft.Artifact{art}
	})
	return e, dir
}

// createServer makes a server on node A as admin A, as the API does, and
// returns it with its install operation.
func (e *env) createServer(t *testing.T, name string, port uint32) (string, string) {
	t.Helper()
	mustExec(t, e.owner, `UPDATE waystone.nodes SET last_seen_at = now() WHERE node_id = $1`, nodeA)
	var s store.Server
	var op store.Operation
	err := e.store.InTenantTx(context.Background(), adminA, func(ctx context.Context, tx *store.Tx) error {
		var err error
		s, op, _, err = tx.CreateServer(ctx, store.NewServer{NodeID: nodeA, Name: name, Version: "26.3",
			Port: port, HeapMiB: 1024, Motd: "hello", RequestID: "test"})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return s.ID, op.ID
}

// install creates a server while session r is live, kicks, and checks
// that the session gets ServerAdded and then the install command.
func (e *env) install(t *testing.T, r *recv, epoch int64, name string, port uint32) (string, string) {
	t.Helper()
	srv, op := e.createServer(t, name, port)
	e.srv.Kick(nodeA)
	if a := r.added(t); a.GetEpoch() != epoch || a.GetServerId() != srv || a.GetGamePort() != port {
		t.Errorf("ServerAdded %v", a)
	}
	if cmd := r.command(t); cmd.GetOperationId() != op || cmd.GetServerId() != srv ||
		cmd.GetAction() != agentv1.CommandAction_COMMAND_ACTION_INSTALL {
		t.Fatalf("command %v", cmd)
	}
	return srv, op
}

// added waits for a ServerAdded.
func (r *recv) added(t *testing.T) *agentv1.ServerAdded {
	t.Helper()
	select {
	case m := <-r.msgs:
		if m.GetServerAdded() == nil {
			t.Fatalf("got %v, want ServerAdded", m)
		}
		return m.GetServerAdded()
	case err := <-r.err:
		t.Fatalf("stream ended: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("no ServerAdded")
	}
	return nil
}

func wantNotPermitted(t *testing.T, what string, err error) {
	t.Helper()
	if st, _ := status.FromError(err); st.Code() != codes.PermissionDenied || st.Message() != MsgNotPermitted {
		t.Errorf("%s: %v, want not_permitted", what, err)
	}
}

// fetch reads a whole FetchArtifact stream.
func fetch(c agentv1.AgentLinkServiceClient, op string, sum []byte) ([]byte, error) {
	s, err := c.FetchArtifact(context.Background(), &agentv1.FetchArtifactRequest{OperationId: op, Sha256: sum})
	if err != nil {
		return nil, err
	}
	var out []byte
	for {
		m, err := s.Recv()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		if len(m.GetData()) > ArtifactChunkBytes {
			return out, errors.New("chunk too big")
		}
		out = append(out, m.GetData()...)
	}
}

// N16, 6M5: who gets a spec and the jar, and who doesn't.
func TestInstallSpecAndFetchAuthorization(t *testing.T) {
	e, _ := setupInstall(t)
	c := e.client(t)
	art := testArtifact()
	sum, _ := hex.DecodeString(art.SHA256)
	// A server made before the hello is in it, with no ServerAdded.
	early, earlyOp := e.createServer(t, "early", 25599)
	s, epoch, r := e.session(t)
	if cmd := r.command(t); cmd.GetOperationId() != earlyOp || cmd.GetServerId() != early {
		t.Fatalf("command %v", cmd)
	}
	mustSend(t, s, resultMsg(epoch, earlyOp, outFailed, agentv1.ResultCode_RESULT_CODE_SPEC_UNAVAILABLE))
	waitFor(t, "failed", func() bool { return e.op(t, earlyOp).state == "failed" })
	if got := e.op(t, earlyOp); got.code != "spec_unavailable" {
		t.Errorf("early %+v", got)
	}

	// Pending, not yet claimed: nothing.
	srv, op := e.createServer(t, "new", 25600)
	_, err := c.GetInstallSpec(context.Background(), &agentv1.GetInstallSpecRequest{ServerId: srv, OperationId: op})
	wantNotPermitted(t, "pending", err)
	_, err = fetch(c, op, sum)
	wantNotPermitted(t, "fetch while pending", err)

	// The session learns of the server first, then gets the command.
	e.srv.Kick(nodeA)
	if a := r.added(t); a.GetEpoch() != epoch || a.GetServerId() != srv || a.GetGamePort() != 25600 {
		t.Errorf("ServerAdded %v", a)
	}
	if cmd := r.command(t); cmd.GetOperationId() != op || cmd.GetAction() != agentv1.CommandAction_COMMAND_ACTION_INSTALL {
		t.Fatalf("command %v", cmd)
	}
	if got := e.op(t, op); got.state != "running" || got.phase != "provisioning" {
		t.Errorf("claimed %+v", got)
	}

	spec, err := c.GetInstallSpec(context.Background(), &agentv1.GetInstallSpecRequest{ServerId: srv, OperationId: op})
	if err != nil {
		t.Fatal(err)
	}
	if spec.GetVersion() != "26.3" || !bytes.Equal(spec.GetArtifactSha256(), sum) || spec.GetArtifactSize() != uint64(len(testJar)) ||
		spec.GetPort() != 25600 || spec.GetHeapMib() != 1024 || spec.GetMotd() != "hello" || spec.GetMaxPlayers() != minecraft.DefaultMaxPlayers {
		t.Errorf("spec %v", spec)
	}
	got, err := fetch(c, op, sum)
	if err != nil || !bytes.Equal(got, testJar) {
		t.Fatalf("fetch: %d bytes, %v", len(got), err)
	}

	for name, req := range map[string]*agentv1.GetInstallSpecRequest{
		"another server": {ServerId: serverA1, OperationId: op},
		"unknown op":     {ServerId: srv, OperationId: "0199e1a0-0000-7000-8000-00000000ffff"},
		"path as op":     {ServerId: srv, OperationId: "../x"},
		"no op":          {ServerId: srv},
	} {
		_, err := c.GetInstallSpec(context.Background(), req)
		wantNotPermitted(t, name, err)
	}
	other := sha256.Sum256([]byte("other"))
	for name, h := range map[string][]byte{
		"unknown hash": other[:],
		"short hash":   sum[:31],
		"hex text":     []byte(art.SHA256),
		"no hash":      nil,
	} {
		_, err := fetch(c, op, h)
		wantNotPermitted(t, name, err)
	}

	// A running operation that isn't an install.
	start := e.createOp(t, serverA1, store.ActionStart)
	e.srv.Kick(nodeA)
	if cmd := r.command(t); cmd.GetOperationId() != start {
		t.Fatalf("command %v", cmd)
	}
	_, err = c.GetInstallSpec(context.Background(), &agentv1.GetInstallSpecRequest{ServerId: serverA1, OperationId: start})
	wantNotPermitted(t, "a start", err)
	_, err = fetch(c, start, sum)
	wantNotPermitted(t, "fetch for a start", err)

	// Finished: nothing any more, and the server is installed.
	mustSend(t, s, resultMsg(epoch, op, outSucceeded, codeNone))
	waitFor(t, "succeeded", func() bool { return e.op(t, op).state == "succeeded" })
	_, err = c.GetInstallSpec(context.Background(), &agentv1.GetInstallSpecRequest{ServerId: srv, OperationId: op})
	wantNotPermitted(t, "finished", err)
	_, err = fetch(c, op, sum)
	wantNotPermitted(t, "fetch when finished", err)
	var installed bool
	if err := e.owner.QueryRow(context.Background(), `SELECT installed_at IS NOT NULL FROM waystone.servers WHERE server_id = $1`, srv).Scan(&installed); err != nil || !installed {
		t.Errorf("installed %v %v", installed, err)
	}
	if !strings.Contains(e.logs.String(), "agent_link=artifact_sent") {
		t.Error("no artifact_sent line")
	}
}

// N7: the cache is checked as it is read. A wrong copy of the right size
// ends the stream with artifact_mismatch and is removed; a missing file, a
// link, a FIFO or the wrong size is artifact_unavailable and stays.
func TestFetchArtifactCache(t *testing.T) {
	e, dir := setupInstall(t)
	c := e.client(t)
	art := testArtifact()
	sum, _ := hex.DecodeString(art.SHA256)
	path := filepath.Join(dir, art.SHA256)
	_, epoch, r := e.session(t)
	_, op := e.install(t, r, epoch, "new", 25600)

	wantStatus := func(name string, code codes.Code, msg string) {
		t.Helper()
		_, err := fetch(c, op, sum)
		if st, _ := status.FromError(err); st.Code() != code || st.Message() != msg {
			t.Errorf("%s: %v, want %v %s", name, err, code, msg)
		}
	}

	tampered := bytes.Clone(testJar)
	tampered[len(tampered)-1] ^= 1
	if err := os.WriteFile(path, tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	wantStatus("tampered", codes.DataLoss, MsgArtifactMismatch)
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Errorf("the tampered copy is still there: %v", err)
	}
	if !strings.Contains(e.logs.String(), "agent_link=artifact_mismatch") {
		t.Error("no artifact_mismatch line")
	}

	wantStatus("missing", codes.NotFound, MsgArtifactUnavailable)

	if err := os.WriteFile(path, testJar[:100], 0o600); err != nil {
		t.Fatal(err)
	}
	wantStatus("short", codes.NotFound, MsgArtifactUnavailable)
	os.Remove(path)

	real := filepath.Join(t.TempDir(), "jar")
	if err := os.WriteFile(real, testJar, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, path); err != nil {
		t.Fatal(err)
	}
	wantStatus("a link", codes.NotFound, MsgArtifactUnavailable)
	if _, err := os.Lstat(path); err != nil {
		t.Errorf("the link was removed: %v", err)
	}
	os.Remove(path)

	if err := mkfifo(path); err != nil {
		t.Fatal(err)
	}
	wantStatus("a FIFO", codes.NotFound, MsgArtifactUnavailable)
	os.Remove(path)

	if err := os.WriteFile(path, testJar, 0o600); err != nil {
		t.Fatal(err)
	}
	// Someone else's file, which only root can arrange.
	if os.Geteuid() == 0 {
		if err := os.Chown(path, 12345, 12345); err != nil {
			t.Fatal(err)
		}
		wantStatus("another owner", codes.NotFound, MsgArtifactUnavailable)
		if err := os.Chown(path, 0, 0); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := fetch(c, op, sum); err != nil || !bytes.Equal(got, testJar) {
		t.Errorf("good copy: %d bytes, %v", len(got), err)
	}
}

func installProgress(epoch int64, op string, phase agentv1.OperationPhase) *agentv1.ConnectRequest {
	return &agentv1.ConnectRequest{Msg: &agentv1.ConnectRequest_Progress{Progress: &agentv1.Progress{
		Epoch: epoch, OperationId: op, Phase: phase}}}
}

var (
	phaseFetching    = agentv1.OperationPhase_OPERATION_PHASE_FETCHING
	phaseConfiguring = agentv1.OperationPhase_OPERATION_PHASE_CONFIGURING
)

// An install's phases go forward one at a time; anything else ends the
// stream.
func TestInstallPhasesOverTheLink(t *testing.T) {
	e, _ := setupInstall(t)
	s, epoch, r := e.session(t)
	port := uint32(25600)
	_, op := e.install(t, r, epoch, "new", port)
	mustSend(t, s, installProgress(epoch, op, phaseFetching))
	waitFor(t, "fetching", func() bool { return e.op(t, op).phase == "fetching" })
	mustSend(t, s, installProgress(epoch, op, phaseConfiguring))
	waitFor(t, "configuring", func() bool { return e.op(t, op).phase == "configuring" })
	mustSend(t, s, installProgress(epoch, op, phaseConfiguring))
	r.closed(t, "configuring twice", codes.InvalidArgument, "invalid_progress")

	for name, phase := range map[string]agentv1.OperationPhase{
		"skip to configuring":   phaseConfiguring,
		"starting":              agentv1.OperationPhase_OPERATION_PHASE_STARTING,
		"provisioning again":    agentv1.OperationPhase_OPERATION_PHASE_PROVISIONING,
		"a number nobody knows": agentv1.OperationPhase(99),
	} {
		e.resolveInstall(t, op)
		port++
		s, epoch, r = e.session(t)
		_, op = e.install(t, r, epoch, name, port)
		mustSend(t, s, installProgress(epoch, op, phase))
		r.closed(t, name, codes.InvalidArgument, "invalid_progress")
	}
	// A FETCHING for a restart is refused too.
	restart := e.freshOp(t, serverA1, store.ActionRestart)
	s, epoch, r = e.session(t)
	for {
		if cmd := r.command(t); cmd.GetOperationId() == restart {
			break
		}
	}
	mustSend(t, s, installProgress(epoch, restart, phaseFetching))
	r.closed(t, "fetching for a restart", codes.InvalidArgument, "invalid_progress")
}

// resolveInstall finishes whatever is left of an install, as the owner.
func (e *env) resolveInstall(t *testing.T, op string) {
	t.Helper()
	mustExec(t, e.owner, `UPDATE waystone.operations SET state = 'failed', finished_at = now(), error_code = 'x', error_summary = 'x'
		WHERE operation_id = $1 AND state IN ('pending', 'running')`, op)
	mustExec(t, e.owner, `UPDATE waystone.operations SET resolved_at = now(), resolved_outcome = 'failed'
		WHERE operation_id = $1 AND state = 'uncertain' AND resolved_at IS NULL`, op)
}

// fakeProvisioner stands in for the helpers under the real agent: no
// unit until PROVISION, and files kept in memory. InstallFile reads the
// descriptor it is given and checks it against the sum, like the worker.
type fakeProvisioner struct {
	mu    sync.Mutex
	calls []string
	files map[string][]byte
	units map[string]bool
	state helperv1.RunState
}

func newFakeProvisioner() *fakeProvisioner {
	return &fakeProvisioner{files: map[string][]byte{}, units: map[string]bool{}, state: helperv1.RunState_RUN_STATE_STOPPED}
}

func (f *fakeProvisioner) log(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, s)
}

func (f *fakeProvisioner) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeProvisioner) Status(_ context.Context, serverID, _ string) (lifecycle.Status, error) {
	f.log("status")
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.units[serverID] {
		return lifecycle.Status{}, lifecycle.ErrNotFound
	}
	return lifecycle.Status{State: f.state}, nil
}

func (f *fakeProvisioner) Provision(_ context.Context, serverID, _ string, spec lifecycle.ProvisionSpec) (bool, error) {
	f.log("provision " + spec.Runtime)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.units[serverID] = true
	return true, nil
}

func (f *fakeProvisioner) InstallFile(_ context.Context, serverID, _, name string, content *os.File, sum [32]byte) error {
	data, err := io.ReadAll(content)
	if err != nil {
		return err
	}
	if sha256.Sum256(data) != sum {
		return lifecycle.ErrFailed
	}
	f.log("install " + name)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[serverID+"/"+name] = data
	return nil
}

func (f *fakeProvisioner) ReadFile(_ context.Context, serverID, _, name string) (lifecycle.File, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.files[serverID+"/"+name]
	return lifecycle.File{Exists: ok, Content: data}, nil
}

func (f *fakeProvisioner) WriteFile(_ context.Context, serverID, _, name string, content []byte, _ lifecycle.File) error {
	f.log("write " + name)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[serverID+"/"+name] = content
	return nil
}

func (f *fakeProvisioner) file(serverID, name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return string(f.files[serverID+"/"+name])
}

// D7 end to end with the real agent: create, install from the cache,
// accept the EULA, start. The staging directory is empty afterwards.
func TestInstallEndToEnd(t *testing.T) {
	e, _ := setupInstall(t)
	p := newFakeProvisioner()
	staging := t.TempDir()
	runner := &fakeRunner{}
	ctx, cancel := context.WithCancel(context.Background())
	a, err := agent.New(agent.Config{
		ControllerSocket: e.path,
		ControllerUID:    uint32(os.Getuid()),
		NodeID:           nodeA,
		Version:          "test",
		Helper:           &fakeHelper{},
		Runner:           runner,
		Prober:           fakeProber{},
		Provisioner:      p,
		StagingDir:       staging,
		Artifacts:        []minecraft.Artifact{testArtifact()},
		Log:              log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.Run(ctx)
	}()
	t.Cleanup(func() { cancel(); <-done })
	waitFor(t, "session", func() bool { return e.srv.sessions.epoch(nodeA) > 0 })

	srv, op := e.createServer(t, "new", 25600)
	e.srv.Kick(nodeA)
	waitFor(t, "installed", func() bool { return e.op(t, op).state == "succeeded" })
	if got := e.op(t, op); got.phase != "configuring" {
		t.Errorf("install ended in phase %q", got.phase)
	}
	want := []string{"status", "provision java25", "install server.jar", "write server.properties"}
	if got := p.got(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("calls %v, want %v", got, want)
	}
	if !bytes.Equal([]byte(p.file(srv, "server.jar")), testJar) {
		t.Error("server.jar isn't the jar")
	}
	props := p.file(srv, "server.properties")
	for _, line := range []string{"server-port=25600", "motd=hello", "max-players=20", "online-mode=true", "enable-rcon=false", "server-ip="} {
		if !strings.Contains(props, line+"\n") {
			t.Errorf("server.properties has no %q:\n%s", line, props)
		}
	}
	if left, _ := os.ReadDir(staging); len(left) != 0 {
		t.Errorf("staging not empty: %v", left)
	}

	// Start waits for the EULA.
	err = e.store.InTenantTx(context.Background(), adminA, func(ctx context.Context, tx *store.Tx) error {
		_, _, err := tx.CreateOperation(ctx, store.CreateOperationInput{ServerID: srv, Action: store.ActionStart})
		return err
	})
	if !errors.Is(err, store.ErrEulaNotAccepted) {
		t.Fatalf("start before the EULA: %v", err)
	}
	eula := e.createOp(t, srv, store.ActionAcceptEula)
	e.srv.Kick(nodeA)
	waitFor(t, "accepted", func() bool { return e.op(t, eula).state == "succeeded" })
	if got := p.file(srv, "eula.txt"); got != "# Accepted through Waystone, operation "+eula+"\neula=true\n" {
		t.Errorf("eula.txt %q", got)
	}
	start := e.createOp(t, srv, store.ActionStart)
	e.srv.Kick(nodeA)
	waitFor(t, "started", func() bool { return e.op(t, start).state == "succeeded" })
	if got := runner.got(); len(got) != 1 || got[0] != "start "+srv {
		t.Errorf("runner %v", got)
	}

	// A reinstall over a running server is refused by the agent too.
	p.mu.Lock()
	p.state = helperv1.RunState_RUN_STATE_RUNNING
	p.mu.Unlock()
	mustExec(t, e.owner, `UPDATE waystone.servers SET process_state = 'stopped' WHERE server_id = $1`, srv)
	again := e.createOp(t, srv, store.ActionInstall)
	e.srv.Kick(nodeA)
	waitFor(t, "refused", func() bool { return e.op(t, again).state == "failed" })
	if got := e.op(t, again); got.code != "server_busy" {
		t.Errorf("reinstall over a running server: %+v", got)
	}
}

// A wrong jar from the controller is never installed: the agent reports
// artifact_mismatch, and nothing reaches INSTALL_FILE.
func TestInstallRefusesWrongJar(t *testing.T) {
	e, dir := setupInstall(t)
	art := testArtifact()
	tampered := bytes.Clone(testJar)
	tampered[0] ^= 1
	if err := os.WriteFile(filepath.Join(dir, art.SHA256), tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	p := newFakeProvisioner()
	staging := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	a, err := agent.New(agent.Config{
		ControllerSocket: e.path, ControllerUID: uint32(os.Getuid()), NodeID: nodeA, Version: "test",
		Helper: &fakeHelper{}, Runner: &fakeRunner{}, Prober: fakeProber{}, Provisioner: p,
		StagingDir: staging, Artifacts: []minecraft.Artifact{art}, Log: log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.Run(ctx)
	}()
	t.Cleanup(func() { cancel(); <-done })
	waitFor(t, "session", func() bool { return e.srv.sessions.epoch(nodeA) > 0 })

	_, op := e.createServer(t, "new", 25600)
	e.srv.Kick(nodeA)
	waitFor(t, "failed", func() bool { return e.op(t, op).state == "failed" })
	if got := e.op(t, op); got.code != "artifact_mismatch" || got.phase != "fetching" {
		t.Errorf("op %+v", got)
	}
	for _, c := range p.got() {
		if strings.HasPrefix(c, "install") {
			t.Errorf("INSTALL_FILE was called: %v", p.got())
		}
	}
	if left, _ := os.ReadDir(staging); len(left) != 0 {
		t.Errorf("staging not empty: %v", left)
	}

	// Removed from the cache: the next try is artifact_unavailable.
	again := e.freshOp(t, mustServerOf(t, e, op), store.ActionInstall)
	e.srv.Kick(nodeA)
	waitFor(t, "failed again", func() bool { return e.op(t, again).state == "failed" })
	if got := e.op(t, again); got.code != "artifact_unavailable" {
		t.Errorf("second try %+v", got)
	}
}

func mkfifo(path string) error { return syscall.Mkfifo(path, 0o600) }

func mustServerOf(t *testing.T, e *env, op string) string {
	t.Helper()
	var id string
	if err := e.owner.QueryRow(context.Background(), `SELECT resource_id::text FROM waystone.operations WHERE operation_id = $1`, op).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
