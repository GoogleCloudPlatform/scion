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
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Status event cadence (ptone/scion#5233): heartbeats that change nothing a
// subscriber sees publish no status event; real changes still publish, and
// count as trigger=change.

// statusEventProbe wires a metered in-memory publisher into srv and counts
// the status events delivered on one agent's subject.
type statusEventProbe struct {
	t      *testing.T
	ch     <-chan Event
	counts func() map[string]int64
}

func newStatusEventProbe(t *testing.T, srv *Server, agentID string) *statusEventProbe {
	t.Helper()
	rec, reader := newPublishMetrics(t)
	pub := NewChannelEventPublisher()
	t.Cleanup(pub.Close)
	srv.SetEventStreamMetrics(rec)
	srv.SetEventPublisher(pub)
	ch, unsubscribe := pub.Subscribe("agent." + agentID + ".status")
	t.Cleanup(unsubscribe)
	return &statusEventProbe{t: t, ch: ch, counts: func() map[string]int64 { return publishedCounts(t, reader) }}
}

// drain returns the number of status events delivered so far. Delivery is
// synchronous with the request, so a short wait only guards against a
// scheduler hiccup.
func (p *statusEventProbe) drain() int {
	n := 0
	for {
		select {
		case <-p.ch:
			n++
		case <-time.After(50 * time.Millisecond):
			return n
		}
	}
}

func postAgentStatus(t *testing.T, srv *Server, agentID string, su store.AgentStatusUpdate) {
	t.Helper()
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agentID+"/status", su)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestAgentStatus_HeartbeatOnlyPublishesNothing(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	project := &store.Project{ID: tid("project-cad"), Name: "Cadence", Slug: "cadence"}
	require.NoError(t, s.CreateProject(ctx, project))
	agent := &store.Agent{
		ID: tid("agent-cad"), Slug: "agent-cad", Name: "agent-cad", ProjectID: project.ID,
		Phase: string(state.PhaseRunning), Activity: string(state.ActivityWaitingForInput),
		LastSeen: time.Now().Add(-time.Hour),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))
	probe := newStatusEventProbe(t, srv, agent.ID)

	for i := 0; i < 20; i++ {
		postAgentStatus(t, srv, agent.ID, store.AgentStatusUpdate{Heartbeat: true})
	}
	assert.Equal(t, 0, probe.drain(), "heartbeat-only reports publish no status event")
	assert.Empty(t, probe.counts(), "nothing counted on scion.hub.events.published")

	// The heartbeats still refresh last_seen (stall detection reads it).
	got, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now(), got.LastSeen, time.Minute)
	assert.Equal(t, string(state.ActivityWaitingForInput), got.Activity)

	// A real change publishes, as trigger=change.
	postAgentStatus(t, srv, agent.ID, store.AgentStatusUpdate{Activity: string(state.ActivityWorking)})
	assert.Equal(t, 1, probe.drain())
	// A heartbeat that carries a field still publishes.
	postAgentStatus(t, srv, agent.ID, store.AgentStatusUpdate{Heartbeat: true, TaskSummary: "reading"})
	assert.Equal(t, 1, probe.drain())
	assert.Equal(t, map[string]int64{
		"agent.status/change":         2,
		"project.agent.status/change": 2,
	}, probe.counts())
}

func TestBrokerHeartbeat_PublishesOnlyVisibleChanges(t *testing.T) {
	f := newReconcileFixture(t)
	a := f.addAgent("cad-1", string(state.PhaseRunning), string(state.ActivityWorking),
		func(a *store.Agent) { a.ContainerStatus = "running" })
	probe := newStatusEventProbe(t, f.srv, a.ID)

	report := func(phase, activity string) {
		f.send(brokerHeartbeatRequest{
			Status:    store.BrokerStatusOnline,
			Inventory: completeInventory(),
			Projects: []brokerProjectHeartbeat{{ProjectID: f.projectID, Agents: []brokerAgentHeartbeat{{
				Slug: a.Slug, Phase: phase, Activity: activity, ContainerStatus: "running", RuntimeTarget: "docker",
			}}}},
		})
	}

	// The broker reports what the hub already holds: no status event, ten
	// times over, while last_seen moves.
	for i := 0; i < 10; i++ {
		report(string(state.PhaseRunning), string(state.ActivityWorking))
	}
	assert.Equal(t, 0, probe.drain(), "unchanged broker heartbeats publish no status event")
	assert.Empty(t, probe.counts())
	got := f.get(a.ID)
	assert.WithinDuration(t, time.Now(), got.LastSeen, time.Minute, "the heartbeat still refreshes last_seen")

	// A heartbeat that carries a real change publishes once, as
	// trigger=change (the phase 0 review: it was counted as heartbeat).
	report(string(state.PhaseRunning), string(state.ActivityWaitingForInput))
	assert.Equal(t, 1, probe.drain())
	assert.Equal(t, string(state.ActivityWaitingForInput), f.get(a.ID).Activity)
	assert.Equal(t, map[string]int64{
		"agent.status/change":         1,
		"project.agent.status/change": 1,
	}, probe.counts())

	// And the same report again is silent.
	report(string(state.PhaseRunning), string(state.ActivityWaitingForInput))
	assert.Equal(t, 0, probe.drain())
}

func TestAgentStatusVisiblyChanged(t *testing.T) {
	base := func() *store.Agent {
		return &store.Agent{ID: "a1", ProjectID: "p1", Phase: "running", Activity: "working", ContainerStatus: "running"}
	}
	same := base()
	same.LastSeen = time.Now()
	same.Updated = time.Now()
	assert.False(t, agentStatusVisiblyChanged(base(), same), "last_seen and updated are not visible")

	for name, mutate := range map[string]func(*store.Agent){
		"phase":     func(a *store.Agent) { a.Phase = "stopped" },
		"activity":  func(a *store.Agent) { a.Activity = "idle" },
		"message":   func(a *store.Agent) { a.Message = "hi" },
		"container": func(a *store.Agent) { a.ContainerStatus = "exited (1)" },
		"tool":      func(a *store.Agent) { a.ToolName = "Bash" },
		"activity event": func(a *store.Agent) {
			a.LastActivityEvent = time.Now()
		},
	} {
		changed := base()
		mutate(changed)
		assert.True(t, agentStatusVisiblyChanged(base(), changed), name)
	}
	assert.True(t, agentStatusVisiblyChanged(nil, base()))
}

// The broker heartbeat writes the HarnessAuth/Profile backfill,
// store.ApplyBrokerAgentConfig and the runtime-target record
// (recordHeartbeatRuntimeTarget) onto the agent it later compares with the
// re-read row, so a field those writers touch must never reach the status
// event or its badges; otherwise a change could go unpublished. The writers
// may also create a nil AppliedConfig, which must not count as a change
// either.
func TestAgentStatusEventIgnoresHeartbeatConfigWrites(t *testing.T) {
	writeAll := func(a *store.Agent) {
		if a.AppliedConfig == nil {
			a.AppliedConfig = &store.AgentAppliedConfig{}
		}
		a.AppliedConfig.HarnessAuth = "api-key"
		a.AppliedConfig.Profile = "default"
		store.ApplyBrokerAgentConfig(a, store.BrokerAgentConfig{
			Template: "t2", HarnessConfig: "hc2", HarnessAuth: "vertex-ai", HarnessConfigSource: "hub",
			Image: "img:2", Profile: "p2", Runtime: "kubernetes",
		})
		a.AppliedConfig.RuntimeTarget = "kubernetes/c1"
		a.AppliedConfig.RuntimeTargetCandidate = "kubernetes/c2"
	}
	for name, base := range map[string]func() *store.Agent{
		"applied config set": func() *store.Agent {
			return &store.Agent{
				ID: "a1", ProjectID: "p1", Phase: "running", Template: "t1", Runtime: "docker",
				AppliedConfig: &store.AgentAppliedConfig{Harness: "claude", ModelSize: "large"},
			}
		},
		"applied config nil": func() *store.Agent {
			return &store.Agent{ID: "a1", ProjectID: "p1", Phase: "running", Template: "t1", Runtime: "docker"}
		},
	} {
		written := base()
		writeAll(written)
		require.Equal(t, "t2", written.Template, "%s: the writes took effect", name)
		require.Equal(t, "p2", written.AppliedConfig.Profile, "%s: the writes took effect", name)
		assert.False(t, agentStatusVisiblyChanged(base(), written), name)
	}
}
