// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package agentlink

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/local"
	"google.golang.org/grpc/status"

	agentv1 "github.com/sharddevs/waystone_panel/gen/waystone/agent/v1"
	"github.com/sharddevs/waystone_panel/internal/agent"
	"github.com/sharddevs/waystone_panel/internal/agenttls"
	"github.com/sharddevs/waystone_panel/internal/store"
)

// 6a-2: the TCP listener, enrollment, the authorizer and revocation, over
// real TLS on loopback and a real PostgreSQL.

var tcpAdmin = store.Actor{Type: store.ActorLocalUser, ID: "1000", Name: "admin a"}

type tcpEnv struct {
	*env
	addr    string
	ctlPin  agenttls.Pin
	served  chan struct{}
	ctlCert tls.Certificate
}

// setupTCP starts a link with only the TCP listener, on 127.0.0.1.
func setupTCP(t *testing.T, mod func(*Config)) *tcpEnv {
	t.Helper()
	owner, st := setupDB(t)
	e := &tcpEnv{env: &env{owner: owner, store: st, logs: &syncBuffer{}}}
	cert, err := agenttls.Generate()
	if err != nil {
		t.Fatal(err)
	}
	e.ctlCert, e.ctlPin = cert, agenttls.PinOf(cert.Leaf)
	cfg := Config{Store: st, TLS: &cert, Version: "test", Log: log.New(e.logs, "", 0)}
	if mod != nil {
		mod(&cfg)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.srv = srv
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	e.addr = l.Addr().String()
	e.served = make(chan struct{})
	go func() {
		defer close(e.served)
		srv.ServeTCP(l)
	}()
	t.Cleanup(func() {
		srv.CancelSessions()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Stop(ctx)
		<-e.served
	})
	return e
}

func newKey(t *testing.T) tls.Certificate {
	t.Helper()
	c, err := agenttls.Generate()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// dial is a gRPC connection that presents key and trusts the
// controller's pin, from local (a 127.x address; "" for any).
func (e *tcpEnv) dial(t *testing.T, key tls.Certificate, local string) *grpc.ClientConn {
	t.Helper()
	opts := []grpc.DialOption{grpc.WithTransportCredentials(credentials.NewTLS(agenttls.ClientConfig(key, e.ctlPin)))}
	if local != "" {
		opts = append(opts, grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
			d := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(local)}}
			return d.DialContext(ctx, "tcp", addr)
		}))
	}
	cc, err := grpc.NewClient("passthrough:///"+e.addr, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cc.Close() })
	return cc
}

func (e *tcpEnv) token(t *testing.T, name string) [32]byte {
	t.Helper()
	var secret [32]byte
	err := e.store.InTenantTx(context.Background(), tcpAdmin, func(ctx context.Context, tx *store.Tx) error {
		var err error
		_, secret, err = tx.CreateEnrollment(ctx, name)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return secret
}

func enrollCall(cc *grpc.ClientConn, secret []byte) (*agentv1.EnrollResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return agentv1.NewEnrollServiceClient(cc).Enroll(ctx, &agentv1.EnrollRequest{Secret: secret})
}

func wantStatus(t *testing.T, what string, err error, code codes.Code, msg string) {
	t.Helper()
	st, _ := status.FromError(err)
	if st.Code() != code || st.Message() != msg {
		t.Fatalf("%s: %v %q, want %v %q", what, st.Code(), st.Message(), code, msg)
	}
}

// enrolled makes a node over the wire and returns its key and ID.
func (e *tcpEnv) enrolled(t *testing.T, name string) (tls.Certificate, string) {
	t.Helper()
	key := newKey(t)
	secret := e.token(t, name)
	r, err := enrollCall(e.dial(t, key, ""), secret[:])
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	return key, r.GetNodeId()
}

func tcpHello(t *testing.T, s agentv1.AgentLinkService_ConnectClient, node, version string, caps ...string) (*agentv1.HelloReply, error) {
	t.Helper()
	err := s.Send(&agentv1.ConnectRequest{Msg: &agentv1.ConnectRequest_Hello{Hello: &agentv1.Hello{NodeId: node, AgentVersion: version, Capabilities: caps}}})
	if err != nil && err != io.EOF {
		return nil, err
	}
	// io.EOF from Send: the server already ended the stream, and Recv
	// has its status.
	m, err := s.Recv()
	if err != nil {
		return nil, err
	}
	return m.GetHelloReply(), nil
}

func connectStream(t *testing.T, cc *grpc.ClientConn) agentv1.AgentLinkService_ConnectClient {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s, err := agentv1.NewAgentLinkServiceClient(cc).Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestTCPEnrollThenConnect(t *testing.T) {
	e := setupTCP(t, nil)
	key, node := e.enrolled(t, "node c")
	// The pinned key is the one the handshake proved.
	n, err := e.store.AgentNodeByPin(context.Background(), agenttls.PinOf(key.Leaf))
	if err != nil || n.NodeID != node || n.Name != "node c" {
		t.Fatalf("pinned node %+v, %v", n, err)
	}
	s := connectStream(t, e.dial(t, key, ""))
	reply, err := tcpHello(t, s, node, "test", "systemd", "minecraft-java")
	if err != nil || reply == nil || reply.GetEpoch() != 1 {
		t.Fatalf("hello: %v %v", reply, err)
	}
	var v string
	var caps []string
	if err := e.owner.QueryRow(context.Background(), `SELECT agent_version, capabilities FROM waystone.nodes WHERE node_id = $1`, node).Scan(&v, &caps); err != nil || v != "test" || len(caps) != 2 {
		t.Fatalf("agent info %q %v, %v", v, caps, err)
	}
}

// 6M1, N2: a key that isn't pinned reaches nothing but Enroll.
func TestTCPUnknownKeyOnlyEnrolls(t *testing.T) {
	e := setupTCP(t, nil)
	s := connectStream(t, e.dial(t, newKey(t), ""))
	_, err := tcpHello(t, s, nodeA, "test")
	wantStatus(t, "unknown key Connect", err, codes.PermissionDenied, MsgNotPermitted)
}

// enrollOnce enrolls over a connection of its own, closed afterwards: a
// connection that isn't pinned has one Enroll call (6a-H2).
func (e *tcpEnv) enrollOnce(t *testing.T, key tls.Certificate, local string, secret []byte) (*agentv1.EnrollResponse, error) {
	t.Helper()
	cc := e.dial(t, key, local)
	defer cc.Close()
	return enrollCall(cc, secret)
}

func TestTCPEnrollRefusals(t *testing.T) {
	e := setupTCP(t, nil)
	key := newKey(t)
	_, err := e.enrollOnce(t, key, "", make([]byte, 32))
	wantStatus(t, "unknown secret", err, codes.PermissionDenied, MsgEnrollmentRefused)
	_, err = e.enrollOnce(t, key, "", make([]byte, 31))
	wantStatus(t, "short secret", err, codes.PermissionDenied, MsgEnrollmentRefused)

	secret := e.token(t, "node c")
	if _, err := e.enrollOnce(t, key, "", secret[:]); err != nil {
		t.Fatal(err)
	}
	// The same key again, with a new token: its key is taken.
	secret2 := e.token(t, "node d")
	_, err = e.enrollOnce(t, newKey(t), "", secret[:])
	wantStatus(t, "used token", err, codes.PermissionDenied, MsgEnrollmentRefused)
	_, err = e.enrollOnce(t, key, "", secret2[:])
	wantStatus(t, "pinned key enrolls again", err, codes.PermissionDenied, MsgEnrollmentRefused)
}

// D2: 10 tries per address in 15 minutes, then the same answer without
// looking.
func TestTCPEnrollRateLimit(t *testing.T) {
	e := setupTCP(t, nil)
	key := newKey(t)
	for range enrollPerIP {
		_, err := e.enrollOnce(t, key, "", make([]byte, 32))
		wantStatus(t, "try", err, codes.PermissionDenied, MsgEnrollmentRefused)
	}
	secret := e.token(t, "node c")
	_, err := e.enrollOnce(t, key, "", secret[:])
	wantStatus(t, "11th try with a good token", err, codes.PermissionDenied, MsgEnrollmentRefused)
	if count(t, e, "nodes") != 2 {
		t.Fatal("the 11th try enrolled")
	}
	// Another address still can.
	if _, err := e.enrollOnce(t, newKey(t), "127.0.0.2", secret[:]); err != nil {
		t.Fatalf("other address: %v", err)
	}
	// Q-b: the refusals are logged with the address and a count, never
	// once per try.
	if logs := e.logs.String(); !strings.Contains(logs, "reason=too_many addr=127.0.0.1 ") || strings.Count(logs, "reason=unknown addr=127.0.0.1 ") != 1 {
		t.Errorf("refusal log lines:\n%s", logs)
	}
}

// 6a-H2: a connection that isn't pinned lives for one Enroll call or its
// lifetime, whichever ends first; keepalive doesn't stretch it.
func TestTCPUnpinnedLifetime(t *testing.T) {
	e := setupTCP(t, func(c *Config) { c.UnpinnedLifetime = 300 * time.Millisecond })
	c, err := tls.Dial("tcp", e.addr, agenttls.ClientConfig(newKey(t), e.ctlPin))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	start := time.Now()
	// The server's HTTP/2 settings come first; then the close.
	for {
		if _, err := c.Read(make([]byte, 256)); err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				t.Fatal("an idle enroll-only connection outlived its lifetime")
			}
			break
		}
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("closed after %v", d)
	}
	waitFor(t, "slot released", func() bool {
		e.srv.slots.mu.Lock()
		defer e.srv.slots.mu.Unlock()
		return e.srv.slots.count == [3]int{}
	})

	// An enrolled node's connection is pinned and has no lifetime.
	key, node := e.enrolled(t, "node c")
	s := connectStream(t, e.dial(t, key, ""))
	if _, err := tcpHello(t, s, node, "test"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	if err := s.Send(&agentv1.ConnectRequest{Msg: &agentv1.ConnectRequest_Report{Report: &agentv1.Report{}}}); err != nil {
		t.Fatalf("pinned stream after the lifetime: %v", err)
	}
}

func TestTCPOneEnrollPerConnection(t *testing.T) {
	e := setupTCP(t, nil)
	cc := e.dial(t, newKey(t), "")
	_, err := enrollCall(cc, make([]byte, 32))
	wantStatus(t, "first", err, codes.PermissionDenied, MsgEnrollmentRefused)
	secret := e.token(t, "node c")
	_, err = enrollCall(cc, secret[:])
	wantStatus(t, "second on one connection", err, codes.PermissionDenied, MsgEnrollmentRefused)
	if !strings.Contains(e.logs.String(), "reason=second_call") || count(t, e, "nodes") != 2 {
		t.Fatalf("second call wasn't refused as one:\n%s", e.logs.String())
	}
	// And the connection goes soon after.
	waitFor(t, "connection closed", func() bool {
		e.srv.slots.mu.Lock()
		defer e.srv.slots.mu.Unlock()
		return e.srv.slots.count == [3]int{}
	})
}

// failingNodes is the store, except that AgentNodeByPin fails from call
// number failFrom on (counting from 1).
type failingNodes struct {
	NodeLookup
	mu       sync.Mutex
	calls    int
	failFrom int
}

func (f *failingNodes) AgentNodeByPin(ctx context.Context, pin [32]byte) (store.AgentNode, error) {
	f.mu.Lock()
	f.calls++
	fail := f.calls >= f.failFrom
	f.mu.Unlock()
	if fail {
		return store.AgentNode{}, errors.New("database down")
	}
	return f.NodeLookup.AgentNodeByPin(ctx, pin)
}

// 6a-H3: a failed lookup falls closed. At the handshake, a node's key
// gets only an enroll-only slot; at Connect, the call is refused as
// Unavailable, never let through on what the handshake saw.
func TestTCPLookupFailureFallsClosed(t *testing.T) {
	for name, failFrom := range map[string]int{"at the handshake": 1, "at Connect": 2} {
		t.Run(name, func(t *testing.T) {
			f := &failingNodes{failFrom: 1 << 30}
			e := setupTCP(t, func(c *Config) { c.Nodes = f })
			f.NodeLookup = e.store
			key, node := e.enrolled(t, "node c")
			f.mu.Lock()
			f.calls, f.failFrom = 0, failFrom
			f.mu.Unlock()
			s := connectStream(t, e.dial(t, key, ""))
			_, err := tcpHello(t, s, node, "test")
			if failFrom == 1 {
				// Enroll-only: refused before any lookup (6a-H4).
				wantStatus(t, "Connect", err, codes.PermissionDenied, MsgNotPermitted)
			} else {
				wantStatus(t, "Connect", err, codes.Unavailable, "lookup_failed")
			}
			f.mu.Lock()
			calls := f.calls
			f.mu.Unlock()
			if calls != failFrom {
				t.Errorf("%d lookups, want %d", calls, failFrom)
			}
			e.srv.slots.mu.Lock()
			counts := e.srv.slots.count
			e.srv.slots.mu.Unlock()
			if failFrom == 1 && (counts[poolPinned] != 0 || counts[poolEnrollOnly] == 0) {
				t.Fatalf("a failed lookup took a pinned slot: %v", counts)
			}
		})
	}
}

// 6a-H4: Connect on a connection that may only enroll costs no lookup,
// however many streams it opens.
func TestTCPEnrollOnlyConnectNoLookups(t *testing.T) {
	f := &failingNodes{failFrom: 1 << 30}
	e := setupTCP(t, func(c *Config) { c.Nodes = f })
	f.NodeLookup = e.store
	cc := e.dial(t, newKey(t), "")
	for i := range 100 {
		s := connectStream(t, cc)
		_, err := tcpHello(t, s, nodeA, "test")
		wantStatus(t, fmt.Sprintf("Connect %d", i), err, codes.PermissionDenied, MsgNotPermitted)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls != 1 {
		t.Fatalf("%d lookups for one handshake and 100 refused streams, want 1", f.calls)
	}
}

func count(t *testing.T, e *tcpEnv, table string) int {
	t.Helper()
	var n int
	if err := e.owner.QueryRow(context.Background(), `SELECT count(*) FROM waystone.`+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// N14 over the wire: as many keys as the enroll-only pool holds race one
// token, and one wins. (The store test races 20.)
func TestTCPEnrollRace(t *testing.T) {
	e := setupTCP(t, nil)
	secret := e.token(t, "node c")
	ccs := make([]*grpc.ClientConn, MaxEnrollOnly)
	for i := range ccs {
		// Spread over addresses: one address may hold only 4.
		ccs[i] = e.dial(t, newKey(t), net.IPv4(127, 0, 1, byte(1+i/4)).String())
	}
	var wg sync.WaitGroup
	errs := make([]error, len(ccs))
	for i, cc := range ccs {
		wg.Go(func() { _, errs[i] = enrollCall(cc, secret[:]) })
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		if err == nil {
			ok++
		} else {
			wantStatus(t, "loser", err, codes.PermissionDenied, MsgEnrollmentRefused)
		}
	}
	if ok != 1 || count(t, e, "nodes") != 3 {
		t.Fatalf("%d enrolled, %d nodes", ok, count(t, e, "nodes"))
	}
}

// N4 in small: revoking ends the live stream at once, and the node can't
// come back.
func TestTCPRevokeCutsStream(t *testing.T) {
	e := setupTCP(t, nil)
	key, node := e.enrolled(t, "node c")
	cc := e.dial(t, key, "")
	s := connectStream(t, cc)
	if _, err := tcpHello(t, s, node, "test"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "session", func() bool { return e.srv.sessions.epoch(node) == 1 })
	err := e.store.InTenantTx(context.Background(), tcpAdmin, func(ctx context.Context, tx *store.Tx) error {
		_, err := tx.RevokeNode(ctx, node, "req-1")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	e.srv.Revoke(node)
	_, err = s.Recv()
	wantStatus(t, "live stream", err, codes.PermissionDenied, MsgNodeRevoked)
	if d := time.Since(start); d > time.Second {
		t.Fatalf("the stream ended after %v", d)
	}
	s2 := connectStream(t, cc)
	_, err = tcpHello(t, s2, node, "test")
	wantStatus(t, "reconnect", err, codes.PermissionDenied, MsgNodeRevoked)
}

// 6M2 defence in depth: a revocation the link wasn't told about still
// stops the node's next write.
func TestTCPRevokedNodeCantReport(t *testing.T) {
	e := setupTCP(t, nil)
	key, node := e.enrolled(t, "node c")
	s := connectStream(t, e.dial(t, key, ""))
	reply, err := tcpHello(t, s, node, "test")
	if err != nil {
		t.Fatal(err)
	}
	err = e.store.InTenantTx(context.Background(), tcpAdmin, func(ctx context.Context, tx *store.Tx) error {
		_, err := tx.RevokeNode(ctx, node, "req-1")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Send(report(reply.GetEpoch(), 1)); err != nil {
		t.Fatal(err)
	}
	_, err = s.Recv()
	wantStatus(t, "report after revoke", err, codes.PermissionDenied, MsgNodeRevoked)

	// Connecting again, the revoked key gets an enroll-only slot, never a
	// pinned one, and nothing but the refusal.
	waitFor(t, "old connection gone", func() bool {
		e.srv.slots.mu.Lock()
		defer e.srv.slots.mu.Unlock()
		return e.srv.slots.count[poolPinned] == 0
	})
	s2 := connectStream(t, e.dial(t, key, ""))
	_, err = tcpHello(t, s2, node, "test")
	wantStatus(t, "revoked key connects", err, codes.PermissionDenied, MsgNodeRevoked)
	e.srv.slots.mu.Lock()
	counts := e.srv.slots.count
	e.srv.slots.mu.Unlock()
	if counts[poolPinned] != 0 || counts[poolEnrollOnly] == 0 {
		t.Fatalf("revoked key's slots: %v", counts)
	}
}

func TestTCPVersionMismatch(t *testing.T) {
	e := setupTCP(t, nil)
	key, node := e.enrolled(t, "node c")
	_, err := tcpHello(t, connectStream(t, e.dial(t, key, "")), node, "0.0.9")
	wantStatus(t, "old agent", err, codes.FailedPrecondition, MsgVersionMismatch)
}

// N3: a pinned key speaks only for its own node.
func TestTCPNodeMismatch(t *testing.T) {
	e := setupTCP(t, nil)
	key, _ := e.enrolled(t, "node c")
	_, err := tcpHello(t, connectStream(t, e.dial(t, key, "")), nodeA, "test")
	wantStatus(t, "other node's ID", err, codes.FailedPrecondition, "node_mismatch")
}

func TestTCPBadCapabilities(t *testing.T) {
	e := setupTCP(t, nil)
	key, node := e.enrolled(t, "node c")
	_, err := tcpHello(t, connectStream(t, e.dial(t, key, "")), node, "test", "NOT OK")
	wantStatus(t, "capabilities", err, codes.InvalidArgument, "invalid_hello")
}

// 6H1: one address holds at most 4 connections that aren't pinned.
func TestTCPPerAddressCap(t *testing.T) {
	e := setupTCP(t, nil)
	var held []net.Conn
	t.Cleanup(func() {
		for _, c := range held {
			c.Close()
		}
	})
	d := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP("127.0.0.9")}}
	for range MaxUnpinnedPerIP {
		c, err := d.Dial("tcp", e.addr)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, c)
	}
	waitFor(t, "slots taken", func() bool {
		e.srv.slots.mu.Lock()
		defer e.srv.slots.mu.Unlock()
		return e.srv.slots.count[poolHandshaking] == MaxUnpinnedPerIP
	})
	c, err := d.Dial("tcp", e.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("5th connection: read %v, want EOF (closed at accept)", err)
	}
}

// N17 in small: 100 idle connections from 25 addresses, and an enrolled
// node still connects from another one.
func TestTCPFloodDoesNotLockOutNodes(t *testing.T) {
	e := setupTCP(t, nil)
	key, node := e.enrolled(t, "node c")
	var held []net.Conn
	t.Cleanup(func() {
		for _, c := range held {
			c.Close()
		}
	})
	for i := range 100 {
		d := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.IPv4(127, 0, 2, byte(1+i/4))}}
		c, err := d.Dial("tcp", e.addr)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, c)
	}
	s := connectStream(t, e.dial(t, key, "127.0.3.1"))
	if _, err := tcpHello(t, s, node, "test"); err != nil {
		t.Fatalf("node during a flood: %v", err)
	}
}

// Enroll exists only on TCP, and only Enroll and Connect exist at all.
func TestUnixHasNoEnroll(t *testing.T) {
	e := setup(t, nil)
	cc, err := grpc.NewClient("unix://"+e.path, grpc.WithTransportCredentials(local.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	_, err = enrollCall(cc, make([]byte, 32))
	if st, _ := status.FromError(err); st.Code() != codes.Unimplemented && st.Code() != codes.PermissionDenied {
		t.Fatalf("Enroll on agent.sock: %v", err)
	}
}

// 6H6: the local node, revoked, can't say hello on agent.sock either.
func TestUnixRevokedLocalNode(t *testing.T) {
	e := setup(t, nil)
	mustExec(t, e.owner, `UPDATE waystone.nodes SET revoked_at = now() WHERE node_id = $1`, nodeA)
	s := e.stream(t, e.client(t))
	if err := s.Send(helloMsg(nodeA)); err != nil {
		t.Fatal(err)
	}
	_, err := s.Recv()
	wantStatus(t, "revoked local node", err, codes.PermissionDenied, MsgNodeRevoked)
}

// The real agent over TCP: it says hello with its capabilities, and a
// revocation sends it to the long wait.
func TestTCPRealAgent(t *testing.T) {
	e := setupTCP(t, nil)
	key, node := e.enrolled(t, "node c")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sl := &sleeps{stop: cancel}
	a, err := agent.New(agent.Config{
		ControllerAddr: e.addr,
		TLS:            agenttls.ClientConfig(key, e.ctlPin),
		NodeID:         node,
		Version:        "test",
		Helper:         &fakeHelper{},
		Runner:         &fakeRunner{},
		Prober:         fakeProber{},
		Log:            log.New(io.Discard, "", 0),
		Sleep:          sl.sleep,
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.Run(ctx)
	}()
	waitFor(t, "agent session", func() bool { return e.srv.sessions.epoch(node) == 1 })
	var caps []string
	if err := e.owner.QueryRow(context.Background(), `SELECT capabilities FROM waystone.nodes WHERE node_id = $1`, node).Scan(&caps); err != nil || strings.Join(caps, ",") != strings.Join(agent.Capabilities, ",") {
		t.Fatalf("capabilities %v, %v", caps, err)
	}
	err = e.store.InTenantTx(context.Background(), tcpAdmin, func(ctx context.Context, tx *store.Tx) error {
		_, err := tx.RevokeNode(ctx, node, "req-1")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	e.srv.Revoke(node)
	<-done
	if waits := sl.got(); len(waits) != 1 || !within(waits[0], time.Minute) {
		t.Fatalf("revoked agent waits %v, want the long 60 s", waits)
	}
}
