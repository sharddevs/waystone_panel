// SPDX-License-Identifier: AGPL-3.0-only

package session

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"mime"
	"net/http"
	"net/netip"
	"strconv"
	"time"

	"github.com/sharddevs/waystone_panel/internal/ids"
	"github.com/sharddevs/waystone_panel/internal/password"
	"github.com/sharddevs/waystone_panel/internal/store"
	"github.com/sharddevs/waystone_panel/internal/web"
)

// PreferencesPattern is the route for the appearance settings.
const PreferencesPattern = web.LoginPattern + "/preferences"

// maxBody bounds a request body. A sign-in is a username and a password
// of at most 256 bytes, so this is generous.
const maxBody = 4 << 10

// Routes are the four session routes (scope §4).
func (m *Manager) Routes() []web.Route {
	return []web.Route{
		{Method: http.MethodPost, Pattern: web.LoginPattern, CSRF: web.CSRFPreSession, Handler: http.HandlerFunc(m.signIn)},
		{Method: http.MethodGet, Pattern: web.LoginPattern, CSRF: web.CSRFNotApplicable, Session: true, Handler: http.HandlerFunc(m.current)},
		{Method: http.MethodDelete, Pattern: web.LoginPattern, CSRF: web.CSRFRequired, Handler: http.HandlerFunc(m.signOut)},
		{Method: http.MethodPatch, Pattern: PreferencesPattern, CSRF: web.CSRFRequired, Handler: http.HandlerFunc(m.setPreferences)},
	}
}

// signIn is POST /api/v1/session: {"username", "password"} → 204 and the
// session cookie. Every refusal of a username or password is the same
// 401 with the same message, and costs one password hash, whether the
// account exists, is disabled, or the password is wrong.
func (m *Manager) signIn(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if code := decodeJSON(w, r, &body); code != "" {
		web.WriteError(w, code)
		return
	}
	ip, ok := web.ClientIP(r)
	if !ok {
		web.WriteError(w, web.CodeBadRequest)
		return
	}
	ctx := r.Context()
	now := m.now()
	// A string that can't be a username shares one bucket, so junk can't
	// grow the limiter's keys. It still gets the same answer and costs the
	// same hash below.
	userKey := "user:-"
	if store.ValidUsername(body.Username) {
		userKey = "user:" + body.Username
	}
	ipKey := addrKey(ip)
	// Counted now, before the lookup and the hash, so parallel guesses
	// can't all slip in under the limit (A1).
	counts, refused, wait := m.limits.attempt(now,
		limit{ipKey, "per_ip", limitPerIP}, limit{userKey, "per_user", limitPerUser})
	if refused != "" {
		m.log.Printf("session sign-in refused: rate limited limit=%s client_ip=%s", refused, ip)
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter(wait)))
		web.WriteError(w, web.CodeTooManyRequests)
		return
	}

	rec, err := m.store.LookupLogin(ctx, body.Username)
	known := err == nil
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		m.limits.refund(now, ipKey, userKey)
		m.log.Printf("session sign-in: login lookup failed: %v", err)
		web.WriteError(w, web.CodeBusy)
		return
	}
	phc := m.decoy
	if known {
		phc = rec.PasswordHash
	}
	match, err := m.verify(ctx, body.Password, phc)
	if errors.Is(err, password.ErrMalformed) {
		m.log.Printf("session sign-in: the stored hash for user %s is malformed", rec.UserID)
	} else if err != nil {
		m.limits.refund(now, ipKey, userKey)
		web.WriteError(w, web.CodeBusy)
		return
	}
	if !known || rec.Disabled || !match {
		// Already counted by attempt.
		reason := "unknown_user"
		switch {
		case !known:
		case rec.Disabled:
			reason = "account_disabled"
		case counts[1] >= limitPerUser:
			reason = "locked_out"
		default:
			reason = "wrong_password"
		}
		// Never the typed username: people type passwords into that box.
		who := "unknown"
		if known {
			who = rec.UserID
		}
		m.log.Printf("session sign-in refused: user=%s reason=%s client_ip=%s", who, reason, ip)
		web.WriteError(w, web.CodeInvalidCredentials)
		// The audit row is written after the response, so a known account
		// answers no later than an unknown one (A3).
		if known {
			m.auditFailure(rec, reason, ip)
		}
		return
	}
	// A success takes back only its own attempt. It doesn't clear the
	// username's other attempts: guesses racing the real user's sign-in
	// stay counted (VM review of audit fix 1).
	m.limits.refund(now, ipKey, userKey)

	// A sign-in always makes a new session (no fixation). The one the
	// browser came with, if any, is ended.
	m.endPresented(ctx, r)
	token, err := newToken()
	if err != nil {
		web.WriteError(w, web.CodeBusy)
		return
	}
	csrf, err := newToken()
	if err != nil {
		web.WriteError(w, web.CodeBusy)
		return
	}
	err = m.store.InTenantTx(ctx, rec.Actor(), func(ctx context.Context, tx *store.Tx) error {
		_, err := tx.CreateSession(ctx, store.NewSession{
			TokenSHA256: hashToken(token),
			CSRFToken:   csrf,
			ExpiresAt:   tx.Now().Add(AbsoluteTimeout),
			ClientIP:    ip.String(),
			UserAgent:   r.UserAgent(),
		})
		return err
	})
	if err != nil {
		m.log.Printf("session sign-in: creating the session for user %s failed: %v", rec.UserID, err)
		web.WriteError(w, web.CodeBusy)
		return
	}
	if err := web.SetSessionCookie(w, token, AbsoluteTimeout); err != nil {
		m.log.Printf("session sign-in: %v", err)
		web.WriteError(w, web.CodeBusy)
		return
	}
	m.log.Printf("session signed in: user=%s client_ip=%s", rec.UserID, ip)
	w.WriteHeader(http.StatusNoContent)
}

// endPresented deletes the session the request's cookie names, if there
// is one. It's best effort: a failure is logged, never shown.
func (m *Manager) endPresented(ctx context.Context, r *http.Request) {
	token, ok := web.SessionToken(r)
	if !ok {
		return
	}
	old, err := m.store.LookupSession(ctx, hashToken(token))
	if err != nil {
		return
	}
	err = m.store.InTenantTx(ctx, old.Actor(), func(ctx context.Context, tx *store.Tx) error {
		return tx.DeleteSession(ctx, old.SessionID)
	})
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		m.log.Printf("session sign-in: ending the previous session failed: %v", err)
	}
}

// auditFailure records a refused sign-in for an existing account, with
// action session.create (scope §3.6), in the background: the response has
// already gone. The row's actor is the account that was tried (the
// schema needs one); its client_ip is where the attempt came from (A6).
// Unknown usernames are only logged: there is no actor or tenant to file
// them under.
func (m *Manager) auditFailure(rec store.LoginRecord, reason string, ip netip.Addr) {
	m.audits.Add(1)
	go func() {
		defer m.audits.Done()
		ctx, cancel := context.WithTimeout(context.Background(), auditTimeout)
		defer cancel()
		requestID, err := ids.NewV7()
		if err == nil {
			err = m.store.InTenantTx(ctx, rec.Actor(), func(ctx context.Context, tx *store.Tx) error {
				return tx.RecordDenial(ctx, store.Denial{
					Action:       "session.create",
					ResourceType: "user",
					ResourceID:   rec.UserID,
					Reason:       reason,
					RequestID:    requestID,
					ClientIP:     ip.WithZone("").String(),
				})
			})
		}
		if err != nil {
			m.log.Printf("session sign-in: auditing a refusal for user %s failed: %v", rec.UserID, err)
		}
	}()
}

// addrKey is the per-address limiter key. An IPv4-mapped IPv6 address
// counts as its IPv4 address, and IPv6 counts by its /64: one home
// connection has a whole /64, and one key per address would give it 2^64
// fresh buckets (A4).
func addrKey(ip netip.Addr) string {
	ip = ip.Unmap().WithZone("")
	if ip.Is6() {
		p, _ := ip.Prefix(64)
		return "ip:" + p.String()
	}
	return "ip:" + ip.String()
}

// sessionJSON is GET /api/v1/session's body.
type sessionJSON struct {
	User struct {
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
	} `json:"user"`
	CSRFToken   string          `json:"csrf_token"`
	ExpiresAt   time.Time       `json:"expires_at"`
	Preferences preferencesJSON `json:"preferences"`
}

type preferencesJSON struct {
	Theme   string `json:"theme"`
	Density string `json:"density"`
	Mode    string `json:"mode"`
}

// current is GET /api/v1/session: who is signed in, the CSRF token for
// unsafe requests, and the appearance settings. web answers 401 before
// this runs when there is no session.
func (m *Manager) current(w http.ResponseWriter, r *http.Request) {
	s, _ := web.SessionFrom(r)
	var p store.Preferences
	err := m.store.InTenantTx(r.Context(), actorOf(s), func(ctx context.Context, tx *store.Tx) error {
		var err error
		p, err = tx.Preferences(ctx)
		return err
	})
	if err != nil {
		m.storeError(w, "reading preferences", err)
		return
	}
	var body sessionJSON
	body.User.ID = s.UserID
	body.User.DisplayName = s.DisplayName
	body.CSRFToken = s.CSRFToken
	body.ExpiresAt = s.ExpiresAt.UTC()
	body.Preferences = preferencesJSON(p)
	web.WriteJSON(w, http.StatusOK, body)
}

// signOut is DELETE /api/v1/session: the session row goes, and the
// browser is told to drop the cookie.
func (m *Manager) signOut(w http.ResponseWriter, r *http.Request) {
	s, _ := web.SessionFrom(r)
	err := m.store.InTenantTx(r.Context(), actorOf(s), func(ctx context.Context, tx *store.Tx) error {
		return tx.DeleteSession(ctx, s.ID)
	})
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		m.storeError(w, "signing out", err)
		return
	}
	web.ClearSession(w)
	w.WriteHeader(http.StatusNoContent)
}

// setPreferences is PATCH /api/v1/session/preferences: all three fields,
// each one of its fixed values, nothing else.
func (m *Manager) setPreferences(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Theme   *string `json:"theme"`
		Density *string `json:"density"`
		Mode    *string `json:"mode"`
	}
	if code := decodeJSON(w, r, &body); code != "" {
		web.WriteError(w, code)
		return
	}
	if body.Theme == nil || body.Density == nil || body.Mode == nil {
		web.WriteError(w, web.CodeBadRequest)
		return
	}
	p := store.Preferences{Theme: *body.Theme, Density: *body.Density, Mode: *body.Mode}
	if !p.Valid() {
		web.WriteError(w, web.CodeBadRequest)
		return
	}
	s, _ := web.SessionFrom(r)
	err := m.store.InTenantTx(r.Context(), actorOf(s), func(ctx context.Context, tx *store.Tx) error {
		return tx.SetPreferences(ctx, p)
	})
	if err != nil {
		m.storeError(w, "saving preferences", err)
		return
	}
	web.WriteJSON(w, http.StatusOK, preferencesJSON(p))
}

// storeError answers a failed store call on a signed-in route. An account
// that lost its membership since the lookup is signed out in effect.
func (m *Manager) storeError(w http.ResponseWriter, what string, err error) {
	if errors.Is(err, store.ErrNoMembership) || errors.Is(err, store.ErrNotFound) {
		web.WriteError(w, web.CodeUnauthenticated)
		return
	}
	m.log.Printf("session: %s failed: %v", what, err)
	web.WriteError(w, web.CodeBusy)
}

func actorOf(s web.Session) store.Actor {
	return store.Actor{Type: store.ActorUser, ID: s.UserID, Name: s.DisplayName}
}

// decodeJSON reads a JSON body into v strictly: Content-Type
// application/json, at most maxBody bytes, no unknown fields, one value.
// It returns the error code to send, or "".
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) string {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		return web.CodeUnsupportedMedia
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return web.CodeBadRequest
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return web.CodeBadRequest
	}
	return ""
}

// retryAfter is a wait in whole seconds, rounded up, at least 1.
func retryAfter(d time.Duration) int {
	s := int(math.Ceil(d.Seconds()))
	if s < 1 {
		return 1
	}
	return s
}
