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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTriggerOfflineQueueStore_ParkFinishAttemptListAndCaps(t *testing.T) {
	ctx := context.Background()
	cs := newTestCompositeStore(t)
	t.Logf("enttest.Active()=%v (postgres=%v)", enttest.Active(), enttest.Active())

	projID := uuid.NewString()
	trID := uuid.NewString()
	agentA := uuid.NewString()
	agentB := uuid.NewString()
	base := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)

	tr := &store.Trigger{
		ID:             trID,
		ProjectID:      projID,
		Name:           "offline-store-trig",
		SourceKind:     store.TriggerSourceKindWebhook,
		SourceID:       uuid.NewString(),
		MatchTypes:     []string{"webhook.received"},
		ActionType:     store.TriggerActionMessage,
		ActionPayload:  `{"agentId":"` + agentA + `","message":"hi","offline":"queue"}`,
		SubjectDialect: store.TriggerDialectCEL,
		SubjectMode:    store.TriggerSubjectModeNew,
		Status:         store.TriggerStatusActive,
		CreatedAt:      base,
		UpdatedAt:      base,
	}
	require.NoError(t, cs.CreateTrigger(ctx, tr))

	claimFire := func(createdAt time.Time) *store.TriggerFire {
		f := &store.TriggerFire{
			ID:         uuid.NewString(),
			ProjectID:  projID,
			TriggerID:  trID,
			EventID:    uuid.NewString(),
			Subject:    "s1",
			ActionType: store.TriggerActionMessage,
			LeaseUntil: createdAt.Add(10 * time.Minute),
			StartedAt:  createdAt,
			CreatedAt:  createdAt,
		}
		claimed, _, err := cs.ClaimTriggerFire(ctx, f)
		require.NoError(t, err)
		require.True(t, claimed)
		return f
	}

	f1 := claimFire(base)
	f2 := claimFire(base.Add(time.Second))
	f3 := claimFire(base.Add(2 * time.Second))
	f4 := claimFire(base.Add(3 * time.Second))

	waitUntil := base.Add(time.Hour)

	// Park f1 and f2 for agentA with per-target cap=2, per-project cap=3.
	parked, capHit, err := cs.ParkTriggerFireWaiting(ctx, f1.ID, projID, agentA, "target not running", waitUntil, 2, 3)
	require.NoError(t, err)
	assert.True(t, parked)
	assert.False(t, capHit)

	// Re-parking f1 (already waiting) is a CAS no-op (not parked, cap not exceeded).
	parkedAgain, capHitAgain, err := cs.ParkTriggerFireWaiting(ctx, f1.ID, projID, agentA, "target not running", waitUntil, 2, 3)
	require.NoError(t, err)
	assert.False(t, parkedAgain)
	assert.False(t, capHitAgain)

	parked, capHit, err = cs.ParkTriggerFireWaiting(ctx, f2.ID, projID, agentA, "target not running", waitUntil, 2, 3)
	require.NoError(t, err)
	assert.True(t, parked)
	assert.False(t, capHit)

	// 3rd waiting fire for agentA breaches per-target cap (2).
	parked, capHit, err = cs.ParkTriggerFireWaiting(ctx, f3.ID, projID, agentA, "target not running", waitUntil, 2, 3)
	require.NoError(t, err)
	assert.False(t, parked)
	assert.True(t, capHit)

	// agentB can still park 1 fire (project total becomes 3 == per-project cap).
	parked, capHit, err = cs.ParkTriggerFireWaiting(ctx, f3.ID, projID, agentB, "target not running", waitUntil, 2, 3)
	require.NoError(t, err)
	assert.True(t, parked)
	assert.False(t, capHit)

	// Next fire for agentB breaches per-project cap (3).
	parked, capHit, err = cs.ParkTriggerFireWaiting(ctx, f4.ID, projID, agentB, "target not running", waitUntil, 2, 3)
	require.NoError(t, err)
	assert.False(t, parked)
	assert.True(t, capHit)

	// ListWaitingFiresForTarget returns agentA's fires in (created ASC, id ASC) order.
	listA, err := cs.ListWaitingFiresForTarget(ctx, agentA, 100)
	require.NoError(t, err)
	assert.Equal(t, []string{f1.ID, f2.ID}, listA)

	listALimited, err := cs.ListWaitingFiresForTarget(ctx, agentA, 1)
	require.NoError(t, err)
	assert.Equal(t, []string{f1.ID}, listALimited)

	// Verify WaitUntil and Attempts persisted on f1.
	loaded1, err := cs.GetTriggerFire(ctx, f1.ID)
	require.NoError(t, err)
	assert.Equal(t, store.TriggerFireStatusWaiting, loaded1.Status)
	assert.Equal(t, agentA, loaded1.TargetAgentID)
	assert.Equal(t, 1, loaded1.Attempts)
	require.NotNil(t, loaded1.WaitUntil)
	assert.True(t, loaded1.WaitUntil.UTC().Equal(waitUntil))
	assert.Nil(t, loaded1.FinishedAt)

	// ClaimWaitingTriggerFire atomically transitions f1 waiting -> firing with a
	// fresh lease and bumped attempt count; a second claim on the firing row is
	// a CAS no-op.
	retryLease := base.Add(12 * time.Minute)
	claimed, err := cs.ClaimWaitingTriggerFire(ctx, f1.ID, 2, retryLease)
	require.NoError(t, err)
	assert.True(t, claimed)
	claimedAgain, err := cs.ClaimWaitingTriggerFire(ctx, f1.ID, 3, retryLease)
	require.NoError(t, err)
	assert.False(t, claimedAgain, "second claim on firing row must not succeed")
	loaded1, err = cs.GetTriggerFire(ctx, f1.ID)
	require.NoError(t, err)
	assert.Equal(t, store.TriggerFireStatusFiring, loaded1.Status)
	assert.Equal(t, 2, loaded1.Attempts)

	// ReturnFiringTriggerFireToWaiting returns f1 firing -> waiting when still offline.
	returned, err := cs.ReturnFiringTriggerFireToWaiting(ctx, f1.ID, "target not running")
	require.NoError(t, err)
	assert.True(t, returned)
	loaded1, err = cs.GetTriggerFire(ctx, f1.ID)
	require.NoError(t, err)
	assert.Equal(t, store.TriggerFireStatusWaiting, loaded1.Status)
	assert.Equal(t, 2, loaded1.Attempts)

	// Claim again for attempt 3 and finish from firing via FinishTriggerFire.
	claimed, err = cs.ClaimWaitingTriggerFire(ctx, f1.ID, 3, retryLease)
	require.NoError(t, err)
	assert.True(t, claimed)
	finAt := base.Add(5 * time.Minute)
	ok, err := cs.FinishTriggerFire(ctx, f1.ID, store.TriggerFireStatusFired, "msg-1", agentA, "", finAt)
	require.NoError(t, err)
	assert.True(t, ok)

	// Second FinishWaitingTriggerFire on f1 is a CAS no-op.
	ok2, err := cs.FinishWaitingTriggerFire(ctx, f1.ID, store.TriggerFireStatusExpired, "", agentA, "offline queue expired", finAt, 4)
	require.NoError(t, err)
	assert.False(t, ok2)

	// FinishWaitingTriggerFire transitions f2 -> expired.
	ok, err = cs.FinishWaitingTriggerFire(ctx, f2.ID, store.TriggerFireStatusExpired, "", agentA, "offline queue expired", finAt, 2)
	require.NoError(t, err)
	assert.True(t, ok)

	// Expired counts as an error on the trigger.
	require.NoError(t, cs.RecordTriggerFire(ctx, trID, finAt, store.TriggerFireStatusExpired, "offline queue expired"))
	loadedTr, err := cs.GetTrigger(ctx, trID)
	require.NoError(t, err)
	assert.Equal(t, 1, loadedTr.FireCount)
	assert.Equal(t, 1, loadedTr.ErrorCount)
	assert.Equal(t, store.TriggerFireStatusExpired, loadedTr.LastFireStatus)
	assert.Equal(t, "offline queue expired", loadedTr.LastFireError)

	// PurgeOldTriggerHistory deletes old terminal fires (f1, f2) but preserves waiting (f3) and firing (f4).
	_, firesPurged, err := cs.PurgeOldTriggerHistory(ctx, base.Add(24*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 2, firesPurged)

	_, err = cs.GetTriggerFire(ctx, f3.ID)
	require.NoError(t, err, "waiting fire must not be purged")
	_, err = cs.GetTriggerFire(ctx, f4.ID)
	require.NoError(t, err, "firing fire must not be purged")

	// A retry-claimed waiting row (f3) whose replica crashes before finishing is
	// reaped by ReapExpiredTriggerFires once lease_until elapses, without re-delivery.
	crashLease := base.Add(15 * time.Minute)
	claimed, err = cs.ClaimWaitingTriggerFire(ctx, f3.ID, 2, crashLease)
	require.NoError(t, err)
	assert.True(t, claimed)
	reaped, err := cs.ReapExpiredTriggerFires(ctx, crashLease.Add(time.Second), 10)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, reaped, 1)
	loaded3, err := cs.GetTriggerFire(ctx, f3.ID)
	require.NoError(t, err)
	assert.Equal(t, store.TriggerFireStatusFailed, loaded3.Status)
	assert.Equal(t, "hub restarted during fire", loaded3.Error)
	assert.Equal(t, 2, loaded3.Attempts)
}

func TestTriggerOfflineQueueStore_ConcurrentCapEnforcement(t *testing.T) {
	ctx := context.Background()
	cs := newTestCompositeStore(t)
	t.Logf("enttest.Active()=%v (postgres=%v)", enttest.Active(), enttest.Active())

	projID := uuid.NewString()
	trID := uuid.NewString()
	agentID := uuid.NewString()
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

	const (
		workers = 8
		capVal  = 3
	)
	fireIDs := make([]string, workers)
	for i := 0; i < workers; i++ {
		f := &store.TriggerFire{
			ID:         uuid.NewString(),
			ProjectID:  projID,
			TriggerID:  trID,
			EventID:    uuid.NewString(),
			Subject:    "s",
			ActionType: store.TriggerActionMessage,
			LeaseUntil: now.Add(10 * time.Minute),
			StartedAt:  now,
			CreatedAt:  now,
		}
		claimed, _, err := cs.ClaimTriggerFire(ctx, f)
		require.NoError(t, err)
		require.True(t, claimed)
		fireIDs[i] = f.ID
	}

	start := make(chan struct{})
	var (
		wg       sync.WaitGroup
		parkedN  atomic.Int32
		cappedN  atomic.Int32
		errSlice = make(chan error, workers)
	)
	for _, id := range fireIDs {
		wg.Add(1)
		go func(fid string) {
			defer wg.Done()
			<-start
			p, capHit, err := cs.ParkTriggerFireWaiting(ctx, fid, projID, agentID, "target not running", now.Add(time.Hour), capVal, 1000)
			if err != nil {
				errSlice <- err
				return
			}
			if p {
				parkedN.Add(1)
			}
			if capHit {
				cappedN.Add(1)
			}
		}(id)
	}
	close(start)
	wg.Wait()
	close(errSlice)
	for err := range errSlice {
		require.NoError(t, err)
	}
	assert.Equal(t, int32(capVal), parkedN.Load(), "exact per-target cap must be enforced under concurrency")
	assert.Equal(t, int32(workers-capVal), cappedN.Load(), "excess fires must hit cap")
}
