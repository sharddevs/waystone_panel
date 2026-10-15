// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"sync"
	"time"

	agentv1 "github.com/sharddevs/waystone_panel/gen/waystone/agent/v1"
)

// probeTimeout bounds one readiness probe.
const probeTimeout = 3 * time.Second

// Prober is the profile's readiness probe (D7): one small interface, no
// general health-check framework. minecraft.StatusPing implements it.
// Probe returns nil only when the game on port answers its players.
type Prober interface {
	Probe(ctx context.Context, port uint32) error
}

// readiness fills in each entry's readiness from this sweep's process
// states (D6):
//
//   - RUNNING with a game port: the probe decides, READY or NOT_READY.
//   - RUNNING without a port: UNKNOWN. The agent never guesses a port.
//   - UNKNOWN process state: UNKNOWN; nobody knows what to probe.
//   - anything else: NOT_READY, without probing.
//
// The probes run at the same time, each under its own deadline, so one
// server that hangs can't hold up the report.
func (a *Agent) readiness(ctx context.Context, entries []*agentv1.ReportEntry, ports map[string]uint32) {
	var wg sync.WaitGroup
	for _, e := range entries {
		switch e.GetProcessState() {
		case agentv1.ProcessState_PROCESS_STATE_RUNNING:
			port, ok := ports[e.GetServerId()]
			if !ok {
				e.Readiness = agentv1.Readiness_READINESS_UNKNOWN
				continue
			}
			wg.Go(func() {
				ctx, cancel := context.WithTimeout(ctx, a.probeTimeout)
				defer cancel()
				if a.prober.Probe(ctx, port) == nil && ctx.Err() == nil {
					e.Readiness = agentv1.Readiness_READINESS_READY
				} else {
					e.Readiness = agentv1.Readiness_READINESS_NOT_READY
				}
			})
		case agentv1.ProcessState_PROCESS_STATE_UNKNOWN:
			e.Readiness = agentv1.Readiness_READINESS_UNKNOWN
		default:
			e.Readiness = agentv1.Readiness_READINESS_NOT_READY
		}
	}
	wg.Wait()
}
