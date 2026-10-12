// SPDX-License-Identifier: AGPL-3.0-only

package minecraft

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"testing"

	"github.com/sharddevs/waystone_panel/internal/lifecycle"
)

// fakeInstaller records each InstallFile call instead of talking to a helper.
type fakeInstaller struct {
	names []string
	files []*os.File
	sums  [][32]byte
	ids   []string
	err   error
}

func (f *fakeInstaller) InstallFile(_ context.Context, serverID, operationID, name string, content *os.File, sum [32]byte) error {
	f.names = append(f.names, name)
	f.files = append(f.files, content)
	f.sums = append(f.sums, sum)
	f.ids = append(f.ids, serverID+" "+operationID)
	return f.err
}

func TestInstallServerJar(t *testing.T) {
	jar, err := os.CreateTemp(t.TempDir(), "jar")
	if err != nil {
		t.Fatal(err)
	}
	defer jar.Close()
	sum := sha256.Sum256([]byte("jar"))
	f := &fakeInstaller{}
	if err := InstallServerJar(context.Background(), f, testServerID, testOpID, jar, sum); err != nil {
		t.Fatalf("InstallServerJar: %v", err)
	}
	if len(f.names) != 1 || f.names[0] != "server.jar" || f.files[0] != jar || f.sums[0] != sum || f.ids[0] != testServerID+" "+testOpID {
		t.Errorf("installed %q %v %x %q; want server.jar, the same file and sum, and the IDs", f.names, f.files, f.sums, f.ids)
	}
}

func TestInstallServerJarKeepsTheError(t *testing.T) {
	f := &fakeInstaller{err: lifecycle.ErrFailed}
	err := InstallServerJar(context.Background(), f, testServerID, testOpID, nil, [32]byte{})
	// InstallServerJar wraps with %w, so errors.Is still finds ErrFailed.
	if !errors.Is(err, lifecycle.ErrFailed) {
		t.Errorf("error = %v, want one that wraps lifecycle.ErrFailed", err)
	}
}
