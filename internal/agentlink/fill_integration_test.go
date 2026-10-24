// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package agentlink

// Stage 6c-3: a jar the cache doesn't have is fetched once, checked again
// by the controller, and copied in; a fetched file that isn't the pin, a
// link, or a failed fetch leaves the cache as it was.

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sharddevs/waystone_panel/internal/profile/minecraft"
)

// fakeFetch stands in for waystone-fetch: it writes content to its own
// directory and names the file.
type fakeFetch struct {
	dir     string
	content []byte
	link    bool // name a link to the content instead
	fail    error
	gate    chan struct{}
	calls   atomic.Int32
}

func (f *fakeFetch) fetch(ctx context.Context, art minecraft.Artifact) (string, error) {
	f.calls.Add(1)
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if f.fail != nil {
		return "", f.fail
	}
	path := filepath.Join(f.dir, art.SHA256)
	if f.link {
		real := filepath.Join(f.dir, "real")
		if err := os.WriteFile(real, f.content, 0o640); err != nil {
			return "", err
		}
		return path, os.Symlink(real, path)
	}
	return path, os.WriteFile(path, f.content, 0o640)
}

func setupFill(t *testing.T, f *fakeFetch) (*env, string) {
	t.Helper()
	cache := t.TempDir()
	f.dir = t.TempDir()
	e := setup(t, func(c *Config) {
		c.ArtifactDir = cache
		c.Artifacts = []minecraft.Artifact{testArtifact()}
		c.Fetch = f.fetch
	})
	return e, cache
}

func TestFillCache(t *testing.T) {
	f := &fakeFetch{content: testJar}
	e, cache := setupFill(t, f)
	c := e.client(t)
	art := testArtifact()
	sum, _ := hex.DecodeString(art.SHA256)
	_, epoch, r := e.session(t)
	_, op := e.install(t, r, epoch, "new", 25600)

	got, err := fetch(c, op, sum)
	if err != nil || !bytes.Equal(got, testJar) {
		t.Fatalf("first fetch: %d bytes, %v", len(got), err)
	}
	fi, err := os.Stat(filepath.Join(cache, art.SHA256))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("cached: %v %v", fi, err)
	}
	if left, _ := filepath.Glob(filepath.Join(cache, ".*")); len(left) != 0 {
		t.Errorf("left %v", left)
	}
	if _, err := fetch(c, op, sum); err != nil || f.calls.Load() != 1 {
		t.Errorf("second fetch: %v, %d fetches", err, f.calls.Load())
	}
}

// Two installs asking at once share one fetch.
func TestFillOneAtATime(t *testing.T) {
	f := &fakeFetch{content: testJar, gate: make(chan struct{})}
	e, _ := setupFill(t, f)
	c := e.client(t)
	art := testArtifact()
	sum, _ := hex.DecodeString(art.SHA256)
	_, epoch, r := e.session(t)
	_, op1 := e.install(t, r, epoch, "one", 25600)
	_, op2 := e.install(t, r, epoch, "two", 25601)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, op := range []string{op1, op2} {
		wg.Go(func() {
			got, err := fetch(c, op, sum)
			if err == nil && !bytes.Equal(got, testJar) {
				err = errors.New("wrong bytes")
			}
			errs <- err
		})
	}
	waitFor(t, "a fetch", func() bool { return f.calls.Load() == 1 })
	time.Sleep(100 * time.Millisecond)
	close(f.gate)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	if n := f.calls.Load(); n != 1 {
		t.Errorf("%d fetches", n)
	}
}

func TestFillRefusesBadFetches(t *testing.T) {
	wrong := bytes.Clone(testJar)
	wrong[1] ^= 1
	for name, f := range map[string]*fakeFetch{
		"wrong bytes": {content: wrong},
		"short":       {content: testJar[:10]},
		"a link":      {content: testJar, link: true},
		"failed":      {fail: errors.New("fetch service: \"failed\\n\"")},
	} {
		t.Run(name, func(t *testing.T) {
			e, cache := setupFill(t, f)
			c := e.client(t)
			art := testArtifact()
			sum, _ := hex.DecodeString(art.SHA256)
			_, epoch, r := e.session(t)
			_, op := e.install(t, r, epoch, "new", 25600)
			_, err := fetch(c, op, sum)
			if st, _ := status.FromError(err); st.Code() != codes.NotFound || st.Message() != MsgArtifactUnavailable {
				t.Errorf("%v, want artifact_unavailable", err)
			}
			if left, _ := os.ReadDir(cache); len(left) != 0 {
				t.Errorf("cache has %v", left)
			}
		})
	}
}

// The wrong SHA-1 alone keeps a fetched file out of the cache.
func TestFillChecksSHA1(t *testing.T) {
	f := &fakeFetch{content: testJar}
	cache := t.TempDir()
	f.dir = t.TempDir()
	art := testArtifact()
	art.SHA1 = "0000000000000000000000000000000000000000"
	e := setup(t, func(c *Config) {
		c.ArtifactDir = cache
		c.Artifacts = []minecraft.Artifact{art}
		c.Fetch = f.fetch
	})
	c := e.client(t)
	sum, _ := hex.DecodeString(art.SHA256)
	_, epoch, r := e.session(t)
	_, op := e.install(t, r, epoch, "new", 25600)
	if _, err := fetch(c, op, sum); err == nil {
		t.Error("served")
	}
	if left, _ := os.ReadDir(cache); len(left) != 0 {
		t.Errorf("cache has %v", left)
	}
}

// 6c-I5: a wrong-size file in the cache doesn't stop a fill for good: it
// is removed and fetched again, once.
func TestFillReplacesABadCacheFile(t *testing.T) {
	f := &fakeFetch{content: testJar}
	e, cache := setupFill(t, f)
	c := e.client(t)
	art := testArtifact()
	sum, _ := hex.DecodeString(art.SHA256)
	if err := os.WriteFile(filepath.Join(cache, art.SHA256), testJar[:100], 0o600); err != nil {
		t.Fatal(err)
	}
	_, epoch, r := e.session(t)
	_, op := e.install(t, r, epoch, "new", 25600)
	if got, err := fetch(c, op, sum); err != nil || !bytes.Equal(got, testJar) {
		t.Fatalf("%d bytes, %v", len(got), err)
	}
	if n := f.calls.Load(); n != 1 {
		t.Errorf("%d fetches, want 1", n)
	}
	if !strings.Contains(e.logs.String(), "agent_link=artifact_refused") {
		t.Error("no artifact_refused line")
	}
}
