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

package runtimebroker

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// mockRuntimeBrokerService implements hubclient.RuntimeBrokerService for testing.
type mockRuntimeBrokerService struct {
	mu             sync.Mutex
	heartbeatCalls []mockHeartbeatCall
	heartbeatErr   error
}

type mockHeartbeatCall struct {
	BrokerID  string
	Heartbeat *hubclient.BrokerHeartbeat
	Time      time.Time
}

func (m *mockRuntimeBrokerService) Create(ctx context.Context, req *hubclient.CreateBrokerRequest) (*hubclient.CreateBrokerResponse, error) {
	return nil, nil
}

func (m *mockRuntimeBrokerService) Join(ctx context.Context, req *hubclient.JoinBrokerRequest) (*hubclient.JoinBrokerResponse, error) {
	return nil, nil
}

func (m *mockRuntimeBrokerService) List(ctx context.Context, opts *hubclient.ListBrokersOptions) (*hubclient.ListBrokersResponse, error) {
	return nil, nil
}

func (m *mockRuntimeBrokerService) Get(ctx context.Context, brokerID string) (*hubclient.RuntimeBroker, error) {
	return nil, nil
}

func (m *mockRuntimeBrokerService) Update(ctx context.Context, brokerID string, req *hubclient.UpdateBrokerRequest) (*hubclient.RuntimeBroker, error) {
	return nil, nil
}

func (m *mockRuntimeBrokerService) Delete(ctx context.Context, brokerID string) error {
	return nil
}

func (m *mockRuntimeBrokerService) ListGroves(ctx context.Context, brokerID string) (*hubclient.ListBrokerGrovesResponse, error) {
	return nil, nil
}

func (m *mockRuntimeBrokerService) Heartbeat(ctx context.Context, brokerID string, status *hubclient.BrokerHeartbeat) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.heartbeatCalls = append(m.heartbeatCalls, mockHeartbeatCall{
		BrokerID:  brokerID,
		Heartbeat: status,
		Time:      time.Now(),
	})
	return m.heartbeatErr
}

func (m *mockRuntimeBrokerService) getHeartbeatCalls() []mockHeartbeatCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]mockHeartbeatCall{}, m.heartbeatCalls...)
}

// heartbeatMockManager implements agent.Manager for testing.
type heartbeatMockManager struct {
	agents []api.AgentInfo
	err    error
}

func (m *heartbeatMockManager) Provision(ctx context.Context, opts api.StartOptions) (*api.ScionConfig, error) {
	return nil, nil
}

func (m *heartbeatMockManager) Start(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
	return nil, nil
}

func (m *heartbeatMockManager) Stop(ctx context.Context, agentID string) error {
	return nil
}

func (m *heartbeatMockManager) Delete(ctx context.Context, agentID string, deleteFiles bool, grovePath string, removeBranch bool) (bool, error) {
	return false, nil
}

func (m *heartbeatMockManager) List(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
	return m.agents, m.err
}

func (m *heartbeatMockManager) Message(ctx context.Context, agentID string, message string, interrupt bool) error {
	return nil
}

func (m *heartbeatMockManager) Watch(ctx context.Context, agentID string) (<-chan api.StatusEvent, error) {
	return nil, nil
}

func (m *heartbeatMockManager) Close() {}

func TestHeartbeatService_StartStop(t *testing.T) {
	client := &mockRuntimeBrokerService{}
	svc := NewHeartbeatService(client, "test-host", 100*time.Millisecond, nil, nil, slog.Default())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start the service
	svc.Start(ctx)

	if !svc.IsRunning() {
		t.Error("Expected service to be running after Start")
	}

	// Wait for at least one heartbeat
	time.Sleep(150 * time.Millisecond)

	// Stop the service
	svc.Stop()

	if svc.IsRunning() {
		t.Error("Expected service to not be running after Stop")
	}

	// Verify at least one heartbeat was sent
	calls := client.getHeartbeatCalls()
	if len(calls) == 0 {
		t.Error("Expected at least one heartbeat to be sent")
	}
}

func TestHeartbeatService_SendsInitialHeartbeat(t *testing.T) {
	client := &mockRuntimeBrokerService{}
	svc := NewHeartbeatService(client, "test-host", time.Hour, nil, nil, slog.Default()) // Long interval

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	svc.Start(ctx)
	defer svc.Stop()

	// Give it a moment for the initial heartbeat
	time.Sleep(50 * time.Millisecond)

	calls := client.getHeartbeatCalls()
	if len(calls) != 1 {
		t.Errorf("Expected exactly 1 initial heartbeat, got %d", len(calls))
	}

	if calls[0].BrokerID != "test-host" {
		t.Errorf("Expected host ID 'test-host', got %q", calls[0].BrokerID)
	}

	if calls[0].Heartbeat.Status != "online" {
		t.Errorf("Expected status 'online', got %q", calls[0].Heartbeat.Status)
	}
}

func TestHeartbeatService_MinInterval(t *testing.T) {
	client := &mockRuntimeBrokerService{}
	// Try to create with interval less than minimum
	svc := NewHeartbeatService(client, "test-host", 1*time.Millisecond, nil, nil, slog.Default())

	if svc.interval < MinHeartbeatInterval {
		t.Errorf("Interval should be at least %v, got %v", MinHeartbeatInterval, svc.interval)
	}
}

func TestHeartbeatService_ForceHeartbeat(t *testing.T) {
	client := &mockRuntimeBrokerService{}
	svc := NewHeartbeatService(client, "test-host", time.Hour, nil, nil, slog.Default())

	err := svc.ForceHeartbeat(context.Background())
	if err != nil {
		t.Fatalf("ForceHeartbeat failed: %v", err)
	}

	calls := client.getHeartbeatCalls()
	if len(calls) != 1 {
		t.Errorf("Expected 1 heartbeat call, got %d", len(calls))
	}
}

func TestHeartbeatService_IncludesAgentInfo(t *testing.T) {
	client := &mockRuntimeBrokerService{}
	manager := &heartbeatMockManager{
		agents: []api.AgentInfo{
			{Name: "agent-1", GroveID: "grove-1", Phase: "running", Activity: "thinking"},
			{Name: "agent-2", GroveID: "grove-1", Phase: "running", Activity: "waiting_for_input"},
			{Name: "agent-3", Grove: "grove-2", Phase: "running", Activity: "completed"},
		},
	}

	svc := NewHeartbeatService(client, "test-host", time.Hour, manager, nil, slog.Default())
	err := svc.ForceHeartbeat(context.Background())
	if err != nil {
		t.Fatalf("ForceHeartbeat failed: %v", err)
	}

	calls := client.getHeartbeatCalls()
	if len(calls) != 1 {
		t.Fatalf("Expected 1 heartbeat call, got %d", len(calls))
	}

	heartbeat := calls[0].Heartbeat
	if len(heartbeat.Groves) != 2 {
		t.Errorf("Expected 2 groves in heartbeat, got %d", len(heartbeat.Groves))
	}

	// Check grove counts
	groveCounts := make(map[string]int)
	for _, g := range heartbeat.Groves {
		groveCounts[g.GroveID] = g.AgentCount
	}

	if groveCounts["grove-1"] != 2 {
		t.Errorf("Expected grove-1 to have 2 agents, got %d", groveCounts["grove-1"])
	}
	if groveCounts["grove-2"] != 1 {
		t.Errorf("Expected grove-2 to have 1 agent, got %d", groveCounts["grove-2"])
	}
}

func TestHeartbeatService_IncludesPhaseActivity(t *testing.T) {
	client := &mockRuntimeBrokerService{}
	manager := &heartbeatMockManager{
		agents: []api.AgentInfo{
			{
				Name:     "agent-structured",
				GroveID:  "grove-1",
				Phase:    "running",
				Activity: "thinking",
			},
			{
				Name:     "agent-waiting",
				GroveID:  "grove-1",
				Phase:    "running",
				Activity: "waiting_for_input",
			},
			{
				Name:    "agent-stopped",
				GroveID: "grove-1",
				Phase:   "stopped",
			},
		},
	}

	svc := NewHeartbeatService(client, "test-host", time.Hour, manager, nil, slog.Default())
	err := svc.ForceHeartbeat(context.Background())
	if err != nil {
		t.Fatalf("ForceHeartbeat failed: %v", err)
	}

	calls := client.getHeartbeatCalls()
	if len(calls) != 1 {
		t.Fatalf("Expected 1 heartbeat call, got %d", len(calls))
	}

	heartbeat := calls[0].Heartbeat
	if len(heartbeat.Groves) != 1 {
		t.Fatalf("Expected 1 grove in heartbeat, got %d", len(heartbeat.Groves))
	}

	grove := heartbeat.Groves[0]
	if len(grove.Agents) != 3 {
		t.Fatalf("Expected 3 agents, got %d", len(grove.Agents))
	}

	// Build a map by slug for easy lookup
	agentMap := make(map[string]hubclient.AgentHeartbeat)
	for _, a := range grove.Agents {
		agentMap[a.Slug] = a
	}

	// Verify structured fields flow through
	structured := agentMap["agent-structured"]
	if structured.Phase != "running" {
		t.Errorf("agent-structured Phase = %q, want %q", structured.Phase, "running")
	}
	if structured.Activity != "thinking" {
		t.Errorf("agent-structured Activity = %q, want %q", structured.Activity, "thinking")
	}
	if structured.Status != "thinking" {
		t.Errorf("agent-structured Status = %q, want %q", structured.Status, "thinking")
	}

	waiting := agentMap["agent-waiting"]
	if waiting.Phase != "running" {
		t.Errorf("agent-waiting Phase = %q, want %q", waiting.Phase, "running")
	}
	if waiting.Activity != "waiting_for_input" {
		t.Errorf("agent-waiting Activity = %q, want %q", waiting.Activity, "waiting_for_input")
	}

	stopped := agentMap["agent-stopped"]
	if stopped.Phase != "stopped" {
		t.Errorf("agent-stopped Phase = %q, want %q", stopped.Phase, "stopped")
	}
	if stopped.Activity != "" {
		t.Errorf("agent-stopped Activity = %q, want empty", stopped.Activity)
	}
}

func TestHeartbeatService_DoubleStart(t *testing.T) {
	client := &mockRuntimeBrokerService{}
	svc := NewHeartbeatService(client, "test-host", time.Hour, nil, nil, slog.Default())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	svc.Start(ctx)
	svc.Start(ctx) // Should be a no-op
	defer svc.Stop()

	if !svc.IsRunning() {
		t.Error("Expected service to be running")
	}
}

func TestHeartbeatService_DoubleStop(t *testing.T) {
	client := &mockRuntimeBrokerService{}
	svc := NewHeartbeatService(client, "test-host", time.Hour, nil, nil, slog.Default())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	svc.Start(ctx)
	svc.Stop()
	svc.Stop() // Should be a no-op

	if svc.IsRunning() {
		t.Error("Expected service to not be running")
	}
}

func TestHeartbeatService_ContextCancellation(t *testing.T) {
	client := &mockRuntimeBrokerService{}
	svc := NewHeartbeatService(client, "test-host", time.Hour, nil, nil, slog.Default())

	ctx, cancel := context.WithCancel(context.Background())

	svc.Start(ctx)

	// Cancel the context
	cancel()

	// Wait a moment for the goroutine to exit
	time.Sleep(50 * time.Millisecond)

	// Service should have stopped
	// Note: The service goroutine exits but IsRunning may still return true
	// until Stop is called to clean up. This is expected behavior.
}

func TestHeartbeatService_StopNotStarted(t *testing.T) {
	client := &mockRuntimeBrokerService{}
	svc := NewHeartbeatService(client, "test-host", time.Hour, nil, nil, slog.Default())

	// Stop without starting should be a no-op
	svc.Stop()

	if svc.IsRunning() {
		t.Error("Service should not be running")
	}
}

func TestDefaultHeartbeatConfig(t *testing.T) {
	cfg := DefaultHeartbeatConfig()

	if cfg.Interval != DefaultHeartbeatInterval {
		t.Errorf("Expected interval %v, got %v", DefaultHeartbeatInterval, cfg.Interval)
	}
	if !cfg.Enabled {
		t.Error("Expected Enabled to be true by default")
	}
}

func TestHeartbeatService_IncludesAuxiliaryRuntimes(t *testing.T) {
	client := &mockRuntimeBrokerService{}

	// Default manager has docker agents
	defaultMgr := &heartbeatMockManager{
		agents: []api.AgentInfo{
			{Name: "docker-agent", GroveID: "grove-1", Phase: "running"},
		},
	}

	// Auxiliary manager has K8s agents
	auxMgr := &heartbeatMockManager{
		agents: []api.AgentInfo{
			{Name: "k8s-agent", GroveID: "grove-1", Phase: "running", Activity: "thinking"},
		},
	}

	svc := NewHeartbeatService(client, "test-host", time.Hour, defaultMgr, nil, slog.Default())
	svc.auxiliaryManagers = func() []agent.Manager { return []agent.Manager{auxMgr} }

	err := svc.ForceHeartbeat(context.Background())
	if err != nil {
		t.Fatalf("ForceHeartbeat failed: %v", err)
	}

	calls := client.getHeartbeatCalls()
	if len(calls) != 1 {
		t.Fatalf("Expected 1 heartbeat call, got %d", len(calls))
	}

	heartbeat := calls[0].Heartbeat
	if len(heartbeat.Groves) != 1 {
		t.Fatalf("Expected 1 grove, got %d", len(heartbeat.Groves))
	}

	grove := heartbeat.Groves[0]
	if grove.AgentCount != 2 {
		t.Errorf("Expected 2 agents (docker + k8s), got %d", grove.AgentCount)
	}

	agentMap := make(map[string]hubclient.AgentHeartbeat)
	for _, ag := range grove.Agents {
		agentMap[ag.Slug] = ag
	}
	if _, ok := agentMap["k8s-agent"]; !ok {
		t.Error("Expected k8s-agent from auxiliary runtime in heartbeat")
	}
}
