// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package agentlink

// The real agent (internal/agent) against the real link, in one process:
// requirement 1's backoff, the agent's own peer check, and the stage 4b
// goal end to end, down to what the API shows.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	helperv1 "github.com/sharddevs/waystone_panel/gen/waystone/helper/v1"
	"github.com/sharddevs/waystone_panel/internal/agent"
	"github.com/sharddevs/waystone_panel/internal/api"
)

// fakeHelper answers STATUS per server; answer can be swapped mid-test.
type fakeHelper struct {
	mu     sync.Mutex
	answer map[string]*helperv1.Response // missing: a transport error
}

func (f *fakeHelper) set(answer map[string]*helperv1.Response) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answer = answer
}

func (f *fakeHelper) Do(_ context.Context, req *helperv1.Request) (*helperv1.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.answer[req.GetServerId()]; ok {
		return r, nil
	}
	return nil, errors.New("helper unavailable")
}

// sleeps records the agent's backoff waits and stops the agent at the
// first one, so a test never really waits a minute.
type sleeps struct {
	mu    sync.Mutex
	waits []time.Duration
	stop  context.CancelFunc
}

func (s *sleeps) sleep(ctx context.Context, d time.Duration) error {
	s.mu.Lock()
	s.waits = append(s.waits, d)
	s.mu.Unlock()
	s.stop()
	return context.Canceled
}

func (s *sleeps) got() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.waits...)
}

// runAgent starts the real agent against e's socket. It returns a
// channel that closes when Run returns, and the cancel that stops it.
func runAgent(t *testing.T, e *env, h agent.Helper, controllerUID uint32, sl *sleeps) (<-chan struct{}, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cfg := agent.Config{
		ControllerSocket: e.path,
		ControllerUID:    controllerUID,
		NodeID:           nodeA,
		Version:          "test",
		Helper:           h,
		Log:              log.New(io.Discard, "", 0),
	}
	if sl != nil {
		sl.stop = cancel
		cfg.Sleep = sl.sleep
	}
	a, err := agent.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.Run(ctx)
	}()
	t.Cleanup(func() { cancel(); <-done })
	return done, cancel
}

func within(d, base time.Duration) bool {
	return d >= time.Duration(float64(base)*0.8) && d <= time.Duration(float64(base)*1.2)
}

// Requirement 1, end to end: a real agent whose session is taken over
// gets ABORTED session_replaced from the real server, and so its next
// wait is the long one (60 s ± 20%), not the normal 1 s.
func TestReplacedAgentBacksOffLong(t *testing.T) {
	e := setup(t, nil)
	sl := &sleeps{}
	done, _ := runAgent(t, e, &fakeHelper{}, uint32(os.Getuid()), sl)
	waitFor(t, "agent session", func() bool { return e.srv.sessions.epoch(nodeA) == 1 })

	// Another agent for the same node says hello.
	hello(t, e.stream(t, e.client(t)))
	<-done
	waits := sl.got()
	if len(waits) != 1 || !within(waits[0], time.Minute) {
		t.Fatalf("replaced agent waits %v, want one wait of 60 s ± 20%%", waits)
	}
}

// The contrast: a controller shutting down (UNAVAILABLE) gets the normal
// 1 s backoff.
func TestShutdownAgentBacksOffShort(t *testing.T) {
	e := setup(t, nil)
	sl := &sleeps{}
	done, _ := runAgent(t, e, &fakeHelper{}, uint32(os.Getuid()), sl)
	waitFor(t, "agent session", func() bool { return e.srv.sessions.epoch(nodeA) == 1 })
	e.srv.CancelSessions()
	<-done
	if waits := sl.got(); len(waits) != 1 || !within(waits[0], time.Second) {
		t.Fatalf("agent waits %v after a shutdown, want one wait of 1 s ± 20%%", waits)
	}
}

// R2, the agent's side, over a real socket: the listener isn't the
// controller's uid, so the agent never says hello.
func TestAgentRefusesWrongControllerUID(t *testing.T) {
	e := setup(t, nil)
	sl := &sleeps{}
	done, _ := runAgent(t, e, &fakeHelper{}, uint32(os.Getuid())+4242, sl)
	<-done
	if waits := sl.got(); len(waits) != 1 || !within(waits[0], time.Second) {
		t.Errorf("agent waits %v, want the normal 1 s", waits)
	}
	if n := e.node(t); n.epoch != 0 {
		t.Errorf("epoch = %d: the agent said hello to the wrong uid", n.epoch)
	}
}

// The stage 4b goal, in one process: hello, reports every interval, the
// API shows the real state; a helper that fails everything still gets a
// report through (all unknown); and once the agent stops, the API shows
// unknown after 46 s with the last value in last_reported.
func TestEndToEnd(t *testing.T) {
	e := setup(t, func(c *Config) { c.ReportInterval = time.Second })
	h := &fakeHelper{}
	h.set(map[string]*helperv1.Response{
		serverA1: {RunState: helperv1.RunState_RUN_STATE_RUNNING, NativeState: "active/running"},
		serverA2: {ErrorCode: helperv1.ErrorCode_ERROR_CODE_NOT_FOUND},
	})
	done, stop := runAgent(t, e, h, uint32(os.Getuid()), nil)
	waitFor(t, "two reports", func() bool { _, seq := e.serverState(t, serverA1); return seq >= 2 })

	now := time.Now()
	handler := api.New(api.Config{
		Version: "test",
		Store:   e.store,
		Lookup:  func(string) (string, error) { return "admin a", nil },
		Log:     log.New(io.Discard, "", 0),
		Now:     func() time.Time { return now },
	})
	type process struct {
		State        string `json:"state"`
		LastReported string `json:"last_reported"`
	}
	servers := func() map[string]process {
		t.Helper()
		r := httptest.NewRequest("GET", "/api/v1/servers", nil)
		r = r.WithContext(api.ContextWithPeer(r.Context(), api.Peer{UID: 1000}))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, r)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET servers: %d %s", rec.Code, rec.Body)
		}
		var body struct {
			Servers []struct {
				ID      string  `json:"id"`
				Process process `json:"process"`
			} `json:"servers"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		out := map[string]process{}
		for _, s := range body.Servers {
			out[s.ID] = s.Process
		}
		return out
	}
	now = time.Now()
	got := servers()
	if got[serverA1].State != "running" || got[serverA2].State != "unknown" {
		t.Errorf("live: %+v", got)
	}

	// The helper goes away: the reports keep coming, all unknown.
	h.set(nil)
	_, seq := e.serverState(t, serverA1)
	waitFor(t, "an all-unknown report", func() bool {
		st, s := e.serverState(t, serverA1)
		return st == "unknown" && s > seq
	})
	h.set(map[string]*helperv1.Response{
		serverA1: {RunState: helperv1.RunState_RUN_STATE_RUNNING},
		serverA2: {RunState: helperv1.RunState_RUN_STATE_STOPPED},
	})
	waitFor(t, "running again", func() bool { st, _ := e.serverState(t, serverA1); return st == "running" })

	// The agent stops. 46 s later the API says unknown and keeps the
	// last report visible; the stored state isn't rewritten.
	stop()
	<-done
	now = time.Now().Add(46 * time.Second)
	got = servers()
	if got[serverA1].State != "unknown" || got[serverA1].LastReported != "running" {
		t.Errorf("46 s after the agent stopped: %+v", got[serverA1])
	}
	if st, _ := e.serverState(t, serverA1); st != "running" {
		t.Errorf("stored a1 = %s, want running", st)
	}
}

// R6: a session that lasts a full report interval resets the backoff.
// Three failed dials grow the waits to 1, 2 and 4 s; then a healthy
// session ends, and the next wait is 1 s again, not 8.
func TestHealthySessionResetsBackoff(t *testing.T) {
	e := setup(t, func(c *Config) { c.ReportInterval = time.Second })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	var waits []time.Duration
	dials := 0
	a, err := agent.New(agent.Config{
		ControllerSocket: e.path,
		ControllerUID:    uint32(os.Getuid()),
		NodeID:           nodeA,
		Helper:           &fakeHelper{},
		Log:              log.New(io.Discard, "", 0),
		Dial: func(ctx context.Context) (net.Conn, error) {
			mu.Lock()
			dials++
			n := dials
			mu.Unlock()
			if n <= 3 {
				return nil, errors.New("controller not there yet")
			}
			var d net.Dialer
			return d.DialContext(ctx, "unix", e.path)
		},
		Sleep: func(ctx context.Context, d time.Duration) error {
			mu.Lock()
			defer mu.Unlock()
			waits = append(waits, d)
			if len(waits) == 4 {
				cancel()
				return context.Canceled
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.Run(ctx)
	}()
	waitFor(t, "two reports", func() bool { _, seq := e.serverState(t, serverA1); return seq >= 2 })
	e.srv.CancelSessions()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if len(waits) != 4 {
		t.Fatalf("waits = %v", waits)
	}
	for i, base := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, time.Second} {
		if !within(waits[i], base) {
			t.Errorf("wait %d = %s, want %s ± 20%%", i+1, waits[i], base)
		}
	}
}
