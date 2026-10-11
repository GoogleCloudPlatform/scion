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
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// setupHeartbeatExitCodeTest creates a server with a broker, project, and
// running agent so that heartbeat processing can be tested end-to-end.
func setupHeartbeatExitCodeTest(t *testing.T) (srv *Server, s store.Store, brokerID, projectID, agentSlug string) {
	t.Helper()

	srv, s = testServer(t)
	grantDevUserRuntimeBrokerAccess(t, s)
	ctx := context.Background()

	broker := &store.RuntimeBroker{
		ID:      tid("hb-broker"),
		Name:    "HB Broker",
		Slug:    "hb-broker",
		Status:  store.BrokerStatusOnline,
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	project := &store.Project{
		ID:      tid("hb-project"),
		Slug:    "hb-project",
		Name:    "HB Project",
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))

	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     broker.Status,
	}))

	agent := &store.Agent{
		ID:              tid("hb-agent"),
		Slug:            "hb-agent",
		Name:            "HB Agent",
		Template:        "default",
		ProjectID:       project.ID,
		RuntimeBrokerID: broker.ID,
		Phase:           "running",
		Activity:        "working",
		Labels:          map[string]string{},
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	return srv, s, broker.ID, project.ID, agent.Slug
}

// sendHeartbeat sends a broker heartbeat with the given agent heartbeat data
// and returns the HTTP status code.
func sendHeartbeat(t *testing.T, srv *Server, brokerID, projectID string, agentHB brokerAgentHeartbeat) int {
	t.Helper()
	hb := brokerHeartbeatRequest{
		Status: "online",
		Projects: []brokerProjectHeartbeat{
			{
				ProjectID: projectID,
				Agents:    []brokerAgentHeartbeat{agentHB},
			},
		},
	}
	rec := doRequest(t, srv, http.MethodPost,
		"/api/v1/runtime-brokers/"+brokerID+"/heartbeat", hb)
	return rec.Code
}

// getAgentState retrieves the current agent state from the store.
func getAgentState(t *testing.T, s store.Store, slug, projectID string) *store.Agent {
	t.Helper()
	agent, err := s.GetAgentBySlug(context.Background(), projectID, slug)
	require.NoError(t, err)
	return agent
}

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

//nolint:unused // Kept for test diagnostics when extending heartbeat timeout cases.
func (t *trackingEventPublisher) reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.agents = nil
}
