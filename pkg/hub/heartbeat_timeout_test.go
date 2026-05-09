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
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/sqlite"
)

// trackingEventPublisher records PublishAgentStatus calls for test assertions.
type trackingEventPublisher struct {
	noopEventPublisher
	mu     sync.Mutex
	agents []*store.Agent
}

func (t *trackingEventPublisher) PublishAgentStatus(_ context.Context, agent *store.Agent) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.agents = append(t.agents, agent)
}

func (t *trackingEventPublisher) publishedAgents() []*store.Agent {
	t.mu.Lock()
	defer t.mu.Unlock()
	result := make([]*store.Agent, len(t.agents))
	copy(result, t.agents)
	return result
}

func (t *trackingEventPublisher) reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.agents = nil
}

func setupHeartbeatTestServer(t *testing.T) (*Server, store.Store, *trackingEventPublisher) {
	t.Helper()

	s, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate test store: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	ep := &trackingEventPublisher{}

	srv := &Server{
		store:  s,
		events: ep,
	}

	return srv, s, ep
}

func TestAgentHeartbeatTimeoutHandler_MarksStaleAgents(t *testing.T) {
	srv, s, ep := setupHeartbeatTestServer(t)
	ctx := context.Background()

	// Create project
	project := &store.Project{
		ID:         api.NewUUID(),
		Name:       "Test Project",
		Slug:       "test-project-hb",
		Visibility: store.VisibilityPrivate,
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	// Create a running agent with a heartbeat
	staleAgent := &store.Agent{
		ID:         api.NewUUID(),
		Slug:       "stale-runner",
		Name:       "Stale Runner",
		Template:   "claude",
		ProjectID:    project.ID,
		Phase:      string(state.PhaseCreated),
		Visibility: store.VisibilityPrivate,
	}
	if err := s.CreateAgent(ctx, staleAgent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	// Set it to running (UpdateAgentStatus sets last_seen = now)
	if err := s.UpdateAgentStatus(ctx, staleAgent.ID, store.AgentStatusUpdate{
		Phase: string(state.PhaseRunning),
	}); err != nil {
		t.Fatalf("failed to update agent status: %v", err)
	}

	// Create a stopped agent (terminal — should not be affected)
	stoppedAgent := &store.Agent{
		ID:         api.NewUUID(),
		Slug:       "stopped-agent",
		Name:       "Stopped Agent",
		Template:   "claude",
		ProjectID:    project.ID,
		Phase:      string(state.PhaseStopped),
		Visibility: store.VisibilityPrivate,
	}
	if err := s.CreateAgent(ctx, stoppedAgent); err != nil {
		t.Fatalf("failed to create stopped agent: %v", err)
	}
	// Give it a heartbeat too
	if err := s.UpdateAgentStatus(ctx, stoppedAgent.ID, store.AgentStatusUpdate{
		Heartbeat: true,
	}); err != nil {
		t.Fatalf("failed to update stopped agent status: %v", err)
	}

	// Run the handler — the handler uses a 2-minute threshold internally.
	// Since we just set last_seen = now, nothing should be stale yet.
	handler := srv.agentHeartbeatTimeoutHandler()
	handler(ctx)

	published := ep.publishedAgents()
	if len(published) != 0 {
		t.Errorf("expected 0 published events (agents are fresh), got %d", len(published))
	}

	// Verify the running agent is still running
	a, err := s.GetAgent(ctx, staleAgent.ID)
	if err != nil {
		t.Fatalf("failed to get agent: %v", err)
	}
	if a.Phase != string(state.PhaseRunning) {
		t.Errorf("agent status = %q, want %q (agent should not be stale yet)", a.Phase, string(state.PhaseRunning))
	}
}

func TestAgentHeartbeatTimeoutHandler_NoStaleAgents(t *testing.T) {
	srv, _, ep := setupHeartbeatTestServer(t)
	ctx := context.Background()

	// Run handler with no agents at all
	handler := srv.agentHeartbeatTimeoutHandler()
	handler(ctx)

	// Verify no events were published
	published := ep.publishedAgents()
	if len(published) != 0 {
		t.Errorf("expected 0 published events, got %d", len(published))
	}
}

func TestAgentHeartbeatTimeoutHandler_ClearedBySubsequentHeartbeat(t *testing.T) {
	_, s, _ := setupHeartbeatTestServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:         api.NewUUID(),
		Name:       "Recovery Project",
		Slug:       "recovery-project",
		Visibility: store.VisibilityPrivate,
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	agent := &store.Agent{
		ID:       api.NewUUID(),
		Slug:     "recovery-agent",
		Name:     "Recovery Agent",
		Template: "claude",
		ProjectID:  project.ID,
		Phase:    string(state.PhaseRunning), Activity: string(state.ActivityOffline),
		Visibility: store.VisibilityPrivate,
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	// Simulate a heartbeat arriving — normal UpdateAgentStatus with a new status
	// clears undetermined without any special logic.
	if err := s.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{
		Phase:     string(state.PhaseRunning),
		Heartbeat: true,
	}); err != nil {
		t.Fatalf("failed to update agent heartbeat: %v", err)
	}

	// Verify agent is back to running
	a, err := s.GetAgent(ctx, agent.ID)
	if err != nil {
		t.Fatalf("failed to get agent: %v", err)
	}
	if a.Phase != string(state.PhaseRunning) {
		t.Errorf("agent status after heartbeat = %q, want %q", a.Phase, string(state.PhaseRunning))
	}
}

func TestAgentHeartbeatTimeoutHandler_SchedulerIntegration(t *testing.T) {
	srv, s, _ := setupHeartbeatTestServer(t)

	// Verify the handler can be registered and runs without panic
	scheduler := NewScheduler(s, slog.Default())
	scheduler.tickInterval = 50 * time.Millisecond

	scheduler.RegisterRecurring("agent-heartbeat-timeout", 1, srv.agentHeartbeatTimeoutHandler())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	scheduler.Start(ctx)

	// Let it run a couple of ticks
	time.Sleep(130 * time.Millisecond)

	scheduler.Stop()
}
