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
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
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

	got, err := s.ListIdleSuspendCandidates(ctx, cutoff, heartbeatRecency, liveWorkCutoff, 0)
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

	got, err := s.ListIdleSuspendCandidates(ctx, cutoff, heartbeatRecency, liveWorkCutoff, 0)
	require.NoError(t, err)
	gotIDs := []string{}
	for _, a := range got {
		gotIDs = append(gotIDs, a.ID)
	}
	assert.Equal(t, []string{eligible.ID, staleLiveWork.ID}, gotIDs, "candidates must be ordered by activity_since ASC")

	// 10. Positive limit bounds the returned slice, picking the oldest activity_since first.
	limited, err := s.ListIdleSuspendCandidates(ctx, cutoff, heartbeatRecency, liveWorkCutoff, 1)
	require.NoError(t, err)
	require.Len(t, limited, 1)
	assert.Equal(t, eligible.ID, limited[0].ID)
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

		t.Run("ClaimAgentStart, ClaimAgentStop, ClaimAgentReincarnation, and UpdateAgentDeletion invalidate claim", func(t *testing.T) {
			// ClaimAgentStart
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

			// ClaimAgentStop
			aStop, condStop := makeEligible("claim-stop-invalidates")
			_, intentAt, cuStop, err := s.ClaimIdleSuspend(ctx, aStop.ID, condStop)
			require.NoError(t, err)
			_, err = s.ClaimAgentStop(ctx, aStop.ID, "hub-1", intentAt, 30*time.Second)
			require.NoError(t, err)
			err = s.UpdateAgentStatus(ctx, aStop.ID, store.AgentStatusUpdate{
				Phase:               string(state.PhaseSuspended),
				SuspendReason:       string(state.SuspendReasonIdle),
				IfIdleSuspendCommit: &cuStop,
			})
			assert.ErrorIs(t, err, store.ErrIdleSuspendPrecondition)

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

		t.Run("expired claim lease and container limits_exceeded or non-zero exit stop are rejected", func(t *testing.T) {
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

			aLim, condLim := makeEligible("limits-stop")
			_, _, cuLim, err := s.ClaimIdleSuspend(ctx, aLim.ID, condLim)
			require.NoError(t, err)
			require.NoError(t, s.UpdateAgentStatus(ctx, aLim.ID, store.AgentStatusUpdate{
				Phase:              string(state.PhaseStopped),
				Activity:           string(state.ActivityLimitsExceeded),
				ReportSource:       store.StatusReportSourceAgent,
				ReportRunID:        aLim.RunID,
				ReportAgentToken:   true,
				GuardReincarnation: true,
			}))
			err = s.UpdateAgentStatus(ctx, aLim.ID, store.AgentStatusUpdate{
				Phase:               string(state.PhaseSuspended),
				SuspendReason:       string(state.SuspendReasonIdle),
				IfRunID:             aLim.RunID,
				IfIdleSuspendCommit: &cuLim,
			})
			assert.ErrorIs(t, err, store.ErrIdleSuspendPrecondition)

			aExit, condExit := makeEligible("nonzero-exit-stop")
			_, _, cuExit, err := s.ClaimIdleSuspend(ctx, aExit.ID, condExit)
			require.NoError(t, err)
			exitOne := 1
			require.NoError(t, s.UpdateAgentStatus(ctx, aExit.ID, store.AgentStatusUpdate{
				Phase:              string(state.PhaseStopped),
				ExitCode:           &exitOne,
				ReportSource:       store.StatusReportSourceRuntime,
				ReportRunID:        aExit.RunID,
				GuardReincarnation: true,
			}))
			err = s.UpdateAgentStatus(ctx, aExit.ID, store.AgentStatusUpdate{
				Phase:               string(state.PhaseSuspended),
				SuspendReason:       string(state.SuspendReasonIdle),
				IfRunID:             aExit.RunID,
				IfIdleSuspendCommit: &cuExit,
			})
			assert.ErrorIs(t, err, store.ErrIdleSuspendPrecondition)
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

		cond.ClaimTTL = 25 * time.Millisecond
		_, _, _, err = s.ClaimIdleSuspend(ctx, a.ID, cond)
		require.NoError(t, err)

		duringByID, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.True(t, duringByID.IdleSuspendClaimActive)
		duringBySlug, err := s.GetAgentBySlug(ctx, a.ProjectID, a.Slug)
		require.NoError(t, err)
		assert.True(t, duringBySlug.IdleSuspendClaimActive)

		time.Sleep(35 * time.Millisecond)
		expiredByID, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.False(t, expiredByID.IdleSuspendClaimActive)
		expiredBySlug, err := s.GetAgentBySlug(ctx, a.ProjectID, a.Slug)
		require.NoError(t, err)
		assert.False(t, expiredBySlug.IdleSuspendClaimActive)
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
}
