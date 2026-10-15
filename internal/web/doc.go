// SPDX-License-Identifier: AGPL-3.0-only

// Package web is the controller's public, browser-facing side (stage WS).
//
// Browsers never reach the controller directly. Caddy terminates HTTPS and
// forwards over a private Unix socket (web.sock) that only the proxy's
// user may use. This package owns everything about that path that matters
// for security:
//
//   - the canonical public origin, parsed once from WAYSTONE_PUBLIC_URL
//     (ParsePublicURL);
//   - the guard every request meets first: Host, forwarding headers and,
//     for unsafe methods, the browser's Origin;
//   - the security headers on every response (HSTS, CSP and the rest);
//   - the only ways to build a session cookie, a redirect, or check a
//     WebSocket's Origin.
//
// The handler can only be built with New, and routes only added through
// it, so a browser route can't skip the guard. The local admin API
// (internal/api, api.sock) is a different identity model, where the
// kernel's peer uid is the actor. Here the peer is the proxy, which is
// never an actor, so the two packages never share a handler.
package web
