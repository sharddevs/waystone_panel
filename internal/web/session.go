// SPDX-License-Identifier: AGPL-3.0-only

package web

import (
	"context"
	"crypto/subtle"
	"errors"
	"log"
	"net/http"
	"time"
)

// CSRFHeader carries the session's CSRF token on every unsafe request
// (C-W5). The token is never in a URL or a cookie the page can read: the
// UI gets it from GET /api/v1/session.
const CSRFHeader = "X-CSRF-Token"

// TokenLength is the length of a session token and of a CSRF token: 32
// random bytes in unpadded base64url.
const TokenLength = 43

// Session is a signed-in browser session, as the session store found it.
type Session struct {
	ID          string
	UserID      string
	TenantID    string
	DisplayName string
	CSRFToken   string
	CreatedAt   time.Time
	ExpiresAt   time.Time
}

// ErrNoSession is what a SessionResolver returns for a token with no live
// session behind it: unknown, expired, signed out, or a disabled account.
var ErrNoSession = errors.New("web: no session")

// SessionResolver finds the session for the cookie's token. It owns the
// session rules (hashing, idle and absolute expiry); this package only
// carries the token to it and the result to the handler. token is always
// TokenLength characters of base64url. Any error other than ErrNoSession
// means the store couldn't answer, and the request gets a 503.
type SessionResolver interface {
	ResolveSession(ctx context.Context, token string) (Session, error)
}

type sessionKey struct{}

// SessionFrom returns the request's session. It's only set on a route
// declared with Session (or CSRFRequired), where New guarantees it.
func SessionFrom(r *http.Request) (Session, bool) {
	s, ok := r.Context().Value(sessionKey{}).(Session)
	return s, ok
}

// SessionToken returns the session token from the request's cookie, if
// there is one of the right shape. It is for sign-out and the session
// routes; every other route uses SessionFrom.
func SessionToken(r *http.Request) (string, bool) {
	c, err := r.Cookie(SessionCookieName)
	if err != nil || !validToken(c.Value) {
		return "", false
	}
	return c.Value, true
}

// requireSession wraps a route that needs a signed-in user. Without a
// live session it's a 401. With csrf, the X-CSRF-Token header must equal
// the session's token, compared in constant time, or it's a 403.
func requireSession(sessions SessionResolver, csrf bool, logger *log.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := SessionToken(r)
		if !ok {
			writeError(w, CodeUnauthenticated)
			return
		}
		s, err := sessions.ResolveSession(r.Context(), token)
		if errors.Is(err, ErrNoSession) {
			writeError(w, CodeUnauthenticated)
			return
		}
		if err != nil {
			logger.Printf("web session lookup failed: %v", err)
			writeError(w, CodeBusy)
			return
		}
		if csrf && !csrfMatches(r.Header.Values(CSRFHeader), s.CSRFToken) {
			writeError(w, CodeCSRF)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), sessionKey{}, s)))
	})
}

// csrfMatches: exactly one header value, of the right shape, equal to the
// session's token.
func csrfMatches(got []string, want string) bool {
	if len(got) != 1 || !validToken(got[0]) || !validToken(want) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got[0]), []byte(want)) == 1
}

// validToken: exactly TokenLength characters of unpadded base64url.
func validToken(s string) bool {
	if len(s) != TokenLength {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// SetSessionCookie sends the session cookie, built by SessionCookie, so
// code outside this package never touches http.SetCookie (C-W2).
func SetSessionCookie(w http.ResponseWriter, token string, maxAge time.Duration) error {
	if !validToken(token) {
		return errors.New("web: a session token is 43 characters of base64url")
	}
	c, err := SessionCookie(token, maxAge)
	if err != nil {
		return err
	}
	http.SetCookie(w, c)
	return nil
}

// ClearSession tells the browser to delete the session cookie.
func ClearSession(w http.ResponseWriter) { http.SetCookie(w, ClearSessionCookie()) }
