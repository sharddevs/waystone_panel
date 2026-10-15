// SPDX-License-Identifier: AGPL-3.0-only

//go:build devweb

package web

// devHTTP is true only in builds with -tags devweb. They also accept
// http://localhost, http://127.0.0.1 and http://[::1] as the public URL,
// so a development frontend works without certificates. Everything else
// still goes through web.sock and the full guard (W7).
const devHTTP = true
