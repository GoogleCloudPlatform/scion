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
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// credentialRecordFaultStore refuses to record an agent credential while
// armed: on its own (CreateAgentCredential) and with a run
// (SetAgentRunID with a credential), as the store reports it. When
// onlyAgent is set only that agent's credentials are refused; cause, when
// set, replaces the refusal error. The JTI hashes of refused credentials
// are kept.
type credentialRecordFaultStore struct {
	store.Store
	fault     *storeFaultSwitch
	onlyAgent string
	cause     error

	mu      sync.Mutex
	refused []string
}

func (f *credentialRecordFaultStore) CreateAgentCredential(ctx context.Context, cred *store.AgentCredential) error {
	if f.fault.Active() && (f.onlyAgent == "" || cred.AgentID == f.onlyAgent) {
		f.mu.Lock()
		f.refused = append(f.refused, cred.TokenJTIHash)
		f.mu.Unlock()
		if f.cause != nil {
			return f.cause
		}
		return errCredentialCreateForTest
	}
	return f.Store.CreateAgentCredential(ctx, cred)
}

func (f *credentialRecordFaultStore) refusedHashes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.refused...)
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
	faulty  *credentialRecordFaultStore
	svc     *AgentTokenService
}

func newRecordFailureFixture(t *testing.T, mode agentRunScopeMode) *recordFailureFixture {
	t.Helper()
	ctx := context.Background()
	srv, s, faulty, fault := testServerWithStoreFault(t, func(inner store.Store, fault *storeFaultSwitch) *credentialRecordFaultStore {
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
	return &recordFailureFixture{srv: srv, store: s, client: client, project: project, fault: fault, faulty: faulty, svc: svc}
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

// TestAgentTokenRecordFailureIsFixed500: at every mint site (create,
// provision, env gather and finalize, workspace start, start, restart, the
// three starts of an existing agent through create, and reset-auth), a
// token whose credential cannot be recorded is not issued. The request is
// answered with the same 500 and fixed message in every run-scope mode,
// without the store's error, and nothing carrying a token reaches the
// broker. A failed create is rolled back. Refresh is covered by
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
		{name: "create with env gather", do: func(t *testing.T, f *recordFailureFixture) (*httptest.ResponseRecorder, string) {
			f.fault.Arm()
			return doRequest(t, f.srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
				Name: "record-fail-gather", ProjectID: f.project.ID, Task: "task", GatherEnv: true,
			}), "record-fail-gather"
		}},
		{name: "finalize env", do: func(t *testing.T, f *recordFailureFixture) (*httptest.ResponseRecorder, string) {
			agent := f.agentIn(t, "record-fail-env", state.PhaseProvisioning)
			f.fault.Arm()
			return doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/env",
				SubmitEnvRequest{Env: map[string]string{"SOME_KEY": "v"}}), ""
		}},
		{name: "workspace start", do: func(t *testing.T, f *recordFailureFixture) (*httptest.ResponseRecorder, string) {
			f.srv.SetStorage(newContentMockStorage("test-bucket"))
			agent := f.agentIn(t, "record-fail-workspace", state.PhaseProvisioning)
			f.fault.Arm()
			return doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/workspace/sync-to/finalize",
				SyncToFinalizeRequest{Manifest: &transfer.Manifest{Version: "1.0"}}), ""
		}},
		{name: "existing agent, suspended", do: func(t *testing.T, f *recordFailureFixture) (*httptest.ResponseRecorder, string) {
			f.agentIn(t, "record-fail-existing-suspended", state.PhaseSuspended)
			f.fault.Arm()
			return doRequest(t, f.srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
				Name: "record-fail-existing-suspended", ProjectID: f.project.ID, Task: "task",
			}), ""
		}},
		{name: "existing agent, resume in place", do: func(t *testing.T, f *recordFailureFixture) (*httptest.ResponseRecorder, string) {
			f.agentIn(t, "record-fail-existing-stopped", state.PhaseStopped)
			f.fault.Arm()
			return doRequest(t, f.srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
				Name: "record-fail-existing-stopped", ProjectID: f.project.ID, Task: "task", Resume: true,
			}), ""
		}},
		{name: "existing agent, provisioned", do: func(t *testing.T, f *recordFailureFixture) (*httptest.ResponseRecorder, string) {
			f.agentIn(t, "record-fail-existing-provisioned", state.PhaseProvisioning)
			f.fault.Arm()
			return doRequest(t, f.srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
				Name: "record-fail-existing-provisioned", ProjectID: f.project.ID, Task: "task",
			}), ""
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

// resetAuthRecordingClient records the agent token each reset-auth hands
// to the broker and, at the moment of the call, whether that token's
// credential was already recorded. It is safe for the bulk reset-auth's
// concurrent dispatches.
type resetAuthRecordingClient struct {
	*mockRuntimeBrokerClient
	store store.Store
	svc   *AgentTokenService

	mu       sync.Mutex
	tokens   map[string]string // agent slug -> token
	recorded map[string]bool   // agent slug -> credential stored before the call
}

func (c *resetAuthRecordingClient) ResetAuthAgent(ctx context.Context, _, _, agentSlug, _, token, _ string) error {
	stored := false
	if claims, err := c.svc.ValidateAgentToken(token); err == nil {
		_, err := c.store.GetAgentCredentialByJTIHash(ctx, hashJTI(claims.ID))
		stored = err == nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tokens[agentSlug] = token
	c.recorded[agentSlug] = stored
	return nil
}

// TestAdminResetAuthAllRecordFailureIsFixed: in a bulk reset-auth, an agent
// whose token could not be recorded is reported with the fixed message and
// internal_error, and its entry carries nothing else: no token and no
// cause. The entry is byte for byte the same whatever the record failure
// and in every mode. No token for it reaches the broker, and every token
// that does reach the broker was recorded before it was handed out.
func TestAdminResetAuthAllRecordFailureIsFixed(t *testing.T) {
	causes := []struct {
		name string
		err  error
	}{
		{name: "insert refused", err: errCredentialCreateForTest},
		{name: "duplicate", err: fmt.Errorf("%w: agent_credentials.token_jti_hash", store.ErrAlreadyExists)},
		{name: "invalid input", err: fmt.Errorf("%w: agent_credentials violates check", store.ErrInvalidInput)},
		{name: "not found", err: store.ErrNotFound},
		{name: "deadline", err: context.DeadlineExceeded},
		{name: "closed", err: errors.New("sql: database is closed")},
	}
	modes := []agentRunScopeMode{agentRunScopeOff, agentRunScopeObserve, agentRunScopeEnforce}

	var firstEntry string
	var firstName string
	for _, cause := range causes {
		for _, mode := range modes {
			name := cause.name + "/" + mode.String()
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				f := newRecordFailureFixture(t, mode)
				failing := f.agentIn(t, "record-fail-reset-all", state.PhaseRunning)
				others := []*store.Agent{
					f.agentIn(t, "record-ok-reset-all-1", state.PhaseRunning),
					f.agentIn(t, "record-ok-reset-all-2", state.PhaseRunning),
				}
				client := &resetAuthRecordingClient{
					mockRuntimeBrokerClient: f.client, store: f.store, svc: f.svc,
					tokens: map[string]string{}, recorded: map[string]bool{},
				}
				d := NewHTTPAgentDispatcherWithClient(f.srv.store, client, false, slog.Default())
				d.SetTokenGenerator(runTokenGenerator{svc: f.svc})
				f.srv.SetDispatcher(d)
				f.faulty.onlyAgent = failing.ID
				f.faulty.cause = cause.err
				f.fault.Arm()

				rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/admin/agents/reset-auth-all", nil)

				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				var body struct {
					Failed    []json.RawMessage `json:"failed"`
					Succeeded []struct {
						ID string `json:"id"`
					} `json:"succeeded"`
				}
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
				require.Len(t, body.Failed, 1, rec.Body.String())
				require.Len(t, body.Succeeded, len(others), rec.Body.String())

				// The entry holds exactly id, name, the fixed message and the code.
				var entry map[string]any
				require.NoError(t, json.Unmarshal(body.Failed[0], &entry))
				assert.Equal(t, map[string]any{
					"id": failing.ID, "name": failing.Name,
					"error": agentTokenRecordFailedMessage, "code": ErrCodeInternalError,
				}, entry)
				assert.NotContains(t, rec.Body.String(), cause.err.Error())
				assert.NotContains(t, rec.Body.String(), "eyJ", "no token in the response")

				entryRaw := string(body.Failed[0])
				if firstEntry == "" {
					firstEntry, firstName = entryRaw, name
				} else {
					assert.Equal(t, firstEntry, entryRaw, "entry differs from %s", firstName)
				}

				// The failing agent's token was signed, refused and never
				// recorded or handed out.
				refused := f.faulty.refusedHashes()
				require.NotEmpty(t, refused, "the failing agent's record was attempted")
				for _, h := range refused {
					_, err := f.store.GetAgentCredentialByJTIHash(ctx, h)
					assert.ErrorIs(t, err, store.ErrNotFound, "a refused credential is not stored")
				}
				client.mu.Lock()
				defer client.mu.Unlock()
				_, sent := client.tokens[failing.Slug]
				assert.False(t, sent, "no token for the failing agent reaches the broker")

				// Every other agent's token was recorded before the broker got it.
				require.Len(t, client.tokens, len(others))
				for _, a := range others {
					tok, ok := client.tokens[a.Slug]
					require.True(t, ok, "agent %s was reset", a.Slug)
					assert.True(t, client.recorded[a.Slug], "agent %s: credential recorded before the token was sent", a.Slug)
					claims, err := f.svc.ValidateAgentToken(tok)
					require.NoError(t, err)
					for _, h := range refused {
						assert.NotEqual(t, h, hashJTI(claims.ID), "a refused credential's token was sent")
					}
				}
			})
		}
	}
}
