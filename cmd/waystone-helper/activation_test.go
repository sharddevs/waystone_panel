//go:build linux

package main

import "testing"

func TestActivationListenerRefusesForeignSockets(t *testing.T) {
	env := func(vars map[string]string) func(string) string {
		return func(k string) string { return vars[k] }
	}
	cases := map[string]map[string]string{
		"not socket-activated":      {},
		"meant for another process": {"LISTEN_PID": "1", "LISTEN_FDS": "1"},
		"two sockets":               {"LISTEN_PID": "4242", "LISTEN_FDS": "2"},
		"no socket count":           {"LISTEN_PID": "4242"},
	}
	for name, vars := range cases {
		if l, err := activationListener(4242, env(vars)); err == nil {
			l.Close()
			t.Errorf("%s: got a listener, want an error", name)
		}
	}
}
