// SPDX-License-Identifier: AGPL-3.0-only

package minecraft

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sharddevs/waystone_panel/internal/lifecycle"
)

const (
	testOpID     = "0192a5c4-7d3e-7b1a-9c2f-3e4d5f6a7b8c"
	testServerID = "3f2504e0-4f89-41d3-9a0c-0305e82c3301"
)

// fakeFiles is a server directory in memory that behaves like the worker:
// a write succeeds only if the file is still what its base says.
type fakeFiles struct {
	files map[string]string
	log   []string
	// beforeWrite runs between the read and the write, to simulate an
	// administrator editing the file.
	beforeWrite func(files map[string]string)
	readErr     error
}

func newFakeFiles(files map[string]string) *fakeFiles {
	if files == nil {
		files = map[string]string{}
	}
	return &fakeFiles{files: files}
}

func (f *fakeFiles) ReadFile(_ context.Context, serverID, operationID, name string) (lifecycle.File, error) {
	f.log = append(f.log, "read "+name+" "+serverID+" "+operationID)
	if f.readErr != nil {
		return lifecycle.File{}, f.readErr
	}
	content, ok := f.files[name]
	return lifecycle.File{Exists: ok, Content: []byte(content)}, nil
}

func (f *fakeFiles) WriteFile(_ context.Context, serverID, operationID, name string, content []byte, base lifecycle.File) error {
	f.log = append(f.log, "write "+name+" "+serverID+" "+operationID)
	if f.beforeWrite != nil {
		f.beforeWrite(f.files)
	}
	current, ok := f.files[name]
	if ok != base.Exists || (ok && !bytes.Equal([]byte(current), base.Content)) {
		return lifecycle.ErrChanged
	}
	f.files[name] = string(content)
	return nil
}

func TestEulaContent(t *testing.T) {
	got, err := EulaContent(testOpID)
	want := "# Accepted through Waystone, operation " + testOpID + "\neula=true\n"
	if err != nil || string(got) != want {
		t.Errorf("EulaContent = %q, %v; want %q", got, err, want)
	}
	for _, op := range []string{"", "x", strings.ToUpper(testOpID), testOpID + "\n", testOpID + "\neula=true"} {
		if _, err := EulaContent(op); !errors.Is(err, ErrInvalid) {
			t.Errorf("EulaContent(%q) err = %v, want ErrInvalid", op, err)
		}
	}
}

func TestAcceptEula(t *testing.T) {
	want := "# Accepted through Waystone, operation " + testOpID + "\neula=true\n"
	for name, before := range map[string]map[string]string{
		"no eula.txt":     nil,
		"eula=false":      {EulaFile: "eula=false\n"},
		"already true":    {EulaFile: "eula=true\n"},
		"other junk here": {EulaFile: "\x00\xff garbage"},
	} {
		f := newFakeFiles(before)
		if err := AcceptEula(context.Background(), f, testServerID, testOpID); err != nil {
			t.Errorf("%s: AcceptEula: %v", name, err)
			continue
		}
		if f.files[EulaFile] != want {
			t.Errorf("%s: eula.txt = %q, want %q", name, f.files[EulaFile], want)
		}
		wantLog := []string{"read eula.txt " + testServerID + " " + testOpID, "write eula.txt " + testServerID + " " + testOpID}
		if strings.Join(f.log, "|") != strings.Join(wantLog, "|") {
			t.Errorf("%s: calls %q, want %q", name, f.log, wantLog)
		}
	}
}

func TestAcceptEulaBadOperationIDTouchesNothing(t *testing.T) {
	f := newFakeFiles(nil)
	if err := AcceptEula(context.Background(), f, testServerID, "not-an-id"); !errors.Is(err, ErrInvalid) {
		t.Errorf("err = %v, want ErrInvalid", err)
	}
	if len(f.log) != 0 {
		t.Errorf("calls were made: %q", f.log)
	}
}

func TestWritePropertiesCreatesFile(t *testing.T) {
	f := newFakeFiles(nil)
	if err := WriteProperties(context.Background(), f, testServerID, testOpID, testProps); err != nil {
		t.Fatalf("WriteProperties: %v", err)
	}
	if got := f.files[PropertiesFile]; got != managed {
		t.Errorf("server.properties = %q, want %q", got, managed)
	}
}

func TestWritePropertiesKeepsAdministratorEdits(t *testing.T) {
	before := "#Minecraft server properties\nwhite-list=true\nmotd=old\ndifficulty=hard\nmotd=duplicate\n"
	f := newFakeFiles(map[string]string{PropertiesFile: before})
	if err := WriteProperties(context.Background(), f, testServerID, testOpID, testProps); err != nil {
		t.Fatalf("WriteProperties: %v", err)
	}
	want := "#Minecraft server properties\nwhite-list=true\nmotd=Waystone test\ndifficulty=hard\n" +
		"server-port=25566\nmax-players=20\nonline-mode=true\n" + locked
	if got := f.files[PropertiesFile]; got != want {
		t.Errorf("server.properties =\n%q\nwant\n%q", got, want)
	}
}

// An edit made between the read and the write is never overwritten.
func TestWritePropertiesChangedMeanwhile(t *testing.T) {
	f := newFakeFiles(map[string]string{PropertiesFile: "motd=old\n"})
	f.beforeWrite = func(files map[string]string) { files[PropertiesFile] = "motd=admin\n" }
	err := WriteProperties(context.Background(), f, testServerID, testOpID, testProps)
	if !errors.Is(err, lifecycle.ErrChanged) {
		t.Errorf("err = %v, want lifecycle.ErrChanged", err)
	}
	if got := f.files[PropertiesFile]; got != "motd=admin\n" {
		t.Errorf("server.properties = %q, want the admin's edit", got)
	}
	if strings.Count(strings.Join(f.log, "|"), "write") != 1 {
		t.Errorf("calls %q: want exactly one write, no retry", f.log)
	}
}

func TestWritePropertiesRefuses(t *testing.T) {
	bad := testProps
	bad.Motd = `§cred`
	f := newFakeFiles(map[string]string{PropertiesFile: "motd=old\n"})
	if err := WriteProperties(context.Background(), f, testServerID, testOpID, bad); !errors.Is(err, ErrInvalid) {
		t.Errorf("invalid motd: err = %v, want ErrInvalid", err)
	}
	if len(f.log) != 0 {
		t.Errorf("invalid motd: calls were made: %q", f.log)
	}

	f = newFakeFiles(map[string]string{PropertiesFile: "server\\-port=1\n"})
	if err := WriteProperties(context.Background(), f, testServerID, testOpID, testProps); !errors.Is(err, ErrUnsupported) {
		t.Errorf("escaped key: err = %v, want ErrUnsupported", err)
	}
	if f.files[PropertiesFile] != "server\\-port=1\n" {
		t.Errorf("escaped key: the file was written")
	}

	boom := errors.New("worker failed")
	f = newFakeFiles(nil)
	f.readErr = boom
	if err := WriteProperties(context.Background(), f, testServerID, testOpID, testProps); !errors.Is(err, boom) {
		t.Errorf("read error: err = %v, want it wrapped", err)
	}
	if strings.Contains(strings.Join(f.log, "|"), "write") {
		t.Errorf("read error: wrote anyway: %q", f.log)
	}
}
