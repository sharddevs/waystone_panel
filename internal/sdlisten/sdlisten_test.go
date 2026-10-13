// SPDX-License-Identifier: AGPL-3.0-only

package sdlisten

import "testing"

// Without the right environment, Listener must refuse instead of grabbing
// whatever descriptor 3 happens to be.
func TestListenerRefusesWithoutActivation(t *testing.T) {
	env := func(vars map[string]string) func(string) string {
		return func(k string) string { return vars[k] }
	}
	cases := map[string]map[string]string{
		"not socket-activated":      {},
		"meant for another process": {"LISTEN_PID": "1", "LISTEN_FDS": "1"},
		"two sockets":               {"LISTEN_PID": "4242", "LISTEN_FDS": "2"},
		"zero sockets":              {"LISTEN_PID": "4242", "LISTEN_FDS": "0"},
		"no socket count":           {"LISTEN_PID": "4242"},
	}
	for name, vars := range cases {
		if l, err := Listener("test", 4242, env(vars)); err == nil {
			l.Close()
			t.Errorf("%s: got a listener, want an error", name)
		}
	}
}
