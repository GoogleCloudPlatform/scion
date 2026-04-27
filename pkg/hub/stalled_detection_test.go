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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/sqlite"
)

func setupStalledTestServer(t *testing.T) (*Server, store.Store, *trackingEventPublisher) {
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
		config: ServerConfig{
			StalledThreshold: 5 * time.Minute,
		},
	}

	return srv, s, ep
}

func TestAgentStalledDetectionHandler_MarksStalledAgents(t *testing.T) {
	srv, s, ep := setupStalledTestServer(t)
	ctx := context.Background()

	grove := &store.Grove{
		ID:         api.NewUUID(),
		Name:       "Stalled Detection Grove",
		Slug:       "stalled-detect-grove",
		Visibility: store.VisibilityPrivate,
	}
	if err := s.CreateGrove(ctx, grove); err != nil {
		t.Fatalf("failed to create grove: %v", err)
	}

	// Create a running agent with stale activity but recent heartbeat
	agent := &store.Agent{
		ID:         api.NewUUID(),
		Slug:       "stalled-runner",
		Name:       "Stalled Runner",
		Template:   "claude",
		GroveID:    grove.ID,
		Phase:      string(state.PhaseCreated),
		Visibility: store.VisibilityPrivate,
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	// Set to running with an activity
	if err := s.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{
		Phase:    string(state.PhaseRunning),
		Activity: string(state.ActivityThinking),
	}); err != nil {
		t.Fatalf("failed to update agent status: %v", err)
	}

	// Make activity stale but keep heartbeat recent
	staleActivity := time.Now().Add(-10 * time.Minute)
	recentHB := time.Now().Add(-30 * time.Second)
	db := s.(*sqlite.SQLiteStore).DB()
	if _, err := db.ExecContext(ctx,
		"UPDATE agents SET last_activity_event = ?, last_seen = ? WHERE id = ?",
		staleActivity, recentHB, agent.ID); err != nil {
		t.Fatalf("failed to set stale activity: %v", err)
	}

	// Run the handler
	handler := srv.agentStalledDetectionHandler()
	handler(ctx)

	// Verify the agent was marked stalled and an event was published
	published := ep.publishedAgents()
	if len(published) != 1 {
		t.Fatalf("expected 1 published event, got %d", len(published))
	}
	if published[0].Activity != string(state.ActivityStalled) {
		t.Errorf("published agent activity = %q, want %q", published[0].Activity, string(state.ActivityStalled))
	}

	// Verify via store
	a, err := s.GetAgent(ctx, agent.ID)
	if err != nil {
		t.Fatalf("failed to get agent: %v", err)
	}
	if a.Activity != string(state.ActivityStalled) {
		t.Errorf("agent activity = %q, want %q", a.Activity, string(state.ActivityStalled))
	}
	// Verify stalled_from_activity records the pre-stall activity
	if a.StalledFromActivity != string(state.ActivityThinking) {
		t.Errorf("stalled_from_activity = %q, want %q", a.StalledFromActivity, string(state.ActivityThinking))
	}
}

func TestAgentStalledDetectionHandler_NoStalledAgents(t *testing.T) {
	srv, _, ep := setupStalledTestServer(t)
	ctx := context.Background()

	handler := srv.agentStalledDetectionHandler()
	handler(ctx)

	published := ep.publishedAgents()
	if len(published) != 0 {
		t.Errorf("expected 0 published events, got %d", len(published))
	}
}

func TestAgentStalledDetectionHandler_ClearedByActivityEvent(t *testing.T) {
	srv, s, ep := setupStalledTestServer(t)
	ctx := context.Background()

	grove := &store.Grove{
		ID:         api.NewUUID(),
		Name:       "Recovery Stalled Grove",
		Slug:       "recovery-stalled-grove",
		Visibility: store.VisibilityPrivate,
	}
	if err := s.CreateGrove(ctx, grove); err != nil {
		t.Fatalf("failed to create grove: %v", err)
	}

	agent := &store.Agent{
		ID:         api.NewUUID(),
		Slug:       "stalled-recovery",
		Name:       "Stalled Recovery",
		Template:   "claude",
		GroveID:    grove.ID,
		Phase:      string(state.PhaseCreated),
		Visibility: store.VisibilityPrivate,
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	// Set to running+stalled
	if err := s.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{
		Phase:    string(state.PhaseRunning),
		Activity: string(state.ActivityStalled),
	}); err != nil {
		t.Fatalf("failed to update agent status: %v", err)
	}

	// Simulate an activity event arriving (agent recovered)
	if err := s.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{
		Activity: string(state.ActivityThinking),
	}); err != nil {
		t.Fatalf("failed to send recovery activity: %v", err)
	}

	// Run stalled detection — should not re-stall since activity is now fresh
	handler := srv.agentStalledDetectionHandler()
	handler(ctx)

	published := ep.publishedAgents()
	if len(published) != 0 {
		t.Errorf("expected 0 published events (agent recovered), got %d", len(published))
	}

	// Verify agent is still thinking
	a, err := s.GetAgent(ctx, agent.ID)
	if err != nil {
		t.Fatalf("failed to get agent: %v", err)
	}
	if a.Activity != string(state.ActivityThinking) {
		t.Errorf("agent activity = %q, want %q", a.Activity, string(state.ActivityThinking))
	}
}

func TestAgentStalledDetectionHandler_StalledFromActivityIsPreserved(t *testing.T) {
	srv, s, _ := setupStalledTestServer(t)
	ctx := context.Background()

	grove := &store.Grove{
		ID:         api.NewUUID(),
		Name:       "Stalled Preserved Grove",
		Slug:       "stalled-preserved-grove",
		Visibility: store.VisibilityPrivate,
	}
	if err := s.CreateGrove(ctx, grove); err != nil {
		t.Fatalf("failed to create grove: %v", err)
	}

	agent := &store.Agent{
		ID:         api.NewUUID(),
		Slug:       "stalled-preserved",
		Name:       "Stalled Preserved",
		Template:   "claude",
		GroveID:    grove.ID,
		Phase:      string(state.PhaseCreated),
		Visibility: store.VisibilityPrivate,
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	// Set to running with executing activity
	if err := s.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{
		Phase:    string(state.PhaseRunning),
		Activity: string(state.ActivityExecuting),
	}); err != nil {
		t.Fatalf("failed to update agent status: %v", err)
	}

	// Make activity stale but keep heartbeat recent
	staleActivity := time.Now().Add(-10 * time.Minute)
	recentHB := time.Now().Add(-30 * time.Second)
	db := s.(*sqlite.SQLiteStore).DB()
	if _, err := db.ExecContext(ctx,
		"UPDATE agents SET last_activity_event = ?, last_seen = ? WHERE id = ?",
		staleActivity, recentHB, agent.ID); err != nil {
		t.Fatalf("failed to set stale activity: %v", err)
	}

	// Run stalled detection
	handler := srv.agentStalledDetectionHandler()
	handler(ctx)

	// Verify stalled_from_activity is set to the pre-stall activity
	a, err := s.GetAgent(ctx, agent.ID)
	if err != nil {
		t.Fatalf("failed to get agent: %v", err)
	}
	if a.Activity != string(state.ActivityStalled) {
		t.Errorf("agent activity = %q, want %q", a.Activity, string(state.ActivityStalled))
	}
	if a.StalledFromActivity != string(state.ActivityExecuting) {
		t.Errorf("stalled_from_activity = %q, want %q", a.StalledFromActivity, string(state.ActivityExecuting))
	}

	// Now simulate recovery: update to a new activity
	if err := s.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{
		Activity: string(state.ActivityThinking),
	}); err != nil {
		t.Fatalf("failed to send recovery activity: %v", err)
	}

	// Verify stalled_from_activity is cleared on recovery
	a, err = s.GetAgent(ctx, agent.ID)
	if err != nil {
		t.Fatalf("failed to get agent after recovery: %v", err)
	}
	if a.Activity != string(state.ActivityThinking) {
		t.Errorf("agent activity = %q, want %q", a.Activity, string(state.ActivityThinking))
	}
	if a.StalledFromActivity != "" {
		t.Errorf("stalled_from_activity = %q, want empty after recovery", a.StalledFromActivity)
	}
}

func TestAgentStalledDetectionHandler_BlockedAgentNotStalled(t *testing.T) {
	srv, s, ep := setupStalledTestServer(t)
	ctx := context.Background()

	grove := &store.Grove{
		ID:         api.NewUUID(),
		Name:       "Blocked Not Stalled Grove",
		Slug:       "blocked-not-stalled-grove",
		Visibility: store.VisibilityPrivate,
	}
	if err := s.CreateGrove(ctx, grove); err != nil {
		t.Fatalf("failed to create grove: %v", err)
	}

	agent := &store.Agent{
		ID:         api.NewUUID(),
		Slug:       "blocked-agent",
		Name:       "Blocked Agent",
		Template:   "claude",
		GroveID:    grove.ID,
		Phase:      string(state.PhaseCreated),
		Visibility: store.VisibilityPrivate,
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	// Set to running with blocked activity (agent is waiting for a child agent)
	if err := s.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{
		Phase:    string(state.PhaseRunning),
		Activity: string(state.ActivityBlocked),
	}); err != nil {
		t.Fatalf("failed to update agent status: %v", err)
	}

	// Make activity stale but keep heartbeat recent (simulates long wait for child agent)
	staleActivity := time.Now().Add(-10 * time.Minute)
	recentHB := time.Now().Add(-30 * time.Second)
	db := s.(*sqlite.SQLiteStore).DB()
	if _, err := db.ExecContext(ctx,
		"UPDATE agents SET last_activity_event = ?, last_seen = ? WHERE id = ?",
		staleActivity, recentHB, agent.ID); err != nil {
		t.Fatalf("failed to set stale activity: %v", err)
	}

	// Run stalled detection — should NOT mark this agent as stalled
	handler := srv.agentStalledDetectionHandler()
	handler(ctx)

	published := ep.publishedAgents()
	if len(published) != 0 {
		t.Errorf("expected 0 published events (blocked agent should not be stalled), got %d", len(published))
	}

	// Verify agent is still blocked
	a, err := s.GetAgent(ctx, agent.ID)
	if err != nil {
		t.Fatalf("failed to get agent: %v", err)
	}
	if a.Activity != string(state.ActivityBlocked) {
		t.Errorf("agent activity = %q, want %q", a.Activity, string(state.ActivityBlocked))
	}
}

func TestAgentStalledDetectionHandler_IdleAgentMarkedStalled(t *testing.T) {
	srv, s, ep := setupStalledTestServer(t)
	ctx := context.Background()

	grove := &store.Grove{
		ID:         api.NewUUID(),
		Name:       "Idle Stalled Grove",
		Slug:       "idle-stalled-grove",
		Visibility: store.VisibilityPrivate,
	}
	if err := s.CreateGrove(ctx, grove); err != nil {
		t.Fatalf("failed to create grove: %v", err)
	}

	agent := &store.Agent{
		ID:         api.NewUUID(),
		Slug:       "idle-agent",
		Name:       "Idle Agent",
		Template:   "claude",
		GroveID:    grove.ID,
		Phase:      string(state.PhaseCreated),
		Visibility: store.VisibilityPrivate,
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	// Set to running with idle activity
	if err := s.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{
		Phase:    string(state.PhaseRunning),
		Activity: string(state.ActivityIdle),
	}); err != nil {
		t.Fatalf("failed to update agent status: %v", err)
	}

	// Make activity stale but keep heartbeat recent (process alive but stuck at idle)
	staleActivity := time.Now().Add(-10 * time.Minute)
	recentHB := time.Now().Add(-30 * time.Second)
	db := s.(*sqlite.SQLiteStore).DB()
	if _, err := db.ExecContext(ctx,
		"UPDATE agents SET last_activity_event = ?, last_seen = ? WHERE id = ?",
		staleActivity, recentHB, agent.ID); err != nil {
		t.Fatalf("failed to set stale activity: %v", err)
	}

	// Run stalled detection — should mark this idle agent as stalled
	handler := srv.agentStalledDetectionHandler()
	handler(ctx)

	published := ep.publishedAgents()
	if len(published) != 1 {
		t.Fatalf("expected 1 published event (idle agent should be stalled), got %d", len(published))
	}
	if published[0].Activity != string(state.ActivityStalled) {
		t.Errorf("published agent activity = %q, want %q", published[0].Activity, string(state.ActivityStalled))
	}

	// Verify via store
	a, err := s.GetAgent(ctx, agent.ID)
	if err != nil {
		t.Fatalf("failed to get agent: %v", err)
	}
	if a.Activity != string(state.ActivityStalled) {
		t.Errorf("agent activity = %q, want %q", a.Activity, string(state.ActivityStalled))
	}
	if a.StalledFromActivity != string(state.ActivityIdle) {
		t.Errorf("stalled_from_activity = %q, want %q", a.StalledFromActivity, string(state.ActivityIdle))
	}
}

func TestNew_DefaultsStalledThresholdWhenZero(t *testing.T) {
	s, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate test store: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	// Create server with zero StalledThreshold (simulates cmd/server.go omission)
	srv, err := New(ServerConfig{}, s)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	if srv.config.StalledThreshold != 5*time.Minute {
		t.Errorf("StalledThreshold = %v, want %v", srv.config.StalledThreshold, 5*time.Minute)
	}
}

func TestAgentStalledDetectionHandler_SchedulerIntegration(t *testing.T) {
	srv, s, _ := setupStalledTestServer(t)

	scheduler := NewScheduler(s, slog.Default())
	scheduler.tickInterval = 50 * time.Millisecond

	scheduler.RegisterRecurring("agent-stalled-detection", 1, srv.agentStalledDetectionHandler())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	scheduler.Start(ctx)

	// Let it run a couple of ticks
	time.Sleep(130 * time.Millisecond)

	scheduler.Stop()
}
