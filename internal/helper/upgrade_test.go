// SPDX-License-Identifier: AGPL-3.0-only

package helper

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// H4: a unit written before 7b (testdata/waystone-srv-6.service, the 6d
// golden) is brought up to this build's templates, and nothing is
// restarted.

var reload = []string{"/usr/bin/systemctl", "--no-ask-password", "daemon-reload"}

func oldUnitDir(t *testing.T, edit func(string) string) string {
	t.Helper()
	old, err := os.ReadFile("testdata/waystone-srv-6.service")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	content := string(old)
	if edit != nil {
		content = edit(content)
	}
	if err := os.WriteFile(filepath.Join(dir, testUnit), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	// Neither of these is a server's unit; both are left alone.
	for _, name := range []string{"waystone-controller.service", "waystone-srv-not-an-id.service"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("[Unit]\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestUpgradeUnits(t *testing.T) {
	dir := oldUnitDir(t, nil)
	r := &fakeRunner{}
	got, err := UpgradeUnits(context.Background(), UnitFiles{Dir: dir}, r, DefaultRuntimes)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != (UpgradeResult{ServerID: testServerID, Changed: true}) {
		t.Fatalf("results %+v", got)
	}
	service, _ := os.ReadFile(filepath.Join(dir, testUnit))
	if want, _ := RenderUnit(goldenParams); !bytes.Equal(service, want) {
		t.Errorf("service:\n%s", service)
	}
	socket, _ := os.ReadFile(filepath.Join(dir, ConsoleSocketName(testServerID)))
	if want, _ := RenderConsoleSocket(testServerID); !bytes.Equal(socket, want) {
		t.Errorf("socket:\n%s", socket)
	}
	// One daemon-reload, and no start, stop or restart.
	if !slices.EqualFunc(r.calls, [][]string{reload}, slices.Equal) {
		t.Errorf("runner calls %q", r.calls)
	}
	for _, name := range []string{"waystone-controller.service", "waystone-srv-not-an-id.service"} {
		if b, _ := os.ReadFile(filepath.Join(dir, name)); string(b) != "[Unit]\n" {
			t.Errorf("%s changed", name)
		}
	}

	// Again: nothing to do, and no reload.
	r = &fakeRunner{}
	got, err = UpgradeUnits(context.Background(), UnitFiles{Dir: dir}, r, DefaultRuntimes)
	if err != nil || len(got) != 1 || got[0].Changed || len(r.calls) != 0 {
		t.Errorf("second run: %+v %v %q", got, err, r.calls)
	}
}

func TestUpgradeUnitsRefuses(t *testing.T) {
	for name, edit := range map[string]func(string) string{
		"not ours":     func(s string) string { return strings.Replace(s, UnitMarker, "", 1) },
		"another user": func(s string) string { return strings.Replace(s, "User=ws-16362f566387", "User=root", 1) },
		"two users":    func(s string) string { return strings.Replace(s, "Group=", "User=ws-16362f566387\nGroup=", 1) },
		"unknown java": func(s string) string {
			return strings.Replace(s, "/usr/lib/jvm/java-25-openjdk-amd64/bin/java", "/tmp/java", 1)
		},
		"heap 64":           func(s string) string { return strings.Replace(s, "-Xms2048M -Xmx2048M", "-Xms64M -Xmx64M", 1) },
		"heap uneven":       func(s string) string { return strings.Replace(s, "-Xms2048M", "-Xms1024M", 1) },
		"another ExecStart": func(s string) string { return strings.Replace(s, " nogui", " nogui --forceUpgrade", 1) },
	} {
		t.Run(name, func(t *testing.T) {
			dir := oldUnitDir(t, edit)
			before, _ := os.ReadFile(filepath.Join(dir, testUnit))
			r := &fakeRunner{}
			if _, err := UpgradeUnits(context.Background(), UnitFiles{Dir: dir}, r, DefaultRuntimes); err == nil {
				t.Fatal("no error")
			}
			after, _ := os.ReadFile(filepath.Join(dir, testUnit))
			if !bytes.Equal(before, after) {
				t.Error("changed the unit")
			}
			// A failure still reloads, in case something before it was
			// written; it never starts or stops anything.
			if !slices.EqualFunc(r.calls, [][]string{reload}, slices.Equal) {
				t.Errorf("runner calls %q", r.calls)
			}
			if _, err := os.Lstat(filepath.Join(dir, ConsoleSocketName(testServerID))); err == nil {
				t.Error("wrote a socket")
			}
		})
	}
}

// A console socket file Waystone didn't write is never taken over.
func TestUpgradeUnitsForeignSocket(t *testing.T) {
	dir := oldUnitDir(t, nil)
	if err := os.WriteFile(filepath.Join(dir, ConsoleSocketName(testServerID)), []byte("[Socket]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := UpgradeUnits(context.Background(), UnitFiles{Dir: dir}, &fakeRunner{}, DefaultRuntimes); err == nil {
		t.Fatal("no error")
	}
}
