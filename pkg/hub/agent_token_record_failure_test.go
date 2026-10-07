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
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// credentialRecordFaultStore refuses to record an agent credential while
// armed: on its own (CreateAgentCredential) and with a run
// (SetAgentRunID with a credential), as the store reports it.
type credentialRecordFaultStore struct {
	store.Store
	fault *storeFaultSwitch
}

func (f *credentialRecordFaultStore) CreateAgentCredential(ctx context.Context, cred *store.AgentCredential) error {
	if f.fault.Active() {
		return errCredentialCreateForTest
	}
	return f.Store.CreateAgentCredential(ctx, cred)
}

func (f *credentialRecordFaultStore) SetAgentRunID(ctx context.Context, agentID, runID string, cred *store.AgentCredential) (string, error) {
	if cred != nil && f.fault.Active() {
		return "", fmt.Errorf("%w: %w", store.ErrCredentialNotRecorded, errCredentialCreateForTest)
	}
	return f.Store.SetAgentRunID(ctx, agentID, runID, cred)
}

// recordFailureFixture is a server whose dispatcher signs real agent
// tokens and reaches a mock broker, over a store that refuses credential
// records once armed.
type recordFailureFixture struct {
	srv     *Server
	store   store.Store // unwrapped
	client  *mockRuntimeBrokerClient
	project *store.Project
	fault   *storeFaultSwitch
}

func newRecordFailureFixture(t *testing.T, mode agentRunScopeMode) *recordFailureFixture {
	t.Helper()
	ctx := context.Background()
	srv, s, _, fault := testServerWithStoreFault(t, func(inner store.Store, fault *storeFaultSwitch) *credentialRecordFaultStore {
		return &credentialRecordFaultStore{Store: inner, fault: fault}
	})
	project := &store.Project{ID: tid("project-record-fail"), Name: "Record Fail Project", Slug: "record-fail-project"}
	require.NoError(t, s.CreateProject(ctx, project))
	broker := &store.RuntimeBroker{
		ID: tid("broker-record-fail"), Name: "Record Fail Broker", Slug: "record-fail-broker",
		Status: store.BrokerStatusOnline, Endpoint: "http://localhost:9800",
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: project.ID, BrokerID: broker.ID, BrokerName: broker.Name, Status: store.BrokerStatusOnline,
	}))
	project.DefaultRuntimeBrokerID = broker.ID
	require.NoError(t, s.UpdateProject(ctx, project))

	svc, err := NewAgentTokenService(AgentTokenConfig{})
	require.NoError(t, err)
	client := &mockRuntimeBrokerClient{}
	d := NewHTTPAgentDispatcherWithClient(srv.store, client, false, slog.Default())
	d.SetTokenGenerator(runTokenGenerator{svc: svc})
	srv.SetDispatcher(d)
	if mode != agentRunScopeOff {
		srv.authConfig.AgentRunScope = testRunScopeChecker(mode, time.Time{}, s)
	}
	return &recordFailureFixture{srv: srv, store: s, client: client, project: project, fault: fault}
}

func (f *recordFailureFixture) agentIn(t *testing.T, name string, phase state.Phase) *store.Agent {
	t.Helper()
	ctx := context.Background()
	agent := &store.Agent{
		ID: tid("agent-" + name), Name: name, Slug: name,
		ProjectID: f.project.ID, RuntimeBrokerID: tid("broker-record-fail"), Phase: string(phase),
		AppliedConfig: &store.AgentAppliedConfig{HarnessConfig: "claude", Task: "task"},
	}
	require.NoError(t, f.store.CreateAgent(ctx, agent))
	_, err := f.store.SetAgentRunID(ctx, agent.ID, "run-before", nil)
	require.NoError(t, err)
	return agent
}

// brokerReceivedToken reports whether any call that carries an agent
// token reached the broker.
func (f *recordFailureFixture) brokerReceivedToken() bool {
	c := f.client
	return c.createCalled || c.startCalled || c.restartCalled || c.resetAuthCalled
}

// TestAgentTokenRecordFailureIsFixed500: at every mint site, a token whose
// credential cannot be recorded is not issued. The request is answered
// with the same 500 and fixed message in every run-scope mode, without the
// store's error, and nothing carrying a token reaches the broker. A
// provision-only create is rolled back. Refresh is covered by
// TestAgentTokenRefreshRecordFailureIsGeneric500.
func TestAgentTokenRecordFailureIsFixed500(t *testing.T) {
	want := httptest.NewRecorder()
	writeError(want, http.StatusInternalServerError, ErrCodeInternalError, agentTokenRecordFailedMessage, nil)

	sites := []struct {
		name string
		// do arms the fault and sends the request; it returns the
		// response and, for a create, the name of the created agent.
		do func(t *testing.T, f *recordFailureFixture) (*httptest.ResponseRecorder, string)
	}{
		{name: "provision-only create", do: func(t *testing.T, f *recordFailureFixture) (*httptest.ResponseRecorder, string) {
			f.fault.Arm()
			return doRequest(t, f.srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
				Name: "record-fail-provision", ProjectID: f.project.ID, Task: "task", ProvisionOnly: true,
			}), "record-fail-provision"
		}},
		{name: "create", do: func(t *testing.T, f *recordFailureFixture) (*httptest.ResponseRecorder, string) {
			f.fault.Arm()
			return doRequest(t, f.srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
				Name: "record-fail-create", ProjectID: f.project.ID, Task: "task",
			}), "record-fail-create"
		}},
		{name: "start", do: func(t *testing.T, f *recordFailureFixture) (*httptest.ResponseRecorder, string) {
			agent := f.agentIn(t, "record-fail-start", state.PhaseStopped)
			f.fault.Arm()
			return doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", nil), ""
		}},
		{name: "restart", do: func(t *testing.T, f *recordFailureFixture) (*httptest.ResponseRecorder, string) {
			agent := f.agentIn(t, "record-fail-restart", state.PhaseRunning)
			f.fault.Arm()
			return doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/restart", nil), ""
		}},
		{name: "reset-auth", do: func(t *testing.T, f *recordFailureFixture) (*httptest.ResponseRecorder, string) {
			agent := f.agentIn(t, "record-fail-reset-auth", state.PhaseRunning)
			f.fault.Arm()
			return doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/reset-auth", nil), ""
		}},
	}
	for _, site := range sites {
		t.Run(site.name, func(t *testing.T) {
			var first rawResponse
			for i, mode := range []agentRunScopeMode{agentRunScopeOff, agentRunScopeObserve, agentRunScopeEnforce} {
				t.Run(mode.String(), func(t *testing.T) {
					f := newRecordFailureFixture(t, mode)
					rec, created := site.do(t, f)

					got := rawOf(rec)
					assert.Equal(t, rawOf(want), got)
					if i == 0 {
						first = got
					} else {
						assert.Equal(t, first, got, "the response must not depend on the mode")
					}
					assert.NotContains(t, rec.Body.String(), errCredentialCreateForTest.Error())
					assert.False(t, f.brokerReceivedToken(), "no token may reach the broker")
					if created != "" {
						agents, err := f.store.ListAgents(context.Background(), store.AgentFilter{ProjectID: f.project.ID}, store.ListOptions{})
						require.NoError(t, err)
						for _, a := range agents.Items {
							assert.NotEqual(t, created, a.Name, "the failed create must be rolled back")
						}
					}
				})
			}
		})
	}
}

// TestAdminResetAuthAllRecordFailureIsFixed: the bulk reset-auth reports an
// agent whose token could not be recorded with the fixed message and
// internal_error, in every mode, without the store's error, and sends no
// token to the broker.
func TestAdminResetAuthAllRecordFailureIsFixed(t *testing.T) {
	var first rawResponse
	for i, mode := range []agentRunScopeMode{agentRunScopeOff, agentRunScopeObserve, agentRunScopeEnforce} {
		t.Run(mode.String(), func(t *testing.T) {
			f := newRecordFailureFixture(t, mode)
			agent := f.agentIn(t, "record-fail-reset-all", state.PhaseRunning)
			f.fault.Arm()

			rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/admin/agents/reset-auth-all", nil)

			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var body struct {
				Failed []struct {
					ID    string `json:"id"`
					Error string `json:"error"`
					Code  string `json:"code"`
				} `json:"failed"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
			require.Len(t, body.Failed, 1, rec.Body.String())
			assert.Equal(t, agent.ID, body.Failed[0].ID)
			assert.Equal(t, agentTokenRecordFailedMessage, body.Failed[0].Error)
			assert.Equal(t, ErrCodeInternalError, body.Failed[0].Code)
			assert.NotContains(t, rec.Body.String(), errCredentialCreateForTest.Error())
			assert.False(t, f.brokerReceivedToken(), "no token may reach the broker")

			got := rawOf(rec)
			if i == 0 {
				first = got
			} else {
				assert.Equal(t, first, got, "the response must not depend on the mode")
			}
		})
	}
}
