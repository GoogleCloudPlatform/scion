// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
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
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// A failed read of the agent's stored environment settings
// (resolveEnvFromStorage) follows the secret resolution policy of
// ptone/scion#4471: create, start, restart and reincarnate's start leg fail
// with a 503 and a fixed message and leave the agent as it was; provision and
// reprovision continue without the settings. Absence is not an error.

const fakeEnvReadBackendError = "fake env settings backend failure"

// envReadErrStore fails the stored env settings reads: ListEnvVars for the
// scopes in failScopes, or for every scope when failScopes is nil, unless
// scopesOK; and ListProgenyEnvVars when failProgeny.
type envReadErrStore struct {
	store.Store
	failScopes  map[string]bool
	scopesOK    bool
	failProgeny bool
}

func (s *envReadErrStore) ListEnvVars(ctx context.Context, filter store.EnvVarFilter) ([]store.EnvVar, error) {
	if !s.scopesOK && (s.failScopes == nil || s.failScopes[filter.Scope]) {
		return nil, errors.New(fakeEnvReadBackendError)
	}
	return s.Store.ListEnvVars(ctx, filter)
}

func (s *envReadErrStore) ListProgenyEnvVars(ctx context.Context, ancestry []string) ([]store.EnvVar, error) {
	if s.failProgeny {
		return nil, errors.New(fakeEnvReadBackendError)
	}
	return s.Store.ListProgenyEnvVars(ctx, ancestry)
}

func envReadFixedMessage(verb string) string {
	return "agent environment settings could not be read; the agent was not " + verb
}

// assertNoEnvReadDetail checks text carries no backend detail and no stored
// key or value.
func assertNoEnvReadDetail(t *testing.T, text, what string) {
	t.Helper()
	assert.NotContains(t, text, fakeEnvReadBackendError, "%s must not carry the backend error", what)
	assert.NotContains(t, text, "FAKE_STORED_KEY", "%s must not name a stored key", what)
	assert.NotContains(t, text, "fake-stored-value", "%s must not carry a stored value", what)
}

type envReadFixture struct {
	store  store.Store
	client *mockRuntimeBrokerClient
	d      *HTTPAgentDispatcher
	agent  *store.Agent
	logs   *bytes.Buffer
}

// newEnvReadFixture returns a dispatcher whose store is wrap(base) (base
// itself when wrap is nil), over a project with a broker provider, a stored
// user-scoped env setting, and an agent with two earlier runs.
func newEnvReadFixture(t *testing.T, wrap func(store.Store) store.Store) *envReadFixture {
	t.Helper()
	ctx := context.Background()
	s := createTestStore(t)
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: tid("project-envread"), Name: "envread-project", Slug: "envread-project",
		GitRemote: "https://github.com/example/envread.git",
	}))
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: tid("broker-envread"), Name: "envread-broker", Slug: "envread-broker",
		Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
	}))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: tid("project-envread"), BrokerID: tid("broker-envread"), BrokerName: "envread-broker",
		LocalPath: "/home/user/projects/envread/.scion", Status: store.BrokerStatusOnline,
	}))
	agent := &store.Agent{
		ID: tid("agent-envread"), Name: "envread-agent", Slug: "envread-agent",
		ProjectID: tid("project-envread"), OwnerID: tid("owner-envread"), RuntimeBrokerID: tid("broker-envread"),
		AppliedConfig: &store.AgentAppliedConfig{HarnessConfig: "claude", Task: "task"},
	}
	require.NoError(t, s.CreateAgent(ctx, agent))
	require.NoError(t, s.CreateEnvVar(ctx, &store.EnvVar{
		ID: tid("envvar-envread"), Key: "FAKE_STORED_KEY", Value: "fake-stored-value",
		Scope: store.ScopeUser, ScopeID: agent.OwnerID, InjectionMode: store.InjectionModeAlways,
	}))
	_, err := s.SetAgentRunID(ctx, agent.ID, "run-earlier", nil)
	require.NoError(t, err)
	_, err = s.SetAgentRunID(ctx, agent.ID, "run-before", nil)
	require.NoError(t, err)
	agent, err = s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)

	ds := s
	if wrap != nil {
		ds = wrap(s)
	}
	logs := &bytes.Buffer{}
	svc, err := NewAgentTokenService(AgentTokenConfig{})
	require.NoError(t, err)
	client := &mockRuntimeBrokerClient{}
	d := NewHTTPAgentDispatcherWithClient(ds, client, false, slog.New(slog.NewTextHandler(logs, nil)))
	d.SetTokenGenerator(runTokenGenerator{svc: svc})
	d.SetSecretBackend(&mockSecretBackend{})
	return &envReadFixture{store: s, client: client, d: d, agent: agent, logs: logs}
}

func failAllEnvReads(s store.Store) store.Store { return &envReadErrStore{Store: s} }

func (f *envReadFixture) brokerCalled() bool {
	return f.client.createCalled || f.client.startCalled || f.client.restartCalled
}

// Start and restart fail on a stored env settings read error with the env
// settings message, before any token is minted or broker call made.
// Reincarnation is covered end to end by
// TestEnvReadError_ReincarnateFailsAndRerenders.
func TestEnvReadError_StartRestartFail(t *testing.T) {
	cases := []struct {
		name     string
		verb     string
		dispatch func(f *envReadFixture) error
	}{
		{"start", "started", func(f *envReadFixture) error {
			return f.d.DispatchAgentStart(context.Background(), f.agent, "", false)
		}},
		{"restart", "restarted", func(f *envReadFixture) error {
			return f.d.DispatchAgentRestart(context.Background(), f.agent)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newEnvReadFixture(t, failAllEnvReads)

			err := tc.dispatch(f)
			var resErr *secretResolutionError
			require.ErrorAs(t, err, &resErr)
			assert.Equal(t, tc.verb, resErr.Verb)
			assert.Equal(t, resolutionSourceEnvSettings, resErr.Source)
			assert.Equal(t, envReadFixedMessage(tc.verb), err.Error())
			assertNoEnvReadDetail(t, err.Error(), "the error text")
			assert.True(t, isConfirmedStartNotActedOnError(err))
			assert.Equal(t, startReleased, startOutcomeOf(err), "the claim is released")
			assert.False(t, f.brokerCalled(), "nothing is sent to the broker")

			got := getAgent(t, f.store, f.agent.ID)
			assert.Equal(t, "run-before", got.RunID, "no run is begun, so no credential is issued")
		})
	}
}

// A failed progeny list read follows the same rule as a scope read.
func TestEnvReadError_ProgenyReadFailsStart(t *testing.T) {
	f := newEnvReadFixture(t, func(s store.Store) store.Store {
		return &envReadErrStore{Store: s, scopesOK: true, failProgeny: true}
	})
	f.agent.Ancestry = []string{f.agent.OwnerID, tid("parent-envread")}

	err := f.d.DispatchAgentStart(context.Background(), f.agent, "", false)
	var resErr *secretResolutionError
	require.ErrorAs(t, err, &resErr)
	assert.Equal(t, envReadFixedMessage("started"), err.Error())
	assert.False(t, f.brokerCalled())
}

// Create, create-with-gather and finalize-env fail with the "not created"
// env settings message before any broker request and before a run begins.
func TestEnvReadError_CreateFailsBeforeBroker(t *testing.T) {
	cases := []struct {
		name     string
		dispatch func(f *envReadFixture) error
	}{
		{"create", func(f *envReadFixture) error {
			_, err := f.d.DispatchAgentCreate(context.Background(), f.agent)
			return err
		}},
		{"create with env gather", func(f *envReadFixture) error {
			_, err := f.d.DispatchAgentCreateWithGather(context.Background(), f.agent)
			return err
		}},
		{"finalize env", func(f *envReadFixture) error {
			_, err := f.d.DispatchFinalizeEnv(context.Background(), f.agent, nil)
			return err
		}},
		{"create with no auth", func(f *envReadFixture) error {
			// The stored env settings read is not skipped under NoAuth
			// (unlike secrets), so a read error still fails the create.
			f.agent.AppliedConfig.NoAuth = true
			_, err := f.d.DispatchAgentCreate(context.Background(), f.agent)
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newEnvReadFixture(t, failAllEnvReads)

			err := tc.dispatch(f)
			var resErr *secretResolutionError
			require.ErrorAs(t, err, &resErr)
			assert.Equal(t, envReadFixedMessage("created"), err.Error())
			assertNoEnvReadDetail(t, err.Error(), "the error text")
			assert.False(t, f.brokerCalled(), "nothing is sent to the broker")
			got := getAgent(t, f.store, f.agent.ID)
			assert.Equal(t, "run-before", got.RunID, "no run is begun")
		})
	}
}

// A stored env settings read error on reincarnation's start leg fails the
// reincarnation as a start that did not happen: the previous config is
// re-rendered and restored on the row, and the recorded error and agent
// message carry the env settings fixed message only, with no backend detail.
func TestEnvReadError_ReincarnateFailsAndRerenders(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	disp.startErr = &secretResolutionError{Verb: "started", Source: resolutionSourceEnvSettings, Err: errors.New(fakeEnvReadBackendError)}
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	self := agentIdentityFor(agent.ID, project.ID)

	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: "h"}), agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	r := waitForReincarnationSettled(t, s, agent.ID)
	require.Equal(t, store.AgentReincarnationStateFailed, r.State)
	assert.Contains(t, r.Error, "start failed: "+envReadFixedMessage("started"))
	assertNoEnvReadDetail(t, r.Error, "the reincarnation error")

	calls, cfgs := disp.reprovisionSnapshot()
	require.Equal(t, 2, calls, "one reprovision, one re-render of the previous config")
	require.NotNil(t, r.PreviousAppliedConfig)
	assert.Equal(t, *reincarnationRecoveryConfig(r.PreviousAppliedConfig, r.ToGeneration, r.Handoff, store.AgentReincarnationStateStarting), cfgs[1])

	final, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, final.Generation)
	assert.Equal(t, "old-image:v1", final.AppliedConfig.Image, "a successful re-render restores previous on the row")
	assertNoEnvReadDetail(t, final.Message, "the agent message")
}

// Provision and reprovision continue on a stored env settings read error
// and send the provision request without the stored settings.
func TestEnvReadError_ProvisionContinues(t *testing.T) {
	cases := []struct {
		name     string
		dispatch func(f *envReadFixture) error
	}{
		{"provision", func(f *envReadFixture) error {
			return f.d.DispatchAgentProvision(context.Background(), f.agent)
		}},
		{"reprovision", func(f *envReadFixture) error {
			return f.d.DispatchAgentReprovision(context.Background(), f.agent)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newEnvReadFixture(t, failAllEnvReads)
			require.NoError(t, tc.dispatch(f))
			require.True(t, f.client.createCalled, "the provision request is sent")
			require.NotNil(t, f.client.lastCreateReq)
			assert.True(t, f.client.lastCreateReq.ProvisionOnly)
			assert.NotContains(t, f.client.lastCreateReq.ResolvedEnv, "FAKE_STORED_KEY")
			assert.Contains(t, f.logs.String(), "failed to read stored env settings; provisioning without them")
		})
	}
}

// The hub log carries the scope and the backend error, never a stored key
// or value.
func TestEnvReadError_HubLogNamesScopeOnly(t *testing.T) {
	f := newEnvReadFixture(t, func(s store.Store) store.Store {
		return &envReadErrStore{Store: s, failScopes: map[string]bool{store.ScopeProject: true}}
	})
	err := f.d.DispatchAgentStart(context.Background(), f.agent, "", false)
	require.Error(t, err)

	logs := f.logs.String()
	assert.Contains(t, logs, "failed to read stored env settings; agent not started")
	assert.Contains(t, logs, "list project env vars: "+fakeEnvReadBackendError)
	assert.NotContains(t, logs, "FAKE_STORED_KEY")
	assert.NotContains(t, logs, "fake-stored-value")
}

// Absence is not an error: with no failing reads, create, start, restart
// and an agent with ancestry but no progeny settings all reach the broker,
// and the stored setting is sent.
func TestEnvReadError_NoReadErrorStartsNormally(t *testing.T) {
	f := newEnvReadFixture(t, nil)
	f.agent.Ancestry = []string{f.agent.OwnerID, tid("parent-envread")}
	ctx := context.Background()

	require.NoError(t, f.d.DispatchAgentStart(ctx, f.agent, "", false))
	assert.True(t, f.client.startCalled)
	assert.Equal(t, "fake-stored-value", f.client.lastResolvedEnv["FAKE_STORED_KEY"])
	require.NoError(t, f.d.DispatchAgentRestart(ctx, f.agent))
	assert.True(t, f.client.restartCalled)
	_, err := f.d.DispatchAgentCreate(ctx, f.agent)
	require.NoError(t, err)
	assert.True(t, f.client.createCalled)
}

// An agent with no stored settings, and scopes with an empty scope ID,
// resolve to empty maps without error.
func TestEnvReadError_NoStoredSettingsIsNotAnError(t *testing.T) {
	d := NewHTTPAgentDispatcherWithClient(createTestStore(t), &mockRuntimeBrokerClient{}, false, slog.Default())
	for _, agent := range []*store.Agent{
		envScopeTestAgent(),
		{ID: "agent-envread-empty", AppliedConfig: &store.AgentAppliedConfig{}},
	} {
		env, plain, err := d.resolveEnvFromStorage(context.Background(), agent)
		require.NoError(t, err)
		assert.Empty(t, env)
		assert.Empty(t, plain)
	}

	f := newEnvReadFixture(t, nil)
	require.NoError(t, f.store.DeleteEnvVar(context.Background(), "FAKE_STORED_KEY", store.ScopeUser, f.agent.OwnerID))
	require.NoError(t, f.d.DispatchAgentStart(context.Background(), f.agent, "", false))
	assert.True(t, f.client.startCalled, "an agent with no stored settings starts")
}

// newEnvReadServer returns a server whose real dispatcher reads env settings
// through a failing store, and a stopped agent in good standing.
func newEnvReadServer(t *testing.T, phase state.Phase) (*Server, store.Store, *store.Agent, *mockRuntimeBrokerClient) {
	t.Helper()
	ctx := context.Background()
	srv, s := testServer(t)
	broker := &store.RuntimeBroker{
		ID: tid("envread-broker-" + t.Name()), Name: "envread-broker", Slug: "envread-broker-" + tidSlugSafe(t.Name()),
		Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	project := &store.Project{
		ID: tid("envread-project-" + t.Name()), Name: "envread-project", Slug: "envread-project-" + tidSlugSafe(t.Name()),
		DefaultRuntimeBrokerID: broker.ID,
	}
	require.NoError(t, s.CreateProject(ctx, project))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: project.ID, BrokerID: broker.ID, BrokerName: broker.Name, Status: broker.Status,
	}))
	ensureStandingRoot(t, s, project.ID, tid("envread-user"))
	agent := &store.Agent{
		ID: tid("envread-agent-" + t.Name()), Name: "envread-agent", Slug: "envread-agent",
		ProjectID: project.ID, OwnerID: tid("envread-user"), RuntimeBrokerID: broker.ID,
		Phase: string(phase), AppliedConfig: &store.AgentAppliedConfig{HarnessConfig: "claude"},
	}
	require.NoError(t, s.CreateAgent(ctx, agent))
	client := &mockRuntimeBrokerClient{}
	d := NewHTTPAgentDispatcherWithClient(&envReadErrStore{Store: s}, client, false, slog.Default())
	d.SetSecretBackend(&mockSecretBackend{})
	srv.SetDispatcher(d)
	return srv, s, agent, client
}

// Through the lifecycle handler, a start or restart that fails on a stored
// env settings read is a 503 unavailable with the env settings message; the
// claim is released and the stopped agent stays stopped.
func TestEnvReadError_LifecycleHandler_503AgentUnchanged(t *testing.T) {
	for _, action := range []string{"start", "restart"} {
		t.Run(action, func(t *testing.T) {
			srv, s, agent, client := newEnvReadServer(t, state.PhaseStopped)
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/"+action, nil)
			require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())

			var body map[string]interface{}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			e, _ := body["error"].(map[string]interface{})
			assert.Equal(t, ErrCodeUnavailable, e["code"])
			msg, _ := e["message"].(string)
			assert.Contains(t, msg, "agent environment settings could not be read; the agent was not ")
			assert.NotContains(t, msg, "secrets")
			assertNoEnvReadDetail(t, rec.Body.String(), "the API body")
			assert.False(t, client.startCalled || client.restartCalled || client.createCalled, "broker must not be asked to start")

			got := getAgent(t, s, agent.ID)
			assert.Equal(t, string(state.PhaseStopped), got.Phase, "the agent keeps its previous state")
			assert.Empty(t, got.StartClaimID, "the claim is released")
			assertNoEnvReadDetail(t, got.Message, "the agent message")
		})
	}
}

// Waking a suspended agent for a DM: 503 with the env settings message, no
// broker call, agent back at rest with its claim released.
func TestEnvReadError_WakeAgentForDM_503(t *testing.T) {
	srv, s, agent, client := newEnvReadServer(t, state.PhaseSuspended)

	_, dmErr := srv.wakeAgentForDM(context.Background(), agent)
	require.NotNil(t, dmErr)
	assert.Equal(t, http.StatusServiceUnavailable, dmErr.HTTPStatus, dmErr.Message)
	assert.Equal(t, ErrCodeUnavailable, dmErr.Code)
	assert.Equal(t, envReadFixedMessage("started"), dmErr.Message)
	assert.False(t, client.startCalled || client.createCalled, "broker must not be called")

	got := getAgent(t, s, agent.ID)
	assert.Equal(t, string(state.PhaseSuspended), got.Phase, "the agent is back at rest")
	assert.Empty(t, got.StartClaimID)
}

// The source travels on a failed cross-node dispatch row, so the requesting
// node rebuilds the env settings message; a row without it (or from a node
// that predates the field) rebuilds the secrets message.
func TestDispatchFailureResult_EnvSettingsSource(t *testing.T) {
	execErr := &secretResolutionError{Verb: "started", Source: resolutionSourceEnvSettings, Err: errors.New(fakeEnvReadBackendError)}
	result := dispatchFailureResult(execErr)
	assert.JSONEq(t, `{"secretResolution":true,"secretResolutionSource":"env_settings"}`, result)

	err := dispatchFailureError(&store.BrokerDispatch{Op: "restart", Error: execErr.Error(), Result: result})
	var resErr *secretResolutionError
	require.ErrorAs(t, err, &resErr)
	assert.Equal(t, resolutionSourceEnvSettings, resErr.Source)
	assert.Equal(t, "dispatch restart failed: "+envReadFixedMessage("restarted"), err.Error())

	secretsResult := dispatchFailureResult(&secretResolutionError{Verb: "started", Err: errors.New("x")})
	assert.JSONEq(t, `{"secretResolution":true}`, secretsResult)
	err = dispatchFailureError(&store.BrokerDispatch{Op: "start", Result: secretsResult})
	assert.Equal(t, "dispatch start failed: agent secrets could not be resolved; the agent was not started", err.Error())
}
