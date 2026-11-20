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
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/observability/txdiag"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

func openTriggerSkipPostgresStore(t *testing.T, ctx context.Context, url string) *CompositeStore {
	t.Helper()
	client, err := entc.OpenPostgres(url, entc.PoolConfig{
		MaxOpenConns: 10,
		MaxIdleConns: 4,
		Diag:         txdiag.QuietConfig(),
	})
	require.NoError(t, err)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })
	require.NoError(t, cs.DB().PingContext(ctx))
	return cs
}

func distinctTriggerForSubjectHash(avoidHashes ...int32) string {
	for {
		cand := uuid.NewString()
		conflict := false
		for _, h := range avoidHashes {
			if store.StableProjectHash(cand+"\x00webhook:ep-pg/pr/101") == h ||
				store.StableProjectHash(cand+"\x00webhook:ep-pg/pr/202") == h {
				conflict = true
				break
			}
		}
		if !conflict {
			return cand
		}
	}
}

// TestClaimTriggerFireSubjectSkip_Postgres exercises the PostgreSQL
// pg_advisory_xact_lock(LockTriggerSubjectOverlap, StableProjectHash(trigger+"\x00"+subject))
// path of ClaimTriggerFireSubjectSkip under READ COMMITTED across independent
// connection pools (ptone/scion#8031, S4-1):
//  1. Holding an uncommitted claim on (trA, subjA) blocks a concurrent claim on
//     the same (trA, subjA) until commit (after which the waiter skips as
//     "subject busy"), while claims on (trA, subjB), (trB, subjA), and
//     (trB, subjB) proceed without blocking and all fire.
//  2. Concurrent multi-replica bursts on one (trigger, subject) serialize so
//     exactly one claimant fires and the rest skip, while concurrent claims on
//     different subjects or triggers in the same burst all fire.
func TestClaimTriggerFireSubjectSkip_Postgres(t *testing.T) {
	if !enttest.Active() {
		t.Skip("requires -tags integration and SCION_TEST_POSTGRES_URL")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	dsn := enttest.NewSchemaURL(t)
	csA := openTriggerSkipPostgresStore(t, ctx, dsn)
	csB := openTriggerSkipPostgresStore(t, ctx, dsn)

	projID := uuid.NewString()
	now := time.Date(2026, 11, 20, 8, 30, 0, 0, time.UTC)

	newFire := func(triggerID, subject string, at time.Time) *store.TriggerFire {
		return &store.TriggerFire{
			ID:         uuid.NewString(),
			ProjectID:  projID,
			TriggerID:  triggerID,
			EventID:    uuid.NewString(),
			Subject:    subject,
			ActionType: store.TriggerActionMessage,
			LeaseUntil: at.Add(10 * time.Minute),
			StartedAt:  at,
		}
	}

	t.Run("in_flight_lock_blocks_same_key_without_blocking_different_subject_or_trigger", func(t *testing.T) {
		const (
			subjA = "webhook:ep-pg/pr/101"
			subjB = "webhook:ep-pg/pr/202"
		)
		trA := uuid.NewString()
		hashAA := store.StableProjectHash(trA + "\x00" + subjA)
		hashAB := store.StableProjectHash(trA + "\x00" + subjB)
		require.NotEqual(t, hashAA, hashAB)
		trB := distinctTriggerForSubjectHash(hashAA, hashAB)

		// tx1 claims (trA, subjA) and stays uncommitted, holding the per-(trigger, subject)
		// advisory lock while its firing row is still invisible to READ COMMITTED readers.
		tx1, err := csA.client.Tx(ctx)
		require.NoError(t, err)
		defer func() { _ = tx1.Rollback() }()
		s1 := newTxCompositeStore(tx1)

		holdFire := newFire(trA, subjA, now)
		claimedHold, skippedHold, _, err := s1.ClaimTriggerFireSubjectSkip(ctx, holdFire, "subject busy", now)
		require.NoError(t, err)
		require.True(t, claimedHold)
		require.False(t, skippedHold)
		assert.Equal(t, store.TriggerFireStatusFiring, holdFire.Status)

		// Claims on different subject, different trigger, or both must NOT wait on tx1's lock.
		for _, tc := range []struct {
			label   string
			trigger string
			subject string
		}{
			{label: "different_subject_same_trigger", trigger: trA, subject: subjB},
			{label: "different_trigger_same_subject", trigger: trB, subject: subjA},
			{label: "different_trigger_and_subject", trigger: trB, subject: subjB},
		} {
			nonBlockCtx, cancelNB := context.WithTimeout(ctx, 5*time.Second)
			otherFire := newFire(tc.trigger, tc.subject, now)
			claimedOther, skippedOther, _, oErr := csB.ClaimTriggerFireSubjectSkip(nonBlockCtx, otherFire, "subject busy", now)
			cancelNB()
			require.NoError(t, oErr, "%s must not block on (trA, subjA)'s advisory lock", tc.label)
			require.True(t, claimedOther, tc.label)
			assert.False(t, skippedOther, "%s must fire independently", tc.label)
			assert.Equal(t, store.TriggerFireStatusFiring, otherFire.Status, tc.label)
		}

		// A second transaction claiming the SAME (trA, subjA) must block on tx1's advisory lock,
		// then observe tx1's committed firing row under READ COMMITTED and record skipped ("subject busy").
		tx2, err := csB.client.Tx(ctx)
		require.NoError(t, err)
		defer func() { _ = tx2.Rollback() }()
		s2 := newTxCompositeStore(tx2)
		pid2 := lockTestBackendPID(t, ctx, s2)

		type claimResult struct {
			claimed     bool
			skippedBusy bool
			err         error
		}
		waiterFire := newFire(trA, subjA, now.Add(time.Second))
		waiterDone := make(chan claimResult, 1)
		go func() {
			cl, sk, _, wErr := s2.ClaimTriggerFireSubjectSkip(ctx, waiterFire, "subject busy", now.Add(time.Second))
			waiterDone <- claimResult{claimed: cl, skippedBusy: sk, err: wErr}
		}()

		waitForBackendBlocked(t, csA.DB(), pid2)
		require.NoError(t, tx1.Commit())

		select {
		case res := <-waiterDone:
			require.NoError(t, res.err)
			require.True(t, res.claimed)
			assert.True(t, res.skippedBusy, "waiter must see committed firing row after lock release and skip")
		case <-time.After(10 * time.Second):
			t.Fatal("same-(trigger, subject) waiter did not unblock after tx1 committed")
		}
		require.NoError(t, tx2.Commit())

		persistedHold, err := csB.GetTriggerFire(ctx, holdFire.ID)
		require.NoError(t, err)
		assert.Equal(t, store.TriggerFireStatusFiring, persistedHold.Status)

		persistedWaiter, err := csA.GetTriggerFire(ctx, waiterFire.ID)
		require.NoError(t, err)
		assert.Equal(t, store.TriggerFireStatusSkipped, persistedWaiter.Status)
		assert.Equal(t, "subject busy", persistedWaiter.Error)
		require.NotNil(t, persistedWaiter.FinishedAt)
	})

	t.Run("concurrent_replicas_serialize_same_key_and_fire_distinct_keys", func(t *testing.T) {
		const (
			rounds            = 5
			sameKeyWorkers    = 8
			diffSubjectClaims = 4
			diffTriggerClaims = 4
		)
		stores := []*CompositeStore{csA, csB}

		for round := 0; round < rounds; round++ {
			trSame := uuid.NewString()
			subjSame := fmt.Sprintf("webhook:ep-pg/round-%d/shared", round)
			roundAt := now.Add(time.Duration(round+1) * time.Minute)

			var (
				wg            sync.WaitGroup
				sameFiring    atomic.Int32
				sameSkipped   atomic.Int32
				distinctFired atomic.Int32
			)
			totalWorkers := sameKeyWorkers + diffSubjectClaims + diffTriggerClaims
			errs := make(chan error, totalWorkers)
			start := make(chan struct{})

			for i := 0; i < sameKeyWorkers; i++ {
				st := stores[i%len(stores)]
				f := newFire(trSame, subjSame, roundAt)
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					claimed, skippedBusy, _, cErr := st.ClaimTriggerFireSubjectSkip(ctx, f, "subject busy", roundAt)
					if cErr != nil {
						errs <- cErr
						return
					}
					if !claimed {
						errs <- fmt.Errorf("same-key worker expected claimed=true")
						return
					}
					if skippedBusy {
						sameSkipped.Add(1)
					} else {
						sameFiring.Add(1)
					}
				}()
			}

			for i := 0; i < diffSubjectClaims; i++ {
				st := stores[i%len(stores)]
				f := newFire(trSame, fmt.Sprintf("webhook:ep-pg/round-%d/other-subj-%d", round, i), roundAt)
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					claimed, skippedBusy, _, cErr := st.ClaimTriggerFireSubjectSkip(ctx, f, "subject busy", roundAt)
					if cErr != nil {
						errs <- cErr
						return
					}
					if !claimed || skippedBusy || f.Status != store.TriggerFireStatusFiring {
						errs <- fmt.Errorf("different-subject claim did not fire: claimed=%v skipped=%v status=%q", claimed, skippedBusy, f.Status)
						return
					}
					distinctFired.Add(1)
				}()
			}

			for i := 0; i < diffTriggerClaims; i++ {
				st := stores[i%len(stores)]
				f := newFire(uuid.NewString(), subjSame, roundAt)
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					claimed, skippedBusy, _, cErr := st.ClaimTriggerFireSubjectSkip(ctx, f, "subject busy", roundAt)
					if cErr != nil {
						errs <- cErr
						return
					}
					if !claimed || skippedBusy || f.Status != store.TriggerFireStatusFiring {
						errs <- fmt.Errorf("different-trigger claim did not fire: claimed=%v skipped=%v status=%q", claimed, skippedBusy, f.Status)
						return
					}
					distinctFired.Add(1)
				}()
			}

			close(start)
			wg.Wait()
			close(errs)
			for err := range errs {
				require.NoError(t, err, "round %d", round)
			}

			assert.Equal(t, int32(1), sameFiring.Load(), "round %d: exactly one concurrent claimant on (trSame, subjSame) must fire", round)
			assert.Equal(t, int32(sameKeyWorkers-1), sameSkipped.Load(), "round %d: remaining concurrent claimants on (trSame, subjSame) must skip", round)
			assert.Equal(t, int32(diffSubjectClaims+diffTriggerClaims), distinctFired.Load(), "round %d: all claims on different subjects or triggers must fire", round)
		}
	})
}
