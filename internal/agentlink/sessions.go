// SPDX-License-Identifier: AGPL-3.0-only

package agentlink

import (
	"context"
	"sync"
)

// sessions holds the live session of each node (R1): its epoch and the
// function that ends it. One mutex guards the whole map, which is plenty
// at v0.1's scale.
//
// This only protects availability. A session with an old epoch can't
// write anything anyway: the database checks the epoch on every report
// (C3).
type sessions struct {
	mu   sync.Mutex
	live map[string]session
}

type session struct {
	epoch  int64
	cancel context.CancelCauseFunc
	kick   func() // wakes the session's dispatcher (4c); nil until set
}

func newSessions() *sessions {
	return &sessions{live: make(map[string]session)}
}

// install records epoch as node's live session, if it is newer than the
// one there. Then the old session is cancelled with errReplaced, and
// replaced says there was one. If epoch isn't newer, the new session is
// the one cancelled and won is false: a lower epoch never replaces or
// cancels a higher one, whatever order the hellos arrive in.
func (s *sessions) install(node string, epoch int64, cancel context.CancelCauseFunc) (won, replaced bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.live[node]
	if ok && epoch <= old.epoch {
		cancel(errReplaced)
		return false, false
	}
	if ok {
		old.cancel(errReplaced)
	}
	s.live[node] = session{epoch: epoch, cancel: cancel}
	return true, ok
}

// remove deletes node's session only if it is still epoch, so a session
// that ends late can't remove its successor.
func (s *sessions) remove(node string, epoch int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.live[node]; ok && cur.epoch == epoch {
		delete(s.live, node)
	}
}

// end cancels node's live session, whatever its epoch, with cause, and
// forgets it. A node with no session is left alone.
func (s *sessions) end(node string, cause error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.live[node]; ok {
		cur.cancel(cause)
		delete(s.live, node)
	}
}

// epoch returns node's live epoch, or 0.
func (s *sessions) epoch(node string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.live[node].epoch
}

// setKick attaches the dispatcher's wake-up to node's session, if it is
// still epoch.
func (s *sessions) setKick(node string, epoch int64, kick func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.live[node]; ok && cur.epoch == epoch {
		cur.kick = kick
		s.live[node] = cur
	}
}

// kick wakes node's live dispatcher, if there is one. It never blocks.
func (s *sessions) kick(node string) {
	s.mu.Lock()
	k := s.live[node].kick
	s.mu.Unlock()
	if k != nil {
		k()
	}
}
