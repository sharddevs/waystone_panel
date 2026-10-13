// SPDX-License-Identifier: AGPL-3.0-only

package agent

import "time"

// R6 backoff. After the controller says session_replaced, another agent
// for this node is live, so this one waits long: 60 s doubling to 10 min.
// After anything else it retries fast: 1 s doubling to 30 s. Every wait
// gets ±20% jitter, so two agents don't stay in lockstep.
const (
	normalFirst   = 1 * time.Second
	normalMax     = 30 * time.Second
	replacedFirst = 60 * time.Second
	replacedMax   = 10 * time.Minute
	jitter        = 0.2
)

type backoff struct {
	rand     func() float64 // in [0, 1)
	replaced bool           // which sequence we are in
	n        int            // waits so far in it
}

// next returns the wait before the next connection attempt. Switching
// between the two sequences starts the new one from the beginning.
func (b *backoff) next(replaced bool) time.Duration {
	if replaced != b.replaced {
		b.replaced = replaced
		b.n = 0
	}
	d, limit := normalFirst, normalMax
	if replaced {
		d, limit = replacedFirst, replacedMax
	}
	for i := 0; i < b.n && d < limit; i++ {
		d *= 2
	}
	if d >= limit {
		d = limit
	} else {
		b.n++
	}
	factor := 1 - jitter + 2*jitter*b.rand()
	return time.Duration(float64(d) * factor)
}

// reset starts over, after a session that lasted a full report interval.
func (b *backoff) reset() { b.n = 0 }
