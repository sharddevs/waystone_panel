// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
)

// MaxBodyBytes is the largest request body the API reads (I12).
const MaxBodyBytes = 64 << 10

// decodeJSON reads r's body into dst, strictly. It returns nil or an
// *apiError:
//
//   - No body at all (empty) is fine: dst stays as it is. Start, stop and
//     restart are usually sent without one.
//   - A body needs Content-Type application/json (parameters such as
//     charset are allowed): otherwise unsupported_media_type.
//   - More than MaxBodyBytes: body_too_large.
//   - Exactly one JSON object (not null, an array or a string) with no
//     unknown fields and nothing after it: otherwise bad_request.
//
// TODO: read through http.MaxBytesReader (an *http.MaxBytesError means too
// large), check the media type with mime.ParseMediaType, and check that
// the first byte that isn't white space is '{' (Decode alone would accept
// null into a struct). Then decode with a json.Decoder that has
// DisallowUnknownFields. After the object, a second Decode must return
// io.EOF, or there was trailing data.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) *apiError {
	r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)

	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return newError(CodeBodyTooLarge, err)
		}
		return newError(CodeBadRequest, err)
	}

	// An entirely empty body is allowed.
	if len(body) == 0 {
		return nil
	}

	// A nonempty body must be application/json.
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return newError(CodeUnsupportedMediaType, err)
	}

	// JSON whitespace is limited to space, tab, CR and LF.
	trimmed := bytes.Trim(body, " \t\r\n")

	// Only JSON objects are accepted.
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return newError(CodeBadRequest, nil)
	}

	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(dst); err != nil {
		return newError(CodeBadRequest, err)
	}

	// Reject a second JSON value or any trailing garbage.
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return newError(CodeBadRequest, err)
	}

	return nil
}
