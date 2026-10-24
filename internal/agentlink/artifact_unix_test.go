// SPDX-License-Identifier: AGPL-3.0-only

//go:build unix

package agentlink

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sharddevs/waystone_panel/internal/profile/minecraft"
)

// cacheArtifact keeps a copy only when the size and both hashes are the
// pin's, each on its own.
func TestCacheArtifactChecksEachPin(t *testing.T) {
	jar := bytes.Repeat([]byte("cache test jar\n"), 3000)
	s1, s256 := sha1.Sum(jar), sha256.Sum256(jar)
	pin := minecraft.Artifact{Version: "26.3", SHA1: hex.EncodeToString(s1[:]),
		SHA256: hex.EncodeToString(s256[:]), Size: int64(len(jar))}
	for name, change := range map[string]func(*minecraft.Artifact){
		"good":   nil,
		"sha1":   func(a *minecraft.Artifact) { a.SHA1 = strings.Repeat("0", 40) },
		"sha256": func(a *minecraft.Artifact) { a.SHA256 = strings.Repeat("0", 64) },
		"size":   func(a *minecraft.Artifact) { a.Size-- },
	} {
		art := pin
		if change != nil {
			change(&art)
		}
		staged := filepath.Join(t.TempDir(), "staged")
		if err := os.WriteFile(staged, jar, 0o600); err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		err := cacheArtifact(staged, dir, art)
		left, _ := os.ReadDir(dir)
		if change == nil {
			if err != nil || len(left) != 1 || left[0].Name() != art.SHA256 {
				t.Errorf("%s: %v, left %v", name, err, left)
			}
			continue
		}
		if err == nil || len(left) != 0 {
			t.Errorf("%s: %v, left %v", name, err, left)
		}
	}
}
