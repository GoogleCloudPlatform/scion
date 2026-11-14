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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAgentStore_UpdateAgentStatus_LiveWork pins how a heartbeat's live
// work lands (ptone/scion#7855): any live work sets last_live_process_at,
// stall-suppressing work also sets last_live_work_at, and neither touches
// the activity or its timestamp.
func TestAgentStore_UpdateAgentStatus_LiveWork(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	a := makeAgent(projectID, "live-work")
	a.Activity = "executing"
	require.NoError(t, s.CreateAgent(ctx, a))

	// A plain heartbeat records no live work.
	require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{Heartbeat: true}))
	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Nil(t, got.LastLiveProcessAt)
	assert.Nil(t, got.LastLiveWorkAt)

	// Live work that does not suppress STALLED sets only the process column.
	before := time.Now().Add(-time.Second)
	require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{
		Heartbeat: true,
		LiveWork:  &store.LiveWork{Kind: "tool"},
	}))
	got, err = s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	require.NotNil(t, got.LastLiveProcessAt)
	assert.True(t, got.LastLiveProcessAt.After(before))
	assert.Nil(t, got.LastLiveWorkAt)

	// Stall-suppressing live work sets both.
	require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{
		Heartbeat: true,
		LiveWork:  &store.LiveWork{Kind: "process", SuppressStall: true, Detail: "make (pid 7)"},
	}))
	got, err = s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	require.NotNil(t, got.LastLiveProcessAt)
	require.NotNil(t, got.LastLiveWorkAt)
	assert.True(t, got.LastLiveWorkAt.After(before))
	assert.Equal(t, "executing", got.Activity, "live work never writes the activity")
	assert.True(t, got.LastActivityEvent.IsZero(), "live work is not an activity event")

	// A later heartbeat without live work keeps the recorded times.
	require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{Heartbeat: true}))
	again, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	require.NotNil(t, again.LastLiveWorkAt)
	assert.True(t, again.LastLiveWorkAt.Equal(*got.LastLiveWorkAt))
}

// TestAgentStore_UpdateAgentStatus_LiveWorkOtherRunIgnored: a report from
// another run writes only the timestamps, so its live work does not count
// for the current run.
func TestAgentStore_UpdateAgentStatus_LiveWorkOtherRunIgnored(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	a := makeAgent(projectID, "live-work-other-run")
	require.NoError(t, s.CreateAgent(ctx, a))

	require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{
		Heartbeat:    true,
		LiveWork:     &store.LiveWork{Kind: "process", SuppressStall: true},
		ReportSource: store.StatusReportSourceAgent,
		ReportRunID:  "some-older-run",
	}))
	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Nil(t, got.LastLiveProcessAt)
	assert.Nil(t, got.LastLiveWorkAt)
}

// TestAgentStore_MarkStalledAgents_LiveWork pins the stall predicate
// (ptone/scion#7855): an agent stalls only when it has had neither an
// activity event nor stall-suppressing live work since the threshold.
func TestAgentStore_MarkStalledAgents_LiveWork(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	now := time.Now()
	activityThreshold := now.Add(-10 * time.Minute)
	heartbeatRecency := now.Add(-2 * time.Minute)

	mk := func(slug string) *store.Agent {
		a := makeAgent(projectID, slug)
		a.Phase = "running"
		a.Activity = "executing"
		a.LastActivityEvent = now.Add(-30 * time.Minute)
		a.LastSeen = now
		require.NoError(t, s.CreateAgent(ctx, a))
		return a
	}
	setLive := func(a *store.Agent, process, work *time.Time) {
		upd := s.client.Agent.UpdateOneID(uuid.MustParse(a.ID))
		if process != nil {
			upd.SetLastLiveProcessAt(*process)
		}
		if work != nil {
			upd.SetLastLiveWorkAt(*work)
		}
		require.NoError(t, upd.Exec(ctx))
	}
	ago := func(d time.Duration) *time.Time { t := now.Add(-d); return &t }

	noLiveWork := mk("no-live-work")
	recentWork := mk("recent-work")
	setLive(recentWork, ago(20*time.Second), ago(20*time.Second))
	oldWork := mk("old-work")
	setLive(oldWork, ago(20*time.Minute), ago(20*time.Minute))
	// Live work that does not suppress STALLED (only the process column)
	// leaves the agent stall-eligible.
	processOnly := mk("process-only")
	setLive(processOnly, ago(20*time.Second), nil)

	updated, err := s.MarkStalledAgents(ctx, activityThreshold, heartbeatRecency)
	require.NoError(t, err)
	ids := map[string]bool{}
	for _, a := range updated {
		ids[a.ID] = true
	}
	assert.True(t, ids[noLiveWork.ID], "no live work: stalls by activity age")
	assert.True(t, ids[oldWork.ID], "live work older than the threshold: stalls")
	assert.True(t, ids[processOnly.ID], "non-suppressing live work: stalls")
	assert.False(t, ids[recentWork.ID], "recent stall-suppressing live work: not stalled")
	assert.Len(t, updated, 3)

	got, err := s.GetAgent(ctx, recentWork.ID)
	require.NoError(t, err)
	assert.Equal(t, "executing", got.Activity)
	assert.Empty(t, got.StalledFromActivity)
}
