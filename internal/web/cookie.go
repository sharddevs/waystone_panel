// SPDX-License-Identifier: AGPL-3.0-only

package web

import (
	"errors"
	"net/http"
	"time"
)

// SessionCookieName has the __Host- prefix, so browsers only accept it
// with Secure, Path=/ and no Domain: it can't be set by, or sent to, any
// other host (W10).
const SessionCookieName = "__Host-waystone_session"

// maxSessionValue bounds a session id. Stage 5 decides the format; this
// only keeps it a plain token.
const maxSessionValue = 256

// SessionCookie is the only way to build the session cookie (contract
// C-W2): Secure, HttpOnly, SameSite=Lax, Path=/, no Domain. value must be
// 1 to 256 characters of A-Z, a-z, 0-9, '-' or '_'. maxAge > 0 sets
// Max-Age in whole seconds; 0 makes a browser-session cookie.
func SessionCookie(value string, maxAge time.Duration) (*http.Cookie, error) {
	if value == "" || len(value) > maxSessionValue {
		return nil, errors.New("web: the session value must be 1 to 256 characters")
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return nil, errors.New("web: the session value may only use A-Z, a-z, 0-9, '-' and '_'")
		}
	}
	if maxAge < 0 {
		return nil, errors.New("web: a negative max age would delete the cookie; use ClearSessionCookie")
	}
	c := sessionCookie(value)
	if maxAge > 0 {
		c.MaxAge = int(maxAge / time.Second)
		if c.MaxAge == 0 {
			c.MaxAge = 1
		}
	}
	return c, nil
}

// ClearSessionCookie tells the browser to delete the session cookie. It
// has the same attributes, or a browser could keep the original.
func ClearSessionCookie() *http.Cookie {
	c := sessionCookie("")
	c.MaxAge = -1
	return c
}

func sessionCookie(value string) *http.Cookie {
	return &http.Cookie{
		Name:     SessionCookieName,
		Value:    value,
		Path:     "/",
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}
}
