// SPDX-License-Identifier: AGPL-3.0-only

//go:build unix

package agentlink

import (
	"fmt"
	"os"
	"syscall"
)

// openArtifact opens a cached jar without following a link, and keeps it
// only if it is a regular file this process owns, of the pinned size.
func openArtifact(path string, size int64) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
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
		err = fmt.Errorf("%s: not a regular file", path)
	case !ok || int(st.Uid) != os.Geteuid():
		err = fmt.Errorf("%s: not owned by the controller", path)
	case fi.Size() != size:
		err = fmt.Errorf("%s: %d bytes, want %d", path, fi.Size(), size)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
