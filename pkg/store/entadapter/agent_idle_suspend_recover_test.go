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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAgentStore_IdleSuspendRecover covers the recovery write
// (AgentStatusUpdate.IfIdleSuspendRecover) and ListExpiredIdleSuspendClaims
// (ptone/scion#8204).
func TestAgentStore_IdleSuspendRecover(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	now := time.Now().UTC()
	idleSince := now.Add(-20 * time.Minute).Truncate(time.Microsecond)

	runIDs := map[string]string{}
	// claimed returns an agent holding an idle claim, expired when expire.
	claimed := func(t *testing.T, slug string, expire bool) (*store.Agent, time.Time) {
		t.Helper()
		a := makeAgent(projectID, slug)
		a.Phase = string(state.PhaseRunning)
		a.Activity = string(state.ActivityIdle)
		a.RunID = "run-1"
		a.LastSeen = now.Add(-15 * time.Second)
		require.NoError(t, s.CreateAgent(ctx, a))
		_, err := s.client.Agent.UpdateOneID(uuid.MustParse(a.ID)).
			SetRunIntent(string(store.RunIntentRunning)).
			SetRunID("run-1").
			SetRunIntentAt(now.Add(-30 * time.Minute)).
			SetActivitySince(idleSince).
			Save(ctx)
		require.NoError(t, err)
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		_, _, until, err := s.ClaimIdleSuspend(ctx, a.ID, store.IdleSuspendCondition{
			RunID: got.RunID, Activity: got.Activity, ActivitySince: *got.ActivitySince,
			IdleSuspendAfter: 15 * time.Minute, HeartbeatRecency: 2 * time.Minute, ClaimTTL: time.Minute,
		})
		require.NoError(t, err)
		if expire {
			until = time.Now().UTC().Add(-time.Second).Truncate(time.Microsecond)
			_, err = s.client.Agent.UpdateOneID(uuid.MustParse(a.ID)).SetIdleSuspendClaimUntil(until).SetRunIntentAt(until.Add(-time.Minute)).SetRunIntentMarkedAt(until.Add(-time.Minute)).Save(ctx)
			require.NoError(t, err)
		}
		runIDs[a.ID] = got.RunID
		return a, until
	}
	setPhase := func(t *testing.T, id, phase string) {
		t.Helper()
		_, err := s.client.Agent.UpdateOneID(uuid.MustParse(id)).SetPhase(phase).Save(ctx)
		require.NoError(t, err)
	}
	recoverUpdate := func(id string, until time.Time) store.AgentStatusUpdate {
		return store.AgentStatusUpdate{
			Phase:                string(state.PhaseSuspended),
			ContainerStatus:      "stopped",
			SuspendReason:        string(state.SuspendReasonIdle),
			IfRunID:              runIDs[id],
			IfIdleSuspendRecover: &until,
		}
	}
	listed := func(t *testing.T, id string) bool {
		t.Helper()
		rows, err := s.ListExpiredIdleSuspendClaims(ctx, 0)
		require.NoError(t, err)
		for _, r := range rows {
			if r.ID == id {
				return true
			}
		}
		return false
	}

	t.Run("expired claim on a stopped row is recorded and the claim cleared", func(t *testing.T) {
		a, until := claimed(t, "rec-ok", true)
		setPhase(t, a.ID, string(state.PhaseStopped))
		assert.True(t, listed(t, a.ID))
		require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, recoverUpdate(a.ID, until)))
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseSuspended), got.Phase)
		assert.Equal(t, string(state.SuspendReasonIdle), got.SuspendReason)
		assert.Nil(t, got.IdleSuspendClaimUntil)
		assert.False(t, listed(t, a.ID))
	})

	t.Run("stopping and error rows are recovered, a running row is not", func(t *testing.T) {
		for _, ph := range []state.Phase{state.PhaseStopping, state.PhaseError} {
			a, until := claimed(t, "rec-"+string(ph), true)
			setPhase(t, a.ID, string(ph))
			require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, recoverUpdate(a.ID, until)), ph)
		}
		a, until := claimed(t, "rec-running", true)
		assert.False(t, listed(t, a.ID))
		assert.ErrorIs(t, s.UpdateAgentStatus(ctx, a.ID, recoverUpdate(a.ID, until)), store.ErrIdleSuspendPrecondition)
	})

	t.Run("an unexpired claim is never recovered", func(t *testing.T) {
		a, until := claimed(t, "rec-active", false)
		setPhase(t, a.ID, string(state.PhaseStopped))
		assert.False(t, listed(t, a.ID))
		assert.ErrorIs(t, s.UpdateAgentStatus(ctx, a.ID, recoverUpdate(a.ID, until)), store.ErrIdleSuspendPrecondition)
	})

	t.Run("a user stop in the window clears the claim and is never converted", func(t *testing.T) {
		a, until := claimed(t, "rec-userstop", true)
		setPhase(t, a.ID, string(state.PhaseStopped))
		require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{
			Phase: string(state.PhaseStopped), ContainerStatus: "stopped",
		}))
		assert.False(t, listed(t, a.ID))
		assert.ErrorIs(t, s.UpdateAgentStatus(ctx, a.ID, recoverUpdate(a.ID, until)), store.ErrIdleSuspendPrecondition)
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseStopped), got.Phase)
		assert.Empty(t, got.SuspendReason)
	})

	t.Run("a new run, a deletion, a reincarnation or a different stamp is skipped", func(t *testing.T) {
		cases := map[string]func(id string) error{
			"new run": func(id string) error {
				_, err := s.client.Agent.UpdateOneID(uuid.MustParse(id)).SetRunID("run-2").Save(ctx)
				return err
			},
			"deleted": func(id string) error {
				_, err := s.client.Agent.UpdateOneID(uuid.MustParse(id)).SetDeletedAt(time.Now()).Save(ctx)
				return err
			},
			"reincarnation": func(id string) error {
				_, err := s.client.Agent.UpdateOneID(uuid.MustParse(id)).SetReincarnationState(store.ReincarnationStateStopping).Save(ctx)
				return err
			},
			"intent running": func(id string) error {
				_, err := s.client.Agent.UpdateOneID(uuid.MustParse(id)).SetRunIntent(string(store.RunIntentRunning)).Save(ctx)
				return err
			},
		}
		for name, mutate := range cases {
			a, until := claimed(t, "rec-guard-"+name, true)
			setPhase(t, a.ID, string(state.PhaseStopped))
			require.NoError(t, mutate(a.ID), name)
			err := s.UpdateAgentStatus(ctx, a.ID, recoverUpdate(a.ID, until))
			assert.Error(t, err, name)
		}
		a, until := claimed(t, "rec-stamp", true)
		setPhase(t, a.ID, string(state.PhaseStopped))
		assert.ErrorIs(t, s.UpdateAgentStatus(ctx, a.ID, recoverUpdate(a.ID, until.Add(time.Hour))), store.ErrIdleSuspendPrecondition)
	})

	t.Run("a second recovery sees the cleared claim", func(t *testing.T) {
		a, until := claimed(t, "rec-twice", true)
		setPhase(t, a.ID, string(state.PhaseStopped))
		require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, recoverUpdate(a.ID, until)))
		assert.ErrorIs(t, s.UpdateAgentStatus(ctx, a.ID, recoverUpdate(a.ID, until)), store.ErrIdleSuspendPrecondition)
	})

	t.Run("delivery sees an expired claim with a stopped intent as in progress", func(t *testing.T) {
		a, _ := claimed(t, "rec-delivery", true)
		marked, inProgress, err := s.MarkAgentDelivery(ctx, a.ID)
		require.NoError(t, err)
		assert.True(t, inProgress)
		assert.True(t, marked.IdleSuspendClaimActive)
		assert.False(t, marked.IdleSuspendClaimLeaseActive)
		assert.Nil(t, marked.LastDeliveryAt)
		// Once recorded suspended, a stale stamp is no longer in progress.
		setPhase(t, a.ID, string(state.PhaseSuspended))
		_, inProgress, err = s.MarkAgentDelivery(ctx, a.ID)
		require.NoError(t, err)
		assert.False(t, inProgress)
	})

	t.Run("a membership hold records suspended with no suspend reason", func(t *testing.T) {
		a, until := claimed(t, "rec-hold", true)
		setPhase(t, a.ID, string(state.PhaseStopped))
		cs := NewCompositeStore(s.client)
		_, err := cs.CreateAgentHolds(ctx, []*store.AgentHold{{
			AgentID: a.ID, ProjectID: projectID, Cause: store.AgentHoldCauseOwnerAccessEnded,
			RootPrincipalType: store.AgentHoldRootUser, RootPrincipalID: uuid.NewString(),
			Trigger: store.MembershipLossTriggerMemberRemove,
		}})
		require.NoError(t, err)
		require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, recoverUpdate(a.ID, until)))
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseSuspended), got.Phase)
		assert.Empty(t, got.SuspendReason, "a held agent never carries suspend_reason=idle")
		assert.Nil(t, got.IdleSuspendClaimUntil)
	})

	t.Run("a held start claim rejects recovery until it is released", func(t *testing.T) {
		a, until := claimed(t, "rec-startclaim", true)
		setPhase(t, a.ID, string(state.PhaseStopped))
		row, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		require.NotNil(t, row.RunIntentAt)
		// The intent-stop backstop's stop claim yields to nothing once the lease expired.
		_, err = s.ClaimAgentStop(ctx, a.ID, "backstop", *row.RunIntentAt, time.Minute)
		require.NoError(t, err)
		assert.ErrorIs(t, s.UpdateAgentStatus(ctx, a.ID, recoverUpdate(a.ID, until)), store.ErrIdleSuspendPrecondition)
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseStopped), got.Phase)

		_, err = clearStartClaim(s.client.Agent.UpdateOneID(uuid.MustParse(a.ID))).Save(ctx)
		require.NoError(t, err)
		require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, recoverUpdate(a.ID, until)))
	})

	t.Run("abandon reverts a running row only past the ceiling and under the guards", func(t *testing.T) {
		ceiling := time.Hour
		past := func(t *testing.T, id string, ago time.Duration) time.Time {
			t.Helper()
			until := time.Now().UTC().Add(-ago).Truncate(time.Microsecond)
			_, err := s.client.Agent.UpdateOneID(uuid.MustParse(id)).SetIdleSuspendClaimUntil(until).SetRunIntentAt(until.Add(-time.Minute)).SetRunIntentMarkedAt(until.Add(-time.Minute)).Save(ctx)
			require.NoError(t, err)
			return until
		}
		a, _ := claimed(t, "ab-ok", true)
		until := past(t, a.ID, 2*time.Hour)
		rows, err := s.ListAbandonedIdleSuspendClaims(ctx, ceiling, 0)
		require.NoError(t, err)
		found := false
		for _, r := range rows {
			found = found || r.ID == a.ID
		}
		assert.True(t, found)
		rowA, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		require.NoError(t, s.AbandonIdleSuspend(ctx, a.ID, until, runIDs[a.ID], *rowA.RunIntentAt, ceiling, "abandoned"))
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, store.RunIntentRunning, got.RunIntent)
		require.NotNil(t, got.IdleSuspendClaimUntil, "the stamp stays as the backoff marker")
		assert.False(t, got.IdleSuspendClaimActive, "intent running: not in progress")
		assert.Equal(t, "abandoned", got.Message)

		// The marker keeps the row out of the candidate query and ClaimIdleSuspend.
		cands, err := s.ListIdleSuspendCandidates(ctx, time.Now().Add(time.Hour), time.Now().Add(-time.Hour), time.Now(), time.Now().Add(-3*time.Hour), nil, 0)
		require.NoError(t, err)
		for _, c := range cands {
			assert.NotEqual(t, a.ID, c.ID, "backoff: not a candidate")
		}
		assert.Equal(t, string(state.PhaseRunning), got.Phase)

		young, _ := claimed(t, "ab-young", true)
		yu := past(t, young.ID, time.Minute)
		assert.ErrorIs(t, s.AbandonIdleSuspend(ctx, young.ID, yu, runIDs[young.ID], time.Now(), ceiling, "x"), store.ErrIdleSuspendPrecondition)

		stamp, _ := claimed(t, "ab-stamp", true)
		su := past(t, stamp.ID, 2*time.Hour)
		assert.ErrorIs(t, s.AbandonIdleSuspend(ctx, stamp.ID, su.Add(time.Hour), runIDs[stamp.ID], time.Now(), ceiling, "x"), store.ErrIdleSuspendPrecondition)
		assert.ErrorIs(t, s.AbandonIdleSuspend(ctx, stamp.ID, su, "other-run", time.Now(), ceiling, "x"), store.ErrIdleSuspendPrecondition)

		// No heartbeat since the lease expired: the container may already be gone.
		quiet, _ := claimed(t, "ab-quiet", true)
		qu := past(t, quiet.ID, 2*time.Hour)
		_, err = s.client.Agent.UpdateOneID(uuid.MustParse(quiet.ID)).SetLastSeen(qu.Add(-time.Minute)).Save(ctx)
		require.NoError(t, err)
		assert.ErrorIs(t, s.AbandonIdleSuspend(ctx, quiet.ID, qu, runIDs[quiet.ID], time.Now(), ceiling, "x"), store.ErrIdleSuspendPrecondition)
		abandonable, err := s.ListAbandonedIdleSuspendClaims(ctx, ceiling, 0)
		require.NoError(t, err)
		for _, r := range abandonable {
			assert.NotEqual(t, quiet.ID, r.ID, "a row with a stale last_seen is not abandoned")
		}

		stopped, _ := claimed(t, "ab-stopped", true)
		stu := past(t, stopped.ID, 2*time.Hour)
		setPhase(t, stopped.ID, string(state.PhaseStopped))
		assert.ErrorIs(t, s.AbandonIdleSuspend(ctx, stopped.ID, stu, runIDs[stopped.ID], time.Now(), ceiling, "x"), store.ErrIdleSuspendPrecondition)

		held, _ := claimed(t, "ab-claim", true)
		hu := past(t, held.ID, 2*time.Hour)
		row, err := s.GetAgent(ctx, held.ID)
		require.NoError(t, err)
		_, err = s.ClaimAgentStop(ctx, held.ID, "backstop", *row.RunIntentAt, time.Minute)
		require.NoError(t, err)
		assert.ErrorIs(t, s.AbandonIdleSuspend(ctx, held.ID, hu, runIDs[held.ID], *row.RunIntentAt, ceiling, "x"), store.ErrIdleSuspendPrecondition)

		userStopped, _ := claimed(t, "ab-userstop", true)
		uu := past(t, userStopped.ID, 2*time.Hour)
		require.NoError(t, s.UpdateAgentStatus(ctx, userStopped.ID, store.AgentStatusUpdate{Phase: string(state.PhaseRunning)}))
		assert.ErrorIs(t, s.AbandonIdleSuspend(ctx, userStopped.ID, uu, runIDs[userStopped.ID], time.Now(), ceiling, "x"), store.ErrIdleSuspendPrecondition, "a hub write cleared the stamp")
	})

	t.Run("ClaimIdleSuspend honours the backoff marker", func(t *testing.T) {
		a := makeAgent(projectID, "claim-backoff")
		a.Phase = string(state.PhaseRunning)
		a.Activity = string(state.ActivityIdle)
		a.RunID = "run-b"
		a.LastSeen = now.Add(-15 * time.Second)
		require.NoError(t, s.CreateAgent(ctx, a))
		_, err := s.client.Agent.UpdateOneID(uuid.MustParse(a.ID)).
			SetRunIntent(string(store.RunIntentRunning)).SetRunIntentAt(now.Add(-time.Hour)).
			SetActivitySince(idleSince).SetIdleSuspendClaimUntil(time.Now().UTC().Add(-time.Minute)).Save(ctx)
		require.NoError(t, err)
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		cond := store.IdleSuspendCondition{RunID: got.RunID, Activity: got.Activity, ActivitySince: *got.ActivitySince,
			IdleSuspendAfter: 15 * time.Minute, HeartbeatRecency: 2 * time.Minute, ClaimTTL: time.Minute,
			ClaimBackoff: time.Hour}
		_, _, _, err = s.ClaimIdleSuspend(ctx, a.ID, cond)
		assert.ErrorIs(t, err, store.ErrIdleSuspendPrecondition, "stamp newer than the backoff")
		cond.ClaimBackoff = 30 * time.Second
		_, _, _, err = s.ClaimIdleSuspend(ctx, a.ID, cond)
		assert.NoError(t, err, "backoff passed")
	})

	t.Run("a newer system stop intent is not the claim's: not listed, recovered, abandoned or in progress", func(t *testing.T) {
		a, until := claimed(t, "rec-notours", true)
		setPhase(t, a.ID, string(state.PhaseStopped))
		// A system writer (membership hold, stalled auto-suspend) writes its own
		// stopped intent after the lease expired; the expired stamp stays.
		_, at, err := s.SwapSystemRunIntent(ctx, a.ID, store.RunIntentStopped)
		require.NoError(t, err)
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		require.NotNil(t, got.IdleSuspendClaimUntil)
		require.True(t, got.RunIntentAt.After(*got.IdleSuspendClaimUntil))
		assert.False(t, got.IdleSuspendClaimActive)
		assert.False(t, listed(t, a.ID))
		assert.ErrorIs(t, s.UpdateAgentStatus(ctx, a.ID, recoverUpdate(a.ID, until)), store.ErrIdleSuspendPrecondition)

		setPhase(t, a.ID, string(state.PhaseRunning))
		assert.ErrorIs(t, s.AbandonIdleSuspend(ctx, a.ID, until, runIDs[a.ID], at, time.Nanosecond, "x"), store.ErrIdleSuspendPrecondition)
		abandoned, err := s.ListAbandonedIdleSuspendClaims(ctx, time.Nanosecond, 0)
		require.NoError(t, err)
		for _, r := range abandoned {
			assert.NotEqual(t, a.ID, r.ID)
		}
	})

	t.Run("a limit does not starve own-intent rows behind foreign ones", func(t *testing.T) {
		// Foreign row: oldest stamp, so it sorts first, but a system writer wrote a newer intent.
		foreign, _ := claimed(t, "lim-foreign", true)
		setPhase(t, foreign.ID, string(state.PhaseStopped))
		_, err := s.client.Agent.UpdateOneID(uuid.MustParse(foreign.ID)).
			SetIdleSuspendClaimUntil(time.Now().UTC().Add(-3 * time.Hour)).
			SetRunIntentAt(time.Now().UTC().Add(-3*time.Hour - time.Minute)).Save(ctx)
		require.NoError(t, err)
		_, _, err = s.SwapSystemRunIntent(ctx, foreign.ID, store.RunIntentStopped)
		require.NoError(t, err)

		own, _ := claimed(t, "lim-own", true)
		setPhase(t, own.ID, string(state.PhaseStopped))
		_, err = s.client.Agent.UpdateOneID(uuid.MustParse(own.ID)).
			SetIdleSuspendClaimUntil(time.Now().UTC().Add(-2 * time.Hour)).
			SetRunIntentAt(time.Now().UTC().Add(-2*time.Hour - time.Minute)).Save(ctx)
		require.NoError(t, err)

		rows, err := s.ListExpiredIdleSuspendClaims(ctx, 1)
		require.NoError(t, err)
		require.NotEmpty(t, rows)
		for _, r := range rows {
			assert.NotEqual(t, foreign.ID, r.ID, "the foreign-intent row is filtered in SQL, before the limit")
		}
		assert.Len(t, rows, 1)
	})
}
