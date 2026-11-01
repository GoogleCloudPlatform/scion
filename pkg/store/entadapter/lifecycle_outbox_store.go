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

package entadapter

// This file holds the store side of agent lifecycle events (WP1 design
// 5.1, 5.3): the observer slot shared by a store and its transactional
// copies, the LifecycleTx implementation every observed writer passes to
// the observer, and the drainer's outbox and cursor-sweep methods.
//
// Every statement here touches only agent_lifecycle_cursors and
// agent_lifecycle_outbox, binds Go time.Time values (never formatted
// strings or SQL date arithmetic), and runs only while an observer is
// installed or a drainer runs: with no observer the agent writers behave
// exactly as before.

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/agentlifecyclecursor"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/agentlifecycleoutbox"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/predicate"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// lifecycleObserverSlot holds the installed observer. One slot is shared
// by a CompositeStore and every transactional copy made from it, so an
// install or removal is seen by all of them; each writer loads it once at
// the start of its write and uses that value through commit.
type lifecycleObserverSlot struct {
	p atomic.Pointer[observerBox]
}

type observerBox struct{ o store.AgentWriteObserver }

func newLifecycleObserverSlot() *lifecycleObserverSlot { return &lifecycleObserverSlot{} }

// load returns the installed observer, or nil. A nil slot (a store built
// without one) has none.
func (s *lifecycleObserverSlot) load() store.AgentWriteObserver {
	if s == nil {
		return nil
	}
	if b := s.p.Load(); b != nil {
		return b.o
	}
	return nil
}

// SetAgentWriteObserver installs o as the observer of every observed agent
// write on this store and its transactions; nil removes it. With no
// observer the writers do nothing extra. It is not part of store.Store:
// the hub type-asserts for it, so store fakes are unaffected.
func (c *CompositeStore) SetAgentWriteObserver(o store.AgentWriteObserver) {
	if o == nil {
		c.lifecycle.p.Store(nil)
		return
	}
	c.lifecycle.p.Store(&observerBox{o: o})
}

// AgentWriteObserverInstalled reports whether an observer is installed.
func (c *CompositeStore) AgentWriteObserverInstalled() bool {
	return c.lifecycle.load() != nil
}

// newTxStore returns a CompositeStore running in tx that shares this
// store's observer slot and knows tx for post-commit hooks. Agent writers
// that may be observed use it instead of newTxCompositeStore.
func (c *CompositeStore) newTxStore(tx *ent.Tx) *CompositeStore {
	txStore := newTxCompositeStore(tx)
	txStore.lifecycle = c.lifecycle
	txStore.entTx = tx
	return txStore
}

// observeAgentWrite runs o (when non-nil) for w in the transaction of
// client. tx, when known, receives the observer's post-commit hooks.
func observeAgentWrite(ctx context.Context, o store.AgentWriteObserver, client *ent.Client, tx *ent.Tx, w store.AgentWrite) error {
	if o == nil {
		return nil
	}
	return o.ObserveAgentWrite(ctx, &lifecycleTx{client: client, tx: tx}, w)
}

// commitHooks collects the observer's post-commit hooks for a writer that
// commits a raw *sql.Tx itself (the launch store and reaper): the writer
// runs them after its Commit succeeds, and drops them on rollback.
type commitHooks struct{ fns []func() }

// run runs the collected hooks once.
func (h *commitHooks) run() {
	if h == nil {
		return
	}
	fns := h.fns
	h.fns = nil
	for _, f := range fns {
		f()
	}
}

// observeAgentWriteDeferred is observeAgentWrite for a raw transaction:
// post-commit hooks go to hooks, which the writer runs after its commit.
func observeAgentWriteDeferred(ctx context.Context, o store.AgentWriteObserver, client *ent.Client, hooks *commitHooks, w store.AgentWrite) error {
	if o == nil {
		return nil
	}
	return o.ObserveAgentWrite(ctx, &lifecycleTx{client: client, hooks: hooks}, w)
}

// lifecycleTx implements store.LifecycleTx over the writer's transaction.
type lifecycleTx struct {
	client *ent.Client
	tx     *ent.Tx
	hooks  *commitHooks
}

var _ store.LifecycleTx = (*lifecycleTx)(nil)

func (l *lifecycleTx) pg() bool { return l.client.Driver().Dialect() == dialect.Postgres }

func entCursorToStore(c *ent.AgentLifecycleCursor) *store.LifecycleCursor {
	return &store.LifecycleCursor{
		AgentID:                 c.ID.String(),
		ProjectID:               c.ProjectID.String(),
		TransitionRevision:      c.TransitionRevision,
		MetadataRevision:        c.MetadataRevision,
		MetadataDigest:          c.MetadataDigest,
		LastFingerprint:         c.LastFingerprint,
		LastDeleted:             c.LastDeleted,
		OpenRun:                 c.OpenRun,
		OpenRunGeneration:       c.OpenRunGeneration,
		OpenRunMetadataRevision: c.OpenRunMetadataRevision,
		PendingRunChangeAt:      c.PendingRunChangeAt,
		ReincarnationBaseRunID:  c.ReincarnationBaseRunID,
		PendingAdoptedFrom:      c.PendingAdoptedFrom,
		PendingAdoptedAt:        c.PendingAdoptedAt,
		PendingAdoptionSource:   c.PendingAdoptionSource,
		PendingAdoptedTo:        c.PendingAdoptedTo,
		CreatorPrincipalType:    c.CreatorPrincipalType,
		CreatorPrincipalID:      c.CreatorPrincipalID,
		RequestedTemplate:       c.RequestedTemplate,
		Attribution: store.LifecycleAttribution{
			State:                      c.AttributionState,
			LastRun:                    c.LastRun,
			LastGeneration:             c.LastGeneration,
			AttributedRun:              c.AttributedRun,
			AttributedGeneration:       c.AttributedGeneration,
			QuarantinedRuns:            c.QuarantinedRuns,
			QuarantineMode:             c.QuarantineMode,
			ReincarnationMintRun:       c.ReincarnationMintRun,
			UntrustedOpenRun:           c.UntrustedOpenRun,
			UntrustedOpenRunGeneration: c.UntrustedOpenRunGeneration,
			UntrustedOpenRunRev:        c.UntrustedOpenRunRev,
		},
		AttributionStamp: c.AttributionStamp,
		UpdatedAt:        c.UpdatedAt,
	}
}

// GetLifecycleCursor implements store.LifecycleTx.
func (l *lifecycleTx) GetLifecycleCursor(ctx context.Context, agentID string) (*store.LifecycleCursor, error) {
	uid, err := parseUUID(agentID)
	if err != nil {
		return nil, err
	}
	q := l.client.AgentLifecycleCursor.Query().Where(agentlifecyclecursor.IDEQ(uid))
	if l.pg() {
		q = q.ForUpdate()
	}
	c, err := q.Only(ctx)
	if ent.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, mapError(err)
	}
	return entCursorToStore(c), nil
}

// BumpLifecycleCursor implements store.LifecycleTx. The update adds n to
// transition_revision in the statement itself (SET transition_revision =
// transition_revision + n), which also holds the row lock until commit.
func (l *lifecycleTx) BumpLifecycleCursor(ctx context.Context, agentID string, n int, upd store.LifecycleCursorUpdate) (*store.LifecycleCursor, error) {
	if n < 0 {
		return nil, fmt.Errorf("%w: negative revision count %d", store.ErrInvalidInput, n)
	}
	uid, err := parseUUID(agentID)
	if err != nil {
		return nil, err
	}
	projectUID, err := parseUUID(upd.ProjectID)
	if err != nil {
		return nil, err
	}
	// One normalised value for updated_at and attribution_stamp, written in
	// the same statement (attribution design 7.1): the stamp check compares
	// the two with time.Equal.
	now := upd.Now
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	att := upd.Attribution
	if upd.Create {
		c, err := l.client.AgentLifecycleCursor.Create().
			SetID(uid).
			SetProjectID(projectUID).
			SetTransitionRevision(int64(n)).
			SetMetadataRevision(upd.MetadataRevision).
			SetMetadataDigest(upd.MetadataDigest).
			SetLastFingerprint(upd.LastFingerprint).
			SetLastDeleted(upd.LastDeleted).
			SetCreatorPrincipalType(upd.CreatorPrincipalType).
			SetCreatorPrincipalID(upd.CreatorPrincipalID).
			SetRequestedTemplate(upd.RequestedTemplate).
			SetUpdatedAt(now).
			SetOpenRun(upd.Run.OpenRun).
			SetOpenRunGeneration(upd.Run.OpenRunGeneration).
			SetOpenRunMetadataRevision(upd.Run.OpenRunMetadataRevision).
			SetNillablePendingRunChangeAt(utcPtr(upd.Run.PendingRunChangeAt)).
			SetReincarnationBaseRunID(upd.Run.ReincarnationBaseRunID).
			SetPendingAdoptedFrom(upd.Run.PendingAdoptedFrom).
			SetNillablePendingAdoptedAt(utcPtr(upd.Run.PendingAdoptedAt)).
			SetPendingAdoptionSource(upd.Run.PendingAdoptionSource).
			SetPendingAdoptedTo(upd.Run.PendingAdoptedTo).
			SetAttributionState(att.State).
			SetAttributionStamp(now).
			SetLastRun(att.LastRun).
			SetLastGeneration(att.LastGeneration).
			SetAttributedRun(att.AttributedRun).
			SetAttributedGeneration(att.AttributedGeneration).
			SetQuarantinedRuns(att.QuarantinedRuns).
			SetQuarantineMode(att.QuarantineMode).
			SetReincarnationMintRun(att.ReincarnationMintRun).
			SetUntrustedOpenRun(att.UntrustedOpenRun).
			SetUntrustedOpenRunGeneration(att.UntrustedOpenRunGeneration).
			SetUntrustedOpenRunRev(att.UntrustedOpenRunRev).
			Save(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		return entCursorToStore(c), nil
	}
	u := l.client.AgentLifecycleCursor.UpdateOneID(uid).
		AddTransitionRevision(int64(n)).
		SetProjectID(projectUID).
		SetMetadataRevision(upd.MetadataRevision).
		SetMetadataDigest(upd.MetadataDigest).
		SetLastFingerprint(upd.LastFingerprint).
		SetLastDeleted(upd.LastDeleted).
		SetUpdatedAt(now).
		SetOpenRun(upd.Run.OpenRun).
		SetOpenRunGeneration(upd.Run.OpenRunGeneration).
		SetOpenRunMetadataRevision(upd.Run.OpenRunMetadataRevision).
		SetReincarnationBaseRunID(upd.Run.ReincarnationBaseRunID).
		SetPendingAdoptedFrom(upd.Run.PendingAdoptedFrom).
		SetPendingAdoptionSource(upd.Run.PendingAdoptionSource).
		SetPendingAdoptedTo(upd.Run.PendingAdoptedTo).
		SetAttributionState(att.State).
		SetAttributionStamp(now).
		SetLastRun(att.LastRun).
		SetLastGeneration(att.LastGeneration).
		SetAttributedRun(att.AttributedRun).
		SetAttributedGeneration(att.AttributedGeneration).
		SetQuarantinedRuns(att.QuarantinedRuns).
		SetQuarantineMode(att.QuarantineMode).
		SetReincarnationMintRun(att.ReincarnationMintRun).
		SetUntrustedOpenRun(att.UntrustedOpenRun).
		SetUntrustedOpenRunGeneration(att.UntrustedOpenRunGeneration).
		SetUntrustedOpenRunRev(att.UntrustedOpenRunRev)
	if t := upd.Run.PendingRunChangeAt; t != nil {
		u.SetPendingRunChangeAt(t.UTC())
	} else {
		u.ClearPendingRunChangeAt()
	}
	if t := upd.Run.PendingAdoptedAt; t != nil {
		u.SetPendingAdoptedAt(t.UTC())
	} else {
		u.ClearPendingAdoptedAt()
	}
	c, err := u.Save(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return entCursorToStore(c), nil
}

// utcPtr returns t in UTC, or nil.
func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// DeleteLifecycleCursor implements store.LifecycleTx.
func (l *lifecycleTx) DeleteLifecycleCursor(ctx context.Context, agentID string) error {
	uid, err := parseUUID(agentID)
	if err != nil {
		return err
	}
	_, err = l.client.AgentLifecycleCursor.Delete().Where(agentlifecyclecursor.IDEQ(uid)).Exec(ctx)
	return mapError(err)
}

// InsertLifecycleOutbox implements store.LifecycleTx.
func (l *lifecycleTx) InsertLifecycleOutbox(ctx context.Context, rows []store.LifecycleOutboxRow) error {
	if len(rows) == 0 {
		return nil
	}
	builders := make([]*ent.AgentLifecycleOutboxCreate, 0, len(rows))
	for _, r := range rows {
		uid, err := parseUUID(r.AgentID)
		if err != nil {
			return err
		}
		builders = append(builders, l.client.AgentLifecycleOutbox.Create().
			SetEventID(r.EventID).
			SetAgentID(uid).
			SetTransitionRevision(r.TransitionRevision).
			SetPayload(string(r.Payload)).
			SetCreatedAt(r.CreatedAt).
			SetNextAttemptAt(r.NextAttemptAt))
	}
	_, err := l.client.AgentLifecycleOutbox.CreateBulk(builders...).Save(ctx)
	return mapError(err)
}

// AfterCommit implements store.LifecycleTx: f runs after the writer's
// transaction commits, and never if it rolls back. Every observed writer
// passes its transaction; with none (a store copy built without one) f
// runs at once.
func (l *lifecycleTx) AfterCommit(f func()) {
	if l.hooks != nil {
		l.hooks.fns = append(l.hooks.fns, f)
		return
	}
	if l.tx == nil {
		f()
		return
	}
	l.tx.OnCommit(func(next ent.Committer) ent.Committer {
		return ent.CommitFunc(func(ctx context.Context, tx *ent.Tx) error {
			if err := next.Commit(ctx, tx); err != nil {
				return err
			}
			f()
			return nil
		})
	})
}

// --- Drainer access (store.LifecycleOutboxStore) ---------------------------

var _ store.LifecycleOutboxStore = (*CompositeStore)(nil)

// lifecycleOutboxBatch bounds the IN lists of the trim deletes.
const lifecycleOutboxBatch = 500

// outboxUnleased matches rows with no live lease at now.
func outboxUnleased(now time.Time) predicate.AgentLifecycleOutbox {
	return agentlifecycleoutbox.Or(agentlifecycleoutbox.LeaseUntilIsNil(), agentlifecycleoutbox.LeaseUntilLT(now))
}

func entOutboxToStore(r *ent.AgentLifecycleOutbox) store.LifecycleOutboxRow {
	return store.LifecycleOutboxRow{
		Seq:                r.ID,
		EventID:            r.EventID,
		AgentID:            r.AgentID.String(),
		TransitionRevision: r.TransitionRevision,
		Payload:            []byte(r.Payload),
		CreatedAt:          r.CreatedAt,
		Attempts:           r.Attempts,
		PermanentFailures:  r.PermanentFailures,
		NextAttemptAt:      r.NextAttemptAt,
		LeaseOwner:         r.LeaseOwner,
		LeaseUntil:         r.LeaseUntil,
		LastError:          r.LastError,
	}
}

// ClaimLifecycleOutbox implements store.LifecycleOutboxStore: in one
// transaction it selects up to limit due, unleased rows in seq order
// (FOR UPDATE SKIP LOCKED on PostgreSQL; SQLite has one writer) and leases
// them to owner.
func (c *CompositeStore) ClaimLifecycleOutbox(ctx context.Context, owner string, now, leaseUntil time.Time, limit int) ([]store.LifecycleOutboxRow, error) {
	if limit <= 0 {
		return nil, nil
	}
	var out []store.LifecycleOutboxRow
	err := runInEntTx(ctx, c.client, c.inTx, func(client *ent.Client) error {
		q := client.AgentLifecycleOutbox.Query().
			Where(outboxUnleased(now), agentlifecycleoutbox.NextAttemptAtLTE(now)).
			Order(ent.Asc(agentlifecycleoutbox.FieldID)).
			Limit(limit)
		if client.Driver().Dialect() == dialect.Postgres {
			q = q.ForUpdate(entsql.WithLockAction(entsql.SkipLocked))
		}
		ids, err := q.IDs(ctx)
		if err != nil || len(ids) == 0 {
			return err
		}
		if _, err := client.AgentLifecycleOutbox.Update().
			Where(agentlifecycleoutbox.IDIn(ids...), outboxUnleased(now)).
			SetLeaseOwner(owner).
			SetLeaseUntil(leaseUntil).
			Save(ctx); err != nil {
			return err
		}
		rows, err := client.AgentLifecycleOutbox.Query().
			Where(agentlifecycleoutbox.IDIn(ids...), agentlifecycleoutbox.LeaseOwnerEQ(owner)).
			Order(ent.Asc(agentlifecycleoutbox.FieldID)).
			All(ctx)
		if err != nil {
			return err
		}
		out = make([]store.LifecycleOutboxRow, 0, len(rows))
		for _, r := range rows {
			out = append(out, entOutboxToStore(r))
		}
		return nil
	})
	if err != nil {
		return nil, mapError(err)
	}
	return out, nil
}

// RenewLifecycleOutboxLease implements store.LifecycleOutboxStore.
func (c *CompositeStore) RenewLifecycleOutboxLease(ctx context.Context, seq int64, owner string, leaseUntil time.Time) (bool, error) {
	n, err := c.client.AgentLifecycleOutbox.Update().
		Where(agentlifecycleoutbox.IDEQ(seq), agentlifecycleoutbox.LeaseOwnerEQ(owner)).
		SetLeaseUntil(leaseUntil).
		Save(ctx)
	if err != nil {
		return false, mapError(err)
	}
	return n > 0, nil
}

// CompleteLifecycleOutboxRow implements store.LifecycleOutboxStore.
func (c *CompositeStore) CompleteLifecycleOutboxRow(ctx context.Context, seq int64, owner string) (bool, error) {
	n, err := c.client.AgentLifecycleOutbox.Delete().
		Where(agentlifecycleoutbox.IDEQ(seq), agentlifecycleoutbox.LeaseOwnerEQ(owner)).
		Exec(ctx)
	if err != nil {
		return false, mapError(err)
	}
	return n > 0, nil
}

// FailLifecycleOutboxRow implements store.LifecycleOutboxStore.
func (c *CompositeStore) FailLifecycleOutboxRow(ctx context.Context, seq int64, owner string, f store.LifecycleOutboxFailure) error {
	upd := c.client.AgentLifecycleOutbox.Update().
		Where(agentlifecycleoutbox.IDEQ(seq), agentlifecycleoutbox.LeaseOwnerEQ(owner)).
		SetNextAttemptAt(f.NextAttemptAt).
		SetLastError(truncateUTF8Bytes(f.LastError, 256)).
		SetLeaseOwner("").
		ClearLeaseUntil()
	if f.CountAttempt {
		upd = upd.AddAttempts(1)
	}
	if f.Permanent {
		upd = upd.AddPermanentFailures(1)
	}
	_, err := upd.Save(ctx)
	return mapError(err)
}

// truncateUTF8Bytes cuts s to at most n bytes on a rune boundary.
func truncateUTF8Bytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

func toDropped(rows []*ent.AgentLifecycleOutbox) []store.LifecycleOutboxDropped {
	out := make([]store.LifecycleOutboxDropped, 0, len(rows))
	for _, r := range rows {
		out = append(out, store.LifecycleOutboxDropped{Seq: r.ID, EventID: r.EventID, AgentID: r.AgentID.String(), TransitionRevision: r.TransitionRevision})
	}
	return out
}

// deleteOutboxRows deletes the given unleased rows in batches and returns
// those actually deleted.
func deleteOutboxRows(ctx context.Context, client *ent.Client, now time.Time, rows []*ent.AgentLifecycleOutbox) ([]*ent.AgentLifecycleOutbox, error) {
	var gone []*ent.AgentLifecycleOutbox
	for start := 0; start < len(rows); start += lifecycleOutboxBatch {
		chunk := rows[start:min(len(rows), start+lifecycleOutboxBatch)]
		ids := make([]int64, len(chunk))
		for i, r := range chunk {
			ids[i] = r.ID
		}
		if _, err := client.AgentLifecycleOutbox.Delete().
			Where(agentlifecycleoutbox.IDIn(ids...), outboxUnleased(now)).
			Exec(ctx); err != nil {
			return nil, err
		}
		left, err := client.AgentLifecycleOutbox.Query().Where(agentlifecycleoutbox.IDIn(ids...)).IDs(ctx)
		if err != nil {
			return nil, err
		}
		kept := make(map[int64]bool, len(left))
		for _, id := range left {
			kept[id] = true
		}
		for _, r := range chunk {
			if !kept[r.ID] {
				gone = append(gone, r)
			}
		}
	}
	return gone, nil
}

// outboxIdentityFields are the columns the drop log needs.
var outboxIdentityFields = []string{
	agentlifecycleoutbox.FieldID, agentlifecycleoutbox.FieldEventID,
	agentlifecycleoutbox.FieldAgentID, agentlifecycleoutbox.FieldTransitionRevision,
}

// lockTrimCandidates makes a trim selection lock its rows on PostgreSQL
// (FOR UPDATE SKIP LOCKED), so the rows a trim reports are exactly the
// rows it deletes: another replica can neither claim and deliver a
// selected row before the delete (it would be counted as a drop although
// delivered) nor select and report the same rows (double counting); it
// skips them. SQLite has one writer, so the transaction already gives
// that.
func lockTrimCandidates(client *ent.Client, q *ent.AgentLifecycleOutboxQuery) *ent.AgentLifecycleOutboxQuery {
	if client.Driver().Dialect() == dialect.Postgres {
		q = q.ForUpdate(entsql.WithLockAction(entsql.SkipLocked))
	}
	return q
}

// TrimLifecycleOutbox implements store.LifecycleOutboxStore. Rows under a
// live lease are never trimmed, so a row another replica is emitting is
// never counted as dropped; the selected rows are locked until the delete
// commits (lockTrimCandidates), so each drop is counted once, by the
// replica that removed it.
func (c *CompositeStore) TrimLifecycleOutbox(ctx context.Context, now time.Time, maxAge time.Duration, maxRows, maxPermanentFailures int) (store.LifecycleOutboxTrim, error) {
	var t store.LifecycleOutboxTrim
	err := runInEntTx(ctx, c.client, c.inTx, func(client *ent.Client) error {
		aged, err := lockTrimCandidates(client, client.AgentLifecycleOutbox.Query().
			Where(outboxUnleased(now), agentlifecycleoutbox.CreatedAtLT(now.Add(-maxAge))).
			Order(ent.Asc(agentlifecycleoutbox.FieldID))).
			Select(outboxIdentityFields...).All(ctx)
		if err != nil {
			return err
		}
		if aged, err = deleteOutboxRows(ctx, client, now, aged); err != nil {
			return err
		}
		t.Age = toDropped(aged)

		poison, err := lockTrimCandidates(client, client.AgentLifecycleOutbox.Query().
			Where(outboxUnleased(now), agentlifecycleoutbox.PermanentFailuresGTE(maxPermanentFailures)).
			Order(ent.Asc(agentlifecycleoutbox.FieldID))).
			Select(outboxIdentityFields...).All(ctx)
		if err != nil {
			return err
		}
		if poison, err = deleteOutboxRows(ctx, client, now, poison); err != nil {
			return err
		}
		t.Poison = toDropped(poison)

		total, err := client.AgentLifecycleOutbox.Query().Count(ctx)
		if err != nil {
			return err
		}
		if excess := total - maxRows; excess > 0 {
			oldest, err := lockTrimCandidates(client, client.AgentLifecycleOutbox.Query().
				Where(outboxUnleased(now)).
				Order(ent.Asc(agentlifecycleoutbox.FieldID)).
				Limit(excess)).
				Select(outboxIdentityFields...).All(ctx)
			if err != nil {
				return err
			}
			if oldest, err = deleteOutboxRows(ctx, client, now, oldest); err != nil {
				return err
			}
			t.Overflow = toDropped(oldest)
		}
		return nil
	})
	if err != nil {
		return store.LifecycleOutboxTrim{}, mapError(err)
	}
	return t, nil
}

// LifecycleOutboxHasRows implements store.LifecycleOutboxStore with an
// existence probe (SELECT ... LIMIT 1), so its cost does not grow with
// the backlog.
func (c *CompositeStore) LifecycleOutboxHasRows(ctx context.Context) (bool, error) {
	ok, err := c.client.AgentLifecycleOutbox.Query().Exist(ctx)
	if err != nil {
		return false, mapError(err)
	}
	return ok, nil
}

// LifecycleOutboxStats implements store.LifecycleOutboxStore.
func (c *CompositeStore) LifecycleOutboxStats(ctx context.Context) (store.LifecycleOutboxStats, error) {
	var st store.LifecycleOutboxStats
	n, err := c.client.AgentLifecycleOutbox.Query().Count(ctx)
	if err != nil {
		return st, mapError(err)
	}
	st.Pending = int64(n)
	if n == 0 {
		return st, nil
	}
	first, err := c.client.AgentLifecycleOutbox.Query().
		Order(ent.Asc(agentlifecycleoutbox.FieldCreatedAt)).
		Select(agentlifecycleoutbox.FieldCreatedAt).
		First(ctx)
	if err != nil && !ent.IsNotFound(err) {
		return st, mapError(err)
	}
	if first != nil {
		oldest := first.CreatedAt
		st.Oldest = &oldest
	}
	last, err := c.client.AgentLifecycleOutbox.Query().
		Order(ent.Desc(agentlifecycleoutbox.FieldID)).
		Select(agentlifecycleoutbox.FieldID).
		First(ctx)
	if err != nil && !ent.IsNotFound(err) {
		return st, mapError(err)
	}
	if last != nil {
		st.MaxSeq = last.ID
	}
	return st, nil
}

// cursorWithoutAgent matches cursors whose agent row no longer exists.
// agent_lifecycle_cursors.agent_id and agents.id are both uuid columns on
// PostgreSQL (and text on SQLite), so the comparison needs no cast (review
// r6 N1).
func cursorWithoutAgent() predicate.AgentLifecycleCursor {
	return func(s *entsql.Selector) {
		t := entsql.Table(agent.Table)
		s.Where(entsql.NotExists(
			entsql.Select(t.C(agent.FieldID)).From(t).
				Where(entsql.ColumnsEQ(t.C(agent.FieldID), s.C(agentlifecyclecursor.FieldID))),
		))
	}
}

// SweepOrphanLifecycleCursors implements store.LifecycleOutboxStore. It is
// idempotent and needs no lease: a concurrent create's cursor and agent row
// become visible together at commit, and the delete re-checks the
// predicate.
func (c *CompositeStore) SweepOrphanLifecycleCursors(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		return 0, nil
	}
	ids, err := c.client.AgentLifecycleCursor.Query().
		Where(cursorWithoutAgent()).
		Order(ent.Asc(agentlifecyclecursor.FieldID)).
		Limit(limit).
		IDs(ctx)
	if err != nil || len(ids) == 0 {
		return 0, mapError(err)
	}
	n, err := c.client.AgentLifecycleCursor.Delete().
		Where(agentlifecyclecursor.IDIn(ids...), cursorWithoutAgent()).
		Exec(ctx)
	if err != nil {
		return 0, mapError(err)
	}
	return n, nil
}

// deleteProjectLifecycleCursors removes every cursor of a project's agents
// (live, soft-deleted or already gone), in agent-ID order.
func deleteProjectLifecycleCursors(ctx context.Context, client *ent.Client, projectUID uuid.UUID) error {
	ids, err := client.AgentLifecycleCursor.Query().
		Where(agentlifecyclecursor.ProjectIDEQ(projectUID)).
		Order(ent.Asc(agentlifecyclecursor.FieldID)).
		IDs(ctx)
	if err != nil || len(ids) == 0 {
		return err
	}
	_, err = client.AgentLifecycleCursor.Delete().Where(agentlifecyclecursor.IDIn(ids...)).Exec(ctx)
	return err
}

// lifecycleWriteTx returns the client an observed writer writes with: its
// own when it already runs in a transaction or no observer is installed,
// otherwise a new transaction, so the row and its lifecycle rows commit
// together. done rolls back an uncommitted own transaction;
// commitLifecycleWriteTx commits it.
func (s *AgentStore) lifecycleWriteTx(ctx context.Context, obs store.AgentWriteObserver) (client *ent.Client, tx *ent.Tx, done func(), err error) {
	if obs == nil || s.inTx {
		return s.client, s.entTx, func() {}, nil
	}
	tx, err = s.client.Tx(ctx)
	if errors.Is(err, ent.ErrTxStarted) {
		// s.client already wraps a transaction it was built on: join it.
		return s.client, s.entTx, func() {}, nil
	}
	if err != nil {
		return nil, nil, nil, markTransient(fmt.Errorf("begin transaction: %w", err))
	}
	return tx.Client(), tx, func() { _ = tx.Rollback() }, nil
}

// markOwnTxErr marks *errp transient (markTransient) when the writer opened
// its own lifecycle transaction (tx is not the ambient one), as
// CompositeStore.WithTx marks its body's errors: a serialization failure
// stays retryable whether or not the observer moved the write into a
// transaction (tele-wp1-arch 21:57, design item 16). markTransient marks
// only real driver conflicts and is idempotent. Writers defer it with a
// named error result.
func markOwnTxErr(errp *error, tx, ambient *ent.Tx) {
	if tx != nil && tx != ambient {
		*errp = markTransient(*errp)
	}
}

// commitLifecycleWriteTx commits tx when the writer opened it (it is not
// the ambient transaction).
func commitLifecycleWriteTx(tx, ambient *ent.Tx) error {
	if tx == nil || tx == ambient {
		return nil
	}
	if err := tx.Commit(); err != nil {
		return markTransient(fmt.Errorf("commit transaction: %w", err))
	}
	return nil
}

// lifecycleBeforeRow reads the full row an observed writer is about to
// change, locked for update where the database supports row locks. It
// returns nil when no observer is installed or the row does not exist.
func lifecycleBeforeRow(ctx context.Context, client *ent.Client, obs store.AgentWriteObserver, uid uuid.UUID) (*store.Agent, error) {
	if obs == nil {
		return nil, nil
	}
	q := client.Agent.Query().Where(agent.IDEQ(uid))
	if client.Driver().Dialect() == dialect.Postgres {
		q = q.ForUpdate()
	}
	row, err := q.Only(ctx)
	if ent.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, mapError(err)
	}
	return entAgentToStore(row), nil
}

// lifecycleAfterRow reads the committed state of uid inside the writer's
// transaction, for the observer's after row.
func lifecycleAfterRow(ctx context.Context, client *ent.Client, uid uuid.UUID) (*store.Agent, error) {
	row, err := client.Agent.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}
	return entAgentToStore(row), nil
}

// observedAgentWrite runs write, an observed agent write that reports how
// many rows it changed, in one transaction with the observer (a new one,
// or the ambient transaction): a locked read of the before row, write,
// a read of the after row, the observer, commit. A write that changed no
// row is not observed. It is the observer entry point of writers whose
// write is a single conditional statement (the run-ID writers,
// ClaimAgentReincarnation).
func (s *AgentStore) observedAgentWrite(ctx context.Context, obs store.AgentWriteObserver, uid uuid.UUID, op store.AgentWriteOp, hints store.AgentWriteHints, write func(client *ent.Client) (int, error)) (_ int, err error) {
	client, tx, done, err := s.lifecycleWriteTx(ctx, obs)
	if err != nil {
		return 0, err
	}
	defer done()
	defer markOwnTxErr(&err, tx, s.entTx)
	before, err := lifecycleBeforeRow(ctx, client, obs, uid)
	if err != nil {
		return 0, err
	}
	n, err := write(client)
	if err != nil || n == 0 {
		return n, err
	}
	if before != nil {
		after, err := lifecycleAfterRow(ctx, client, uid)
		if err != nil {
			return 0, err
		}
		if err := observeAgentWrite(ctx, obs, client, tx, store.AgentWrite{
			Op: op, Before: before, After: after, Now: time.Now(), Hints: hints,
		}); err != nil {
			return 0, err
		}
	}
	if err := commitLifecycleWriteTx(tx, s.entTx); err != nil {
		return 0, err
	}
	return n, nil
}

// RecordRestartStopLeg records that a restart's stop leg confirmed run
// runID stopped (WP1 design 2.4a step 3, r5 B1). In its own transaction it
// reads the agent row under lock and runs the observer, which closes the
// open run segment as stopped(stopped, cause=restart_stop_leg) when it is
// runID, and does nothing otherwise. It writes only the lifecycle cursor
// and outbox, never an agents column, and does nothing while no observer
// is installed or when the agent is gone. It is not part of store.Store:
// the hub type-asserts for it.
func (s *AgentStore) RecordRestartStopLeg(ctx context.Context, agentID, runID string) (err error) {
	obs := s.lifecycle.load()
	if obs == nil || runID == "" {
		return nil
	}
	uid, err := parseUUID(agentID)
	if err != nil {
		return err
	}
	client, tx, done, err := s.lifecycleWriteTx(ctx, obs)
	if err != nil {
		return err
	}
	defer done()
	defer markOwnTxErr(&err, tx, s.entTx)
	row, err := lifecycleBeforeRow(ctx, client, obs, uid)
	if err != nil || row == nil {
		return err
	}
	if err := observeAgentWrite(ctx, obs, client, tx, store.AgentWrite{
		Op: store.AgentWriteRestartStopLeg, Before: row, After: row, Now: time.Now(),
		Hints: store.AgentWriteHints{StopLegRunID: runID},
	}); err != nil {
		return err
	}
	return commitLifecycleWriteTx(tx, s.entTx)
}
