// SPDX-License-Identifier: AGPL-3.0-only

//go:build unix

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/sharddevs/waystone_panel/internal/agenttls"
	"github.com/sharddevs/waystone_panel/internal/enrollbundle"
)

type fakeEnroll struct {
	uid       int
	bundle    string
	enrollErr error
	gotSecret []byte
	gotTLS    *tls.Config
	gotAddr   string
	keyPins   []agenttls.Pin // the key each try presented
	systemctl [][]string
}

// The tests' agent user is whoever runs them, so the real chowns work
// without root.
var testAgentUID, testAgentGID = os.Getuid(), os.Getgid()

func owner(t *testing.T, path string) [2]int {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	st := fi.Sys().(*syscall.Stat_t)
	return [2]int{int(st.Uid), int(st.Gid)}
}

func (f *fakeEnroll) deps() enrollDeps {
	return enrollDeps{
		uid:         func() int { return f.uid },
		readBundle:  func() (string, error) { return f.bundle, nil },
		lookupAgent: func() (int, int, error) { return testAgentUID, testAgentGID, nil },
		enroll: func(_ context.Context, addr string, tc *tls.Config, secret []byte) (string, string, error) {
			f.gotAddr, f.gotTLS, f.gotSecret = addr, tc, secret
			f.keyPins = append(f.keyPins, agenttls.PinOf(tc.Certificates[0].Leaf))
			if f.enrollErr != nil {
				return "", "", f.enrollErr
			}
			return testNode, "node c", nil
		},
		systemctl: func(args ...string) error {
			f.systemctl = append(f.systemctl, args)
			return nil
		},
	}
}

func testBundle(t *testing.T) (string, enrollbundle.Bundle) {
	t.Helper()
	b := enrollbundle.Bundle{Addr: netip.MustParseAddrPort("10.0.20.80:7443")}
	b.Pin[0], b.Secret[0] = 1, 2
	s, err := enrollbundle.Encode(b)
	if err != nil {
		t.Fatal(err)
	}
	return s, b
}

func enrollArgs(dir string) []string {
	return []string{"-tls-dir", filepath.Join(dir, "tls"), "-env-file", filepath.Join(dir, "agent.env"), "-drop-in", filepath.Join(dir, "d", "remote.conf")}
}

func TestEnroll(t *testing.T) {
	dir := t.TempDir()
	s, b := testBundle(t)
	f := &fakeEnroll{bundle: s + "\n"}
	var out, errs bytes.Buffer
	if code := runEnroll(enrollArgs(dir), &out, &errs, f.deps()); code != 0 {
		t.Fatalf("enroll = %d: %s", code, errs.String())
	}
	if f.gotAddr != "10.0.20.80:7443" || !bytes.Equal(f.gotSecret, b.Secret[:]) || f.gotTLS.VerifyConnection == nil {
		t.Fatalf("enroll called with %q %x", f.gotAddr, f.gotSecret)
	}
	// The key that enrolled is the one now in place, owned by the agent.
	key, err := agenttls.Load(filepath.Join(dir, "tls"))
	if err != nil || agenttls.PinOf(key.Leaf) != f.keyPins[0] {
		t.Fatalf("key in place: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "tls", pendingDir)); !os.IsNotExist(err) {
		t.Fatalf("pending dir left: %v", err)
	}
	fi, _ := os.Stat(filepath.Join(dir, "tls", agenttls.KeyFile))
	agent := [2]int{testAgentUID, testAgentGID}
	if fi.Mode().Perm() != 0o600 || owner(t, filepath.Join(dir, "tls", agenttls.KeyFile)) != agent || owner(t, filepath.Join(dir, "tls")) != agent {
		t.Fatalf("key mode %v", fi.Mode().Perm())
	}
	env, _ := os.ReadFile(filepath.Join(dir, "agent.env"))
	for _, want := range []string{"WAYSTONE_NODE_ID=" + testNode, "WAYSTONE_CONTROLLER_ADDR=10.0.20.80:7443", "WAYSTONE_CONTROLLER_PIN=" + b.Pin.String()} {
		if !strings.Contains(string(env), want+"\n") {
			t.Errorf("agent.env lacks %q:\n%s", want, env)
		}
	}
	if strings.Contains(string(env), s) {
		t.Error("agent.env holds the bundle")
	}
	fi, _ = os.Stat(filepath.Join(dir, "agent.env"))
	if fi.Mode().Perm() != 0o640 || owner(t, filepath.Join(dir, "agent.env"))[1] != testAgentGID {
		t.Errorf("agent.env mode %v owner %v", fi.Mode().Perm(), owner(t, filepath.Join(dir, "agent.env")))
	}
	drop, _ := os.ReadFile(filepath.Join(dir, "d", "remote.conf"))
	if !strings.Contains(string(drop), "\nIPAddressAllow=10.0.20.80\n") || !strings.Contains(string(drop), "\nExecStart=\n") {
		t.Errorf("drop-in:\n%s", drop)
	}
	if len(f.systemctl) != 2 || strings.Join(f.systemctl[1], " ") != "enable --now waystone-agent.service" {
		t.Errorf("systemctl calls %v", f.systemctl)
	}

	// The drop-in's command line is one the agent accepts.
	var execLine string
	for line := range strings.SplitSeq(string(drop), "\n") {
		if v, ok := strings.CutPrefix(line, "ExecStart="); ok && v != "" {
			execLine = v
		}
	}
	args := strings.NewReplacer("${WAYSTONE_CONTROLLER_ADDR}", "10.0.20.80:7443", "${WAYSTONE_CONTROLLER_PIN}", b.Pin.String(), "${WAYSTONE_NODE_ID}", testNode).
		Replace(strings.TrimPrefix(execLine, "/usr/local/bin/waystone-agent "))
	if cfg, err := parse(strings.Fields(args), 995, &errs); err != nil || !cfg.remote() || cfg.tlsDir != filepath.Join(dir, "tls") {
		t.Errorf("drop-in ExecStart %q: %+v %v", args, cfg, err)
	}

	// Again: already a node.
	if code := runEnroll(enrollArgs(dir), &out, &errs, f.deps()); code != 1 {
		t.Fatalf("second enroll = %d", code)
	}
}

func TestEnrollRefusals(t *testing.T) {
	s, _ := testBundle(t)
	var out bytes.Buffer
	if code := runEnroll(enrollArgs(t.TempDir()), &out, &out, (&fakeEnroll{uid: 1000, bundle: s}).deps()); code != 2 {
		t.Errorf("not root: %d", code)
	}
	if code := runEnroll(append(enrollArgs(t.TempDir()), "extra"), &out, &out, (&fakeEnroll{bundle: s}).deps()); code != 2 {
		t.Errorf("an argument (a bundle on the command line?): %d", code)
	}
	if code := runEnroll(enrollArgs(t.TempDir()), &out, &out, (&fakeEnroll{bundle: "garbage"}).deps()); code != 1 {
		t.Errorf("bad bundle: %d", code)
	}
}

// A failed try keeps its key for the next one, and writes nothing else.
func TestEnrollRetryKeepsKey(t *testing.T) {
	dir := t.TempDir()
	s, _ := testBundle(t)
	f := &fakeEnroll{bundle: s, enrollErr: errors.New("enrollment_refused")}
	var out bytes.Buffer
	if code := runEnroll(enrollArgs(dir), &out, &out, f.deps()); code != 1 {
		t.Fatalf("failed enroll = %d", code)
	}
	for _, p := range []string{"agent.env", filepath.Join("d", "remote.conf"), filepath.Join("tls", agenttls.KeyFile)} {
		if _, err := os.Stat(filepath.Join(dir, p)); !os.IsNotExist(err) {
			t.Errorf("%s written after a failure", p)
		}
	}
	f.enrollErr = nil
	if code := runEnroll(enrollArgs(dir), &out, &out, f.deps()); code != 0 {
		t.Fatalf("retry = %d: %s", code, out.String())
	}
	if len(f.keyPins) != 2 || f.keyPins[0] != f.keyPins[1] {
		t.Fatalf("the retry used another key: %v", f.keyPins)
	}
}

// A machine enrolling again after its node was revoked (agent.env
// removed) gets a new key, never the revoked one.
func TestReEnrollNewKey(t *testing.T) {
	dir := t.TempDir()
	s, _ := testBundle(t)
	f := &fakeEnroll{bundle: s}
	var out bytes.Buffer
	if code := runEnroll(enrollArgs(dir), &out, &out, f.deps()); code != 0 {
		t.Fatal(out.String())
	}
	os.Remove(filepath.Join(dir, "agent.env"))
	if code := runEnroll(enrollArgs(dir), &out, &out, f.deps()); code != 0 {
		t.Fatal(out.String())
	}
	if f.keyPins[0] == f.keyPins[1] {
		t.Fatal("re-enrolled with the old key")
	}
}

// 6a-M1: enroll runs as root in a directory the agent user may control,
// so a planted symlink or key is refused, never followed or reused.
func TestEnrollRefusesPlantedFiles(t *testing.T) {
	s, _ := testBundle(t)
	target := filepath.Join(t.TempDir(), "shadow")
	if err := os.WriteFile(target, []byte("root:secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	planted, err := agenttls.Generate()
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(t *testing.T, tls string){
		// The targets are private directories of the right owner, so
		// only not following the link refuses them.
		"symlinked key dir": func(t *testing.T, tls string) {
			must(t, os.Symlink(privateDir(t), tls))
		},
		"symlinked pending": func(t *testing.T, tls string) {
			must(t, os.Mkdir(tls, 0o700))
			must(t, os.Symlink(privateDir(t), filepath.Join(tls, pendingDir)))
		},
		"symlinked key": func(t *testing.T, tls string) {
			must(t, os.MkdirAll(filepath.Join(tls, pendingDir), 0o700))
			must(t, os.Symlink(target, filepath.Join(tls, pendingDir, agenttls.KeyFile)))
			must(t, os.WriteFile(filepath.Join(tls, pendingDir, agenttls.CertFile), []byte("x"), 0o644))
		},
		"symlinked key, no cert": func(t *testing.T, tls string) {
			must(t, os.MkdirAll(filepath.Join(tls, pendingDir), 0o700))
			must(t, os.Symlink(target, filepath.Join(tls, pendingDir, agenttls.KeyFile)))
		},
		"group-readable key": func(t *testing.T, tls string) {
			writeKey(t, filepath.Join(tls, pendingDir), planted, 0o640)
		},
		"open key dir": func(t *testing.T, tls string) {
			must(t, os.Mkdir(tls, 0o755))
		},
		"open pending": func(t *testing.T, tls string) {
			must(t, os.Mkdir(tls, 0o700))
			must(t, os.Mkdir(filepath.Join(tls, pendingDir), 0o750))
		},
		"key dir is a file": func(t *testing.T, tls string) {
			must(t, os.WriteFile(tls, nil, 0o600))
		},
	}
	if os.Getuid() == 0 {
		// Only root can hand a file to someone else.
		cases["foreign key"] = func(t *testing.T, tls string) {
			p := filepath.Join(tls, pendingDir)
			writeKey(t, p, planted, 0o600)
			must(t, os.Chown(filepath.Join(p, agenttls.KeyFile), 12345, 12345))
		}
		cases["foreign pending"] = func(t *testing.T, tls string) {
			p := filepath.Join(tls, pendingDir)
			writeKey(t, p, planted, 0o600)
			must(t, os.Chown(p, testAgentUID+12345, 0))
		}
		cases["agent's pending"] = func(t *testing.T, tls string) {
			// The agent user owns agent-tls after an enrollment; a
			// pending dir it made is still refused.
			p := filepath.Join(tls, pendingDir)
			writeKey(t, p, planted, 0o600)
			must(t, os.Chown(tls, 995, 995))
			must(t, os.Chown(p, 995, 995))
		}
	}
	for name, plant := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			plant(t, filepath.Join(dir, "tls"))
			f := &fakeEnroll{bundle: s}
			d := f.deps()
			if name == "agent's pending" {
				d.lookupAgent = func() (int, int, error) { return 995, 995, nil }
			}
			var out bytes.Buffer
			if code := runEnroll(enrollArgs(dir), &out, &out, d); code != 1 {
				t.Fatalf("enroll = %d: %s", code, out.String())
			}
			if len(f.keyPins) != 0 {
				t.Fatal("enrolled anyway")
			}
			if !strings.Contains(out.String(), "refusing") {
				t.Errorf("no clear refusal: %s", out.String())
			}
			if got, _ := os.ReadFile(target); string(got) != "root:secret\n" {
				t.Fatalf("target changed: %q", got)
			}
			if fi, _ := os.Stat(target); fi.Mode().Perm() != 0o600 || owner(t, target) != [2]int{os.Getuid(), os.Getgid()} {
				t.Fatal("target's mode or owner changed")
			}
		})
	}
}

func privateDir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	must(t, os.Chmod(d, 0o700))
	return d
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func writeKey(t *testing.T, dir string, c tls.Certificate, keyMode os.FileMode) {
	t.Helper()
	must(t, os.MkdirAll(dir, 0o700))
	keyPEM, certPEM, err := agenttls.Encode(c)
	must(t, err)
	must(t, os.WriteFile(filepath.Join(dir, agenttls.KeyFile), keyPEM, keyMode))
	must(t, os.Chmod(filepath.Join(dir, agenttls.KeyFile), keyMode))
	must(t, os.WriteFile(filepath.Join(dir, agenttls.CertFile), certPEM, 0o644))
}
