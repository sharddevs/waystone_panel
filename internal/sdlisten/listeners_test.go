// SPDX-License-Identifier: AGPL-3.0-only

package sdlisten

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Listeners refuses the same broken environments as Listener, plus bad
// names.
func TestListenersRefuses(t *testing.T) {
	env := func(vars map[string]string) func(string) string {
		return func(k string) string { return vars[k] }
	}
	cases := map[string]map[string]string{
		"not socket-activated":      {},
		"meant for another process": {"LISTEN_PID": "1", "LISTEN_FDS": "1", "LISTEN_FDNAMES": "a"},
		"zero sockets":              {"LISTEN_PID": "4242", "LISTEN_FDS": "0", "LISTEN_FDNAMES": ""},
		"too many sockets":          {"LISTEN_PID": "4242", "LISTEN_FDS": "9", "LISTEN_FDNAMES": "a:b:c:d:e:f:g:h:i"},
		"no names":                  {"LISTEN_PID": "4242", "LISTEN_FDS": "2"},
		"too few names":             {"LISTEN_PID": "4242", "LISTEN_FDS": "2", "LISTEN_FDNAMES": "a"},
		"empty name":                {"LISTEN_PID": "4242", "LISTEN_FDS": "2", "LISTEN_FDNAMES": "a:"},
	}
	for name, vars := range cases {
		if ls, err := Listeners(4242, env(vars)); err == nil {
			for _, l := range ls {
				l.Close()
			}
			t.Errorf("%s: got listeners, want an error", name)
		}
	}
}

// Two real sockets handed over the way systemd does it (descriptors 3 and
// 4, in a child process) come back under their names. A repeated name is
// refused. The test runs itself as the child.
func TestListenersTakesOverNamedSockets(t *testing.T) {
	if os.Getenv("SDLISTEN_CHILD") == "1" {
		childListeners()
		return
	}
	dir := t.TempDir()
	var files []*os.File
	for _, name := range []string{"api", "agent"} {
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
	for names, want := range map[string]string{
		"waystone-controller-api:waystone-controller-agent": "ok",
		"same:same": "error",
	} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestListenersTakesOverNamedSockets$")
		cmd.Env = append(os.Environ(), "SDLISTEN_CHILD=1", "LISTEN_FDS=2", "LISTEN_FDNAMES="+names)
		cmd.ExtraFiles = files
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: child: %v\n%s", names, err, out)
		}
		if !strings.Contains(string(out), "RESULT "+want) {
			t.Errorf("%s: child said %q, want RESULT %s", names, out, want)
		}
	}
}

// childListeners runs in the child: LISTEN_PID is its own pid, which the
// parent can't know in advance, so it's filled in here.
func childListeners() {
	getenv := func(k string) string {
		if k == "LISTEN_PID" {
			return strconv.Itoa(os.Getpid())
		}
		return os.Getenv(k)
	}
	ls, err := Listeners(os.Getpid(), getenv)
	if err != nil {
		os.Stdout.WriteString("RESULT error " + err.Error() + "\n")
		return
	}
	api := ls["waystone-controller-api"]
	agent := ls["waystone-controller-agent"]
	if len(ls) == 2 && api != nil && agent != nil &&
		strings.HasSuffix(api.Addr().String(), "api.sock") && strings.HasSuffix(agent.Addr().String(), "agent.sock") {
		os.Stdout.WriteString("RESULT ok\n")
		return
	}
	os.Stdout.WriteString("RESULT wrong\n")
}

// A TCP socket is taken only under a name the caller allows as TCP, and a
// name allowed as TCP must be one (stage 6a). Descriptor 3 is a Unix
// socket, 4 a TCP one.
func TestListenersTCP(t *testing.T) {
	if os.Getenv("SDLISTEN_CHILD") == "tcp" {
		getenv := func(k string) string {
			if k == "LISTEN_PID" {
				return strconv.Itoa(os.Getpid())
			}
			return os.Getenv(k)
		}
		var allow []string
		if a := os.Getenv("SDLISTEN_ALLOW"); a != "" {
			allow = strings.Split(a, ",")
		}
		ls, err := Listeners(os.Getpid(), getenv, allow...)
		if err != nil {
			os.Stdout.WriteString("RESULT error " + err.Error() + "\n")
			return
		}
		if _, ok := ls["tcp"].(*net.TCPListener); ok && len(ls) == 2 {
			os.Stdout.WriteString("RESULT ok\n")
			return
		}
		os.Stdout.WriteString("RESULT wrong\n")
		return
	}
	u, err := net.Listen("unix", filepath.Join(t.TempDir(), "u.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	tl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tl.Close()
	uf, _ := u.(*net.UnixListener).File()
	tf, _ := tl.(*net.TCPListener).File()
	defer uf.Close()
	defer tf.Close()
	for allow, want := range map[string]string{
		"tcp":      "ok",
		"":         "error", // TCP not allowed
		"unix,tcp": "error", // the Unix one named as TCP
	} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestListenersTCP$")
		cmd.Env = append(os.Environ(), "SDLISTEN_CHILD=tcp", "LISTEN_FDS=2", "LISTEN_FDNAMES=unix:tcp", "SDLISTEN_ALLOW="+allow)
		cmd.ExtraFiles = []*os.File{uf, tf}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%q: child: %v\n%s", allow, err, out)
		}
		if !strings.Contains(string(out), "RESULT "+want) {
			t.Errorf("allow %q: child said %q, want RESULT %s", allow, out, want)
		}
	}
}
