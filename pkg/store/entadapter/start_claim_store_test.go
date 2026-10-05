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

// Start-claim store tests. Named TestStartClaim_* so the Postgres CI job
// (make test-launch-store-postgres) runs them against Postgres as well as
// SQLite.
package entadapter

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testClaimTTL = 90 * time.Second

var testHolds = store.StartClaimHolds{Default: 13 * time.Minute, Create: 5 * time.Minute}

// setClaimLeaseUntil backdates or extends a claim's lease directly
// (white-box setup), so lease expiry needs no real wait.
func setClaimLeaseUntil(t *testing.T, ctx context.Context, s *AgentStore, agentID string, at time.Time) {
	t.Helper()
	uid, err := parseUUID(agentID)
	require.NoError(t, err)
	_, err = s.client.Agent.UpdateOneID(uid).SetStartClaimLeaseUntil(at).Save(ctx)
	require.NoError(t, err)
}

func newClaimAgent(t *testing.T, ctx context.Context, s *AgentStore, projectID, slug string) *store.Agent {
	t.Helper()
	a := makeAgent(projectID, slug)
	a.Phase = "stopped"
	require.NoError(t, s.CreateAgent(ctx, a))
	return a
}

func TestStartClaim_ClaimWritesIntentAndDoesNotBumpVersion(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := newClaimAgent(t, ctx, s, projectID, "sc-claim")
	stopAt, err := s.SetRunIntent(ctx, a.ID, store.RunIntentStopped)
	require.NoError(t, err)
	before, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)

	c, err := s.ClaimAgentStart(ctx, a.ID, "hub-1", store.StartClaimUser, "default", testClaimTTL)
	require.NoError(t, err)
	assert.NotEmpty(t, c.ID)
	assert.True(t, c.RunIntentAt.After(stopAt), "claim intent time %v must follow %v", c.RunIntentAt, stopAt)
	assert.Equal(t, testClaimTTL, c.LeaseUntil.Sub(c.At))

	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, c.ID, got.StartClaimID)
	assert.Equal(t, store.StartClaimUser, got.StartClaimKind)
	assert.Equal(t, store.StartClaimLive, got.StartClaimState)
	assert.Equal(t, "hub-1", got.StartClaimOwner)
	assert.Equal(t, "default", got.StartClaimTarget)
	require.NotNil(t, got.StartClaimAt)
	assert.True(t, got.StartClaimAt.Equal(c.At))
	assert.True(t, got.RunIntentMatches(store.RunIntentRunning, c.RunIntentAt))
	assert.Equal(t, before.StateVersion, got.StateVersion, "a claim must not bump state_version")

	held, err := s.RenewAgentStart(ctx, a.ID, c.ID, "hub-1", testClaimTTL)
	require.NoError(t, err)
	assert.True(t, held)
	held, err = s.ReleaseAgentStart(ctx, a.ID, c.ID, "hub-1")
	require.NoError(t, err)
	assert.True(t, held)
	got, err = s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Empty(t, got.StartClaimID)
	assert.Empty(t, got.StartClaimState)
	assert.Nil(t, got.StartClaimLeaseUntil)
	assert.Equal(t, before.StateVersion, got.StateVersion, "renew and release must not bump state_version")
	assert.Equal(t, store.RunIntentRunning, got.RunIntent, "release leaves run intent as the claim wrote it")
}

func TestStartClaim_HeldClaimRefusesSecondClaim(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := newClaimAgent(t, ctx, s, projectID, "sc-held")

	c, err := s.ClaimAgentStart(ctx, a.ID, "hub-1", store.StartClaimRecovery, "", testClaimTTL)
	require.NoError(t, err)
	_, err = s.ClaimAgentStart(ctx, a.ID, "hub-2", store.StartClaimUser, "", testClaimTTL)
	var held *store.ClaimHeldError
	require.ErrorAs(t, err, &held)
	assert.Equal(t, c.ID, held.ClaimID)
	assert.Equal(t, store.StartClaimRecovery, held.Kind)
	assert.Equal(t, store.StartClaimLive, held.State)
	assert.True(t, held.Since.Equal(c.At))

	// An expired lease is not taken over directly: it stays held until the
	// reaper demotes and releases it.
	setClaimLeaseUntil(t, ctx, s, a.ID, time.Now().Add(-time.Hour))
	_, err = s.ClaimAgentStart(ctx, a.ID, "hub-2", store.StartClaimUser, "", testClaimTTL)
	require.ErrorAs(t, err, &held)
}

func TestStartClaim_ConcurrentClaimsOneWins(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := newClaimAgent(t, ctx, s, projectID, "sc-race")

	const n = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins, heldErrs := 0, 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.ClaimAgentStart(ctx, a.ID, "hub", store.StartClaimUser, "", testClaimTTL)
			var held *store.ClaimHeldError
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
			case errors.As(err, &held):
				heldErrs++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, 1, wins)
	assert.Equal(t, n-1, heldErrs)
}

func TestStartClaim_PredicateMismatch(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	cases := []struct {
		name  string
		setup func(a *store.Agent)
		ok    bool
	}{
		{"reincarnation pending", func(a *store.Agent) { a.ReincarnationState = store.ReincarnationStatePending }, false},
		{"reincarnation failed", func(a *store.Agent) { a.ReincarnationState = store.ReincarnationStateFailed }, true},
		{"soft deleted", func(a *store.Agent) { a.DeletedAt = time.Now() }, false},
	}
	wantErr := map[string]error{
		"reincarnation pending": store.ErrClaimPredicate,
		"soft deleted":          store.ErrDeleteInProgress,
	}
	// CreateAgent may not persist every field above; write them back whole.
	persist := func(t *testing.T, a *store.Agent) {
		t.Helper()
		cur, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		cur.ReincarnationState = a.ReincarnationState
		cur.DeletedAt = a.DeletedAt
		require.NoError(t, s.UpdateAgent(ctx, cur))
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := makeAgent(projectID, "sc-pred-"+string(rune('a'+i)))
			require.NoError(t, s.CreateAgent(ctx, a))
			tc.setup(a)
			persist(t, a)
			_, err := s.ClaimAgentStart(ctx, a.ID, "hub", store.StartClaimUser, "", testClaimTTL)
			if tc.ok {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, wantErr[tc.name])
			got, err := s.GetAgent(ctx, a.ID)
			require.NoError(t, err)
			assert.Empty(t, got.StartClaimID)
			assert.Equal(t, store.RunIntent(""), got.RunIntent, "a refused claim writes nothing")
		})
	}

	t.Run("being deleted", func(t *testing.T) {
		a := newClaimAgent(t, ctx, s, projectID, "sc-pred-del")
		deleting := store.DeletionStateDeleting
		lease := time.Now().Add(time.Hour)
		n, err := s.UpdateAgentDeletion(ctx, a.ID, store.DeletionPredicate{}, store.DeletionFields{State: &deleting, LeaseAt: &lease})
		require.NoError(t, err)
		require.Equal(t, 1, n)
		_, err = s.ClaimAgentStart(ctx, a.ID, "hub", store.StartClaimUser, "", testClaimTTL)
		require.ErrorIs(t, err, store.ErrDeleteInProgress, "refused as the running-intent write is")
	})

	t.Run("missing agent", func(t *testing.T) {
		_, err := s.ClaimAgentStart(ctx, "00000000-0000-0000-0000-000000000099", "hub", store.StartClaimUser, "", testClaimTTL)
		require.ErrorIs(t, err, store.ErrNotFound)
	})

	t.Run("invalid input", func(t *testing.T) {
		a := newClaimAgent(t, ctx, s, projectID, "sc-pred-inv")
		_, err := s.ClaimAgentStart(ctx, a.ID, "hub", store.StartClaimStop, "", testClaimTTL)
		require.ErrorIs(t, err, store.ErrInvalidInput)
		_, err = s.ClaimAgentStart(ctx, a.ID, "hub", store.StartClaimUser, "", 0)
		require.ErrorIs(t, err, store.ErrInvalidInput)
		_, err = s.ClaimAgentStart(ctx, a.ID, "", store.StartClaimUser, "", testClaimTTL)
		require.ErrorIs(t, err, store.ErrInvalidInput)
	})
}

func TestStartClaim_RenewPredicate(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := newClaimAgent(t, ctx, s, projectID, "sc-renew")
	c, err := s.ClaimAgentStart(ctx, a.ID, "hub-1", store.StartClaimUser, "", testClaimTTL)
	require.NoError(t, err)

	held, err := s.RenewAgentStart(ctx, a.ID, c.ID, "hub-2", testClaimTTL)
	require.NoError(t, err)
	assert.False(t, held, "another owner cannot renew")
	held, err = s.RenewAgentStart(ctx, a.ID, "other-claim", "hub-1", testClaimTTL)
	require.NoError(t, err)
	assert.False(t, held, "another claim ID cannot renew")

	// An expired lease cannot be renewed: zero rows means lost.
	setClaimLeaseUntil(t, ctx, s, a.ID, time.Now().Add(-time.Second))
	held, err = s.RenewAgentStart(ctx, a.ID, c.ID, "hub-1", testClaimTTL)
	require.NoError(t, err)
	assert.False(t, held, "an expired lease cannot be renewed")
	held, err = s.MarkStartUnconfirmed(ctx, a.ID, c.ID, "hub-1", testHolds.Default)
	require.NoError(t, err)
	assert.False(t, held, "an expired lease cannot be marked unconfirmed by its holder")
	held, err = s.ReleaseAgentStart(ctx, a.ID, c.ID, "hub-1")
	require.NoError(t, err)
	assert.False(t, held, "an expired lease cannot be released by its holder")

	held, err = s.RenewAgentStart(ctx, "00000000-0000-0000-0000-000000000099", c.ID, "hub-1", testClaimTTL)
	require.NoError(t, err)
	assert.False(t, held, "a missing agent means the claim is lost")
}

func TestStartClaim_ReaperDemotesDuringStalledHolder(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := newClaimAgent(t, ctx, s, projectID, "sc-demote")
	c, err := s.ClaimAgentStart(ctx, a.ID, "hub-1", store.StartClaimRecovery, "", testClaimTTL)
	require.NoError(t, err)

	demoted, err := s.DemoteExpiredStartClaim(ctx, a.ID, c.ID, testHolds)
	require.NoError(t, err)
	assert.False(t, demoted, "an unexpired lease is not demoted")

	setClaimLeaseUntil(t, ctx, s, a.ID, time.Now().Add(-time.Second))
	demoted, err = s.DemoteExpiredStartClaim(ctx, a.ID, c.ID, testHolds)
	require.NoError(t, err)
	assert.True(t, demoted)

	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, store.StartClaimUnconfirmed, got.StartClaimState)
	require.NotNil(t, got.StartClaimUnconfirmedAt)
	require.NotNil(t, got.StartClaimHoldUntil)
	assert.Equal(t, testHolds.Default, got.StartClaimHoldUntil.Sub(*got.StartClaimUnconfirmedAt))

	// The stalled holder's renew now finds zero rows: lost.
	held, err := s.RenewAgentStart(ctx, a.ID, c.ID, "hub-1", testClaimTTL)
	require.NoError(t, err)
	assert.False(t, held)
	held, err = s.ReleaseAgentStart(ctx, a.ID, c.ID, "hub-1")
	require.NoError(t, err)
	assert.False(t, held, "a holder that lost its claim writes no outcome")

	// Demoting again is a no-op; an unconfirmed claim still blocks starts.
	demoted, err = s.DemoteExpiredStartClaim(ctx, a.ID, c.ID, testHolds)
	require.NoError(t, err)
	assert.False(t, demoted)
	_, err = s.ClaimAgentStart(ctx, a.ID, "hub-2", store.StartClaimUser, "", testClaimTTL)
	var heldErr *store.ClaimHeldError
	require.ErrorAs(t, err, &heldErr)
	assert.Equal(t, store.StartClaimUnconfirmed, heldErr.State)

	released, err := s.ReleaseUnconfirmedStart(ctx, a.ID, c.ID)
	require.NoError(t, err)
	assert.True(t, released)
	_, err = s.ClaimAgentStart(ctx, a.ID, "hub-2", store.StartClaimUser, "", testClaimTTL)
	require.NoError(t, err)
}

func TestStartClaim_CreateKindUsesCreateHold(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := newClaimAgent(t, ctx, s, projectID, "sc-create-hold")
	c, err := s.ClaimAgentStart(ctx, a.ID, "hub-1", store.StartClaimCreate, "default", testClaimTTL)
	require.NoError(t, err)
	setClaimLeaseUntil(t, ctx, s, a.ID, time.Now().Add(-time.Second))
	demoted, err := s.DemoteExpiredStartClaim(ctx, a.ID, c.ID, testHolds)
	require.NoError(t, err)
	require.True(t, demoted)
	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	require.NotNil(t, got.StartClaimHoldUntil)
	assert.Equal(t, testHolds.Create, got.StartClaimHoldUntil.Sub(*got.StartClaimUnconfirmedAt))
	exp, ok := testHolds.HoldExpiry(got)
	require.True(t, ok)
	assert.True(t, exp.Equal(*got.StartClaimHoldUntil))
}

func TestStartClaim_MarkUnconfirmed(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := newClaimAgent(t, ctx, s, projectID, "sc-unconf")
	c, err := s.ClaimAgentStart(ctx, a.ID, "hub-1", store.StartClaimUser, "", testClaimTTL)
	require.NoError(t, err)

	held, err := s.MarkStartUnconfirmed(ctx, a.ID, c.ID, "hub-1", time.Minute)
	require.NoError(t, err)
	assert.True(t, held)
	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, store.StartClaimUnconfirmed, got.StartClaimState)
	require.NotNil(t, got.StartClaimHoldUntil)
	assert.Equal(t, time.Minute, got.StartClaimHoldUntil.Sub(*got.StartClaimUnconfirmedAt))

	held, err = s.RenewAgentStart(ctx, a.ID, c.ID, "hub-1", testClaimTTL)
	require.NoError(t, err)
	assert.False(t, held, "an unconfirmed claim is no longer renewable")
	released, err := s.ReleaseUnconfirmedStart(ctx, a.ID, "wrong")
	require.NoError(t, err)
	assert.False(t, released)
}

func TestStartClaim_LaunchActiveBlocksReaperWrites(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := createLaunchableAgent(t, ctx, s, projectID, "sc-launch-active")
	c, err := s.ClaimAgentStart(ctx, a.ID, "hub-1", store.StartClaimCreate, "", testClaimTTL)
	require.NoError(t, err)
	_, err = s.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, time.Hour)
	require.NoError(t, err)

	setClaimLeaseUntil(t, ctx, s, a.ID, time.Now().Add(-time.Second))
	demoted, err := s.DemoteExpiredStartClaim(ctx, a.ID, c.ID, testHolds)
	require.NoError(t, err)
	assert.False(t, demoted, "an active launch owns liveness: no demotion")
	n, err := s.DemoteOwnerStartClaims(ctx, "hub-", "", testHolds)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
	changed, err := s.SettleEndedLaunchClaim(ctx, a.ID, c.ID)
	require.NoError(t, err)
	assert.False(t, changed)

	// Even an unconfirmed claim is not released while the launch is active.
	held, err := s.MarkStartUnconfirmed(ctx, a.ID, c.ID, "hub-1", time.Minute)
	require.NoError(t, err)
	assert.False(t, held, "the expired lease cannot be marked by its holder")
	uid, err := parseUUID(a.ID)
	require.NoError(t, err)
	_, err = s.client.Agent.UpdateOneID(uid).SetStartClaimState(string(store.StartClaimUnconfirmed)).Save(ctx)
	require.NoError(t, err)
	released, err := s.ReleaseUnconfirmedStart(ctx, a.ID, c.ID)
	require.NoError(t, err)
	assert.False(t, released, "an active launch owns liveness: no release")
}

func TestStartClaim_ReleaseSuperseded(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	t.Run("stop after claim releases it", func(t *testing.T) {
		for _, unconfirmed := range []bool{false, true} {
			a := newClaimAgent(t, ctx, s, projectID, map[bool]string{false: "sc-sup-live", true: "sc-sup-unc"}[unconfirmed])
			c, err := s.ClaimAgentStart(ctx, a.ID, "hub-1", store.StartClaimRecovery, "", testClaimTTL)
			require.NoError(t, err)
			if unconfirmed {
				held, err := s.MarkStartUnconfirmed(ctx, a.ID, c.ID, "hub-1", time.Minute)
				require.NoError(t, err)
				require.True(t, held)
			}
			stopAt, err := s.SetRunIntent(ctx, a.ID, store.RunIntentStopped)
			require.NoError(t, err)
			released, err := s.ReleaseSupersededStart(ctx, a.ID, c.ID, stopAt.Add(-time.Microsecond))
			require.NoError(t, err)
			assert.False(t, released, "the CAS is on the stop's exact intent time")
			released, err = s.ReleaseSupersededStart(ctx, a.ID, c.ID, stopAt)
			require.NoError(t, err)
			assert.True(t, released)
			held, err := s.RenewAgentStart(ctx, a.ID, c.ID, "hub-1", testClaimTTL)
			require.NoError(t, err)
			assert.False(t, held, "the superseded holder sees zero rows")
		}
	})

	t.Run("older stop does not release a newer claim", func(t *testing.T) {
		a := newClaimAgent(t, ctx, s, projectID, "sc-sup-old")
		stopAt, err := s.SetRunIntent(ctx, a.ID, store.RunIntentStopped)
		require.NoError(t, err)
		c, err := s.ClaimAgentStart(ctx, a.ID, "hub-1", store.StartClaimUser, "", testClaimTTL)
		require.NoError(t, err)
		released, err := s.ReleaseSupersededStart(ctx, a.ID, c.ID, stopAt)
		require.NoError(t, err)
		assert.False(t, released)
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, c.ID, got.StartClaimID)
	})
}

func TestStartClaim_StopKind(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := newClaimAgent(t, ctx, s, projectID, "sc-stop")
	stopAt, err := s.SetRunIntent(ctx, a.ID, store.RunIntentStopped)
	require.NoError(t, err)

	_, err = s.ClaimAgentStop(ctx, a.ID, "hub-1", stopAt.Add(-time.Microsecond), testClaimTTL)
	require.ErrorIs(t, err, store.ErrClaimPredicate, "a stop claim needs the exact queued intent time")

	c, err := s.ClaimAgentStop(ctx, a.ID, "hub-1", stopAt, testClaimTTL)
	require.NoError(t, err)
	assert.Equal(t, store.StartClaimStop, c.Kind)
	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.True(t, got.RunIntentMatches(store.RunIntentStopped, stopAt), "a stop claim never changes intent")

	_, err = s.ClaimAgentStart(ctx, a.ID, "hub-2", store.StartClaimUser, "", testClaimTTL)
	var held *store.ClaimHeldError
	require.ErrorAs(t, err, &held)
	assert.Equal(t, store.StartClaimStop, held.Kind)

	// The stop-kind claim follows the same lease rules.
	setClaimLeaseUntil(t, ctx, s, a.ID, time.Now().Add(-time.Second))
	demoted, err := s.DemoteExpiredStartClaim(ctx, a.ID, c.ID, testHolds)
	require.NoError(t, err)
	require.True(t, demoted)
	got, err = s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, testHolds.Create, got.StartClaimHoldUntil.Sub(*got.StartClaimUnconfirmedAt))

	// A start then supersedes nothing until the claim is released; once
	// released, a start writes newer intent and a stop claim on the old
	// intent time no longer applies.
	released, err := s.ReleaseUnconfirmedStart(ctx, a.ID, c.ID)
	require.NoError(t, err)
	require.True(t, released)
	_, err = s.ClaimAgentStart(ctx, a.ID, "hub-2", store.StartClaimUser, "", testClaimTTL)
	require.NoError(t, err)
	_, err = s.ClaimAgentStop(ctx, a.ID, "hub-1", stopAt, testClaimTTL)
	require.Error(t, err)
}

func TestStartClaim_DemoteOwnerPrefix(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	mine := newClaimAgent(t, ctx, s, projectID, "sc-owner-mine")
	other := newClaimAgent(t, ctx, s, projectID, "sc-owner-other")
	_, err := s.ClaimAgentStart(ctx, mine.ID, "hub-pod-a-1234", store.StartClaimUser, "", testClaimTTL)
	require.NoError(t, err)
	_, err = s.ClaimAgentStart(ctx, other.ID, "hub-pod-b-5678", store.StartClaimUser, "", testClaimTTL)
	require.NoError(t, err)

	self := newClaimAgent(t, ctx, s, projectID, "sc-owner-self")
	_, err = s.ClaimAgentStart(ctx, self.ID, "hub-pod-a-9999", store.StartClaimUser, "", testClaimTTL)
	require.NoError(t, err)

	n, err := s.DemoteOwnerStartClaims(ctx, "hub-pod-a-", "hub-pod-a-9999", testHolds)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	got0, err := s.GetAgent(ctx, self.ID)
	require.NoError(t, err)
	assert.Equal(t, store.StartClaimLive, got0.StartClaimState, "this process's own claims are not demoted")
	got, err := s.GetAgent(ctx, mine.ID)
	require.NoError(t, err)
	assert.Equal(t, store.StartClaimUnconfirmed, got.StartClaimState)
	got, err = s.GetAgent(ctx, other.ID)
	require.NoError(t, err)
	assert.Equal(t, store.StartClaimLive, got.StartClaimState)
	_, err = s.DemoteOwnerStartClaims(ctx, "", "", testHolds)
	require.ErrorIs(t, err, store.ErrInvalidInput)
}

func TestStartClaim_UpdateAgentNeverTouchesClaim(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	a := makeAgent(projectID, "sc-upd")
	a.StartClaimID = "from-create"
	a.StartClaimState = store.StartClaimLive
	require.NoError(t, s.CreateAgent(ctx, a))
	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Empty(t, got.StartClaimID, "CreateAgent must not write start claim columns")

	c, err := s.ClaimAgentStart(ctx, a.ID, "hub-1", store.StartClaimUser, "t", testClaimTTL)
	require.NoError(t, err)

	// A stale struct with no claim, written back whole, leaves the claim.
	stale := *got
	stale.Message = "changed"
	require.NoError(t, s.UpdateAgent(ctx, &stale))
	// A struct carrying a different claim also changes nothing.
	got2, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	got2.StartClaimID = "other"
	got2.StartClaimState = store.StartClaimUnconfirmed
	got2.StartClaimOwner = "other"
	require.NoError(t, s.UpdateAgent(ctx, got2))
	phase := "running"
	require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{Phase: phase, ClearExit: true}))

	final, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, c.ID, final.StartClaimID)
	assert.Equal(t, store.StartClaimLive, final.StartClaimState)
	assert.Equal(t, "hub-1", final.StartClaimOwner)
	assert.Equal(t, "changed", final.Message)
}

func TestStartClaim_LaunchEndSettlement(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	for i, reason := range []string{
		store.LaunchEndReasonSucceeded, store.LaunchEndReasonRunningObserved,
		store.LaunchEndReasonFailed, store.LaunchEndReasonNotLaunched,
		store.LaunchEndReasonTimedOut, store.LaunchEndReasonLost, store.LaunchEndReasonSuperseded,
	} {
		t.Run(reason, func(t *testing.T) {
			a := createLaunchableAgent(t, ctx, s, projectID, "sc-settle-"+string(rune('a'+i)))
			c, err := s.ClaimAgentStart(ctx, a.ID, "hub-1", store.StartClaimCreate, "", testClaimTTL)
			require.NoError(t, err)
			launchID, err := s.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, time.Hour)
			require.NoError(t, err)
			require.NoError(t, s.EndLaunch(ctx, a.ID, launchID, reason))

			got, err := s.GetAgent(ctx, a.ID)
			require.NoError(t, err)
			release, demote := store.LaunchEndClaimSettlement(reason)
			switch {
			case release:
				assert.Empty(t, got.StartClaimID, "end reason %s releases", reason)
			case demote:
				assert.Equal(t, c.ID, got.StartClaimID)
				assert.Equal(t, store.StartClaimUnconfirmed, got.StartClaimState, "end reason %s demotes", reason)
				require.NotNil(t, got.StartClaimUnconfirmedAt)
				assert.Nil(t, got.StartClaimHoldUntil)
				exp, ok := testHolds.HoldExpiry(got)
				require.True(t, ok)
				assert.Equal(t, testHolds.Create, exp.Sub(*got.StartClaimUnconfirmedAt))
			default:
				t.Fatalf("reason %s settles nothing", reason)
			}
		})
	}

	t.Run("non-create claim untouched", func(t *testing.T) {
		a := createLaunchableAgent(t, ctx, s, projectID, "sc-settle-user")
		c, err := s.ClaimAgentStart(ctx, a.ID, "hub-1", store.StartClaimUser, "", testClaimTTL)
		require.NoError(t, err)
		launchID, err := s.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, time.Hour)
		require.NoError(t, err)
		linked, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Empty(t, linked.StartClaimLaunchID, "only a create claim is linked to a launch")
		require.NoError(t, s.EndLaunch(ctx, a.ID, launchID, store.LaunchEndReasonFailed))
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, c.ID, got.StartClaimID)
		assert.Equal(t, store.StartClaimLive, got.StartClaimState)
	})

	t.Run("reaper deadline demotes in the same write", func(t *testing.T) {
		a := createLaunchableAgent(t, ctx, s, projectID, "sc-settle-reap")
		c, err := s.ClaimAgentStart(ctx, a.ID, "hub-1", store.StartClaimCreate, "", testClaimTTL)
		require.NoError(t, err)
		_, err = s.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, time.Hour)
		require.NoError(t, err)
		setAgentLaunchDeadline(t, ctx, s, a.ID, time.Now().Add(-time.Second))
		result, err := s.RunLaunchReaperTick(ctx, testReaperParams)
		require.NoError(t, err)
		require.Len(t, result.Reaped, 1)
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, c.ID, got.StartClaimID)
		assert.Equal(t, store.StartClaimUnconfirmed, got.StartClaimState)
	})

	t.Run("succeeded report releases", func(t *testing.T) {
		a := createLaunchableAgent(t, ctx, s, projectID, "sc-settle-report")
		_, err := s.ClaimAgentStart(ctx, a.ID, "hub-1", store.StartClaimCreate, "", testClaimTTL)
		require.NoError(t, err)
		launchID, err := s.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, time.Hour)
		require.NoError(t, err)
		_, _, err = s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{LaunchID: launchID, State: store.LaunchReportStateSucceeded})
		require.NoError(t, err)
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, store.LaunchEndReasonSucceeded, got.LaunchEndReason)
		assert.Empty(t, got.StartClaimID)
	})

	t.Run("backstop settles a linked launch that ended without settling", func(t *testing.T) {
		a := createLaunchableAgent(t, ctx, s, projectID, "sc-settle-backstop")
		c, err := s.ClaimAgentStart(ctx, a.ID, "hub-1", store.StartClaimCreate, "", testClaimTTL)
		require.NoError(t, err)
		_, err = s.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, time.Hour)
		require.NoError(t, err)
		// A running status write ends the launch as running_observed
		// without settling the claim; the backstop does.
		require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{Phase: "running"}))
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		require.Equal(t, store.LaunchEndReasonRunningObserved, got.LaunchEndReason)
		require.Equal(t, c.ID, got.StartClaimID)
		changed, err := s.SettleEndedLaunchClaim(ctx, a.ID, c.ID)
		require.NoError(t, err)
		assert.True(t, changed)
		got, err = s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Empty(t, got.StartClaimID)
	})

	t.Run("claim taken after the launch ended is untouched", func(t *testing.T) {
		a := createLaunchableAgent(t, ctx, s, projectID, "sc-settle-after")
		launchID, err := s.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, time.Hour)
		require.NoError(t, err)
		require.NoError(t, s.EndLaunch(ctx, a.ID, launchID, store.LaunchEndReasonLost))
		c, err := s.ClaimAgentStart(ctx, a.ID, "hub-1", store.StartClaimCreate, "", testClaimTTL)
		require.NoError(t, err)
		changed, err := s.SettleEndedLaunchClaim(ctx, a.ID, c.ID)
		require.NoError(t, err)
		assert.False(t, changed, "a claim not linked to the ended launch is not settled")
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, store.StartClaimLive, got.StartClaimState)
	})

	t.Run("a newer launch for the same start relinks a live claim", func(t *testing.T) {
		a := createLaunchableAgent(t, ctx, s, projectID, "sc-settle-relink")
		c, err := s.ClaimAgentStart(ctx, a.ID, "hub-1", store.StartClaimCreate, "", testClaimTTL)
		require.NoError(t, err)
		first, err := s.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, time.Hour)
		require.NoError(t, err)
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		require.Equal(t, first, got.StartClaimLaunchID)
		second, err := s.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, time.Hour)
		require.NoError(t, err)
		got, err = s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, second, got.StartClaimLaunchID, "a live claim follows the launch of its start")
		assert.Equal(t, store.StartClaimLive, got.StartClaimState)
		held, err := s.RenewAgentStart(ctx, a.ID, c.ID, "hub-1", testClaimTTL)
		require.NoError(t, err)
		assert.True(t, held, "the holder keeps its claim")
		require.NoError(t, s.EndLaunch(ctx, a.ID, second, store.LaunchEndReasonSucceeded))
		got, err = s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Empty(t, got.StartClaimID, "the second launch's end settles the claim")
	})

	t.Run("a stop claim is not linked", func(t *testing.T) {
		a := createLaunchableAgent(t, ctx, s, projectID, "sc-settle-stopkind")
		at, err := s.SetRunIntent(ctx, a.ID, store.RunIntentStopped)
		require.NoError(t, err)
		_, err = s.ClaimAgentStop(ctx, a.ID, "hub-1", at, testClaimTTL)
		require.NoError(t, err)
		_, err = s.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, time.Hour)
		require.NoError(t, err)
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Empty(t, got.StartClaimLaunchID)
	})

	t.Run("an unconfirmed claim is not relinked and the newer launch does not settle it", func(t *testing.T) {
		a := createLaunchableAgent(t, ctx, s, projectID, "sc-settle-unconf")
		c, err := s.ClaimAgentStart(ctx, a.ID, "hub-1", store.StartClaimCreate, "", testClaimTTL)
		require.NoError(t, err)
		first, err := s.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, time.Hour)
		require.NoError(t, err)
		held, err := s.MarkStartUnconfirmed(ctx, a.ID, c.ID, "hub-1", time.Minute)
		require.NoError(t, err)
		require.True(t, held)
		second, err := s.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, time.Hour)
		require.NoError(t, err)
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, first, got.StartClaimLaunchID, "an unconfirmed claim keeps its link")
		require.NoError(t, s.EndLaunch(ctx, a.ID, second, store.LaunchEndReasonFailed))
		got, err = s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, store.StartClaimUnconfirmed, got.StartClaimState, "the newer launch's end does not settle it")
	})

	t.Run("an expired claim is not relinked and the newer launch does not settle it", func(t *testing.T) {
		a := createLaunchableAgent(t, ctx, s, projectID, "sc-settle-expired")
		c, err := s.ClaimAgentStart(ctx, a.ID, "hub-1", store.StartClaimCreate, "", testClaimTTL)
		require.NoError(t, err)
		first, err := s.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, time.Hour)
		require.NoError(t, err)
		setClaimLeaseUntil(t, ctx, s, a.ID, time.Now().Add(-time.Second))
		second, err := s.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, time.Hour)
		require.NoError(t, err)
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, first, got.StartClaimLaunchID)
		require.NoError(t, s.EndLaunch(ctx, a.ID, second, store.LaunchEndReasonFailed))
		got, err = s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, c.ID, got.StartClaimID, "a launch the claim is not linked to does not settle it")
		assert.Equal(t, store.StartClaimLive, got.StartClaimState)
		changed, err := s.SettleEndedLaunchClaim(ctx, a.ID, c.ID)
		require.NoError(t, err)
		assert.False(t, changed)
	})

	t.Run("a stale end reason does not settle during a new launch", func(t *testing.T) {
		a := createLaunchableAgent(t, ctx, s, projectID, "sc-settle-stale")
		first, err := s.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, time.Hour)
		require.NoError(t, err)
		require.NoError(t, s.EndLaunch(ctx, a.ID, first, store.LaunchEndReasonFailed))
		c, err := s.ClaimAgentStart(ctx, a.ID, "hub-1", store.StartClaimCreate, "", testClaimTTL)
		require.NoError(t, err)
		_, err = s.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, time.Hour)
		require.NoError(t, err)
		changed, err := s.SettleEndedLaunchClaim(ctx, a.ID, c.ID)
		require.NoError(t, err)
		assert.False(t, changed, "an active launch is never settled from the previous launch's end reason")
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, store.StartClaimLive, got.StartClaimState)
	})
}

func TestStartClaim_ListAgentsWithStartClaim(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := newClaimAgent(t, ctx, s, projectID, "sc-list-a")
	_ = newClaimAgent(t, ctx, s, projectID, "sc-list-b")
	_, err := s.ClaimAgentStart(ctx, a.ID, "hub-1", store.StartClaimUser, "", testClaimTTL)
	require.NoError(t, err)
	list, err := s.ListAgentsWithStartClaim(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, a.ID, list[0].ID)
}

func TestStartClaim_ClaimAgentReincarnation(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := newClaimAgent(t, ctx, s, projectID, "sc-reinc")
	cur, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	at := time.Now().UTC().Truncate(time.Microsecond)

	_, err = s.ClaimAgentReincarnation(ctx, a.ID, cur.StateVersion+1, at)
	require.ErrorIs(t, err, store.ErrVersionConflict)

	c, err := s.ClaimAgentStart(ctx, a.ID, "hub-1", store.StartClaimUser, "", testClaimTTL)
	require.NoError(t, err)
	_, err = s.ClaimAgentReincarnation(ctx, a.ID, cur.StateVersion, at)
	var held *store.ClaimHeldError
	require.ErrorAs(t, err, &held, "reincarnation is refused while any start claim is held")
	_, err = s.ReleaseAgentStart(ctx, a.ID, c.ID, "hub-1")
	require.NoError(t, err)

	v, err := s.ClaimAgentReincarnation(ctx, a.ID, cur.StateVersion, at)
	require.NoError(t, err)
	assert.Equal(t, cur.StateVersion+1, v)
	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ReincarnationStatePending, got.ReincarnationState)
	assert.Equal(t, v, got.StateVersion)

	_, err = s.ClaimAgentReincarnation(ctx, a.ID, v, at)
	require.ErrorIs(t, err, store.ErrClaimPredicate, "a reincarnation in flight refuses another")
	_, err = s.ClaimAgentStart(ctx, a.ID, "hub-1", store.StartClaimUser, "", testClaimTTL)
	require.ErrorIs(t, err, store.ErrClaimPredicate, "a start claim is refused while a reincarnation is in flight")
}

func TestStartClaim_IntentStrictlyIncreasesWhenClockBehind(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := newClaimAgent(t, ctx, s, projectID, "sc-clock")
	future := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
	setStoredRunIntent(t, ctx, s, a.ID, store.RunIntentStopped, future)

	c, err := s.ClaimAgentStart(ctx, a.ID, "hub-1", store.StartClaimUser, "", testClaimTTL)
	require.NoError(t, err)
	assert.True(t, c.RunIntentAt.After(future), "claim intent %v must follow stored %v even with the clock behind", c.RunIntentAt, future)
	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.True(t, got.RunIntentMatches(store.RunIntentRunning, c.RunIntentAt))
}

func TestStartClaim_SupersededStopDoesNotReleaseStopClaim(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := newClaimAgent(t, ctx, s, projectID, "sc-sup-stopkind")
	stopAt, err := s.SetRunIntent(ctx, a.ID, store.RunIntentStopped)
	require.NoError(t, err)
	c, err := s.ClaimAgentStop(ctx, a.ID, "hub-1", stopAt, testClaimTTL)
	require.NoError(t, err)
	released, err := s.ReleaseSupersededStart(ctx, a.ID, c.ID, stopAt)
	require.NoError(t, err)
	assert.False(t, released, "a stop does not release a queued stop's own claim")
}

func TestStartClaim_WritesKeepUpdatedAndListSkipsDeleted(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := newClaimAgent(t, ctx, s, projectID, "sc-updated")
	before, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	time.Sleep(5 * time.Millisecond)
	c, err := s.ClaimAgentStart(ctx, a.ID, "hub-1", store.StartClaimUser, "", testClaimTTL)
	require.NoError(t, err)
	_, err = s.RenewAgentStart(ctx, a.ID, c.ID, "hub-1", testClaimTTL)
	require.NoError(t, err)
	_, err = s.MarkStartUnconfirmed(ctx, a.ID, c.ID, "hub-1", time.Minute)
	require.NoError(t, err)
	_, err = s.ReleaseUnconfirmedStart(ctx, a.ID, c.ID)
	require.NoError(t, err)
	after, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.True(t, after.Updated.Equal(before.Updated), "claim writes leave agents.updated unchanged (%v -> %v)", before.Updated, after.Updated)

	gone := newClaimAgent(t, ctx, s, projectID, "sc-updated-deleted")
	_, err = s.ClaimAgentStart(ctx, gone.ID, "hub-1", store.StartClaimUser, "", testClaimTTL)
	require.NoError(t, err)
	cur, err := s.GetAgent(ctx, gone.ID)
	require.NoError(t, err)
	cur.DeletedAt = time.Now()
	require.NoError(t, s.UpdateAgent(ctx, cur))
	list, err := s.ListAgentsWithStartClaim(ctx)
	require.NoError(t, err)
	assert.Empty(t, list, "soft-deleted agents are not listed")

	now, err := s.StoreClock(ctx)
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now(), now, time.Minute)
}

func TestStartClaim_AbandonedDeleteDoesNotRefuse(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := newClaimAgent(t, ctx, s, projectID, "sc-abandoned")
	deleting := store.DeletionStateDeleting
	expired := time.Now().Add(-time.Minute)
	n, err := s.UpdateAgentDeletion(ctx, a.ID, store.DeletionPredicate{}, store.DeletionFields{State: &deleting, LeaseAt: &expired})
	require.NoError(t, err)
	require.Equal(t, 1, n)
	_, err = s.ClaimAgentStart(ctx, a.ID, "hub", store.StartClaimUser, "", testClaimTTL)
	require.NoError(t, err, "a delete whose lease expired does not hold the row")
}
