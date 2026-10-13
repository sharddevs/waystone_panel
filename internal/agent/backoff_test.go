// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"testing"
	"time"
)

// R6: both sequences, exactly, with the jitter at its midpoint.
func TestBackoffSequences(t *testing.T) {
	b := backoff{rand: func() float64 { return 0.5 }}
	s := time.Second
	normal := []time.Duration{1 * s, 2 * s, 4 * s, 8 * s, 16 * s, 30 * s, 30 * s}
	for i, want := range normal {
		if got := b.next(false); got != want {
			t.Errorf("normal wait %d = %s, want %s", i+1, got, want)
		}
	}
	// session_replaced switches to the long sequence, from its start.
	m := time.Minute
	replaced := []time.Duration{1 * m, 2 * m, 4 * m, 8 * m, 10 * m, 10 * m}
	for i, want := range replaced {
		if got := b.next(true); got != want {
			t.Errorf("replaced wait %d = %s, want %s", i+1, got, want)
		}
	}
	// Any other error goes back to the short one, from its start.
	if got := b.next(false); got != s {
		t.Errorf("normal after replaced = %s, want 1s", got)
	}
	// A healthy session resets the sequence.
	b.next(false)
	b.reset()
	if got := b.next(false); got != s {
		t.Errorf("after reset = %s, want 1s", got)
	}
	b.next(true)
	b.next(true)
	b.reset()
	if got := b.next(true); got != m {
		t.Errorf("replaced after reset = %s, want 1m", got)
	}
}

// R6: jitter is ±20% of every wait, never more.
func TestBackoffJitter(t *testing.T) {
	for _, r := range []float64{0, 0.25, 0.999999} {
		b := backoff{rand: func() float64 { return r }}
		for range 8 {
			for _, replaced := range []bool{false, true} {
				base := backoff{rand: func() float64 { return 0.5 }, replaced: b.replaced, n: b.n}
				if replaced != b.replaced {
					base = backoff{rand: base.rand, replaced: replaced}
				}
				want := base.next(replaced)
				got := b.next(replaced)
				lo, hi := time.Duration(float64(want)*0.8), time.Duration(float64(want)*1.2)
				if got < lo || got > hi {
					t.Errorf("rand %v: wait %s outside [%s, %s]", r, got, lo, hi)
				}
			}
		}
	}
	b := backoff{rand: func() float64 { return 0 }}
	if got := b.next(true); got != 48*time.Second {
		t.Errorf("replaced at -20%% = %s, want 48s", got)
	}
}
