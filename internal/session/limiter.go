// SPDX-License-Identifier: AGPL-3.0-only

package session

import (
	"sync"
	"time"
)

// limiter counts sign-in attempts per key in a sliding window, in memory.
// An attempt is counted before its password is checked and taken back
// if it succeeds or can't be decided, so what stays counted are the
// failures. The controller is one process, so this is the whole count; a restart
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

// limit is one key and how many attempts it may have in the window.
type limit struct {
	key, name string
	max       int
}

// attempt counts one sign-in attempt against every limit, or none. Under
// one lock it refuses if any key already has max attempts in the window,
// naming that limit and how long until its oldest attempt that matters
// drops out; otherwise it records now for every key and returns their new
// counts. Counting before the password is checked is what stops parallel
// guesses: each one takes its place in the count before it hashes, so at
// most max of them can ever be in flight (A1).
func (l *limiter) attempt(now time.Time, limits ...limit) (counts []int, refused string, wait time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, lim := range limits {
		if f := l.live(lim.key, now); len(f) >= lim.max {
			// The oldest attempt that keeps the count at max must expire.
			return nil, lim.name, f[len(f)-lim.max].Add(l.window).Sub(now)
		}
	}
	if len(l.failures) >= sweepAt {
		for k := range l.failures {
			l.live(k, now)
		}
	}
	counts = make([]int, len(limits))
	for i, lim := range limits {
		l.failures[lim.key] = append(l.live(lim.key, now), now)
		counts[i] = len(l.failures[lim.key])
	}
	return counts, "", 0
}

// refund takes back an attempt recorded at at for each key: the sign-in
// couldn't be decided (a 503), or it succeeded and shouldn't count.
func (l *limiter) refund(at time.Time, keys ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, k := range keys {
		f := l.failures[k]
		for i := len(f) - 1; i >= 0; i-- {
			if f[i].Equal(at) {
				f = append(f[:i:i], f[i+1:]...)
				break
			}
		}
		if len(f) == 0 {
			delete(l.failures, k)
		} else {
			l.failures[k] = f
		}
	}
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
