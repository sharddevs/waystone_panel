// SPDX-License-Identifier: AGPL-3.0-only

package agentlink

import (
	"testing"
	"time"
)

// C4: the 6th takeover in 10 minutes is flagged, the 7th isn't, and once
// the window has emptied a new burst is flagged again.
func TestChurnWindow(t *testing.T) {
	c := newChurn()
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	for i := 1; i <= 7; i++ {
		got := c.note(testNode, now)
		if got != (i == 6) {
			t.Errorf("takeover %d flagged = %t", i, got)
		}
		now = now.Add(time.Minute)
	}
	// Spread out: one a minute keeps the window from ever emptying, so
	// nothing more is flagged however long it goes on.
	for range 30 {
		if c.note(testNode, now) {
			t.Fatal("flagged again while the window never emptied")
		}
		now = now.Add(time.Minute)
	}
	// Quiet for a full window, then a new burst.
	now = now.Add(churnWindow)
	for i := 1; i <= 6; i++ {
		if got := c.note(testNode, now); got != (i == 6) {
			t.Errorf("second burst takeover %d flagged = %t", i, got)
		}
	}
	// Five takeovers spread wider than the window never reach the limit.
	c = newChurn()
	for range 20 {
		if c.note(testNode, now) {
			t.Fatal("flagged takeovers more than 10 minutes apart")
		}
		now = now.Add(churnWindow / 5)
	}
	// Another node has its own count.
	if c.note("0199e1a0-0000-7000-8000-0000000000b1", now) {
		t.Error("a first takeover of another node was flagged")
	}
}
