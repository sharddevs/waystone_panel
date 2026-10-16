// SPDX-License-Identifier: AGPL-3.0-only

package session

import (
	"sync"
	"time"
)

// limiter counts failed sign-ins per key in a sliding window, in memory.
// The controller is one process, so this is the whole count; a restart
// forgets it, which only gives an attacker one fresh window.
type limiter struct {
	mu       sync.Mutex
	window   time.Duration
	failures map[string][]time.Time // oldest first, all within window
}

// sweepAt is how many keys the map may hold before a failure sweeps out
// every key whose failures have all expired.
const sweepAt = 4096

func newLimiter(window time.Duration) *limiter {
	return &limiter{window: window, failures: map[string][]time.Time{}}
}

// blocked reports whether key has max or more failures in the window, and
// if so how long until the oldest of them drops out.
func (l *limiter) blocked(key string, max int, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f := l.live(key, now)
	if len(f) < max {
		return false, 0
	}
	// The oldest failure that keeps the count at max must expire.
	return true, f[len(f)-max].Add(l.window).Sub(now)
}

// fail records a failure for each key and returns the new counts.
func (l *limiter) fail(now time.Time, keys ...string) []int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.failures) >= sweepAt {
		for k := range l.failures {
			l.live(k, now)
		}
	}
	counts := make([]int, len(keys))
	for i, k := range keys {
		l.failures[k] = append(l.live(k, now), now)
		counts[i] = len(l.failures[k])
	}
	return counts
}

// reset forgets key's failures (a successful sign-in for that username).
func (l *limiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failures, key)
}

// live drops key's expired failures and returns the rest. Callers hold mu.
func (l *limiter) live(key string, now time.Time) []time.Time {
	f := l.failures[key]
	i := 0
	for i < len(f) && !f[i].Add(l.window).After(now) {
		i++
	}
	if i == len(f) {
		delete(l.failures, key)
		return nil
	}
	f = f[i:]
	l.failures[key] = f
	return f
}
