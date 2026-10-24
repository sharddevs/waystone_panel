// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Stage 6c-1: creating a server, its install and EULA operations, and
// migration 00009's guard, CHECKs and trigger.

import (
	"context"
	"errors"
	"testing"
)

const remoteA = "0199e1a0-0000-7000-8000-0000000000a2"

func newServer(name string, port uint32) NewServer {
	return NewServer{NodeID: nodeA, Name: name, Version: "26.3", Port: port, HeapMiB: 2048, Motd: "hi", RequestID: "0199e1a0-0000-7000-8000-00000000e001"}
}

func (e *env) createServer(actor Actor, in NewServer) (Server, Operation, bool, error) {
	var s Server
	var op Operation
	var replayed bool
	err := e.store.InTenantTx(context.Background(), actor, func(ctx context.Context, tx *Tx) error {
		var err error
		s, op, replayed, err = tx.CreateServer(ctx, in)
		return err
	})
	return s, op, replayed, err
}

func TestCreateServer(t *testing.T) {
	e := setup(t)
	s, op, replayed, err := e.createServer(adminA, newServer("new", 25565))
	if err != nil || replayed {
		t.Fatalf("CreateServer: %v %v", err, replayed)
	}
	if s.NodeID != nodeA || s.Name != "new" || s.Installed || s.EulaAccepted || s.DesiredState != "stopped" ||
		s.Port == nil || *s.Port != 25565 || s.HeapMiB == nil || *s.HeapMiB != 2048 ||
		s.ProfileVersion == nil || *s.ProfileVersion != "26.3" || s.Blocking != StatePending || s.ProcessState != "unknown" {
		t.Errorf("server %+v", s)
	}
	if op.Action != ActionInstall || op.State != StatePending || op.ResourceID != s.ID || op.NodeID != nodeA || op.Actor != adminA {
		t.Errorf("operation %+v", op)
	}
	var motd string
	if err := e.owner.QueryRow(context.Background(), `SELECT config->>'motd' FROM waystone.servers WHERE server_id = $1`, s.ID).Scan(&motd); err != nil || motd != "hi" {
		t.Errorf("motd %q %v", motd, err)
	}
	if n := e.count(t, `SELECT count(*) FROM waystone.audit_events WHERE action = 'server.create' AND resource_id = $1
		AND outcome = 'recorded' AND reason = 'created' AND actor_type = 'local_user' AND actor_id = '1000'`, s.ID); n != 1 {
		t.Errorf("%d server.create audit rows, want 1", n)
	}
	// The node now has it in its hello, with its port.
	_, ids, ports, err := e.helloPorts(nodeActA)
	if err != nil || len(ids) != 3 || ports[s.ID] != 25565 {
		t.Errorf("hello: %v %v %v", ids, ports, err)
	}

	// The same port on the same node, and on another node.
	if _, _, _, err := e.createServer(adminA, newServer("dup", 25565)); !errors.Is(err, ErrPortTaken) {
		t.Errorf("duplicate port: %v", err)
	}
	other := newServer("b", 25565)
	other.NodeID = nodeB
	if _, _, _, err := e.createServer(adminB, other); err != nil {
		t.Errorf("same port on another node: %v", err)
	}
	if n := e.count(t, `SELECT count(*) FROM waystone.servers`); n != 5 {
		t.Errorf("%d servers, want 5", n)
	}
}

func TestCreateServerRefusals(t *testing.T) {
	e := setup(t)
	pin := pinOf(9)
	mustExec(t, e.owner, `INSERT INTO waystone.nodes (tenant_id, node_id, name, last_seen_at, agent_spki_sha256)
		VALUES ($1, $2, 'remote', '2100-01-01T00:00:00Z', $3)`, tenantA, remoteA, pin[:])
	bad := func(f func(*NewServer)) NewServer { in := newServer("x", 25600); f(&in); return in }
	for name, in := range map[string]NewServer{
		"bad node ID":    bad(func(n *NewServer) { n.NodeID = "x" }),
		"empty name":     bad(func(n *NewServer) { n.Name = "" }),
		"control char":   bad(func(n *NewServer) { n.Name = "a\tb" }),
		"port 1023":      bad(func(n *NewServer) { n.Port = 1023 }),
		"port 65536":     bad(func(n *NewServer) { n.Port = 65536 }),
		"heap 511":       bad(func(n *NewServer) { n.HeapMiB = 511 }),
		"heap 32769":     bad(func(n *NewServer) { n.HeapMiB = 32769 }),
		"bad version":    bad(func(n *NewServer) { n.Version = "1 2" }),
		"motd backslash": bad(func(n *NewServer) { n.Motd = `a\b` }),
		"motd too long":  bad(func(n *NewServer) { n.Motd = string(make([]byte, 65)) }),
		"no request ID":  bad(func(n *NewServer) { n.RequestID = "" }),
		"key no print":   bad(func(n *NewServer) { n.IdempotencyKey = "k" }),
	} {
		if _, _, _, err := e.createServer(adminA, in); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s: %v, want ErrInvalidInput", name, err)
		}
	}
	if _, _, _, err := e.createServer(nodeActA, newServer("x", 25600)); !errors.Is(err, ErrNotAdmin) {
		t.Errorf("node actor: %v", err)
	}
	other := newServer("x", 25600)
	other.NodeID = nodeB
	if _, _, _, err := e.createServer(adminA, other); !errors.Is(err, ErrNotFound) {
		t.Errorf("other tenant's node: %v", err)
	}
	// The controller's own agent port: refused on its node, fine elsewhere.
	reserved := newServer("x", 7443)
	reserved.ReservedPort = 7443
	if _, _, _, err := e.createServer(adminA, reserved); !errors.Is(err, ErrPortTaken) {
		t.Errorf("reserved port on the local node: %v", err)
	}
	reserved.NodeID = remoteA
	if _, _, _, err := e.createServer(adminA, reserved); err != nil {
		t.Errorf("reserved port on a remote node: %v", err)
	}
	mustExec(t, e.owner, `UPDATE waystone.nodes SET last_seen_at = '2000-01-01T00:00:00Z' WHERE node_id = $1`, nodeA)
	if _, _, _, err := e.createServer(adminA, newServer("x", 25600)); !errors.Is(err, ErrNodeUnreachable) {
		t.Errorf("unreachable: %v", err)
	}
	mustExec(t, e.owner, `UPDATE waystone.nodes SET revoked_at = now() WHERE node_id = $1`, nodeA)
	if _, _, _, err := e.createServer(adminA, newServer("x", 25600)); !errors.Is(err, ErrNodeRevoked) {
		t.Errorf("revoked: %v", err)
	}
	if n := e.count(t, `SELECT count(*) FROM waystone.servers WHERE node_id = $1`, nodeA); n != 2 {
		t.Errorf("%d servers on node a, want the 2 fixtures", n)
	}
	if n := e.count(t, `SELECT count(*) FROM waystone.operations`); n != 1 {
		t.Errorf("%d operations, want 1 (the remote's)", n)
	}
}

// A node's hello refuses more than MaxServersPerNode servers, so a create
// must never make one more.
func TestCreateServerNodeFull(t *testing.T) {
	e := setup(t)
	// The fixtures put 2 on node a; the owner adds the rest by hand.
	mustExec(t, e.owner, `INSERT INTO waystone.servers (tenant_id, server_id, node_id, name, profile, runtime)
		SELECT $1, gen_random_uuid(), $2, 'filler ' || i, 'minecraft-java', 'systemd'
		FROM generate_series(1, $3::int) AS i`, tenantA, nodeA, MaxServersPerNode-3)
	if _, _, _, err := e.createServer(adminA, newServer("last", 25600)); err != nil {
		t.Fatalf("the last one that fits: %v", err)
	}
	if _, _, _, err := e.createServer(adminA, newServer("one too many", 25601)); !errors.Is(err, ErrTooManyServers) {
		t.Errorf("one too many: %v", err)
	}
	if _, _, _, err := e.helloPorts(nodeActA); err != nil {
		t.Errorf("the full node can still say hello: %v", err)
	}
}

func TestCreateServerIdempotency(t *testing.T) {
	e := setup(t)
	in := newServer("new", 25565)
	in.IdempotencyKey, in.RequestSHA256 = "k-1", [32]byte{1}
	s, op, _, err := e.createServer(adminA, in)
	if err != nil {
		t.Fatal(err)
	}
	s2, op2, replayed, err := e.createServer(adminA, in)
	if err != nil || !replayed || s2.ID != s.ID || op2.ID != op.ID {
		t.Errorf("replay: %v %v %s %s", err, replayed, s2.ID, op2.ID)
	}
	in.RequestSHA256 = [32]byte{2}
	if _, _, _, err := e.createServer(adminA, in); !errors.Is(err, ErrIdempotencyKeyReused) {
		t.Errorf("another request, same key: %v", err)
	}
	// A key first used for a start can't replay as a create.
	start, _, err := e.create(adminA, CreateOperationInput{ServerID: serverA1, Action: ActionStart, IdempotencyKey: "k-2", RequestSHA256: [32]byte{3}})
	if err != nil {
		t.Fatal(err)
	}
	in.IdempotencyKey, in.RequestSHA256 = "k-2", [32]byte{3}
	if _, _, _, err := e.createServer(adminA, in); !errors.Is(err, ErrIdempotencyKeyReused) {
		t.Errorf("a start's key: %v (start %s)", err, start.ID)
	}
	if n := e.count(t, `SELECT count(*) FROM waystone.servers`); n != 4 {
		t.Errorf("%d servers, want 4", n)
	}
}

// The database holds the rules without the Go checks: the guard for the
// app, the CHECKs and the port index for everyone.
func TestServerInsertGuard(t *testing.T) {
	e := setup(t)
	insert := func(actor Actor, node, config string) error {
		return e.store.InTenantTx(context.Background(), actor, func(ctx context.Context, tx *Tx) error {
			_, err := tx.tx.Exec(ctx, `INSERT INTO waystone.servers (tenant_id, server_id, node_id, name, profile, runtime, config)
				VALUES ($1, '0199e1a0-0000-7000-8000-000000000a09', $2, 'x', 'minecraft-java', 'systemd', $3)`, tenantA, node, config)
			return err
		})
	}
	good := `{"version": "26.3", "port": 25600, "heap_mib": 1024, "motd": ""}`
	for name, c := range map[string]struct {
		actor        Actor
		config, node string
	}{
		"node actor":       {nodeActA, good, nodeA},
		"extra key":        {adminA, `{"version": "26.3", "port": 25600, "heap_mib": 1024, "motd": "", "jvm": "x"}`, nodeA},
		"missing key":      {adminA, `{"version": "26.3", "port": 25600, "heap_mib": 1024}`, nodeA},
		"port as a string": {adminA, `{"version": "26.3", "port": "25600", "heap_mib": 1024, "motd": ""}`, nodeA},
		"port 80":          {adminA, `{"version": "26.3", "port": 80, "heap_mib": 1024, "motd": ""}`, nodeA},
		"heap 64":          {adminA, `{"version": "26.3", "port": 25600, "heap_mib": 64, "motd": ""}`, nodeA},
		"motd newline":     {adminA, `{"version": "26.3", "port": 25600, "heap_mib": 1024, "motd": "a\nb"}`, nodeA},
		"version object":   {adminA, `{"version": {}, "port": 25600, "heap_mib": 1024, "motd": ""}`, nodeA},
		"other tenant":     {adminA, good, nodeB},
	} {
		if err := insert(c.actor, c.node, c.config); err == nil {
			t.Errorf("%s: inserted", name)
		}
	}
	// A person whose membership isn't admin: only the guard refuses,
	// since this skips CreateServer's own check.
	mustExec(t, e.owner, `INSERT INTO waystone.memberships (tenant_id, actor_type, actor_id, role) VALUES ($1, 'local_user', '5000', 'node')`, tenantA)
	if err := insert(Actor{Type: ActorLocalUser, ID: "5000", Name: "not admin"}, nodeA, good); err == nil {
		t.Error("a member who isn't an admin: inserted")
	}
	mustExec(t, e.owner, `UPDATE waystone.nodes SET revoked_at = now() WHERE node_id = $1`, nodeA)
	if err := insert(adminA, nodeA, good); err == nil {
		t.Error("revoked node: inserted")
	}
	mustExec(t, e.owner, `UPDATE waystone.nodes SET revoked_at = NULL WHERE node_id = $1`, nodeA)
	if err := insert(adminA, nodeA, good); err != nil {
		t.Fatalf("a good row: %v", err)
	}
	var installed, accepted bool
	if err := e.owner.QueryRow(context.Background(), `SELECT installed_at IS NOT NULL, eula_accepted_at IS NOT NULL
		FROM waystone.servers WHERE server_id = '0199e1a0-0000-7000-8000-000000000a09'`).Scan(&installed, &accepted); err != nil || installed || accepted {
		t.Errorf("an app row starts installed=%v accepted=%v (%v)", installed, accepted, err)
	}
	// The owner is held to the limits and the port index too (6H4).
	for name, q := range map[string]string{
		"owner port 80":    `UPDATE waystone.servers SET config = '{"port": 80}' WHERE server_id = '` + serverA1 + `'`,
		"owner port 1023":  `UPDATE waystone.servers SET config = '{"port": 1023}' WHERE server_id = '` + serverA1 + `'`,
		"owner port 65536": `UPDATE waystone.servers SET config = '{"port": 65536}' WHERE server_id = '` + serverA1 + `'`,
		"owner heap 1":     `UPDATE waystone.servers SET config = '{"heap_mib": 1}' WHERE server_id = '` + serverA1 + `'`,
		"owner same port":  `UPDATE waystone.servers SET config = '{"port": 25600}' WHERE server_id = '` + serverA1 + `'`,
		"owner an array":   `UPDATE waystone.servers SET config = '[]' WHERE server_id = '` + serverA1 + `'`,
	} {
		if _, err := e.owner.Exec(context.Background(), q); err == nil {
			t.Errorf("%s: stored", name)
		}
	}
	// The app can't mark a server installed itself.
	err := e.store.InTenantTx(context.Background(), adminA, func(ctx context.Context, tx *Tx) error {
		_, err := tx.tx.Exec(ctx, `UPDATE waystone.servers SET installed_at = now() WHERE server_id = '0199e1a0-0000-7000-8000-000000000a09'`)
		return err
	})
	if err == nil {
		t.Error("the app set installed_at")
	}
}

// Start waits for the install and the EULA; an install needs the server
// stopped; the EULA is accepted once; success, or a resolve as succeeded,
// marks the server.
func TestInstallAndEulaRules(t *testing.T) {
	e := setup(t)
	s, install, _, err := e.createServer(adminA, newServer("new", 25565))
	if err != nil {
		t.Fatal(err)
	}
	try := func(a Action) error {
		_, _, err := e.create(adminA, CreateOperationInput{ServerID: s.ID, Action: a})
		return err
	}
	if err := try(ActionStart); !errors.Is(err, ErrOperationInProgress) {
		t.Errorf("start while installing: %v", err)
	}
	e.setState(t, install.ID, StateRunning, StateFailed)
	for a, want := range map[Action]error{ActionStart: ErrNotInstalled, ActionRestart: ErrNotInstalled, ActionAcceptEula: ErrNotInstalled} {
		if err := try(a); !errors.Is(err, want) {
			t.Errorf("%s before install: %v, want %v", a, err, want)
		}
	}
	// A failed install leaves it not installed. Its state reads unknown
	// (no unit yet, so the helper says not_found), and a server that was
	// never installed may install again anyway.
	if got, _ := getServer(e, adminA, s.ID); got.ProcessState != "unknown" {
		t.Fatalf("a new server reads %s", got.ProcessState)
	}
	op, _, err := e.create(adminA, CreateOperationInput{ServerID: s.ID, Action: ActionInstall})
	if err != nil {
		t.Fatalf("reinstall after a failed install: %v", err)
	}
	if got, _ := getServer(e, adminA, s.ID); got.DesiredState != "stopped" {
		t.Errorf("an install changed desired_state to %s", got.DesiredState)
	}
	e.setState(t, op.ID, StateRunning, StateSucceeded)
	if got, _ := getServer(e, adminA, s.ID); !got.Installed || got.EulaAccepted {
		t.Fatalf("after install: installed %v accepted %v", got.Installed, got.EulaAccepted)
	}
	for _, a := range []Action{ActionStart, ActionRestart} {
		if err := try(a); !errors.Is(err, ErrEulaNotAccepted) {
			t.Errorf("%s before the EULA: %v", a, err)
		}
	}
	// Stop is always allowed.
	stop, _, err := e.create(adminA, CreateOperationInput{ServerID: s.ID, Action: ActionStop})
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	e.setState(t, stop.ID, StateRunning, StateSucceeded)

	// An EULA operation that ends uncertain and is resolved as succeeded
	// counts too.
	eula, _, err := e.create(adminA, CreateOperationInput{ServerID: s.ID, Action: ActionAcceptEula})
	if err != nil {
		t.Fatal(err)
	}
	e.setState(t, eula.ID, StateRunning, StateUncertain)
	if got, _ := getServer(e, adminA, s.ID); got.EulaAccepted {
		t.Fatal("an uncertain EULA operation counted")
	}
	err = e.store.InTenantTx(context.Background(), adminA, func(ctx context.Context, tx *Tx) error {
		_, err := tx.ResolveOperation(ctx, eula.ID, StateSucceeded, "checked eula.txt")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := getServer(e, adminA, s.ID); !got.EulaAccepted {
		t.Fatal("resolved as succeeded, but not accepted")
	}
	if err := try(ActionAcceptEula); !errors.Is(err, ErrAlreadyAccepted) {
		t.Errorf("accept twice: %v", err)
	}
	if err := try(ActionStart); err != nil {
		t.Errorf("start once installed and accepted: %v", err)
	}
	// The running server can't be reinstalled.
	e.setState(t, opIDOf(t, e, s.ID), StateRunning, StateSucceeded)
	mustExec(t, e.owner, `UPDATE waystone.servers SET process_state = 'running' WHERE server_id = $1`, s.ID)
	if err := try(ActionInstall); !errors.Is(err, ErrServerRunning) {
		t.Errorf("install while running: %v", err)
	}
	mustExec(t, e.owner, `UPDATE waystone.servers SET process_state = 'unknown' WHERE server_id = $1`, s.ID)
	if err := try(ActionInstall); !errors.Is(err, ErrServerRunning) {
		t.Errorf("install an installed server in an unknown state: %v", err)
	}
	mustExec(t, e.owner, `UPDATE waystone.servers SET process_state = 'stopped' WHERE server_id = $1`, s.ID)
	if err := try(ActionInstall); err != nil {
		t.Errorf("reinstall a stopped server: %v", err)
	}
	// A failed EULA operation marks nothing (another server).
	s2, i2, _, err := e.createServer(adminA, newServer("two", 25566))
	if err != nil {
		t.Fatal(err)
	}
	e.setState(t, i2.ID, StateRunning, StateSucceeded)
	e2, _, err := e.create(adminA, CreateOperationInput{ServerID: s2.ID, Action: ActionAcceptEula})
	if err != nil {
		t.Fatal(err)
	}
	e.setState(t, e2.ID, StateRunning, StateFailed)
	if got, _ := getServer(e, adminA, s2.ID); got.EulaAccepted {
		t.Error("a failed EULA operation counted")
	}
	// The fixtures, added by the owner, count as set up by hand.
	if got, _ := getServer(e, adminA, serverA1); !got.Installed || !got.EulaAccepted {
		t.Errorf("an owner-added server: installed %v accepted %v", got.Installed, got.EulaAccepted)
	}
}

func (e *env) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := e.owner.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func (e *env) helloPorts(actor Actor) (int64, []string, map[string]uint32, error) {
	var epoch int64
	var ids []string
	var ports map[string]uint32
	err := e.store.InTenantTx(context.Background(), actor, func(ctx context.Context, tx *Tx) error {
		var err error
		epoch, ids, ports, err = tx.NodeHello(ctx)
		return err
	})
	return epoch, ids, ports, err
}

func opIDOf(t *testing.T, e *env, serverID string) string {
	t.Helper()
	var id string
	if err := e.owner.QueryRow(context.Background(), `SELECT operation_id::text FROM waystone.operations
		WHERE resource_id = $1 AND state IN ('pending', 'running')`, serverID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// An install is claimed in phase provisioning and moves forward one step
// at a time, only by the session that claimed it.
func TestInstallPhases(t *testing.T) {
	e := setup(t)
	epoch := e.mustHello(t, nodeActA)
	s, op, _, err := e.createServer(adminA, newServer("new", 25565))
	if err != nil {
		t.Fatal(err)
	}
	c, ok, err := e.claim(nodeActA, epoch)
	if err != nil || !ok || c.OperationID != op.ID || c.Action != ActionInstall || c.ServerID != s.ID {
		t.Fatalf("claim: %+v %v %v", c, ok, err)
	}
	if r := e.op(t, op.ID); r.phase != "provisioning" {
		t.Errorf("claimed in phase %q", r.phase)
	}
	phase := func(epoch int64, p string) error {
		return e.store.InTenantTx(context.Background(), nodeActA, func(ctx context.Context, tx *Tx) error {
			return tx.InstallPhase(ctx, epoch, op.ID, p)
		})
	}
	if err := phase(epoch, "configuring"); !errors.Is(err, ErrNotClaimed) {
		t.Errorf("skip to configuring: %v", err)
	}
	if err := phase(epoch, "starting"); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("a restart phase: %v", err)
	}
	if err := phase(epoch, "fetching"); err != nil {
		t.Fatal(err)
	}
	if err := phase(epoch, "fetching"); !errors.Is(err, ErrNotClaimed) {
		t.Errorf("fetching twice: %v", err)
	}
	// The guard holds the order without the Go check: fetching back to
	// provisioning.
	err = e.store.InTenantTx(context.Background(), nodeActA, func(ctx context.Context, tx *Tx) error {
		_, err := tx.tx.Exec(ctx, `UPDATE waystone.operations SET phase = 'provisioning' WHERE operation_id = $1`, op.ID)
		return err
	})
	if err == nil {
		t.Error("the guard let fetching go back to provisioning")
	}
	if err := phase(epoch, "configuring"); err != nil {
		t.Fatal(err)
	}
	if r := e.op(t, op.ID); r.phase != "configuring" {
		t.Errorf("phase %q", r.phase)
	}
	// The guard holds the order without the Go check: back to fetching.
	err = e.store.InTenantTx(context.Background(), nodeActA, func(ctx context.Context, tx *Tx) error {
		_, err := tx.tx.Exec(ctx, `UPDATE waystone.operations SET phase = 'fetching' WHERE operation_id = $1`, op.ID)
		return err
	})
	if err == nil {
		t.Error("the guard let the phase go back")
	}
	if err := e.complete(nodeActA, epoch, op.ID, succeeded); err != nil {
		t.Fatal(err)
	}
	if got, _ := getServer(e, adminA, s.ID); !got.Installed {
		t.Error("a node's succeeded install didn't mark the server")
	}
}
