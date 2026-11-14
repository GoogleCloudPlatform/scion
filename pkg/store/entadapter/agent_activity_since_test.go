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

package entadapter

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// activitySinceOf reads the agent's stored activity_since.
func activitySinceOf(t *testing.T, s *AgentStore, id string) *time.Time {
	t.Helper()
	got, err := s.GetAgent(context.Background(), id)
	require.NoError(t, err)
	return got.ActivitySince
}

// requireStampedAfter asserts that since is set and not before before
// (with a millisecond of slack for column precision).
func requireStampedAfter(t *testing.T, since *time.Time, before time.Time, msg string) {
	t.Helper()
	require.NotNil(t, since, msg)
	assert.False(t, since.Before(before.Add(-time.Millisecond)), "%s: activity_since %v predates the write at %v", msg, *since, before)
}

// An idle agent is neither stall-marked nor swept offline, matching
// blocked, while a working agent with the same timestamps is.
func TestAgentStore_IdleNotStalledOrOffline(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	now := time.Now()

	create := func(slug, activity string, lastActivity, lastSeen time.Time) *store.Agent {
		a := makeAgent(projectID, slug)
		a.Activity = activity
		a.LastActivityEvent = lastActivity
		a.LastSeen = lastSeen
		require.NoError(t, s.CreateAgent(ctx, a))
		return a
	}

	// Stall candidates: fresh heartbeat, stale activity.
	idleStall := create("idle-stall", "idle", now.Add(-time.Hour), now)
	blockedStall := create("blocked-stall", "blocked", now.Add(-time.Hour), now)
	workingStall := create("working-stall", "working", now.Add(-time.Hour), now)

	stalled, err := s.MarkStalledAgents(ctx, now.Add(-15*time.Minute), now.Add(-2*time.Minute))
	require.NoError(t, err)
	require.Len(t, stalled, 1)
	assert.Equal(t, workingStall.ID, stalled[0].ID)
	for _, a := range []*store.Agent{idleStall, blockedStall} {
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, a.Activity, got.Activity, "%s must not be marked stalled", a.Slug)
	}

	// Offline candidates: stale heartbeat.
	idleStale := create("idle-stale", "idle", now.Add(-time.Hour), now.Add(-time.Hour))
	workingStale := create("working-stale", "working", now.Add(-time.Hour), now.Add(-time.Hour))

	offline, err := s.MarkStaleAgentsOffline(ctx, now.Add(-30*time.Minute))
	require.NoError(t, err)
	require.Len(t, offline, 1)
	assert.Equal(t, workingStale.ID, offline[0].ID)
	got, err := s.GetAgent(ctx, idleStale.ID)
	require.NoError(t, err)
	assert.Equal(t, "idle", got.Activity, "idle must not be swept offline")
}

// activity_since is set when a status update changes the activity value,
// kept when the same value or no activity is written, and cleared when the
// phase leaves running.
func TestAgentStore_ActivitySince_StatusUpdate(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	a := makeAgent(projectID, "since-status")
	beforeCreate := time.Now()
	require.NoError(t, s.CreateAgent(ctx, a))
	requireStampedAfter(t, activitySinceOf(t, s, a.ID), beforeCreate, "create running/thinking")

	beforeIdle := time.Now()
	require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{Activity: "idle", Message: "nothing to do"}))
	idleSince := activitySinceOf(t, s, a.ID)
	requireStampedAfter(t, idleSince, beforeIdle, "thinking -> idle")

	// The same activity again, and a heartbeat with no activity, keep it.
	time.Sleep(5 * time.Millisecond)
	require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{Activity: "idle"}))
	require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{ContainerStatus: "Up 5 minutes"}))
	got := activitySinceOf(t, s, a.ID)
	require.NotNil(t, got)
	assert.True(t, got.Equal(*idleSince), "re-reporting idle must not move activity_since: %v != %v", *got, *idleSince)

	beforeWork := time.Now()
	require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{Activity: "working"}))
	workSince := activitySinceOf(t, s, a.ID)
	requireStampedAfter(t, workSince, beforeWork, "idle -> working")
	assert.True(t, workSince.After(*idleSince), "a new activity moves activity_since forward")

	require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{Phase: "stopped"}))
	assert.Nil(t, activitySinceOf(t, s, a.ID), "leaving running clears activity_since")

	beforeRestart := time.Now()
	require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{Phase: "running", Activity: "working"}))
	requireStampedAfter(t, activitySinceOf(t, s, a.ID), beforeRestart, "re-entering running")
}

// A stall mark and the un-stall that follows both change the activity
// value, so both set activity_since; the offline sweep does too.
func TestAgentStore_ActivitySince_StallAndOffline(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	now := time.Now()

	a := makeAgent(projectID, "since-stall")
	a.Activity = "executing"
	a.LastActivityEvent = now.Add(-time.Hour)
	a.LastSeen = now
	require.NoError(t, s.CreateAgent(ctx, a))

	beforeStall := time.Now()
	stalled, err := s.MarkStalledAgents(ctx, now.Add(-15*time.Minute), now.Add(-2*time.Minute))
	require.NoError(t, err)
	require.Len(t, stalled, 1)
	requireStampedAfter(t, stalled[0].ActivitySince, beforeStall, "stall mark (returned row)")
	stallSince := activitySinceOf(t, s, a.ID)
	requireStampedAfter(t, stallSince, beforeStall, "stall mark")

	time.Sleep(5 * time.Millisecond)
	beforeUnstall := time.Now()
	require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{Activity: "executing"}))
	unstallSince := activitySinceOf(t, s, a.ID)
	requireStampedAfter(t, unstallSince, beforeUnstall, "un-stall")
	assert.True(t, unstallSince.After(*stallSince))

	b := makeAgent(projectID, "since-offline")
	b.LastSeen = now.Add(-time.Hour)
	require.NoError(t, s.CreateAgent(ctx, b))
	beforeOffline := time.Now()
	offline, err := s.MarkStaleAgentsOffline(ctx, now.Add(-30*time.Minute))
	require.NoError(t, err)
	require.Len(t, offline, 1)
	requireStampedAfter(t, activitySinceOf(t, s, b.ID), beforeOffline, "offline sweep")
}

// A whole-row UpdateAgent maintains activity_since from the stored row and
// ignores the caller's value.
func TestAgentStore_ActivitySince_UpdateAgent(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	a := makeAgent(projectID, "since-update")
	require.NoError(t, s.CreateAgent(ctx, a))
	createdSince := activitySinceOf(t, s, a.ID)
	require.NotNil(t, createdSince)

	// Same activity, bogus caller value: unchanged.
	cur, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	bogus := time.Now().Add(-48 * time.Hour)
	cur.ActivitySince = &bogus
	cur.Message = "unrelated change"
	require.NoError(t, s.UpdateAgent(ctx, cur))
	got := activitySinceOf(t, s, a.ID)
	require.NotNil(t, got)
	assert.True(t, got.Equal(*createdSince), "an unchanged activity keeps activity_since and the caller's value is ignored")

	// Activity change: stamped.
	cur, err = s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	cur.Activity = "working"
	beforeWork := time.Now()
	require.NoError(t, s.UpdateAgent(ctx, cur))
	requireStampedAfter(t, activitySinceOf(t, s, a.ID), beforeWork, "UpdateAgent activity change")

	// Phase leaves running: cleared.
	cur, err = s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	cur.Phase = "suspended"
	cur.Activity = ""
	require.NoError(t, s.UpdateAgent(ctx, cur))
	assert.Nil(t, activitySinceOf(t, s, a.ID), "UpdateAgent leaving running clears activity_since")
}

// A writer that only moves phase (here a bare ent update) still
// clears activity_since, through the ent hook.
func TestAgentStore_ActivitySince_PhaseOnlyWriterClears(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	a := makeAgent(projectID, "since-phase-only")
	require.NoError(t, s.CreateAgent(ctx, a))
	require.NotNil(t, activitySinceOf(t, s, a.ID))

	_, err := s.client.Agent.UpdateOneID(uuid.MustParse(a.ID)).SetPhase("stopped").Save(ctx)
	require.NoError(t, err)
	assert.Nil(t, activitySinceOf(t, s, a.ID))
}
