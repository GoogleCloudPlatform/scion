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

// Run intent store tests. Named TestRunIntent_* so the Postgres CI job
// (make test-launch-store-postgres) runs them against Postgres as well as
// SQLite.
package entadapter

import (
	"context"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setStoredRunIntent writes run_intent_at directly (white-box setup).
func setStoredRunIntent(t *testing.T, ctx context.Context, s *AgentStore, agentID string, intent store.RunIntent, at time.Time) {
	t.Helper()
	uid, err := parseUUID(agentID)
	require.NoError(t, err)
	_, err = s.client.Agent.UpdateOneID(uid).SetRunIntent(string(intent)).SetRunIntentAt(at).Save(ctx)
	require.NoError(t, err)
}

func TestRunIntent_NewAgentHasNoIntent(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := makeAgent(projectID, "ri-new")
	a.RunIntent = store.RunIntentRunning
	now := time.Now()
	a.RunIntentAt = &now
	require.NoError(t, s.CreateAgent(ctx, a))

	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, store.RunIntent(""), got.RunIntent, "CreateAgent must not write run_intent")
	assert.Nil(t, got.RunIntentAt)
}

func TestRunIntent_SetIsMonotonicAndDoesNotBumpVersion(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := makeAgent(projectID, "ri-mono")
	require.NoError(t, s.CreateAgent(ctx, a))
	before, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)

	at1, err := s.SetRunIntent(ctx, a.ID, store.RunIntentRunning)
	require.NoError(t, err)
	at2, err := s.SetRunIntent(ctx, a.ID, store.RunIntentStopped)
	require.NoError(t, err)
	at3, err := s.SetRunIntent(ctx, a.ID, store.RunIntentRunning)
	require.NoError(t, err)
	assert.True(t, at2.After(at1), "at2 %v must be after at1 %v", at2, at1)
	assert.True(t, at3.After(at2), "at3 %v must be after at2 %v", at3, at2)

	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, store.RunIntentRunning, got.RunIntent)
	require.NotNil(t, got.RunIntentAt)
	assert.True(t, got.RunIntentMatches(store.RunIntentRunning, at3), "stored %v, returned %v", *got.RunIntentAt, at3)
	assert.Equal(t, before.StateVersion, got.StateVersion, "SetRunIntent must not bump state_version")
}

// A stored run_intent_at ahead of the store clock (a clock step backwards,
// or a failover to a node whose clock is behind) still yields a strictly
// larger value.
func TestRunIntent_SetWithClockBehindStoredStillIncreases(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := makeAgent(projectID, "ri-skew")
	require.NoError(t, s.CreateAgent(ctx, a))

	future := store.NormalizeRunIntentTime(time.Now().Add(time.Hour))
	setStoredRunIntent(t, ctx, s, a.ID, store.RunIntentRunning, future)

	at, err := s.SetRunIntent(ctx, a.ID, store.RunIntentStopped)
	require.NoError(t, err)
	assert.Equal(t, future.Add(time.Microsecond), at)

	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, store.RunIntentStopped, got.RunIntent)
	assert.True(t, got.RunIntentMatches(store.RunIntentStopped, at))
}

func TestRunIntent_SetRejectsUnknownAndMissing(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := makeAgent(projectID, "ri-bad")
	require.NoError(t, s.CreateAgent(ctx, a))

	_, err := s.SetRunIntent(ctx, a.ID, store.RunIntent("paused"))
	assert.ErrorIs(t, err, store.ErrInvalidInput)
	_, err = s.SetRunIntent(ctx, "7d1c2b9e-0000-4000-8000-000000000000", store.RunIntentStopped)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// UpdateAgent never reads or writes the intent columns, whatever the
// caller's struct carries, and an UpdateAgent after a SetRunIntent with the
// pre-intent state_version does not conflict.
func TestRunIntent_UpdateAgentNeverTouchesIntent(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := makeAgent(projectID, "ri-update")
	require.NoError(t, s.CreateAgent(ctx, a))
	loaded, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)

	at, err := s.SetRunIntent(ctx, a.ID, store.RunIntentStopped)
	require.NoError(t, err)

	// loaded predates the intent write and carries a different intent.
	loaded.RunIntent = store.RunIntentRunning
	other := time.Now().Add(-time.Hour)
	loaded.RunIntentAt = &other
	loaded.Message = "updated"
	require.NoError(t, s.UpdateAgent(ctx, loaded), "intent write must not cause a version conflict")

	// A running write takes UpdateAgent's other path; check it too.
	loaded.Phase = "running"
	loaded.RunIntent = ""
	loaded.RunIntentAt = nil
	require.NoError(t, s.UpdateAgent(ctx, loaded))

	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, "updated", got.Message)
	assert.True(t, got.RunIntentMatches(store.RunIntentStopped, at), "UpdateAgent changed run intent: %q %v", got.RunIntent, got.RunIntentAt)
}

func TestRunIntent_UpdateAgentStatusNeverTouchesIntent(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := makeAgent(projectID, "ri-status")
	require.NoError(t, s.CreateAgent(ctx, a))
	at, err := s.SetRunIntent(ctx, a.ID, store.RunIntentRunning)
	require.NoError(t, err)

	require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{Phase: "stopped", ContainerStatus: "stopped"}))

	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, "stopped", got.Phase)
	assert.True(t, got.RunIntentMatches(store.RunIntentRunning, at))
}

func TestRunIntent_SwapReturnsPriorIntent(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := makeAgent(projectID, "ri-swap")
	require.NoError(t, s.CreateAgent(ctx, a))

	prior, at1, err := s.SwapRunIntent(ctx, a.ID, store.RunIntentRunning)
	require.NoError(t, err)
	assert.Equal(t, store.RunIntent(""), prior, "no intent before the first write")

	prior, at2, err := s.SwapRunIntent(ctx, a.ID, store.RunIntentStopped)
	require.NoError(t, err)
	assert.Equal(t, store.RunIntentRunning, prior)
	assert.True(t, at2.After(at1))

	prior, _, err = s.SwapRunIntent(ctx, a.ID, store.RunIntentStopped)
	require.NoError(t, err)
	assert.Equal(t, store.RunIntentStopped, prior)

	_, _, err = s.SwapRunIntent(ctx, a.ID, store.RunIntent("paused"))
	assert.ErrorIs(t, err, store.ErrInvalidInput)
}

func TestRunIntent_RevertIsACompareAndSet(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := makeAgent(projectID, "ri-revert")
	require.NoError(t, s.CreateAgent(ctx, a))
	before, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)

	at, err := s.SetRunIntent(ctx, a.ID, store.RunIntentStopped)
	require.NoError(t, err)

	// Wrong time: no change.
	ok, err := s.RevertRunIntent(ctx, a.ID, store.RunIntentStopped, at.Add(-time.Microsecond), store.RunIntentRunning)
	require.NoError(t, err)
	assert.False(t, ok)
	// Wrong from: no change.
	ok, err = s.RevertRunIntent(ctx, a.ID, store.RunIntentRunning, at, store.RunIntentStopped)
	require.NoError(t, err)
	assert.False(t, ok)

	ok, err = s.RevertRunIntent(ctx, a.ID, store.RunIntentStopped, at, store.RunIntentRunning)
	require.NoError(t, err)
	assert.True(t, ok)
	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, store.RunIntentRunning, got.RunIntent)
	assert.True(t, got.RunIntentMatches(store.RunIntentRunning, at), "revert must leave run_intent_at unchanged")
	assert.Equal(t, before.StateVersion, got.StateVersion)

	// A newer intent write supersedes the one the revert was taken against.
	at2, err := s.SetRunIntent(ctx, a.ID, store.RunIntentStopped)
	require.NoError(t, err)
	ok, err = s.RevertRunIntent(ctx, a.ID, store.RunIntentStopped, at, store.RunIntentRunning)
	require.NoError(t, err)
	assert.False(t, ok, "an older revert must not undo a newer stop")
	got, err = s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.True(t, got.RunIntentMatches(store.RunIntentStopped, at2))
}

func TestRunIntent_BackfillMatrix(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	cases := []struct {
		slug    string
		phase   string
		want    store.RunIntent
		presets bool
	}{
		{slug: "bf-running", phase: "running", want: store.RunIntentRunning},
		{slug: "bf-starting", phase: "starting", want: store.RunIntentRunning},
		{slug: "bf-created", phase: "created", want: store.RunIntentStopped},
		{slug: "bf-provisioning", phase: "provisioning", want: store.RunIntentStopped},
		{slug: "bf-cloning", phase: "cloning", want: store.RunIntentStopped},
		{slug: "bf-stopping", phase: "stopping", want: store.RunIntentStopped},
		{slug: "bf-stopped", phase: "stopped", want: store.RunIntentStopped},
		{slug: "bf-suspended", phase: "suspended", want: store.RunIntentStopped},
		{slug: "bf-error", phase: "error", want: store.RunIntentStopped},
		// Already set: the backfill leaves it alone.
		{slug: "bf-preset", phase: "stopped", want: store.RunIntentRunning, presets: true},
	}
	ids := map[string]string{}
	var presetAt time.Time
	for _, c := range cases {
		a := makeAgent(projectID, c.slug)
		a.Phase = c.phase
		require.NoError(t, s.CreateAgent(ctx, a))
		ids[c.slug] = a.ID
		if c.presets {
			var err error
			presetAt, err = s.SetRunIntent(ctx, a.ID, store.RunIntentRunning)
			require.NoError(t, err)
		}
	}
	// An agent whose container was lost before the upgrade.
	// MarkAgentContainerMissing leaves phase error and container_status
	// missing; the row is created in that state directly.
	lost := makeAgent(projectID, "bf-missing")
	lost.Phase = "error"
	lost.ContainerStatus = containerMissingStatus
	require.NoError(t, s.CreateAgent(ctx, lost))

	n, err := s.BackfillRunIntent(ctx)
	require.NoError(t, err)
	assert.Equal(t, len(cases), n, "every NULL row is written once") // cases minus the preset row, plus the lost row

	for _, c := range cases {
		got, err := s.GetAgent(ctx, ids[c.slug])
		require.NoError(t, err)
		assert.Equal(t, c.want, got.RunIntent, c.slug)
		require.NotNil(t, got.RunIntentAt, c.slug)
		if c.presets {
			assert.True(t, got.RunIntentMatches(store.RunIntentRunning, presetAt), "backfill overwrote a set intent")
		}
	}
	gotLost, err := s.GetAgent(ctx, lost.ID)
	require.NoError(t, err)
	assert.Equal(t, store.RunIntentStopped, gotLost.RunIntent, "a pre-upgrade container_missing agent stays stopped")

	// Idempotent.
	n, err = s.BackfillRunIntent(ctx)
	require.NoError(t, err)
	assert.Zero(t, n)
}

func TestRunIntent_ListFilterOrRunIntent(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	mk := func(slug, phase string, intent store.RunIntent) string {
		a := makeAgent(projectID, slug)
		a.Phase = phase
		require.NoError(t, s.CreateAgent(ctx, a))
		if intent != "" {
			_, err := s.SetRunIntent(ctx, a.ID, intent)
			require.NoError(t, err)
		}
		return a.ID
	}
	runningRunning := mk("f-rr", "running", store.RunIntentRunning)
	runningNull := mk("f-rn", "running", "")
	errorRunning := mk("f-er", "error", store.RunIntentRunning)
	_ = mk("f-es", "error", store.RunIntentStopped)
	_ = mk("f-sn", "stopped", "")

	res, err := s.ListAgents(ctx, store.AgentFilter{ProjectID: projectID, Phase: "running", OrRunIntent: string(store.RunIntentRunning)}, store.ListOptions{Limit: 100})
	require.NoError(t, err)
	got := map[string]bool{}
	for _, a := range res.Items {
		got[a.ID] = true
	}
	assert.Equal(t, map[string]bool{runningRunning: true, runningNull: true, errorRunning: true}, got)

	res, err = s.ListAgents(ctx, store.AgentFilter{ProjectID: projectID, Phase: "running"}, store.ListOptions{Limit: 100})
	require.NoError(t, err)
	assert.Len(t, res.Items, 2, "Phase alone is unchanged")
}

// A running intent is refused with store.ErrDeleteInProgress, writing
// nothing, on the rows SetAgentRunID refuses: a start that the run-ID write
// would refuse must not leave intent running (ptone/scion#2550, round 5
// N2). A stopped intent is always recorded, since the delete engine
// records one on the row it holds.
func TestRunIntent_RunningRefusedWhileDeleteHoldsRow(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	live, expired := now.Add(time.Minute), now.Add(-time.Minute)
	for _, tc := range []struct {
		name      string
		state     string
		leaseAt   *time.Time
		deletedAt *time.Time
		refused   bool
	}{
		{"no delete", "", nil, nil, false},
		{"deleting, live lease", store.DeletionStateDeleting, &live, nil, true},
		{"deleting, lease expired", store.DeletionStateDeleting, &expired, nil, false},
		{"finalizing", store.DeletionStateFinalizing, &expired, nil, true},
		{"failed", store.DeletionStateFailed, nil, nil, false},
		{"soft-deleted", "", nil, &now, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, projectID := newTestAgentStore(t)
			a := makeAgent(projectID, "intent-delete-agent")
			require.NoError(t, s.CreateAgent(ctx, a))
			firstAt, err := s.SetRunIntent(ctx, a.ID, store.RunIntentStopped)
			require.NoError(t, err)
			set := store.DeletionFields{DeletedAt: tc.deletedAt}
			if tc.state != "" {
				st := tc.state
				set.State = &st
			}
			set.LeaseAt = tc.leaseAt
			n, err := s.UpdateAgentDeletion(ctx, a.ID, store.DeletionPredicate{}, set)
			require.NoError(t, err)
			require.Equal(t, 1, n)

			_, _, err = s.SwapRunIntent(ctx, a.ID, store.RunIntentRunning)
			got, gerr := s.GetAgent(ctx, a.ID)
			require.NoError(t, gerr)
			if tc.refused {
				require.ErrorIs(t, err, store.ErrDeleteInProgress)
				assert.Equal(t, store.RunIntentStopped, got.RunIntent, "a refused write changes nothing")
				require.NotNil(t, got.RunIntentAt)
				assert.True(t, got.RunIntentAt.Equal(firstAt), "a refused write keeps run_intent_at")
				_, err = s.SetRunIntent(ctx, a.ID, store.RunIntentRunning)
				require.ErrorIs(t, err, store.ErrDeleteInProgress, "SetRunIntent refuses too")
			} else {
				require.NoError(t, err)
				assert.Equal(t, store.RunIntentRunning, got.RunIntent)
			}
			// Stopped is recorded whatever the delete state.
			_, err = s.SetRunIntent(ctx, a.ID, store.RunIntentStopped)
			require.NoError(t, err)
		})
	}
}

// A revert to running does not apply to a row a delete holds or a
// soft-deleted row; a revert to stopped always applies (ptone/scion#2550).
func TestRunIntent_RevertToRunningSkippedWhileDeleteHoldsRow(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	live, expired := now.Add(time.Minute), now.Add(-time.Minute)
	for _, tc := range []struct {
		name      string
		state     string
		leaseAt   *time.Time
		deletedAt *time.Time
		skipped   bool
	}{
		{"no delete", "", nil, nil, false},
		{"deleting, live lease", store.DeletionStateDeleting, &live, nil, true},
		{"deleting, lease expired", store.DeletionStateDeleting, &expired, nil, false},
		{"finalizing", store.DeletionStateFinalizing, &expired, nil, true},
		{"failed", store.DeletionStateFailed, nil, nil, false},
		{"soft-deleted", "", nil, &now, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, projectID := newTestAgentStore(t)
			a := makeAgent(projectID, "revert-delete-agent")
			require.NoError(t, s.CreateAgent(ctx, a))
			_, err := s.SetRunIntent(ctx, a.ID, store.RunIntentRunning)
			require.NoError(t, err)
			stoppedAt, err := s.SetRunIntent(ctx, a.ID, store.RunIntentStopped)
			require.NoError(t, err)
			// b holds running from before the claim, for the revert to stopped.
			b := makeAgent(projectID, "revert-delete-agent-b")
			require.NoError(t, s.CreateAgent(ctx, b))
			runningAt, err := s.SetRunIntent(ctx, b.ID, store.RunIntentRunning)
			require.NoError(t, err)
			set := store.DeletionFields{DeletedAt: tc.deletedAt, LeaseAt: tc.leaseAt}
			if tc.state != "" {
				st := tc.state
				set.State = &st
			}
			for _, id := range []string{a.ID, b.ID} {
				n, err := s.UpdateAgentDeletion(ctx, id, store.DeletionPredicate{}, set)
				require.NoError(t, err)
				require.Equal(t, 1, n)
			}

			// A revert to stopped applies whatever the delete state.
			changed, err := s.RevertRunIntent(ctx, b.ID, store.RunIntentRunning, runningAt, store.RunIntentStopped)
			require.NoError(t, err)
			assert.True(t, changed, "running -> stopped revert on a held row")
			gotB, err := s.GetAgent(ctx, b.ID)
			require.NoError(t, err)
			assert.Equal(t, store.RunIntentStopped, gotB.RunIntent)

			changed, err = s.RevertRunIntent(ctx, a.ID, store.RunIntentStopped, stoppedAt, store.RunIntentRunning)
			require.NoError(t, err)
			got, err := s.GetAgent(ctx, a.ID)
			require.NoError(t, err)
			if tc.skipped {
				assert.False(t, changed)
				assert.Equal(t, store.RunIntentStopped, got.RunIntent, "a skipped revert writes nothing")
				return
			}
			assert.True(t, changed)
			assert.Equal(t, store.RunIntentRunning, got.RunIntent)
		})
	}
}
