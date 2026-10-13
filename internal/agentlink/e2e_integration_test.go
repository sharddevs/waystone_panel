// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package agentlink

// Stage 4c-1 end to end in one process: the API, the link, the real agent,
// lifecycle.HelperRuntime, helper.Client and the real helper server code,
// with only systemctl faked.

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sharddevs/waystone_panel/internal/api"
	"github.com/sharddevs/waystone_panel/internal/helper"
	"github.com/sharddevs/waystone_panel/internal/lifecycle"
	"github.com/sharddevs/waystone_panel/internal/store"
)

// fakeSystemctl answers show with an idle unit and records start and
// stop. A unit with a gate blocks its start or stop until the gate
// closes; active and maxActive count actions running at once per unit.
type fakeSystemctl struct {
	mu        sync.Mutex
	gates     map[string]chan struct{}
	entered   chan string
	active    map[string]int
	maxActive map[string]int
	changes   []string
}

func newFakeSystemctl() *fakeSystemctl {
	return &fakeSystemctl{
		gates:     map[string]chan struct{}{},
		entered:   make(chan string, 16),
		active:    map[string]int{},
		maxActive: map[string]int{},
	}
}

func (f *fakeSystemctl) Run(ctx context.Context, argv []string) ([]byte, error) {
	verb, unit := argv[2], argv[len(argv)-1]
	if verb == "show" {
		return []byte("LoadState=loaded\nActiveState=inactive\nSubState=dead\nJob=\n"), nil
	}
	f.mu.Lock()
	f.changes = append(f.changes, verb+" "+unit)
	f.active[unit]++
	f.maxActive[unit] = max(f.maxActive[unit], f.active[unit])
	gate := f.gates[unit]
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.active[unit]--
		f.mu.Unlock()
	}()
	select {
	case f.entered <- verb + " " + unit:
	default:
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, nil
}

func (f *fakeSystemctl) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.changes)
}

// startHelper runs the real helper server code on a socket, with fake
// systemctl. The peer check is faked too: the helper refuses uid 0 as
// the agent, and these tests may run as root.
func startHelper(t *testing.T, f *fakeSystemctl) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "helper.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	const uid = 4242
	srv := &helper.Server{
		AgentUID: uid,
		PeerCred: func(net.Conn) (helper.PeerCred, error) { return helper.PeerCred{UID: uid, PID: 1}, nil },
		Handle:   helper.Systemd{Runner: f, Busy: &helper.Busy{}}.Handle,
		Log:      log.New(io.Discard, "", 0),
	}
	go srv.Serve(l)
	t.Cleanup(func() { l.Close() })
	return path
}

func helperAgent(t *testing.T, e *env, helperPath string) context.CancelFunc {
	t.Helper()
	client := helper.Client{SocketPath: helperPath}
	return runRealAgent(t, e.path, lifecycle.HelperRuntime{Client: client}, client)
}

func (e *env) resolve(t *testing.T, id string) {
	t.Helper()
	err := e.store.InTenantTx(context.Background(), adminA, func(ctx context.Context, tx *store.Tx) error {
		_, err := tx.ResolveOperation(ctx, id, store.StateFailed, "checked by hand")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// S3, forced interleaving across agent sessions, through the real helper
// server code:
//
//  1. Session 1's STOP for a1 blocks inside "systemctl".
//  2. Session 1 goes away; session 2's hello makes the stop uncertain.
//  3. An admin resolves it and asks for START on a1.
//  4. Session 2 sends it; the helper refuses (CONFLICT), so the operation
//     fails with server_busy. Only one action ever runs on a1 at a time.
//  5. A START for a2 meanwhile runs normally.
//  6. The old stop finishes; a new START for a1 succeeds.
func TestHelperExclusionAcrossSessions(t *testing.T) {
	e := setup(t, nil)
	f := newFakeSystemctl()
	unitA1, unitA2 := helper.UnitName(serverA1), helper.UnitName(serverA2)
	gate := make(chan struct{})
	f.gates[unitA1] = gate
	helperPath := startHelper(t, f)

	stopOld := helperAgent(t, e, helperPath)
	stop := e.createOp(t, serverA1, store.ActionStop)
	e.srv.Kick(nodeA)
	select {
	case got := <-f.entered:
		if got != "stop "+unitA1 {
			t.Fatalf("first change %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("the stop never reached systemctl; op %+v, log:\n%s", e.op(t, stop), e.logs)
	}

	// 2. The old agent process goes; its helper call keeps running.
	stopOld()
	before := e.node(t).epoch
	helperAgent(t, e, helperPath)
	waitFor(t, "session 2", func() bool { return e.node(t).epoch > before && e.srv.sessions.epoch(nodeA) > before })
	if got := e.op(t, stop); got.state != "uncertain" || got.code != "session_replaced" {
		t.Fatalf("old stop = %+v", got)
	}

	// 3, 4.
	e.resolve(t, stop)
	busy := e.createOp(t, serverA1, store.ActionStart)
	e.srv.Kick(nodeA)
	waitFor(t, "server_busy", func() bool { return e.op(t, busy).state == "failed" })
	if got := e.op(t, busy); got.code != "server_busy" {
		t.Errorf("start during the old stop = %+v, want failed server_busy", got)
	}

	// 5.
	other := e.createOp(t, serverA2, store.ActionStart)
	e.srv.Kick(nodeA)
	waitFor(t, "a2 succeeded", func() bool { return e.op(t, other).state == "succeeded" })

	// 6.
	close(gate)
	waitFor(t, "old stop done", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.active[unitA1] == 0
	})
	again := e.createOp(t, serverA1, store.ActionStart)
	e.srv.Kick(nodeA)
	waitFor(t, "a1 start succeeded", func() bool { return e.op(t, again).state == "succeeded" })

	want := []string{"stop " + unitA1, "start " + unitA2, "start " + unitA1}
	if got := f.got(); !slices.Equal(got, want) {
		t.Errorf("systemctl ran %v, want %v", got, want)
	}
	if f.maxActive[unitA1] != 1 {
		t.Errorf("%d actions ran at once on a1", f.maxActive[unitA1])
	}
	// The old stop's late answer never changed its (resolved) operation.
	if got := e.op(t, stop); got.state != "uncertain" {
		t.Errorf("old stop later = %+v", got)
	}
}

// End to end: start, stop and restart through the API, the link, the
// real agent and the real helper code. Each succeeds, keeps the actor who
// asked, and the API shows it.
func TestOperationsEndToEnd(t *testing.T) {
	e := setup(t, nil)
	f := newFakeSystemctl()
	helperAgent(t, e, startHelper(t, f))
	waitFor(t, "session", func() bool { return e.srv.sessions.epoch(nodeA) > 0 })

	handler := api.New(api.Config{
		Version:     "test",
		Store:       e.store,
		Lookup:      func(string) (string, error) { return "trevor", nil },
		Log:         log.New(io.Discard, "", 0),
		OnOperation: func(op store.Operation) { e.srv.Kick(op.NodeID) },
	})
	do := func(method, path string) (int, map[string]any) {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader("{}"))
		r.Header.Set("Content-Type", "application/json")
		r = r.WithContext(api.ContextWithPeer(r.Context(), api.Peer{UID: 1000}))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, r)
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s %s: %d %s", method, path, rec.Code, rec.Body)
		}
		return rec.Code, body
	}

	for _, action := range []string{"start", "stop", "restart"} {
		code, op := do("POST", "/api/v1/servers/"+serverA1+"/"+action)
		if code != http.StatusAccepted {
			t.Fatalf("%s: %d %v", action, code, op)
		}
		id := op["id"].(string)
		var got map[string]any
		deadline := time.Now().Add(5 * time.Second)
		for {
			_, got = do("GET", "/api/v1/operations/"+id)
			if got["state"] != "pending" && got["state"] != "running" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: still %v", action, got["state"])
			}
			time.Sleep(20 * time.Millisecond)
		}
		actor, _ := got["actor"].(map[string]any)
		if got["state"] != "succeeded" || actor["name"] != "trevor" {
			t.Errorf("%s: %v", action, got)
		}
	}
	unit := helper.UnitName(serverA1)
	want := []string{"start " + unit, "stop " + unit, "stop " + unit, "start " + unit}
	if got := f.got(); !slices.Equal(got, want) {
		t.Errorf("systemctl ran %v, want %v", got, want)
	}
}
