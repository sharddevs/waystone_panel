// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package agentlink

// Setup for the agent link's integration tests: the real gRPC server on a
// real Unix socket, a real PostgreSQL, and real gRPC clients. Like the
// store's and the API's tests, these wipe the waystone_test database, so
// run the packages one at a time:
//
//	CGO_ENABLED=1 go test -race -p 1 -tags integration ./internal/...

import (
	"bytes"
	"context"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/local"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/sharddevs/waystone_panel/gen/waystone/agent/v1"
	"github.com/sharddevs/waystone_panel/internal/store"
)

const (
	tenantA  = "0199e1a0-0000-7000-8000-00000000000a"
	tenantB  = "0199e1a0-0000-7000-8000-00000000000b"
	nodeA    = testNode
	nodeB    = "0199e1a0-0000-7000-8000-0000000000b1"
	serverA1 = testServer
	serverA2 = "0199e1a0-0000-7000-8000-000000000a02"
	serverB1 = "0199e1a0-0000-7000-8000-000000000b01"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func ownerDSN() string {
	return envOr("WAYSTONE_TEST_OWNER_DSN", "host=/var/run/postgresql dbname=waystone_test user=waystone_owner")
}

func appDSN() string {
	return envOr("WAYSTONE_TEST_APP_DSN", "host=/var/run/postgresql dbname=waystone_test user=waystone_app")
}

// syncBuffer is a log destination the server and the test can share.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// env is a migrated, seeded database and a running link for node A.
type env struct {
	owner *pgx.Conn
	store *store.Store
	srv   *Server
	cc    agentv1.AgentLinkServiceClient // shared by e.session, which may open many streams
	path  string
	logs  *syncBuffer
}

// setupDB wipes waystone_test, migrates it, and seeds two tenants: node A
// (a member of A with role node) with servers a1 and a2, node B with b1.
func setupDB(t *testing.T) (*pgx.Conn, *store.Store) {
	t.Helper()
	ctx := context.Background()
	if !strings.Contains(ownerDSN(), "waystone_test") {
		t.Fatalf("refusing to wipe a database that isn't waystone_test: %q", ownerDSN())
	}
	owner, err := pgx.Connect(ctx, ownerDSN())
	if err != nil {
		t.Fatalf("connect as owner: %v", err)
	}
	t.Cleanup(func() { owner.Close(context.Background()) })
	// Fixtures move operations by hand, which the 4c guard allows the
	// owner only with the override on. The history rules still apply.
	mustExec(t, owner, `SET waystone.owner_override = 'on'`)
	for _, q := range []string{
		`DROP SCHEMA IF EXISTS waystone CASCADE`,
		`DROP SCHEMA IF EXISTS waystone_meta CASCADE`,
		`CREATE SCHEMA waystone`,
		`CREATE SCHEMA waystone_meta`,
	} {
		mustExec(t, owner, q)
	}
	if err := store.Migrate(ctx, ownerDSN()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	for _, q := range []string{
		`INSERT INTO waystone.tenants (tenant_id, name) VALUES ('` + tenantA + `', 'A'), ('` + tenantB + `', 'B')`,
		`INSERT INTO waystone.memberships (tenant_id, actor_type, actor_id, role) VALUES
			('` + tenantA + `', 'local_user', '1000', 'admin'),
			('` + tenantA + `', 'node', '` + nodeA + `', 'node'),
			('` + tenantB + `', 'node', '` + nodeB + `', 'node')`,
		`INSERT INTO waystone.nodes (tenant_id, node_id, name) VALUES
			('` + tenantA + `', '` + nodeA + `', 'node a'), ('` + tenantB + `', '` + nodeB + `', 'node b')`,
		`INSERT INTO waystone.servers (tenant_id, server_id, node_id, name, profile, runtime) VALUES
			('` + tenantA + `', '` + serverA1 + `', '` + nodeA + `', 'a1', 'minecraft-java', 'systemd'),
			('` + tenantA + `', '` + serverA2 + `', '` + nodeA + `', 'a2', 'minecraft-java', 'systemd'),
			('` + tenantB + `', '` + serverB1 + `', '` + nodeB + `', 'b1', 'minecraft-java', 'systemd')`,
	} {
		mustExec(t, owner, q)
	}
	st, err := store.Open(ctx, appDSN())
	if err != nil {
		t.Fatalf("Open as app: %v", err)
	}
	t.Cleanup(st.Close)
	return owner, st
}

// setup starts the link on a fresh socket. The agent uid is our own, so
// real SO_PEERCRED passes for the test's own clients; mod can change the
// config before the server is built.
func setup(t *testing.T, mod func(*Config)) *env {
	t.Helper()
	owner, st := setupDB(t)
	e := &env{owner: owner, store: st, logs: &syncBuffer{}}
	cfg := Config{Store: st, NodeID: nodeA, AgentUID: uint32(os.Getuid()), Log: log.New(e.logs, "", 0)}
	if mod != nil {
		mod(&cfg)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.srv = srv
	e.path = filepath.Join(t.TempDir(), "agent.sock")
	l, err := net.Listen("unix", e.path)
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan struct{})
	go func() {
		defer close(served)
		srv.Serve(l)
	}()
	t.Cleanup(func() {
		srv.CancelSessions()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Stop(ctx)
		<-served
	})
	return e
}

func mustExec(t *testing.T, conn *pgx.Conn, sql string, args ...any) {
	t.Helper()
	if _, err := conn.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// client is one gRPC connection to the link. The test's own clients use
// gRPC's local credentials; the agent's peer check lives in internal/agent.
func (e *env) client(t *testing.T) agentv1.AgentLinkServiceClient {
	t.Helper()
	cc, err := grpc.NewClient("unix://"+e.path, grpc.WithTransportCredentials(local.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cc.Close() })
	return agentv1.NewAgentLinkServiceClient(cc)
}

// stream opens one Connect stream that ends with the test.
func (e *env) stream(t *testing.T, c agentv1.AgentLinkServiceClient) agentv1.AgentLinkService_ConnectClient {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s, err := c.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func helloMsg(node string) *agentv1.ConnectRequest {
	return &agentv1.ConnectRequest{Msg: &agentv1.ConnectRequest_Hello{Hello: &agentv1.Hello{NodeId: node, AgentVersion: "test"}}}
}

// hello says hello as node A and returns the reply.
func hello(t *testing.T, s agentv1.AgentLinkService_ConnectClient) *agentv1.HelloReply {
	t.Helper()
	if err := s.Send(helloMsg(nodeA)); err != nil {
		t.Fatal(err)
	}
	m, err := s.Recv()
	if err != nil {
		t.Fatalf("hello: %v", err)
	}
	if m.GetHelloReply() == nil {
		t.Fatalf("hello: got %v", m)
	}
	return m.GetHelloReply()
}

func entry(id string, state agentv1.ProcessState) *agentv1.ReportEntry {
	return &agentv1.ReportEntry{ServerId: id, ProcessState: state}
}

func report(epoch, seq int64, entries ...*agentv1.ReportEntry) *agentv1.ConnectRequest {
	return &agentv1.ConnectRequest{Msg: &agentv1.ConnectRequest_Report{Report: &agentv1.Report{
		Epoch: epoch, Seq: seq, AgentTime: timestamppb.Now(), Entries: entries,
	}}}
}

// wantClosed waits for the stream to end and checks its status.
func wantClosed(t *testing.T, name string, s agentv1.AgentLinkService_ConnectClient, code codes.Code, msg string) {
	t.Helper()
	for {
		_, err := s.Recv()
		if err == nil {
			continue
		}
		st, _ := status.FromError(err)
		if st.Code() != code || st.Message() != msg {
			t.Errorf("%s: stream ended with %v %q, want %v %q", name, st.Code(), st.Message(), code, msg)
		}
		return
	}
}

// nodeRow is node A's session columns.
type nodeRow struct {
	epoch    int64
	lastSeen *time.Time
}

func (e *env) node(t *testing.T) nodeRow {
	t.Helper()
	var n nodeRow
	if err := e.owner.QueryRow(context.Background(),
		`SELECT session_epoch, last_seen_at FROM waystone.nodes WHERE node_id = $1`, nodeA).Scan(&n.epoch, &n.lastSeen); err != nil {
		t.Fatal(err)
	}
	return n
}

// reportCols is every report column of every server, as one string, to
// prove a refused message changed nothing.
func (e *env) reportCols(t *testing.T) string {
	t.Helper()
	var s string
	if err := e.owner.QueryRow(context.Background(), `
		SELECT string_agg(concat_ws('|', server_id, process_state, process_since, process_observed_at,
		       report_epoch, report_seq, agent_reported_at), ';' ORDER BY server_id)
		FROM waystone.servers`).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// serverState is one server's stored state and seq.
func (e *env) serverState(t *testing.T, id string) (string, int64) {
	t.Helper()
	var state string
	var seq int64
	if err := e.owner.QueryRow(context.Background(),
		`SELECT process_state, report_seq FROM waystone.servers WHERE server_id = $1`, id).Scan(&state, &seq); err != nil {
		t.Fatal(err)
	}
	return state, seq
}

// waitFor polls cond for up to 5 s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for range 500 {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
