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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestTriggerSubjectClaimStore_LifecycleAndAbandonedTakeover(t *testing.T) {
	ctx := context.Background()
	cs := newTestCompositeStore(t)

	trigID := uuid.NewString()
	ev1, fire1 := uuid.NewString(), uuid.NewString()
	ev2, fire2 := uuid.NewString(), uuid.NewString()
	ev3, fire3 := uuid.NewString(), uuid.NewString()
	agent1 := uuid.NewString()
	t0 := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

	// Subject > 256 bytes rejected.
	_, _, err := cs.ClaimTriggerSubject(ctx, trigID, strings.Repeat("a", 257), ev1, fire1, t0, t0.Add(-5*time.Minute))
	require.ErrorIs(t, err, store.ErrInvalidInput)

	// 1. Fresh claim succeeds.
	c1, existing, err := cs.ClaimTriggerSubject(ctx, trigID, "pr-1", ev1, fire1, t0, t0.Add(-5*time.Minute))
	require.NoError(t, err)
	require.Nil(t, existing)
	require.NotNil(t, c1)
	assert.Equal(t, store.TriggerSubjectClaimStateActive, c1.State)
	assert.Empty(t, c1.AgentID)
	assert.Equal(t, fire1, c1.FireID)
	assert.Equal(t, ev1, c1.EventID)
	assert.Equal(t, t0, c1.ClaimedAt)

	// 2. Competing claim while younger than 5m (4m59s) is rejected and returns existing.
	tYoung := t0.Add(4*time.Minute + 59*time.Second)
	c2, existing, err := cs.ClaimTriggerSubject(ctx, trigID, "pr-1", ev2, fire2, tYoung, tYoung.Add(-5*time.Minute))
	require.NoError(t, err)
	assert.Nil(t, c2)
	require.NotNil(t, existing)
	assert.Equal(t, c1.ID, existing.ID)
	assert.Equal(t, fire1, existing.FireID)

	// 3. After 5m01s with agent_id IS NULL, abandoned claim is taken over, reusing the row.
	tOld := t0.Add(5*time.Minute + time.Second)
	c2, existing, err = cs.ClaimTriggerSubject(ctx, trigID, "pr-1", ev2, fire2, tOld, tOld.Add(-5*time.Minute))
	require.NoError(t, err)
	require.Nil(t, existing)
	require.NotNil(t, c2)
	assert.Equal(t, c1.ID, c2.ID, "row is reused for the same (trigger_id, subject)")
	assert.Equal(t, fire2, c2.FireID)
	assert.Equal(t, ev2, c2.EventID)
	assert.Equal(t, tOld, c2.ClaimedAt)

	// 4. Stale holder (fire1) cannot Bind or Release the superseded claim.
	bound, err := cs.BindTriggerSubjectClaim(ctx, c1.ID, fire1, agent1, tOld.Add(time.Second))
	require.NoError(t, err)
	assert.False(t, bound, "stale holder must not bind a superseded claim")

	released, err := cs.ReleaseTriggerSubjectClaim(ctx, c1.ID, fire1, tOld.Add(time.Second))
	require.NoError(t, err)
	assert.False(t, released, "stale holder must not release a superseded claim")

	// 5. Current holder (fire2) binds agent1.
	tBind := tOld.Add(2 * time.Second)
	bound, err = cs.BindTriggerSubjectClaim(ctx, c2.ID, fire2, agent1, tBind)
	require.NoError(t, err)
	assert.True(t, bound)

	got, err := cs.GetActiveTriggerSubjectClaim(ctx, trigID, "pr-1")
	require.NoError(t, err)
	assert.Equal(t, agent1, got.AgentID)
	require.NotNil(t, got.BoundAt)
	assert.Equal(t, tBind, *got.BoundAt)

	// 6. Bound claim is NOT taken over as abandoned even hours later.
	tLater := tBind.Add(2 * time.Hour)
	c3, existing, err := cs.ClaimTriggerSubject(ctx, trigID, "pr-1", ev3, fire3, tLater, tLater.Add(-5*time.Minute))
	require.NoError(t, err)
	assert.Nil(t, c3)
	require.NotNil(t, existing)
	assert.Equal(t, agent1, existing.AgentID)

	// 7. Explicit ReleaseTriggerSubjectClaim refuses to release a BOUND claim
	// (protects against an ambiguous COMMIT error releasing a committed bind).
	released, err = cs.ReleaseTriggerSubjectClaim(ctx, c2.ID, fire2, tLater)
	require.NoError(t, err)
	assert.False(t, released, "explicit Release must not release a bound claim (agent_id IS NOT NULL)")

	stillActive, err := cs.GetActiveTriggerSubjectClaim(ctx, trigID, "pr-1")
	require.NoError(t, err)
	assert.Equal(t, agent1, stillActive.AgentID)

	// Releasing bound claims for agent1 allows fire3 to take over and clears agent_id/bound_at/released_at.
	nReleased, err := cs.ReleaseTriggerSubjectClaimsForAgent(ctx, agent1, tLater)
	require.NoError(t, err)
	assert.Equal(t, 1, nReleased)

	_, err = cs.GetActiveTriggerSubjectClaim(ctx, trigID, "pr-1")
	assert.ErrorIs(t, err, store.ErrNotFound)

	c3, existing, err = cs.ClaimTriggerSubject(ctx, trigID, "pr-1", ev3, fire3, tLater.Add(time.Second), tLater.Add(time.Second-5*time.Minute))
	require.NoError(t, err)
	require.Nil(t, existing)
	require.NotNil(t, c3)
	assert.Equal(t, c1.ID, c3.ID)
	assert.Empty(t, c3.AgentID)
	assert.Nil(t, c3.BoundAt)
	assert.Nil(t, c3.ReleasedAt)
	assert.Equal(t, fire3, c3.FireID)
}

func TestTriggerSubjectClaimStore_Concurrency(t *testing.T) {
	ctx := context.Background()
	cs := newTestCompositeStore(t)

	trigID := uuid.NewString()
	t0 := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

	raceClaim := func(subject string, now time.Time, workers int) (winner *store.TriggerSubjectClaim) {
		t.Helper()
		start := make(chan struct{})
		var (
			wg        sync.WaitGroup
			wins      atomic.Int32
			existingN atomic.Int32
			mu        sync.Mutex
		)
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				c, ex, err := cs.ClaimTriggerSubject(ctx, trigID, subject, uuid.NewString(), uuid.NewString(), now, now.Add(-5*time.Minute))
				require.NoError(t, err)
				if c != nil {
					wins.Add(1)
					mu.Lock()
					winner = c
					mu.Unlock()
				} else {
					require.NotNil(t, ex)
					existingN.Add(1)
				}
			}()
		}
		close(start)
		wg.Wait()
		assert.Equal(t, int32(1), wins.Load(), "exactly one contender must win")
		assert.Equal(t, int32(workers-1), existingN.Load(), "all other contenders must see existing claim")
		require.NotNil(t, winner)
		return winner
	}

	// 1. Fresh insert race.
	w1 := raceClaim("race-subject", t0, 12)

	// 2. Release + reclaim race.
	released, err := cs.ReleaseTriggerSubjectClaim(ctx, w1.ID, w1.FireID, t0.Add(time.Second))
	require.NoError(t, err)
	require.True(t, released)
	w2 := raceClaim("race-subject", t0.Add(2*time.Second), 12)
	assert.Equal(t, w1.ID, w2.ID)

	// 3. Abandoned unbound claim takeover race (> 5m later).
	w3 := raceClaim("race-subject", t0.Add(6*time.Minute), 12)
	assert.Equal(t, w1.ID, w3.ID)
	assert.NotEqual(t, w2.FireID, w3.FireID)
}

func TestTriggerSubjectClaimStore_ReleaseForAgentAndMaintenance(t *testing.T) {
	ctx := context.Background()
	cs := newTestCompositeStore(t)

	proj := &store.Project{
		ID:   uuid.NewString(),
		Name: "claims-maint-proj",
		Slug: "claims-maint-proj",
	}
	require.NoError(t, cs.CreateProject(ctx, proj))

	mkAgent := func(name string, phase state.Phase, softDeleted bool) *store.Agent {
		ag := &store.Agent{
			ID:        uuid.NewString(),
			Slug:      name,
			Name:      name,
			ProjectID: proj.ID,
			Phase:     string(phase),
		}
		require.NoError(t, cs.CreateAgent(ctx, ag))
		if softDeleted {
			now := time.Now().UTC()
			_, err := cs.FinalizeAgentDeletion(ctx, ag.ID, store.DeletionPredicate{DeletedAtNull: true}, store.DeletionFinalizeSoft, store.DeletionFields{
				DeletedAt: &now,
			}, nil)
			require.NoError(t, err)
		}
		return ag
	}

	agRunning := mkAgent("ag-running", state.PhaseRunning, false)
	agSuspended := mkAgent("ag-suspended", state.PhaseSuspended, false)
	agStopped := mkAgent("ag-stopped", state.PhaseStopped, false)
	agError := mkAgent("ag-error", state.PhaseError, false)
	agHalted := mkAgent("ag-halted", state.PhaseHalted, false)
	agSoftDel := mkAgent("ag-softdel", state.PhaseRunning, true)
	agDeleteTarget := mkAgent("ag-deltarget", state.PhaseRunning, false)
	missingAgentID := uuid.NewString()

	trigID := uuid.NewString()
	t0 := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

	seedBound := func(subject, agentID string, at time.Time) *store.TriggerSubjectClaim {
		c, ex, err := cs.ClaimTriggerSubject(ctx, trigID, subject, uuid.NewString(), uuid.NewString(), at, at.Add(-5*time.Minute))
		require.NoError(t, err)
		require.Nil(t, ex)
		if agentID != "" {
			ok, bErr := cs.BindTriggerSubjectClaim(ctx, c.ID, c.FireID, agentID, at)
			require.NoError(t, bErr)
			require.True(t, ok)
		}
		return c
	}

	seedBound("s-running", agRunning.ID, t0)
	seedBound("s-suspended", agSuspended.ID, t0)
	seedBound("s-unbound", "", t0)
	seedBound("s-stopped", agStopped.ID, t0)
	seedBound("s-error", agError.ID, t0)
	seedBound("s-halted", agHalted.ID, t0)
	seedBound("s-softdel", agSoftDel.ID, t0)
	seedBound("s-missing", missingAgentID, t0)
	seedBound("s-deltarget-1", agDeleteTarget.ID, t0)
	seedBound("s-deltarget-2", agDeleteTarget.ID, t0)

	// 1. ReleaseTriggerSubjectClaimsForAgent releases both claims for agDeleteTarget and is idempotent.
	n, err := cs.ReleaseTriggerSubjectClaimsForAgent(ctx, agDeleteTarget.ID, t0.Add(time.Minute))
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	n2, err := cs.ReleaseTriggerSubjectClaimsForAgent(ctx, agDeleteTarget.ID, t0.Add(2*time.Minute))
	require.NoError(t, err)
	assert.Equal(t, 0, n2, "idempotent on retry")

	// 2. ReleaseTerminalTriggerSubjectClaims releases stopped, error, halted, soft-deleted, and missing-agent claims
	// bound at least 1 minute ago (5) plus abandoned unbound claims older than AbandonedClaimTTL (1: s-unbound at t0),
	// for 6 total — while leaving running, suspended, freshly-bound (< 1m), and fresh unbound (< 5m) claims active.
	tMaint := t0.Add(time.Hour)
	seedBound("s-fresh-missing", uuid.NewString(), tMaint.Add(-30*time.Second))
	seedBound("s-fresh-stopped", agStopped.ID, tMaint.Add(-30*time.Second))
	seedBound("s-fresh-unbound", "", tMaint.Add(-2*time.Minute))

	released, err := cs.ReleaseTerminalTriggerSubjectClaims(ctx, tMaint)
	require.NoError(t, err)
	assert.Equal(t, 6, released)

	for _, keep := range []string{"s-running", "s-suspended", "s-fresh-missing", "s-fresh-stopped", "s-fresh-unbound"} {
		_, gErr := cs.GetActiveTriggerSubjectClaim(ctx, trigID, keep)
		assert.NoError(t, gErr, "expected %s to remain active", keep)
	}
	for _, gone := range []string{"s-stopped", "s-error", "s-halted", "s-softdel", "s-missing", "s-unbound"} {
		_, gErr := cs.GetActiveTriggerSubjectClaim(ctx, trigID, gone)
		assert.ErrorIs(t, gErr, store.ErrNotFound, "expected %s to be released", gone)
	}

	// Once past the 1m bound grace window (2m later), the freshly-bound missing/terminal claims are released,
	// while s-fresh-unbound (4m old < 5m TTL) is still active.
	tAfterGrace := tMaint.Add(2 * time.Minute)
	releasedAfterGrace, err := cs.ReleaseTerminalTriggerSubjectClaims(ctx, tAfterGrace)
	require.NoError(t, err)
	assert.Equal(t, 2, releasedAfterGrace)
	for _, gone := range []string{"s-fresh-missing", "s-fresh-stopped"} {
		_, gErr := cs.GetActiveTriggerSubjectClaim(ctx, trigID, gone)
		assert.ErrorIs(t, gErr, store.ErrNotFound, "expected %s to be released after grace window", gone)
	}
	_, gErr := cs.GetActiveTriggerSubjectClaim(ctx, trigID, "s-fresh-unbound")
	assert.NoError(t, gErr, "unbound claim younger than 5m TTL must remain active")

	// Once past the 5m abandoned TTL (4m after tMaint -> 6m old), s-fresh-unbound is released too.
	tAfterAbandoned := tMaint.Add(4 * time.Minute)
	releasedAbandoned, err := cs.ReleaseTerminalTriggerSubjectClaims(ctx, tAfterAbandoned)
	require.NoError(t, err)
	assert.Equal(t, 1, releasedAbandoned)
	_, gErr = cs.GetActiveTriggerSubjectClaim(ctx, trigID, "s-fresh-unbound")
	assert.ErrorIs(t, gErr, store.ErrNotFound, "abandoned unbound claim past 5m TTL must be released")

	// 3. PurgeOldTriggerSubjectClaims deletes only released rows older than cutoff.
	// At cutoff t0+30m: the 2 rows released at t0+1m are older than cutoff, while rows
	// released at tMaint / tAfterGrace / tAfterAbandoned are newer (and active rows are never purged).
	purged, err := cs.PurgeOldTriggerSubjectClaims(ctx, t0.Add(30*time.Minute))
	require.NoError(t, err)
	assert.Equal(t, 2, purged)

	// Purging with cutoff after tAfterAbandoned removes the remaining 9 released rows, still keeping the 2 active ones.
	purged2, err := cs.PurgeOldTriggerSubjectClaims(ctx, tAfterAbandoned.Add(time.Second))
	require.NoError(t, err)
	assert.Equal(t, 9, purged2)
	for _, keep := range []string{"s-running", "s-suspended"} {
		_, gErr := cs.GetActiveTriggerSubjectClaim(ctx, trigID, keep)
		assert.NoError(t, gErr, "active claim %s must never be purged", keep)
	}
}

func TestTriggerSubjectClaimStore_ReleaseForProjectBySourceKind(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	projA := uuid.NewString()
	projB := uuid.NewString()
	for _, p := range []struct {
		id, slug, name string
	}{
		{projA, "proj-a", "Project A"},
		{projB, "proj-b", "Project B"},
	} {
		require.NoError(t, cs.CreateProject(ctx, &store.Project{ID: p.id, Slug: p.slug, Name: p.name}))
	}

	makeTrigger := func(projectID, name, sourceKind string) string {
		t.Helper()
		id := uuid.NewString()
		require.NoError(t, cs.CreateTrigger(ctx, &store.Trigger{
			ID:            id,
			ProjectID:     projectID,
			Name:          name,
			SourceKind:    sourceKind,
			MatchTypes:    []string{"*"},
			MatchDialect:  "cel",
			ActionType:    store.TriggerActionDispatchAgent,
			ActionPayload: `{"template":"default"}`,
			SubjectMode:   store.TriggerSubjectModeSkip,
			Status:        store.TriggerStatusActive,
		}))
		return id
	}

	trigAGH1 := makeTrigger(projA, "gh-1", store.TriggerSourceKindGitHub)
	trigAGH2 := makeTrigger(projA, "gh-2", store.TriggerSourceKindGitHub)
	trigAWH := makeTrigger(projA, "wh-1", store.TriggerSourceKindWebhook)
	trigBGH := makeTrigger(projB, "gh-other", store.TriggerSourceKindGitHub)

	t0 := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	seedClaim := func(trigID, subject string) *store.TriggerSubjectClaim {
		t.Helper()
		c, existing, err := cs.ClaimTriggerSubject(ctx, trigID, subject, uuid.NewString(), uuid.NewString(), t0, t0.Add(-5*time.Minute))
		require.NoError(t, err)
		require.Nil(t, existing)
		require.NotNil(t, c)
		return c
	}

	cAGH1 := seedClaim(trigAGH1, "github:acme/widgets#1")
	cAGH2 := seedClaim(trigAGH2, "github:acme/widgets#2")
	cAWH := seedClaim(trigAWH, "wh-subj-1")
	cBGH := seedClaim(trigBGH, "github:acme/other#1")

	// Release active GitHub claims in projA with release_reason = association_lost.
	tRelease := t0.Add(time.Minute)
	n, err := cs.ReleaseTriggerSubjectClaimsForProjectBySourceKind(
		ctx, projA, store.TriggerSourceKindGitHub, store.TriggerSubjectClaimReleaseReasonAssociationLost, tRelease,
	)
	require.NoError(t, err)
	assert.Equal(t, 2, n)

	// Idempotent on second call.
	n2, err := cs.ReleaseTriggerSubjectClaimsForProjectBySourceKind(
		ctx, projA, store.TriggerSourceKindGitHub, store.TriggerSubjectClaimReleaseReasonAssociationLost, tRelease.Add(time.Second),
	)
	require.NoError(t, err)
	assert.Equal(t, 0, n2)

	// Both GitHub claims in projA are no longer active and have release_reason = association_lost.
	for _, tc := range []struct {
		trigID string
		claim  *store.TriggerSubjectClaim
	}{
		{trigAGH1, cAGH1},
		{trigAGH2, cAGH2},
	} {
		_, gErr := cs.GetActiveTriggerSubjectClaim(ctx, tc.trigID, tc.claim.Subject)
		assert.ErrorIs(t, gErr, store.ErrNotFound)

		row, qErr := cs.TriggerStore.client.TriggerSubjectClaim.Get(ctx, uuid.MustParse(tc.claim.ID))
		require.NoError(t, qErr)
		assert.Equal(t, store.TriggerSubjectClaimStateReleased, row.State)
		assert.Equal(t, store.TriggerSubjectClaimReleaseReasonAssociationLost, row.ReleaseReason)
		require.NotNil(t, row.ReleasedAt)
	}

	// Non-GitHub claim in projA and GitHub claim in projB remain active.
	gotAWH, err := cs.GetActiveTriggerSubjectClaim(ctx, trigAWH, cAWH.Subject)
	require.NoError(t, err)
	assert.Equal(t, cAWH.ID, gotAWH.ID)

	gotBGH, err := cs.GetActiveTriggerSubjectClaim(ctx, trigBGH, cBGH.Subject)
	require.NoError(t, err)
	assert.Equal(t, cBGH.ID, gotBGH.ID)

	// Re-claiming a released row clears release_reason.
	reclaimed, existing, err := cs.ClaimTriggerSubject(ctx, trigAGH1, cAGH1.Subject, uuid.NewString(), uuid.NewString(), tRelease.Add(2*time.Minute), tRelease)
	require.NoError(t, err)
	require.Nil(t, existing)
	require.NotNil(t, reclaimed)
	assert.Equal(t, store.TriggerSubjectClaimStateActive, reclaimed.State)
	assert.Empty(t, reclaimed.ReleaseReason)

	reclaimedRow, err := cs.TriggerStore.client.TriggerSubjectClaim.Get(ctx, uuid.MustParse(cAGH1.ID))
	require.NoError(t, err)
	assert.Empty(t, reclaimedRow.ReleaseReason)
}
