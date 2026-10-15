// SPDX-License-Identifier: AGPL-3.0-only

package web

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// maxRedirectPath bounds a redirect target.
const maxRedirectPath = 2048

// SafeNext returns next if it is a same-origin path that is safe to
// redirect to after, say, a login, and "/" otherwise. It accepts only a
// path starting with a single "/": never a scheme, a host, "//" or "/\"
// (which browsers treat as another host), a backslash anywhere, control
// characters or spaces, or anything that becomes one of those after
// percent-decoding (contract C-W8).
func SafeNext(next string) string {
	if checkPath(next) != nil {
		return "/"
	}
	return next
}

// Redirect sends the browser to path on the canonical public origin, never
// to anything derived from the request's Host or forwarding headers. path
// must pass the same rules as SafeNext; otherwise the redirect goes to
// "/". code must be 302, 303 or 307; anything else becomes 303.
func Redirect(w http.ResponseWriter, r *http.Request, path string, code int) {
	ri, ok := infoFrom(r.Context())
	if !ok {
		// Only reachable if a handler runs outside New's chain, which the
		// package doesn't allow. Refuse rather than guess an origin.
		writeError(w, CodeBadRequest)
		return
	}
	switch code {
	case http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect:
	default:
		code = http.StatusSeeOther
	}
	w.Header().Set("Location", ri.origin.String()+SafeNext(path))
	w.WriteHeader(code)
}

func checkPath(p string) error {
	if p == "" || len(p) > maxRedirectPath || p[0] != '/' {
		return errors.New("not a path")
	}
	if strings.HasPrefix(p, "//") || strings.ContainsRune(p, '\\') {
		return errors.New("looks like another host")
	}
	for i := 0; i < len(p); i++ {
		if p[i] <= ' ' || p[i] == 0x7f {
			return errors.New("control character or space")
		}
	}
	u, err := url.Parse(p)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || u.Opaque != "" {
		return errors.New("not a plain path")
	}
	dec, err := url.PathUnescape(u.EscapedPath())
	if err != nil {
		return errors.New("bad escape")
	}
	if strings.HasPrefix(dec, "//") || strings.ContainsRune(dec, '\\') {
		return errors.New("decodes to another host")
	}
	for i := 0; i < len(dec); i++ {
		if dec[i] < ' ' || dec[i] == 0x7f {
			return errors.New("decodes to a control character")
		}
	}
	return nil
}
