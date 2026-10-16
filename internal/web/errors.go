// SPDX-License-Identifier: AGPL-3.0-only

package web

import (
	"encoding/json"
	"net/http"
)

// The public error codes of the browser-facing side. A refusal never says
// more than its code: in particular it never echoes the Host or a header
// value back.
const (
	CodeBadRequest  = "bad_request"
	CodeMisdirected = "misdirected_request"
	CodeForbidden   = "forbidden"
	CodeNotFound    = "not_found"

	// Sign-in and sessions (5a-2).
	CodeUnauthenticated    = "unauthenticated"
	CodeInvalidCredentials = "invalid_credentials"
	CodeCSRF               = "csrf_failed"
	CodeTooManyRequests    = "too_many_requests"
	CodeUnsupportedMedia   = "unsupported_media_type"
	CodeBusy               = "controller_busy"
)

var errorStatus = map[string]struct {
	status  int
	message string
}{
	CodeBadRequest:  {http.StatusBadRequest, "The request is malformed."},
	CodeMisdirected: {http.StatusMisdirectedRequest, "This server does not answer for that host."},
	CodeForbidden:   {http.StatusForbidden, "This request is not allowed from here."},
	CodeNotFound:    {http.StatusNotFound, "Not found."},

	CodeUnauthenticated:    {http.StatusUnauthorized, "Sign in first."},
	CodeInvalidCredentials: {http.StatusUnauthorized, "Username or password is wrong."},
	CodeCSRF:               {http.StatusForbidden, "The request's CSRF token is missing or wrong."},
	CodeTooManyRequests:    {http.StatusTooManyRequests, "Too many failed sign-ins. Try again later."},
	CodeUnsupportedMedia:   {http.StatusUnsupportedMediaType, "The request body must be JSON."},
	CodeBusy:               {http.StatusServiceUnavailable, "The controller is busy. Try again."},
}

// WriteError sends one of this package's error codes as the browser API's
// JSON error, with its fixed status and message. An unknown code is sent
// as bad_request.
func WriteError(w http.ResponseWriter, code string) { writeError(w, code) }

// WriteJSON sends v as JSON with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) { writeJSON(w, status, v) }

func writeError(w http.ResponseWriter, code string) {
	info, ok := errorStatus[code]
	if !ok {
		info, code = errorStatus[CodeBadRequest], CodeBadRequest
	}
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	body.Error.Code = code
	body.Error.Message = info.message
	writeJSON(w, info.status, body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
