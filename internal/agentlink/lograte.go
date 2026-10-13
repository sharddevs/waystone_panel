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
}

// maxLogKeys bounds the map. Keys come from agent input (server IDs), so
// old ones are dropped once there are too many.
const maxLogKeys = 1024

func newLogLimit(every time.Duration, now func() time.Time) *logLimit {
	return &logLimit{every: every, now: now, last: make(map[string]time.Time)}
}

func (l *logLimit) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if t, ok := l.last[key]; ok && now.Sub(t) < l.every {
		return false
	}
	if len(l.last) >= maxLogKeys {
		for k, t := range l.last {
			if now.Sub(t) >= l.every {
				delete(l.last, k)
			}
		}
		if len(l.last) >= maxLogKeys {
			return false
		}
	}
	l.last[key] = now
	return true
}
