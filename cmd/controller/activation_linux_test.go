// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package main

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// D2 and W5: under socket activation the controller takes the API and web
// sockets and, if present, the agent socket, by name. Anything else, or
// either required socket missing, is refused. Each
// case runs in a child process that gets real descriptors 3 and up, the
// way systemd passes them.
func TestActivatedListeners(t *testing.T) {
	if os.Getenv("CONTROLLER_ACTIVATION_CHILD") == "1" {
		activationChild()
		return
	}
	dir := t.TempDir()
	var files []*os.File
	for _, name := range []string{"one", "two", "three", "four"} {
		l, err := net.Listen("unix", filepath.Join(dir, name+".sock"))
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		f, err := l.(*net.UnixListener).File()
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		files = append(files, f)
	}
	cases := []struct {
		names string
		want  string
	}{
		{"waystone-controller-api:waystone-controller-web", "api=one web=two agent=none"},
		{"waystone-controller-api:waystone-controller-web:waystone-controller-agent", "api=one web=two agent=three"},
		{"waystone-controller-agent:waystone-controller-web:waystone-controller-api", "api=three web=two agent=one"},
		{"waystone-controller-web:waystone-controller-api", "api=two web=one agent=none"},
		{"waystone-controller-api", "error"},
		{"waystone-controller-api:waystone-controller-agent", "error"},
		{"waystone-controller-web", "error"},
		{"waystone-controller-web:waystone-controller-agent", "error"},
		{"waystone-controller-agent", "error"},
		{"waystone-controller-api:waystone-controller-web:something-else", "error"},
		{"waystone-controller-api:waystone-controller-web:waystone-controller-agent:extra", "error"},
		{"waystone-controller-api:waystone-controller-api", "error"},
	}
	for _, c := range cases {
		n := len(strings.Split(c.names, ":"))
		cmd := exec.Command(os.Args[0], "-test.run=^TestActivatedListeners$")
		cmd.Env = append(os.Environ(), "CONTROLLER_ACTIVATION_CHILD=1",
			"LISTEN_FDS="+strconv.Itoa(n), "LISTEN_FDNAMES="+c.names)
		cmd.ExtraFiles = files[:n]
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: child: %v\n%s", c.names, err, out)
		}
		if !strings.Contains(string(out), "RESULT "+c.want+"\n") {
			t.Errorf("%s: child said %q, want RESULT %s", c.names, out, c.want)
		}
	}
}

func activationChild() {
	getenv := func(k string) string {
		if k == "LISTEN_PID" {
			return strconv.Itoa(os.Getpid())
		}
		return os.Getenv(k)
	}
	socks, err := listeners("", "", "", os.Getpid(), getenv)
	if err != nil {
		os.Stdout.WriteString("RESULT error\n")
		return
	}
	name := func(l net.Listener) string {
		if l == nil {
			return "none"
		}
		return strings.TrimSuffix(filepath.Base(l.Addr().String()), ".sock")
	}
	os.Stdout.WriteString("RESULT api=" + name(socks.api) + " web=" + name(socks.web) + " agent=" + name(socks.agent) + "\n")
}
