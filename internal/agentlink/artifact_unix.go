// SPDX-License-Identifier: AGPL-3.0-only

//go:build unix

package agentlink

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/sharddevs/waystone_panel/internal/profile/minecraft"
)

// openArtifact opens a cached jar without following a link, and keeps it
// only if it is a regular file this process owns, of the pinned size.
// Anything else there is errBadArtifact.
func openArtifact(path string, size int64) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, syscall.ELOOP) {
		return nil, fmt.Errorf("%s: a link: %w", path, errBadArtifact)
	}
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	switch {
	case !fi.Mode().IsRegular():
		err = fmt.Errorf("%s: not a regular file: %w", path, errBadArtifact)
	case !ok || int(st.Uid) != os.Geteuid():
		err = fmt.Errorf("%s: not owned by the controller: %w", path, errBadArtifact)
	case fi.Size() != size:
		err = fmt.Errorf("%s: %d bytes, want %d: %w", path, fi.Size(), size, errBadArtifact)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// cacheArtifact copies the fetched jar at staged into dir as
// <sha256>, through .<sha256>.new (0600), only once its size, SHA-1 and
// SHA-256 are the pin's.
func cacheArtifact(staged, dir string, art minecraft.Artifact) (err error) {
	src, err := os.OpenFile(staged, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer src.Close()
	fi, err := src.Stat()
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() || fi.Size() != art.Size {
		return fmt.Errorf("%s: not a regular file of %d bytes", staged, art.Size)
	}
	tmp := filepath.Join(dir, "."+art.SHA256+".new")
	if err := os.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	dst, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			dst.Close()
			os.Remove(tmp)
		}
	}()
	h1, h256 := sha1.New(), sha256.New()
	n, err := io.Copy(io.MultiWriter(dst, h1, h256), io.LimitReader(src, art.Size+1))
	if err != nil {
		return err
	}
	if n != art.Size || hex.EncodeToString(h1.Sum(nil)) != art.SHA1 || hex.EncodeToString(h256.Sum(nil)) != art.SHA256 {
		return fmt.Errorf("%s: not the pinned artifact", staged)
	}
	if err = dst.Sync(); err != nil {
		return err
	}
	if err = dst.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, filepath.Join(dir, art.SHA256)); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
