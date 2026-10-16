// SPDX-License-Identifier: AGPL-3.0-only

package api

import "github.com/sharddevs/waystone_panel/internal/service"

// The routes moved to internal/service in 5b. These names keep the API's
// tests reading as they did, so they prove api.sock still behaves exactly
// as before.
const (
	CodeBadRequest                 = service.CodeBadRequest
	CodeInvalidIdempotencyKey      = service.CodeInvalidIdempotencyKey
	CodeForbidden                  = service.CodeForbidden
	CodeNotFound                   = service.CodeNotFound
	CodeOperationInProgress        = service.CodeOperationInProgress
	CodePreviousOperationUncertain = service.CodePreviousOperationUncertain
	CodeNotUncertain               = service.CodeNotUncertain
	CodeAlreadyResolved            = service.CodeAlreadyResolved
	CodeBodyTooLarge               = service.CodeBodyTooLarge
	CodeUnsupportedMediaType       = service.CodeUnsupportedMediaType
	CodeIdempotencyKeyReused       = service.CodeIdempotencyKeyReused
	CodeRateLimited                = service.CodeRateLimited
	CodeControllerBusy             = service.CodeControllerBusy
	CodeNodeUnreachable            = service.CodeNodeUnreachable
	CodeInternal                   = service.CodeInternal

	RateLimit    = service.RateLimit
	RateWindow   = service.RateWindow
	MaxBodyBytes = service.MaxBodyBytes
)

type actionRequest = service.ActionRequest

var Fingerprint = service.Fingerprint
