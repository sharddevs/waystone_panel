// SPDX-License-Identifier: AGPL-3.0-only

package web

import (
	"net/http"
	"strconv"
)

// ContentSecurityPolicy is sent on every response (W8). It allows nothing
// but same-origin scripts, styles, images (plus data: images), fetches and
// form posts, and no framing at all. A frontend build must not need
// inline scripts or styles.
//
// Changing this is a security decision: it may only change in a reviewed,
// narrowly justified diff together with its exact-value test, never
// weakened without explicit review (WS scope §4a).
const ContentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'; object-src 'none'"

// HSTS limits (W6). The default is deliberately short while a deployment
// is being validated; an administrator raises it once HTTPS is known to
// work. includeSubDomains and preload are never sent and can't be set.
const (
	DefaultHSTSMaxAge = 300
	MaxHSTSMaxAge     = 63072000 // two years
)

// baselineHeaders are on every response, refusals included.
var baselineHeaders = [][2]string{
	{"Content-Security-Policy", ContentSecurityPolicy},
	{"X-Content-Type-Options", "nosniff"},
	{"Referrer-Policy", "same-origin"},
	{"X-Frame-Options", "DENY"},
	{"Cross-Origin-Opener-Policy", "same-origin"},
	{"Cross-Origin-Resource-Policy", "same-origin"},
	{"Cache-Control", "no-store"},
}

func setBaselineHeaders(h http.Header) {
	for _, kv := range baselineHeaders {
		h.Set(kv[0], kv[1])
	}
}

// hstsValue is the Strict-Transport-Security value for maxAge seconds.
func hstsValue(maxAge int) string { return "max-age=" + strconv.Itoa(maxAge) }
