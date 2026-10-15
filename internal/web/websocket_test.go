// SPDX-License-Identifier: AGPL-3.0-only

package web

import (
	"net/http"
	"testing"
)

// C-W6 (helper part): the WebSocket Origin rule. The authenticated
// wrapper arrives with the console (stage 7).
func TestCheckWebSocketOrigin(t *testing.T) {
	var got bool
	h, _ := newTestHandler(t, Route{Method: "GET", Pattern: "/ws", CSRF: CSRFNotApplicable,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = CheckWebSocketOrigin(r) })})
	for _, c := range []struct {
		origin []string
		want   bool
	}{
		{[]string{"https://panel.example.com"}, true},
		{nil, false},
		{[]string{""}, false},
		{[]string{"null"}, false},
		{[]string{"https://evil.test"}, false},
		{[]string{"http://panel.example.com"}, false},
		{[]string{"https://panel.example.com:443"}, false},
		{[]string{"https://PANEL.example.com"}, false},
		{[]string{"https://panel.example.com/"}, false},
		{[]string{"https://panel.example.com", "https://panel.example.com"}, false},
	} {
		got = false
		r := caddyRequest(http.MethodGet, "/ws")
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		if c.origin != nil {
			r.Header["Origin"] = c.origin
		}
		if w := serve(h, r); w.Code != http.StatusOK {
			t.Fatalf("status %d", w.Code)
		}
		if got != c.want {
			t.Errorf("Origin %q: %v, want %v", c.origin, got, c.want)
		}
	}
}
