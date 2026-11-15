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

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"
	"unicode/utf8"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/durablequeueitem"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/predicate"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

var _ store.DurableQueueStore = (*CompositeStore)(nil)
var _ store.DurableQueueStore = (*DurableQueueStore)(nil)

// durableQueueDeleteBatch bounds the IN lists of trim deletes.
const durableQueueDeleteBatch = 500

// DurableQueueStore implements store.DurableQueueStore over Ent.
// Embedded by CompositeStore so callers can type-assert store.DurableQueueStore
// without expanding store.Store.
type DurableQueueStore struct {
	client *ent.Client
	inTx   bool
}

// NewDurableQueueStore creates an Ent-backed DurableQueueStore.
func NewDurableQueueStore(client *ent.Client) *DurableQueueStore {
	return &DurableQueueStore{client: client}
}

func queueItemUnleased(now time.Time) predicate.DurableQueueItem {
	return durablequeueitem.Or(
		durablequeueitem.LeaseUntilIsNil(),
		durablequeueitem.LeaseUntilLT(now),
	)
}

func entDurableQueueItemToStore(r *ent.DurableQueueItem) store.DurableQueueRow {
	var leaseUntil *time.Time
	if r.LeaseUntil != nil {
		lu := r.LeaseUntil.UTC()
		leaseUntil = &lu
	}
	return store.DurableQueueRow{
		Seq:               r.ID,
		Kind:              r.Kind,
		DedupKey:          r.DedupKey,
		ProjectID:         r.ProjectID,
		Payload:           []byte(r.Payload),
		NotBefore:         r.NotBefore.UTC(),
		CreatedAt:         r.CreatedAt.UTC(),
		Attempts:          r.Attempts,
		PermanentFailures: r.PermanentFailures,
		NextAttemptAt:     r.NextAttemptAt.UTC(),
		LeaseOwner:        r.LeaseOwner,
		LeaseUntil:        leaseUntil,
		LastError:         r.LastError,
	}
}

// resolveEnqueueClient resolves the Ent client to write to for EnqueueDurableItem.
// If tx is nil, s.client is used directly. If tx is non-nil, it must be an
// active Ent-backed transaction handle (*CompositeStore with inTx, *DurableQueueStore
// with inTx, or *ent.Tx); unknown or non-transactional handles fail closed so an
// enqueue never silently auto-commits outside the caller's transaction.
func (s *DurableQueueStore) resolveEnqueueClient(tx store.Tx) (*ent.Client, error) {
	if tx == nil {
		return s.client, nil
	}
	switch v := tx.(type) {
	case *CompositeStore:
		if !v.inTx {
			return nil, fmt.Errorf("%w: durable queue Enqueue tx is not an active transaction", store.ErrInvalidInput)
		}
		return v.client, nil
	case *DurableQueueStore:
		if !v.inTx {
			return nil, fmt.Errorf("%w: durable queue Enqueue tx is not an active transaction", store.ErrInvalidInput)
		}
		return v.client, nil
	case *ent.Tx:
		return v.Client(), nil
	default:
		return nil, fmt.Errorf("%w: unsupported durable queue transaction %T", store.ErrInvalidInput, tx)
	}
}

// EnqueueDurableItem implements store.DurableQueueStore.
func (s *DurableQueueStore) EnqueueDurableItem(ctx context.Context, tx store.Tx, row store.DurableQueueRow) (int64, error) {
	if err := store.ValidateDurableQueueKind(row.Kind); err != nil {
		return 0, err
	}
	dedupKey := row.DedupKey
	if dedupKey == "" {
		dedupKey = uuid.NewString()
	}
	if len(dedupKey) > store.MaxDurableQueueDedupKeyLen {
		return 0, fmt.Errorf("%w: durable queue dedup_key exceeds %d bytes", store.ErrInvalidInput, store.MaxDurableQueueDedupKeyLen)
	}
	if !utf8.Valid(row.Payload) || bytes.IndexByte(row.Payload, 0) >= 0 {
		return 0, fmt.Errorf("%w: durable queue payload must be valid UTF-8 without NUL bytes", store.ErrInvalidInput)
	}

	client, err := s.resolveEnqueueClient(tx)
	if err != nil {
		return 0, err
	}

	now := time.Now().UTC()
	createdAt := row.CreatedAt.UTC()
	if row.CreatedAt.IsZero() {
		createdAt = now
	}
	notBefore := row.NotBefore.UTC()
	if row.NotBefore.IsZero() {
		notBefore = createdAt
	}
	nextAttemptAt := row.NextAttemptAt.UTC()
	if row.NextAttemptAt.IsZero() {
		nextAttemptAt = notBefore
	}

	// Attempt, failure, and lease fields on row are intentionally ignored so a
	// producer cannot insert a pre-leased or pre-failed item.
	create := client.DurableQueueItem.Create().
		SetKind(row.Kind).
		SetDedupKey(dedupKey).
		SetProjectID(row.ProjectID).
		SetPayload(string(row.Payload)).
		SetNotBefore(notBefore).
		SetCreatedAt(createdAt).
		SetNextAttemptAt(nextAttemptAt)

	// Single-row Create + OnConflictColumns + DoNothing emits
	// INSERT ... ON CONFLICT (kind, dedup_key) DO NOTHING RETURNING seq:
	// - When inserted, RETURNING yields seq in one statement.
	// - When (kind, dedup_key) already exists, DO NOTHING suppresses only that
	//   unique conflict without aborting an enclosing PostgreSQL transaction
	//   (SQLSTATE 25P02) and RETURNING yields sql.ErrNoRows, which maps to
	//   store.ErrDuplicate. Other constraint errors surface through mapError.
	seq, err := create.
		OnConflictColumns(durablequeueitem.FieldKind, durablequeueitem.FieldDedupKey).
		DoNothing().
		ID(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, store.ErrDuplicate
		}
		return 0, markTransient(mapError(err))
	}
	return seq, nil
}

// ClaimDurableItems implements store.DurableQueueStore.
func (s *DurableQueueStore) ClaimDurableItems(ctx context.Context, kind, owner string, now, leaseUntil time.Time, limit int) ([]store.DurableQueueRow, error) {
	if err := store.ValidateDurableQueueKind(kind); err != nil {
		return nil, err
	}
	if owner == "" {
		return nil, fmt.Errorf("%w: durable queue lease owner is required", store.ErrInvalidInput)
	}
	if limit <= 0 {
		return nil, nil
	}
	now = now.UTC()
	leaseUntil = leaseUntil.UTC()

	var out []store.DurableQueueRow
	err := runInEntTx(ctx, s.client, s.inTx, func(client *ent.Client) error {
		q := client.DurableQueueItem.Query().
			Where(
				durablequeueitem.KindEQ(kind),
				durablequeueitem.NextAttemptAtLTE(now),
				queueItemUnleased(now),
			).
			Order(
				ent.Asc(durablequeueitem.FieldNextAttemptAt),
				ent.Asc(durablequeueitem.FieldID),
			).
			Limit(limit)
		if client.Driver().Dialect() == dialect.Postgres {
			q = q.ForUpdate(entsql.WithLockAction(entsql.SkipLocked))
		}
		ids, err := q.IDs(ctx)
		if err != nil || len(ids) == 0 {
			return err
		}
		if _, err := client.DurableQueueItem.Update().
			Where(
				durablequeueitem.IDIn(ids...),
				durablequeueitem.KindEQ(kind),
				queueItemUnleased(now),
			).
			SetLeaseOwner(owner).
			SetLeaseUntil(leaseUntil).
			Save(ctx); err != nil {
			return err
		}
		rows, err := client.DurableQueueItem.Query().
			Where(
				durablequeueitem.IDIn(ids...),
				durablequeueitem.LeaseOwnerEQ(owner),
			).
			Order(
				ent.Asc(durablequeueitem.FieldNextAttemptAt),
				ent.Asc(durablequeueitem.FieldID),
			).
			All(ctx)
		if err != nil {
			return err
		}
		out = make([]store.DurableQueueRow, 0, len(rows))
		for _, r := range rows {
			out = append(out, entDurableQueueItemToStore(r))
		}
		return nil
	})
	if err != nil {
		return nil, mapError(err)
	}
	return out, nil
}

// RenewDurableLease implements store.DurableQueueStore.
func (s *DurableQueueStore) RenewDurableLease(ctx context.Context, seq int64, owner string, leaseUntil time.Time) (bool, error) {
	if owner == "" {
		return false, fmt.Errorf("%w: durable queue lease owner is required", store.ErrInvalidInput)
	}
	n, err := s.client.DurableQueueItem.Update().
		Where(
			durablequeueitem.IDEQ(seq),
			durablequeueitem.LeaseOwnerEQ(owner),
		).
		SetLeaseUntil(leaseUntil.UTC()).
		Save(ctx)
	if err != nil {
		return false, markTransient(mapError(err))
	}
	return n > 0, nil
}

// CompleteDurableItem implements store.DurableQueueStore.
func (s *DurableQueueStore) CompleteDurableItem(ctx context.Context, seq int64, owner string) (bool, error) {
	if owner == "" {
		return false, fmt.Errorf("%w: durable queue lease owner is required", store.ErrInvalidInput)
	}
	n, err := s.client.DurableQueueItem.Delete().
		Where(
			durablequeueitem.IDEQ(seq),
			durablequeueitem.LeaseOwnerEQ(owner),
		).
		Exec(ctx)
	if err != nil {
		return false, markTransient(mapError(err))
	}
	return n > 0, nil
}

// FailDurableItem implements store.DurableQueueStore.
func (s *DurableQueueStore) FailDurableItem(ctx context.Context, seq int64, owner string, f store.DurableQueueFailure) (bool, error) {
	if owner == "" {
		return false, fmt.Errorf("%w: durable queue lease owner is required", store.ErrInvalidInput)
	}
	upd := s.client.DurableQueueItem.Update().
		Where(
			durablequeueitem.IDEQ(seq),
			durablequeueitem.LeaseOwnerEQ(owner),
		).
		SetNextAttemptAt(f.NextAttemptAt.UTC()).
		SetLastError(truncateUTF8Bytes(f.LastError, store.MaxDurableQueueLastErrorBytes)).
		SetLeaseOwner("").
		ClearLeaseUntil()
	if f.CountAttempt {
		upd = upd.AddAttempts(1)
	}
	if f.Permanent {
		upd = upd.AddPermanentFailures(1)
	}
	n, err := upd.Save(ctx)
	if err != nil {
		return false, markTransient(mapError(err))
	}
	return n > 0, nil
}

func lockQueueTrimCandidates(client *ent.Client, q *ent.DurableQueueItemQuery) *ent.DurableQueueItemQuery {
	if client.Driver().Dialect() == dialect.Postgres {
		q = q.ForUpdate(entsql.WithLockAction(entsql.SkipLocked))
	}
	return q
}

func deleteDurableQueueRows(ctx context.Context, client *ent.Client, now time.Time, rows []*ent.DurableQueueItem) ([]store.DurableQueueRow, error) {
	var gone []store.DurableQueueRow
	for start := 0; start < len(rows); start += durableQueueDeleteBatch {
		chunk := rows[start:min(len(rows), start+durableQueueDeleteBatch)]
		ids := make([]int64, len(chunk))
		for i, r := range chunk {
			ids[i] = r.ID
		}
		if _, err := client.DurableQueueItem.Delete().
			Where(durablequeueitem.IDIn(ids...), queueItemUnleased(now)).
			Exec(ctx); err != nil {
			return nil, err
		}
		left, err := client.DurableQueueItem.Query().
			Where(durablequeueitem.IDIn(ids...)).
			IDs(ctx)
		if err != nil {
			return nil, err
		}
		kept := make(map[int64]bool, len(left))
		for _, id := range left {
			kept[id] = true
		}
		for _, r := range chunk {
			if !kept[r.ID] {
				gone = append(gone, entDurableQueueItemToStore(r))
			}
		}
	}
	return gone, nil
}

// TrimDurableQueue implements store.DurableQueueStore.
// Rows under a live lease are never trimmed. Overflow trimming removes the
// oldest unleased items of the largest project first (stage1-design §6.1 F5),
// never trimming a smaller project while a larger project still has unleased
// items.
func (s *DurableQueueStore) TrimDurableQueue(ctx context.Context, kind string, now time.Time, maxAge time.Duration, maxRows, maxPermanent int) (store.DurableQueueTrim, error) {
	if err := store.ValidateDurableQueueKind(kind); err != nil {
		return store.DurableQueueTrim{}, err
	}
	now = now.UTC()

	var trim store.DurableQueueTrim
	err := runInEntTx(ctx, s.client, s.inTx, func(client *ent.Client) error {
		if maxAge > 0 {
			aged, err := lockQueueTrimCandidates(client, client.DurableQueueItem.Query().
				Where(
					durablequeueitem.KindEQ(kind),
					queueItemUnleased(now),
					durablequeueitem.CreatedAtLT(now.Add(-maxAge)),
				).
				Order(ent.Asc(durablequeueitem.FieldID))).
				All(ctx)
			if err != nil {
				return err
			}
			if trim.Age, err = deleteDurableQueueRows(ctx, client, now, aged); err != nil {
				return err
			}
		}

		if maxPermanent > 0 {
			poison, err := lockQueueTrimCandidates(client, client.DurableQueueItem.Query().
				Where(
					durablequeueitem.KindEQ(kind),
					queueItemUnleased(now),
					durablequeueitem.PermanentFailuresGTE(maxPermanent),
				).
				Order(ent.Asc(durablequeueitem.FieldID))).
				All(ctx)
			if err != nil {
				return err
			}
			if trim.Poison, err = deleteDurableQueueRows(ctx, client, now, poison); err != nil {
				return err
			}
		}

		if maxRows > 0 {
			total, err := client.DurableQueueItem.Query().
				Where(durablequeueitem.KindEQ(kind)).
				Count(ctx)
			if err != nil {
				return err
			}
			if excess := total - maxRows; excess > 0 {
				overflow, err := trimOverflowByLargestProject(ctx, client, kind, now, excess)
				if err != nil {
					return err
				}
				trim.Overflow = overflow
			}
		}
		return nil
	})
	if err != nil {
		return store.DurableQueueTrim{}, mapError(err)
	}
	return trim, nil
}

func trimOverflowByLargestProject(ctx context.Context, client *ent.Client, kind string, now time.Time, excess int) ([]store.DurableQueueRow, error) {
	var buckets []struct {
		ProjectID string `json:"project_id"`
		Count     int    `json:"count"`
	}
	if err := client.DurableQueueItem.Query().
		Where(durablequeueitem.KindEQ(kind)).
		GroupBy(durablequeueitem.FieldProjectID).
		Aggregate(ent.Count()).
		Scan(ctx, &buckets); err != nil {
		return nil, err
	}

	counts := make(map[string]int, len(buckets))
	for _, b := range buckets {
		counts[b.ProjectID] = b.Count
	}
	exhausted := make(map[string]bool)
	var dropped []store.DurableQueueRow

	for excess > 0 {
		maxCount := 0
		for proj, c := range counts {
			if !exhausted[proj] && c > maxCount {
				maxCount = c
			}
		}
		if maxCount == 0 {
			break
		}

		var topProjects []string
		secondMax := 0
		for proj, c := range counts {
			if exhausted[proj] || c <= 0 {
				continue
			}
			if c == maxCount {
				topProjects = append(topProjects, proj)
			} else if c > secondMax {
				secondMax = c
			}
		}
		sort.Strings(topProjects)

		perProject := min(maxCount-secondMax, excess/len(topProjects))
		if perProject > 0 {
			for _, proj := range topProjects {
				if excess <= 0 {
					break
				}
				limit := min(perProject, excess)
				cand, err := lockQueueTrimCandidates(client, client.DurableQueueItem.Query().
					Where(
						durablequeueitem.KindEQ(kind),
						durablequeueitem.ProjectIDEQ(proj),
						queueItemUnleased(now),
					).
					Order(ent.Asc(durablequeueitem.FieldID)).
					Limit(limit)).
					All(ctx)
				if err != nil {
					return nil, err
				}
				gone, err := deleteDurableQueueRows(ctx, client, now, cand)
				if err != nil {
					return nil, err
				}
				dropped = append(dropped, gone...)
				counts[proj] -= len(gone)
				excess -= len(gone)
				if len(gone) < limit {
					exhausted[proj] = true
				}
			}
			continue
		}

		// excess < len(topProjects): delete the single oldest unleased row
		// across the tied-for-largest projects, breaking the tie on seq.
		cand, err := lockQueueTrimCandidates(client, client.DurableQueueItem.Query().
			Where(
				durablequeueitem.KindEQ(kind),
				durablequeueitem.ProjectIDIn(topProjects...),
				queueItemUnleased(now),
			).
			Order(ent.Asc(durablequeueitem.FieldID)).
			Limit(1)).
			All(ctx)
		if err != nil {
			return nil, err
		}
		if len(cand) == 0 {
			for _, proj := range topProjects {
				exhausted[proj] = true
			}
			continue
		}
		gone, err := deleteDurableQueueRows(ctx, client, now, cand)
		if err != nil {
			return nil, err
		}
		if len(gone) == 0 {
			// Concurrent claimer took cand[0]; retry in the next iteration.
			if len(cand) > 0 {
				counts[cand[0].ProjectID]--
			}
			continue
		}
		dropped = append(dropped, gone...)
		for _, g := range gone {
			counts[g.ProjectID]--
		}
		excess -= len(gone)
	}

	sort.Slice(dropped, func(i, j int) bool {
		return dropped[i].Seq < dropped[j].Seq
	})
	return dropped, nil
}

// DurableQueueStats implements store.DurableQueueStore.
func (s *DurableQueueStore) DurableQueueStats(ctx context.Context, kind, projectID string) (store.DurableQueueStats, error) {
	if err := store.ValidateDurableQueueKind(kind); err != nil {
		return store.DurableQueueStats{}, err
	}
	preds := []predicate.DurableQueueItem{durablequeueitem.KindEQ(kind)}
	if projectID != "" {
		preds = append(preds, durablequeueitem.ProjectIDEQ(projectID))
	}

	var st store.DurableQueueStats
	n, err := s.client.DurableQueueItem.Query().
		Where(preds...).
		Count(ctx)
	if err != nil {
		return st, mapError(err)
	}
	st.Pending = int64(n)
	if n == 0 {
		return st, nil
	}

	now := time.Now().UTC()
	leasedPreds := append(slices.Clone(preds),
		durablequeueitem.LeaseUntilNotNil(),
		durablequeueitem.LeaseUntilGTE(now),
	)
	leased, err := s.client.DurableQueueItem.Query().
		Where(leasedPreds...).
		Count(ctx)
	if err != nil {
		return st, mapError(err)
	}
	st.Leased = int64(leased)

	first, err := s.client.DurableQueueItem.Query().
		Where(preds...).
		Order(ent.Asc(durablequeueitem.FieldCreatedAt)).
		Select(durablequeueitem.FieldCreatedAt).
		First(ctx)
	if err != nil && !ent.IsNotFound(err) {
		return st, mapError(err)
	}
	if first != nil {
		oldest := first.CreatedAt.UTC()
		st.Oldest = &oldest
	}
	return st, nil
}
