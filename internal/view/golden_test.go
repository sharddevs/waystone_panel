// SPDX-License-Identifier: AGPL-3.0-only

package view

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sharddevs/waystone_panel/internal/store"
)

// update rewrites the golden files: go test ./internal/view -run Golden -update
var update = flag.Bool("update", false, "rewrite testdata/*.json")

// golden compares v's JSON with testdata/name, or rewrites it with
// -update. The web client's contract test (web/src/api.contract.test.ts)
// decodes the same files, so a shape change here fails there too (Q9).
func golden(t *testing.T, name string, v any) {
	t.Helper()
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test ./internal/view -run Golden -update)", err)
	}
	if !bytes.Equal(b.Bytes(), want) {
		t.Errorf("%s changed; if that's intended, run with -update and update the web client:\n%s", path, b.String())
	}
}

// goldenServers covers every field and the omitempty cases: a ready
// server with a version, an unreachable node with stale states
// (last_reported) and every action blocked with its reason, and a server
// never observed at all (null times, no version).
func goldenServers() []Server {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { t := now.Add(-d); return &t }
	version := "26.3"
	onlyStartStop := func(a store.Action) bool { return a != store.ActionRestart }
	return []Server{
		ServerOf(store.Server{
			ID: "0199e1a0-0000-7000-8000-000000000a01", NodeID: "0199e1a0-0000-7000-8000-0000000000a1",
			Name: "Survival World", Profile: "minecraft-java", ProfileVersion: &version, Runtime: "systemd", DesiredState: "running",
			NodeName: "homelab-1", NodeLastSeenAt: at(5 * time.Second),
			ProcessState: "running", ProcessSince: at(12 * time.Minute), ProcessObservedAt: at(8 * time.Second),
			Readiness: "ready", ReadinessSince: at(11 * time.Minute), ReadinessObservedAt: at(8 * time.Second),
		}, all, now),
		ServerOf(store.Server{
			ID: "0199e1a0-0000-7000-8000-000000000a02", NodeID: "0199e1a0-0000-7000-8000-0000000000a2",
			Name: "Modded", Profile: "minecraft-java", Runtime: "systemd", DesiredState: "running",
			NodeName: "pi-node", NodeLastSeenAt: at(4 * time.Minute),
			ProcessState: "running", ProcessSince: at(time.Hour), ProcessObservedAt: at(4 * time.Minute),
			Readiness: "not_ready", ReadinessSince: at(time.Hour), ReadinessObservedAt: at(4 * time.Minute),
			Blocking: store.StateUncertain,
		}, onlyStartStop, now),
		ServerOf(store.Server{
			ID: "0199e1a0-0000-7000-8000-000000000a03", NodeID: "0199e1a0-0000-7000-8000-0000000000a1",
			Name: "Test", Profile: "minecraft-java", Runtime: "systemd", DesiredState: "stopped",
			NodeName: "homelab-1", NodeLastSeenAt: at(5 * time.Second),
			ProcessState: "unknown", Readiness: "unknown", Blocking: store.StatePending,
		}, all, now),
	}
}

// goldenOperations: a restart running in its stopping phase, a failed
// stop with its error, and an uncertain start that was resolved.
func goldenOperations() []Operation {
	t0 := time.Date(2026, 10, 16, 11, 30, 2, 0, time.UTC)
	at := func(d time.Duration) *time.Time { t := t0.Add(d); return &t }
	trevor := store.Actor{Type: store.ActorUser, ID: "0199e1a0-0000-7000-8000-00000000c001", Name: "Trevor"}
	local := store.Actor{Type: store.ActorLocalUser, ID: "1000", Name: "trevor"}
	return []Operation{
		OperationOf(store.Operation{ID: "0199e1a0-0000-7000-8000-00000000d003", ResourceID: "0199e1a0-0000-7000-8000-000000000a01",
			NodeID: "0199e1a0-0000-7000-8000-0000000000a1", Action: store.ActionRestart, State: store.StateRunning, Phase: "stopping",
			Actor: trevor, RequestedAt: t0.Add(20 * time.Minute), StartedAt: at(20 * time.Minute)}),
		OperationOf(store.Operation{ID: "0199e1a0-0000-7000-8000-00000000d002", ResourceID: "0199e1a0-0000-7000-8000-000000000a01",
			NodeID: "0199e1a0-0000-7000-8000-0000000000a1", Action: store.ActionStop, State: store.StateFailed,
			Actor: local, ErrorCode: "server_busy", ErrorSummary: "systemd was busy with this server.",
			RequestedAt: t0, StartedAt: at(0), FinishedAt: at(7100 * time.Millisecond)}),
		OperationOf(store.Operation{ID: "0199e1a0-0000-7000-8000-00000000d001", ResourceID: "0199e1a0-0000-7000-8000-000000000a02",
			NodeID: "0199e1a0-0000-7000-8000-0000000000a2", Action: store.ActionStart, State: store.StateUncertain,
			Actor: trevor, RequestedAt: t0.Add(-time.Hour), StartedAt: at(-time.Hour),
			Resolution: &store.Resolution{At: t0.Add(-30 * time.Minute), Outcome: store.StateFailed, By: trevor, Note: "Checked the console;\nit never came up."}}),
	}
}

// The shapes the handlers send (service/handlers.go): a list is wrapped
// in {"servers": …} or {"operations": …}.
func TestGolden(t *testing.T) {
	servers := goldenServers()
	golden(t, "server.json", servers[0])
	golden(t, "servers.json", map[string]any{"servers": servers})
	ops := goldenOperations()
	golden(t, "operation.json", ops[1])
	golden(t, "operations.json", map[string]any{"operations": ops})
}
