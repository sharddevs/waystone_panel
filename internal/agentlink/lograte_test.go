// SPDX-License-Identifier: AGPL-3.0-only

package agentlink

import (
	"fmt"
	"testing"
	"time"
)

// R5: one line per key per minute, and the key map can't grow without
// bound.
func TestLogLimit(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	l := newLogLimit(time.Minute, func() time.Time { return now })
	if !l.allow("a") || l.allow("a") || !l.allow("b") {
		t.Fatal("first per key should pass, the repeat shouldn't")
	}
	now = now.Add(59 * time.Second)
	if l.allow("a") {
		t.Error("allowed again within the minute")
	}
	now = now.Add(time.Second)
	if !l.allow("a") {
		t.Error("not allowed after a minute")
	}
	for i := range 2 * maxLogKeys {
		l.allow(fmt.Sprint(i))
	}
	if len(l.last) > maxLogKeys {
		t.Errorf("%d keys kept, at most %d", len(l.last), maxLogKeys)
	}
}
