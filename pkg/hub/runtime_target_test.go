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
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runtimeTargetDispatchFixture stores a project, broker and an agent whose
// recorded runtime target is set, plus a dispatcher with a mock broker
// client.
func runtimeTargetDispatchFixture(t *testing.T) (store.Store, *mockRuntimeBrokerClient, *HTTPAgentDispatcher, *store.Agent) {
	t.Helper()
	ctx := context.Background()
	memStore := createTestStore(t)
	require.NoError(t, memStore.CreateProject(ctx, &store.Project{
		ID: tid("rt-project"), Name: "rt-project", Slug: "rt-project",
	}))
	require.NoError(t, memStore.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: tid("rt-broker"), Name: "rt-broker", Slug: "rt-broker",
		Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
	}))
	require.NoError(t, memStore.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: tid("rt-project"), BrokerID: tid("rt-broker"), BrokerName: "rt-broker",
		LocalPath: "/tmp/rt-project/.scion", Status: store.BrokerStatusOnline,
	}))
	a := &store.Agent{
		ID:              tid("rt-agent"),
		Name:            "rt-agent",
		Slug:            "rt-agent",
		ProjectID:       tid("rt-project"),
		RuntimeBrokerID: tid("rt-broker"),
		Phase:           string(state.PhaseRunning),
		AppliedConfig: &store.AgentAppliedConfig{
			Image:         "example/image:1",
			Profile:       "remote",
			RuntimeTarget: k8sTargetB,
		},
		Labels: map[string]string{},
	}
	require.NoError(t, memStore.CreateAgent(ctx, a))
	client := &mockRuntimeBrokerClient{}
	return memStore, client, NewHTTPAgentDispatcherWithClient(memStore, client, false, slog.Default()), a
}

func storedRuntimeTarget(t *testing.T, s store.Store, id string) *store.AgentAppliedConfig {
	t.Helper()
	got, err := s.GetAgent(context.Background(), id)
	require.NoError(t, err)
	require.NotNil(t, got.AppliedConfig)
	return got.AppliedConfig
}

// A start or restart accepted by the broker forgets the recorded target in
// the store, keeping every other applied-config field, so the reconcile does
// not use a target the agent may no longer be on.
func TestDispatch_StartAndRestartForgetRuntimeTarget(t *testing.T) {
	for _, tc := range []struct {
		name     string
		dispatch func(d *HTTPAgentDispatcher, a *store.Agent) error
	}{
		{"start", func(d *HTTPAgentDispatcher, a *store.Agent) error {
			return d.DispatchAgentStart(context.Background(), a, "", false)
		}},
		{"restart", func(d *HTTPAgentDispatcher, a *store.Agent) error {
			return d.DispatchAgentRestart(context.Background(), a)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, d, a := runtimeTargetDispatchFixture(t)
			require.NoError(t, tc.dispatch(d, a))
			cfg := storedRuntimeTarget(t, s, a.ID)
			assert.Empty(t, cfg.RuntimeTarget, "stored target cleared")
			assert.Equal(t, "example/image:1", cfg.Image, "other fields kept")
			assert.Equal(t, "remote", cfg.Profile, "other fields kept")
			assert.Empty(t, a.AppliedConfig.RuntimeTarget, "in-memory target cleared")
		})
	}
}

// nilStartResponseClient accepts a start with no parseable response body,
// as the control-channel client does for a successful non-JSON reply.
type nilStartResponseClient struct{ *mockRuntimeBrokerClient }

func (c nilStartResponseClient) StartAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID, task, projectPath, projectSlug, harnessConfig, harnessConfigID, harnessConfigHash string, resolvedEnv map[string]string, resolvedSecrets []ResolvedSecret, inlineConfig *api.ScionConfig, sharedDirs []api.SharedDir, sharedWorkspace, resume bool, extras StartExtras) (*RemoteAgentResponse, error) {
	if _, err := c.mockRuntimeBrokerClient.StartAgent(ctx, brokerID, brokerEndpoint, agentID, projectID, task, projectPath, projectSlug, harnessConfig, harnessConfigID, harnessConfigHash, resolvedEnv, resolvedSecrets, inlineConfig, sharedDirs, sharedWorkspace, resume, extras); err != nil {
		return nil, err
	}
	return nil, nil
}

// A start the broker accepts without a parseable response still forgets the
// recorded target.
func TestDispatch_StartWithoutResponseForgetsRuntimeTarget(t *testing.T) {
	s, client, _, a := runtimeTargetDispatchFixture(t)
	d := NewHTTPAgentDispatcherWithClient(s, nilStartResponseClient{client}, false, slog.Default())
	require.NoError(t, d.DispatchAgentStart(context.Background(), a, "", false))
	require.True(t, client.startCalled)
	assert.Empty(t, storedRuntimeTarget(t, s, a.ID).RuntimeTarget)
	assert.Empty(t, a.AppliedConfig.RuntimeTarget)
}

// The clear bumps state_version. The dispatched agent, current before the
// clear, adopts the new version so the caller's own later write succeeds,
// while another holder of a pre-clear read gets a conflict instead of
// writing the old target back.
func TestDispatch_ClearAdoptsVersionAndFencesStaleWriters(t *testing.T) {
	ctx := context.Background()
	s, _, d, a := runtimeTargetDispatchFixture(t)
	stale, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, k8sTargetB, stale.AppliedConfig.RuntimeTarget)

	require.NoError(t, d.DispatchAgentStart(ctx, a, "", false))
	stored, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, stale.StateVersion+1, stored.StateVersion, "the clear bumped state_version")
	assert.Equal(t, stored.StateVersion, a.StateVersion, "the dispatched agent adopted it")

	require.ErrorIs(t, s.UpdateAgent(ctx, stale), store.ErrVersionConflict, "a pre-clear read cannot write back")
	assert.Empty(t, storedRuntimeTarget(t, s, a.ID).RuntimeTarget)
	require.NoError(t, s.UpdateAgent(ctx, a), "the caller's own write still succeeds")
	assert.Empty(t, storedRuntimeTarget(t, s, a.ID).RuntimeTarget)
}

// An in-memory agent that was already behind the store before the clear does
// not adopt the clear's version, so its own conflict is not masked.
func TestDispatch_ClearDoesNotMaskAnEarlierConflict(t *testing.T) {
	ctx := context.Background()
	s, _, d, a := runtimeTargetDispatchFixture(t)
	cur, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	cur.Message = "concurrent write"
	require.NoError(t, s.UpdateAgent(ctx, cur))
	behind := a.StateVersion

	require.NoError(t, d.DispatchAgentRestart(ctx, a))
	assert.Equal(t, behind, a.StateVersion, "version not adopted")
	require.ErrorIs(t, s.UpdateAgent(ctx, a), store.ErrVersionConflict)
}

func TestDispatch_FailedStartKeepsRuntimeTarget(t *testing.T) {
	s, client, d, a := runtimeTargetDispatchFixture(t)
	client.returnErr = errors.New("broker unavailable")
	require.Error(t, d.DispatchAgentStart(context.Background(), a, "", false))
	assert.Equal(t, k8sTargetB, storedRuntimeTarget(t, s, a.ID).RuntimeTarget)
}

func brokerNotFoundErr(code string) error {
	body, _ := json.Marshal(map[string]interface{}{"error": map[string]string{"code": code, "message": "Agent not found"}})
	return &brokerStatusError{StatusCode: http.StatusNotFound, Body: string(body)}
}

func TestIsBrokerAgentNotFound(t *testing.T) {
	assert.True(t, isBrokerAgentNotFound(brokerNotFoundErr(ErrCodeAgentNotFound)))
	assert.True(t, isBrokerAgentNotFound(fmt.Errorf("dispatch: %w", brokerNotFoundErr(ErrCodeAgentNotFound))), "wrapped")
	assert.False(t, isBrokerAgentNotFound(brokerNotFoundErr("not_found")), "another 404 code")
	assert.False(t, isBrokerAgentNotFound(&brokerStatusError{StatusCode: http.StatusNotFound, Body: "not json"}))
	assert.False(t, isBrokerAgentNotFound(&brokerStatusError{StatusCode: http.StatusInternalServerError,
		Body: `{"error":{"code":"agent_not_found"}}`}), "only a 404")
	assert.False(t, isBrokerAgentNotFound(errors.New("connection refused")))
	assert.False(t, isBrokerAgentNotFound(nil))
}

// The broker's 404 agent_not_found on message dispatch becomes 409
// agent_not_running; every other broker error keeps 502 runtime_error.
func TestHandleAgentMessage_BrokerAgentNotFoundMapping(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		wantCode int
		wantErr  string
	}{
		{"agent_not_found", brokerNotFoundErr(ErrCodeAgentNotFound), http.StatusConflict, ErrCodeAgentNotRunning},
		{"other 404", brokerNotFoundErr("not_found"), http.StatusBadGateway, ErrCodeRuntimeError},
		{"broker 500", &brokerStatusError{StatusCode: http.StatusInternalServerError, Body: "boom"}, http.StatusBadGateway, ErrCodeRuntimeError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, s := testServer(t)
			_, agentID := setupMessageTestAgent(t, s, string(state.PhaseRunning))
			srv.SetDispatcher(&errorDispatcher{err: tc.err})

			rec := doRequest(t, srv, http.MethodPost, fmt.Sprintf("/api/v1/agents/%s/message", agentID), map[string]interface{}{
				"structured_message": &messages.StructuredMessage{
					Sender: "user:test", Recipient: "agent:msg-agent",
					Msg: "hello", Type: messages.TypeInstruction,
				},
			})
			require.Equal(t, tc.wantCode, rec.Code, rec.Body.String())
			var errResp ErrorResponse
			require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp))
			assert.Equal(t, tc.wantErr, errResp.Error.Code)
		})
	}
}

func TestDelivery_BrokerAgentNotFound_Returns409AndPersistsFailed(t *testing.T) {
	srv, s, _, sender, target, _, dispatcher := deliverySetup(t)
	ctx := context.Background()
	dispatcher.returnErr = brokerNotFoundErr(ErrCodeAgentNotFound)

	_, dmErr := srv.ExecuteAgentDM(ctx, deliveryDMInput(sender, target, "no-container"))
	require.NotNil(t, dmErr)
	assert.Equal(t, ErrCodeAgentNotRunning, dmErr.Code)
	assert.Equal(t, http.StatusConflict, dmErr.HTTPStatus)

	msgID, ok := dmErr.Details["message_id"].(string)
	require.True(t, ok)
	msg, err := s.GetMessage(ctx, msgID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchFailed, msg.DispatchState)
}
