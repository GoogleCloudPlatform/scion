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
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// A failed read of a storage-scope TZ must never let a lower-precedence
// rung (a lower scope, the hub default or none) stand in for the TZ the hub
// could not read. Dispatches fail with the env settings
// *secretResolutionError (provision sends no TZ); hub handlers answer 503
// with the fixed EnvSettingsUnavailableText body.

const tzReadBackendError = "simulated TZ read failure"

// The TZ values the tests seed. The lower-precedence ones must never reach
// a broker or a response while a higher scope's read fails.
const (
	tzErrProjectTZ    = "America/Denver"
	tzErrHubDefaultTZ = "Asia/Tokyo"
)

// tzKeyErrStore fails only the TZ storage rung's reads: ListEnvVars filtered
// on the TZ key at scope (any scope when scope is empty), once armed. The
// general env settings reads, which list a scope without a key filter, pass
// through, so the TZ path is tested on its own.
type tzKeyErrStore struct {
	store.Store
	scope string
	armed atomic.Bool
}

func (s *tzKeyErrStore) ListEnvVars(ctx context.Context, filter store.EnvVarFilter) ([]store.EnvVar, error) {
	if s.armed.Load() && filter.Key == agentTZEnvKey && (s.scope == "" || filter.Scope == s.scope) {
		return nil, errors.New(tzReadBackendError)
	}
	return s.Store.ListEnvVars(ctx, filter)
}

// newTZStoreErrFixture returns an envReadFixture whose dispatcher store
// fails the user-scope TZ read (armed when armed is true), with a usable
// project-scope TZ and a hub default below it.
func newTZStoreErrFixture(t *testing.T, armed bool) (*envReadFixture, *tzKeyErrStore) {
	t.Helper()
	var es *tzKeyErrStore
	f := newEnvReadFixture(t, func(s store.Store) store.Store {
		es = &tzKeyErrStore{Store: s, scope: store.ScopeUser}
		es.armed.Store(armed)
		return es
	})
	tzTestEnvVar(t, f.store, store.EnvVar{Value: tzErrProjectTZ, Scope: store.ScopeProject, ScopeID: f.agent.ProjectID})
	f.d.SetHubAgentDefaultsProvider(func() opsettings.AgentDefaultsSettings {
		return opsettings.AgentDefaultsSettings{DefaultTimezone: tzErrHubDefaultTZ}
	})
	return f, es
}

// assertNoTZErrDetail checks text carries no backend error, scope name or
// lower-precedence zone.
func assertNoTZErrDetail(t *testing.T, text, what string) {
	t.Helper()
	assert.NotContains(t, text, tzReadBackendError, "%s must not carry the backend error", what)
	assert.NotContains(t, text, tzErrProjectTZ, "%s must not carry a lower-precedence zone", what)
	assert.NotContains(t, text, tzErrHubDefaultTZ, "%s must not carry the hub default zone", what)
	assert.NotContains(t, text, store.ScopeUser, "%s must not name the failed scope", what)
}

// TestAgentTZStoreError_DispatchFailsClosed: create, create-with-gather,
// finalize-env, start and restart fail on a higher-scope TZ read error with
// the fixed env settings message, before anything reaches the broker.
func TestAgentTZStoreError_DispatchFailsClosed(t *testing.T) {
	cases := []struct {
		name     string
		verb     string
		dispatch func(f *envReadFixture) error
	}{
		{"create", "created", func(f *envReadFixture) error {
			_, err := f.d.DispatchAgentCreate(context.Background(), f.agent)
			return err
		}},
		{"create with env gather", "created", func(f *envReadFixture) error {
			_, err := f.d.DispatchAgentCreateWithGather(context.Background(), f.agent)
			return err
		}},
		{"finalize env", "created", func(f *envReadFixture) error {
			_, err := f.d.DispatchFinalizeEnv(context.Background(), f.agent, nil)
			return err
		}},
		{"start", "started", func(f *envReadFixture) error {
			return f.d.DispatchAgentStart(context.Background(), f.agent, "", false)
		}},
		{"restart", "restarted", func(f *envReadFixture) error {
			return f.d.DispatchAgentRestart(context.Background(), f.agent)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := newTZStoreErrFixture(t, true)

			err := tc.dispatch(f)
			var resErr *secretResolutionError
			require.ErrorAs(t, err, &resErr)
			assert.Equal(t, tc.verb, resErr.Verb)
			assert.Equal(t, resolutionSourceEnvSettings, resErr.Source)
			assert.Equal(t, envReadFixedMessage(tc.verb), err.Error())
			assertNoTZErrDetail(t, err.Error(), "the error text")
			assert.False(t, f.brokerCalled(), "nothing is sent to the broker, so no lower-precedence TZ is used")
			assertTZLogScopeOnly(t, f.logs.String())
		})
	}
}

// TestAgentTZStoreError_ProvisionSendsNoTZ: provision continues on a TZ
// read error, like on a stored env settings read error, and sends no TZ:
// neither the lower-precedence project TZ nor the hub default.
func TestAgentTZStoreError_ProvisionSendsNoTZ(t *testing.T) {
	f, _ := newTZStoreErrFixture(t, true)
	require.NoError(t, f.d.DispatchAgentProvision(context.Background(), f.agent))
	require.NotNil(t, f.client.lastCreateReq)
	assert.NotContains(t, f.client.lastCreateReq.ResolvedEnv, agentTZEnvKey)
	assert.Contains(t, f.logs.String(), "failed to read stored TZ settings; provisioning without TZ")
	assertTZLogScopeOnly(t, f.logs.String())
}

// TestAgentTZStoreError_GatherAnswer covers the TZ need an older broker
// reports: the read fails only on the replay (the fake broker arms the
// store), so finalize-env fails closed and provision answers with the no-TZ
// placeholder, never the lower-precedence zones.
func TestAgentTZStoreError_GatherAnswer(t *testing.T) {
	setup := func(t *testing.T) (*envReadFixture, *[]*RemoteCreateAgentRequest) {
		f, es := newTZStoreErrFixture(t, false)
		// No TZ in the first request, so the old broker asks for one.
		f.d.SetHubAgentDefaultsProvider(func() opsettings.AgentDefaultsSettings { return opsettings.AgentDefaultsSettings{} })
		require.NoError(t, f.store.DeleteEnvVar(context.Background(), agentTZEnvKey, store.ScopeProject, f.agent.ProjectID))
		var reqs []*RemoteCreateAgentRequest
		old := oldBrokerReportingTZ(&reqs)
		f.client.createWithGatherFunc = func(ctx context.Context, brokerID, endpoint string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
			es.armed.Store(true)
			f.d.SetHubAgentDefaultsProvider(func() opsettings.AgentDefaultsSettings {
				return opsettings.AgentDefaultsSettings{DefaultTimezone: tzErrHubDefaultTZ}
			})
			return old(ctx, brokerID, endpoint, req)
		}
		return f, &reqs
	}

	t.Run("finalize env fails closed", func(t *testing.T) {
		f, reqs := setup(t)
		_, err := f.d.DispatchFinalizeEnv(context.Background(), f.agent, nil)
		var resErr *secretResolutionError
		require.ErrorAs(t, err, &resErr)
		assert.Equal(t, envReadFixedMessage("created"), err.Error())
		assertNoTZErrDetail(t, err.Error(), "the error text")
		assert.Len(t, *reqs, 1, "no replay is sent")
		assertTZLogScopeOnly(t, f.logs.String())
	})

	t.Run("provision answers with the placeholder", func(t *testing.T) {
		f, reqs := setup(t)
		require.NoError(t, f.d.DispatchAgentProvision(context.Background(), f.agent))
		require.GreaterOrEqual(t, len(*reqs), 2, "the TZ need is answered with a replay")
		assert.Equal(t, gatherAnswerTZ, (*reqs)[len(*reqs)-1].ResolvedEnv[agentTZEnvKey])
	})
}

// armTZStoreError seeds a usable project-scope TZ for agent and the hub
// default below it, then makes the server fail the user-scope TZ read: on
// its own store, and on its HTTP dispatcher's when one is installed
// (Server.agentTZ resolves through it).
func armTZStoreError(t *testing.T, srv *Server, s store.Store, agent *store.Agent) {
	t.Helper()
	tzTestEnvVar(t, s, store.EnvVar{Value: tzErrProjectTZ, Scope: store.ScopeProject, ScopeID: agent.ProjectID})
	setHubAgentDefaults(srv, opsettings.AgentDefaultsSettings{DefaultTimezone: tzErrHubDefaultTZ})
	es := &tzKeyErrStore{Store: s, scope: store.ScopeUser}
	es.armed.Store(true)
	srv.store = es
	t.Cleanup(func() { srv.store = s })
	if d, ok := srv.GetDispatcher().(*HTTPAgentDispatcher); ok && d != nil {
		prev := d.store
		d.store = es
		t.Cleanup(func() { d.store = prev })
	}
}

// tzStoreErrServer is tzPatchServer with armTZStoreError applied.
func tzStoreErrServer(t *testing.T, phase string, mutate func(a *store.Agent)) (*Server, store.Store, *store.Agent) {
	t.Helper()
	srv, s, agent := tzPatchServer(t, phase, mutate)
	armTZStoreError(t, srv, s, agent)
	return srv, s, agent
}

// assertEnvSettingsUnavailable checks a 503 with exactly the fixed body text.
func assertEnvSettingsUnavailable(t *testing.T, code int, body []byte) {
	t.Helper()
	require.Equal(t, http.StatusServiceUnavailable, code, string(body))
	var resp struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(body, &resp), string(body))
	assert.Equal(t, ErrCodeUnavailable, resp.Error.Code)
	assert.Equal(t, EnvSettingsUnavailableText, resp.Error.Message)
	assertNoTZErrDetail(t, string(body), "the 503 body")
}

// TestAgentTZStoreError_PatchAnswers503NothingWritten: an agent PATCH whose
// resolved TZ needs the failed read answers 503 with the fixed body, reports
// no lower-precedence zone and writes nothing.
func TestAgentTZStoreError_PatchAnswers503NothingWritten(t *testing.T) {
	for _, phase := range []string{string(state.PhaseStopped), string(state.PhaseRunning)} {
		t.Run(phase, func(t *testing.T) {
			srv, s, agent := tzStoreErrServer(t, phase, func(a *store.Agent) {
				a.AppliedConfig.ExplicitTimezone = "Europe/Paris"
			})
			rec := doRequest(t, srv, http.MethodPatch, "/api/v1/agents/"+agent.ID,
				map[string]interface{}{"explicitTimezone": "", "taskSummary": "changed"})
			assertEnvSettingsUnavailable(t, rec.Code, rec.Body.Bytes())

			got, err := s.GetAgent(context.Background(), agent.ID)
			require.NoError(t, err)
			assert.Equal(t, "Europe/Paris", got.AppliedConfig.ExplicitTimezone, "the unpin is not written")
			assert.NotEqual(t, "changed", got.TaskSummary, "nothing is written")
		})
	}
}

// TestAgentTZStoreError_GetAnswers503: the agent detail GET shows the
// resolved zone to a caller who may view the agent's env (the owner, as in
// TestAgentGet_ResolvedTimezoneGatedOnEnvView); it answers 503 with the
// fixed body rather than a lower rung.
func TestAgentTZStoreError_GetAnswers503(t *testing.T) {
	srv, s, alice, _, project := setupDemoPolicyTest(t)
	agent := &store.Agent{
		ID:            tid("agent-get-tz-err"),
		Slug:          "agent-get-tz-err",
		Name:          "Get TZ Error Agent",
		ProjectID:     project.ID,
		OwnerID:       alice.ID,
		Phase:         string(state.PhaseStopped),
		AppliedConfig: &store.AgentAppliedConfig{},
	}
	require.NoError(t, s.CreateAgent(context.Background(), agent))
	armTZStoreError(t, srv, s, agent)

	rec := doRequestAsUser(t, srv, alice, http.MethodGet, "/api/v1/agents/"+agent.ID, nil)
	assertEnvSettingsUnavailable(t, rec.Code, rec.Body.Bytes())
}

// TestAgentTZStoreError_EnvGatherOmitsTZLabel: the env-gather provenance is
// advisory, so a failed TZ read omits the TZ label rather than failing the
// response or reporting a lower rung.
func TestAgentTZStoreError_EnvGatherOmitsTZLabel(t *testing.T) {
	srv, _, agent := tzStoreErrServer(t, string(state.PhaseStopped), nil)
	resp := srv.buildEnvGatherResponse(context.Background(), agent, &RemoteEnvRequirementsResponse{
		AgentID:  agent.ID,
		Required: []string{agentTZEnvKey},
		HubHas:   []string{agentTZEnvKey},
	})
	for _, src := range resp.HubHas {
		assert.NotEqual(t, agentTZEnvKey, src.Key, "no TZ label after a failed read, got %+v", src)
	}
}

// TestAgentTZStoreError_HappyPathResolves: with every read succeeding, the
// same setup resolves the highest scope holding a TZ on dispatch and on the
// PATCH response.
func TestAgentTZStoreError_HappyPathResolves(t *testing.T) {
	f, _ := newTZStoreErrFixture(t, false)
	_, err := f.d.DispatchAgentCreate(context.Background(), f.agent)
	require.NoError(t, err)
	require.NotNil(t, f.client.lastCreateReq)
	assert.Equal(t, tzErrProjectTZ, f.client.lastCreateReq.ResolvedEnv[agentTZEnvKey])

	srv, s, agent := tzPatchServer(t, string(state.PhaseStopped), nil)
	tzTestEnvVar(t, s, store.EnvVar{Value: tzErrProjectTZ, Scope: store.ScopeProject, ScopeID: agent.ProjectID})
	code, resp := patchAgentTZ(t, srv, agent.ID, map[string]interface{}{"taskSummary": "hello"})
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, tzErrProjectTZ, *resp.ResolvedTimezone)
	assert.Equal(t, TZSourceProject, resp.TimezoneSource)
}

// assertTZLogScopeOnly checks the hub log names the failed scope kind and
// carries no backend error text (sec-review condition: a TZ read warning
// logs a fixed reason and the scope kind only).
func assertTZLogScopeOnly(t *testing.T, logs string) {
	t.Helper()
	assert.Contains(t, logs, "failed to read stored TZ settings")
	assert.Contains(t, logs, "scope="+store.ScopeUser)
	assert.NotContains(t, logs, tzReadBackendError, "a TZ read warning must not log the backend error")
}

// TestTZStorageReadError_FixedText: the resolver's error text is a fixed
// reason plus the scope kind; the backend error is reachable only through
// Unwrap.
func TestTZStorageReadError_FixedText(t *testing.T) {
	backend := errors.New(tzReadBackendError)
	err := error(&tzStorageReadError{Scope: store.ScopeProject, Err: backend})
	assert.Equal(t, "stored TZ settings could not be read (project scope)", err.Error())
	assert.ErrorIs(t, err, backend)
	assert.Equal(t, store.ScopeProject, tzReadScope(err))
	wrapped := &secretResolutionError{Verb: "started", Source: resolutionSourceEnvSettings, Err: err}
	assert.Equal(t, store.ScopeProject, tzReadScope(wrapped))
	assert.NotContains(t, wrapped.Error(), tzReadBackendError)
	assert.Equal(t, "unknown", tzReadScope(backend))

	// The resolver returns it for a failed scope read.
	f, _ := newTZStoreErrFixture(t, true)
	_, rerr := f.d.resolveAgentTZ(context.Background(), f.agent, false)
	require.Error(t, rerr)
	assert.Equal(t, store.ScopeUser, tzReadScope(rerr))
	assert.NotContains(t, rerr.Error(), tzReadBackendError)
}
