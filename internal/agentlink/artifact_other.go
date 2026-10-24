// SPDX-License-Identifier: AGPL-3.0-only

//go:build !unix

package agentlink

import (
	"errors"
	"os"

	"github.com/sharddevs/waystone_panel/internal/profile/minecraft"
)

// openArtifact: the cache's checks need Unix file ownership, so other
// systems never serve a jar.
func openArtifact(string, int64) (*os.File, error) {
	return nil, errors.New("artifacts are served on Unix only")
}

func cacheArtifact(string, string, minecraft.Artifact) error {
	return errors.New("artifacts are cached on Unix only")
}
