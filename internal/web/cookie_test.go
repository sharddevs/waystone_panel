// SPDX-License-Identifier: AGPL-3.0-only

package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// W10: the session cookie's attributes, as a browser receives them.
func TestSessionCookie(t *testing.T) {
	c, err := SessionCookie("abcDEF012-_", 12*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	http.SetCookie(w, c)
	got := w.Header().Get("Set-Cookie")
	want := "__Host-waystone_session=abcDEF012-_; Path=/; Max-Age=43200; HttpOnly; Secure; SameSite=Lax"
	if got != want {
		t.Errorf("Set-Cookie %q\nwant        %q", got, want)
	}
	if strings.Contains(strings.ToLower(got), "domain") {
		t.Error("a Domain attribute would break the __Host- prefix")
	}

	c, err = SessionCookie("x", 0)
	if err != nil || c.MaxAge != 0 || c.Expires != (time.Time{}) {
		t.Errorf("browser-session cookie: %+v %v", c, err)
	}
	c, err = SessionCookie("x", 300*time.Millisecond)
	if err != nil || c.MaxAge != 1 {
		t.Errorf("sub-second max age rounds up to 1: %+v %v", c, err)
	}

	for _, v := range []string{"", strings.Repeat("a", 257), "a b", "a;b", "a=b", "a,b", "ä", "a\"b", "a\nb"} {
		if _, err := SessionCookie(v, time.Hour); err == nil {
			t.Errorf("value %q accepted", v)
		}
	}
	if _, err := SessionCookie("x", -time.Second); err == nil {
		t.Error("negative max age accepted")
	}
	if _, err := SessionCookie(strings.Repeat("a", 256), time.Hour); err != nil {
		t.Error("256 characters refused")
	}
}

func TestClearSessionCookie(t *testing.T) {
	w := httptest.NewRecorder()
	http.SetCookie(w, ClearSessionCookie())
	want := "__Host-waystone_session=; Path=/; Max-Age=0; HttpOnly; Secure; SameSite=Lax"
	if got := w.Header().Get("Set-Cookie"); got != want {
		t.Errorf("Set-Cookie %q\nwant        %q", got, want)
	}
}
