// SPDX-License-Identifier: AGPL-3.0-only

package helper

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"

	"github.com/sharddevs/waystone_panel/internal/ids"
)

// Stage 7b, audit H4: servers provisioned before 7b have a unit without
// the console socket. UpgradeUnits rewrites them in place from what each
// unit already says, and never restarts a game: one that is running keeps
// its old stdin, and gets its console at its next start. It runs once per
// deploy, as root, from the command line (waystone-helper -upgrade-units),
// not through the socket.

var (
	unitFileName = regexp.MustCompile(`^waystone-srv-([0-9a-f-]{36})\.service$`)
	execStart    = regexp.MustCompile(`(?m)^ExecStart=(\S+) -Xms([0-9]+)M -Xmx([0-9]+)M -jar server\.jar nogui$`)
	userLine     = regexp.MustCompile(`(?m)^User=(\S+)$`)
)

// UpgradeResult is what UpgradeUnits did to one server's units.
type UpgradeResult struct {
	ServerID string
	Changed  bool
}

// UpgradeUnits brings every Waystone unit in units.Dir up to this build's
// templates. A unit is read for its user, runtime and heap, each checked
// as provisioning would (the user is the server's own, the runtime one of
// runtimes, the heap in range); a unit that fails is an error, and nothing
// after it is touched. daemon-reload runs once at the end if anything
// changed, and always after an error.
func UpgradeUnits(ctx context.Context, units UnitFiles, runner Runner, runtimes map[string]string) ([]UpgradeResult, error) {
	entries, err := os.ReadDir(units.Dir)
	if err != nil {
		return nil, err
	}
	var out []UpgradeResult
	for _, e := range entries {
		m := unitFileName.FindStringSubmatch(e.Name())
		if m == nil || ids.Check(m[1]) != nil {
			continue
		}
		r, err := upgradeServer(units, m[1], runtimes)
		if err != nil {
			// Whatever was written before the failure still needs systemd
			// to see it, or a later run would find nothing to change.
			return out, errors.Join(fmt.Errorf("server %s: %w", m[1], err), daemonReload(ctx, runner))
		}
		out = append(out, r)
	}
	return out, reloadIfChanged(ctx, runner, out)
}

// upgradeServer rewrites one server's units, socket first. Not
// writeUnits: one daemon-reload at the end is enough.
func upgradeServer(units UnitFiles, serverID string, runtimes map[string]string) (UpgradeResult, error) {
	params, err := unitParamsOf(units, serverID, runtimes)
	if err != nil {
		return UpgradeResult{}, err
	}
	socket, err := RenderConsoleSocket(serverID)
	if err != nil {
		return UpgradeResult{}, err
	}
	service, err := RenderUnit(params)
	if err != nil {
		return UpgradeResult{}, err
	}
	socketChanged, err := units.Write(ConsoleSocketName(serverID), socket)
	if err != nil {
		return UpgradeResult{}, err
	}
	unitChanged, err := units.Write(UnitName(serverID), service)
	if err != nil {
		return UpgradeResult{}, err
	}
	return UpgradeResult{ServerID: serverID, Changed: socketChanged || unitChanged}, nil
}

func reloadIfChanged(ctx context.Context, runner Runner, results []UpgradeResult) error {
	for _, r := range results {
		if r.Changed {
			return daemonReload(ctx, runner)
		}
	}
	return nil
}

// unitParamsOf reads back the values provisioning put into a server's
// unit.
func unitParamsOf(units UnitFiles, serverID string, runtimes map[string]string) (UnitParams, error) {
	if err := units.Check(UnitName(serverID)); err != nil {
		return UnitParams{}, err
	}
	if err := units.Check(ConsoleSocketName(serverID)); err != nil {
		return UnitParams{}, err
	}
	content, err := os.ReadFile(units.path(UnitName(serverID)))
	if err != nil {
		return UnitParams{}, err
	}
	if !bytes.HasPrefix(content, []byte(UnitMarker)) {
		return UnitParams{}, fmt.Errorf("%w: unit is not managed by Waystone", ErrConflict)
	}
	user := userLine.FindAllSubmatch(content, -1)
	exec := execStart.FindAllSubmatch(content, -1)
	if len(user) != 1 || len(exec) != 1 {
		return UnitParams{}, errors.New("unit doesn't have exactly one User= and one ExecStart= Waystone wrote")
	}
	if string(user[0][1]) != ServerUser(serverID) {
		return UnitParams{}, errors.New("unit runs as another user")
	}
	java := string(exec[0][1])
	known := false
	for _, path := range runtimes {
		known = known || path == java
	}
	if !known {
		return UnitParams{}, fmt.Errorf("unit runs %q, which isn't a configured runtime", java)
	}
	heap, err := strconv.ParseUint(string(exec[0][3]), 10, 32)
	if err != nil || string(exec[0][2]) != string(exec[0][3]) || heap < MinHeapMiB || heap > MaxHeapMiB {
		return UnitParams{}, errors.New("unit's heap is out of range")
	}
	return UnitParams{
		ServerID:     serverID,
		User:         ServerUser(serverID),
		Java:         java,
		HeapMiB:      uint32(heap),
		MemoryMaxMiB: uint32(heap) + JVMOverheadMiB,
	}, nil
}
