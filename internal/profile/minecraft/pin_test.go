// SPDX-License-Identifier: AGPL-3.0-only

package minecraft

import (
	"encoding/hex"
	"strings"
	"testing"
)

// The pin is self-consistent: the URL names the SHA-1, and both hashes
// are lowercase hex of the right length.
func TestPin(t *testing.T) {
	if !strings.HasPrefix(PinnedURL, "https://piston-data.mojang.com/v1/objects/"+PinnedSHA1+"/") {
		t.Errorf("URL %s doesn't name the SHA-1", PinnedURL)
	}
	for name, h := range map[string]struct {
		s string
		n int
	}{"sha1": {PinnedSHA1, 20}, "sha256": {PinnedSHA256, 32}} {
		b, err := hex.DecodeString(h.s)
		if err != nil || len(b) != h.n || strings.ToLower(h.s) != h.s {
			t.Errorf("%s %q", name, h.s)
		}
	}
	if PinnedSize <= 0 || PinnedSize > 256<<20 {
		t.Errorf("size %d is outside what INSTALL_FILE takes", PinnedSize)
	}
	p := Properties{ServerPort: 25565, MaxPlayers: DefaultMaxPlayers}
	if err := p.Validate(); err != nil {
		t.Errorf("default properties: %v", err)
	}
}
