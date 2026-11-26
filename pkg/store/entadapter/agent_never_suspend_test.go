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
	"database/sql"
	"testing"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAgentStore_SuspendPolicy_CreateAndUpdatePin(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	t.Run("default is auto", func(t *testing.T) {
		a := makeAgent(projectID, "sp-default")
		require.NoError(t, s.CreateAgent(ctx, a))
		assert.Equal(t, store.SuspendPolicyAuto, a.SuspendPolicy)

		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, store.SuspendPolicyAuto, got.SuspendPolicy)
	})

	t.Run("explicit never persists", func(t *testing.T) {
		a := makeAgent(projectID, "sp-never")
		a.SuspendPolicy = store.SuspendPolicyNever
		require.NoError(t, s.CreateAgent(ctx, a))

		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, store.SuspendPolicyNever, got.SuspendPolicy)
	})

	t.Run("invalid value rejected on create", func(t *testing.T) {
		a := makeAgent(projectID, "sp-invalid")
		a.SuspendPolicy = "bogus"
		err := s.CreateAgent(ctx, a)
		assert.ErrorIs(t, err, store.ErrInvalidInput)
	})

	// Acceptance §9 item 9: generic UpdateAgent / buildAgentUpdate must not
	// clobber suspend_policy from a stale whole-row struct.
	t.Run("UpdateAgent never overwrites suspend_policy", func(t *testing.T) {
		neverAgent := makeAgent(projectID, "sp-pin-never")
		neverAgent.SuspendPolicy = store.SuspendPolicyNever
		require.NoError(t, s.CreateAgent(ctx, neverAgent))

		stale := *neverAgent
		stale.SuspendPolicy = store.SuspendPolicyAuto
		stale.TaskSummary = "updated-summary"
		require.NoError(t, s.UpdateAgent(ctx, &stale))

		got, err := s.GetAgent(ctx, neverAgent.ID)
		require.NoError(t, err)
		assert.Equal(t, "updated-summary", got.TaskSummary)
		assert.Equal(t, store.SuspendPolicyNever, got.SuspendPolicy, "UpdateAgent must not overwrite suspend_policy=never with auto")

		staleEmpty := *got
		staleEmpty.SuspendPolicy = ""
		staleEmpty.TaskSummary = "updated-again"
		require.NoError(t, s.UpdateAgent(ctx, &staleEmpty))

		got2, err := s.GetAgent(ctx, neverAgent.ID)
		require.NoError(t, err)
		assert.Equal(t, store.SuspendPolicyNever, got2.SuspendPolicy, "UpdateAgent must not overwrite suspend_policy=never with empty")

		autoAgent := makeAgent(projectID, "sp-pin-auto")
		require.NoError(t, s.CreateAgent(ctx, autoAgent))
		stalePromote := *autoAgent
		stalePromote.SuspendPolicy = store.SuspendPolicyNever
		require.NoError(t, s.UpdateAgent(ctx, &stalePromote))

		gotAuto, err := s.GetAgent(ctx, autoAgent.ID)
		require.NoError(t, err)
		assert.Equal(t, store.SuspendPolicyAuto, gotAuto.SuspendPolicy, "UpdateAgent must not promote suspend_policy=auto to never")
	})
}

func TestAgentStore_ListIdleSuspendCandidates_SuspendPolicy(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	now := time.Now().UTC()
	cutoff := now.Add(-15 * time.Minute)
	heartbeatRecency := now.Add(-2 * time.Minute)
	liveWorkCutoff := now.Add(-store.LiveWorkFreshness)
	past := now.Add(-20 * time.Minute)

	makeEligible := func(slug, policy string) *store.Agent {
		a := makeAgent(projectID, slug)
		a.Phase = string(state.PhaseRunning)
		a.Activity = string(state.ActivityIdle)
		a.LastSeen = now.Add(-30 * time.Second)
		a.SuspendPolicy = policy
		require.NoError(t, s.CreateAgent(ctx, a))
		_, err := s.client.Agent.UpdateOneID(uuid.MustParse(a.ID)).
			SetActivitySince(past).
			Save(ctx)
		require.NoError(t, err)
		return a
	}

	autoAgent := makeEligible("sp-cand-auto", store.SuspendPolicyAuto)
	neverAgent := makeEligible("sp-cand-never", store.SuspendPolicyNever)

	// Experiment on (excludeNeverSuspend=true): only autoAgent is returned.
	gotOn, err := s.ListIdleSuspendCandidates(ctx, cutoff, heartbeatRecency, liveWorkCutoff, time.Time{}, nil, 0, true)
	require.NoError(t, err)
	require.Len(t, gotOn, 1)
	assert.Equal(t, autoAgent.ID, gotOn[0].ID)

	// Experiment off (excludeNeverSuspend=false): both are returned.
	gotOff, err := s.ListIdleSuspendCandidates(ctx, cutoff, heartbeatRecency, liveWorkCutoff, time.Time{}, nil, 0, false)
	require.NoError(t, err)
	require.Len(t, gotOff, 2)
	ids := map[string]bool{gotOff[0].ID: true, gotOff[1].ID: true}
	assert.True(t, ids[autoAgent.ID])
	assert.True(t, ids[neverAgent.ID])
}

func TestAgentStore_ClaimIdleSuspend_AndSwapAutoSuspend_SuspendPolicy(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	now := time.Now().UTC()
	past := now.Add(-20 * time.Minute)

	makeEligible := func(slug, policy string) (*store.Agent, store.IdleSuspendCondition) {
		a := makeAgent(projectID, slug)
		a.Phase = string(state.PhaseRunning)
		a.Activity = string(state.ActivityIdle)
		a.LastSeen = now.Add(-30 * time.Second)
		a.SuspendPolicy = policy
		require.NoError(t, s.CreateAgent(ctx, a))
		_, err := s.SetRunIntent(ctx, a.ID, store.RunIntentRunning)
		require.NoError(t, err)
		_, err = s.client.Agent.UpdateOneID(uuid.MustParse(a.ID)).
			SetRunID("run-" + slug).
			SetActivitySince(past).
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
			RequireSuspendPolicyAuto: true,
		}
	}

	t.Run("ClaimIdleSuspend refuses never-suspend when RequireSuspendPolicyAuto=true", func(t *testing.T) {
		a, cond := makeEligible("sp-claim-never-on", store.SuspendPolicyNever)
		_, _, _, err := s.ClaimIdleSuspend(ctx, a.ID, cond)
		assert.ErrorIs(t, err, store.ErrIdleSuspendPrecondition)

		after, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, store.RunIntentRunning, after.RunIntent)
		assert.Nil(t, after.IdleSuspendClaimUntil)
	})

	t.Run("ClaimIdleSuspend allows never-suspend when RequireSuspendPolicyAuto=false (experiment off)", func(t *testing.T) {
		a, cond := makeEligible("sp-claim-never-off", store.SuspendPolicyNever)
		cond.RequireSuspendPolicyAuto = false
		_, _, claimUntil, err := s.ClaimIdleSuspend(ctx, a.ID, cond)
		require.NoError(t, err)
		assert.False(t, claimUntil.IsZero())
	})

	t.Run("race rule: flag committed before claim blocks ClaimIdleSuspend; claim taken first is not cancelled", func(t *testing.T) {
		// Case A: flag flips to never before ClaimIdleSuspend commits -> blocked.
		aBefore, condBefore := makeEligible("sp-race-before", store.SuspendPolicyAuto)
		_, err := s.client.Agent.UpdateOneID(uuid.MustParse(aBefore.ID)).
			SetSuspendPolicy(agent.SuspendPolicyNever).
			Save(ctx)
		require.NoError(t, err)
		_, _, _, err = s.ClaimIdleSuspend(ctx, aBefore.ID, condBefore)
		assert.ErrorIs(t, err, store.ErrIdleSuspendPrecondition)

		// Case B: ClaimIdleSuspend commits first, then flag flips to never ->
		// claim remains active and IfIdleSuspendCommit still settles suspended(idle).
		aAfter, condAfter := makeEligible("sp-race-after", store.SuspendPolicyAuto)
		_, _, claimUntil, err := s.ClaimIdleSuspend(ctx, aAfter.ID, condAfter)
		require.NoError(t, err)
		_, err = s.client.Agent.UpdateOneID(uuid.MustParse(aAfter.ID)).
			SetSuspendPolicy(agent.SuspendPolicyNever).
			Save(ctx)
		require.NoError(t, err)

		err = s.UpdateAgentStatus(ctx, aAfter.ID, store.AgentStatusUpdate{
			Phase:               string(state.PhaseSuspended),
			SuspendReason:       string(state.SuspendReasonIdle),
			IfIdleSuspendCommit: &claimUntil,
		})
		require.NoError(t, err)
		settled, err := s.GetAgent(ctx, aAfter.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseSuspended), settled.Phase)
		assert.Equal(t, string(state.SuspendReasonIdle), settled.SuspendReason)
		assert.Equal(t, store.SuspendPolicyNever, settled.SuspendPolicy)
	})

	t.Run("SwapAutoSuspendRunIntent checks suspend_policy while SwapSystemRunIntent does not", func(t *testing.T) {
		a, _ := makeEligible("sp-swap-never", store.SuspendPolicyNever)

		// With requireSuspendPolicyAuto=true: refused with ErrAgentNeverSuspend.
		_, _, err := s.SwapAutoSuspendRunIntent(ctx, a.ID, store.RunIntentStopped, true)
		assert.ErrorIs(t, err, store.ErrAgentNeverSuspend)
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, store.RunIntentRunning, got.RunIntent)

		// With requireSuspendPolicyAuto=false (experiment off): succeeds.
		_, _, err = s.SwapAutoSuspendRunIntent(ctx, a.ID, store.RunIntentStopped, false)
		require.NoError(t, err)

		// SwapSystemRunIntent (membership-loss hold) always succeeds on never-suspend agents.
		b, _ := makeEligible("sp-swap-hold", store.SuspendPolicyNever)
		_, _, err = s.SwapSystemRunIntent(ctx, b.ID, store.RunIntentStopped)
		require.NoError(t, err)
		gotB, err := s.GetAgent(ctx, b.ID)
		require.NoError(t, err)
		assert.Equal(t, store.RunIntentStopped, gotB.RunIntent)
	})

	t.Run("non-enum suspend_policy value is not exempt (only explicit never is exempt)", func(t *testing.T) {
		forceEmptyPolicy := func(id string) {
			query, args := entsql.Dialect(s.dialect()).
				Update(agent.Table).
				Set(agent.FieldSuspendPolicy, "").
				Where(entsql.EQ(agent.FieldID, id)).
				Query()
			var res sql.Result
			require.NoError(t, s.client.Driver().Exec(ctx, query, args, &res))
		}

		// 1. Idle candidate query includes non-enum row when excludeNeverSuspend=true.
		a, cond := makeEligible("sp-nonenum-idle", store.SuspendPolicyAuto)
		forceEmptyPolicy(a.ID)

		cutoff := now.Add(-15 * time.Minute)
		heartbeatRecency := now.Add(-2 * time.Minute)
		liveWorkCutoff := now.Add(-store.LiveWorkFreshness)
		cands, err := s.ListIdleSuspendCandidates(ctx, cutoff, heartbeatRecency, liveWorkCutoff, time.Time{}, nil, 0, true)
		require.NoError(t, err)
		var found bool
		for _, c := range cands {
			if c.ID == a.ID {
				found = true
				break
			}
		}
		assert.True(t, found, "non-enum suspend_policy row must remain an idle candidate")

		// 2. ClaimIdleSuspend succeeds on non-enum row when RequireSuspendPolicyAuto=true.
		_, _, claimUntil, err := s.ClaimIdleSuspend(ctx, a.ID, cond)
		require.NoError(t, err)
		assert.False(t, claimUntil.IsZero())

		// 3. SwapAutoSuspendRunIntent succeeds on non-enum row when requireSuspendPolicyAuto=true.
		b, _ := makeEligible("sp-nonenum-stalled", store.SuspendPolicyAuto)
		forceEmptyPolicy(b.ID)
		_, _, err = s.SwapAutoSuspendRunIntent(ctx, b.ID, store.RunIntentStopped, true)
		require.NoError(t, err)
		gotB, err := s.GetAgent(ctx, b.ID)
		require.NoError(t, err)
		assert.Equal(t, store.RunIntentStopped, gotB.RunIntent)
	})
}

func TestAgentStore_SetAgentSuspendPolicy_RunningCountsAndFilter(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	t.Run("SetAgentSuspendPolicy toggles without bumping state_version and validates input", func(t *testing.T) {
		a := makeAgent(projectID, "sp-set-toggle")
		require.NoError(t, s.CreateAgent(ctx, a))
		before, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, store.SuspendPolicyAuto, before.SuspendPolicy)

		require.NoError(t, s.SetAgentSuspendPolicy(ctx, a.ID, store.SuspendPolicyNever))
		afterNever, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, store.SuspendPolicyNever, afterNever.SuspendPolicy)
		assert.Equal(t, before.StateVersion, afterNever.StateVersion, "SetAgentSuspendPolicy must not bump state_version")

		require.NoError(t, s.SetAgentSuspendPolicy(ctx, a.ID, store.SuspendPolicyAuto))
		afterAuto, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, store.SuspendPolicyAuto, afterAuto.SuspendPolicy)
		assert.Equal(t, before.StateVersion, afterAuto.StateVersion)

		assert.ErrorIs(t, s.SetAgentSuspendPolicy(ctx, a.ID, "bogus"), store.ErrInvalidInput)
		assert.ErrorIs(t, s.SetAgentSuspendPolicy(ctx, a.ID, ""), store.ErrInvalidInput)
		assert.ErrorIs(t, s.SetAgentSuspendPolicy(ctx, uuid.New().String(), store.SuspendPolicyNever), store.ErrNotFound)

		// Soft-deleted row must not be written by SetAgentSuspendPolicy.
		_, err = s.client.Agent.UpdateOneID(uuid.MustParse(a.ID)).
			SetDeletedAt(time.Now().UTC()).
			Save(ctx)
		require.NoError(t, err)
		assert.ErrorIs(t, s.SetAgentSuspendPolicy(ctx, a.ID, store.SuspendPolicyNever), store.ErrNotFound)
		afterDeleted, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, store.SuspendPolicyAuto, afterDeleted.SuspendPolicy, "soft-deleted row policy must remain unchanged")
	})

	t.Run("CountRunningNeverSuspendAgents and QueryTerms filter", func(t *testing.T) {
		running := makeAgent(projectID, "sp-cnt-running")
		running.Phase = string(state.PhaseRunning)
		running.SuspendPolicy = store.SuspendPolicyNever
		running.RuntimeBrokerID = "broker-a"
		require.NoError(t, s.CreateAgent(ctx, running))

		stopped := makeAgent(projectID, "sp-cnt-stopped")
		stopped.Phase = string(state.PhaseStopped)
		stopped.SuspendPolicy = store.SuspendPolicyNever
		stopped.RuntimeBrokerID = "broker-a"
		require.NoError(t, s.CreateAgent(ctx, stopped))

		autoRunning := makeAgent(projectID, "sp-cnt-auto")
		autoRunning.Phase = string(state.PhaseRunning)
		autoRunning.SuspendPolicy = store.SuspendPolicyAuto
		autoRunning.RuntimeBrokerID = "broker-a"
		require.NoError(t, s.CreateAgent(ctx, autoRunning))

		deletedNever := makeAgent(projectID, "sp-cnt-deleted")
		deletedNever.Phase = string(state.PhaseRunning)
		deletedNever.SuspendPolicy = store.SuspendPolicyNever
		deletedNever.RuntimeBrokerID = "broker-a"
		require.NoError(t, s.CreateAgent(ctx, deletedNever))
		_, err := s.client.Agent.UpdateOneID(uuid.MustParse(deletedNever.ID)).
			SetDeletedAt(time.Now().UTC()).
			Save(ctx)
		require.NoError(t, err)

		// CountRunningNeverSuspendAgents counts only non-deleted running flagged agents.
		runningCounts, err := s.CountRunningNeverSuspendAgents(ctx)
		require.NoError(t, err)
		require.Len(t, runningCounts, 1)
		assert.Equal(t, projectID, runningCounts[0].ProjectID)
		assert.Equal(t, "broker-a", runningCounts[0].RuntimeBrokerID)
		assert.Equal(t, int64(1), runningCounts[0].Count)

		// QueryTerms filter on suspendPolicy.
		resNever, err := s.ListAgents(ctx, store.AgentFilter{
			ProjectID: projectID,
			Query: &store.QueryTerms{
				Terms: []store.QueryTerm{{Field: "suspendpolicy", Values: []string{store.SuspendPolicyNever}}},
			},
		}, store.ListOptions{})
		require.NoError(t, err)
		gotNeverIDs := make([]string, len(resNever.Items))
		for i, item := range resNever.Items {
			gotNeverIDs[i] = item.ID
		}
		assert.ElementsMatch(t, []string{running.ID, stopped.ID}, gotNeverIDs)

		resNotNever, err := s.ListAgents(ctx, store.AgentFilter{
			ProjectID: projectID,
			Query: &store.QueryTerms{
				Terms: []store.QueryTerm{{Field: "suspendpolicy", Values: []string{store.SuspendPolicyNever}, Negate: true}},
			},
		}, store.ListOptions{})
		require.NoError(t, err)
		for _, item := range resNotNever.Items {
			assert.Equal(t, store.SuspendPolicyAuto, item.SuspendPolicy)
		}
	})
}
