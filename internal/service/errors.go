// SPDX-License-Identifier: AGPL-3.0-only

package service

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/sharddevs/waystone_panel/internal/store"
)

// The public error codes (decision L). A client may rely on these strings
// and on the status that goes with each; never change one once released.
const (
	CodeBadRequest                 = "bad_request"
	CodeInvalidIdempotencyKey      = "invalid_idempotency_key"
	CodeForbidden                  = "forbidden"
	CodeNotFound                   = "not_found"
	CodeOperationInProgress        = "operation_in_progress"
	CodePreviousOperationUncertain = "previous_operation_uncertain"
	CodeNotUncertain               = "not_uncertain"
	CodeAlreadyResolved            = "already_resolved"
	CodeBodyTooLarge               = "body_too_large"
	CodeUnsupportedMediaType       = "unsupported_media_type"
	CodeIdempotencyKeyReused       = "idempotency_key_reused"
	CodeRateLimited                = "rate_limited"
	CodeControllerBusy             = "controller_busy"
	CodeNodeUnreachable            = "node_unreachable"
	CodeNodeRevoked                = "node_revoked"
	CodeEnrollmentUnavailable      = "enrollment_unavailable"
	CodeInternal                   = "internal"
)

// errorInfo is the fixed status and message for each code. The message is
// the same every time, so it can't leak anything about the database or
// the host. Details go to the log under the request ID instead.
var errorInfo = map[string]struct {
	status  int
	message string
}{
	CodeBadRequest:                 {http.StatusBadRequest, "The request is malformed."},
	CodeInvalidIdempotencyKey:      {http.StatusBadRequest, "The Idempotency-Key header must be 1 to 64 characters of A-Z, a-z, 0-9, _ or -."},
	CodeForbidden:                  {http.StatusForbidden, "You are not allowed to do this."},
	CodeNotFound:                   {http.StatusNotFound, "Not found."},
	CodeOperationInProgress:        {http.StatusConflict, "Another operation on this server is still in progress."},
	CodePreviousOperationUncertain: {http.StatusConflict, "The previous operation on this server has an unknown outcome. Resolve it first."},
	CodeNotUncertain:               {http.StatusConflict, "Only an uncertain operation can be resolved."},
	CodeAlreadyResolved:            {http.StatusConflict, "This operation has already been resolved."},
	CodeBodyTooLarge:               {http.StatusRequestEntityTooLarge, "The request body is too large."},
	CodeUnsupportedMediaType:       {http.StatusUnsupportedMediaType, "The request body must be application/json."},
	CodeIdempotencyKeyReused:       {http.StatusUnprocessableEntity, "This Idempotency-Key was already used for a different request."},
	CodeRateLimited:                {http.StatusTooManyRequests, "Too many requests. Try again later."},
	CodeControllerBusy:             {http.StatusServiceUnavailable, "The controller is busy. Try again shortly."},
	CodeNodeUnreachable:            {http.StatusConflict, "The server's node is not connected. Try again once it is back."},
	CodeNodeRevoked:                {http.StatusConflict, "The server's node has been revoked."},
	CodeEnrollmentUnavailable:      {http.StatusConflict, "Nodes can't be added: the controller has no agent listener on a reachable address."},
	CodeInternal:                   {http.StatusInternalServerError, "Internal error."},
}

// ErrorStatus is the fixed HTTP status and message sent with code, and
// whether code is one of this package's codes.
func ErrorStatus(code string) (status int, message string, ok bool) {
	info, ok := errorInfo[code]
	return info.status, info.message, ok
}

// Error is an error on its way to the client: a public code, and for a
// 409 caused by a blocking operation, that operation's ID. cause is the
// real reason, which only goes to the log.
type Error struct {
	code        string
	operationID string
	cause       error
}

func (e *Error) Error() string {
	if e.cause != nil {
		return e.code + ": " + e.cause.Error()
	}
	return e.code
}

func (e *Error) Unwrap() error { return e.cause }

// NewError is an Error without an operation ID, for the socket layers'
// own refusals (a missing peer, say).
func NewError(code string, cause error) *Error {
	return &Error{code: code, cause: cause}
}

func newError(code string, cause error) *Error { return NewError(code, cause) }

// errorBody is the JSON shape of every error response.
type errorBody struct {
	Error struct {
		Code        string `json:"code"`
		Message     string `json:"message"`
		RequestID   string `json:"request_id"`
		OperationID string `json:"operation_id,omitempty"`
	} `json:"error"`
}

// writeError sends e to the client and logs its cause under the
// request's ID. An unknown code is sent as "internal", so a typo can't
// produce a response outside the contract. Both sockets send every error
// through here, so they look the same on both (E1).
func (s *Service) writeError(w http.ResponseWriter, r *http.Request, e *Error) {
	info, ok := errorInfo[e.code]
	if !ok {
		e = &Error{code: CodeInternal, cause: e}
		info = errorInfo[CodeInternal]
	}
	var body errorBody
	body.Error.Code = e.code
	body.Error.Message = info.message
	body.Error.RequestID = requestIDFrom(r.Context())
	body.Error.OperationID = e.operationID

	if e.cause != nil || info.status >= 500 {
		s.logf("request_id=%s error=%s cause=%v", body.Error.RequestID, e.code, e.cause)
	}
	if e.code == CodeRateLimited {
		w.Header().Set("Retry-After", "60")
	}
	writeJSON(w, info.status, body)
}

// writeJSON sends v as JSON with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// errorFor turns an error from the store (or from this package) into the
// Error to send. An *Error passes through unchanged.
//
// TODO: map each store error to its code:
//   - *store.InProgressError: previous_operation_uncertain if its State is
//     StateUncertain, otherwise operation_in_progress; set operationID.
//   - ErrOperationInProgress without the details: operation_in_progress.
//   - ErrNoMembership, ErrAmbiguousMembership: forbidden.
//   - ErrNotFound: not_found.  ErrInvalidInput: bad_request.
//   - ErrIdempotencyKeyReused: idempotency_key_reused.
//   - ErrNotUncertain: not_uncertain.  ErrAlreadyResolved: already_resolved.
//   - ErrNodeUnreachable: node_unreachable (no operation ID).
//   - ErrNodeRevoked: node_revoked.  ErrNotAdmin: forbidden (run
//     refuses non-admins first; this is the store's own check).
//   - anything else (ErrInvalidActor included: the actor comes from the
//     kernel, so a bad one is our bug): internal.
//
// Use errors.As and errors.Is, because the store wraps its errors. Keep the
// original error as cause, so the log shows what really happened.
func errorFor(err error) *Error {
	var apiErr *Error
	if errors.As(err, &apiErr) {
		return apiErr
	}

	var inProgress *store.InProgressError
	if errors.As(err, &inProgress) {
		code := CodeOperationInProgress
		if inProgress.State == store.StateUncertain {
			code = CodePreviousOperationUncertain
		}

		return &Error{
			code:        code,
			operationID: inProgress.OperationID,
			cause:       err,
		}
	}

	switch {
	case errors.Is(err, store.ErrOperationInProgress):
		return newError(CodeOperationInProgress, err)

	case errors.Is(err, store.ErrNoMembership),
		errors.Is(err, store.ErrAmbiguousMembership):
		return newError(CodeForbidden, err)

	case errors.Is(err, store.ErrNotFound):
		return newError(CodeNotFound, err)

	case errors.Is(err, store.ErrInvalidInput):
		return newError(CodeBadRequest, err)

	case errors.Is(err, store.ErrIdempotencyKeyReused):
		return newError(CodeIdempotencyKeyReused, err)

	case errors.Is(err, store.ErrNotUncertain):
		return newError(CodeNotUncertain, err)

	case errors.Is(err, store.ErrAlreadyResolved):
		return newError(CodeAlreadyResolved, err)

	case errors.Is(err, store.ErrNodeUnreachable):
		return newError(CodeNodeUnreachable, err)

	case errors.Is(err, store.ErrNodeRevoked):
		return newError(CodeNodeRevoked, err)

	case errors.Is(err, store.ErrNotAdmin):
		return newError(CodeForbidden, err)

	default:
		return newError(CodeInternal, err)
	}
}
