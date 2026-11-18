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
			_, err = s.client.Agent.UpdateOneID(uuid.MustParse(a.ID)).SetIdleSuspendClaimUntil(until).Save(ctx)
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
}
