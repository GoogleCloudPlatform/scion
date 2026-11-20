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
	"hash/fnv"
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
var _ store.DurableQueuePendingCounter = (*CompositeStore)(nil)
var _ store.DurableQueuePendingCounter = (*DurableQueueStore)(nil)
var _ store.DurableQueueFairClaimer = (*CompositeStore)(nil)
var _ store.DurableQueueFairClaimer = (*DurableQueueStore)(nil)

// durableQueueDeleteBatch bounds the IN lists of durable queue queries and
// mutations (such as trim deletes and nudge updates).
const durableQueueDeleteBatch = 500

// durableQueueTrimPassLimit caps total trim candidates selected and deleted in
// a single TrimDurableQueue pass so maintenance stays bounded in memory and
// lock scope even after an extended stall or lowered retention window.
const durableQueueTrimPassLimit = 1000

// durableQueueTrimLockPrefix namespaces per-kind durable queue trim advisory
// locks in the upper 32 bits of Postgres's 64-bit pg_advisory_xact_lock(bigint)
// keyspace, away from store.AdvisoryLockKey constants (whose upper 32 bits are
// zero).
const durableQueueTrimLockPrefix int64 = 0x5C10_2001

func durableQueueTrimLockKey(kind string) int64 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(kind))
	return (durableQueueTrimLockPrefix << 32) | int64(h.Sum32())
}

func lockDurableQueueTrim(ctx context.Context, client *ent.Client, kind string) error {
	if client.Driver().Dialect() != dialect.Postgres {
		return nil
	}
	var res sql.Result
	if err := client.Driver().Exec(ctx, "SELECT pg_advisory_xact_lock($1)", []any{durableQueueTrimLockKey(kind)}, &res); err != nil {
		return fmt.Errorf("lock durable queue trim %q: %w", kind, err)
	}
	return nil
}

var durableQueueTrimSelectFields = []string{
	durablequeueitem.FieldID,
	durablequeueitem.FieldKind,
	durablequeueitem.FieldProjectID,
	durablequeueitem.FieldDedupKey,
	durablequeueitem.FieldCreatedAt,
}

// DurableQueueStore implements store.DurableQueueStore over Ent.
// Embedded by CompositeStore so callers can type-assert store.DurableQueueStore
// without expanding store.Store.
type DurableQueueStore struct {
	client        *ent.Client
	inTx          bool
	trimPassLimit int // test override; <= 0 uses durableQueueTrimPassLimit
}

func (s *DurableQueueStore) effectiveTrimPassLimit() int {
	if s.trimPassLimit > 0 {
		return s.trimPassLimit
	}
	return durableQueueTrimPassLimit
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

// durableQueueFairProbeMultiplier and durableQueueFairMinProbeBudget bound the
// number of distinct projects probed per ClaimDurableItemsFair call so claims
// stay O(budget * log N) regardless of non-due/leased backlog or project count.
const (
	durableQueueFairProbeMultiplier = 4
	durableQueueFairMinProbeBudget  = 16
)

// ClaimDurableItemsFair implements store.DurableQueueFairClaimer.
// It walks distinct project_ids of kind in lexicographic round-robin order
// starting strictly after afterProjectID using pure-index seeks (no due/lease
// filter in the project-step query), tests each project's eligibility with an
// indexed (kind, project_id, next_attempt_at, seq) probe, caps total projects
// probed per call at max(limit*4, 16), and returns a resume cursor so empty or
// short passes over backoff-only projects continue where they stopped on the
// next call. Claimed rows are selected in (next_attempt_at, seq) order per
// project (with FOR UPDATE SKIP LOCKED on PostgreSQL), topped up if an earlier
// project met its quota and slots remain, interleaved round-robin across
// projects, and leased via the unleased CAS UPDATE.
func (s *DurableQueueStore) ClaimDurableItemsFair(ctx context.Context, kind, owner, afterProjectID string, now, leaseUntil time.Time, limit int) (store.DurableQueueFairClaimResult, error) {
	if err := store.ValidateDurableQueueKind(kind); err != nil {
		return store.DurableQueueFairClaimResult{NextCursor: afterProjectID}, err
	}
	if owner == "" {
		return store.DurableQueueFairClaimResult{NextCursor: afterProjectID}, fmt.Errorf("%w: durable queue lease owner is required", store.ErrInvalidInput)
	}
	if limit <= 0 {
		return store.DurableQueueFairClaimResult{NextCursor: afterProjectID}, nil
	}
	now = now.UTC()
	leaseUntil = leaseUntil.UTC()

	result := store.DurableQueueFairClaimResult{NextCursor: afterProjectID}
	err := runInEntTx(ctx, s.client, s.inTx, func(client *ent.Client) error {
		probeBudget := max(limit*durableQueueFairProbeMultiplier, durableQueueFairMinProbeBudget)
		projects, resumeCursor, exhausted, wrapped, err := discoverDueQueueProjects(ctx, client, kind, afterProjectID, now, limit, probeBudget)
		if err != nil {
			return err
		}
		result.NextCursor = resumeCursor
		result.Exhausted = exhausted
		result.Wrapped = wrapped
		if len(projects) == 0 {
			if exhausted {
				// O(1) indexed check on (kind, next_attempt_at, seq): when zero
				// due unleased rows exist anywhere in the kind, clear Exhausted
				// so an idle hub with many backoff-only projects stops after a
				// single claim call instead of sweeping further.
				anyDue, err := hasAnyDueQueueRow(ctx, client, kind, now)
				if err != nil {
					return err
				}
				result.Exhausted = anyDue
			}
			return nil
		}

		selectProjectIDs := func(proj string, want int, exclude []int64) ([]int64, error) {
			preds := []predicate.DurableQueueItem{
				durablequeueitem.KindEQ(kind),
				durablequeueitem.ProjectIDEQ(proj),
				durablequeueitem.NextAttemptAtLTE(now),
				queueItemUnleased(now),
			}
			if len(exclude) > 0 {
				preds = append(preds, durablequeueitem.IDNotIn(exclude...))
			}
			q := client.DurableQueueItem.Query().
				Where(preds...).
				Order(
					ent.Asc(durablequeueitem.FieldNextAttemptAt),
					ent.Asc(durablequeueitem.FieldID),
				).
				Limit(want)
			if client.Driver().Dialect() == dialect.Postgres {
				q = q.ForUpdate(entsql.WithLockAction(entsql.SkipLocked))
			}
			return q.IDs(ctx)
		}

		perProj := make([][]int64, len(projects))
		full := make([]bool, len(projects))
		remaining := limit
		for idx, proj := range projects {
			if remaining <= 0 {
				break
			}
			remProjects := len(projects) - idx
			want := (remaining + remProjects - 1) / remProjects
			ids, err := selectProjectIDs(proj, want, nil)
			if err != nil {
				return err
			}
			perProj[idx] = ids
			full[idx] = len(ids) == want
			remaining -= len(ids)
		}
		for idx, proj := range projects {
			if remaining <= 0 {
				break
			}
			if !full[idx] {
				continue
			}
			extra, err := selectProjectIDs(proj, remaining, perProj[idx])
			if err != nil {
				return err
			}
			perProj[idx] = append(perProj[idx], extra...)
			remaining -= len(extra)
		}

		orderedIDs := make([]int64, 0, limit-remaining)
		for round := 0; ; round++ {
			added := false
			for _, ids := range perProj {
				if round < len(ids) {
					orderedIDs = append(orderedIDs, ids[round])
					added = true
				}
			}
			if !added {
				break
			}
		}
		if len(orderedIDs) == 0 {
			return nil
		}

		if _, err := client.DurableQueueItem.Update().
			Where(
				durablequeueitem.IDIn(orderedIDs...),
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
				durablequeueitem.IDIn(orderedIDs...),
				durablequeueitem.LeaseOwnerEQ(owner),
			).
			All(ctx)
		if err != nil {
			return err
		}
		byID := make(map[int64]*ent.DurableQueueItem, len(rows))
		for _, r := range rows {
			byID[r.ID] = r
		}
		out := make([]store.DurableQueueRow, 0, len(rows))
		for _, id := range orderedIDs {
			if r, ok := byID[id]; ok {
				out = append(out, entDurableQueueItemToStore(r))
			}
		}
		result.Rows = out
		return nil
	})
	if err != nil {
		return store.DurableQueueFairClaimResult{NextCursor: afterProjectID}, mapError(err)
	}
	return result, nil
}

func discoverDueQueueProjects(ctx context.Context, client *ent.Client, kind, afterProjectID string, now time.Time, limit, probeBudget int) (projects []string, lastProbed string, exhausted, wrapped bool, err error) {
	projects = make([]string, 0, limit)
	probes := 0
	lastProbed = afterProjectID

	hasDueRow := func(proj string) (bool, error) {
		ids, err := client.DurableQueueItem.Query().
			Where(
				durablequeueitem.KindEQ(kind),
				durablequeueitem.ProjectIDEQ(proj),
				durablequeueitem.NextAttemptAtLTE(now),
				queueItemUnleased(now),
			).
			Order(
				ent.Asc(durablequeueitem.FieldNextAttemptAt),
				ent.Asc(durablequeueitem.FieldID),
			).
			Limit(1).
			IDs(ctx)
		if err != nil {
			return false, err
		}
		return len(ids) > 0, nil
	}

	// Phase 1a: pure-index distinct-project walk strictly after afterProjectID.
	cursor := afterProjectID
	reachedEnd := false
	for len(projects) < limit && probes < probeBudget {
		next, err := client.DurableQueueItem.Query().
			Where(
				durablequeueitem.KindEQ(kind),
				durablequeueitem.ProjectIDGT(cursor),
			).
			Order(ent.Asc(durablequeueitem.FieldProjectID)).
			Limit(1).
			Select(durablequeueitem.FieldProjectID).
			Strings(ctx)
		if err != nil {
			return nil, afterProjectID, false, false, err
		}
		if len(next) == 0 {
			reachedEnd = true
			break
		}
		proj := next[0]
		cursor = proj
		lastProbed = proj
		probes++

		due, err := hasDueRow(proj)
		if err != nil {
			return nil, afterProjectID, false, false, err
		}
		if due {
			projects = append(projects, proj)
		}
	}

	if len(projects) >= limit {
		return projects, lastProbed, false, false, nil
	}
	if !reachedEnd {
		// Stopped because probes reached probeBudget before reaching the end of the range.
		return projects, lastProbed, true, false, nil
	}

	// Phase 1b: budgeted wrap-around from the start of the kind through
	// afterProjectID (reached end of range with budget remaining).
	wrapped = true
	completedCycle := false
	firstWrap := true
	wrapCursor := ""
	for len(projects) < limit && probes < probeBudget {
		preds := []predicate.DurableQueueItem{
			durablequeueitem.KindEQ(kind),
			durablequeueitem.ProjectIDLTE(afterProjectID),
		}
		if !firstWrap {
			preds = append(preds, durablequeueitem.ProjectIDGT(wrapCursor))
		}
		next, err := client.DurableQueueItem.Query().
			Where(preds...).
			Order(ent.Asc(durablequeueitem.FieldProjectID)).
			Limit(1).
			Select(durablequeueitem.FieldProjectID).
			Strings(ctx)
		if err != nil {
			return nil, afterProjectID, false, false, err
		}
		if len(next) == 0 {
			completedCycle = true
			break
		}
		proj := next[0]
		wrapCursor = proj
		lastProbed = proj
		firstWrap = false
		probes++

		due, err := hasDueRow(proj)
		if err != nil {
			return nil, afterProjectID, false, false, err
		}
		if due {
			projects = append(projects, proj)
		}
		if wrapCursor == afterProjectID {
			completedCycle = true
			break
		}
	}

	exhausted = !completedCycle && len(projects) < limit && probes >= probeBudget
	return projects, lastProbed, exhausted, wrapped, nil
}

func hasAnyDueQueueRow(ctx context.Context, client *ent.Client, kind string, now time.Time) (bool, error) {
	ids, err := client.DurableQueueItem.Query().
		Where(
			durablequeueitem.KindEQ(kind),
			durablequeueitem.NextAttemptAtLTE(now),
			queueItemUnleased(now),
		).
		Order(
			ent.Asc(durablequeueitem.FieldNextAttemptAt),
			ent.Asc(durablequeueitem.FieldID),
		).
		Limit(1).
		IDs(ctx)
	if err != nil {
		return false, err
	}
	return len(ids) > 0, nil
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

func entDurableQueueTrimItemToStore(r *ent.DurableQueueItem) store.DurableQueueRow {
	return store.DurableQueueRow{
		Seq:       r.ID,
		Kind:      r.Kind,
		DedupKey:  r.DedupKey,
		ProjectID: r.ProjectID,
		CreatedAt: r.CreatedAt.UTC(),
	}
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
				gone = append(gone, entDurableQueueTrimItemToStore(r))
			}
		}
	}
	return gone, nil
}

// TrimDurableQueue implements store.DurableQueueStore.
// Rows under a live lease are never trimmed. Candidate queries project only
// metadata columns (seq, kind, project_id, dedup_key, created_at) and cap total
// candidates per pass; when the cap is reached with eligible rows remaining,
// DurableQueueTrim.More is set so the caller continues on the next pass.
// Overflow trimming removes the oldest unleased items of the largest project
// first (stage1-design §6.1 F5), never trimming a smaller project while a
// larger project still has unleased items. On PostgreSQL a transaction-scoped
// per-kind advisory lock serializes concurrent trims so replicas never each
// delete excess rows computed from the same unlocked COUNT.
func (s *DurableQueueStore) TrimDurableQueue(ctx context.Context, kind string, now time.Time, maxAge time.Duration, maxRows, maxPermanent int) (store.DurableQueueTrim, error) {
	if err := store.ValidateDurableQueueKind(kind); err != nil {
		return store.DurableQueueTrim{}, err
	}
	now = now.UTC()
	passLimit := s.effectiveTrimPassLimit()

	var trim store.DurableQueueTrim
	err := runInEntTx(ctx, s.client, s.inTx, func(client *ent.Client) error {
		if err := lockDurableQueueTrim(ctx, client, kind); err != nil {
			return err
		}
		budget := passLimit

		if maxAge > 0 && budget > 0 {
			aged, err := lockQueueTrimCandidates(client, client.DurableQueueItem.Query().
				Where(
					durablequeueitem.KindEQ(kind),
					queueItemUnleased(now),
					durablequeueitem.CreatedAtLT(now.Add(-maxAge)),
				).
				Order(ent.Asc(durablequeueitem.FieldID)).
				Limit(budget+1)).
				Select(durableQueueTrimSelectFields...).
				All(ctx)
			if err != nil {
				return err
			}
			if len(aged) > budget {
				aged = aged[:budget]
				trim.More = true
			}
			budget -= len(aged)
			if trim.Age, err = deleteDurableQueueRows(ctx, client, now, aged); err != nil {
				return err
			}
		}

		if maxPermanent > 0 {
			pq := client.DurableQueueItem.Query().
				Where(
					durablequeueitem.KindEQ(kind),
					queueItemUnleased(now),
					durablequeueitem.PermanentFailuresGTE(maxPermanent),
				)
			if budget <= 0 {
				hasPoison, err := pq.Exist(ctx)
				if err != nil {
					return err
				}
				if hasPoison {
					trim.More = true
				}
			} else {
				poison, err := lockQueueTrimCandidates(client, pq.
					Order(ent.Asc(durablequeueitem.FieldID)).
					Limit(budget+1)).
					Select(durableQueueTrimSelectFields...).
					All(ctx)
				if err != nil {
					return err
				}
				if len(poison) > budget {
					poison = poison[:budget]
					trim.More = true
				}
				budget -= len(poison)
				if trim.Poison, err = deleteDurableQueueRows(ctx, client, now, poison); err != nil {
					return err
				}
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
				if budget <= 0 {
					hasUnleased, err := client.DurableQueueItem.Query().
						Where(durablequeueitem.KindEQ(kind), queueItemUnleased(now)).
						Exist(ctx)
					if err != nil {
						return err
					}
					if hasUnleased {
						trim.More = true
					}
				} else {
					overflow, moreOverflow, err := trimOverflowByLargestProject(ctx, client, kind, now, excess, budget)
					if err != nil {
						return err
					}
					trim.Overflow = overflow
					if moreOverflow {
						trim.More = true
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		return store.DurableQueueTrim{}, mapError(err)
	}
	return trim, nil
}

func trimOverflowByLargestProject(ctx context.Context, client *ent.Client, kind string, now time.Time, excess, candBudget int) ([]store.DurableQueueRow, bool, error) {
	var buckets []struct {
		ProjectID string `json:"project_id"`
		Count     int    `json:"count"`
	}
	if err := client.DurableQueueItem.Query().
		Where(durablequeueitem.KindEQ(kind)).
		GroupBy(durablequeueitem.FieldProjectID).
		Aggregate(ent.Count()).
		Scan(ctx, &buckets); err != nil {
		return nil, false, err
	}

	counts := make(map[string]int, len(buckets))
	for _, b := range buckets {
		counts[b.ProjectID] = b.Count
	}
	exhausted := make(map[string]bool)
	var dropped []store.DurableQueueRow

	for excess > 0 && candBudget > 0 {
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

		stepBudget := min(excess, candBudget)
		perProject := min(maxCount-secondMax, stepBudget/len(topProjects))
		if perProject > 0 {
			for _, proj := range topProjects {
				if excess <= 0 || candBudget <= 0 {
					break
				}
				limit := min(perProject, min(excess, candBudget))
				cand, err := lockQueueTrimCandidates(client, client.DurableQueueItem.Query().
					Where(
						durablequeueitem.KindEQ(kind),
						durablequeueitem.ProjectIDEQ(proj),
						queueItemUnleased(now),
					).
					Order(ent.Asc(durablequeueitem.FieldID)).
					Limit(limit)).
					Select(durableQueueTrimSelectFields...).
					All(ctx)
				if err != nil {
					return nil, false, err
				}
				candBudget -= len(cand)
				gone, err := deleteDurableQueueRows(ctx, client, now, cand)
				if err != nil {
					return nil, false, err
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

		// min(excess, candBudget) < len(topProjects): delete the single oldest
		// unleased row across the tied-for-largest projects, breaking ties on seq.
		cand, err := lockQueueTrimCandidates(client, client.DurableQueueItem.Query().
			Where(
				durablequeueitem.KindEQ(kind),
				durablequeueitem.ProjectIDIn(topProjects...),
				queueItemUnleased(now),
			).
			Order(ent.Asc(durablequeueitem.FieldID)).
			Limit(1)).
			Select(durableQueueTrimSelectFields...).
			All(ctx)
		if err != nil {
			return nil, false, err
		}
		if len(cand) == 0 {
			for _, proj := range topProjects {
				exhausted[proj] = true
			}
			continue
		}
		candBudget -= len(cand)
		gone, err := deleteDurableQueueRows(ctx, client, now, cand)
		if err != nil {
			return nil, false, err
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

	more := false
	if excess > 0 && candBudget <= 0 {
		var nonExhausted []string
		for proj, c := range counts {
			if !exhausted[proj] && c > 0 {
				nonExhausted = append(nonExhausted, proj)
			}
		}
		if len(nonExhausted) > 0 {
			hasUnleased, err := client.DurableQueueItem.Query().
				Where(
					durablequeueitem.KindEQ(kind),
					durablequeueitem.ProjectIDIn(nonExhausted...),
					queueItemUnleased(now),
				).
				Exist(ctx)
			if err != nil {
				return nil, false, err
			}
			more = hasUnleased
		}
	}
	return dropped, more, nil
}

// CountDurableQueuePending implements store.DurableQueuePendingCounter with a
// single COUNT query over durable_queue_items.
func (s *DurableQueueStore) CountDurableQueuePending(ctx context.Context, kind, projectID string) (int64, error) {
	if err := store.ValidateDurableQueueKind(kind); err != nil {
		return 0, err
	}
	preds := []predicate.DurableQueueItem{durablequeueitem.KindEQ(kind)}
	if projectID != "" {
		preds = append(preds, durablequeueitem.ProjectIDEQ(projectID))
	}
	n, err := s.client.DurableQueueItem.Query().
		Where(preds...).
		Count(ctx)
	if err != nil {
		return 0, mapError(err)
	}
	return int64(n), nil
}

// DurableQueueStats implements store.DurableQueueStore.
func (s *DurableQueueStore) DurableQueueStats(ctx context.Context, kind, projectID string, now time.Time) (store.DurableQueueStats, error) {
	pending, err := s.CountDurableQueuePending(ctx, kind, projectID)
	if err != nil {
		return store.DurableQueueStats{}, err
	}
	var st store.DurableQueueStats
	st.Pending = pending
	if pending == 0 {
		return st, nil
	}

	now = now.UTC()
	preds := []predicate.DurableQueueItem{durablequeueitem.KindEQ(kind)}
	if projectID != "" {
		preds = append(preds, durablequeueitem.ProjectIDEQ(projectID))
	}

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

// NudgeDurableItems implements store.DurableQueueStore.
func (s *DurableQueueStore) NudgeDurableItems(ctx context.Context, kind string, dedupKeys []string, now time.Time) (int, error) {
	if err := store.ValidateDurableQueueKind(kind); err != nil {
		return 0, err
	}
	if len(dedupKeys) > store.MaxDurableQueueNudgeKeys {
		return 0, fmt.Errorf("%w: durable queue nudge key count %d exceeds %d", store.ErrInvalidInput, len(dedupKeys), store.MaxDurableQueueNudgeKeys)
	}
	if len(dedupKeys) == 0 {
		return 0, nil
	}
	unique := make([]string, 0, len(dedupKeys))
	for _, k := range dedupKeys {
		if store.IsNudgeableDedupKey(k) {
			unique = append(unique, k)
		}
	}
	if len(unique) == 0 {
		return 0, nil
	}
	slices.Sort(unique)
	unique = slices.Compact(unique)

	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	total := 0
	for start := 0; start < len(unique); start += durableQueueDeleteBatch {
		chunk := unique[start:min(len(unique), start+durableQueueDeleteBatch)]
		n, err := s.client.DurableQueueItem.Update().
			Where(
				durablequeueitem.KindEQ(kind),
				durablequeueitem.DedupKeyIn(chunk...),
				queueItemUnleased(now),
				durablequeueitem.NextAttemptAtGT(now),
			).
			SetNextAttemptAt(now).
			Save(ctx)
		if err != nil {
			return total, markTransient(mapError(err))
		}
		total += n
	}
	return total, nil
}
