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

package cmd

import (
	"context"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mockAgentManager struct {
	startOpts  api.StartOptions
	startCalls int
	stopCalls  int
	stopAgent  string
}

func (m *mockAgentManager) Provision(ctx context.Context, opts api.StartOptions) (*api.ScionConfig, error) {
	return nil, nil
}

func (m *mockAgentManager) Start(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
	m.startCalls++
	m.startOpts = opts
	return &api.AgentInfo{
		ID:              "container-123",
		Name:            opts.Name,
		ContainerStatus: "running",
	}, nil
}

func (m *mockAgentManager) Stop(ctx context.Context, agentID string) error {
	m.stopCalls++
	m.stopAgent = agentID
	return nil
}

func (m *mockAgentManager) Delete(ctx context.Context, agentID string, deleteFiles bool, projectPath string, removeBranch bool) (bool, error) {
	return true, nil
}

func (m *mockAgentManager) List(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
	return nil, nil
}

func (m *mockAgentManager) Message(ctx context.Context, agentID, projectID string, message string, interrupt bool) error {
	return nil
}

func (m *mockAgentManager) MessageRaw(ctx context.Context, agentID, projectID string, keys string) error {
	return nil
}

func (m *mockAgentManager) Watch(ctx context.Context, agentID string) (<-chan api.StatusEvent, error) {
	return nil, nil
}

func (m *mockAgentManager) Close() {}

func TestDispatchAgentStart(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	mgr := &mockAgentManager{}
	brokerID := "test-broker"

	adapter := newAgentDispatcherAdapter(mgr, s, brokerID)

	// Create test grove and broker
	grove := &store.Project{
		ID:   "grove-1",
		Slug: "test-grove",
		Name: "Test Project",
	}
	err := s.CreateProject(ctx, grove)
	require.NoError(t, err)

	broker := &store.RuntimeBroker{
		ID:   brokerID,
		Name: "test-broker",
	}
	err = s.CreateRuntimeBroker(ctx, broker)
	require.NoError(t, err)

	provider := &store.ProjectProvider{
		ProjectID:   grove.ID,
		BrokerID:  brokerID,
		LocalPath: "/tmp/fake/grove",
	}
	err = s.AddProjectProvider(ctx, provider)
	require.NoError(t, err)

	// Create agent
	agent := &store.Agent{
		ID:       "agent-1",
		Slug:     "test-agent",
		Name:     "test-agent",
		ProjectID:  grove.ID,
		Template: "gemini",
		Image:    "test-image",
		Detached: true,
		AppliedConfig: &store.AgentAppliedConfig{
			Env:  map[string]string{"FOO": "BAR"},
			Task: "original task",
		},
	}
	err = s.CreateAgent(ctx, agent)
	require.NoError(t, err)

	// Test DispatchAgentStart
	err = adapter.DispatchAgentStart(ctx, agent, "new task")
	require.NoError(t, err)

	// Verify manager calls
	assert.Equal(t, 1, mgr.startCalls)
	assert.Equal(t, "test-agent", mgr.startOpts.Name)
	assert.Equal(t, true, mgr.startOpts.Resume)
	assert.Equal(t, "new task", mgr.startOpts.Task)
	assert.Equal(t, "/tmp/fake/grove", mgr.startOpts.ProjectPath)
	assert.Equal(t, "gemini", mgr.startOpts.Template)
	assert.Equal(t, "BAR", mgr.startOpts.Env["FOO"])

	// Verify agent update
	updatedAgent, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "running", updatedAgent.Phase)
	assert.Equal(t, "running", updatedAgent.ContainerStatus)
	assert.Equal(t, "container:container-123", updatedAgent.RuntimeState)
}

func TestDispatchAgentRestart(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	mgr := &mockAgentManager{}
	brokerID := "test-broker"

	adapter := newAgentDispatcherAdapter(mgr, s, brokerID)

	// Create test grove and agent
	grove := &store.Project{
		ID:   "grove-1",
		Slug: "test-grove",
		Name: "Test Project",
	}
	err := s.CreateProject(ctx, grove)
	require.NoError(t, err)

	agent := &store.Agent{
		ID:      "agent-1",
		Slug:    "test-agent",
		Name:    "test-agent",
		ProjectID: grove.ID,
	}
	err = s.CreateAgent(ctx, agent)
	require.NoError(t, err)

	// Test DispatchAgentRestart
	err = adapter.DispatchAgentRestart(ctx, agent)
	require.NoError(t, err)

	// Verify manager calls
	assert.Equal(t, 1, mgr.stopCalls)
	assert.Equal(t, "test-agent", mgr.stopAgent)

	assert.Equal(t, 1, mgr.startCalls)
	assert.Equal(t, "test-agent", mgr.startOpts.Name)
	assert.Equal(t, true, mgr.startOpts.Resume) // Restart acts like a resume

	// Verify agent update
	updatedAgent, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "running", updatedAgent.Phase)
}
