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
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// brokerMockDispatcher records dispatched messages for test assertions.
type brokerMockDispatcher struct {
	mu       sync.Mutex
	messages []brokerDispatchedMsg
}

type brokerDispatchedMsg struct {
	agentSlug  string
	msg        string
	interrupt  bool
	structured *messages.StructuredMessage
	messageID  string // hub message ID carried on the dispatch context (#1820)
}

func newBrokerTestStore(t *testing.T) store.Store {
	t.Helper()
	s, err := newTestStore(t, ":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	return s
}

// setupBrokerTestProject creates a project and a runtime broker, returns the project ID.
func setupBrokerTestProject(t *testing.T, s store.Store) string {
	t.Helper()
	ctx := context.Background()

	// Create a runtime broker for agent FK constraints
	rb := &store.RuntimeBroker{
		ID:       tid("broker-1"),
		Name:     "test-broker",
		Slug:     "test-broker",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := s.CreateRuntimeBroker(ctx, rb); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	project := &store.Project{
		ID:   api.NewUUID(),
		Name: "test-project",
		Slug: "test-project",
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}
	return project.ID
}

// setupBrokerTestAgent creates a running agent and returns it.
func setupBrokerTestAgent(t *testing.T, s store.Store, projectID, slug, phase string) *store.Agent {
	t.Helper()
	agent := &store.Agent{
		ID:              api.NewUUID(),
		Name:            slug,
		Slug:            slug,
		ProjectID:       projectID,
		Phase:           phase,
		RuntimeBrokerID: tid("broker-1"),
	}
	if err := s.CreateAgent(context.Background(), agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}
	return agent
}

func (d *brokerMockDispatcher) DispatchAgentCreate(ctx context.Context, agent *store.Agent) (*CreateDispatchResult, error) {
	return nil, nil
}
func (d *brokerMockDispatcher) DispatchAgentProvision(ctx context.Context, agent *store.Agent) error {
	return nil
}

func (d *brokerMockDispatcher) DispatchAgentReprovision(ctx context.Context, agent *store.Agent) error {
	return nil
}
func (d *brokerMockDispatcher) DispatchAgentStart(ctx context.Context, agent *store.Agent, task string, _ bool) error {
	return nil
}
func (d *brokerMockDispatcher) DispatchAgentStop(ctx context.Context, agent *store.Agent) error {
	return nil
}
func (d *brokerMockDispatcher) DispatchAgentRestart(ctx context.Context, agent *store.Agent) error {
	return nil
}
func (d *brokerMockDispatcher) DispatchAgentResetAuth(_ context.Context, _ *store.Agent) error {
	return nil
}
func (d *brokerMockDispatcher) DispatchAgentDelete(ctx context.Context, agent *store.Agent, deleteFiles, removeBranch, softDelete bool, deletedAt time.Time) error {
	return nil
}
func (d *brokerMockDispatcher) DispatchAgentMessage(ctx context.Context, agent *store.Agent, message string, interrupt bool, structuredMsg *messages.StructuredMessage) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.messages = append(d.messages, brokerDispatchedMsg{
		agentSlug:  agent.Slug,
		msg:        message,
		interrupt:  interrupt,
		structured: structuredMsg,
		messageID:  dispatchMessageIDFromContext(ctx),
	})
	return nil
}
func (d *brokerMockDispatcher) DispatchCheckAgentPrompt(ctx context.Context, agent *store.Agent) (bool, error) {
	return false, nil
}
func (d *brokerMockDispatcher) DispatchAgentCreateWithGather(ctx context.Context, agent *store.Agent) (*CreateDispatchResult, error) {
	return nil, nil
}
func (d *brokerMockDispatcher) DispatchAgentLogs(_ context.Context, _ *store.Agent, _ int) (string, error) {
	return "", nil
}
func (d *brokerMockDispatcher) DispatchAgentExec(_ context.Context, _ *store.Agent, _ []string, _ int) (string, int, error) {
	return "", 0, nil
}
func (d *brokerMockDispatcher) DispatchFinalizeEnv(ctx context.Context, agent *store.Agent, env map[string]string) (*CreateDispatchResult, error) {
	return nil, nil
}

func (d *brokerMockDispatcher) getMessages() []brokerDispatchedMsg {
	d.mu.Lock()
	defer d.mu.Unlock()
	result := make([]brokerDispatchedMsg, len(d.messages))
	copy(result, d.messages)
	return result
}
