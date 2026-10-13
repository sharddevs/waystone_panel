// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"os/user"
	"strconv"
	"unicode"
	"unicode/utf8"

	"github.com/sharddevs/waystone_panel/internal/store"
)

// LookupName finds the login name for a uid. The real one reads the
// passwd database; tests pass a fake.
type LookupName func(uid string) (string, error)

// systemLookup is the LookupName used when Config doesn't set one.
func systemLookup(uid string) (string, error) {
	u, err := user.LookupId(uid)
	if err != nil {
		return "", err
	}
	return u.Username, nil
}

// actorFor builds the store actor for a peer. The ID is the uid in
// decimal and nothing else: the actor's identity comes only from the
// kernel (C5). The name is just for display, so if the lookup fails, or
// returns something that isn't a usable display name (empty, over 100
// bytes, invalid UTF-8, control characters), the name is "uid <n>"
// instead. A missing name never refuses a request.
//
// TODO: build the ID with strconv.FormatUint, call lookup, and fall back
// as described.
func actorFor(p Peer, lookup LookupName) store.Actor {
	uid := strconv.FormatUint(uint64(p.UID), 10)

	name, err := lookup(uid)
	if err != nil || !usableName(name) {
		name = "uid " + uid
	}

	return store.Actor{
		Type: "local_user",
		ID:   uid,
		Name: name,
	}
}

func usableName(s string) bool {
	if s == "" || len(s) > 100 || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
