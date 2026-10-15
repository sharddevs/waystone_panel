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
)

var errorStatus = map[string]struct {
	status  int
	message string
}{
	CodeBadRequest:  {http.StatusBadRequest, "The request is malformed."},
	CodeMisdirected: {http.StatusMisdirectedRequest, "This server does not answer for that host."},
	CodeForbidden:   {http.StatusForbidden, "This request is not allowed from here."},
	CodeNotFound:    {http.StatusNotFound, "Not found."},
}

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
