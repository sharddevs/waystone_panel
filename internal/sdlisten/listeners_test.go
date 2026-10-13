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
