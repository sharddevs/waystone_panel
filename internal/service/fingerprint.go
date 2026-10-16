// SPDX-License-Identifier: AGPL-3.0-only

package service

import (
	"crypto/sha256"
	"encoding/json"
)

// Fingerprint is the SHA-256 that identifies one request for idempotency
// (contract C1):
//
//	SHA-256(method "\n" route "\n" serverID "\n" canonical body)
//
// route is the route template ("/api/v1/servers/{id}/start"), not the
// path, so the server ID only counts once, through serverID. body is the
// already decoded request struct. It is encoded again with encoding/json,
// which always writes struct fields in the same order and without extra
// whitespace, so two bodies that differ only in key order or spacing get
// the same fingerprint.
//
// TODO: json.Marshal the body, then hash the four parts joined by "\n".
func Fingerprint(method, route, serverID string, body any) ([32]byte, error) {
	canonicalBody, err := json.Marshal(body)
	if err != nil {
		return [32]byte{}, err
	}

	data := method + "\n" + route + "\n" + serverID + "\n" + string(canonicalBody)

	return sha256.Sum256([]byte(data)), nil
}
