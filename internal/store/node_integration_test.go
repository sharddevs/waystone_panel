// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Stage 4b-1 tests (notes/stage4b-scope.md v2.1): the hello, reports, the
// two database guards and the migration itself, all on real PostgreSQL.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pressly/goose/v3"
)

// A second node in tenant A, with its own server and membership, so a
// node can try to touch another node's rows inside the same tenant.
const (
	nodeA2      = "0199e1a0-0000-7000-8000-0000000000a2"
	serverOnA2  = "0199e1a0-0000-7000-8000-000000000a21"
	agentTimeOK = "2026-10-13T12:00:00Z"
)

var nodeActA2 = Actor{Type: ActorNode, ID: nodeA2, Name: "node a2"}

// addNodeA2 creates nodeA2, its membership and one server on it.
func addNodeA2(t *testing.T, e *env) {
	t.Helper()
	mustExec(t, e.owner, `INSERT INTO waystone.nodes (tenant_id, node_id, name, last_seen_at) VALUES ($1, $2, 'node a2', '2100-01-01T00:00:00Z')`, tenantA, nodeA2)
	mustExec(t, e.owner, `INSERT INTO waystone.memberships (tenant_id, actor_type, actor_id, role) VALUES ($1, 'node', $2, 'node')`, tenantA, nodeA2)
	mustExec(t, e.owner, `INSERT INTO waystone.servers (tenant_id, server_id, node_id, name, profile, runtime)
		VALUES ($1, $2, $3, 'on a2', 'minecraft-java', 'systemd')`, tenantA, serverOnA2, nodeA2)
}

func agentTime(t *testing.T) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, agentTimeOK)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

func (e *env) hello(actor Actor) (int64, []string, error) {
	var epoch int64
	var ids []string
	err := e.store.InTenantTx(context.Background(), actor, func(ctx context.Context, tx *Tx) error {
		var err error
		epoch, ids, _, err = tx.NodeHello(ctx)
		return err
	})
	return epoch, ids, err
}

func (e *env) report(actor Actor, r Report) (int, int, error) {
	var acc, rej int
	err := e.store.InTenantTx(context.Background(), actor, func(ctx context.Context, tx *Tx) error {
		var err error
		acc, rej, err = tx.AcceptReport(ctx, r)
		return err
	})
	return acc, rej, err
}

type nodeRow struct {
	epoch    int64
	lastSeen *time.Time
}

func (e *env) node(t *testing.T, id string) nodeRow {
	t.Helper()
	var r nodeRow
	if err := e.owner.QueryRow(context.Background(),
		`SELECT session_epoch, last_seen_at FROM waystone.nodes WHERE node_id = $1`, id).Scan(&r.epoch, &r.lastSeen); err != nil {
		t.Fatal(err)
	}
	return r
}

type serverRow struct {
	state           string
	since, observed *time.Time
	epoch, seq      int64
	agentReportedAt *time.Time
}

func (e *env) server(t *testing.T, id string) serverRow {
	t.Helper()
	var r serverRow
	if err := e.owner.QueryRow(context.Background(), `
		SELECT process_state, process_since, process_observed_at, report_epoch, report_seq, agent_reported_at
		FROM waystone.servers WHERE server_id = $1`, id).
		Scan(&r.state, &r.since, &r.observed, &r.epoch, &r.seq, &r.agentReportedAt); err != nil {
		t.Fatal(err)
	}
	return r
}

func (e *env) opState(t *testing.T, id string) (state, code string) {
	t.Helper()
	var c *string
	if err := e.owner.QueryRow(context.Background(),
		`SELECT state, error_code FROM waystone.operations WHERE operation_id = $1`, id).Scan(&state, &c); err != nil {
		t.Fatal(err)
	}
	if c != nil {
		code = *c
	}
	return state, code
}

// runningOp creates an operation on server and moves it to running, as
// the 4c claim would.
func (e *env) runningOp(t *testing.T, server string) string {
	t.Helper()
	op, _, err := e.create(adminA, CreateOperationInput{ServerID: server, Action: ActionStart})
	if err != nil {
		t.Fatal(err)
	}
	e.setState(t, op.ID, StateRunning)
	return op.ID
}

// ------------------------------------------------------------- migration

// Migrating a database that already has 00001 and data applies 00002
// without touching the data, adds exactly the grants and triggers the
// scope lists, and a second run applies nothing.
func TestMigration00002(t *testing.T) {
	e := setup(t)
	ctx := context.Background()

	// Rebuild at version 1 only, with the seed data in it.
	mustExec(t, e.owner, `DROP SCHEMA waystone CASCADE`)
	mustExec(t, e.owner, `DROP SCHEMA waystone_meta CASCADE`)
	mustExec(t, e.owner, `CREATE SCHEMA waystone`)
	mustExec(t, e.owner, `CREATE SCHEMA waystone_meta`)
	db, err := goose.OpenDBWithDriver("pgx", ownerDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	p, err := goose.NewProvider(goose.DialectPostgres, db, Migrations(),
		goose.WithTableName(VersionTable), goose.WithDisableGlobalRegistry(true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.UpTo(ctx, 1); err != nil {
		t.Fatal(err)
	}
	mustExec(t, e.owner, `INSERT INTO waystone.tenants (tenant_id, name) VALUES ($1, 'A')`, tenantA)
	mustExec(t, e.owner, `INSERT INTO waystone.nodes (tenant_id, node_id, name, session_epoch) VALUES ($1, $2, 'node a', 7)`, tenantA, nodeA)
	mustExec(t, e.owner, `INSERT INTO waystone.servers (tenant_id, server_id, node_id, name, profile, runtime, process_state)
		VALUES ($1, $2, $3, 'a1', 'minecraft-java', 'systemd', 'running')`, tenantA, serverA1, nodeA)

	if err := Migrate(ctx, ownerDSN()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if n := e.node(t, nodeA); n.epoch != 7 || n.lastSeen != nil {
		t.Errorf("node after migrate = %+v, want epoch 7 and no last_seen_at", n)
	}
	if s := e.server(t, serverA1); s.state != "running" || s.epoch != 0 || s.seq != 0 {
		t.Errorf("server after migrate = %+v", s)
	}

	// Exactly these UPDATE column grants for waystone_app on the two tables.
	rows, err := e.owner.Query(ctx, `
		SELECT table_name || '.' || column_name FROM information_schema.column_privileges
		WHERE grantee = 'waystone_app' AND privilege_type = 'UPDATE'
		  AND table_schema = 'waystone' AND table_name IN ('nodes', 'servers')
		ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"nodes.agent_version", "nodes.capabilities", // 00008
		"nodes.last_seen_at", "nodes.session_epoch",
		"servers.agent_reported_at", "servers.desired_state", "servers.process_observed_at",
		"servers.process_since", "servers.process_state",
		"servers.readiness", "servers.readiness_observed_at", "servers.readiness_since", // 00005
		"servers.report_epoch", "servers.report_seq",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("UPDATE grants = %v, want %v", got, want)
	}

	var triggers []string
	rows, err = e.owner.Query(ctx, `
		SELECT tgname FROM pg_trigger
		WHERE tgrelid IN ('waystone.nodes'::regclass, 'waystone.servers'::regclass) AND NOT tgisinternal
		ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	if triggers, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		t.Fatal(err)
	}
	// 00009 adds the insert guard (6c-1).
	if strings.Join(triggers, ",") != "nodes_guard,servers_insert_guard,servers_report_guard" {
		t.Errorf("triggers = %v", triggers)
	}

	// A second run finds nothing to do.
	var before, after int
	if err := e.owner.QueryRow(ctx, `SELECT count(*) FROM `+VersionTable).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, ownerDSN()); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if err := e.owner.QueryRow(ctx, `SELECT count(*) FROM `+VersionTable).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Errorf("second migrate added version rows: %d -> %d", before, after)
	}
}

// ----------------------------------------------------------------- hello

// I8: each hello raises the epoch by exactly one, returns the node's
// servers, sets last_seen_at, and turns the node's running operations
// uncertain. Pending operations and other nodes are left alone.
func TestNodeHello(t *testing.T) {
	e := setup(t)
	addNodeA2(t, e)
	running := e.runningOp(t, serverA1)
	pending, _, err := e.create(adminA, CreateOperationInput{ServerID: serverA2, Action: ActionStart})
	if err != nil {
		t.Fatal(err)
	}
	otherNode := e.runningOp(t, serverOnA2)

	epoch, ids, err := e.hello(nodeActA)
	if err != nil {
		t.Fatal(err)
	}
	if epoch != 1 || strings.Join(ids, ",") != serverA1+","+serverA2 {
		t.Errorf("hello = %d %v, want 1 [a1 a2]", epoch, ids)
	}
	if n := e.node(t, nodeA); n.epoch != 1 || n.lastSeen == nil {
		t.Errorf("node = %+v", n)
	}
	if s, c := e.opState(t, running); s != "uncertain" || c != "session_replaced" {
		t.Errorf("running op = %s %s, want uncertain session_replaced", s, c)
	}
	if s, _ := e.opState(t, pending.ID); s != "pending" {
		t.Errorf("pending op = %s", s)
	}
	if s, _ := e.opState(t, otherNode); s != "running" {
		t.Errorf("other node's op = %s, want running", s)
	}

	if epoch, _, err = e.hello(nodeActA); err != nil || epoch != 2 {
		t.Errorf("second hello = %d, %v; want 2", epoch, err)
	}

	// Only a node may say hello.
	if _, _, err := e.hello(adminA); !errors.Is(err, ErrNotNode) {
		t.Errorf("hello as admin: %v, want ErrNotNode", err)
	}
	if n := e.node(t, nodeA); n.epoch != 2 {
		t.Errorf("epoch after refused hello = %d", n.epoch)
	}
	// A node actor whose membership says admin (a bad seed) is no node
	// either: both the actor type and the role must say so.
	mustExec(t, e.owner, `UPDATE waystone.memberships SET role = 'admin' WHERE actor_type = 'node' AND actor_id = $1`, nodeA)
	if _, _, err := e.hello(nodeActA); !errors.Is(err, ErrNotNode) {
		t.Errorf("hello as node with admin role: %v, want ErrNotNode", err)
	}
	if _, _, err := e.report(nodeActA, Report{Epoch: 2, Seq: 1, AgentTime: agentTime(t)}); !errors.Is(err, ErrNotNode) {
		t.Errorf("report as node with admin role: %v, want ErrNotNode", err)
	}
	if n := e.node(t, nodeA); n.epoch != 2 {
		t.Errorf("epoch after admin-role hello = %d", n.epoch)
	}
}

// R7, v2.1 point 3: 256 servers are fine; 257 refuse the hello and change
// nothing: not the epoch, not last_seen_at, not a running operation.
func TestNodeHelloTooManyServers(t *testing.T) {
	e := setup(t)
	running := e.runningOp(t, serverA1)
	// a1 and a2 exist; add 254 more for 256.
	mustExec(t, e.owner, `
		INSERT INTO waystone.servers (tenant_id, server_id, node_id, name, profile, runtime)
		SELECT $1, ('0199e1a0-0000-7000-8000-' || lpad(to_hex(g), 12, '0'))::uuid, $2, 'bulk ' || g, 'minecraft-java', 'systemd'
		FROM generate_series(1, 254) AS g`, tenantA, nodeA)

	epoch, ids, err := e.hello(nodeActA)
	if err != nil || epoch != 1 || len(ids) != 256 {
		t.Fatalf("hello with 256 servers = %d, %d ids, %v", epoch, len(ids), err)
	}
	// That hello made the op uncertain; start a fresh running one.
	mustExec(t, e.owner, `UPDATE waystone.operations SET resolved_at = now(), resolved_outcome = 'failed' WHERE operation_id = $1`, running)
	running = e.runningOp(t, serverA2)
	mustExec(t, e.owner, `INSERT INTO waystone.servers (tenant_id, server_id, node_id, name, profile, runtime)
		VALUES ($1, '0199e1a0-0000-7000-8000-0000000fffff', $2, 'one too many', 'minecraft-java', 'systemd')`, tenantA, nodeA)
	before := e.node(t, nodeA)

	if _, _, err := e.hello(nodeActA); !errors.Is(err, ErrTooManyServers) {
		t.Fatalf("hello with 257 servers: %v, want ErrTooManyServers", err)
	}
	after := e.node(t, nodeA)
	if after.epoch != before.epoch || !after.lastSeen.Equal(*before.lastSeen) {
		t.Errorf("node changed: %+v -> %+v", before, after)
	}
	if s, _ := e.opState(t, running); s != "running" {
		t.Errorf("running op = %s, want still running", s)
	}
}

// ---------------------------------------------------------------- reports

// C2: reports apply in (epoch, seq) order per server; gaps are fine,
// repeats and older seqs are not; the observation time is the controller's
// receipt time no matter what the agent's clock says.
func TestAcceptReportOrdering(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	epoch, _, err := e.hello(nodeActA)
	if err != nil {
		t.Fatal(err)
	}
	at := agentTime(t)

	acc, rej, err := e.report(nodeActA, Report{Epoch: epoch, Seq: 5, AgentTime: at, Entries: []ReportEntry{
		{ServerID: serverA1, State: ProcessRunning, Readiness: ReadinessUnknown}, {ServerID: serverA2, State: ProcessStopped, Readiness: ReadinessUnknown},
	}})
	if err != nil || acc != 2 || rej != 0 {
		t.Fatalf("seq 5 = %d/%d, %v", acc, rej, err)
	}
	a1 := e.server(t, serverA1)
	n := e.node(t, nodeA)
	if a1.state != "running" || a1.epoch != epoch || a1.seq != 5 || a1.observed == nil ||
		!a1.observed.Equal(*n.lastSeen) || a1.since == nil || !a1.since.Equal(*a1.observed) ||
		a1.agentReportedAt == nil || !a1.agentReportedAt.Equal(at) {
		t.Errorf("a1 after seq 5 = %+v (node %+v)", a1, n)
	}

	// Older seq: nothing changes.
	acc, rej, err = e.report(nodeActA, Report{Epoch: epoch, Seq: 4, AgentTime: at, Entries: []ReportEntry{
		{ServerID: serverA1, State: ProcessStopped, Readiness: ReadinessUnknown},
	}})
	if err != nil || acc != 0 || rej != 1 {
		t.Errorf("seq 4 = %d/%d, %v; want 0/1", acc, rej, err)
	}
	// Same seq again: refused too.
	if acc, rej, err = e.report(nodeActA, Report{Epoch: epoch, Seq: 5, AgentTime: at, Entries: []ReportEntry{
		{ServerID: serverA1, State: ProcessStopped, Readiness: ReadinessUnknown},
	}}); err != nil || acc != 0 || rej != 1 {
		t.Errorf("repeated seq 5 = %d/%d, %v; want 0/1", acc, rej, err)
	}
	if s := e.server(t, serverA1); s.state != "running" || s.seq != 5 {
		t.Errorf("a1 changed by old reports: %+v", s)
	}

	// A gap is fine. Same state: since stays, observed moves. The agent's
	// clock a year ahead changes nothing about ordering or receipt time.
	future := at.AddDate(1, 0, 0)
	if acc, rej, err = e.report(nodeActA, Report{Epoch: epoch, Seq: 7, AgentTime: future, Entries: []ReportEntry{
		{ServerID: serverA1, State: ProcessRunning, Readiness: ReadinessUnknown},
	}}); err != nil || acc != 1 || rej != 0 {
		t.Fatalf("seq 7 = %d/%d, %v", acc, rej, err)
	}
	s := e.server(t, serverA1)
	var dbNow time.Time
	if err := e.owner.QueryRow(ctx, `SELECT now()`).Scan(&dbNow); err != nil {
		t.Fatal(err)
	}
	if s.seq != 7 || !s.since.Equal(*a1.since) || !s.observed.After(*a1.observed) ||
		s.observed.After(dbNow) || dbNow.Sub(*s.observed) > time.Minute || !s.agentReportedAt.Equal(future) {
		t.Errorf("a1 after seq 7 = %+v (db now %v)", s, dbNow)
	}

	// A state change moves since.
	if _, _, err := e.report(nodeActA, Report{Epoch: epoch, Seq: 8, AgentTime: at, Entries: []ReportEntry{
		{ServerID: serverA1, State: ProcessStopping, Readiness: ReadinessUnknown},
	}}); err != nil {
		t.Fatal(err)
	}
	if s2 := e.server(t, serverA1); s2.state != "stopping" || !s2.since.Equal(*s2.observed) || !s2.since.After(*s.since) {
		t.Errorf("a1 after state change = %+v", s2)
	}
}

// I8, C3, R11: after a new hello, a report from the old session is refused
// as a whole and writes nothing, not even last_seen_at.
func TestAcceptReportSuperseded(t *testing.T) {
	e := setup(t)
	old, _, err := e.hello(nodeActA)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.hello(nodeActA); err != nil {
		t.Fatal(err)
	}
	before := e.node(t, nodeA)
	_, _, err = e.report(nodeActA, Report{Epoch: old, Seq: 1, AgentTime: agentTime(t), Entries: []ReportEntry{
		{ServerID: serverA1, State: ProcessRunning, Readiness: ReadinessUnknown},
	}})
	if !errors.Is(err, ErrSessionSuperseded) {
		t.Fatalf("old-epoch report: %v, want ErrSessionSuperseded", err)
	}
	if s := e.server(t, serverA1); s.state != "unknown" || s.seq != 0 {
		t.Errorf("a1 written by a superseded report: %+v", s)
	}
	if after := e.node(t, nodeA); !after.lastSeen.Equal(*before.lastSeen) {
		t.Errorf("last_seen_at moved on a refused report: %v -> %v", before.lastSeen, after.lastSeen)
	}
	// A future epoch is refused the same way.
	if _, _, err := e.report(nodeActA, Report{Epoch: before.epoch + 1, Seq: 1, AgentTime: agentTime(t)}); !errors.Is(err, ErrSessionSuperseded) {
		t.Errorf("future-epoch report: %v, want ErrSessionSuperseded", err)
	}
}

// F9, R5: entries for servers that aren't on this node are rejected one
// by one; the rest of the batch still counts. A zero-entry report is valid
// and refreshes last_seen_at (R11).
func TestAcceptReportOtherNodes(t *testing.T) {
	e := setup(t)
	addNodeA2(t, e)
	epoch, _, err := e.hello(nodeActA)
	if err != nil {
		t.Fatal(err)
	}
	acc, rej, err := e.report(nodeActA, Report{Epoch: epoch, Seq: 1, AgentTime: agentTime(t), Entries: []ReportEntry{
		{ServerID: serverA1, State: ProcessRunning, Readiness: ReadinessUnknown},
		{ServerID: serverOnA2, State: ProcessRunning, Readiness: ReadinessUnknown},    // same tenant, another node
		{ServerID: serverB1, State: ProcessRunning, Readiness: ReadinessUnknown},      // another tenant
		{ServerID: missingServer, State: ProcessRunning, Readiness: ReadinessUnknown}, // nobody's
	}})
	if err != nil || acc != 1 || rej != 3 {
		t.Fatalf("mixed report = %d/%d, %v; want 1/3", acc, rej, err)
	}
	for _, id := range []string{serverOnA2, serverB1} {
		if s := e.server(t, id); s.state != "unknown" || s.seq != 0 {
			t.Errorf("%s written by node a: %+v", id, s)
		}
	}

	before := e.node(t, nodeA)
	time.Sleep(10 * time.Millisecond) // so a new now() differs
	if acc, rej, err := e.report(nodeActA, Report{Epoch: epoch, Seq: 2, AgentTime: agentTime(t)}); err != nil || acc != 0 || rej != 0 {
		t.Fatalf("empty report = %d/%d, %v", acc, rej, err)
	}
	if after := e.node(t, nodeA); !after.lastSeen.After(*before.lastSeen) {
		t.Errorf("empty report didn't refresh last_seen_at: %v -> %v", before.lastSeen, after.lastSeen)
	}

	// An admin can't send a report, whatever the epoch.
	if _, _, err := e.report(adminA, Report{Epoch: epoch, Seq: 3, AgentTime: agentTime(t)}); !errors.Is(err, ErrNotNode) {
		t.Errorf("report as admin: %v, want ErrNotNode", err)
	}
	// A malformed report never reaches the database.
	if _, _, err := e.report(nodeActA, Report{Epoch: epoch, Seq: 3, AgentTime: agentTime(t), Entries: []ReportEntry{
		{ServerID: serverA1, State: ProcessRunning, Readiness: ReadinessUnknown}, {ServerID: serverA1, State: ProcessStopped, Readiness: ReadinessUnknown},
	}}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("duplicate entries: %v, want ErrInvalidInput", err)
	}
}

// ------------------------------------------------------------ concurrency

// Two hellos at the same time get consecutive, different epochs.
func TestConcurrentHellos(t *testing.T) {
	e := setup(t)
	const n = 8
	epochs := make([]int64, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			epochs[i], _, errs[i] = e.hello(nodeActA)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	sort.Slice(epochs, func(i, j int) bool { return epochs[i] < epochs[j] })
	for i, ep := range epochs {
		if ep != int64(i+1) {
			t.Fatalf("epochs = %v, want 1..%d", epochs, n)
		}
	}
}

// beginAs opens a raw app transaction acting as actor in tenant.
func beginAs(t *testing.T, conn *pgx.Conn, actorType, actorID, tenant string) pgx.Tx {
	t.Helper()
	ctx := context.Background()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('app.actor_type', $1, true),
		set_config('app.actor_id', $2, true), set_config('app.tenant_id', $3, true)`, actorType, actorID, tenant); err != nil {
		t.Fatal(err)
	}
	return tx
}

// waitBlocked waits until some backend is waiting on a lock, so a test
// knows its goroutine reached the blocking statement. pg_locks, unlike
// pg_stat_activity, shows other roles' sessions to a non-superuser.
func waitBlocked(t *testing.T, e *env) {
	t.Helper()
	for range 200 {
		var n int
		if err := e.owner.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_locks WHERE NOT granted`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("nothing blocked on a lock")
}

// C3, report first: a report holding the node row FOR SHARE makes a hello
// wait, and the report is stored whole under the old epoch.
func TestReportBeforeHelloSerializes(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	epoch, _, err := e.hello(nodeActA)
	if err != nil {
		t.Fatal(err)
	}
	conn := appConn(t)
	tx := beginAs(t, conn, "node", nodeA, tenantA)
	defer tx.Rollback(ctx)
	var locked int64
	if err := tx.QueryRow(ctx, `SELECT session_epoch FROM waystone.nodes WHERE node_id = $1 FOR SHARE`, nodeA).Scan(&locked); err != nil || locked != epoch {
		t.Fatalf("lock = %d, %v", locked, err)
	}

	done := make(chan error, 1)
	var newEpoch int64
	go func() {
		var err error
		newEpoch, _, err = e.hello(nodeActA)
		done <- err
	}()
	waitBlocked(t, e)

	for i, id := range []string{serverA1, serverA2} {
		if _, err := tx.Exec(ctx, `UPDATE waystone.servers SET process_state = 'running', process_since = now(),
			process_observed_at = now(), readiness = 'not_ready', readiness_since = now(), readiness_observed_at = now(),
			report_epoch = $2, report_seq = 1, agent_reported_at = now()
			WHERE server_id = $1`, id, epoch); err != nil {
			t.Fatalf("entry %d: %v", i, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil || newEpoch != epoch+1 {
		t.Fatalf("hello after report = %d, %v", newEpoch, err)
	}
	for _, id := range []string{serverA1, serverA2} {
		if s := e.server(t, id); s.state != "running" || s.epoch != epoch {
			t.Errorf("%s = %+v, want the old-epoch report stored", id, s)
		}
	}
}

// C3, hello first: a hello holding the node row makes AcceptReport wait,
// and once the hello commits the report is refused as superseded.
func TestHelloBeforeReportSerializes(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	epoch, _, err := e.hello(nodeActA)
	if err != nil {
		t.Fatal(err)
	}
	conn := appConn(t)
	tx := beginAs(t, conn, "node", nodeA, tenantA)
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE waystone.nodes SET session_epoch = session_epoch + 1, last_seen_at = now() WHERE node_id = $1`, nodeA); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, _, err := e.report(nodeActA, Report{Epoch: epoch, Seq: 1, AgentTime: agentTime(t), Entries: []ReportEntry{
			{ServerID: serverA1, State: ProcessRunning, Readiness: ReadinessUnknown}, {ServerID: serverA2, State: ProcessRunning, Readiness: ReadinessUnknown},
		}})
		done <- err
	}()
	waitBlocked(t, e)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrSessionSuperseded) {
		t.Fatalf("report after hello: %v, want ErrSessionSuperseded", err)
	}
	for _, id := range []string{serverA1, serverA2} {
		if s := e.server(t, id); s.state != "unknown" || s.seq != 0 {
			t.Errorf("%s = %+v, want nothing written", id, s)
		}
	}
}

// ----------------------------------------------------------------- guards

// guardCase runs one UPDATE as actor in its own transaction and reports
// the error (nil if the statement succeeded and changed one row).
func guardExec(t *testing.T, conn *pgx.Conn, actorType, actorID, q string, args ...any) error {
	t.Helper()
	ctx := context.Background()
	tx := beginAs(t, conn, actorType, actorID, tenantA)
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, q, args...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%d rows changed", tag.RowsAffected())
	}
	return nil
}

func wantRaised(t *testing.T, name string, err error, msg string) {
	t.Helper()
	if sqlState(err) != "P0001" || !strings.Contains(err.Error(), msg) {
		t.Errorf("%s: err = %v, want a P0001 exception mentioning %q", name, err, msg)
	}
}

// R4 nodes_guard, rule by rule.
func TestNodesGuard(t *testing.T) {
	e := setup(t)
	addNodeA2(t, e)
	conn := appConn(t)
	upd := func(actorType, actorID, set, node string) error {
		return guardExec(t, conn, actorType, actorID, `UPDATE waystone.nodes SET `+set+` WHERE node_id = $1`, node)
	}

	// The happy path, so the failures below are about the rule alone.
	if err := upd("node", nodeA, "session_epoch = session_epoch + 1, last_seen_at = now()", nodeA); err != nil {
		t.Fatalf("own +1: %v", err)
	}
	// Rule 1: an admin, and another node of the same tenant.
	wantRaised(t, "admin", upd("local_user", "1000", "last_seen_at = now()", nodeA), "only the node itself")
	wantRaised(t, "other node", upd("node", nodeA2, "last_seen_at = now()", nodeA), "only the node itself")
	// Rule 2.
	wantRaised(t, "+2", upd("node", nodeA, "session_epoch = session_epoch + 2", nodeA), "up by one")
	wantRaised(t, "-1", upd("node", nodeA, "session_epoch = session_epoch - 1", nodeA), "up by one")
	// Rule 3.
	wantRaised(t, "old last_seen_at", upd("node", nodeA, "last_seen_at = now() - interval '1 second'", nodeA), "must be now()")
	// The column grant stops identity changes before the trigger.
	if err := upd("node", nodeA, "name = 'x'", nodeA); sqlState(err) != "42501" {
		t.Errorf("rename: err = %v, want 42501", err)
	}
	// The owner is exempt (seeding, migrations).
	mustExec(t, e.owner, `UPDATE waystone.nodes SET session_epoch = 40, last_seen_at = NULL WHERE node_id = $1`, nodeA)
}

// R4 servers_report_guard, rule by rule, plus the v2.1 partial updates.
func TestServersReportGuard(t *testing.T) {
	e := setup(t)
	addNodeA2(t, e)
	epoch, _, err := e.hello(nodeActA)
	if err != nil {
		t.Fatal(err)
	}
	// Committed starting point: a1 stopped at (epoch, 2). guardExec rolls
	// every statement back, so each case below runs against this row.
	if _, _, err := e.report(nodeActA, Report{Epoch: epoch, Seq: 2, AgentTime: agentTime(t), Entries: []ReportEntry{
		{ServerID: serverA1, State: ProcessStopped, Readiness: ReadinessUnknown},
	}}); err != nil {
		t.Fatal(err)
	}
	conn := appConn(t)
	// set builds a coherent report SET clause; over replaces single
	// columns so each case breaks exactly one rule (Postgres refuses the
	// same column twice in one SET).
	set := func(state string, seq int64, over map[string]string) string {
		readiness := "'not_ready'"
		if r, ok := over["readiness"]; ok {
			readiness = r
		}
		cols := []struct{ name, value string }{
			{"process_state", fmt.Sprintf("'%s'", state)},
			{"process_since", fmt.Sprintf("CASE WHEN process_state IS DISTINCT FROM '%s' THEN now() ELSE process_since END", state)},
			{"process_observed_at", "now()"},
			{"readiness", readiness},
			{"readiness_since", fmt.Sprintf("CASE WHEN readiness IS DISTINCT FROM %s THEN now() ELSE readiness_since END", readiness)},
			{"readiness_observed_at", "now()"},
			{"report_epoch", fmt.Sprint(epoch)},
			{"report_seq", fmt.Sprint(seq)},
			{"agent_reported_at", "now()"},
		}
		parts := make([]string, 0, len(cols))
		for _, c := range cols {
			v := c.value
			if o, ok := over[c.name]; ok {
				v = o
			}
			parts = append(parts, c.name+" = "+v)
		}
		return strings.Join(parts, ", ")
	}
	full := func(state string, seq int64) string { return set(state, seq, nil) }
	upd := func(actorType, actorID, set, server string) error {
		return guardExec(t, conn, actorType, actorID, `UPDATE waystone.servers SET `+set+` WHERE server_id = $1`, server)
	}

	// The happy path, a state change and a same-state refresh.
	if err := upd("node", nodeA, full("running", 3), serverA1); err != nil {
		t.Fatalf("coherent update: %v", err)
	}
	if err := upd("node", nodeA, full("stopped", 3), serverA1); err != nil {
		t.Fatalf("coherent update: %v", err)
	}

	// Rule 1: a node never changes desired_state; an admin may.
	wantRaised(t, "node desired_state", upd("node", nodeA, "desired_state = 'running'", serverA1), "desired_state")
	if err := upd("local_user", "1000", "desired_state = 'running'", serverA1); err != nil {
		t.Errorf("admin desired_state: %v", err)
	}
	// Rule 2.1: an admin, or another node.
	wantRaised(t, "admin report", upd("local_user", "1000", full("running", 9), serverA1), "only the server's node")
	wantRaised(t, "other node", upd("node", nodeA2, full("running", 9), serverA1), "only the server's node")
	// Rule 2.2: not the current epoch.
	wantRaised(t, "wrong epoch", upd("node", nodeA, set("running", 9, map[string]string{"report_epoch": fmt.Sprint(epoch + 1)}), serverA1), "current epoch")
	// Rule 2.3: (epoch, seq) not newer.
	wantRaised(t, "same seq", upd("node", nodeA, full("running", 1), serverA1), "move forward")
	// Rule 2.4: observed not now().
	wantRaised(t, "old observed", upd("node", nodeA, set("running", 9, map[string]string{"process_observed_at": "now() - interval '1 second'"}), serverA1), "process_observed_at")
	// Rule 2.5: since without a state change, and a state change without since.
	wantRaised(t, "since moved", upd("node", nodeA, set("stopped", 9, map[string]string{"process_since": "now() - interval '1 hour'"}), serverA1), "without a state change")
	// Rules 2.7 to 2.9 (00005): readiness moves with the report, observed
	// now(), since only on a change, and ready only while running.
	if err := upd("node", nodeA, set("running", 9, map[string]string{"readiness": "'ready'"}), serverA1); err != nil {
		t.Errorf("running and ready: %v", err)
	}
	wantRaised(t, "readiness old observed", upd("node", nodeA, set("running", 9, map[string]string{"readiness_observed_at": "now() - interval '1 second'"}), serverA1), "readiness_observed_at")
	wantRaised(t, "readiness observed NULL", upd("node", nodeA, set("running", 9, map[string]string{"readiness_observed_at": "NULL"}), serverA1), "readiness_observed_at")
	wantRaised(t, "readiness since moved", upd("node", nodeA, set("running", 9, map[string]string{"readiness": "'unknown'", "readiness_since": "now() - interval '1 hour'"}), serverA1), "without a readiness change")
	wantRaised(t, "readiness change, since kept", upd("node", nodeA, set("running", 9, map[string]string{"readiness_since": "readiness_since"}), serverA1), "readiness_since must be now()")
	for _, state := range []string{"stopped", "starting", "stopping", "failed", "unknown"} {
		wantRaised(t, "ready while "+state, upd("node", nodeA, set(state, 9, map[string]string{"readiness": "'ready'"}), serverA1), "only be ready while")
	}
	wantRaised(t, "admin readiness", upd("local_user", "1000", "readiness = 'ready', readiness_since = now(), readiness_observed_at = now()", serverA1), "only the server's node")

	// Rule 2.6 and the other partial updates (v2.1 point 2).
	partial := map[string]struct{ set, msg string }{
		"only process_state":         {"process_state = 'running'", ""},
		"only report_seq":            {"report_seq = 50", ""},
		"epoch and seq, no observed": {fmt.Sprintf("report_epoch = %d, report_seq = 51", epoch), "process_observed_at"},
		"only process_observed_at":   {"process_observed_at = now()", "move forward"},
		"only agent_reported_at":     {"agent_reported_at = now()", "move forward"},
		"only readiness":             {"readiness = 'not_ready'", "move forward"},
		"report without readiness":   {fmt.Sprintf("process_state = 'stopped', process_observed_at = now(), report_epoch = %d, report_seq = 55, agent_reported_at = now()", epoch), "readiness_observed_at"},
		"agent_reported_at NULL":     {set("stopped", 52, map[string]string{"agent_reported_at": "NULL"}), "agent_reported_at"},
		"observed not now":           {set("stopped", 53, map[string]string{"process_observed_at": "'2020-01-01'"}), "process_observed_at"},
		"state change, since kept":   {set("running", 54, map[string]string{"process_since": "process_since"}), "process_since"},
	}
	for name, c := range partial {
		err := upd("node", nodeA, c.set, serverA1)
		if sqlState(err) != "P0001" || (c.msg != "" && !strings.Contains(err.Error(), c.msg)) {
			t.Errorf("%s: err = %v, want a P0001 exception mentioning %q", name, err, c.msg)
		}
	}

	// Nothing above got through.
	if s := e.server(t, serverA1); s.state != "stopped" || s.seq != 2 {
		t.Errorf("a1 = %+v, want the committed report untouched", s)
	}
	// The owner is exempt.
	mustExec(t, e.owner, `UPDATE waystone.servers SET process_state = 'failed' WHERE server_id = $1`, serverA1)
}
