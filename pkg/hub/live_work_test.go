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

package hub

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
)

// A container heartbeat carrying live work (ptone/scion#7855) records the
// live-work timestamps and, like any heartbeat-only report, publishes no
// status event.
func TestAgentStatus_HeartbeatWithLiveWorkPublishesNothing(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	project := &store.Project{ID: tid("project-lw"), Name: "LiveWork", Slug: "live-work"}
	require.NoError(t, s.CreateProject(ctx, project))
	agent := &store.Agent{
		ID: tid("agent-lw"), Slug: "agent-lw", Name: "agent-lw", ProjectID: project.ID,
		Phase: string(state.PhaseRunning), Activity: string(state.ActivityExecuting),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))
	probe := newStatusEventProbe(t, srv, agent.ID)

	postAgentStatus(t, srv, agent.ID, store.AgentStatusUpdate{
		Heartbeat: true,
		LiveWork:  &store.LiveWork{Kind: "process", SuppressStall: true, Detail: "go (pid 42)"},
	})
	assert.Equal(t, 0, probe.drain(), "a heartbeat with live work publishes no status event")
	assert.Empty(t, probe.counts())

	got, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	require.NotNil(t, got.LastLiveProcessAt)
	require.NotNil(t, got.LastLiveWorkAt)
	assert.WithinDuration(t, time.Now(), *got.LastLiveWorkAt, time.Minute)
	assert.Equal(t, string(state.ActivityExecuting), got.Activity)
	assert.True(t, got.HasRecentLiveWork(time.Now()))
}

// Auto-suspend skips a stalled agent whose container reported live work
// within store.LiveWorkFreshness, and still suspends one whose live work is
// older than that (ptone/scion#7855, invariant 2).
func TestAutoSuspend_SkipsAgentWithRecentLiveWork(t *testing.T) {
	srv, s := testServer(t)
	disp := newGatedStopDispatcher()
	close(disp.release)
	srv.SetDispatcher(disp)
	ctx := context.Background()

	liveAgent := newStalledBrokerAgent(t, s, "lw-live")
	staleAgent := newStalledBrokerAgent(t, s, "lw-stale")
	db := s.(*entadapter.CompositeStore).DB()
	for id, at := range map[string]time.Time{
		liveAgent.ID:  time.Now().Add(-10 * time.Second),
		staleAgent.ID: time.Now().Add(-10 * time.Minute),
	} {
		_, err := db.ExecContext(ctx, "UPDATE agents SET last_live_process_at = ? WHERE id = ?", at, id)
		require.NoError(t, err)
	}
	a, err := s.GetAgent(ctx, liveAgent.ID)
	require.NoError(t, err)
	require.True(t, a.HasRecentLiveWork(time.Now()))
	b, err := s.GetAgent(ctx, staleAgent.ID)
	require.NoError(t, err)
	require.False(t, b.HasRecentLiveWork(time.Now()))

	srv.autoSuspendStalledAgents(ctx, []store.Agent{*a, *b})

	assert.Equal(t, []string{staleAgent.ID}, disp.stopped())
	gotLive, err := s.GetAgent(ctx, liveAgent.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseRunning), gotLive.Phase, "an agent with live work stays running")
}
