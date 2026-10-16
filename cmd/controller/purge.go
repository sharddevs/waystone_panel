// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"time"
)

// purgeEvery is how often the controller deletes sessions that can't be
// used any more (I4).
const purgeEvery = time.Hour

// purger is what purgeSessions needs; *store.Store is the real one.
type purger interface {
	PurgeSessions(ctx context.Context) (int, error)
}

// purgeSessions deletes expired and idle sessions once straight away and
// then every interval, until stop is closed. A failure is logged and
// tried again next time: sessions past their expiry are refused anyway,
// this only keeps the table from growing.
func purgeSessions(p purger, every time.Duration, stop <-chan struct{}, logf func(string, ...any)) {
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		n, err := p.PurgeSessions(ctx)
		cancel()
		if err != nil {
			logf("session purge: %v", err)
		} else if n > 0 {
			logf("session purge: %d expired or idle session(s) deleted", n)
		}
		t.Reset(every)
	}
}
