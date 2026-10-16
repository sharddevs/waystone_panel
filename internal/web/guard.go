// SPDX-License-Identifier: AGPL-3.0-only

package web

import (
	"context"
	"net/http"
	"net/netip"
	"strings"
)

// The forwarding headers the supported Caddyfile sets on every request
// (WS scope §4a, R1). Each must arrive exactly once.
const (
	headerXFF   = "X-Forwarded-For"
	headerXFP   = "X-Forwarded-Proto"
	headerXFH   = "X-Forwarded-Host"
	headerOrig  = "Origin"
	headerFetch = "Sec-Fetch-Site"
)

// strippedHeaders are removed by the supported Caddyfile, so they never
// reach Waystone from a correct deployment. If one arrives anyway, the
// proxy isn't the supported one (or was bypassed), and the request fails
// closed. Keys are in canonical form.
var strippedHeaders = []string{
	"Forwarded",
	"X-Real-Ip",
	"X-Forwarded-Port",
	"X-Forwarded-Server",
	"X-Forwarded-Prefix",
	"X-Original-Url",
	"X-Original-Host",
	"X-Rewrite-Url",
	"X-Client-Ip",
	"True-Client-Ip",
	"Cf-Connecting-Ip",
}

// requestInfo is what the guard established about a request. Handlers get
// at it through ClientIP and the helpers in this package, never by reading
// the headers again.
type requestInfo struct {
	origin   Origin
	clientIP netip.Addr
}

type infoKey struct{}

func infoFrom(ctx context.Context) (requestInfo, bool) {
	ri, ok := ctx.Value(infoKey{}).(requestInfo)
	return ri, ok
}

// ClientIP is the browser's address as the trusted proxy reported it. It
// is metadata for logs and audit records, and the key of the sign-in rate
// limit, which is its one allowed security use: the guard has already
// checked that the proxy is the trusted one, and a wrong address there
// only moves an attempt to another bucket. Never use it to identify or
// authorize anyone (W3).
func ClientIP(r *http.Request) (netip.Addr, bool) {
	ri, ok := infoFrom(r.Context())
	if !ok {
		return netip.Addr{}, false
	}
	return ri.clientIP, true
}

// refusal says why the guard refused a request: the public code, and a
// reason for the log only.
type refusal struct {
	code   string
	reason string
}

// check is the guard's decision for one request (WS scope §3, §4a). It
// runs after the connection was accepted from the proxy's uid, and in
// this order:
//
//  1. the request target is origin-form ("/path"), never absolute-form;
//  2. Host names the canonical origin (else 421);
//  3. no header the supported proxy strips is present;
//  4. X-Forwarded-Proto is exactly one value, equal to the canonical
//     scheme;
//  5. X-Forwarded-Host is exactly one value naming the canonical origin;
//  6. X-Forwarded-For is exactly one value holding one IP address;
//  7. for an unsafe method, the browser's Origin is the canonical origin
//     (W9).
func check(o Origin, r *http.Request) (netip.Addr, *refusal) {
	if r.URL.Host != "" || !strings.HasPrefix(r.RequestURI, "/") {
		return netip.Addr{}, &refusal{CodeBadRequest, "request target is not origin-form"}
	}
	if !o.matchAuthority(r.Host) {
		return netip.Addr{}, &refusal{CodeMisdirected, "host is not the public origin"}
	}
	for _, h := range strippedHeaders {
		if _, present := r.Header[h]; present {
			return netip.Addr{}, &refusal{CodeBadRequest, "header " + h + " present; the supported proxy removes it"}
		}
	}
	proto, ok := single(r.Header, headerXFP)
	if !ok || proto != o.scheme {
		return netip.Addr{}, &refusal{CodeBadRequest, "X-Forwarded-Proto is not exactly " + o.scheme}
	}
	fwdHost, ok := single(r.Header, headerXFH)
	if !ok || !o.matchAuthority(fwdHost) {
		return netip.Addr{}, &refusal{CodeBadRequest, "X-Forwarded-Host is not exactly the public origin"}
	}
	ipText, ok := single(r.Header, headerXFF)
	if !ok {
		return netip.Addr{}, &refusal{CodeBadRequest, "X-Forwarded-For is not exactly one value"}
	}
	ip, err := netip.ParseAddr(ipText)
	if err != nil || ip.Zone() != "" {
		return netip.Addr{}, &refusal{CodeBadRequest, "X-Forwarded-For is not one IP address"}
	}
	if unsafeMethod(r.Method) {
		if reason := checkOrigin(o, r.Header); reason != "" {
			return ip, &refusal{CodeForbidden, reason}
		}
	}
	return ip, nil
}

// single returns a header's value if it was sent exactly once, holds no
// list (no comma) and isn't empty.
func single(h http.Header, key string) (string, bool) {
	vs := h[key]
	if len(vs) != 1 {
		return "", false
	}
	v := vs[0]
	if v == "" || strings.ContainsRune(v, ',') {
		return "", false
	}
	return v, true
}

// unsafeMethod reports whether a request may change state. Anything that
// isn't GET, HEAD or OPTIONS counts, unknown methods included.
func unsafeMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	return true
}

// checkOrigin is the W9 rule for unsafe methods: Origin must be exactly
// the canonical origin. Without an Origin header, only a browser's own
// Sec-Fetch-Site: same-origin is accepted. It returns why it refused, or
// "". This is not CSRF protection by itself; stage 5 adds a token.
func checkOrigin(o Origin, h http.Header) string {
	if vs, present := h[headerOrig]; present {
		if len(vs) != 1 || vs[0] != o.String() {
			return "Origin is not the public origin"
		}
		return ""
	}
	if vs := h[headerFetch]; len(vs) == 1 && vs[0] == "same-origin" {
		return ""
	}
	return "unsafe method without a same-origin Origin or Sec-Fetch-Site"
}

// CheckWebSocketOrigin is the Origin rule for a WebSocket upgrade: the
// Origin header is required and must be exactly the canonical origin.
// Browsers always send it on a WebSocket handshake. A valid Origin is not
// authentication: the upgrade must also require an authenticated actor
// (contract C-W6, stage 7).
func CheckWebSocketOrigin(r *http.Request) bool {
	ri, ok := infoFrom(r.Context())
	if !ok {
		return false
	}
	vs := r.Header[headerOrig]
	return len(vs) == 1 && vs[0] == ri.origin.String()
}
