// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"net/http"
	"sync"
	"time"
)

// The I12 limits.
const (
	// MaxConcurrent is how many requests may run at once. One more gets
	// 503 controller_busy straight away instead of waiting in a queue.
	MaxConcurrent = 32
	// RateLimit POSTs per actor per RateWindow; the next one gets 429.
	RateLimit  = 30
	RateWindow = time.Minute
)

// rateLimiter counts requests per key (an actor) in fixed windows: the
// first request from a key starts a window of length window, and at most
// limit requests are allowed until it ends. Then the next request starts
// a new window. It is safe for concurrent use.
type rateLimiter struct {
	limit  int
	window time.Duration

	mu      sync.Mutex
	windows map[string]*rateWindow
}

type rateWindow struct {
	start time.Time
	count int
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{limit: limit, window: window, windows: make(map[string]*rateWindow)}
}

// allow reports whether key may make one more request at time now, and
// counts it if so. A refused request isn't counted.
//
// So the map can't grow forever, allow also drops every window that has
// already ended. (With a handful of local admins this map stays tiny; it
// matters once there are many actors.)
//
// TODO: lock mu; delete ended windows (now >= start+window); then find or
// start key's window and compare count with limit.
func (l *rateLimiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	// Remove expired windows for every actor.
	for actor, window := range l.windows {
		if !now.Before(window.start.Add(l.window)) {
			delete(l.windows, actor)
		}
	}

	// Find the actor's current window.
	window, exists := l.windows[key]
	if !exists {
		l.windows[key] = &rateWindow{
			start: now,
			count: 1,
		}
		return true
	}

	// Refuse requests once the limit is reached.
	if window.count >= l.limit {
		return false
	}

	window.count++
	return true
}

// limitConcurrency lets at most n requests run through next at the same
// time. A request that finds all n slots taken gets busy straight away,
// without waiting: under load, failing fast is kinder than piling up.
//
// TODO: a buffered channel of size n as the semaphore; a select with a
// default case to try to take a slot without blocking; release the slot
// when next returns (defer).
func limitConcurrency(n int, busy http.Handler, next http.Handler) http.Handler {
	slots := make(chan struct{}, n)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case slots <- struct{}{}:
			defer func() {
				<-slots
			}()

			next.ServeHTTP(w, r)

		default:
			busy.ServeHTTP(w, r)
		}
	})
}
