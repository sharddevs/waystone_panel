// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package lifecycle

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	helperv1 "github.com/sharddevs/waystone_panel/gen/waystone/helper/v1"
	"github.com/sharddevs/waystone_panel/internal/helper"
)

func jarFile(t *testing.T, content string) *os.File {
	t.Helper()
	path := filepath.Join(t.TempDir(), "server.jar")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

// This goes through a real helper.Server, so the descriptor really crosses
// a Unix socket. That only works on Linux, hence the build tag on this file.
func TestHelperRuntimeInstallFile(t *testing.T) {
	var got []helper.Command
	var content []string
	rt := startHelper(t, func(_ context.Context, cmd helper.Command) *helperv1.Response {
		got = append(got, cmd)
		b, err := io.ReadAll(cmd.Input)
		if err != nil {
			return &helperv1.Response{ErrorCode: helperv1.ErrorCode_ERROR_CODE_FAILED, ErrorMessage: err.Error()}
		}
		content = append(content, string(b))
		return &helperv1.Response{}
	})
	// Only the provisioner's socket exists, so this also checks that
	// InstallFile talks to the provisioner.
	rt.Provisioner = rt.Client
	rt.Client = helper.Client{SocketPath: filepath.Join(t.TempDir(), "missing.sock")}
	var r Runtime = rt

	jar := jarFile(t, "jar bytes")
	sum := sha256.Sum256([]byte("jar bytes"))
	if err := r.InstallFile(ctx(t), serverID, opID, "server.jar", jar, sum); err != nil {
		t.Fatalf("InstallFile: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("helper received %d requests, want 1", len(got))
	}
	want := helper.FileCommand{Name: "server.jar", ContentSHA256: sum}
	if got[0].Action != helperv1.Action_ACTION_INSTALL_FILE || got[0].File != want || got[0].ServerID != serverID || got[0].OperationID != opID {
		t.Errorf("helper received %+v, want INSTALL_FILE with %+v", got[0], want)
	}
	if content[0] != "jar bytes" {
		t.Errorf("descriptor read %q, want \"jar bytes\"", content[0])
	}
	if _, err := jar.Stat(); err != nil {
		t.Errorf("caller's file after InstallFile: %v; it must stay open", err)
	}
}

func TestHelperRuntimeInstallFileNeedsContent(t *testing.T) {
	called := false
	rt := startHelper(t, func(context.Context, helper.Command) *helperv1.Response {
		called = true
		return &helperv1.Response{}
	})
	rt.Provisioner = rt.Client
	if err := rt.InstallFile(ctx(t), serverID, opID, "server.jar", nil, [32]byte{}); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("nil content: %v, want ErrInvalidRequest", err)
	}
	if called {
		t.Error("a request without content reached the helper")
	}
}

func TestHelperRuntimeInstallFileFailed(t *testing.T) {
	rt := startHelper(t, func(context.Context, helper.Command) *helperv1.Response {
		return &helperv1.Response{ErrorCode: helperv1.ErrorCode_ERROR_CODE_FAILED, ErrorMessage: "content does not match sha256"}
	})
	rt.Provisioner = rt.Client
	err := rt.InstallFile(ctx(t), serverID, opID, "server.jar", jarFile(t, "x"), [32]byte{})
	if !errors.Is(err, ErrFailed) {
		t.Errorf("FAILED answer: %v, want ErrFailed", err)
	}
}
