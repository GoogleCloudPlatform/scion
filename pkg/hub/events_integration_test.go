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
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// noopDispatcher is a minimal AgentDispatcher that does nothing.
type noopDispatcher struct{}

func (noopDispatcher) DispatchAgentCreate(_ context.Context, agent *store.Agent) error {
	agent.Phase = string(state.PhaseRunning)
	return nil
}
func (noopDispatcher) DispatchAgentProvision(_ context.Context, _ *store.Agent) error { return nil }
func (noopDispatcher) DispatchAgentStart(_ context.Context, _ *store.Agent, _ string) error {
	return nil
}
func (noopDispatcher) DispatchAgentStop(_ context.Context, _ *store.Agent) error    { return nil }
func (noopDispatcher) DispatchAgentRestart(_ context.Context, _ *store.Agent) error { return nil }
func (noopDispatcher) DispatchAgentDelete(_ context.Context, _ *store.Agent, _, _, _ bool, _ time.Time) error {
	return nil
}
func (noopDispatcher) DispatchAgentMessage(_ context.Context, _ *store.Agent, _ string, _ bool, _ *messages.StructuredMessage) error {
	return nil
}
func (noopDispatcher) DispatchCheckAgentPrompt(_ context.Context, _ *store.Agent) (bool, error) {
	return false, nil
}
func (noopDispatcher) DispatchAgentCreateWithGather(_ context.Context, agent *store.Agent) (*RemoteEnvRequirementsResponse, error) {
	agent.Phase = string(state.PhaseRunning)
	return nil, nil
}
func (noopDispatcher) DispatchAgentLogs(_ context.Context, _ *store.Agent, _ int) (string, error) {
	return "", nil
}
func (noopDispatcher) DispatchAgentExec(_ context.Context, _ *store.Agent, _ []string, _ int) (string, error) {
	return "", nil
}
func (noopDispatcher) DispatchFinalizeEnv(_ context.Context, _ *store.Agent, _ map[string]string) error {
	return nil
}

// setupEventTestServer creates a test server with an event publisher, grove, broker, and dispatcher.
func setupEventTestServer(t *testing.T) (*Server, store.Store, *ChannelEventPublisher, *store.Grove) {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()

	pub := NewChannelEventPublisher()
	srv.SetEventPublisher(pub)
	t.Cleanup(func() { pub.Close() })

	grove := &store.Grove{
		ID:         "grove-evt",
		Name:       "Event Test Grove",
		Slug:       "event-test-grove",
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateGrove(ctx, grove))

	broker := &store.RuntimeBroker{
		ID:     "broker-evt",
		Name:   "Event Test Broker",
		Slug:   "event-test-broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	provider := &store.GroveProvider{
		GroveID:    grove.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}
	require.NoError(t, s.AddGroveProvider(ctx, provider))

	grove.DefaultRuntimeBrokerID = broker.ID
	require.NoError(t, s.UpdateGrove(ctx, grove))

	srv.SetDispatcher(noopDispatcher{})

	return srv, s, pub, grove
}

func TestEventPublisher_CreateAgentEmitsEvent(t *testing.T) {
	srv, _, pub, grove := setupEventTestServer(t)

	// Subscribe to grove agent events
	ch, unsub := pub.Subscribe("grove." + grove.ID + ".agent.created")
	defer unsub()

	// Create agent via API
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "event-agent",
		GroveID: grove.ID,
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	// Verify event was published
	select {
	case evt := <-ch:
		assert.Equal(t, "grove."+grove.ID+".agent.created", evt.Subject)
		var data AgentCreatedEvent
		require.NoError(t, json.Unmarshal(evt.Data, &data))
		assert.Equal(t, grove.ID, data.GroveID)
		assert.Equal(t, "event-agent", data.Name)
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for agent created event")
	}
}

func TestEventPublisher_DeleteAgentEmitsEvent(t *testing.T) {
	srv, s, pub, grove := setupEventTestServer(t)
	ctx := context.Background()

	agent := &store.Agent{
		ID:      "agent-evt-del",
		Slug:    "agent-evt-del",
		Name:    "Delete Me",
		GroveID: grove.ID,
		Phase:   string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// Subscribe to agent deleted events
	ch, unsub := pub.Subscribe("grove." + grove.ID + ".agent.deleted")
	defer unsub()

	// Delete agent via API
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/agent-evt-del", nil)
	require.Equal(t, http.StatusNoContent, rec.Code)

	select {
	case evt := <-ch:
		assert.Equal(t, "grove."+grove.ID+".agent.deleted", evt.Subject)
		var data AgentDeletedEvent
		require.NoError(t, json.Unmarshal(evt.Data, &data))
		assert.Equal(t, "agent-evt-del", data.AgentID)
		assert.Equal(t, grove.ID, data.GroveID)
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for agent deleted event")
	}
}

func TestEventPublisher_CreateGroveEmitsEvent(t *testing.T) {
	srv, _ := testServer(t)

	pub := NewChannelEventPublisher()
	srv.SetEventPublisher(pub)
	defer pub.Close()

	// Subscribe to all grove created events using wildcard
	ch, unsub := pub.Subscribe("grove.>")
	defer unsub()

	// Create grove via API
	reqBody := map[string]interface{}{
		"name": "Event Grove",
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/groves", reqBody)
	require.Equal(t, http.StatusCreated, rec.Code)

	// Parse response to get grove ID
	var grove store.Grove
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &grove))

	select {
	case evt := <-ch:
		assert.Equal(t, "grove."+grove.ID+".created", evt.Subject)
		var data GroveCreatedEvent
		require.NoError(t, json.Unmarshal(evt.Data, &data))
		assert.Equal(t, grove.ID, data.GroveID)
		assert.Equal(t, "Event Grove", data.Name)
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for grove created event")
	}
}
