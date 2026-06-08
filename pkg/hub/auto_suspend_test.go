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
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
)

// setupAutoSuspendTestServer mirrors setupStalledTestServer but also wires the
// agentLifecycleLog that the auto-suspend handler logs through.
func setupAutoSuspendTestServer(t *testing.T) (*Server, store.Store, *trackingEventPublisher) {
	t.Helper()

	s, err := newTestStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate test store: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	ep := &trackingEventPublisher{}

	srv := &Server{
		store:             s,
		events:            ep,
		agentLifecycleLog: slog.Default(),
		config: ServerConfig{
			StalledThreshold: 5 * time.Minute,
		},
	}

	return srv, s, ep
}

// makeStalledAgent creates a running agent in the "stalled" activity with the
// given harness, then forces last_activity_event / last_seen to the supplied
// times so the auto-suspend finder can select (or reject) it.
func makeStalledAgent(t *testing.T, s store.Store, slug, harnessConfig string, lastActivity, lastSeen time.Time) *store.Agent {
	t.Helper()
	ctx := context.Background()

	project := &store.Project{
		ID:         api.NewUUID(),
		Name:       "Auto Suspend Project " + slug,
		Slug:       "auto-suspend-" + slug,
		Visibility: store.VisibilityPrivate,
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	agent := &store.Agent{
		ID:         api.NewUUID(),
		Slug:       slug,
		Name:       slug,
		Template:   "claude",
		ProjectID:  project.ID,
		Phase:      string(state.PhaseCreated),
		Visibility: store.VisibilityPrivate,
		AppliedConfig: &store.AgentAppliedConfig{
			HarnessConfig: harnessConfig,
		},
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	// Mark running + stalled (the activity the auto-suspend finder requires).
	if err := s.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{
		Phase:    string(state.PhaseRunning),
		Activity: string(state.ActivityStalled),
	}); err != nil {
		t.Fatalf("failed to update agent status: %v", err)
	}

	db := s.(*entadapter.CompositeStore).DB()
	if _, err := db.ExecContext(ctx,
		"UPDATE agents SET last_activity_event = ?, last_seen = ? WHERE id = ?",
		lastActivity, lastSeen, agent.ID); err != nil {
		t.Fatalf("failed to set timing: %v", err)
	}

	return agent
}

func TestAgentAutoSuspendHandler_SuspendsStalledBeyondGrace(t *testing.T) {
	srv, s, ep := setupAutoSuspendTestServer(t)
	ctx := context.Background()

	// Stalled longer than StalledThreshold (5m) + grace (5m) = 10m, with a
	// recent heartbeat and a resume-capable harness (claude).
	staleActivity := time.Now().Add(-15 * time.Minute)
	recentHB := time.Now().Add(-30 * time.Second)
	agent := makeStalledAgent(t, s, "stalled-beyond-grace", "claude", staleActivity, recentHB)

	srv.agentAutoSuspendHandler()(ctx)

	a, err := s.GetAgent(ctx, agent.ID)
	if err != nil {
		t.Fatalf("failed to get agent: %v", err)
	}
	if a.Phase != string(state.PhaseSuspended) {
		t.Errorf("phase = %q, want %q", a.Phase, string(state.PhaseSuspended))
	}

	published := ep.publishedAgents()
	if len(published) != 1 {
		t.Fatalf("expected 1 published event, got %d", len(published))
	}
	if published[0].Phase != string(state.PhaseSuspended) {
		t.Errorf("published phase = %q, want %q", published[0].Phase, string(state.PhaseSuspended))
	}
	// The published event must reflect the persisted container_status/activity,
	// not stale values from before the suspend.
	if published[0].ContainerStatus != "stopped" {
		t.Errorf("published container status = %q, want %q", published[0].ContainerStatus, "stopped")
	}
	if published[0].Activity != "" {
		t.Errorf("published activity = %q, want empty", published[0].Activity)
	}
}

func TestAgentAutoSuspendHandler_NotSuspendedWithinGrace(t *testing.T) {
	srv, s, ep := setupAutoSuspendTestServer(t)
	ctx := context.Background()

	// Stalled past StalledThreshold (5m) but NOT past the +5m grace (total 10m):
	// 7 minutes stale should not be auto-suspended.
	staleActivity := time.Now().Add(-7 * time.Minute)
	recentHB := time.Now().Add(-30 * time.Second)
	agent := makeStalledAgent(t, s, "within-grace", "claude", staleActivity, recentHB)

	srv.agentAutoSuspendHandler()(ctx)

	a, err := s.GetAgent(ctx, agent.ID)
	if err != nil {
		t.Fatalf("failed to get agent: %v", err)
	}
	if a.Phase != string(state.PhaseRunning) {
		t.Errorf("phase = %q, want %q (still running, within grace)", a.Phase, string(state.PhaseRunning))
	}
	if a.Activity != string(state.ActivityStalled) {
		t.Errorf("activity = %q, want %q", a.Activity, string(state.ActivityStalled))
	}
	if len(ep.publishedAgents()) != 0 {
		t.Errorf("expected 0 published events, got %d", len(ep.publishedAgents()))
	}
}

func TestAgentAutoSuspendHandler_NotSuspendedWhenHarnessNoResume(t *testing.T) {
	srv, s, ep := setupAutoSuspendTestServer(t)
	ctx := context.Background()

	// Stalled beyond grace and online, but the generic harness cannot resume,
	// so the agent must be left stalled.
	staleActivity := time.Now().Add(-15 * time.Minute)
	recentHB := time.Now().Add(-30 * time.Second)
	agent := makeStalledAgent(t, s, "no-resume", "generic", staleActivity, recentHB)

	srv.agentAutoSuspendHandler()(ctx)

	a, err := s.GetAgent(ctx, agent.ID)
	if err != nil {
		t.Fatalf("failed to get agent: %v", err)
	}
	if a.Phase != string(state.PhaseRunning) {
		t.Errorf("phase = %q, want %q (harness cannot resume)", a.Phase, string(state.PhaseRunning))
	}
	if a.Activity != string(state.ActivityStalled) {
		t.Errorf("activity = %q, want %q", a.Activity, string(state.ActivityStalled))
	}
	if len(ep.publishedAgents()) != 0 {
		t.Errorf("expected 0 published events, got %d", len(ep.publishedAgents()))
	}
}

func TestAgentAutoSuspendHandler_NotSuspendedWhenOffline(t *testing.T) {
	srv, s, ep := setupAutoSuspendTestServer(t)
	ctx := context.Background()

	// Stalled beyond grace and resume-capable, but heartbeat is stale (>2m):
	// the container is presumed gone, so do not auto-suspend.
	staleActivity := time.Now().Add(-15 * time.Minute)
	staleHB := time.Now().Add(-10 * time.Minute)
	agent := makeStalledAgent(t, s, "offline", "claude", staleActivity, staleHB)

	srv.agentAutoSuspendHandler()(ctx)

	a, err := s.GetAgent(ctx, agent.ID)
	if err != nil {
		t.Fatalf("failed to get agent: %v", err)
	}
	if a.Phase != string(state.PhaseRunning) {
		t.Errorf("phase = %q, want %q (offline, not selected)", a.Phase, string(state.PhaseRunning))
	}
	if len(ep.publishedAgents()) != 0 {
		t.Errorf("expected 0 published events, got %d", len(ep.publishedAgents()))
	}
}
