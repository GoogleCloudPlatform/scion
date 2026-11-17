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

//go:build !no_sqlite

package entadapter

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestDurableQueueEnqueueAndTxAtomicity(t *testing.T) {
	ctx := context.Background()
	cs := newTestCompositeStore(t)
	base := time.Date(2026, 11, 15, 3, 0, 0, 0, time.UTC)

	// Passing a non-transactional store handle or an unknown tx type must fail closed.
	_, err := cs.EnqueueDurableItem(ctx, cs, store.DurableQueueRow{
		Kind:     "test.echo",
		DedupKey: "bad-tx",
		Payload:  []byte(`{"msg":"bad"}`),
	})
	require.ErrorIs(t, err, store.ErrInvalidInput)

	_, err = cs.EnqueueDurableItem(ctx, cs.DurableQueueStore, store.DurableQueueRow{
		Kind:     "test.echo",
		DedupKey: "bad-substore-tx",
		Payload:  []byte(`{"msg":"bad"}`),
	})
	require.ErrorIs(t, err, store.ErrInvalidInput)

	otherCS := newTestCompositeStore(t)
	_, err = cs.EnqueueDurableItem(ctx, store.DurableQueueStore(otherCS.DurableQueueStore), store.DurableQueueRow{
		Kind:     "test.echo",
		DedupKey: "foreign-dqs-tx",
		Payload:  []byte(`{"msg":"bad"}`),
	})
	require.ErrorIs(t, err, store.ErrInvalidInput)

	_, err = cs.EnqueueDurableItem(ctx, struct{ x int }{x: 1}, store.DurableQueueRow{
		Kind:     "test.echo",
		DedupKey: "unknown-tx",
		Payload:  []byte(`{"msg":"bad"}`),
	})
	require.ErrorIs(t, err, store.ErrInvalidInput)

	// Presetting attempt, failure, or lease fields on insert must be ignored so
	// producers cannot enqueue pre-leased or pre-failed items.
	presetLease := base.Add(time.Hour)
	seqPreset, err := cs.EnqueueDurableItem(ctx, nil, store.DurableQueueRow{
		Kind:              "preset.check",
		DedupKey:          "preset-1",
		Payload:           []byte(`{"ok":true}`),
		CreatedAt:         base,
		NotBefore:         base,
		Attempts:          9,
		PermanentFailures: 4,
		LeaseOwner:        "sneaky-owner",
		LeaseUntil:        &presetLease,
		LastError:         "preset error",
	})
	require.NoError(t, err)
	presetClaimed, err := cs.ClaimDurableItems(ctx, "preset.check", "worker-1", base, base.Add(time.Minute), 1)
	require.NoError(t, err)
	require.Len(t, presetClaimed, 1)
	assert.Equal(t, seqPreset, presetClaimed[0].Seq)
	assert.Zero(t, presetClaimed[0].Attempts)
	assert.Zero(t, presetClaimed[0].PermanentFailures)
	assert.Empty(t, presetClaimed[0].LastError)

	// Rollback inside WithTx leaves neither the project nor the queue row.
	rollbackProjID := uuid.NewString()
	boom := errors.New("boom")
	err = cs.WithTx(ctx, func(tx store.Store) error {
		if err := tx.CreateProject(ctx, &store.Project{
			ID:   rollbackProjID,
			Name: "rb-proj",
			Slug: "rb-" + rollbackProjID[:8],
		}); err != nil {
			return err
		}
		_, err := cs.EnqueueDurableItem(ctx, tx, store.DurableQueueRow{
			Kind:      "test.echo",
			DedupKey:  "rb-1",
			ProjectID: rollbackProjID,
			Payload:   []byte(`{"msg":"rolled-back"}`),
			CreatedAt: base,
		})
		if err != nil {
			return err
		}
		return boom
	})
	require.ErrorIs(t, err, boom)

	_, err = cs.GetProject(ctx, rollbackProjID)
	require.ErrorIs(t, err, store.ErrNotFound)
	st, err := cs.DurableQueueStats(ctx, "test.echo", "", base)
	require.NoError(t, err)
	assert.Zero(t, st.Pending)

	// Commit inside WithTx persists both atomically; catching ErrDuplicate inside
	// the same transaction must not abort the transaction (including on Postgres).
	commitProjID := uuid.NewString()
	var seq1, seqOtherKind int64
	err = cs.WithTx(ctx, func(tx store.Store) error {
		if err := tx.CreateProject(ctx, &store.Project{
			ID:   commitProjID,
			Name: "ok-proj",
			Slug: "ok-" + commitProjID[:8],
		}); err != nil {
			return err
		}
		var enqErr error
		seq1, enqErr = cs.EnqueueDurableItem(ctx, tx, store.DurableQueueRow{
			Kind:      "test.echo",
			DedupKey:  "dup-key",
			ProjectID: commitProjID,
			Payload:   []byte(`{"msg":"first"}`),
			CreatedAt: base,
		})
		if enqErr != nil {
			return enqErr
		}
		// Duplicate (kind, dedup_key) returns ErrDuplicate without poisoning tx.
		_, dupErr := cs.EnqueueDurableItem(ctx, tx, store.DurableQueueRow{
			Kind:      "test.echo",
			DedupKey:  "dup-key",
			ProjectID: commitProjID,
			Payload:   []byte(`{"msg":"second"}`),
			CreatedAt: base,
		})
		if !errors.Is(dupErr, store.ErrDuplicate) || !errors.Is(dupErr, store.ErrAlreadyExists) {
			return errors.New("expected ErrDuplicate wrapping ErrAlreadyExists")
		}
		// Same dedup_key on a different kind succeeds inside the same tx.
		seqOtherKind, enqErr = cs.EnqueueDurableItem(ctx, tx, store.DurableQueueRow{
			Kind:      "other.kind",
			DedupKey:  "dup-key",
			ProjectID: commitProjID,
			Payload:   []byte(`{"msg":"other"}`),
			CreatedAt: base,
		})
		return enqErr
	})
	require.NoError(t, err)
	assert.Positive(t, seq1)
	assert.Greater(t, seqOtherKind, seq1)

	// Auto-generated UUID dedup keys and standalone (tx == nil) enqueue.
	seqAuto1, err := cs.EnqueueDurableItem(ctx, nil, store.DurableQueueRow{
		Kind:      "test.echo",
		Payload:   []byte(`{"auto":1}`),
		CreatedAt: base,
	})
	require.NoError(t, err)
	seqAuto2, err := cs.EnqueueDurableItem(ctx, nil, store.DurableQueueRow{
		Kind:      "test.echo",
		Payload:   []byte(`{"auto":2}`),
		CreatedAt: base,
	})
	require.NoError(t, err)
	assert.Greater(t, seqAuto2, seqAuto1)

	// Input validation.
	for _, badKind := range []string{"", "Upper.case", ".leading", "trailing.", "a..b", "bad-dash", strings.Repeat("a", 65)} {
		_, err := cs.EnqueueDurableItem(ctx, nil, store.DurableQueueRow{Kind: badKind, Payload: []byte(`{}`)})
		require.ErrorIs(t, err, store.ErrInvalidInput, "kind=%q", badKind)
	}
	_, err = cs.EnqueueDurableItem(ctx, nil, store.DurableQueueRow{
		Kind:     "test.echo",
		DedupKey: strings.Repeat("k", 257),
		Payload:  []byte(`{}`),
	})
	require.ErrorIs(t, err, store.ErrInvalidInput)
	_, err = cs.EnqueueDurableItem(ctx, nil, store.DurableQueueRow{
		Kind:    "test.echo",
		Payload: []byte{0xff, 0xfe},
	})
	require.ErrorIs(t, err, store.ErrInvalidInput)
	_, err = cs.EnqueueDurableItem(ctx, nil, store.DurableQueueRow{
		Kind:    "test.echo",
		Payload: []byte("nul\x00byte"),
	})
	require.ErrorIs(t, err, store.ErrInvalidInput)
}

func TestDurableQueueClaimOrderingAndLeaseLifecycle(t *testing.T) {
	ctx := context.Background()
	cs := newTestCompositeStore(t)
	t0 := time.Date(2026, 11, 15, 4, 0, 0, 0, time.UTC)

	// Enqueue 3 items of test.echo with distinct NotBefore / tie-break seq, plus 1 of another kind.
	seqDelayed, err := cs.EnqueueDurableItem(ctx, nil, store.DurableQueueRow{
		Kind:      "test.echo",
		DedupKey:  "delayed",
		Payload:   []byte(`{"id":"delayed"}`),
		CreatedAt: t0,
		NotBefore: t0.Add(2 * time.Minute),
	})
	require.NoError(t, err)

	seqEarly1, err := cs.EnqueueDurableItem(ctx, nil, store.DurableQueueRow{
		Kind:      "test.echo",
		DedupKey:  "early-1",
		Payload:   []byte(`{"id":"early-1"}`),
		CreatedAt: t0,
		NotBefore: t0.Add(10 * time.Second),
	})
	require.NoError(t, err)

	seqEarly2, err := cs.EnqueueDurableItem(ctx, nil, store.DurableQueueRow{
		Kind:      "test.echo",
		DedupKey:  "early-2",
		Payload:   []byte(`{"id":"early-2"}`),
		CreatedAt: t0,
		NotBefore: t0.Add(10 * time.Second),
	})
	require.NoError(t, err)

	_, err = cs.EnqueueDurableItem(ctx, nil, store.DurableQueueRow{
		Kind:      "other.kind",
		DedupKey:  "other",
		Payload:   []byte(`{"id":"other"}`),
		CreatedAt: t0,
		NotBefore: t0,
	})
	require.NoError(t, err)

	// Before t0+10s nothing in test.echo is due.
	claimed, err := cs.ClaimDurableItems(ctx, "test.echo", "owner-a", t0.Add(5*time.Second), t0.Add(35*time.Second), 10)
	require.NoError(t, err)
	assert.Empty(t, claimed)

	// At t0+30s early-1 and early-2 are due (in seq order); delayed is still future.
	now := t0.Add(30 * time.Second)
	claimed, err = cs.ClaimDurableItems(ctx, "test.echo", "owner-a", now, now.Add(30*time.Second), 10)
	require.NoError(t, err)
	require.Len(t, claimed, 2)
	assert.Equal(t, []int64{seqEarly1, seqEarly2}, []int64{claimed[0].Seq, claimed[1].Seq})
	assert.Equal(t, []byte(`{"id":"early-1"}`), claimed[0].Payload)

	// While leased, owner-b cannot claim them.
	otherClaim, err := cs.ClaimDurableItems(ctx, "test.echo", "owner-b", now.Add(5*time.Second), now.Add(35*time.Second), 10)
	require.NoError(t, err)
	assert.Empty(t, otherClaim)

	// Renew lease works only for owner-a.
	held, err := cs.RenewDurableLease(ctx, seqEarly1, "owner-b", now.Add(time.Minute))
	require.NoError(t, err)
	assert.False(t, held)
	held, err = cs.RenewDurableLease(ctx, seqEarly1, "owner-a", now.Add(time.Minute))
	require.NoError(t, err)
	assert.True(t, held)

	// Fail early-2 with multi-byte UTF-8 error > 256 bytes and schedule it after delayed.
	longErr := strings.Repeat("é", 200) // 400 UTF-8 bytes
	failHeld, err := cs.FailDurableItem(ctx, seqEarly2, "owner-a", store.DurableQueueFailure{
		CountAttempt:  true,
		Permanent:     true,
		NextAttemptAt: t0.Add(3 * time.Minute),
		LastError:     longErr,
	})
	require.NoError(t, err)
	assert.True(t, failHeld)

	// Complete early-1 as owner-a.
	done, err := cs.CompleteDurableItem(ctx, seqEarly1, "owner-a")
	require.NoError(t, err)
	assert.True(t, done)

	// At t0+4m both delayed (next_attempt_at=t0+2m) and early-2 (next_attempt_at=t0+3m)
	// are due.
	now4m := t0.Add(4 * time.Minute)
	claimed, err = cs.ClaimDurableItems(ctx, "test.echo", "owner-b", now4m, now4m.Add(30*time.Second), 10)
	require.NoError(t, err)
	require.Len(t, claimed, 2)
	assert.Equal(t, seqDelayed, claimed[0].Seq)
	assert.Equal(t, seqEarly2, claimed[1].Seq)
	assert.Equal(t, 1, claimed[1].Attempts)
	assert.Equal(t, 1, claimed[1].PermanentFailures)
	assert.LessOrEqual(t, len(claimed[1].LastError), store.MaxDurableQueueLastErrorBytes)
	assert.True(t, utf8.ValidString(claimed[1].LastError))

	// Reschedule seqDelayed (lower seq) to t0+6m and seqEarly2 (higher seq) to t0+5m
	// without counting attempts, then verify next_attempt_at ordering beats seq!
	failHeld, err = cs.FailDurableItem(ctx, seqDelayed, "owner-b", store.DurableQueueFailure{
		CountAttempt:  false,
		NextAttemptAt: t0.Add(6 * time.Minute),
	})
	require.NoError(t, err)
	assert.True(t, failHeld)
	failHeld, err = cs.FailDurableItem(ctx, seqEarly2, "owner-b", store.DurableQueueFailure{
		CountAttempt:  false,
		NextAttemptAt: t0.Add(5 * time.Minute),
	})
	require.NoError(t, err)
	assert.True(t, failHeld)

	now7m := t0.Add(7 * time.Minute)
	claimed, err = cs.ClaimDurableItems(ctx, "test.echo", "owner-a", now7m, now7m.Add(30*time.Second), 10)
	require.NoError(t, err)
	require.Len(t, claimed, 2)
	assert.Equal(t, []int64{seqEarly2, seqDelayed}, []int64{claimed[0].Seq, claimed[1].Seq})
	assert.Equal(t, 1, claimed[0].Attempts, "CountAttempt=false must not increment attempts")

	// Lost-lease re-claim: let owner-a's lease expire, owner-b re-claims seqEarly2,
	// and stale owner-a can neither Complete nor Fail it (leaving owner-b's lease
	// and the row's attempt/error fields completely intact).
	expiredAt := now7m.Add(31 * time.Second)
	ownerBLeaseUntil := expiredAt.Add(30 * time.Second)
	reclaimed, err := cs.ClaimDurableItems(ctx, "test.echo", "owner-b", expiredAt, ownerBLeaseUntil, 1)
	require.NoError(t, err)
	require.Len(t, reclaimed, 1)
	assert.Equal(t, seqEarly2, reclaimed[0].Seq)
	assert.Equal(t, []byte(`{"id":"early-2"}`), reclaimed[0].Payload)

	staleDone, err := cs.CompleteDurableItem(ctx, seqEarly2, "owner-a")
	require.NoError(t, err)
	assert.False(t, staleDone)

	staleFailHeld, err := cs.FailDurableItem(ctx, seqEarly2, "owner-a", store.DurableQueueFailure{
		CountAttempt:  true,
		Permanent:     true,
		NextAttemptAt: expiredAt.Add(time.Hour),
		LastError:     "stale",
	})
	require.NoError(t, err)
	assert.False(t, staleFailHeld, "stale owner's FailDurableItem must report held=false")

	// Verify owner-b's lease and the row's counters/last_error were untouched by stale owner-a.
	storedRow, err := cs.client.DurableQueueItem.Get(ctx, seqEarly2)
	require.NoError(t, err)
	assert.Equal(t, "owner-b", storedRow.LeaseOwner)
	require.NotNil(t, storedRow.LeaseUntil)
	assert.True(t, storedRow.LeaseUntil.Equal(ownerBLeaseUntil))
	assert.Equal(t, 1, storedRow.Attempts)
	assert.Equal(t, 1, storedRow.PermanentFailures)
	assert.Empty(t, storedRow.LastError)

	ownerBDone, err := cs.CompleteDurableItem(ctx, seqEarly2, "owner-b")
	require.NoError(t, err)
	assert.True(t, ownerBDone)
}

func TestDurableQueueTrimBoundsAndLargestProjectFirst(t *testing.T) {
	ctx := context.Background()
	cs := newTestCompositeStore(t)
	// Anchor to truncated wall clock (keeps Postgres timestamptz µs checks clean).
	now := time.Now().UTC().Truncate(time.Millisecond)

	enq := func(kind, proj, key string, createdAt time.Time, perm int, leased bool) int64 {
		t.Helper()
		seq, err := cs.EnqueueDurableItem(ctx, nil, store.DurableQueueRow{
			Kind:      kind,
			DedupKey:  key,
			ProjectID: proj,
			Payload:   []byte(`{"k":"` + key + `"}`),
			CreatedAt: createdAt,
			NotBefore: createdAt,
		})
		require.NoError(t, err)
		if perm > 0 || leased {
			// Drive failure count and lease state through the store contract
			// (EnqueueDurableItem ignores preset attempt/lease fields).
			upd := cs.client.DurableQueueItem.UpdateOneID(seq)
			if perm > 0 {
				upd = upd.SetPermanentFailures(perm).SetAttempts(perm)
			}
			if leased {
				upd = upd.SetLeaseOwner("live-worker").SetLeaseUntil(now.Add(5 * time.Minute))
			}
			require.NoError(t, upd.Exec(ctx))
		}
		return seq
	}

	// 1. Aged unleased row + aged leased row (leased must survive) + other kind aged row (must survive).
	agedSeq := enq("test.echo", "proj-small", "aged-unleased", now.Add(-73*time.Hour), 0, false)
	agedLeasedSeq := enq("test.echo", "proj-small", "aged-leased", now.Add(-73*time.Hour), 0, true)
	otherKindSeq := enq("other.kind", "proj-small", "other-aged", now.Add(-80*time.Hour), 9, false)

	// 2. Poison unleased row + poison leased row (leased must survive).
	poisonSeq := enq("test.echo", "proj-small", "poison-unleased", now.Add(-time.Minute), 5, false)
	poisonLeasedSeq := enq("test.echo", "proj-small", "poison-leased", now.Add(-time.Minute), 5, true)

	// 3. Overflow setup on remaining test.echo rows:
	// proj-small already has 2 live-leased rows above (agedLeasedSeq, poisonLeasedSeq).
	// Add 10 unleased rows to proj-flood (total 10), 4 unleased rows to proj-mid (total 4).
	// Total remaining after age+poison trim = 2 + 10 + 4 = 16.
	// With maxRows = 9, excess = 7:
	// - proj-flood drops 6 oldest (10 -> 4, now tied with proj-mid at 4).
	// - 1 remaining excess drops the single oldest between tied {proj-flood, proj-mid},
	//   which belongs to proj-flood (inserted earlier) -> proj-flood drops 7 total,
	//   proj-mid drops 0, proj-small drops 0!
	var floodSeqs []int64
	for i := 0; i < 10; i++ {
		floodSeqs = append(floodSeqs, enq("test.echo", "proj-flood", uuid.NewString(), now, 0, false))
	}
	for i := 0; i < 4; i++ {
		enq("test.echo", "proj-mid", uuid.NewString(), now, 0, false)
	}

	trim, err := cs.TrimDurableQueue(ctx, "test.echo", now, 72*time.Hour, 9, 5)
	require.NoError(t, err)
	assert.False(t, trim.More)

	require.Len(t, trim.Age, 1)
	assert.Equal(t, agedSeq, trim.Age[0].Seq)
	assert.Equal(t, "aged-unleased", trim.Age[0].DedupKey)
	assert.Equal(t, "proj-small", trim.Age[0].ProjectID)
	assert.Nil(t, trim.Age[0].Payload)

	require.Len(t, trim.Poison, 1)
	assert.Equal(t, poisonSeq, trim.Poison[0].Seq)
	assert.Equal(t, "poison-unleased", trim.Poison[0].DedupKey)
	assert.Nil(t, trim.Poison[0].Payload)

	require.Len(t, trim.Overflow, 7)
	for i := 0; i < 7; i++ {
		assert.Equal(t, floodSeqs[i], trim.Overflow[i].Seq)
		assert.Equal(t, "proj-flood", trim.Overflow[i].ProjectID)
		assert.Nil(t, trim.Overflow[i].Payload)
	}

	// Verify surviving counts per project and on other.kind.
	stFlood, err := cs.DurableQueueStats(ctx, "test.echo", "proj-flood", now)
	require.NoError(t, err)
	assert.Equal(t, int64(3), stFlood.Pending)

	stMid, err := cs.DurableQueueStats(ctx, "test.echo", "proj-mid", now)
	require.NoError(t, err)
	assert.Equal(t, int64(4), stMid.Pending)

	stSmall, err := cs.DurableQueueStats(ctx, "test.echo", "proj-small", now)
	require.NoError(t, err)
	assert.Equal(t, int64(2), stSmall.Pending)
	assert.Equal(t, int64(2), stSmall.Leased)
	_ = agedLeasedSeq
	_ = poisonLeasedSeq

	stOther, err := cs.DurableQueueStats(ctx, "other.kind", "", now)
	require.NoError(t, err)
	assert.Equal(t, int64(1), stOther.Pending)
	_ = otherKindSeq
}

func TestDurableQueueTrimPassCapAndMetadataProjection(t *testing.T) {
	ctx := context.Background()
	cs := newTestCompositeStore(t)
	cs.trimPassLimit = 4
	now := time.Date(2026, 11, 15, 5, 30, 0, 0, time.UTC)
	bigPayload := []byte(`{"blob":"` + strings.Repeat("x", 4096) + `"}`)

	insertRow := func(proj, key string, createdAt time.Time, perm int, leased bool) int64 {
		t.Helper()
		seq, err := cs.EnqueueDurableItem(ctx, nil, store.DurableQueueRow{
			Kind:      "test.echo",
			DedupKey:  key,
			ProjectID: proj,
			Payload:   bigPayload,
			CreatedAt: createdAt,
			NotBefore: createdAt,
		})
		require.NoError(t, err)
		if perm > 0 || leased {
			upd := cs.client.DurableQueueItem.UpdateOneID(seq)
			if perm > 0 {
				upd = upd.SetPermanentFailures(perm).SetAttempts(perm)
			}
			if leased {
				upd = upd.SetLeaseOwner("live-worker").SetLeaseUntil(now.Add(5 * time.Minute))
			}
			require.NoError(t, upd.Exec(ctx))
		}
		return seq
	}

	// Seed:
	// - 5 aged unleased rows (proj-small)
	// - 3 poison unleased rows (proj-small)
	// - 13 overflow unleased rows (proj-flood) + 2 unleased rows (proj-small) + 2 leased rows (proj-small)
	// After 5 aged + 3 poison are trimmed (8 rows), 17 rows remain (13 in proj-flood, 4 in proj-small).
	// With maxRows = 9 (excess = 8), proj-flood drops from 13 -> 5 (always strictly larger than
	// proj-small's 4), so all 8 overflow trims must come exclusively from proj-flood across
	// bounded passes of <= 4 rows!
	for i := 0; i < 5; i++ {
		insertRow("proj-small", uuid.NewString(), now.Add(-80*time.Hour), 0, false)
	}
	for i := 0; i < 3; i++ {
		insertRow("proj-small", uuid.NewString(), now.Add(-time.Minute), 5, false)
	}
	for i := 0; i < 13; i++ {
		insertRow("proj-flood", uuid.NewString(), now, 0, false)
	}
	for i := 0; i < 2; i++ {
		insertRow("proj-small", uuid.NewString(), now, 0, false)
	}
	for i := 0; i < 2; i++ {
		insertRow("proj-small", uuid.NewString(), now, 0, true)
	}

	var totalAge, totalPoison, totalOverflow int
	passes := 0
	for {
		passes++
		require.LessOrEqual(t, passes, 10, "bounded trim must converge in a finite number of passes")
		tr, err := cs.TrimDurableQueue(ctx, "test.echo", now, 72*time.Hour, 9, 5)
		require.NoError(t, err)

		passDropped := len(tr.Age) + len(tr.Poison) + len(tr.Overflow)
		assert.LessOrEqual(t, passDropped, 4, "each pass must obey trimPassLimit")
		for _, r := range append(append(tr.Age, tr.Poison...), tr.Overflow...) {
			assert.Positive(t, r.Seq)
			assert.Equal(t, "test.echo", r.Kind)
			assert.NotEmpty(t, r.DedupKey)
			assert.False(t, r.CreatedAt.IsZero())
			assert.Nil(t, r.Payload, "trimmed rows must omit payload")
		}
		for _, r := range tr.Overflow {
			assert.Equal(t, "proj-flood", r.ProjectID, "overflow must trim only the largest project")
		}
		totalAge += len(tr.Age)
		totalPoison += len(tr.Poison)
		totalOverflow += len(tr.Overflow)

		if !tr.More {
			break
		}
	}

	assert.Equal(t, 4, passes, "16 total drops at cap 4 -> 4 bounded passes")
	assert.Equal(t, 5, totalAge)
	assert.Equal(t, 3, totalPoison)
	assert.Equal(t, 8, totalOverflow)

	// Also verify no More busy-spin when remaining excess rows become live-leased only:
	// 9 rows remain (5 unleased in proj-flood + 2 unleased in proj-small + 2 leased in proj-small).
	// Lower maxRows to 1 (cap 4): Pass A drops 4 (More=true), Pass B drops remaining 3 unleased
	// leaving only the 2 live-leased rows above maxRows=1 -> More must be false!
	trCapA, err := cs.TrimDurableQueue(ctx, "test.echo", now, 72*time.Hour, 1, 5)
	require.NoError(t, err)
	assert.Len(t, trCapA.Overflow, 4)
	assert.True(t, trCapA.More)

	trLeasedOnly, err := cs.TrimDurableQueue(ctx, "test.echo", now, 72*time.Hour, 1, 5)
	require.NoError(t, err)
	assert.Len(t, trLeasedOnly.Overflow, 3)
	assert.False(t, trLeasedOnly.More, "More must be false when only live-leased rows remain above maxRows")
}

func TestDurableQueueStats(t *testing.T) {
	ctx := context.Background()
	cs := newTestCompositeStore(t)

	now := time.Date(2026, 11, 15, 6, 0, 0, 0, time.UTC)
	empty, err := cs.DurableQueueStats(ctx, "test.echo", "", now)
	require.NoError(t, err)
	assert.Zero(t, empty.Pending)
	assert.Zero(t, empty.Leased)
	assert.Nil(t, empty.Oldest)

	tOldest := now.Add(-10 * time.Minute)
	tNewer := now.Add(-2 * time.Minute)
	activeLease := now.Add(5 * time.Minute)
	expiredLease := now.Add(-time.Minute)

	_, err = cs.EnqueueDurableItem(ctx, nil, store.DurableQueueRow{
		Kind:      "test.echo",
		DedupKey:  "p1-leased",
		ProjectID: "p1",
		Payload:   []byte(`{}`),
		CreatedAt: tOldest,
		NotBefore: tOldest,
	})
	require.NoError(t, err)
	claimedActive, err := cs.ClaimDurableItems(ctx, "test.echo", "w1", tOldest, activeLease, 1)
	require.NoError(t, err)
	require.Len(t, claimedActive, 1)

	_, err = cs.EnqueueDurableItem(ctx, nil, store.DurableQueueRow{
		Kind:      "test.echo",
		DedupKey:  "p1-expired-lease",
		ProjectID: "p1",
		Payload:   []byte(`{}`),
		CreatedAt: tNewer,
		NotBefore: tNewer,
	})
	require.NoError(t, err)
	claimedExpired, err := cs.ClaimDurableItems(ctx, "test.echo", "w2", tNewer, expiredLease, 1)
	require.NoError(t, err)
	require.Len(t, claimedExpired, 1)

	_, err = cs.EnqueueDurableItem(ctx, nil, store.DurableQueueRow{
		Kind:      "test.echo",
		DedupKey:  "p2-unleased",
		ProjectID: "p2",
		Payload:   []byte(`{}`),
		CreatedAt: now,
	})
	require.NoError(t, err)

	allStats, err := cs.DurableQueueStats(ctx, "test.echo", "", now)
	require.NoError(t, err)
	assert.Equal(t, int64(3), allStats.Pending)
	assert.Equal(t, int64(1), allStats.Leased)
	require.NotNil(t, allStats.Oldest)
	assert.True(t, allStats.Oldest.Equal(tOldest))

	// Advancing caller now past activeLease flips Leased to 0 deterministically.
	afterExpiryStats, err := cs.DurableQueueStats(ctx, "test.echo", "", activeLease.Add(time.Millisecond))
	require.NoError(t, err)
	assert.Equal(t, int64(3), afterExpiryStats.Pending)
	assert.Zero(t, afterExpiryStats.Leased)

	p2Stats, err := cs.DurableQueueStats(ctx, "test.echo", "p2", now)
	require.NoError(t, err)
	assert.Equal(t, int64(1), p2Stats.Pending)
	assert.Zero(t, p2Stats.Leased)
	require.NotNil(t, p2Stats.Oldest)
	assert.True(t, p2Stats.Oldest.Equal(now))

	pendingAll, err := cs.CountDurableQueuePending(ctx, "test.echo", "")
	require.NoError(t, err)
	assert.Equal(t, int64(3), pendingAll)
	pendingP1, err := cs.CountDurableQueuePending(ctx, "test.echo", "p1")
	require.NoError(t, err)
	assert.Equal(t, int64(2), pendingP1)
	pendingP2, err := cs.CountDurableQueuePending(ctx, "test.echo", "p2")
	require.NoError(t, err)
	assert.Equal(t, int64(1), pendingP2)
	require.NoError(t, cs.WithTx(ctx, func(tx store.Store) error {
		txCounter, ok := tx.(store.DurableQueuePendingCounter)
		require.True(t, ok, "transaction store must implement DurableQueuePendingCounter")
		n, txErr := txCounter.CountDurableQueuePending(ctx, "test.echo", "p1")
		require.NoError(t, txErr)
		assert.Equal(t, int64(2), n)
		return nil
	}))
}

func TestDurableQueueConcurrentClaimDisjointSets(t *testing.T) {
	ctx := context.Background()
	cs := newTestCompositeStore(t)
	now := time.Date(2026, 11, 15, 6, 0, 0, 0, time.UTC)

	for round := 0; round < 5; round++ {
		kind := fmt.Sprintf("claim.round_%d", round)
		expected := make(map[int64]bool, 30)
		for i := 0; i < 30; i++ {
			seq, err := cs.EnqueueDurableItem(ctx, nil, store.DurableQueueRow{
				Kind:      kind,
				DedupKey:  fmt.Sprintf("item-%02d", i),
				Payload:   []byte(`{}`),
				CreatedAt: now,
				NotBefore: now,
			})
			require.NoError(t, err)
			expected[seq] = true
		}

		start := make(chan struct{})
		var (
			c1, c2     []store.DurableQueueRow
			err1, err2 error
			wg         sync.WaitGroup
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			c1, err1 = cs.ClaimDurableItems(ctx, kind, "claimer-1", now, now.Add(time.Minute), 20)
		}()
		go func() {
			defer wg.Done()
			<-start
			c2, err2 = cs.ClaimDurableItems(ctx, kind, "claimer-2", now, now.Add(time.Minute), 20)
		}()
		close(start)
		wg.Wait()

		require.NoError(t, err1)
		require.NoError(t, err2)
		assert.GreaterOrEqual(t, len(c1), 10, "round %d: claimer-1 should claim at least 10 of 30 rows", round)
		assert.GreaterOrEqual(t, len(c2), 10, "round %d: claimer-2 should claim at least 10 of 30 rows", round)
		assert.Equal(t, 30, len(c1)+len(c2), "round %d: disjoint claims must partition all 30 rows", round)

		seen := make(map[int64]string, 30)
		for _, r := range c1 {
			assert.NotContains(t, seen, r.Seq, "round %d: duplicate seq %d in claimer-1", round, r.Seq)
			seen[r.Seq] = "claimer-1"
		}
		for _, r := range c2 {
			assert.NotContains(t, seen, r.Seq, "round %d: seq %d claimed by both claimers", round, r.Seq)
			seen[r.Seq] = "claimer-2"
		}
		assert.Len(t, seen, len(expected))
	}
}

func TestDurableQueueTrimLockKey(t *testing.T) {
	k1 := durableQueueTrimLockKey("trigger.event")
	assert.Equal(t, k1, durableQueueTrimLockKey("trigger.event"))

	k2 := durableQueueTrimLockKey("message.deliver")
	k3 := durableQueueTrimLockKey("test.echo")
	assert.NotEqual(t, k1, k2)
	assert.NotEqual(t, k1, k3)
	assert.NotEqual(t, k2, k3)

	// Upper 32 bits carry the non-zero namespace prefix so per-kind trim locks
	// never collide with store.AdvisoryLockKey singletons (upper 32 bits == 0).
	for _, k := range []int64{k1, k2, k3} {
		assert.Equal(t, durableQueueTrimLockPrefix, k>>32)
	}
}

func TestDurableQueueConcurrentOverflowTrimExactExcess(t *testing.T) {
	ctx := context.Background()
	cs := newTestCompositeStore(t)
	now := time.Now().UTC().Truncate(time.Millisecond)

	for round := 0; round < 5; round++ {
		kind := fmt.Sprintf("test.trim.%d", round)
		for i := 0; i < 12; i++ {
			_, err := cs.EnqueueDurableItem(ctx, nil, store.DurableQueueRow{
				Kind:      kind,
				DedupKey:  fmt.Sprintf("item-%02d", i),
				ProjectID: "proj-a",
				Payload:   []byte(`{}`),
				CreatedAt: now,
				NotBefore: now,
			})
			require.NoError(t, err)
		}

		start := make(chan struct{})
		var (
			t1, t2     store.DurableQueueTrim
			err1, err2 error
			wg         sync.WaitGroup
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			t1, err1 = cs.TrimDurableQueue(ctx, kind, now, 0, 7, 0)
		}()
		go func() {
			defer wg.Done()
			<-start
			t2, err2 = cs.TrimDurableQueue(ctx, kind, now, 0, 7, 0)
		}()
		close(start)
		wg.Wait()

		require.NoError(t, err1)
		require.NoError(t, err2)
		assert.Equal(t, 5, len(t1.Overflow)+len(t2.Overflow),
			"round %d: concurrent overflow trims must drop exactly excess=5 rows, not 2*excess", round)

		stats, err := cs.DurableQueueStats(ctx, kind, "", now)
		require.NoError(t, err)
		assert.Equal(t, int64(7), stats.Pending, "round %d: remaining rows must equal maxRows=7", round)
	}
}

func TestDurableQueueNudge(t *testing.T) {
	ctx := context.Background()
	cs := newTestCompositeStore(t)
	t0 := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	now := t0.Add(30 * time.Second)

	// 1. Input validation: invalid kinds (even with empty key list) and oversized key lists.
	for _, badKind := range []string{"", "Upper.case", ".leading", "trailing.", "a..b", "bad-dash", strings.Repeat("a", 65)} {
		_, err := cs.NudgeDurableItems(ctx, badKind, nil, now)
		require.ErrorIs(t, err, store.ErrInvalidInput, "kind=%q", badKind)
	}

	oversized := make([]string, store.MaxDurableQueueNudgeKeys+1)
	for i := range oversized {
		oversized[i] = "same-dup-key" // pre-dedup length must still enforce the bound
	}
	_, err := cs.NudgeDurableItems(ctx, "test.echo", oversized, now)
	require.ErrorIs(t, err, store.ErrInvalidInput)

	// 2. Empty key list and unknown keys are no-ops.
	n, err := cs.NudgeDurableItems(ctx, "test.echo", nil, now)
	require.NoError(t, err)
	assert.Zero(t, n)

	n, err = cs.NudgeDurableItems(ctx, "test.echo", []string{}, now)
	require.NoError(t, err)
	assert.Zero(t, n)

	assert.False(t, store.IsNudgeableDedupKey(""))
	assert.True(t, store.IsNudgeableDedupKey("a"))
	assert.True(t, store.IsNudgeableDedupKey(strings.Repeat("x", store.MaxDurableQueueDedupKeyLen)))
	assert.False(t, store.IsNudgeableDedupKey(strings.Repeat("x", store.MaxDurableQueueDedupKeyLen+1)))

	n, err = cs.NudgeDurableItems(ctx, "test.echo", []string{"unknown-a", "", strings.Repeat("x", store.MaxDurableQueueDedupKeyLen+1)}, now)
	require.NoError(t, err)
	assert.Zero(t, n)

	// 3. Seed items covering every lease, kind, key, and next_attempt_at case.
	enq := func(kind, key string, notBefore time.Time) int64 {
		t.Helper()
		seq, eerr := cs.EnqueueDurableItem(ctx, nil, store.DurableQueueRow{
			Kind:      kind,
			DedupKey:  key,
			ProjectID: "proj-1",
			Payload:   []byte(`{"k":"` + key + `"}`),
			CreatedAt: t0,
			NotBefore: notBefore,
		})
		require.NoError(t, eerr)
		return seq
	}

	// Leased items: one with lease_until > now, one at exact boundary lease_until == now.
	seqLeasedActive := enq("test.echo", "leased-active", t0)
	seqLeasedBoundary := enq("test.echo", "leased-boundary", t0)
	claimedActive, err := cs.ClaimDurableItems(ctx, "test.echo", "worker-live", t0, now.Add(time.Minute), 1)
	require.NoError(t, err)
	require.Len(t, claimedActive, 1)
	assert.Equal(t, seqLeasedActive, claimedActive[0].Seq)
	claimedBoundary, err := cs.ClaimDurableItems(ctx, "test.echo", "worker-boundary", t0, now, 1)
	require.NoError(t, err)
	require.Len(t, claimedBoundary, 1)
	assert.Equal(t, seqLeasedBoundary, claimedBoundary[0].Seq)
	// Push both leased rows' next_attempt_at into the future while keeping their active leases,
	// proving NudgeDurableItems checks lease state rather than merely next_attempt_at <= now.
	futureAttempt := now.Add(10 * time.Minute)
	require.NoError(t, cs.client.DurableQueueItem.UpdateOneID(seqLeasedActive).SetNextAttemptAt(futureAttempt).Exec(ctx))
	require.NoError(t, cs.client.DurableQueueItem.UpdateOneID(seqLeasedBoundary).SetNextAttemptAt(futureAttempt).Exec(ctx))

	// Expired-lease future item (lease_until < now): unleased per queueItemUnleased(now), must be nudged.
	seqExpiredLease := enq("test.echo", "expired-lease", futureAttempt)
	require.NoError(t, cs.client.DurableQueueItem.UpdateOneID(seqExpiredLease).
		SetLeaseOwner("worker-stale").
		SetLeaseUntil(now.Add(-time.Millisecond)).
		Exec(ctx))

	// Unleased future items to nudge (inserted in order nudge-1 then nudge-2, with reversed NotBefore).
	seqNudge1 := enq("test.echo", "nudge-1", now.Add(5*time.Minute))
	seqNudge2 := enq("test.echo", "nudge-2", now.Add(2*time.Minute))

	// Already-due-earlier item (next_attempt_at < now) and due-at-exact-now item:
	// nudge must NEVER move next_attempt_at later than it already is.
	earlierDue := now.Add(-10 * time.Second)
	seqAlreadyEarly := enq("test.echo", "already-early", earlierDue)
	seqAlreadyAtNow := enq("test.echo", "already-at-now", now)

	// Unselected key of same kind, and same key ("nudge-1") on a different kind: both untouched.
	seqOtherKey := enq("test.echo", "untouched-key", now.Add(4*time.Minute))
	seqOtherKind := enq("other.kind", "nudge-1", now.Add(4*time.Minute))

	// Act: nudge a mix of eligible, leased, already-due, duplicate, and unknown keys.
	nudged, err := cs.NudgeDurableItems(ctx, "test.echo", []string{
		"leased-active",
		"leased-boundary",
		"expired-lease",
		"nudge-2",
		"nudge-1",
		"nudge-1", // duplicate key in input slice
		"already-early",
		"already-at-now",
		"does-not-exist",
	}, now)
	require.NoError(t, err)
	assert.Equal(t, 3, nudged, "only expired-lease, nudge-1, and nudge-2 should be updated")

	// Idempotency: repeating the exact same nudge at the same now updates 0 rows.
	nudgedAgain, err := cs.NudgeDurableItems(ctx, "test.echo", []string{"expired-lease", "nudge-1", "nudge-2"}, now)
	require.NoError(t, err)
	assert.Zero(t, nudgedAgain)

	// Verify leased rows were completely untouched.
	rowActive, err := cs.client.DurableQueueItem.Get(ctx, seqLeasedActive)
	require.NoError(t, err)
	assert.Equal(t, "worker-live", rowActive.LeaseOwner)
	assert.True(t, rowActive.NextAttemptAt.Equal(futureAttempt))

	rowBoundary, err := cs.client.DurableQueueItem.Get(ctx, seqLeasedBoundary)
	require.NoError(t, err)
	assert.Equal(t, "worker-boundary", rowBoundary.LeaseOwner)
	assert.True(t, rowBoundary.NextAttemptAt.Equal(futureAttempt))

	// Verify already-early kept its strictly earlier next_attempt_at.
	rowEarly, err := cs.client.DurableQueueItem.Get(ctx, seqAlreadyEarly)
	require.NoError(t, err)
	assert.True(t, rowEarly.NextAttemptAt.Equal(earlierDue), "nudge must not delay an item already due earlier")

	// Verify untouched key and other kind kept their future next_attempt_at.
	rowOtherKey, err := cs.client.DurableQueueItem.Get(ctx, seqOtherKey)
	require.NoError(t, err)
	assert.True(t, rowOtherKey.NextAttemptAt.Equal(now.Add(4*time.Minute)))

	rowOtherKind, err := cs.client.DurableQueueItem.Get(ctx, seqOtherKind)
	require.NoError(t, err)
	assert.True(t, rowOtherKind.NextAttemptAt.Equal(now.Add(4*time.Minute)))

	// Verify ClaimDurableItems at now claims the due unleased items in exact (next_attempt_at, seq) order:
	// 1. seqAlreadyEarly (next_attempt_at = now - 10s)
	// 2. seqExpiredLease, seqNudge1, seqNudge2, seqAlreadyAtNow (all next_attempt_at == now, ordered by seq ASC)
	claimed, err := cs.ClaimDurableItems(ctx, "test.echo", "drainer-1", now, now.Add(time.Minute), 20)
	require.NoError(t, err)
	gotSeqs := make([]int64, len(claimed))
	for i, r := range claimed {
		gotSeqs[i] = r.Seq
	}
	assert.Equal(t, []int64{
		seqAlreadyEarly,
		seqExpiredLease,
		seqNudge1,
		seqNudge2,
		seqAlreadyAtNow,
	}, gotSeqs)

	// Zero now defaults to time.Now().UTC(), and NudgeDurableItems inside a rolled-back WithTx leaves rows unchanged.
	farFuture := time.Now().UTC().Add(time.Hour).Truncate(time.Millisecond)
	seqTx := enq("test.tx_nudge", "tx-item", farFuture)
	rbErr := errors.New("rollback nudge")
	err = cs.WithTx(ctx, func(tx store.Store) error {
		dqs, ok := tx.(store.DurableQueueStore)
		require.True(t, ok)
		txNudged, nerr := dqs.NudgeDurableItems(ctx, "test.tx_nudge", []string{"tx-item"}, time.Time{})
		if nerr != nil {
			return nerr
		}
		assert.Equal(t, 1, txNudged)
		return rbErr
	})
	require.ErrorIs(t, err, rbErr)
	rowTxAfterRollback, err := cs.client.DurableQueueItem.Get(ctx, seqTx)
	require.NoError(t, err)
	assert.True(t, rowTxAfterRollback.NextAttemptAt.Equal(farFuture), "rolled-back tx nudge must not persist")

	zeroNowNudged, err := cs.NudgeDurableItems(ctx, "test.tx_nudge", []string{"tx-item"}, time.Time{})
	require.NoError(t, err)
	assert.Equal(t, 1, zeroNowNudged)
	claimedZeroNow, err := cs.ClaimDurableItems(ctx, "test.tx_nudge", "drainer-zero", time.Now().UTC().Add(time.Second), time.Now().UTC().Add(time.Minute), 1)
	require.NoError(t, err)
	require.Len(t, claimedZeroNow, 1)
	assert.Equal(t, seqTx, claimedZeroNow[0].Seq)
}

func TestDurableQueueNudgeMaxKeysAndConcurrentClaim(t *testing.T) {
	ctx := context.Background()
	cs := newTestCompositeStore(t)
	t0 := time.Date(2026, 11, 16, 13, 0, 0, 0, time.UTC)
	now := t0.Add(time.Minute)

	// 1. Exact MaxDurableQueueNudgeKeys (1000) succeeds on both SQLite and Postgres
	// and executes in durableQueueDeleteBatch-sized UPDATE chunks.
	var (
		countMaxUpdates atomic.Bool
		maxUpdateCalls  atomic.Int32
	)
	cs.client.DurableQueueItem.Use(func(next ent.Mutator) ent.Mutator {
		return ent.MutateFunc(func(ctx context.Context, m ent.Mutation) (ent.Value, error) {
			if countMaxUpdates.Load() && m.Op().Is(ent.OpUpdate) {
				maxUpdateCalls.Add(1)
			}
			return next.Mutate(ctx, m)
		})
	})

	keys := make([]string, store.MaxDurableQueueNudgeKeys)
	for i := range keys {
		keys[i] = fmt.Sprintf("max-key-%04d", i)
		_, err := cs.EnqueueDurableItem(ctx, nil, store.DurableQueueRow{
			Kind:      "test.max_nudge",
			DedupKey:  keys[i],
			Payload:   []byte(`{}`),
			CreatedAt: t0,
			NotBefore: t0.Add(time.Hour),
		})
		require.NoError(t, err)
	}
	countMaxUpdates.Store(true)
	nudged, err := cs.NudgeDurableItems(ctx, "test.max_nudge", keys, now)
	countMaxUpdates.Store(false)
	require.NoError(t, err)
	assert.Equal(t, store.MaxDurableQueueNudgeKeys, nudged)
	expectedChunks := (store.MaxDurableQueueNudgeKeys + durableQueueDeleteBatch - 1) / durableQueueDeleteBatch
	assert.Equal(t, int32(expectedChunks), maxUpdateCalls.Load())

	claimedMax, err := cs.ClaimDurableItems(ctx, "test.max_nudge", "claimer", now, now.Add(time.Minute), store.MaxDurableQueueNudgeKeys+10)
	require.NoError(t, err)
	assert.Len(t, claimedMax, store.MaxDurableQueueNudgeKeys)

	// 2. Interleaved nudges and claims across concurrent replicas:
	// two replicas nudge overlapping slices of keys in opposite orders while two
	// drainers concurrently poll and claim due items. NextAttemptAtGT(now) ensures
	// each of the itemCount future rows is nudged exactly once across both
	// nudgers (n1+n2 == itemCount), and claims partition all rows disjointly.
	const itemCount = 40
	raceKeys := make([]string, itemCount)
	for i := 0; i < itemCount; i++ {
		raceKeys[i] = fmt.Sprintf("race-%02d", i)
		_, err := cs.EnqueueDurableItem(ctx, nil, store.DurableQueueRow{
			Kind:      "test.race_nudge",
			DedupKey:  raceKeys[i],
			Payload:   []byte(`{}`),
			CreatedAt: t0,
			NotBefore: t0.Add(time.Hour),
		})
		require.NoError(t, err)
	}

	reversedKeys := make([]string, itemCount)
	for i := range raceKeys {
		reversedKeys[i] = raceKeys[itemCount-1-i]
	}

	nudgeOverlappingSlices := func(allKeys []string) (int, error) {
		total := 0
		const window, step = 12, 8
		for startIdx := 0; startIdx < len(allKeys); startIdx += step {
			endIdx := min(len(allKeys), startIdx+window)
			n, nerr := cs.NudgeDurableItems(ctx, "test.race_nudge", allKeys[startIdx:endIdx], now)
			if nerr != nil {
				return total, nerr
			}
			total += n
			time.Sleep(time.Millisecond)
		}
		return total, nil
	}

	start := make(chan struct{})
	nudgersDone := make(chan struct{})
	var (
		n1, n2       int
		nerr1, nerr2 error
		c1, c2       []store.DurableQueueRow
		cerr1, cerr2 error
		nudgerWG     sync.WaitGroup
		wg           sync.WaitGroup
	)
	claimInterleaved := func(owner string) ([]store.DurableQueueRow, error) {
		var out []store.DurableQueueRow
		for {
			nudgersFinished := false
			select {
			case <-nudgersDone:
				nudgersFinished = true
			default:
			}
			batch, cerr := cs.ClaimDurableItems(ctx, "test.race_nudge", owner, now, now.Add(time.Minute), 7)
			if cerr != nil {
				return out, cerr
			}
			out = append(out, batch...)
			if len(batch) == 0 {
				if nudgersFinished {
					return out, nil
				}
				time.Sleep(time.Millisecond)
			}
		}
	}

	nudgerWG.Add(2)
	wg.Add(4)
	go func() {
		defer wg.Done()
		defer nudgerWG.Done()
		<-start
		n1, nerr1 = nudgeOverlappingSlices(raceKeys)
	}()
	go func() {
		defer wg.Done()
		defer nudgerWG.Done()
		<-start
		n2, nerr2 = nudgeOverlappingSlices(reversedKeys)
	}()
	go func() {
		nudgerWG.Wait()
		close(nudgersDone)
	}()
	go func() {
		defer wg.Done()
		<-start
		c1, cerr1 = claimInterleaved("replica-1")
	}()
	go func() {
		defer wg.Done()
		<-start
		c2, cerr2 = claimInterleaved("replica-2")
	}()
	close(start)
	wg.Wait()

	require.NoError(t, nerr1)
	require.NoError(t, nerr2)
	assert.Equal(t, itemCount, n1+n2)

	require.NoError(t, cerr1)
	require.NoError(t, cerr2)
	assert.Equal(t, itemCount, len(c1)+len(c2))
	seen := make(map[int64]bool, itemCount)
	for _, r := range append(c1, c2...) {
		assert.False(t, seen[r.Seq], "seq %d claimed twice", r.Seq)
		seen[r.Seq] = true
	}

	// 3. Direct race on already-claimable rows (NotBefore == now) between
	// ClaimDurableItems(..., now, ...) and NudgeDurableItems(..., earlierNow):
	// any row leased before the nudge commits must keep NextAttemptAt == now.
	earlierNow := now.Add(-10 * time.Second)
	for i := 0; i < itemCount; i++ {
		_, err := cs.EnqueueDurableItem(ctx, nil, store.DurableQueueRow{
			Kind:      "test.race_claim_lease",
			DedupKey:  raceKeys[i],
			Payload:   []byte(`{}`),
			CreatedAt: t0,
			NotBefore: now,
		})
		require.NoError(t, err)
	}

	raceStart := make(chan struct{})
	var (
		nLease         int
		nLeaseErr      error
		lc1, lc2       []store.DurableQueueRow
		lcerr1, lcerr2 error
		leaseRaceWG    sync.WaitGroup
	)
	leaseRaceWG.Add(3)
	go func() {
		defer leaseRaceWG.Done()
		<-raceStart
		nLease, nLeaseErr = cs.NudgeDurableItems(ctx, "test.race_claim_lease", raceKeys, earlierNow)
	}()
	go func() {
		defer leaseRaceWG.Done()
		<-raceStart
		lc1, lcerr1 = cs.ClaimDurableItems(ctx, "test.race_claim_lease", "replica-1", now, now.Add(time.Minute), 25)
	}()
	go func() {
		defer leaseRaceWG.Done()
		<-raceStart
		lc2, lcerr2 = cs.ClaimDurableItems(ctx, "test.race_claim_lease", "replica-2", now, now.Add(time.Minute), 25)
	}()
	close(raceStart)
	leaseRaceWG.Wait()

	require.NoError(t, nLeaseErr)
	require.NoError(t, lcerr1)
	require.NoError(t, lcerr2)
	assert.Equal(t, itemCount, len(lc1)+len(lc2))
	loweredInDB := 0
	seenLease := make(map[int64]bool, itemCount)
	for _, r := range append(lc1, lc2...) {
		assert.False(t, seenLease[r.Seq], "seq %d claimed twice", r.Seq)
		seenLease[r.Seq] = true
		stored, gerr := cs.client.DurableQueueItem.Get(ctx, r.Seq)
		require.NoError(t, gerr)
		assert.True(t, stored.NextAttemptAt.Equal(r.NextAttemptAt), "leased row %d must not have next_attempt_at modified after claim", r.Seq)
		if stored.NextAttemptAt.Equal(earlierNow) {
			loweredInDB++
		}
	}
	assert.Equal(t, nLease, loweredInDB)
}

func TestDurableQueueNudgeChunking(t *testing.T) {
	ctx := context.Background()
	cs := newTestCompositeStore(t)
	t0 := time.Date(2026, 11, 16, 14, 0, 0, 0, time.UTC)
	t1 := t0.Add(10 * time.Minute)
	t2 := t0.Add(5 * time.Minute)

	const totalKeys = durableQueueDeleteBatch + 35 // 535 distinct keys -> chunks of 500 and 35
	var (
		chunk1Eligible int
		chunk2Eligible int
		alreadyDue     int
	)
	distinctKeys := make([]string, totalKeys)
	for i := 0; i < totalKeys; i++ {
		key := fmt.Sprintf("chunk-key-%04d", i)
		distinctKeys[i] = key
		notBefore := t0.Add(2 * time.Hour)
		if i%25 == 0 {
			notBefore = t0 // already due before both t2 and t1
			alreadyDue++
		} else if i < durableQueueDeleteBatch {
			chunk1Eligible++
		} else {
			chunk2Eligible++
		}
		_, err := cs.EnqueueDurableItem(ctx, nil, store.DurableQueueRow{
			Kind:      "test.chunk_nudge",
			DedupKey:  key,
			Payload:   []byte(`{}`),
			CreatedAt: t0,
			NotBefore: notBefore,
		})
		require.NoError(t, err)
	}
	totalEligible := chunk1Eligible + chunk2Eligible

	// Build an unsorted input slice with duplicates and an unknown key (within MaxDurableQueueNudgeKeys).
	inputKeys := make([]string, 0, totalKeys+20)
	for i := totalKeys - 1; i >= 0; i-- {
		inputKeys = append(inputKeys, distinctKeys[i])
	}
	inputKeys = append(inputKeys, distinctKeys[:15]...)
	inputKeys = append(inputKeys, "z-chunk-key-missing") // sorts after chunk-key-*, into chunk 2
	require.LessOrEqual(t, len(inputKeys), store.MaxDurableQueueNudgeKeys)

	var (
		trackUpdates     bool
		updateCalls      int
		failOnUpdateCall int
	)
	injectedErr := &sqliteFakeErr{code: sqliteBusy}
	cs.client.DurableQueueItem.Use(func(next ent.Mutator) ent.Mutator {
		return ent.MutateFunc(func(ctx context.Context, m ent.Mutation) (ent.Value, error) {
			if trackUpdates && m.Op().Is(ent.OpUpdate) {
				updateCalls++
				if failOnUpdateCall > 0 && updateCalls == failOnUpdateCall {
					return nil, injectedErr
				}
			}
			return next.Mutate(ctx, m)
		})
	})

	// 1. Multi-chunk pass nudges all eligible rows across both chunks and sums the count.
	trackUpdates = true
	updateCalls = 0
	nudged, err := cs.NudgeDurableItems(ctx, "test.chunk_nudge", inputKeys, t1)
	require.NoError(t, err)
	assert.Equal(t, 2, updateCalls, "536 unique keys must execute in 2 UPDATE batches")
	assert.Equal(t, totalEligible, nudged)

	// 2. Mid-loop error on the second chunk returns chunk 1's running total and maps the error as transient.
	updateCalls = 0
	failOnUpdateCall = 2
	partial, err := cs.NudgeDurableItems(ctx, "test.chunk_nudge", inputKeys, t2)
	require.Error(t, err)
	assert.ErrorIs(t, err, injectedErr)
	assert.ErrorIs(t, err, store.ErrTransient, "chunk update busy failure should wrap store.ErrTransient")
	assert.Equal(t, 2, updateCalls)
	assert.Equal(t, chunk1Eligible, partial, "partial count must reflect completed chunks before the error")

	// 3. Retrying at t2 after clearing the fault nudges only the remaining second-chunk rows.
	updateCalls = 0
	failOnUpdateCall = 0
	rem, err := cs.NudgeDurableItems(ctx, "test.chunk_nudge", inputKeys, t2)
	trackUpdates = false
	require.NoError(t, err)
	assert.Equal(t, 2, updateCalls)
	assert.Equal(t, chunk2Eligible, rem)

	claimed, err := cs.ClaimDurableItems(ctx, "test.chunk_nudge", "claimer", t2, t2.Add(time.Minute), totalKeys+10)
	require.NoError(t, err)
	assert.Len(t, claimed, alreadyDue+totalEligible)
}
