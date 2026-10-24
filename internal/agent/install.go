// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/sharddevs/waystone_panel/gen/waystone/agent/v1"
	helperv1 "github.com/sharddevs/waystone_panel/gen/waystone/helper/v1"
	"github.com/sharddevs/waystone_panel/internal/helper"
	"github.com/sharddevs/waystone_panel/internal/lifecycle"
	"github.com/sharddevs/waystone_panel/internal/profile/minecraft"
)

// Stage 6c-2 (skeletons/stage6/6c §2): install and accept the EULA, all
// through the helper's existing verbs (D7). The command carries only IDs;
// the settings come from GetInstallSpec and the jar from FetchArtifact.
// Everything the controller sends is checked again here against this
// build's own pins and limits.

// Provisioner is what an install and the EULA need from the helpers.
// lifecycle.HelperRuntime, with both sockets, is the real one.
type Provisioner interface {
	Status(ctx context.Context, serverID, operationID string) (lifecycle.Status, error)
	Provision(ctx context.Context, serverID, operationID string, spec lifecycle.ProvisionSpec) (bool, error)
	minecraft.Installer
	minecraft.Files
}

var _ Provisioner = lifecycle.HelperRuntime{}

// DefaultStagingDir holds a jar between the controller and the helper:
// inside the agent's StateDirectory, mode 0700.
const DefaultStagingDir = "/var/lib/waystone-agent/staging"

// installTimeout covers a whole install, the transfer included.
const installTimeout = 10 * time.Minute

// The controller's heap limits (store.MinHeapMiB, store.MaxHeapMiB).
const (
	minHeapMiB = 512
	maxHeapMiB = 32768
)

// The controller's reasons this agent acts on (agentlink).
const msgArtifactMismatch = "artifact_mismatch"

// An install's own failures, besides the helper's.
var (
	errSpecUnavailable     = errors.New("install spec unavailable")
	errArtifactUnavailable = errors.New("artifact unavailable")
	errArtifactMismatch    = errors.New("artifact mismatch")
	errServerBusy          = errors.New("server is not stopped")
)

// installOutcome is outcomeOf with the install's own failures added.
func installOutcome(err error) (agentv1.ResultOutcome, agentv1.ResultCode) {
	failed := agentv1.ResultOutcome_RESULT_OUTCOME_FAILED
	switch {
	case errors.Is(err, errSpecUnavailable):
		return failed, agentv1.ResultCode_RESULT_CODE_SPEC_UNAVAILABLE
	case errors.Is(err, errArtifactUnavailable):
		return failed, agentv1.ResultCode_RESULT_CODE_ARTIFACT_UNAVAILABLE
	case errors.Is(err, errArtifactMismatch):
		return failed, agentv1.ResultCode_RESULT_CODE_ARTIFACT_MISMATCH
	case errors.Is(err, errServerBusy):
		return failed, agentv1.ResultCode_RESULT_CODE_SERVER_BUSY
	}
	return outcomeOf(err)
}

var errNoProvisioner = fmt.Errorf("no provisioner configured: %w", helper.ErrNotSent)

// install runs one INSTALL to the end:
//
//  1. The server must be stopped or failed, or have no unit yet;
//     otherwise SERVER_BUSY and nothing is touched.
//  2. GetInstallSpec, checked against this build's pins and limits
//     (SPEC_UNAVAILABLE).
//  3. PROVISION with the pinned runtime and the heap.
//  4. Progress FETCHING, then the jar through the staging directory into
//     INSTALL_FILE server.jar.
//  5. Progress CONFIGURING, then server.properties.
func (a *Agent) install(cmd *agentv1.Command, cmds *commands, send func(*agentv1.ConnectRequest) bool) error {
	if a.provisioner == nil {
		return errNoProvisioner
	}
	ctx, cancel := context.WithTimeout(context.Background(), a.installTimeout)
	defer cancel()
	server, op := cmd.GetServerId(), cmd.GetOperationId()
	call := func() (context.Context, context.CancelFunc) { return context.WithTimeout(ctx, a.commandCallTimeout) }

	callCtx, done := call()
	st, err := a.provisioner.Status(callCtx, server, op)
	done()
	switch {
	case errors.Is(err, lifecycle.ErrNotFound):
		// No unit yet: a first install.
	case err != nil:
		return err
	case st.State != helperv1.RunState_RUN_STATE_STOPPED && st.State != helperv1.RunState_RUN_STATE_FAILED:
		return errServerBusy
	}

	callCtx, done = call()
	resp, err := cmds.link.GetInstallSpec(callCtx, &agentv1.GetInstallSpecRequest{ServerId: server, OperationId: op})
	done()
	if err != nil {
		return fmt.Errorf("%w: %v", errSpecUnavailable, err)
	}
	art, props, err := a.checkSpec(resp)
	if err != nil {
		return err
	}

	callCtx, done = call()
	_, err = a.provisioner.Provision(callCtx, server, op, lifecycle.ProvisionSpec{Runtime: art.Runtime, HeapMiB: resp.GetHeapMib()})
	done()
	if err != nil {
		return err
	}

	send(progress(cmd, agentv1.OperationPhase_OPERATION_PHASE_FETCHING))
	if err := a.fetchAndInstall(ctx, cmds.link, server, op, art); err != nil {
		return err
	}

	send(progress(cmd, agentv1.OperationPhase_OPERATION_PHASE_CONFIGURING))
	callCtx, done = call()
	defer done()
	return minecraft.WriteProperties(callCtx, a.provisioner, server, op, props)
}

func progress(cmd *agentv1.Command, phase agentv1.OperationPhase) *agentv1.ConnectRequest {
	return &agentv1.ConnectRequest{Msg: &agentv1.ConnectRequest_Progress{Progress: &agentv1.Progress{
		Epoch: cmd.GetEpoch(), OperationId: cmd.GetOperationId(), Phase: phase,
	}}}
}

// checkSpec refuses a spec this build wouldn't have made itself: a
// version it doesn't pin, an artifact that isn't that version's, or
// settings outside the limits.
func (a *Agent) checkSpec(s *agentv1.GetInstallSpecResponse) (minecraft.Artifact, minecraft.Properties, error) {
	art, ok := minecraft.ForVersion(a.artifacts, s.GetVersion())
	props := minecraft.Properties{ServerPort: s.GetPort(), Motd: s.GetMotd(), MaxPlayers: s.GetMaxPlayers()}
	switch {
	case hasUnknownFields(s.ProtoReflect()):
		return art, props, fmt.Errorf("%w: unknown fields", errSpecUnavailable)
	case !ok:
		return art, props, fmt.Errorf("%w: version %q isn't pinned", errSpecUnavailable, s.GetVersion())
	case hex.EncodeToString(s.GetArtifactSha256()) != art.SHA256 || s.GetArtifactSize() != uint64(art.Size):
		return art, props, fmt.Errorf("%w: not the pinned artifact", errSpecUnavailable)
	case s.GetHeapMib() < minHeapMiB || s.GetHeapMib() > maxHeapMiB:
		return art, props, fmt.Errorf("%w: heap %d MiB", errSpecUnavailable, s.GetHeapMib())
	}
	if err := props.Validate(); err != nil {
		return art, props, fmt.Errorf("%w: %v", errSpecUnavailable, err)
	}
	return art, props, nil
}

// fetchAndInstall streams the jar into staging/<op>.jar, made new with
// mode 0600, and checks its size and SHA-256 as it arrives. Only a jar
// that is exactly the pin, reopened read-only, goes to INSTALL_FILE (the
// worker checks the hash again). The staging file is removed whatever
// happens.
func (a *Agent) fetchAndInstall(ctx context.Context, link agentv1.AgentLinkServiceClient, server, op string, art minecraft.Artifact) error {
	want, err := hex.DecodeString(art.SHA256)
	if err != nil || len(want) != sha256.Size {
		return fmt.Errorf("%w: bad pin", errSpecUnavailable)
	}
	path := filepath.Join(a.stagingDir, op+".jar")
	w, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("%w: staging: %v", errArtifactUnavailable, err)
	}
	defer os.Remove(path)
	sum, err := receive(ctx, link, op, want, art.Size, w)
	if cerr := w.Close(); err == nil && cerr != nil {
		err = fmt.Errorf("%w: staging: %v", errArtifactUnavailable, cerr)
	}
	if err != nil {
		return err
	}

	jar, err := openStaged(path)
	if err != nil {
		return fmt.Errorf("%w: staging: %v", errArtifactUnavailable, err)
	}
	defer jar.Close()
	if fi, err := jar.Stat(); err != nil || !fi.Mode().IsRegular() || fi.Size() != art.Size {
		return fmt.Errorf("%w: the staged jar changed", errArtifactMismatch)
	}
	callCtx, done := context.WithTimeout(ctx, a.commandCallTimeout)
	defer done()
	return minecraft.InstallServerJar(callCtx, a.provisioner, server, op, jar, sum)
}

// clearStaging removes the jars an agent that was killed mid-install left
// in the staging directory (audit 6c-I4). At start nothing is in flight, so
// every *.jar there is stale. os.Remove never follows a link.
func (a *Agent) clearStaging() {
	entries, err := os.ReadDir(a.stagingDir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			a.logf("agent=staging_unreadable error=%q", err.Error())
		}
		return
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".jar") {
			continue
		}
		if err := os.Remove(filepath.Join(a.stagingDir, e.Name())); err != nil {
			a.logf("agent=staging_left file=%s error=%q", e.Name(), err.Error())
			continue
		}
		a.logf("agent=staging_removed file=%s", e.Name())
	}
}

// receive copies FetchArtifact's stream into w, refusing more than size
// bytes, and returns the hash of what it wrote once that is exactly the
// pin.
func receive(ctx context.Context, link agentv1.AgentLinkServiceClient, op string, want []byte, size int64, w io.Writer) ([sha256.Size]byte, error) {
	var sum [sha256.Size]byte
	stream, err := link.FetchArtifact(ctx, &agentv1.FetchArtifactRequest{OperationId: op, Sha256: want})
	if err != nil {
		return sum, fmt.Errorf("%w: %v", errArtifactUnavailable, err)
	}
	h := sha256.New()
	var total int64
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if st, ok := status.FromError(err); ok && st.Code() == codes.DataLoss && st.Message() == msgArtifactMismatch {
				return sum, fmt.Errorf("%w: the controller's copy", errArtifactMismatch)
			}
			return sum, fmt.Errorf("%w: %v", errArtifactUnavailable, err)
		}
		data := chunk.GetData()
		if total+int64(len(data)) > size {
			return sum, fmt.Errorf("%w: more than %d bytes", errArtifactMismatch, size)
		}
		total += int64(len(data))
		h.Write(data)
		if _, err := w.Write(data); err != nil {
			return sum, fmt.Errorf("%w: staging: %v", errArtifactUnavailable, err)
		}
	}
	copy(sum[:], h.Sum(nil))
	if total != size || !bytes.Equal(sum[:], want) {
		return sum, fmt.Errorf("%w: %d bytes, sha256 %x", errArtifactMismatch, total, sum)
	}
	return sum, nil
}

// acceptEula writes eula.txt for this operation: only ever as the answer
// to a person's explicit acceptance, which is what the command is.
func (a *Agent) acceptEula(cmd *agentv1.Command) error {
	if a.provisioner == nil {
		return errNoProvisioner
	}
	ctx, cancel := context.WithTimeout(context.Background(), a.commandCallTimeout)
	defer cancel()
	return minecraft.AcceptEula(ctx, a.provisioner, cmd.GetServerId(), cmd.GetOperationId())
}
