// SPDX-License-Identifier: AGPL-3.0-only

package minecraft

// The one curated server version (scope D8): taken from Mojang's version
// manifest when the pin was made, and checked on the dev VM. There is no
// manifest lookup at runtime, so the manifest isn't in the trust path.
// Changing the version means changing all of these together.
const (
	PinnedVersion = "26.3"
	// PinnedURL is the only address waystone-fetch may download from.
	PinnedURL    = "https://piston-data.mojang.com/v1/objects/33680f5f2ac32864d6d7cf5e56a705fdb3e05f4c/server.jar"
	PinnedSHA1   = "33680f5f2ac32864d6d7cf5e56a705fdb3e05f4c"
	PinnedSHA256 = "d052f14d7a173734fba553711e5b570162e2f2a313267ee31a21b975a679be64"
	PinnedSize   = 62294556
	// PinnedRuntime is the helper's runtime name the version runs on.
	PinnedRuntime = "java25"
	// DefaultMaxPlayers is max-players for v0.1, which has no setting for it.
	DefaultMaxPlayers = 20
)

// Artifact is one pinned server jar. The controller, the agent and
// waystone-fetch each check a jar against the set they were built with,
// never against anything they were sent.
type Artifact struct {
	Version string
	URL     string
	SHA1    string // lowercase hex
	SHA256  string // lowercase hex
	Size    int64
	Runtime string
}

// Pinned is the set this build trusts: the one curated version.
var Pinned = []Artifact{{
	Version: PinnedVersion,
	URL:     PinnedURL,
	SHA1:    PinnedSHA1,
	SHA256:  PinnedSHA256,
	Size:    PinnedSize,
	Runtime: PinnedRuntime,
}}

// ForVersion finds version's artifact in set.
func ForVersion(set []Artifact, version string) (Artifact, bool) {
	for _, a := range set {
		if a.Version == version {
			return a, true
		}
	}
	return Artifact{}, false
}

// ForSHA256 finds the artifact with this SHA-256 (lowercase hex) in set.
func ForSHA256(set []Artifact, sum string) (Artifact, bool) {
	for _, a := range set {
		if a.SHA256 == sum {
			return a, true
		}
	}
	return Artifact{}, false
}
