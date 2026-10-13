// SPDX-License-Identifier: AGPL-3.0-only

package agentlink

import (
	"sync"
	"time"
)

// C4: more than churnLimit takeovers of one node within churnWindow is
// flagged once, in the audit log and with a warning.
const (
	churnWindow = 10 * time.Minute
	churnLimit  = 5
)

// churn counts takeovers per node in a sliding window, in memory only: a
// controller restart starts the count again.
type churn struct {
	mu    sync.Mutex
	nodes map[string]*churnWindowState
}

type churnWindowState struct {
	times   []time.Time
	flagged bool
}

func newChurn() *churn {
	return &churn{nodes: make(map[string]*churnWindowState)}
}

// note records one takeover at now and reports whether it is the one to
// flag: the first takeover above the limit. Nothing more is flagged until
// the window has emptied completely.
func (c *churn) note(node string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	w := c.nodes[node]
	if w == nil {
		w = &churnWindowState{}
		c.nodes[node] = w
	}
	kept := w.times[:0]
	for _, t := range w.times {
		if now.Sub(t) < churnWindow {
			kept = append(kept, t)
		}
	}
	w.times = kept
	if len(w.times) == 0 {
		w.flagged = false
	}
	w.times = append(w.times, now)
	if len(w.times) > churnLimit && !w.flagged {
		w.flagged = true
		return true
	}
	return false
}
