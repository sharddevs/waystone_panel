// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakePurger struct {
	mu    sync.Mutex
	calls int
	ran   chan struct{}
}

func (f *fakePurger) PurgeSessions(context.Context) (int, error) {
	f.mu.Lock()
	f.calls++
	n := f.calls
	f.mu.Unlock()
	f.ran <- struct{}{}
	if n == 2 {
		return 0, errors.New("database down")
	}
	return 3, nil
}

// The purge runs at once, then on every tick, keeps going after a
// failure, and stops when told.
func TestPurgeSessions(t *testing.T) {
	f := &fakePurger{ran: make(chan struct{}, 10)}
	var mu sync.Mutex
	var logs []string
	logf := func(format string, args ...any) {
		mu.Lock()
		logs = append(logs, fmt.Sprintf(format, args...))
		mu.Unlock()
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		purgeSessions(f, 10*time.Millisecond, stop, logf)
		close(done)
	}()
	for i := 0; i < 3; i++ {
		select {
		case <-f.ran:
		case <-time.After(5 * time.Second):
			t.Fatalf("purge %d never ran", i+1)
		}
	}
	close(stop)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("purgeSessions didn't stop")
	}
	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "3 expired or idle session(s) deleted") || !strings.Contains(joined, "database down") {
		t.Errorf("logs: %q", joined)
	}
}
