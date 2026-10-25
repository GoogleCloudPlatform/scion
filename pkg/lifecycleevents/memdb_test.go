// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package lifecycleevents

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// memDB is an in-memory stand-in for the cursor and outbox tables with
// transactional semantics: a memTx works on a copy and applies it on
// commit, and holds the database lock from begin to commit (like the row
// locks the real writers take), so concurrent writers serialize.
type memDB struct {
	mu      sync.Mutex // held by an open memTx
	stateMu sync.Mutex // guards the fields below for outbox-store calls
	cursors map[string]store.LifecycleCursor
	outbox  map[int64]store.LifecycleOutboxRow
	nextSeq int64
	// agents is the set of live agent rows, for the orphan sweep.
	agents map[string]bool

	// Fault injection.
	failInsert   error
	failComplete error
}

func newMemDB() *memDB {
	return &memDB{cursors: map[string]store.LifecycleCursor{}, outbox: map[int64]store.LifecycleOutboxRow{}, agents: map[string]bool{}}
}

type memTx struct {
	db       *memDB
	cursors  map[string]store.LifecycleCursor
	inserted []store.LifecycleOutboxRow
	hooks    []func()
	done     bool
}

func (db *memDB) begin() *memTx {
	db.mu.Lock()
	db.stateMu.Lock()
	cp := make(map[string]store.LifecycleCursor, len(db.cursors))
	for k, v := range db.cursors {
		cp[k] = v
	}
	db.stateMu.Unlock()
	return &memTx{db: db, cursors: cp}
}

func (t *memTx) commit() {
	if t.done {
		return
	}
	t.done = true
	t.db.stateMu.Lock()
	t.db.cursors = t.cursors
	for _, r := range t.inserted {
		t.db.nextSeq++
		r.Seq = t.db.nextSeq
		t.db.outbox[r.Seq] = r
	}
	t.db.stateMu.Unlock()
	t.db.mu.Unlock()
	for _, h := range t.hooks {
		h()
	}
}

func (t *memTx) rollback() {
	if t.done {
		return
	}
	t.done = true
	t.db.mu.Unlock()
}

// observe runs one observed write in its own transaction, committing when
// the observer returns nil (as the real writers do).
func (db *memDB) observe(ctx context.Context, o store.AgentWriteObserver, w store.AgentWrite) error {
	tx := db.begin()
	if err := o.ObserveAgentWrite(ctx, tx, w); err != nil {
		tx.rollback()
		return err
	}
	tx.commit()
	return nil
}

func (t *memTx) GetLifecycleCursor(ctx context.Context, agentID string) (*store.LifecycleCursor, error) {
	c, ok := t.cursors[agentID]
	if !ok {
		return nil, nil
	}
	return &c, nil
}

func (t *memTx) BumpLifecycleCursor(ctx context.Context, agentID string, n int, upd store.LifecycleCursorUpdate) (*store.LifecycleCursor, error) {
	c, ok := t.cursors[agentID]
	switch {
	case upd.Create && ok:
		return nil, fmt.Errorf("cursor %s exists", agentID)
	case !upd.Create && !ok:
		return nil, fmt.Errorf("cursor %s missing", agentID)
	case upd.Create:
		c = store.LifecycleCursor{AgentID: agentID, CreatorPrincipalType: upd.CreatorPrincipalType, CreatorPrincipalID: upd.CreatorPrincipalID, RequestedTemplate: upd.RequestedTemplate}
	}
	c.TransitionRevision += int64(n)
	c.ProjectID = upd.ProjectID
	c.MetadataRevision = upd.MetadataRevision
	c.MetadataDigest = upd.MetadataDigest
	c.LastFingerprint = upd.LastFingerprint
	c.LastDeleted = upd.LastDeleted
	// As the store: one UTC value for updated_at and attribution_stamp.
	now := upd.Now
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	c.UpdatedAt = now
	stamp := now
	c.AttributionStamp = &stamp
	c.Attribution = upd.Attribution
	r := upd.Run
	c.OpenRun, c.OpenRunGeneration, c.OpenRunMetadataRevision = r.OpenRun, r.OpenRunGeneration, r.OpenRunMetadataRevision
	c.PendingRunChangeAt, c.ReincarnationBaseRunID = r.PendingRunChangeAt, r.ReincarnationBaseRunID
	c.PendingAdoptedFrom, c.PendingAdoptedAt, c.PendingAdoptionSource = r.PendingAdoptedFrom, r.PendingAdoptedAt, r.PendingAdoptionSource
	c.PendingAdoptedTo = r.PendingAdoptedTo
	t.cursors[agentID] = c
	return &c, nil
}

func (t *memTx) DeleteLifecycleCursor(ctx context.Context, agentID string) error {
	delete(t.cursors, agentID)
	return nil
}

func (t *memTx) InsertLifecycleOutbox(ctx context.Context, rows []store.LifecycleOutboxRow) error {
	if t.db.failInsert != nil {
		return t.db.failInsert
	}
	for _, r := range rows {
		r.Payload = append([]byte(nil), r.Payload...)
		t.inserted = append(t.inserted, r)
	}
	return nil
}

func (t *memTx) AfterCommit(f func()) { t.hooks = append(t.hooks, f) }

// --- LifecycleOutboxStore -------------------------------------------------

func (db *memDB) sortedSeqs() []int64 {
	seqs := make([]int64, 0, len(db.outbox))
	for s := range db.outbox {
		seqs = append(seqs, s)
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	return seqs
}

func unleased(r store.LifecycleOutboxRow, now time.Time) bool {
	return r.LeaseUntil == nil || r.LeaseUntil.Before(now)
}

func (db *memDB) ClaimLifecycleOutbox(ctx context.Context, owner string, now, leaseUntil time.Time, limit int) ([]store.LifecycleOutboxRow, error) {
	db.stateMu.Lock()
	defer db.stateMu.Unlock()
	var out []store.LifecycleOutboxRow
	for _, s := range db.sortedSeqs() {
		r := db.outbox[s]
		if len(out) == limit {
			break
		}
		if !unleased(r, now) || r.NextAttemptAt.After(now) {
			continue
		}
		lu := leaseUntil
		r.LeaseOwner, r.LeaseUntil = owner, &lu
		db.outbox[s] = r
		r.Payload = append([]byte(nil), r.Payload...)
		out = append(out, r)
	}
	return out, nil
}

func (db *memDB) RenewLifecycleOutboxLease(ctx context.Context, seq int64, owner string, leaseUntil time.Time) (bool, error) {
	db.stateMu.Lock()
	defer db.stateMu.Unlock()
	r, ok := db.outbox[seq]
	if !ok || r.LeaseOwner != owner {
		return false, nil
	}
	lu := leaseUntil
	r.LeaseUntil = &lu
	db.outbox[seq] = r
	return true, nil
}

func (db *memDB) CompleteLifecycleOutboxRow(ctx context.Context, seq int64, owner string) (bool, error) {
	db.stateMu.Lock()
	defer db.stateMu.Unlock()
	if db.failComplete != nil {
		return false, db.failComplete
	}
	r, ok := db.outbox[seq]
	if !ok || r.LeaseOwner != owner {
		return false, nil
	}
	delete(db.outbox, seq)
	return true, nil
}

func (db *memDB) FailLifecycleOutboxRow(ctx context.Context, seq int64, owner string, f store.LifecycleOutboxFailure) error {
	db.stateMu.Lock()
	defer db.stateMu.Unlock()
	r, ok := db.outbox[seq]
	if !ok || r.LeaseOwner != owner {
		return nil
	}
	if f.CountAttempt {
		r.Attempts++
	}
	if f.Permanent {
		r.PermanentFailures++
	}
	r.NextAttemptAt = f.NextAttemptAt
	r.LastError = f.LastError
	r.LeaseOwner, r.LeaseUntil = "", nil
	db.outbox[seq] = r
	return nil
}

func dropped(r store.LifecycleOutboxRow) store.LifecycleOutboxDropped {
	return store.LifecycleOutboxDropped{Seq: r.Seq, EventID: r.EventID, AgentID: r.AgentID, TransitionRevision: r.TransitionRevision}
}

func (db *memDB) TrimLifecycleOutbox(ctx context.Context, now time.Time, maxAge time.Duration, maxRows, maxPermanent int) (store.LifecycleOutboxTrim, error) {
	db.stateMu.Lock()
	defer db.stateMu.Unlock()
	var t store.LifecycleOutboxTrim
	cutoff := now.Add(-maxAge)
	for _, s := range db.sortedSeqs() {
		r := db.outbox[s]
		if !unleased(r, now) {
			continue
		}
		switch {
		case r.CreatedAt.Before(cutoff):
			t.Age = append(t.Age, dropped(r))
			delete(db.outbox, s)
		case r.PermanentFailures >= maxPermanent:
			t.Poison = append(t.Poison, dropped(r))
			delete(db.outbox, s)
		}
	}
	if excess := len(db.outbox) - maxRows; excess > 0 {
		for _, s := range db.sortedSeqs() {
			if excess == 0 {
				break
			}
			r := db.outbox[s]
			if !unleased(r, now) {
				continue
			}
			t.Overflow = append(t.Overflow, dropped(r))
			delete(db.outbox, s)
			excess--
		}
	}
	return t, nil
}

func (db *memDB) LifecycleOutboxHasRows(ctx context.Context) (bool, error) {
	db.stateMu.Lock()
	defer db.stateMu.Unlock()
	return len(db.outbox) > 0, nil
}

func (db *memDB) LifecycleOutboxStats(ctx context.Context) (store.LifecycleOutboxStats, error) {
	db.stateMu.Lock()
	defer db.stateMu.Unlock()
	var st store.LifecycleOutboxStats
	for s, r := range db.outbox {
		st.Pending++
		if s > st.MaxSeq {
			st.MaxSeq = s
		}
		if st.Oldest == nil || r.CreatedAt.Before(*st.Oldest) {
			c := r.CreatedAt
			st.Oldest = &c
		}
	}
	return st, nil
}

func (db *memDB) SweepOrphanLifecycleCursors(ctx context.Context, limit int) (int, error) {
	db.stateMu.Lock()
	defer db.stateMu.Unlock()
	n := 0
	for id := range db.cursors {
		if n == limit {
			break
		}
		if !db.agents[id] {
			delete(db.cursors, id)
			n++
		}
	}
	return n, nil
}

// rows returns the outbox in seq order.
func (db *memDB) rows() []store.LifecycleOutboxRow {
	db.stateMu.Lock()
	defer db.stateMu.Unlock()
	out := make([]store.LifecycleOutboxRow, 0, len(db.outbox))
	for _, s := range db.sortedSeqs() {
		out = append(out, db.outbox[s])
	}
	return out
}

func (db *memDB) cursor(id string) (store.LifecycleCursor, bool) {
	db.stateMu.Lock()
	defer db.stateMu.Unlock()
	c, ok := db.cursors[id]
	return c, ok
}

var errInjected = errors.New("injected failure")
