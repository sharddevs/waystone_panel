// SPDX-License-Identifier: AGPL-3.0-only

package agentlink

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/grpc/codes"

	agentv1 "github.com/sharddevs/waystone_panel/gen/waystone/agent/v1"
	"github.com/sharddevs/waystone_panel/internal/profile/minecraft"
	"github.com/sharddevs/waystone_panel/internal/store"
)

// Stage 6c-2 (skeletons/stage6/6c §2): an agent running an install asks
// for its settings and pulls the pinned jar. Both calls answer only for a
// server.install running on the calling node, and refuse everything else
// with the same not_permitted (6M5). The node comes from the authorizer,
// never from the request.

// DefaultArtifactDir is the controller's cache of pinned jars, one file
// per SHA-256, inside its StateDirectory.
const DefaultArtifactDir = "/var/lib/waystone-controller/artifacts"

// ArtifactChunkBytes is the most one FetchArtifact message carries.
const ArtifactChunkBytes = 32 << 10

// The reasons an install's agent can rely on.
const (
	MsgArtifactUnavailable = "artifact_unavailable"
	MsgArtifactMismatch    = "artifact_mismatch"
)

// runningInstall finds the install operationID for node n, or refuses.
func (s *Server) runningInstall(ctx context.Context, n store.AgentNode, method, operationID string) (store.InstallSpec, error) {
	var spec store.InstallSpec
	err := s.store.InTenantTx(ctx, n.Actor(), func(ctx context.Context, tx *store.Tx) error {
		var err error
		spec, err = tx.RunningInstall(ctx, operationID)
		return err
	})
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.refusedCall(method)
		return store.InstallSpec{}, errNotPermitted
	case errors.Is(err, store.ErrNoMembership):
		return store.InstallSpec{}, streamError(codes.PermissionDenied, MsgNodeRevoked)
	case err != nil:
		if ctx.Err() == nil {
			s.logf("agent_link=install_lookup_failed node=%s error=%q", n.NodeID, err.Error())
		}
		return store.InstallSpec{}, streamError(codes.Unavailable, "lookup_failed")
	}
	return spec, nil
}

// GetInstallSpec answers with the settings of a running install, for the
// server it is installing, with the artifact this build pins for its
// version.
func (s *Server) GetInstallSpec(ctx context.Context, req *agentv1.GetInstallSpecRequest) (*agentv1.GetInstallSpecResponse, error) {
	n, ok := nodeFrom(ctx)
	if !ok {
		return nil, errNotPermitted
	}
	if hasUnknownFields(req.ProtoReflect()) {
		return nil, streamError(codes.InvalidArgument, "unknown_field")
	}
	spec, err := s.runningInstall(ctx, n, methodGetInstallSpec, req.GetOperationId())
	if err != nil {
		return nil, err
	}
	art, ok := minecraft.ForVersion(s.artifacts, spec.Version)
	if spec.ServerID != req.GetServerId() || !ok {
		s.refusedCall(methodGetInstallSpec)
		return nil, errNotPermitted
	}
	sum, err := hex.DecodeString(art.SHA256)
	if err != nil || len(sum) != sha256.Size {
		return nil, errNotPermitted
	}
	return &agentv1.GetInstallSpecResponse{
		Version:        spec.Version,
		ArtifactSha256: sum,
		ArtifactSize:   uint64(art.Size),
		Port:           spec.Port,
		HeapMib:        spec.HeapMiB,
		Motd:           spec.Motd,
		MaxPlayers:     minecraft.DefaultMaxPlayers,
	}, nil
}

// FetchWait is the most a FetchArtifact waits for the fetch service.
const FetchWait = 3 * time.Minute

// fill gets art from the fetch service into the cache (6c-3), one fill
// at a time. The fetch service's file is only a source: it is opened
// without following a link, checked as a regular file of the pinned size,
// and copied into the cache while its SHA-1 and SHA-256 are checked
// again, so nothing fetch says or names is trusted.
func (s *Server) fill(ctx context.Context, art minecraft.Artifact) error {
	select {
	case s.fillSem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-s.fillSem }()
	// Another fill may have finished while this one waited.
	if _, err := os.Lstat(filepath.Join(s.artifactDir, art.SHA256)); err == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, FetchWait)
	defer cancel()
	staged, err := s.fetch(ctx, art)
	if err != nil {
		return err
	}
	if err := cacheArtifact(staged, s.artifactDir, art); err != nil {
		return err
	}
	s.logf("agent_link=artifact_cached sha256=%s version=%s", art.SHA256, art.Version)
	return nil
}

// FetchArtifact streams a pinned jar from the cache to an agent running
// an install of that jar's version. It hashes what it sends: a cached
// copy that isn't the pinned one ends the stream with artifact_mismatch,
// and is removed so it is never served again (N7). The agent checks the
// size and hash itself as well.
func (s *Server) FetchArtifact(req *agentv1.FetchArtifactRequest, stream agentv1.AgentLinkService_FetchArtifactServer) error {
	ctx := stream.Context()
	n, ok := nodeFrom(ctx)
	if !ok {
		return errNotPermitted
	}
	if hasUnknownFields(req.ProtoReflect()) {
		return streamError(codes.InvalidArgument, "unknown_field")
	}
	art, ok := minecraft.ForSHA256(s.artifacts, hex.EncodeToString(req.GetSha256()))
	if len(req.GetSha256()) != sha256.Size || !ok {
		s.refusedCall(methodFetchArtifact)
		return errNotPermitted
	}
	spec, err := s.runningInstall(ctx, n, methodFetchArtifact, req.GetOperationId())
	if err != nil {
		return err
	}
	if spec.Version != art.Version {
		s.refusedCall(methodFetchArtifact)
		return errNotPermitted
	}

	path := filepath.Join(s.artifactDir, art.SHA256)
	f, err := openArtifact(path, art.Size)
	if errors.Is(err, os.ErrNotExist) && s.fetch != nil {
		if err = s.fill(ctx, art); err == nil {
			f, err = openArtifact(path, art.Size)
		}
	}
	if err != nil {
		s.logf("agent_link=artifact_unavailable node=%s op=%s sha256=%s error=%q", n.NodeID, req.GetOperationId(), art.SHA256, err.Error())
		return streamError(codes.NotFound, MsgArtifactUnavailable)
	}
	defer f.Close()

	h := sha256.New()
	buf := make([]byte, ArtifactChunkBytes)
	var total int64
	for total <= art.Size {
		m, err := f.Read(buf)
		if m > 0 {
			total += int64(m)
			h.Write(buf[:m])
			if total <= art.Size {
				if err := stream.Send(&agentv1.FetchArtifactResponse{Data: buf[:m]}); err != nil {
					return err
				}
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			s.logf("agent_link=artifact_read_failed node=%s op=%s error=%q", n.NodeID, req.GetOperationId(), err.Error())
			return streamError(codes.Unavailable, MsgArtifactUnavailable)
		}
	}
	if total != art.Size || hex.EncodeToString(h.Sum(nil)) != art.SHA256 {
		rmErr := os.Remove(path)
		s.logf("WARNING agent_link=artifact_mismatch node=%s op=%s sha256=%s size=%d removed=%t",
			n.NodeID, req.GetOperationId(), art.SHA256, total, rmErr == nil)
		return streamError(codes.DataLoss, MsgArtifactMismatch)
	}
	s.logf("agent_link=artifact_sent node=%s op=%s sha256=%s", n.NodeID, req.GetOperationId(), art.SHA256)
	return nil
}
