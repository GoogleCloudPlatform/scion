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
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestAgentStore_ListIdleSuspendCandidates_Activities verifies across every
// Activity value (plus empty) that only idle, completed, and harness_error
// are returned when running, past the grace cutoff, and with a recent
// heartbeat.
func TestAgentStore_ListIdleSuspendCandidates_Activities(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	now := time.Now().UTC()
	cutoff := now.Add(-15 * time.Minute)
	heartbeatRecency := now.Add(-2 * time.Minute)
	liveWorkCutoff := now.Add(-store.LiveWorkFreshness)

	wantIDs := map[string]string{}
	for i, act := range append(state.Activities(), "") {
		slug := fmt.Sprintf("act-%d", i)
		a := makeAgent(projectID, slug)
		a.Phase = string(state.PhaseRunning)
		a.Activity = string(act)
		a.LastSeen = now.Add(-30 * time.Second)
		require.NoError(t, s.CreateAgent(ctx, a))

		_, err := s.client.Agent.UpdateOneID(uuid.MustParse(a.ID)).
			SetActivitySince(now.Add(-20 * time.Minute)).
			Save(ctx)
		require.NoError(t, err)

		if act.IsSuspendEligible() {
			wantIDs[a.ID] = string(act)
		}
	}
	require.Len(t, wantIDs, 3, "expected idle, completed, and harness_error to be eligible")

	got, err := s.ListIdleSuspendCandidates(ctx, cutoff, heartbeatRecency, liveWorkCutoff, time.Time{}, nil, 0, false)
	require.NoError(t, err)

	gotIDs := map[string]string{}
	for _, a := range got {
		gotIDs[a.ID] = a.Activity
	}
	assert.Equal(t, wantIDs, gotIDs)
}

// TestAgentStore_ListIdleSuspendCandidates_Predicates verifies the phase,
// activity_since (including NULL and boundary), heartbeat recency (offline /
// stale / NULL last_seen), live-work staleness, limit, and soft-delete filters.
func TestAgentStore_ListIdleSuspendCandidates_Predicates(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	now := time.Now().UTC()
	cutoff := now.Add(-15 * time.Minute)
	heartbeatRecency := now.Add(-2 * time.Minute)
	liveWorkCutoff := now.Add(-store.LiveWorkFreshness)

	makeIdle := func(slug string, lastSeen time.Time) *store.Agent {
		a := makeAgent(projectID, slug)
		a.Phase = string(state.PhaseRunning)
		a.Activity = string(state.ActivityIdle)
		a.LastSeen = lastSeen
		require.NoError(t, s.CreateAgent(ctx, a))
		return a
	}
	setSince := func(id string, ts *time.Time) {
		upd := s.client.Agent.UpdateOneID(uuid.MustParse(id))
		if ts == nil {
			upd.ClearActivitySince()
		} else {
			upd.SetActivitySince(*ts)
		}
		_, err := upd.Save(ctx)
		require.NoError(t, err)
	}

	oldestPast := now.Add(-30 * time.Minute)
	past := now.Add(-20 * time.Minute)
	recent := now.Add(-5 * time.Minute)

	// 1. Eligible baseline (oldest activity_since, NULL last_live_process_at).
	eligible := makeIdle("eligible", now.Add(-30*time.Second))
	setSince(eligible.ID, &oldestPast)

	// 1b. Eligible with stale last_live_process_at (< liveWorkCutoff), newer activity_since.
	staleLiveWork := makeIdle("stale-live-work", now.Add(-30*time.Second))
	_, err := s.client.Agent.UpdateOneID(uuid.MustParse(staleLiveWork.ID)).
		SetActivitySince(past).
		SetLastLiveProcessAt(now.Add(-2 * time.Minute)).
		Save(ctx)
	require.NoError(t, err)

	// 2. Inside grace window (activity_since >= cutoff) -> excluded.
	withinGrace := makeIdle("within-grace", now.Add(-30*time.Second))
	setSince(withinGrace.ID, &recent)

	// 3. Exact cutoff (activity_since == cutoff) -> excluded (strict < cutoff).
	atCutoff := makeIdle("at-cutoff", now.Add(-30*time.Second))
	setSince(atCutoff.ID, &cutoff)

	// 4. NULL activity_since (pre-migration row) -> never eligible.
	nullSince := makeIdle("null-since", now.Add(-30*time.Second))
	setSince(nullSince.ID, nil)

	// 5. Stale heartbeat (last_seen < heartbeatRecency) -> offline/stale excluded.
	staleSeen := makeIdle("stale-seen", now.Add(-10*time.Minute))
	setSince(staleSeen.ID, &past)

	// 6. NULL last_seen -> excluded.
	nullSeen := makeIdle("null-seen", time.Time{})
	_, err = s.client.Agent.UpdateOneID(uuid.MustParse(nullSeen.ID)).
		ClearLastSeen().
		SetActivitySince(past).
		Save(ctx)
	require.NoError(t, err)

	// 7. Non-running phase -> excluded even if columns somehow held idle/since.
	notRunning := makeIdle("not-running", now.Add(-30*time.Second))
	_, err = s.client.Agent.UpdateOneID(uuid.MustParse(notRunning.ID)).
		SetPhase(string(state.PhaseSuspended)).
		SetActivitySince(past).
		Save(ctx)
	require.NoError(t, err)

	// 8. Soft-deleted row -> excluded.
	softDeleted := makeIdle("soft-deleted", now.Add(-30*time.Second))
	_, err = s.client.Agent.UpdateOneID(uuid.MustParse(softDeleted.ID)).
		SetActivitySince(past).
		SetDeletedAt(now).
		Save(ctx)
	require.NoError(t, err)

	// 9. Fresh live work (last_live_process_at >= liveWorkCutoff) -> excluded.
	freshLiveWork := makeIdle("fresh-live-work", now.Add(-30*time.Second))
	_, err = s.client.Agent.UpdateOneID(uuid.MustParse(freshLiveWork.ID)).
		SetActivitySince(past).
		SetLastLiveProcessAt(now.Add(-30 * time.Second)).
		Save(ctx)
	require.NoError(t, err)

	got, err := s.ListIdleSuspendCandidates(ctx, cutoff, heartbeatRecency, liveWorkCutoff, time.Time{}, nil, 0, false)
	require.NoError(t, err)
	gotIDs := []string{}
	for _, a := range got {
		gotIDs = append(gotIDs, a.ID)
	}
	assert.Equal(t, []string{eligible.ID, staleLiveWork.ID}, gotIDs, "candidates must be ordered by activity_since ASC")

	// 10. Positive limit bounds the returned slice, picking the oldest activity_since first,
	// and a keyset cursor advances strictly past (activity_since, id), including equal timestamps.
	tieA := makeIdle("tie-a", now.Add(-30*time.Second))
	setSince(tieA.ID, &past)

	allOrdered, err := s.ListIdleSuspendCandidates(ctx, cutoff, heartbeatRecency, liveWorkCutoff, time.Time{}, nil, 0, false)
	require.NoError(t, err)
	require.Len(t, allOrdered, 3)
	assert.Equal(t, eligible.ID, allOrdered[0].ID)

	limited, err := s.ListIdleSuspendCandidates(ctx, cutoff, heartbeatRecency, liveWorkCutoff, time.Time{}, nil, 1, false)
	require.NoError(t, err)
	require.Len(t, limited, 1)
	assert.Equal(t, eligible.ID, limited[0].ID)

	var pagedIDs []string
	var cursor *store.IdleSuspendCursor
	for {
		page, perr := s.ListIdleSuspendCandidates(ctx, cutoff, heartbeatRecency, liveWorkCutoff, time.Time{}, cursor, 1, false)
		require.NoError(t, perr)
		if len(page) == 0 {
			break
		}
		pagedIDs = append(pagedIDs, page[0].ID)
		cursor = &store.IdleSuspendCursor{ActivitySince: *page[0].ActivitySince, ID: page[0].ID}
	}
	wantOrdered := []string{allOrdered[0].ID, allOrdered[1].ID, allOrdered[2].ID}
	assert.Equal(t, wantOrdered, pagedIDs, "keyset cursor must walk every candidate in exact (activity_since, id) order")
}

func TestAgentStore_ClaimIdleSuspend_AndDeliveryHandshake(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	now := time.Now().UTC()
	idleSince := now.Add(-20 * time.Minute).Truncate(time.Microsecond)

	makeEligible := func(slug string) (*store.Agent, store.IdleSuspendCondition) {
		a := makeAgent(projectID, slug)
		a.Phase = string(state.PhaseRunning)
		a.Activity = string(state.ActivityIdle)
		a.RunID = "run-1"
		a.LastSeen = now.Add(-15 * time.Second)
		require.NoError(t, s.CreateAgent(ctx, a))
		_, err := s.client.Agent.UpdateOneID(uuid.MustParse(a.ID)).
			SetRunIntent(string(store.RunIntentRunning)).
			SetRunIntentAt(now.Add(-30 * time.Minute)).
			SetActivitySince(idleSince).
			Save(ctx)
		require.NoError(t, err)
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		return got, store.IdleSuspendCondition{
			RunID:            got.RunID,
			Activity:         got.Activity,
			ActivitySince:    *got.ActivitySince,
			IdleSuspendAfter: 15 * time.Minute,
			HeartbeatRecency: 2 * time.Minute,
			ClaimTTL:         30 * time.Second,
		}
	}

	t.Run("delivery before claim blocks ClaimIdleSuspend", func(t *testing.T) {
		a, cond := makeEligible("delivery-first")
		marked, claimActive, err := s.MarkAgentDelivery(ctx, a.ID)
		require.NoError(t, err)
		assert.False(t, claimActive)
		assert.False(t, marked.IdleSuspendClaimActive)
		require.NotNil(t, marked.LastDeliveryAt)
		assert.False(t, marked.HasActiveIdleSuspendClaim(time.Now()))

		_, _, _, err = s.ClaimIdleSuspend(ctx, a.ID, cond)
		assert.ErrorIs(t, err, store.ErrIdleSuspendPrecondition)
	})

	t.Run("R2: ClaimIdleSuspend rejects run_intent!=running and accepts NULL as running", func(t *testing.T) {
		// Reviewer reproducer: a user stop recorded run_intent=stopped while
		// DispatchAgentStop failed (leaving phase=running). ClaimIdleSuspend must
		// refuse so the agent never becomes auto-wakeable suspended(idle).
		aStopped, condStopped := makeEligible("intent-already-stopped")
		_, _, err := s.SwapRunIntent(ctx, aStopped.ID, store.RunIntentStopped)
		require.NoError(t, err)
		_, _, _, err = s.ClaimIdleSuspend(ctx, aStopped.ID, condStopped)
		assert.ErrorIs(t, err, store.ErrIdleSuspendPrecondition)

		afterStopped, err := s.GetAgent(ctx, aStopped.ID)
		require.NoError(t, err)
		assert.Equal(t, store.RunIntentStopped, afterStopped.RunIntent)
		assert.Nil(t, afterStopped.IdleSuspendClaimUntil)

		// Legacy NULL run_intent is treated as running, matching BackfillRunIntent.
		aNull, condNull := makeEligible("intent-null-running")
		_, err = s.client.Agent.UpdateOneID(uuid.MustParse(aNull.ID)).ClearRunIntent().Save(ctx)
		require.NoError(t, err)
		prior, _, cu, err := s.ClaimIdleSuspend(ctx, aNull.ID, condNull)
		require.NoError(t, err)
		assert.Equal(t, store.RunIntent(""), prior)
		assert.False(t, cu.IsZero())
	})

	t.Run("claim before delivery leaves claim visible and commits across stopping and stopped reports", func(t *testing.T) {
		a, cond := makeEligible("claim-first")
		prior, at, claimUntil, err := s.ClaimIdleSuspend(ctx, a.ID, cond)
		require.NoError(t, err)
		assert.Equal(t, store.RunIntentRunning, prior)
		assert.False(t, at.IsZero())
		assert.False(t, claimUntil.IsZero())

		marked, claimActive, err := s.MarkAgentDelivery(ctx, a.ID)
		require.NoError(t, err)
		assert.True(t, claimActive, "delivery after claim must observe active claim via store clock")
		assert.True(t, marked.IdleSuspendClaimActive)
		assert.True(t, marked.HasActiveIdleSuspendClaim(time.Now()), "delivery after claim must observe active claim")
		assert.Nil(t, marked.LastDeliveryAt, "delivery while claimed must not stamp last_delivery_at")

		// Simulate container's stopping + stopped reports arriving before recordStopStatus.
		require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{
			Phase:              string(state.PhaseStopping),
			Message:            "Agent shutting down",
			ReportSource:       store.StatusReportSourceAgent,
			ReportRunID:        a.RunID,
			ReportAgentToken:   true,
			GuardReincarnation: true,
		}))
		require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{
			Phase:              string(state.PhaseStopped),
			Message:            "Agent stopped",
			ExitReason:         string(state.ExitReasonSignaled),
			ReportSource:       store.StatusReportSourceAgent,
			ReportRunID:        a.RunID,
			ReportAgentToken:   true,
			GuardReincarnation: true,
		}))

		require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{
			Phase:               string(state.PhaseSuspended),
			RetainedActivity:    string(state.ActivityIdle),
			SuspendReason:       string(state.SuspendReasonIdle),
			IfRunID:             a.RunID,
			IfIdleSuspendCommit: &claimUntil,
		}))
		require.NoError(t, s.ClearIdleSuspendClaim(ctx, a.ID))

		after, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseSuspended), after.Phase)
		assert.Equal(t, string(state.ActivityIdle), after.Activity, "idle-suspend write must restore retained pre-suspend activity after container stopped report")
		assert.Equal(t, string(state.SuspendReasonIdle), after.SuspendReason)
		assert.Nil(t, after.ActivitySince)
		assert.False(t, after.HasActiveIdleSuspendClaim(time.Now()))
	})

	t.Run("IfIdleSuspendCommit rejects mismatched claim, error phase, and foreign start claim", func(t *testing.T) {
		a, cond := makeEligible("commit-guards")
		_, _, claimUntil, err := s.ClaimIdleSuspend(ctx, a.ID, cond)
		require.NoError(t, err)

		wrongClaim := claimUntil.Add(time.Second)
		err = s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{
			Phase:               string(state.PhaseSuspended),
			SuspendReason:       string(state.SuspendReasonIdle),
			IfIdleSuspendCommit: &wrongClaim,
		})
		assert.ErrorIs(t, err, store.ErrIdleSuspendPrecondition)

		// Move row to phase=error: IfIdleSuspendCommit must not overwrite a crash.
		require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{
			Phase:   string(state.PhaseError),
			Message: "Agent crashed",
		}))
		err = s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{
			Phase:               string(state.PhaseSuspended),
			SuspendReason:       string(state.SuspendReasonIdle),
			IfIdleSuspendCommit: &claimUntil,
		})
		assert.ErrorIs(t, err, store.ErrIdleSuspendPrecondition)
	})

	t.Run("R1: user stop and other lifecycle actions invalidate idle claim and reject commit", func(t *testing.T) {
		t.Run("reviewer reproducer: user stop UpdateAgentStatus is never overwritten by idle commit", func(t *testing.T) {
			a, cond := makeEligible("user-stop-repro")
			got, err := s.GetAgent(ctx, a.ID)
			require.NoError(t, err)

			_, _, claimUntil, err := s.ClaimIdleSuspend(ctx, a.ID, cond)
			require.NoError(t, err)
			require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{
				Phase:           string(state.PhaseStopped),
				ContainerStatus: "stopped",
				IfRunID:         got.RunID,
			}))
			err = s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{
				Phase:               string(state.PhaseSuspended),
				RetainedActivity:    string(state.ActivityIdle),
				SuspendReason:       string(state.SuspendReasonIdle),
				IfRunID:             got.RunID,
				IfIdleSuspendCommit: &claimUntil,
			})
			require.ErrorIs(t, err, store.ErrIdleSuspendPrecondition)

			after, err := s.GetAgent(ctx, a.ID)
			require.NoError(t, err)
			assert.Equal(t, string(state.PhaseStopped), after.Phase)
			assert.Empty(t, after.SuspendReason)
			assert.Nil(t, after.IdleSuspendClaimUntil)
			assert.False(t, after.IdleSuspendClaimActive)
		})

		t.Run("SwapRunIntent invalidates claim before user stop status write lands", func(t *testing.T) {
			a, cond := makeEligible("swap-intent-invalidates")
			_, _, claimUntil, err := s.ClaimIdleSuspend(ctx, a.ID, cond)
			require.NoError(t, err)

			_, _, err = s.SwapRunIntent(ctx, a.ID, store.RunIntentStopped)
			require.NoError(t, err)

			mid, err := s.GetAgent(ctx, a.ID)
			require.NoError(t, err)
			assert.Nil(t, mid.IdleSuspendClaimUntil)
			assert.False(t, mid.IdleSuspendClaimActive)

			err = s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{
				Phase:               string(state.PhaseSuspended),
				RetainedActivity:    string(state.ActivityIdle),
				SuspendReason:       string(state.SuspendReasonIdle),
				IfRunID:             a.RunID,
				IfIdleSuspendCommit: &claimUntil,
			})
			require.ErrorIs(t, err, store.ErrIdleSuspendPrecondition)
		})

		t.Run("user suspend invalidates claim and keeps suspend_reason user", func(t *testing.T) {
			a, cond := makeEligible("user-suspend-invalidates")
			_, _, claimUntil, err := s.ClaimIdleSuspend(ctx, a.ID, cond)
			require.NoError(t, err)

			require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{
				Phase:            string(state.PhaseSuspended),
				ContainerStatus:  "stopped",
				RetainedActivity: string(state.ActivityIdle),
				SuspendReason:    string(state.SuspendReasonUser),
				IfRunID:          a.RunID,
			}))

			err = s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{
				Phase:               string(state.PhaseSuspended),
				RetainedActivity:    string(state.ActivityIdle),
				SuspendReason:       string(state.SuspendReasonIdle),
				IfRunID:             a.RunID,
				IfIdleSuspendCommit: &claimUntil,
			})
			require.ErrorIs(t, err, store.ErrIdleSuspendPrecondition)

			after, err := s.GetAgent(ctx, a.ID)
			require.NoError(t, err)
			assert.Equal(t, string(state.PhaseSuspended), after.Phase)
			assert.Equal(t, string(state.SuspendReasonUser), after.SuspendReason)
		})

		t.Run("user start/reincarnation/deletion preempt claim while system ClaimAgentStop yields until expiry", func(t *testing.T) {
			// User ClaimAgentStart preempts and clears idle claim.
			aStart, condStart := makeEligible("claim-start-invalidates")
			_, _, cuStart, err := s.ClaimIdleSuspend(ctx, aStart.ID, condStart)
			require.NoError(t, err)
			_, err = s.ClaimAgentStart(ctx, aStart.ID, "hub-1", store.StartClaimUser, "", 30*time.Second)
			require.NoError(t, err)
			err = s.UpdateAgentStatus(ctx, aStart.ID, store.AgentStatusUpdate{
				Phase:               string(state.PhaseSuspended),
				SuspendReason:       string(state.SuspendReasonIdle),
				IfIdleSuspendCommit: &cuStart,
			})
			assert.ErrorIs(t, err, store.ErrIdleSuspendPrecondition)

			// System ClaimAgentStop yields while idle claim is active, preserving the claim
			// so IfIdleSuspendCommit succeeds; once an idle claim expires, ClaimAgentStop takes over.
			aStop, condStop := makeEligible("claim-stop-yields")
			_, intentAt, cuStop, err := s.ClaimIdleSuspend(ctx, aStop.ID, condStop)
			require.NoError(t, err)
			_, err = s.ClaimAgentStop(ctx, aStop.ID, "hub-2", intentAt, 30*time.Second)
			assert.ErrorIs(t, err, store.ErrClaimPredicate)
			afterYield, err := s.GetAgent(ctx, aStop.ID)
			require.NoError(t, err)
			require.NotNil(t, afterYield.IdleSuspendClaimUntil)
			assert.True(t, afterYield.IdleSuspendClaimActive)
			require.NoError(t, s.UpdateAgentStatus(ctx, aStop.ID, store.AgentStatusUpdate{
				Phase:               string(state.PhaseSuspended),
				RetainedActivity:    string(state.ActivityIdle),
				SuspendReason:       string(state.SuspendReasonIdle),
				IfRunID:             aStop.RunID,
				IfIdleSuspendCommit: &cuStop,
			}))

			aStopExp, condStopExp := makeEligible("claim-stop-after-expiry")
			condStopExp.ClaimTTL = 5 * time.Millisecond
			_, intentAtExp, _, err := s.ClaimIdleSuspend(ctx, aStopExp.ID, condStopExp)
			require.NoError(t, err)
			time.Sleep(15 * time.Millisecond)
			stopClaim, err := s.ClaimAgentStop(ctx, aStopExp.ID, "hub-2", intentAtExp, 30*time.Second)
			require.NoError(t, err)
			assert.Equal(t, store.StartClaimStop, stopClaim.Kind)

			// ClaimAgentReincarnation
			aReinc, condReinc := makeEligible("claim-reinc-invalidates")
			_, _, cuReinc, err := s.ClaimIdleSuspend(ctx, aReinc.ID, condReinc)
			require.NoError(t, err)
			curReinc, err := s.GetAgent(ctx, aReinc.ID)
			require.NoError(t, err)
			_, err = s.ClaimAgentReincarnation(ctx, aReinc.ID, curReinc.StateVersion, time.Now())
			require.NoError(t, err)
			afterReinc, err := s.GetAgent(ctx, aReinc.ID)
			require.NoError(t, err)
			assert.Nil(t, afterReinc.IdleSuspendClaimUntil)
			err = s.UpdateAgentStatus(ctx, aReinc.ID, store.AgentStatusUpdate{
				Phase:               string(state.PhaseSuspended),
				SuspendReason:       string(state.SuspendReasonIdle),
				IfIdleSuspendCommit: &cuReinc,
			})
			assert.ErrorIs(t, err, store.ErrIdleSuspendPrecondition)

			// UpdateAgentDeletion
			aDel, condDel := makeEligible("del-invalidates")
			_, _, cuDel, err := s.ClaimIdleSuspend(ctx, aDel.ID, condDel)
			require.NoError(t, err)
			delState := store.DeletionStateDeleting
			lease := time.Now().Add(time.Minute)
			n, err := s.UpdateAgentDeletion(ctx, aDel.ID, store.DeletionPredicate{}, store.DeletionFields{
				State:   &delState,
				LeaseAt: &lease,
			})
			require.NoError(t, err)
			require.Equal(t, 1, n)
			err = s.UpdateAgentStatus(ctx, aDel.ID, store.AgentStatusUpdate{
				Phase:               string(state.PhaseSuspended),
				SuspendReason:       string(state.SuspendReasonIdle),
				IfIdleSuspendCommit: &cuDel,
			})
			assert.ErrorIs(t, err, store.ErrIdleSuspendPrecondition)
		})

		t.Run("expired claim lease is rejected while runtime error or non-zero stopped reports commit cleanly", func(t *testing.T) {
			aExp, condExp := makeEligible("expired-claim")
			condExp.ClaimTTL = 2 * time.Millisecond
			_, _, cuExp, err := s.ClaimIdleSuspend(ctx, aExp.ID, condExp)
			require.NoError(t, err)
			time.Sleep(10 * time.Millisecond)
			err = s.UpdateAgentStatus(ctx, aExp.ID, store.AgentStatusUpdate{
				Phase:               string(state.PhaseSuspended),
				SuspendReason:       string(state.SuspendReasonIdle),
				IfRunID:             aExp.RunID,
				IfIdleSuspendCommit: &cuExp,
			})
			assert.ErrorIs(t, err, store.ErrIdleSuspendPrecondition)

			// Multi-replica post-stop broker heartbeat promoting Exited (137) to phase=error:
			// claim remains held because ReportSource is runtime, so IfIdleSuspendCommit
			// must commit suspended(idle) and restore RetainedActivity while keeping exit metadata.
			aErr, condErr := makeEligible("heartbeat-error-137")
			_, _, cuErr, err := s.ClaimIdleSuspend(ctx, aErr.ID, condErr)
			require.NoError(t, err)
			exit137 := 137
			require.NoError(t, s.UpdateAgentStatus(ctx, aErr.ID, store.AgentStatusUpdate{
				Phase:              string(state.PhaseError),
				ContainerStatus:    "Exited (137)",
				ExitCode:           &exit137,
				ExitReason:         string(state.ExitReasonCrashed),
				Heartbeat:          true,
				Message:            "Agent crashed with exit code 137",
				ReportSource:       store.StatusReportSourceRuntime,
				ReportRunID:        aErr.RunID,
				GuardReincarnation: true,
			}))
			require.NoError(t, s.UpdateAgentStatus(ctx, aErr.ID, store.AgentStatusUpdate{
				Phase:               string(state.PhaseSuspended),
				ContainerStatus:     "stopped",
				RetainedActivity:    string(state.ActivityIdle),
				SuspendReason:       string(state.SuspendReasonIdle),
				IfRunID:             aErr.RunID,
				IfIdleSuspendCommit: &cuErr,
			}))
			afterErr, err := s.GetAgent(ctx, aErr.ID)
			require.NoError(t, err)
			assert.Equal(t, string(state.PhaseSuspended), afterErr.Phase)
			assert.Equal(t, string(state.SuspendReasonIdle), afterErr.SuspendReason)
			assert.Equal(t, string(state.ActivityIdle), afterErr.Activity)
			require.NotNil(t, afterErr.ExitCode)
			assert.Equal(t, 137, *afterErr.ExitCode)

			// Container/runtime stopped report with non-zero exit code or terminal activity
			// during shutdown also commits suspended(idle) while the exact claim is held.
			aExit, condExit := makeEligible("nonzero-exit-stop")
			_, _, cuExit, err := s.ClaimIdleSuspend(ctx, aExit.ID, condExit)
			require.NoError(t, err)
			exitOne := 1
			require.NoError(t, s.UpdateAgentStatus(ctx, aExit.ID, store.AgentStatusUpdate{
				Phase:              string(state.PhaseStopped),
				Activity:           string(state.ActivityCrashed),
				ExitCode:           &exitOne,
				ReportSource:       store.StatusReportSourceRuntime,
				ReportRunID:        aExit.RunID,
				GuardReincarnation: true,
			}))
			require.NoError(t, s.UpdateAgentStatus(ctx, aExit.ID, store.AgentStatusUpdate{
				Phase:               string(state.PhaseSuspended),
				ContainerStatus:     "stopped",
				RetainedActivity:    string(state.ActivityIdle),
				SuspendReason:       string(state.SuspendReasonIdle),
				IfRunID:             aExit.RunID,
				IfIdleSuspendCommit: &cuExit,
			}))
			afterExit, err := s.GetAgent(ctx, aExit.ID)
			require.NoError(t, err)
			assert.Equal(t, string(state.PhaseSuspended), afterExit.Phase)
			assert.Equal(t, string(state.SuspendReasonIdle), afterExit.SuspendReason)
			assert.Equal(t, string(state.ActivityIdle), afterExit.Activity)
		})
	})

	t.Run("O1: GetAgent and GetAgentBySlug populate IdleSuspendClaimActive", func(t *testing.T) {
		a, cond := makeEligible("get-claim-active")

		beforeByID, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.False(t, beforeByID.IdleSuspendClaimActive)
		beforeBySlug, err := s.GetAgentBySlug(ctx, a.ProjectID, a.Slug)
		require.NoError(t, err)
		assert.False(t, beforeBySlug.IdleSuspendClaimActive)

		cond.ClaimTTL = 10 * time.Second
		_, _, _, err = s.ClaimIdleSuspend(ctx, a.ID, cond)
		require.NoError(t, err)

		duringByID, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.True(t, duringByID.IdleSuspendClaimActive)
		duringBySlug, err := s.GetAgentBySlug(ctx, a.ProjectID, a.Slug)
		require.NoError(t, err)
		assert.True(t, duringBySlug.IdleSuspendClaimActive)

		_, err = s.client.Agent.UpdateOneID(uuid.MustParse(a.ID)).
			SetIdleSuspendClaimUntil(time.Now().UTC().Add(-time.Second)).
			SetRunIntentAt(time.Now().UTC().Add(-time.Minute)).SetRunIntentMarkedAt(time.Now().UTC().Add(-time.Minute)).
			Save(ctx)
		require.NoError(t, err)
		expiredByID, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.False(t, expiredByID.IdleSuspendClaimLeaseActive)
		assert.True(t, expiredByID.IdleSuspendClaimActive, "expired stamp with run_intent=stopped is an idle suspend in progress")
		expiredBySlug, err := s.GetAgentBySlug(ctx, a.ProjectID, a.Slug)
		require.NoError(t, err)
		assert.False(t, expiredBySlug.IdleSuspendClaimLeaseActive)
	})

	t.Run("IfNoStartClaim and IfAutoWakeReason guard conversion CAS", func(t *testing.T) {
		a, _ := makeEligible("convert-cas")
		require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{
			Phase:         string(state.PhaseSuspended),
			SuspendReason: string(state.SuspendReasonIdle),
		}))

		// Take a start claim: conversion with IfNoStartClaim must fail with ErrClaimPredicate.
		_, err := s.ClaimAgentStart(ctx, a.ID, "hub-1", store.StartClaimWake, "", 30*time.Second)
		require.NoError(t, err)

		err = s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{
			Phase:            string(state.PhaseSuspended),
			SuspendReason:    string(state.SuspendReasonUser),
			IfPhase:          string(state.PhaseSuspended),
			IfNoStartClaim:   true,
			IfAutoWakeReason: true,
		})
		assert.ErrorIs(t, err, store.ErrClaimPredicate)

		after, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.SuspendReasonIdle), after.SuspendReason)
	})

	t.Run("concurrent ClaimIdleSuspend vs MarkAgentDelivery serializes cleanly", func(t *testing.T) {
		for round := 0; round < 20; round++ {
			a, cond := makeEligible(fmt.Sprintf("race-round-%d", round))
			start := make(chan struct{})
			var claimErr, markErr error
			var claimActive bool
			done := make(chan struct{}, 2)

			go func() {
				<-start
				_, _, _, claimErr = s.ClaimIdleSuspend(ctx, a.ID, cond)
				done <- struct{}{}
			}()
			go func() {
				<-start
				_, claimActive, markErr = s.MarkAgentDelivery(ctx, a.ID)
				done <- struct{}{}
			}()
			close(start)
			<-done
			<-done

			require.NoError(t, markErr)
			if claimErr == nil {
				// ClaimIdleSuspend ran first -> MarkAgentDelivery MUST have seen claimActive=true.
				assert.True(t, claimActive, "round %d: when ClaimIdleSuspend wins, MarkAgentDelivery must observe claimActive", round)
			} else {
				// MarkAgentDelivery ran first -> ClaimIdleSuspend MUST have failed with ErrIdleSuspendPrecondition.
				assert.ErrorIs(t, claimErr, store.ErrIdleSuspendPrecondition, "round %d", round)
				assert.False(t, claimActive, "round %d: when MarkAgentDelivery wins, claimActive must be false", round)
			}
		}
	})

	t.Run("post-commit late agent and runtime stopped reports keep suspended(idle) intact", func(t *testing.T) {
		a, cond := makeEligible("post-commit-sticky-reports")
		_, _, claimUntil, err := s.ClaimIdleSuspend(ctx, a.ID, cond)
		require.NoError(t, err)

		require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{
			Phase:               string(state.PhaseSuspended),
			ContainerStatus:     "stopped",
			SuspendReason:       string(state.SuspendReasonIdle),
			RetainedActivity:    string(state.ActivityIdle),
			IfRunID:             a.RunID,
			IfIdleSuspendCommit: &claimUntil,
		}))

		// 1. Late agent "Session ended" report arriving in the post-commit, pre-clear window.
		require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{
			Phase:              string(state.PhaseStopped),
			Activity:           string(state.ActivityCrashed),
			Message:            "Session ended",
			ExitReason:         string(state.ExitReasonSignaled),
			ReportSource:       store.StatusReportSourceAgent,
			ReportRunID:        a.RunID,
			ReportAgentToken:   true,
			GuardReincarnation: true,
		}))
		mid, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseSuspended), mid.Phase)
		assert.Equal(t, string(state.SuspendReasonIdle), mid.SuspendReason)
		assert.Equal(t, string(state.ActivityIdle), mid.Activity)
		assert.Equal(t, store.RunIntentStopped, mid.RunIntent)

		require.NoError(t, s.ClearIdleSuspendClaim(ctx, a.ID))

		// 2. Late agent stopped report and late runtime broker stopped report after claim clear.
		require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{
			Phase:              string(state.PhaseStopped),
			Message:            "Session ended",
			ExitReason:         string(state.ExitReasonSignaled),
			ReportSource:       store.StatusReportSourceAgent,
			ReportRunID:        a.RunID,
			ReportAgentToken:   true,
			GuardReincarnation: true,
		}))
		exit137 := 137
		require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{
			Phase:           string(state.PhaseStopped),
			Activity:        string(state.ActivityCrashed),
			ContainerStatus: "exited (137)",
			ExitCode:        &exit137,
			ReportSource:    store.StatusReportSourceRuntime,
			ReportRunID:     a.RunID,
		}))

		after, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseSuspended), after.Phase)
		assert.Equal(t, string(state.SuspendReasonIdle), after.SuspendReason)
		assert.Equal(t, string(state.ActivityIdle), after.Activity)
		assert.Empty(t, after.ExitReason)
		assert.Nil(t, after.ExitCode)
		assert.Equal(t, store.RunIntentStopped, after.RunIntent)
		assert.Nil(t, after.IdleSuspendClaimUntil)
	})
}

func TestAgentStore_ClaimAgentStartWakeYieldsToActiveIdleClaim(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	cs := NewCompositeStore(s.client)
	now := time.Now().UTC()
	idleSince := now.Add(-20 * time.Minute).Truncate(time.Microsecond)

	makeClaimed := func(slug string, ttl time.Duration) (*store.Agent, time.Time) {
		a := makeAgent(projectID, slug)
		a.Phase = string(state.PhaseRunning)
		a.Activity = string(state.ActivityIdle)
		a.RunID = "run-" + slug
		a.LastSeen = now.Add(-15 * time.Second)
		require.NoError(t, s.CreateAgent(ctx, a))
		_, err := s.client.Agent.UpdateOneID(uuid.MustParse(a.ID)).
			SetRunIntent(string(store.RunIntentRunning)).
			SetRunIntentAt(now.Add(-30 * time.Minute)).
			SetActivitySince(idleSince).
			Save(ctx)
		require.NoError(t, err)
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		_, _, cu, err := s.ClaimIdleSuspend(ctx, a.ID, store.IdleSuspendCondition{
			RunID:            got.RunID,
			Activity:         got.Activity,
			ActivitySince:    *got.ActivitySince,
			IdleSuspendAfter: 15 * time.Minute,
			HeartbeatRecency: 2 * time.Minute,
			ClaimTTL:         ttl,
		})
		require.NoError(t, err)
		return got, cu
	}

	// 1. System wake and recovery start claims yield while an idle claim is active,
	// without clearing idle_suspend_claim_until.
	for _, kind := range []store.StartClaimKind{store.StartClaimWake, store.StartClaimRecovery} {
		a, cu := makeClaimed("wake-yield-"+string(kind), 30*time.Second)
		_, err := s.ClaimAgentStart(ctx, a.ID, "hub-wake", kind, "", 30*time.Second)
		assert.ErrorIs(t, err, store.ErrClaimPredicate)

		after, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		require.NotNil(t, after.IdleSuspendClaimUntil)
		assert.True(t, after.IdleSuspendClaimActive)
		assert.Equal(t, store.RunIntentStopped, after.RunIntent)

		// SwapSystemRunIntent also yields without clearing the active claim.
		_, _, err = s.SwapSystemRunIntent(ctx, a.ID, store.RunIntentRunning)
		assert.ErrorIs(t, err, store.ErrIdleSuspendPrecondition)
		_, _, err = s.SwapSystemRunIntent(ctx, a.ID, store.RunIntentStopped)
		assert.ErrorIs(t, err, store.ErrIdleSuspendPrecondition)

		// Stale container/agent starting report preserves the active claim.
		require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{
			Phase:              string(state.PhaseStarting),
			ReportSource:       store.StatusReportSourceAgent,
			ReportRunID:        a.RunID,
			ReportAgentToken:   true,
			GuardReincarnation: true,
		}))
		// ConvertUnconfirmedToStop yields while the idle claim is active.
		_, err = s.ConvertUnconfirmedToStop(ctx, a.ID, "missing-claim", "hub-reaper", 30*time.Second)
		assert.ErrorIs(t, err, store.ErrClaimPredicate)

		// Move back to stopping via agent report and commit suspended(idle) cleanly.
		require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{
			Phase:              string(state.PhaseStopping),
			ReportSource:       store.StatusReportSourceAgent,
			ReportRunID:        a.RunID,
			ReportAgentToken:   true,
			GuardReincarnation: true,
		}))
		require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{
			Phase:               string(state.PhaseSuspended),
			RetainedActivity:    string(state.ActivityIdle),
			SuspendReason:       string(state.SuspendReasonIdle),
			IfRunID:             a.RunID,
			IfIdleSuspendCommit: &cu,
		}))
	}

	// 2. Active AgentHold during IfIdleSuspendCommit omits suspend_reason so held
	// agents land in suspended(reason="") and never auto-wake.
	aHeld, cuHeld := makeClaimed("commit-with-hold", 30*time.Second)
	_, err := s.client.AgentHold.Create().
		SetAgentID(uuid.MustParse(aHeld.ID)).
		SetProjectID(uuid.MustParse(projectID)).
		SetCause("owner_access_ended").
		SetTrigger("member_remove").
		SetRootPrincipalType("user").
		SetRootPrincipalID("u-hold").
		Save(ctx)
	require.NoError(t, err)
	require.NoError(t, s.UpdateAgentStatus(ctx, aHeld.ID, store.AgentStatusUpdate{
		Phase:               string(state.PhaseSuspended),
		RetainedActivity:    string(state.ActivityIdle),
		SuspendReason:       string(state.SuspendReasonIdle),
		IfRunID:             aHeld.RunID,
		IfIdleSuspendCommit: &cuHeld,
	}))
	afterHeld, err := s.GetAgent(ctx, aHeld.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseSuspended), afterHeld.Phase)
	assert.Empty(t, afterHeld.SuspendReason, "membership-held agent must not get suspend_reason=idle")

	// 3. ListAgents stop-all filter (Phase=running OR OrRunIntent=running) matches
	// active idle claims (with IdleSuspendClaimActive=true) and ignores expired claims.
	aActiveFilter, _ := makeClaimed("stopall-active-claim", 30*time.Second)
	require.NoError(t, s.UpdateAgentStatus(ctx, aActiveFilter.ID, store.AgentStatusUpdate{
		Phase:        string(state.PhaseStopped),
		ReportSource: store.StatusReportSourceRuntime,
		ReportRunID:  aActiveFilter.RunID,
	}))
	aExpiredFilter, _ := makeClaimed("stopall-expired-claim", 5*time.Millisecond)
	require.NoError(t, s.UpdateAgentStatus(ctx, aExpiredFilter.ID, store.AgentStatusUpdate{
		Phase:        string(state.PhaseStopped),
		ReportSource: store.StatusReportSourceRuntime,
		ReportRunID:  aExpiredFilter.RunID,
	}))
	// An expired stamp on a row already recorded suspended is not in progress.
	aExpiredSuspended, _ := makeClaimed("stopall-expired-suspended", 5*time.Millisecond)
	_, err = s.client.Agent.UpdateOneID(uuid.MustParse(aExpiredSuspended.ID)).SetPhase(string(state.PhaseSuspended)).Save(ctx)
	require.NoError(t, err)
	time.Sleep(15 * time.Millisecond)

	listed, err := s.ListAgents(ctx, store.AgentFilter{
		ProjectID:         projectID,
		Phase:             string(state.PhaseRunning),
		OrRunIntent:       string(store.RunIntentRunning),
		OrActiveIdleClaim: true,
	}, store.ListOptions{})
	require.NoError(t, err)
	listedMap := map[string]store.Agent{}
	for _, item := range listed.Items {
		listedMap[item.ID] = item
	}
	require.Contains(t, listedMap, aActiveFilter.ID, "active idle claim must be matched by stop-all filter")
	assert.True(t, listedMap[aActiveFilter.ID].IdleSuspendClaimActive, "ListAgents must populate IdleSuspendClaimActive")
	assert.Contains(t, listedMap, aExpiredFilter.ID, "an expired claim whose suspend is still in progress (intent stopped, not suspended) must be matched by stop-all")
	assert.NotContains(t, listedMap, aExpiredSuspended.ID, "an expired claim on a suspended row must NOT be matched by stop-all filter")

	// With OrActiveIdleClaim=false (lifecycle.idle_suspend off), ListAgents skips
	// the active-claim OR branch and incurs no extra store-clock query.
	listedOff, err := s.ListAgents(ctx, store.AgentFilter{
		ProjectID:         projectID,
		Phase:             string(state.PhaseRunning),
		OrRunIntent:       string(store.RunIntentRunning),
		OrActiveIdleClaim: false,
	}, store.ListOptions{})
	require.NoError(t, err)
	for _, item := range listedOff.Items {
		assert.NotEqual(t, aActiveFilter.ID, item.ID, "OrActiveIdleClaim=false must omit stopped row with active claim")
		assert.NotEqual(t, aExpiredFilter.ID, item.ID)
	}

	// 4. Held awaiting_session messages survive active idle claim and suspended(idle),
	// rebind cleanly via RebindHeldMessagesLaunch, and still expire on user suspend.
	aMsg := makeAgent(projectID, "hold-survive-store")
	aMsg.Phase = string(state.PhaseRunning)
	aMsg.Activity = string(state.ActivityIdle)
	aMsg.RunID = "run-msg-1"
	aMsg.LastSeen = now.Add(-10 * time.Second)
	require.NoError(t, s.CreateAgent(ctx, aMsg))
	_, err = s.SetAgentRunID(ctx, aMsg.ID, "run-msg-1", nil)
	require.NoError(t, err)
	_, err = s.client.Agent.UpdateOneID(uuid.MustParse(aMsg.ID)).
		SetRunIntent(string(store.RunIntentRunning)).
		SetRunIntentAt(now.Add(-30 * time.Minute)).
		SetActivitySince(idleSince).
		Save(ctx)
	require.NoError(t, err)

	msg := &store.Message{
		ID:            uuid.NewString(),
		ProjectID:     projectID,
		AgentID:       aMsg.ID,
		Sender:        "user:u1",
		SenderID:      "u1",
		Recipient:     "agent:" + aMsg.Slug,
		RecipientID:   aMsg.ID,
		Msg:           "survive-idle-suspend",
		Type:          "instruction",
		DispatchState: store.MessageDispatchDispatched,
		CreatedAt:     now,
	}
	require.NoError(t, cs.CreateMessage(ctx, msg))
	_, err = cs.PrepareOrHoldMessage(ctx, aMsg.ID, msg.ID, 100)
	require.NoError(t, err)
	bindRes, err := cs.BindMessageDelivery(ctx, aMsg.ID, "deliv-1", []string{msg.ID}, now)
	require.NoError(t, err)
	require.Equal(t, store.DeliveryBound, bindRes.Result)
	require.NoError(t, cs.UnbindMessageDeliveryWithProof(ctx, "deliv-1", []string{msg.ID}, store.MessageNotReachedNoSession))
	heldOK, err := cs.HoldMessageAwaitingSession(ctx, store.HoldMessageParams{
		MessageID:   msg.ID,
		AgentID:     aMsg.ID,
		MaxProofAge: 2 * time.Minute,
		HoldTTL:     10 * time.Minute,
		MaxHeld:     100,
	})
	require.NoError(t, err)
	require.True(t, heldOK)

	gotMsgAgent, err := s.GetAgent(ctx, aMsg.ID)
	require.NoError(t, err)
	_, _, cuMsg, err := s.ClaimIdleSuspend(ctx, aMsg.ID, store.IdleSuspendCondition{
		RunID:            gotMsgAgent.RunID,
		Activity:         gotMsgAgent.Activity,
		ActivitySince:    *gotMsgAgent.ActivitySince,
		IdleSuspendAfter: 15 * time.Minute,
		HeartbeatRecency: 2 * time.Minute,
		ClaimTTL:         30 * time.Second,
	})
	require.NoError(t, err)

	// Container post-stop report moves phase to stopped; ExpireHeldMessagesForAgent("agent stopped") preserves row!
	require.NoError(t, s.UpdateAgentStatus(ctx, aMsg.ID, store.AgentStatusUpdate{
		Phase:        string(state.PhaseStopped),
		ReportSource: store.StatusReportSourceRuntime,
		ReportRunID:  aMsg.RunID,
	}))
	expDuringClaim, err := cs.ExpireHeldMessagesForAgent(ctx, aMsg.ID, "agent stopped")
	require.NoError(t, err)
	assert.Empty(t, expDuringClaim, "held row must survive post-stop report while idle claim is active")

	// Commit suspended(idle) and clear claim; ExpireHeldMessagesForAgent("agent stopped") STILL preserves row!
	require.NoError(t, s.UpdateAgentStatus(ctx, aMsg.ID, store.AgentStatusUpdate{
		Phase:               string(state.PhaseSuspended),
		RetainedActivity:    string(state.ActivityIdle),
		SuspendReason:       string(state.SuspendReasonIdle),
		IfRunID:             aMsg.RunID,
		IfIdleSuspendCommit: &cuMsg,
	}))
	require.NoError(t, s.ClearIdleSuspendClaim(ctx, aMsg.ID))
	expWhileIdleSuspended, err := cs.ExpireHeldMessagesForAgent(ctx, aMsg.ID, "agent stopped")
	require.NoError(t, err)
	assert.Empty(t, expWhileIdleSuspended, "held row must survive resting phase=suspended, suspend_reason=idle")

	// RebindHeldMessagesLaunch rebinds hold_launch_id to resumed RunID.
	rebound, err := cs.RebindHeldMessagesLaunch(ctx, aMsg.ID, "run-msg-1", "run-msg-2")
	require.NoError(t, err)
	assert.Equal(t, 1, rebound)
	gotMsg, err := cs.GetMessage(ctx, msg.ID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchAwaitingSession, gotMsg.DispatchState)
	assert.Equal(t, "run-msg-2", gotMsg.HoldLaunchID)

	// Converting to suspended(user) makes ExpireHeldMessagesForAgent("agent stopped") expire the row.
	require.NoError(t, s.UpdateAgentStatus(ctx, aMsg.ID, store.AgentStatusUpdate{
		Phase:         string(state.PhaseSuspended),
		SuspendReason: string(state.SuspendReasonUser),
	}))
	expUserSuspend, err := cs.ExpireHeldMessagesForAgent(ctx, aMsg.ID, "agent stopped")
	require.NoError(t, err)
	require.Len(t, expUserSuspend, 1)
	assert.Equal(t, msg.ID, expUserSuspend[0].ID)
}

func TestAgentStore_ClaimIdleSuspend_PreconditionPredicates(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	now := time.Now().UTC()
	idleSince := now.Add(-20 * time.Minute).Truncate(time.Microsecond)

	makeEligible := func(slug string) (*store.Agent, store.IdleSuspendCondition) {
		a := makeAgent(projectID, slug)
		a.Phase = string(state.PhaseRunning)
		a.Activity = string(state.ActivityIdle)
		a.RunID = "run-pred-" + slug
		a.LastSeen = now.Add(-15 * time.Second)
		require.NoError(t, s.CreateAgent(ctx, a))
		_, err := s.client.Agent.UpdateOneID(uuid.MustParse(a.ID)).
			SetRunIntent(string(store.RunIntentRunning)).
			SetRunIntentAt(now.Add(-30 * time.Minute)).
			SetActivitySince(idleSince).
			Save(ctx)
		require.NoError(t, err)
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		return got, store.IdleSuspendCondition{
			RunID:                    got.RunID,
			Activity:                 got.Activity,
			ActivitySince:            *got.ActivitySince,
			IdleSuspendAfter:         15 * time.Minute,
			HeartbeatRecency:         2 * time.Minute,
			ClaimTTL:                 30 * time.Second,
			ClaimBackoff:             2 * time.Minute,
			RequireSuspendPolicyAuto: true,
		}
	}

	assertPredicate := func(t *testing.T, err error, wantPredicate string, wantKeys ...string) {
		t.Helper()
		require.Error(t, err)
		assert.ErrorIs(t, err, store.ErrIdleSuspendPrecondition)
		assert.ErrorIs(t, err, store.ErrVersionConflict)
		var preErr *store.IdleSuspendPreconditionError
		require.True(t, errors.As(err, &preErr), "expected *store.IdleSuspendPreconditionError, got %T: %v", err, err)
		assert.Equal(t, wantPredicate, preErr.Predicate)
		assert.Contains(t, preErr.Error(), wantPredicate)
		require.Zero(t, len(preErr.Attrs)%2, "Attrs must be key/value pairs")
		keys := make(map[string]any, len(preErr.Attrs)/2)
		for i := 0; i < len(preErr.Attrs); i += 2 {
			k, ok := preErr.Attrs[i].(string)
			require.True(t, ok, "attr key at index %d must be string", i)
			keys[k] = preErr.Attrs[i+1]
		}
		for _, wk := range wantKeys {
			assert.Contains(t, keys, wk)
		}
	}

	t.Run("row_nil", func(t *testing.T) {
		preErr := checkIdleSuspendRow(nil, store.IdleSuspendCondition{}, now)
		assertPredicate(t, preErr, "row_nil")
	})

	cases := []struct {
		name          string
		mutate        func(t *testing.T, a *store.Agent, cond *store.IdleSuspendCondition)
		wantPredicate string
		wantKeys      []string
	}{
		{
			name: "deleted_or_deleting",
			mutate: func(t *testing.T, a *store.Agent, _ *store.IdleSuspendCondition) {
				_, err := s.client.Agent.UpdateOneID(uuid.MustParse(a.ID)).
					SetDeletedAt(now).
					Save(ctx)
				require.NoError(t, err)
			},
			wantPredicate: "deleted_or_deleting",
			wantKeys:      []string{"deleted_at", "deletion_state", "deletion_lease_at", "store_now"},
		},
		{
			name: "reincarnation_in_flight",
			mutate: func(t *testing.T, a *store.Agent, _ *store.IdleSuspendCondition) {
				_, err := s.client.Agent.UpdateOneID(uuid.MustParse(a.ID)).
					SetReincarnationState(store.ReincarnationStatePending).
					Save(ctx)
				require.NoError(t, err)
			},
			wantPredicate: "reincarnation_in_flight",
			wantKeys:      []string{"reincarnation_state"},
		},
		{
			name: "start_claim_held",
			mutate: func(t *testing.T, a *store.Agent, _ *store.IdleSuspendCondition) {
				_, err := s.ClaimAgentStart(ctx, a.ID, "hub-1", store.StartClaimUser, "", 30*time.Second)
				require.NoError(t, err)
			},
			wantPredicate: "start_claim_held",
			wantKeys:      []string{"start_claim_id", "start_claim_kind", "start_claim_state", "start_claim_owner"},
		},
		{
			name: "idle_claim_active",
			mutate: func(t *testing.T, a *store.Agent, _ *store.IdleSuspendCondition) {
				_, err := s.client.Agent.UpdateOneID(uuid.MustParse(a.ID)).
					SetIdleSuspendClaimUntil(time.Now().UTC().Add(time.Minute)).
					Save(ctx)
				require.NoError(t, err)
			},
			wantPredicate: "idle_claim_active",
			wantKeys:      []string{"idle_suspend_claim_until", "store_now"},
		},
		{
			name: "idle_claim_backoff",
			mutate: func(t *testing.T, a *store.Agent, _ *store.IdleSuspendCondition) {
				_, err := s.client.Agent.UpdateOneID(uuid.MustParse(a.ID)).
					SetIdleSuspendClaimUntil(time.Now().UTC().Add(-10 * time.Second)).
					Save(ctx)
				require.NoError(t, err)
			},
			wantPredicate: "idle_claim_backoff",
			wantKeys:      []string{"idle_suspend_claim_until", "claim_backoff", "store_now"},
		},
		{
			name: "phase_not_running",
			mutate: func(t *testing.T, a *store.Agent, _ *store.IdleSuspendCondition) {
				_, err := s.client.Agent.UpdateOneID(uuid.MustParse(a.ID)).
					SetPhase(string(state.PhaseStopped)).
					Save(ctx)
				require.NoError(t, err)
			},
			wantPredicate: "phase_not_running",
			wantKeys:      []string{"phase"},
		},
		{
			name: "suspend_policy_never",
			mutate: func(t *testing.T, a *store.Agent, _ *store.IdleSuspendCondition) {
				_, err := s.client.Agent.UpdateOneID(uuid.MustParse(a.ID)).
					SetSuspendPolicy(agent.SuspendPolicyNever).
					Save(ctx)
				require.NoError(t, err)
			},
			wantPredicate: "suspend_policy_never",
			wantKeys:      []string{"suspend_policy"},
		},
		{
			name: "run_intent_not_running",
			mutate: func(t *testing.T, a *store.Agent, _ *store.IdleSuspendCondition) {
				_, _, err := s.SwapRunIntent(ctx, a.ID, store.RunIntentStopped)
				require.NoError(t, err)
			},
			wantPredicate: "run_intent_not_running",
			wantKeys:      []string{"run_intent"},
		},
		{
			name: "run_id_mismatch",
			mutate: func(t *testing.T, _ *store.Agent, cond *store.IdleSuspendCondition) {
				cond.RunID = "different-run-id"
			},
			wantPredicate: "run_id_mismatch",
			wantKeys:      []string{"row_run_id", "cond_run_id"},
		},
		{
			name: "activity_changed_or_ineligible",
			mutate: func(t *testing.T, a *store.Agent, _ *store.IdleSuspendCondition) {
				_, err := s.client.Agent.UpdateOneID(uuid.MustParse(a.ID)).
					SetActivity(string(state.ActivityThinking)).
					Save(ctx)
				require.NoError(t, err)
			},
			wantPredicate: "activity_changed_or_ineligible",
			wantKeys:      []string{"row_activity", "cond_activity"},
		},
		{
			name: "activity_since_changed",
			mutate: func(t *testing.T, a *store.Agent, _ *store.IdleSuspendCondition) {
				_, err := s.client.Agent.UpdateOneID(uuid.MustParse(a.ID)).
					SetActivitySince(idleSince.Add(time.Minute)).
					Save(ctx)
				require.NoError(t, err)
			},
			wantPredicate: "activity_since_changed",
			wantKeys:      []string{"row_activity_since", "cond_activity_since"},
		},
		{
			name: "idle_grace_not_elapsed",
			mutate: func(t *testing.T, a *store.Agent, cond *store.IdleSuspendCondition) {
				recent := time.Now().UTC().Add(-5 * time.Minute).Truncate(time.Microsecond)
				_, err := s.client.Agent.UpdateOneID(uuid.MustParse(a.ID)).
					SetActivitySince(recent).
					Save(ctx)
				require.NoError(t, err)
				cond.ActivitySince = recent
			},
			wantPredicate: "idle_grace_not_elapsed",
			wantKeys:      []string{"row_activity_since", "idle_suspend_after", "store_now"},
		},
		{
			name: "last_seen_nil",
			mutate: func(t *testing.T, a *store.Agent, _ *store.IdleSuspendCondition) {
				_, err := s.client.Agent.UpdateOneID(uuid.MustParse(a.ID)).
					ClearLastSeen().
					Save(ctx)
				require.NoError(t, err)
			},
			wantPredicate: "last_seen_nil",
		},
		{
			name: "heartbeat_stale",
			mutate: func(t *testing.T, a *store.Agent, _ *store.IdleSuspendCondition) {
				_, err := s.client.Agent.UpdateOneID(uuid.MustParse(a.ID)).
					SetLastSeen(time.Now().UTC().Add(-10 * time.Minute)).
					Save(ctx)
				require.NoError(t, err)
			},
			wantPredicate: "heartbeat_stale",
			wantKeys:      []string{"last_seen", "heartbeat_recency", "store_now"},
		},
		{
			name: "recent_live_work",
			mutate: func(t *testing.T, a *store.Agent, _ *store.IdleSuspendCondition) {
				_, err := s.client.Agent.UpdateOneID(uuid.MustParse(a.ID)).
					SetLastLiveProcessAt(time.Now().UTC().Add(-10 * time.Second)).
					Save(ctx)
				require.NoError(t, err)
			},
			wantPredicate: "recent_live_work",
			wantKeys:      []string{"last_live_process_at", "live_work_freshness", "store_now"},
		},
		{
			name: "delivery_since_activity",
			mutate: func(t *testing.T, a *store.Agent, _ *store.IdleSuspendCondition) {
				_, _, err := s.MarkAgentDelivery(ctx, a.ID)
				require.NoError(t, err)
			},
			wantPredicate: "delivery_since_activity",
			wantKeys:      []string{"last_delivery_at", "row_activity_since", "store_now"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, cond := makeEligible("pred-" + tc.name)
			tc.mutate(t, a, &cond)
			_, _, _, err := s.ClaimIdleSuspend(ctx, a.ID, cond)
			assertPredicate(t, err, tc.wantPredicate, tc.wantKeys...)
		})
	}
}
