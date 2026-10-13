// SPDX-License-Identifier: AGPL-3.0-only

package agentlink

import (
	"context"
	"math/rand/v2"
	"sync"
	"testing"
)

const testNode = "0199e1a0-0000-7000-8000-0000000000a1"

// newTestSession is a cancellable context, as Connect makes one.
func newTestSession() (context.Context, context.CancelCauseFunc) {
	return context.WithCancelCause(context.Background())
}

// R1: 6 arrives before 5. 5 is cancelled at once, 6 stays live.
func TestInstallLowerEpochLoses(t *testing.T) {
	s := newSessions()
	ctx6, cancel6 := newTestSession()
	ctx5, cancel5 := newTestSession()
	if won, replaced := s.install(testNode, 6, cancel6); !won || replaced {
		t.Fatalf("install 6 = %t, %t", won, replaced)
	}
	if won, _ := s.install(testNode, 5, cancel5); won {
		t.Fatal("install 5 after 6 won")
	}
	if context.Cause(ctx5) != errReplaced {
		t.Errorf("5's cause = %v, want errReplaced", context.Cause(ctx5))
	}
	if ctx6.Err() != nil {
		t.Error("6 was cancelled by a lower epoch")
	}
	if got := s.epoch(testNode); got != 6 {
		t.Errorf("live epoch = %d, want 6", got)
	}
	// The same epoch twice can't happen (the database hands out each
	// once), but if it did, the second would lose too.
	ctxSame, cancelSame := newTestSession()
	if won, _ := s.install(testNode, 6, cancelSame); won || ctxSame.Err() == nil || ctx6.Err() != nil {
		t.Error("a second 6 replaced the first")
	}
}

// R1: a higher epoch replaces and cancels the old one, and the old one's
// late cleanup leaves its successor alone.
func TestOldSessionExitKeepsNewer(t *testing.T) {
	s := newSessions()
	ctx5, cancel5 := newTestSession()
	ctx6, cancel6 := newTestSession()
	s.install(testNode, 5, cancel5)
	if won, replaced := s.install(testNode, 6, cancel6); !won || !replaced {
		t.Fatalf("install 6 = %t, %t, want won and replaced", won, replaced)
	}
	if context.Cause(ctx5) != errReplaced {
		t.Errorf("5's cause = %v, want errReplaced", context.Cause(ctx5))
	}
	s.remove(testNode, 5)
	if got := s.epoch(testNode); got != 6 || ctx6.Err() != nil {
		t.Errorf("after 5 exits: live epoch %d, 6 cancelled %t", got, ctx6.Err() != nil)
	}
	s.remove(testNode, 6)
	if got := s.epoch(testNode); got != 0 {
		t.Errorf("after 6 exits: live epoch %d, want none", got)
	}
}

// R1: 50 installs racing in random order. The highest is live, every
// other one is cancelled. Run with -race.
func TestConcurrentInstalls(t *testing.T) {
	s := newSessions()
	const n = 50
	ctxs := make([]context.Context, n+1)
	cancels := make([]context.CancelCauseFunc, n+1)
	for e := 1; e <= n; e++ {
		ctxs[e], cancels[e] = newTestSession()
	}
	order := rand.Perm(n)
	var wg sync.WaitGroup
	for _, i := range order {
		e := i + 1
		wg.Go(func() { s.install(testNode, int64(e), cancels[e]) })
	}
	wg.Wait()
	if got := s.epoch(testNode); got != n {
		t.Fatalf("live epoch = %d, want %d", got, n)
	}
	for e := 1; e <= n; e++ {
		if cancelled := ctxs[e].Err() != nil; cancelled != (e != n) {
			t.Errorf("epoch %d cancelled = %t", e, cancelled)
		}
	}
}
