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
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func setupHeartbeatTestServer(t *testing.T) (*Server, store.Store, *trackingEventPublisher) {
	t.Helper()

	s, err := newTestStore(t, ":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}

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
		ID:   api.NewUUID(),
		Name: "Test Project",
		Slug: "test-project-hb",
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	// Create a running agent with a heartbeat
	staleAgent := &store.Agent{
		ID:        api.NewUUID(),
		Slug:      "stale-runner",
		Name:      "Stale Runner",
		Template:  "claude",
		ProjectID: project.ID,
		Phase:     string(state.PhaseCreated),
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
		ID:        api.NewUUID(),
		Slug:      "stopped-agent",
		Name:      "Stopped Agent",
		Template:  "claude",
		ProjectID: project.ID,
		Phase:     string(state.PhaseStopped),
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
		ID:   api.NewUUID(),
		Name: "Recovery Project",
		Slug: "recovery-project",
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	agent := &store.Agent{
		ID:        api.NewUUID(),
		Slug:      "recovery-agent",
		Name:      "Recovery Agent",
		Template:  "claude",
		ProjectID: project.ID,
		Phase:     string(state.PhaseRunning), Activity: string(state.ActivityOffline),
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
	scheduler.MaxJitter = 0

	scheduler.RegisterRecurring("agent-heartbeat-timeout", 1, srv.agentHeartbeatTimeoutHandler())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	scheduler.Start(ctx)

	// Let it run a couple of ticks
	time.Sleep(130 * time.Millisecond)

	scheduler.Stop()
}

// createHeartbeatTestBroker creates an online broker whose last heartbeat is
// lastHeartbeat (zero leaves it unset, as the create path of the co-located
// registration does).
func createHeartbeatTestBroker(t *testing.T, s store.Store, slug string, lastHeartbeat time.Time) *store.RuntimeBroker {
	t.Helper()
	b := &store.RuntimeBroker{
		ID:            api.NewUUID(),
		Name:          slug,
		Slug:          slug,
		Status:        store.BrokerStatusOnline,
		LastHeartbeat: lastHeartbeat,
	}
	if err := s.CreateRuntimeBroker(context.Background(), b); err != nil {
		t.Fatalf("failed to create broker %s: %v", slug, err)
	}
	return b
}

func getHeartbeatTestBroker(t *testing.T, s store.Store, id string) *store.RuntimeBroker {
	t.Helper()
	b, err := s.GetRuntimeBroker(context.Background(), id)
	if err != nil {
		t.Fatalf("failed to get broker %s: %v", id, err)
	}
	return b
}

// A stateless co-located broker has no heartbeat of its own. After a restart
// its last heartbeat is the registration time, so once the timeout has passed
// the reaper must keep it online rather than mark it offline.
func TestBrokerHeartbeatTimeoutHandler_StatelessBrokerStaysOnlineAfterRestart(t *testing.T) {
	srv, s, _ := setupHeartbeatTestServer(t)
	ctx := context.Background()

	stateless := createHeartbeatTestBroker(t, s, "hosted-broker", time.Now().Add(-10*time.Minute))
	srv.SetStatelessEmbeddedBrokerID(stateless.ID)

	before := time.Now()
	srv.brokerHeartbeatTimeoutHandler()(ctx)

	got := getHeartbeatTestBroker(t, s, stateless.ID)
	if got.Status != store.BrokerStatusOnline {
		t.Errorf("stateless broker status = %q after the timeout, want %q", got.Status, store.BrokerStatusOnline)
	}
	if got.LastHeartbeat.Before(before.Add(-time.Second)) {
		t.Errorf("stateless broker last heartbeat = %v, want refreshed at or after %v", got.LastHeartbeat, before)
	}

	// A second sweep long after the first must still leave it online.
	srv.brokerHeartbeatTimeoutHandler()(ctx)
	if got := getHeartbeatTestBroker(t, s, stateless.ID); got.Status != store.BrokerStatusOnline {
		t.Errorf("stateless broker status = %q after a second sweep, want %q", got.Status, store.BrokerStatusOnline)
	}
}

// On first boot the co-located registration leaves the last heartbeat unset.
// The sweep must treat that the same as a restart: online, with a heartbeat.
func TestBrokerHeartbeatTimeoutHandler_StatelessBrokerFirstBootGetsHeartbeat(t *testing.T) {
	srv, s, _ := setupHeartbeatTestServer(t)
	ctx := context.Background()

	stateless := createHeartbeatTestBroker(t, s, "hosted-broker", time.Time{})
	srv.SetStatelessEmbeddedBrokerID(stateless.ID)

	srv.brokerHeartbeatTimeoutHandler()(ctx)

	got := getHeartbeatTestBroker(t, s, stateless.ID)
	if got.Status != store.BrokerStatusOnline {
		t.Errorf("stateless broker status = %q, want %q", got.Status, store.BrokerStatusOnline)
	}
	if got.LastHeartbeat.IsZero() {
		t.Error("stateless broker last heartbeat is unset after the sweep, want it set")
	}
}

// Only the hub's own stateless broker is kept fresh: other stale brokers are
// still marked offline in the same sweep.
func TestBrokerHeartbeatTimeoutHandler_OtherStaleBrokersStillReaped(t *testing.T) {
	srv, s, _ := setupHeartbeatTestServer(t)
	ctx := context.Background()

	stateless := createHeartbeatTestBroker(t, s, "hosted-broker", time.Now().Add(-10*time.Minute))
	remote := createHeartbeatTestBroker(t, s, "remote-broker", time.Now().Add(-10*time.Minute))
	fresh := createHeartbeatTestBroker(t, s, "fresh-broker", time.Now())
	srv.SetStatelessEmbeddedBrokerID(stateless.ID)

	srv.brokerHeartbeatTimeoutHandler()(ctx)

	if got := getHeartbeatTestBroker(t, s, stateless.ID); got.Status != store.BrokerStatusOnline {
		t.Errorf("stateless broker status = %q, want %q", got.Status, store.BrokerStatusOnline)
	}
	if got := getHeartbeatTestBroker(t, s, remote.ID); got.Status != store.BrokerStatusOffline {
		t.Errorf("stale remote broker status = %q, want %q", got.Status, store.BrokerStatusOffline)
	}
	if got := getHeartbeatTestBroker(t, s, fresh.ID); got.Status != store.BrokerStatusOnline {
		t.Errorf("fresh broker status = %q, want %q", got.Status, store.BrokerStatusOnline)
	}
}

// Without a stateless broker the sweep reaps as before, including a stale
// stateful co-located broker (it has its own heartbeat loop).
func TestBrokerHeartbeatTimeoutHandler_NoStatelessBrokerReapsNormally(t *testing.T) {
	srv, s, _ := setupHeartbeatTestServer(t)
	ctx := context.Background()

	colocated := createHeartbeatTestBroker(t, s, "colocated-broker", time.Now().Add(-10*time.Minute))
	srv.SetEmbeddedBrokerID(colocated.ID)

	srv.brokerHeartbeatTimeoutHandler()(ctx)

	if got := getHeartbeatTestBroker(t, s, colocated.ID); got.Status != store.BrokerStatusOffline {
		t.Errorf("stale stateful co-located broker status = %q, want %q", got.Status, store.BrokerStatusOffline)
	}
}

// A failed refresh is logged and the sweep still runs.
func TestBrokerHeartbeatTimeoutHandler_RefreshErrorDoesNotBlockSweep(t *testing.T) {
	srv, s, _ := setupHeartbeatTestServer(t)
	ctx := context.Background()

	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	remote := createHeartbeatTestBroker(t, s, "remote-broker", time.Now().Add(-10*time.Minute))
	// A stateless broker ID with no row makes the refresh fail.
	srv.SetStatelessEmbeddedBrokerID(api.NewUUID())

	srv.brokerHeartbeatTimeoutHandler()(ctx)

	if got := getHeartbeatTestBroker(t, s, remote.ID); got.Status != store.BrokerStatusOffline {
		t.Errorf("stale remote broker status = %q after a failed refresh, want %q", got.Status, store.BrokerStatusOffline)
	}
	if !strings.Contains(logs.String(), "failed to refresh stateless co-located broker heartbeat") {
		t.Errorf("refresh failure not logged; logs:\n%s", logs.String())
	}
}
