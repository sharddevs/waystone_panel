// SPDX-License-Identifier: AGPL-3.0-only

package agentlink

import (
	"sync"
	"time"
)

// logLimit lets one log line per key through each interval, so a confused
// or hostile agent can't flood the journal (R5).
type logLimit struct {
	mu    sync.Mutex
	every time.Duration
	now   func() time.Time
	last  map[string]time.Time
	// skipped counts, per key, the lines held back since the last one.
	skipped map[string]int
}

// maxLogKeys bounds the map. Keys come from agent input (server IDs), so
// old ones are dropped once there are too many.
const maxLogKeys = 1024

func newLogLimit(every time.Duration, now func() time.Time) *logLimit {
	return &logLimit{every: every, now: now, last: make(map[string]time.Time), skipped: make(map[string]int)}
}

func (l *logLimit) allow(key string) bool {
	ok, _ := l.allowCount(key)
	return ok
}

// allowCount is allow, and also how many lines for key were held back
// since the last one let through.
func (l *logLimit) allowCount(key string) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if t, ok := l.last[key]; ok && now.Sub(t) < l.every {
		l.skipped[key]++
		return false, 0
	}
	if len(l.last) >= maxLogKeys {
		for k, t := range l.last {
			if now.Sub(t) >= l.every {
				delete(l.last, k)
				delete(l.skipped, k)
			}
		}
		if len(l.last) >= maxLogKeys {
			return false, 0
		}
	}
	l.last[key] = now
	n := l.skipped[key]
	delete(l.skipped, key)
	return true, n
}
