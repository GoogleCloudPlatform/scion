// Package claim holds the one-time claim tokens that runners exchange for
// their GitHub JIT runner config (the mediator-to-runner contract in
// cr-ci/docs/architecture.md).
//
// A token is 32 lowercase hex characters (128 bits from crypto/rand). It is
// valid for one successful read within its TTL. The store keeps only the
// SHA-256 of each token, so the raw token exists in memory only for the
// moment it is handed to the launcher. Claims live in process memory: the
// mediator must run with max-instances=1, and a restart loses pending claims.
package claim

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

// TokenLen is the length of a claim token in hex characters.
const TokenLen = 32

type entry struct {
	jit     string
	jobID   int64
	expires time.Time
}

// Store is a concurrency-safe in-memory claim store.
type Store struct {
	ttl time.Duration
	now func() time.Time

	mu      sync.Mutex
	entries map[[sha256.Size]byte]entry
}

// NewStore returns a store whose claims expire ttl after Issue. now may be
// nil (time.Now).
func NewStore(ttl time.Duration, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{ttl: ttl, now: now, entries: map[[sha256.Size]byte]entry{}}
}

// Issue stores jit for jobID and returns a fresh single-use token.
func (s *Store) Issue(jobID int64, jit string) (string, error) {
	var b [TokenLen / 2]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b[:])
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[sha256.Sum256([]byte(token))] = entry{jit: jit, jobID: jobID, expires: s.now().Add(s.ttl)}
	return token, nil
}

// Consume returns the JIT config for token and deletes the claim. ok is false
// if the token is unknown, already used or expired.
func (s *Store) Consume(token string) (jit string, jobID int64, ok bool) {
	if len(token) != TokenLen {
		return "", 0, false
	}
	key := sha256.Sum256([]byte(token))
	s.mu.Lock()
	defer s.mu.Unlock()
	e, found := s.entries[key]
	if !found {
		return "", 0, false
	}
	delete(s.entries, key)
	if !s.now().Before(e.expires) {
		return "", 0, false
	}
	return e.jit, e.jobID, true
}

// DropJob deletes every claim for jobID (launch failed or job cancelled).
func (s *Store) DropJob(jobID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, e := range s.entries {
		if e.jobID == jobID {
			delete(s.entries, k)
		}
	}
}

// Pending reports whether jobID has an unexpired, unused claim.
func (s *Store) Pending(jobID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for _, e := range s.entries {
		if e.jobID == jobID && now.Before(e.expires) {
			return true
		}
	}
	return false
}

// Sweep deletes expired claims and returns how many it removed.
func (s *Store) Sweep() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	n := 0
	for k, e := range s.entries {
		if !now.Before(e.expires) {
			delete(s.entries, k)
			n++
		}
	}
	return n
}

// Len returns the number of stored claims, expired or not.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}
