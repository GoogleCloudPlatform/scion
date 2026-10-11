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
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// asyncLaunchClient is a broker client whose create answers the launch the
// way an async-capable broker does: launchPending with the requested ID.
type asyncLaunchClient struct {
	*mockRuntimeBrokerClient
	// answer builds the create answer from the request. nil means "echo
	// the launch as accepted".
	answer     func(req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error)
	wouldDefer bool
	sends      []RemoteCreateAgentRequest
}

func acceptedAnswer(req *RemoteCreateAgentRequest, launchID string) *RemoteAgentResponse {
	return &RemoteAgentResponse{
		Agent:            &RemoteAgentInfo{ID: req.ID, Slug: req.Slug, Name: req.Name, Template: "tmpl-from-broker", Phase: string(state.PhaseRunning)},
		Created:          true,
		LaunchPending:    true,
		LaunchID:         launchID,
		LaunchInstanceID: "broker-instance-1",
	}
}

type asyncLaunchFixture struct {
	store      store.Store
	client     *asyncLaunchClient
	dispatcher *HTTPAgentDispatcher
	broker     *store.RuntimeBroker
	settings   AsyncLaunchSettings
}

// newAsyncLaunchFixtureOn builds the fixture on an existing store, so a
// Server sharing the store can serve requests with f.dispatcher.
func newAsyncLaunchFixtureOn(t *testing.T, s store.Store, caps *store.BrokerCapabilities) *asyncLaunchFixture {
	t.Helper()
	ctx := context.Background()
	project := &store.Project{ID: tid("al-project"), Name: "al-project", Slug: "al-project"}
	require.NoError(t, s.CreateProject(ctx, project))
	broker := &store.RuntimeBroker{
		ID: tid("al-broker"), Name: "al-broker", Slug: "al-broker",
		Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline, Capabilities: caps,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	f := &asyncLaunchFixture{
		store:    s,
		client:   &asyncLaunchClient{mockRuntimeBrokerClient: &mockRuntimeBrokerClient{}},
		broker:   broker,
		settings: AsyncLaunchSettings{Enabled: true, Timeout: 5 * time.Minute, KeepaliveSeconds: 15},
	}
	f.dispatcher = NewHTTPAgentDispatcherWithClient(s, f.client, false, slog.Default())
	f.dispatcher.SetAsyncLaunchSettingsProvider(func() AsyncLaunchSettings { return f.settings })
	return f
}

// launchSeed puts an agent created in phase "created" into one of the
// launch states the start guard distinguishes.
type launchSeed int

const (
	seedInFlight launchSeed = iota
	seedIncompleteActive
	seedIncompleteEnded
)

func seedLaunch(t *testing.T, s store.Store, agent *store.Agent, seed launchSeed) *store.Agent {
	t.Helper()
	ctx := context.Background()
	launchID, err := s.BeginLaunch(ctx, agent.ID, store.LaunchKindCreate, 5*time.Minute)
	require.NoError(t, err)
	switch seed {
	case seedIncompleteActive:
		require.NoError(t, s.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{Phase: string(state.PhaseStopped)}))
	case seedIncompleteEnded:
		require.NotEmpty(t, agent.RuntimeBrokerID, "an ended seed needs the agent's broker")
		ans, _, err := s.ApplyLaunchReport(ctx, agent.ID, agent.RuntimeBrokerID, store.LaunchReport{
			LaunchID: launchID, InstanceID: "i1", State: "failed",
			Step: "pull", Message: "image not found", ErrorCode: "image_pull_failed",
		})
		require.NoError(t, err)
		require.Equal(t, 0, ans.HTTPStatus, "report must apply")
	}
	got, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	switch seed {
	case seedInFlight:
		require.True(t, got.IsInFlight())
	default:
		require.True(t, got.IsIncompleteCreate())
	}
	return got
}

func decodeLaunchGuardError(t *testing.T, rec *httptest.ResponseRecorder) APIError {
	t.Helper()
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), rec.Body.String())
	return resp.Error
}

func mustAgent(t *testing.T, s store.Store, id string) *store.Agent {
	t.Helper()
	a, err := s.GetAgent(context.Background(), id)
	require.NoError(t, err)
	return a
}

// newAsyncCreateServer is setupCreateAgentServer with an HTTP dispatcher
// whose broker client accepts asynchronous launches.
func newAsyncCreateServer(t *testing.T, flag bool) (*Server, store.Store, *store.Project, *asyncLaunchClient) {
	t.Helper()
	ctx := context.Background()
	srv, s, project := setupCreateAgentServer(t, &createAgentDispatcher{})
	broker, err := s.GetRuntimeBroker(ctx, tid("broker-create"))
	require.NoError(t, err)
	broker.Endpoint = "http://localhost:9800"
	broker.Capabilities = &store.BrokerCapabilities{AsyncLaunch: true}
	require.NoError(t, s.UpdateRuntimeBroker(ctx, broker))

	client := &asyncLaunchClient{mockRuntimeBrokerClient: &mockRuntimeBrokerClient{}}
	d := NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default())
	d.SetAsyncLaunchSettingsProvider(func() AsyncLaunchSettings {
		return AsyncLaunchSettings{Enabled: flag, Timeout: 5 * time.Minute, KeepaliveSeconds: 15}
	})
	srv.SetDispatcher(d)
	return srv, s, project, client
}

func (c *asyncLaunchClient) respond(req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
	c.sends = append(c.sends, *req)
	if c.answer != nil {
		return c.answer(req)
	}
	if !req.AsyncLaunch {
		return &RemoteAgentResponse{Agent: &RemoteAgentInfo{ID: req.ID, Slug: req.Slug, Name: req.Name, Phase: string(state.PhaseRunning)}, Created: true}, nil, nil
	}
	return acceptedAnswer(req, req.LaunchID), nil, nil
}

func (c *asyncLaunchClient) CreateAgent(_ context.Context, _, _ string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, error) {
	resp, _, err := c.respond(req)
	return resp, err
}

func (c *asyncLaunchClient) CreateAgentWithGather(_ context.Context, _, _ string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
	return c.respond(req)
}

func (c *asyncLaunchClient) createWithGatherWouldDefer(context.Context, string, string) bool {
	return c.wouldDefer
}

func (f *asyncLaunchFixture) agent(t *testing.T, name, phase string, optIn bool) *store.Agent {
	t.Helper()
	a := &store.Agent{
		ID: tid("al-agent-" + name), Slug: "al-" + name, Name: "al-" + name,
		ProjectID: tid("al-project"), RuntimeBrokerID: f.broker.ID,
		Phase: phase, LaunchAsyncOptIn: optIn,
		AppliedConfig: &store.AgentAppliedConfig{HarnessConfig: "claude", Task: "do the thing"},
	}
	require.NoError(t, f.store.CreateAgent(context.Background(), a))
	got, err := f.store.GetAgent(context.Background(), a.ID)
	require.NoError(t, err)
	return got
}

func (f *asyncLaunchFixture) row(t *testing.T, id string) *store.Agent {
	t.Helper()
	got, err := f.store.GetAgent(context.Background(), id)
	require.NoError(t, err)
	return got
}
