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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGitHubTokenInstallationOnly_BrokerCapabilityGateAndWireFlag verifies
// item 1 on the Hub side:
//   - Brokers lacking Capabilities.GitHubTokenInstallationOnly fail closed with
//     ErrBrokerLacksGitHubTokenInstallationOnly (unwrapping errStartRequestNotSent)
//     on create, provision, start, and restart without sending any broker request.
//   - Capable brokers receive GitHubTokenInstallationOnly=true on create and
//     StartExtras (and wire key "githubTokenInstallationOnly" via applyStartExtras).
//   - Non-installation agents are unaffected on brokers lacking the capability.
func TestGitHubTokenInstallationOnly_BrokerCapabilityGateAndWireFlag(t *testing.T) {
	ctx := context.Background()
	st := createTestStore(t)

	proj := &store.Project{ID: tid("gh-cap-proj"), Name: "gh-cap", Slug: "gh-cap"}
	require.NoError(t, st.CreateProject(ctx, proj))

	oldBroker := &store.RuntimeBroker{
		ID:       tid("gh-cap-old-broker"),
		Name:     "old-broker",
		Slug:     "old-broker",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
		// Capabilities nil / GitHubTokenInstallationOnly false.
	}
	require.NoError(t, st.CreateRuntimeBroker(ctx, oldBroker))

	newBroker := &store.RuntimeBroker{
		ID:           tid("gh-cap-new-broker"),
		Name:         "new-broker",
		Slug:         "new-broker",
		Endpoint:     "http://localhost:9801",
		Status:       store.BrokerStatusOnline,
		Capabilities: &store.BrokerCapabilities{GitHubTokenInstallationOnly: true},
	}
	require.NoError(t, st.CreateRuntimeBroker(ctx, newBroker))

	mockClient := &mockRuntimeBrokerClient{}
	disp := NewHTTPAgentDispatcherWithClient(st, mockClient, false, nil)

	makeAgent := func(slug, brokerID, pref string) *store.Agent {
		ag := &store.Agent{
			ID:              api.NewUUID(),
			Name:            slug,
			Slug:            slug,
			ProjectID:       proj.ID,
			RuntimeBrokerID: brokerID,
			AppliedConfig: &store.AgentAppliedConfig{
				GitHubTokenPreference: pref,
			},
		}
		require.NoError(t, st.CreateAgent(ctx, ag))
		return ag
	}

	t.Run("old_broker_refuses_installation_agent_fail_closed", func(t *testing.T) {
		ag := makeAgent("old-inst", oldBroker.ID, DispatchAgentGitHubTokenInstallation)

		mockClient.createCalled = false
		_, err := disp.DispatchAgentCreate(ctx, ag)
		require.ErrorIs(t, err, ErrBrokerLacksGitHubTokenInstallationOnly)
		require.ErrorIs(t, err, errStartRequestNotSent)
		assert.Contains(t, err.Error(), "upgrade the Runtime Broker to a version that advertises githubTokenInstallationOnly")
		assert.False(t, mockClient.createCalled, "must not call broker when capability is missing")

		mockClient.createCalled = false
		_, err = disp.DispatchAgentCreateWithGather(ctx, ag)
		require.ErrorIs(t, err, ErrBrokerLacksGitHubTokenInstallationOnly)
		assert.False(t, mockClient.createCalled)

		mockClient.createCalled = false
		err = disp.DispatchAgentProvision(ctx, ag)
		require.ErrorIs(t, err, ErrBrokerLacksGitHubTokenInstallationOnly)
		assert.False(t, mockClient.createCalled)

		mockClient.startCalled = false
		err = disp.DispatchAgentStart(ctx, ag, "", false)
		require.ErrorIs(t, err, ErrBrokerLacksGitHubTokenInstallationOnly)
		assert.False(t, mockClient.startCalled)

		mockClient.restartCalled = false
		err = disp.DispatchAgentRestart(ctx, ag)
		require.ErrorIs(t, err, ErrBrokerLacksGitHubTokenInstallationOnly)
		assert.False(t, mockClient.restartCalled)
	})

	t.Run("unassigned_broker_reports_no_broker_before_capability_gate", func(t *testing.T) {
		ag := makeAgent("no-broker-inst", "", DispatchAgentGitHubTokenInstallation)

		err := disp.DispatchAgentStart(ctx, ag, "", false)
		require.Error(t, err)
		assert.NotErrorIs(t, err, ErrBrokerLacksGitHubTokenInstallationOnly)
		assert.Contains(t, err.Error(), "no runtime broker assigned")

		err = disp.DispatchAgentRestart(ctx, ag)
		require.Error(t, err)
		assert.NotErrorIs(t, err, ErrBrokerLacksGitHubTokenInstallationOnly)
		assert.Contains(t, err.Error(), "no runtime broker assigned")
	})

	t.Run("old_broker_allows_non_installation_agent", func(t *testing.T) {
		ag := makeAgent("old-userpref", oldBroker.ID, DispatchAgentGitHubTokenUserPreferred)

		mockClient.lastCreateReq = nil
		_, err := disp.DispatchAgentCreate(ctx, ag)
		require.NoError(t, err)
		require.NotNil(t, mockClient.lastCreateReq)
		assert.False(t, mockClient.lastCreateReq.GitHubTokenInstallationOnly)
	})

	t.Run("capable_broker_receives_flag_on_create_start_restart", func(t *testing.T) {
		ag := makeAgent("new-inst", newBroker.ID, DispatchAgentGitHubTokenInstallation)

		mockClient.lastCreateReq = nil
		_, err := disp.DispatchAgentCreate(ctx, ag)
		require.NoError(t, err)
		require.NotNil(t, mockClient.lastCreateReq)
		assert.True(t, mockClient.lastCreateReq.GitHubTokenInstallationOnly)

		mockClient.lastStartExtras = StartExtras{}
		require.NoError(t, disp.DispatchAgentStart(ctx, ag, "", false))
		assert.True(t, mockClient.lastStartExtras.GitHubTokenInstallationOnly)

		mockClient.lastRestartExtras = StartExtras{}
		require.NoError(t, disp.DispatchAgentRestart(ctx, ag))
		assert.True(t, mockClient.lastRestartExtras.GitHubTokenInstallationOnly)
	})

	t.Run("applyStartExtras_wire_key", func(t *testing.T) {
		empty := map[string]interface{}{}
		applyStartExtras(empty, StartExtras{})
		_, exists := empty["githubTokenInstallationOnly"]
		assert.False(t, exists)

		set := map[string]interface{}{}
		applyStartExtras(set, StartExtras{GitHubTokenInstallationOnly: true})
		assert.Equal(t, true, set["githubTokenInstallationOnly"])
	})
}

// TestGitHubTokenInstallation_CredentialFileSecretsBlockedAllDoors verifies
// item 2: file-type secrets targeting gh/git/netrc credential paths (stored
// under arbitrary non-blocked names) are stripped on push and hidden on every
// pull and list door for installation-only agents, while unrelated file secrets
// survive and non-installation agents are unaffected.
func TestGitHubTokenInstallation_CredentialFileSecretsBlockedAllDoors(t *testing.T) {
	ctx := context.Background()

	t.Run("push_strips_credential_file_secrets_on_create_and_start", func(t *testing.T) {
		st := createTestStore(t)
		secBackend := secret.NewLocalBackend(st, "test-hub-id", "test-secret")

		proj := &store.Project{ID: tid("gh-file-push-proj"), Name: "gh-file-push", Slug: "gh-file-push"}
		require.NoError(t, st.CreateProject(ctx, proj))
		broker := &store.RuntimeBroker{
			ID:           tid("gh-file-push-broker"),
			Name:         "gh-file-push-broker",
			Slug:         "gh-file-push-broker",
			Endpoint:     "http://localhost:9800",
			Status:       store.BrokerStatusOnline,
			Capabilities: &store.BrokerCapabilities{GitHubTokenInstallationOnly: true},
		}
		require.NoError(t, st.CreateRuntimeBroker(ctx, broker))
		userID := tid("gh-file-push-user")
		require.NoError(t, st.CreateUser(ctx, &store.User{
			ID: userID, Email: "file-push@example.com", DisplayName: "File Push", Role: store.UserRoleAdmin, Status: "active",
		}))

		seedFile := func(name, target, val string) {
			_, _, err := secBackend.Set(ctx, &secret.SetSecretInput{
				Name:          name,
				Value:         val,
				SecretType:    store.SecretTypeFile,
				Target:        target,
				Scope:         store.ScopeProject,
				ScopeID:       proj.ID,
				InjectionMode: store.InjectionModeAlways,
			})
			require.NoError(t, err)
		}
		seedFile("MY_GH_HOSTS", "~/.config/gh/hosts.yml", "github.com:\n  oauth_token: ghp_file_1")
		seedFile("MY_GIT_CREDS", "~/.git-credentials", "https://u:ghp_file_2@github.com")
		seedFile("MY_XDG_GIT_CREDS", "/home/scion/.config/git/credentials", "https://u:ghp_file_3@github.com")
		seedFile("MY_NETRC", "~/.netrc", "machine github.com password ghp_file_4")
		seedFile("MY_ETC_NETRC", "/etc/netrc", "machine github.com password ghp_file_5")
		seedFile("SAFE_FILE_SECRET", "~/.config/app/config.json", `{"ok":true}`)

		mockClient := &mockRuntimeBrokerClient{}
		disp := NewHTTPAgentDispatcherWithClient(st, mockClient, false, slog.Default())
		disp.SetSecretBackend(secBackend)
		disp.SetAuthzService(NewAuthzService(st, slog.Default()))

		instAg := &store.Agent{
			ID: api.NewUUID(), Name: "inst-file-ag", Slug: "inst-file-ag",
			OwnerID: userID, Ancestry: []string{userID}, ProjectID: proj.ID, RuntimeBrokerID: broker.ID,
			AppliedConfig: &store.AgentAppliedConfig{GitHubTokenPreference: DispatchAgentGitHubTokenInstallation},
		}
		require.NoError(t, st.CreateAgent(ctx, instAg))

		createReq, err := disp.buildCreateRequest(ctx, instAg, "TestFileSecretCreate")
		require.NoError(t, err)
		var createNames []string
		for _, s := range createReq.ResolvedSecrets {
			createNames = append(createNames, s.Name)
			assert.False(t, api.IsBlockedInstallationGitHubCredentialFileTarget(s.Target),
				"create leaked credential file secret %q target=%q", s.Name, s.Target)
		}
		assert.Contains(t, createNames, "SAFE_FILE_SECRET")

		startRes, err := disp.buildStartEnv(ctx, instAg, "DispatchAgentStart", mintSiteStart)
		require.NoError(t, err)
		var startNames []string
		for _, s := range startRes.secrets {
			startNames = append(startNames, s.Name)
			assert.False(t, api.IsBlockedInstallationGitHubCredentialFileTarget(s.Target),
				"start leaked credential file secret %q target=%q", s.Name, s.Target)
		}
		assert.Contains(t, startNames, "SAFE_FILE_SECRET")

		// Non-installation agent still receives the file secrets.
		userPrefAg := &store.Agent{
			ID: api.NewUUID(), Name: "userpref-file-ag", Slug: "userpref-file-ag",
			OwnerID: userID, Ancestry: []string{userID}, ProjectID: proj.ID, RuntimeBrokerID: broker.ID,
			AppliedConfig: &store.AgentAppliedConfig{GitHubTokenPreference: DispatchAgentGitHubTokenUserPreferred},
		}
		require.NoError(t, st.CreateAgent(ctx, userPrefAg))
		upReq, err := disp.buildCreateRequest(ctx, userPrefAg, "TestFileSecretCreateUserPref")
		require.NoError(t, err)
		var upNames []string
		for _, s := range upReq.ResolvedSecrets {
			upNames = append(upNames, s.Name)
		}
		assert.Contains(t, upNames, "MY_GH_HOSTS")
		assert.Contains(t, upNames, "MY_GIT_CREDS")
		assert.Contains(t, upNames, "MY_NETRC")
	})

	t.Run("pull_and_list_hide_credential_file_secrets", func(t *testing.T) {
		f := newMaterialFixture(t, "gh-file-pull")
		f.recordAgentEdge(t)
		raw := json.RawMessage(`{"overrides":{"hub.agent_secrets":true}}`)
		_, err := f.Store.UpsertHubSetting(ctx, "experiments", raw, "test", -1, "managed")
		require.NoError(t, err)
		ops := NewOperationalSettings(f.Store, emptyKoanf(), emptyKoanf())
		_, err = ops.Refresh(ctx)
		require.NoError(t, err)
		f.Server.SetOperationalSettings(ops)
		require.True(t, f.Server.experimentEnabled(experiments.AgentSecrets))
		ag, err := f.Store.GetAgent(ctx, f.AgentID)
		require.NoError(t, err)
		if ag.AppliedConfig == nil {
			ag.AppliedConfig = &store.AgentAppliedConfig{}
		}
		ag.AppliedConfig.GitHubTokenPreference = DispatchAgentGitHubTokenInstallation
		require.NoError(t, f.Store.UpdateAgent(ctx, ag))

		// Seed credential file secrets in agent, project, and user scopes plus a safe file secret.
		_, _, err = f.Server.secretBackend.Set(ctx, &secret.SetSecretInput{
			Name: "AGENT_GH_HOSTS", Value: "ghp_agent_file", SecretType: store.SecretTypeFile,
			Target: "~/.config/gh/hosts.yml", Scope: store.ScopeAgent, ScopeID: f.AgentID,
			CreatedBy: f.UserID, UpdatedBy: f.UserID,
		})
		require.NoError(t, err)
		_, _, err = f.Server.secretBackend.Set(ctx, &secret.SetSecretInput{
			Name: "PROJ_GIT_CREDS", Value: "ghp_proj_file", SecretType: store.SecretTypeFile,
			Target: "~/.git-credentials", Scope: store.ScopeProject, ScopeID: f.ProjectID,
			CreatedBy: f.UserID, UpdatedBy: f.UserID,
		})
		require.NoError(t, err)
		_, _, err = f.Server.secretBackend.Set(ctx, &secret.SetSecretInput{
			Name: "USER_NETRC", Value: "ghp_user_file", SecretType: store.SecretTypeFile,
			Target: "~/.netrc", Scope: store.ScopeUser, ScopeID: f.UserID, AllowProgeny: true,
			CreatedBy: f.UserID, UpdatedBy: f.UserID,
		})
		require.NoError(t, err)
		_, _, err = f.Server.secretBackend.Set(ctx, &secret.SetSecretInput{
			Name: "PROJ_SAFE_FILE", Value: "safe_content", SecretType: store.SecretTypeFile,
			Target: "~/.config/app/safe.yml", Scope: store.ScopeProject, ScopeID: f.ProjectID,
			CreatedBy: f.UserID, UpdatedBy: f.UserID,
		})
		require.NoError(t, err)

		tok := mintRecordedMaterialToken(t, f.Server, f.Store, f.AgentID, f.ProjectID,
			[]AgentTokenScope{ScopeProjectSecretRead}, ag.Ancestry)

		// 1. Runtime GET /agents/{id}/secrets/{key} refuses each credential file secret with 404.
		for _, tc := range []struct{ scope, key string }{
			{store.ScopeAgent, "AGENT_GH_HOSTS"},
			{store.ScopeProject, "PROJ_GIT_CREDS"},
			{store.ScopeUser, "USER_NETRC"},
		} {
			rec := doRequestWithAgentToken(t, f.Server, http.MethodGet,
				fmt.Sprintf("/api/v1/agents/%s/secrets/%s?scope=%s", f.AgentID, tc.key, tc.scope), nil, tok)
			assert.Equal(t, http.StatusNotFound, rec.Code, "GET scope=%s key=%s body=%s", tc.scope, tc.key, rec.Body.String())
			assert.NotContains(t, rec.Body.String(), "ghp_")
		}
		// Safe file secret is still readable.
		recSafe := doRequestWithAgentToken(t, f.Server, http.MethodGet,
			fmt.Sprintf("/api/v1/agents/%s/secrets/PROJ_SAFE_FILE?scope=project", f.AgentID), nil, tok)
		assert.Equal(t, http.StatusOK, recSafe.Code)

		// 2. Runtime POST /agent/secrets refuses credential file secrets across all scopes.
		recFetch := doRequestWithAgentToken(t, f.Server, http.MethodPost, "/api/v1/agent/secrets", map[string]any{
			"keys":  []string{"AGENT_GH_HOSTS", "PROJ_GIT_CREDS", "USER_NETRC", "PROJ_SAFE_FILE"},
			"scope": "project",
		}, tok)
		assert.Equal(t, http.StatusOK, recFetch.Code)
		assert.NotContains(t, recFetch.Body.String(), "ghp_")
		assert.Contains(t, recFetch.Body.String(), "PROJ_SAFE_FILE")

		// 3. Runtime list GET /agents/{id}/secrets hides credential file secrets across all scopes.
		recList := doRequestWithAgentToken(t, f.Server, http.MethodGet,
			fmt.Sprintf("/api/v1/agents/%s/secrets", f.AgentID), nil, tok)
		assert.Equal(t, http.StatusOK, recList.Code)
		assert.NotContains(t, recList.Body.String(), "AGENT_GH_HOSTS")
		assert.NotContains(t, recList.Body.String(), "PROJ_GIT_CREDS")
		assert.NotContains(t, recList.Body.String(), "USER_NETRC")
		assert.Contains(t, recList.Body.String(), "PROJ_SAFE_FILE")

		// 4. Project secret metadata GET and list hide PROJ_GIT_CREDS.
		recMetaGet := doRequestWithAgentToken(t, f.Server, http.MethodGet,
			fmt.Sprintf("/api/v1/projects/%s/secrets/PROJ_GIT_CREDS", f.ProjectID), nil, tok)
		assert.Equal(t, http.StatusNotFound, recMetaGet.Code)

		recMetaList := doRequestWithAgentToken(t, f.Server, http.MethodGet,
			fmt.Sprintf("/api/v1/projects/%s/secrets", f.ProjectID), nil, tok)
		assert.Equal(t, http.StatusOK, recMetaList.Code)
		assert.NotContains(t, recMetaList.Body.String(), "PROJ_GIT_CREDS")
		assert.Contains(t, recMetaList.Body.String(), "PROJ_SAFE_FILE")
	})
}

// TestBuildEnvGatherResponse_InstallationAgentOmitsBlockedKeys verifies item 3:
// buildEnvGatherResponse must not disclose blocked GitHub key names or their
// storage scopes in Required, BrokerHas, Needs, HubHas, SecretInfo, or
// HubWarnings to an installation-only agent create caller.
func TestBuildEnvGatherResponse_InstallationAgentOmitsBlockedKeys(t *testing.T) {
	ctx := context.Background()
	srv, st := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(st, srv.HubID(), "test-secret"))

	proj := &store.Project{ID: tid("envgather-inst-proj"), Name: "envgather-inst", Slug: "envgather-inst"}
	require.NoError(t, st.CreateProject(ctx, proj))
	userID := tid("envgather-inst-user")
	require.NoError(t, st.CreateUser(ctx, &store.User{
		ID: userID, Email: "eg-inst@example.com", DisplayName: "EG Inst", Role: store.UserRoleAdmin, Status: "active",
	}))

	// Store GITHUB_TOKEN and GH_TOKEN in project env/secrets plus an unrelated key.
	require.NoError(t, st.CreateEnvVar(ctx, &store.EnvVar{
		ID: api.NewUUID(), Key: "GITHUB_TOKEN", Value: "ghp_stored_env", Scope: store.ScopeProject, ScopeID: proj.ID,
	}))
	_, _, err := srv.secretBackend.Set(ctx, &secret.SetSecretInput{
		Name: "GH_TOKEN", Value: "ghp_stored_secret", SecretType: store.SecretTypeEnvironment, Target: "GH_TOKEN",
		Scope: store.ScopeProject, ScopeID: proj.ID,
	})
	require.NoError(t, err)
	require.NoError(t, st.CreateEnvVar(ctx, &store.EnvVar{
		ID: api.NewUUID(), Key: "ANTHROPIC_API_KEY", Value: "sk-ant-test", Scope: store.ScopeProject, ScopeID: proj.ID,
	}))

	brokerReqs := &RemoteEnvRequirementsResponse{
		AgentID:   "ag-1",
		Required:  []string{"GITHUB_TOKEN", "GH_TOKEN", "ANTHROPIC_API_KEY", "MISSING_OTHER"},
		HubHas:    []string{"GITHUB_TOKEN", "ANTHROPIC_API_KEY"},
		BrokerHas: []string{"GH_ENTERPRISE_TOKEN", "SAFE_BROKER_KEY"},
		Needs:     []string{"GITHUB_TOKEN", "GH_TOKEN", "MISSING_OTHER"},
		SecretInfo: map[string]SecretKeyInfo{
			"GITHUB_TOKEN":  {Description: "GitHub PAT", Source: "template", Type: "environment"},
			"MISSING_OTHER": {Description: "Other key", Source: "template", Type: "environment"},
		},
	}

	instAgent := &store.Agent{
		ID: api.NewUUID(), Name: "inst-eg", Slug: "inst-eg",
		OwnerID: userID, ProjectID: proj.ID,
		AppliedConfig: &store.AgentAppliedConfig{GitHubTokenPreference: DispatchAgentGitHubTokenInstallation},
	}
	gotInst := srv.buildEnvGatherResponse(ctx, instAgent, brokerReqs)
	rawInst, err := json.Marshal(gotInst)
	require.NoError(t, err)
	for _, blocked := range []string{"GITHUB_TOKEN", "GH_TOKEN", "GH_ENTERPRISE_TOKEN"} {
		assert.NotContains(t, string(rawInst), blocked, "installation env-gather response must not name %s", blocked)
	}
	assert.Equal(t, []string{"ANTHROPIC_API_KEY", "MISSING_OTHER"}, gotInst.Required)
	assert.Equal(t, []string{"SAFE_BROKER_KEY"}, gotInst.BrokerHas)
	assert.Equal(t, []string{"MISSING_OTHER"}, gotInst.Needs)
	require.Len(t, gotInst.HubHas, 1)
	assert.Equal(t, "ANTHROPIC_API_KEY", gotInst.HubHas[0].Key)
	assert.Contains(t, gotInst.SecretInfo, "MISSING_OTHER")
	assert.NotContains(t, gotInst.SecretInfo, "GITHUB_TOKEN")

	// Non-installation agent still receives the full response as before.
	normalAgent := &store.Agent{
		ID: api.NewUUID(), Name: "normal-eg", Slug: "normal-eg",
		OwnerID: userID, ProjectID: proj.ID,
		AppliedConfig: &store.AgentAppliedConfig{GitHubTokenPreference: DispatchAgentGitHubTokenUserPreferred},
	}
	gotNormal := srv.buildEnvGatherResponse(ctx, normalAgent, brokerReqs)
	assert.Contains(t, gotNormal.Needs, "GITHUB_TOKEN")
	assert.Contains(t, gotNormal.Needs, "GH_TOKEN")
	assert.NotEmpty(t, gotNormal.HubWarnings)
}

// TestShouldPersistResolvedEnvKey_AllBlockedGitHubKeysRejected verifies item 4
// on the Hub persistence write path.
func TestShouldPersistResolvedEnvKey_AllBlockedGitHubKeysRejected(t *testing.T) {
	for _, key := range []string{
		"GITHUB_TOKEN",
		"GH_TOKEN",
		"GH_ENTERPRISE_TOKEN",
		"GITHUB_ENTERPRISE_TOKEN",
		"SCION_USER_GITHUB_TOKEN",
	} {
		assert.False(t, shouldPersistResolvedEnvKey(key, map[string]api.EnvKind{key: api.EnvKindPlain}),
			"shouldPersistResolvedEnvKey(%q) must return false even when EnvKindPlain", key)
	}
	assert.True(t, shouldPersistResolvedEnvKey("SAFE_KEY", map[string]api.EnvKind{"SAFE_KEY": api.EnvKindPlain}))
}

// TestReapExpiredPendingDispatch_OfflineStopSurvivesPastTTLAndAppliesOnReconnect
// verifies R3 (lifted from p1c4b-review-1 probe): an offline-queued stop (202)
// survives past dispatchPendingTTL and is still applied when the broker
// reconnects.
func TestReapExpiredPendingDispatch_OfflineStopSurvivesPastTTLAndAppliesOnReconnect(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	disp := &runIntentDispatcher{}
	srv.SetDispatcher(disp)
	srv.commandBus = &recordingCommandBus{}
	_, broker, agent := setupOfflineBrokerAgent(t, s, "offline-stop-ttl")
	_, err := s.SetRunIntent(ctx, agent.ID, store.RunIntentRunning)
	require.NoError(t, err)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/stop", nil)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	pending, err := s.ListPendingDispatch(ctx, broker.ID)
	require.NoError(t, err)
	require.Len(t, pending, 1)

	later := time.Now().Add(srv.pendingDispatchTTL() + time.Minute)
	n, err := s.ReapExpiredPendingDispatch(ctx, later.Add(-srv.pendingDispatchTTL()))
	require.NoError(t, err)
	assert.Equal(t, 0, n, "offline-queued stop must not be expired by the pending-dispatch TTL sweep")

	srv.reconcileBroker(ctx, broker.ID)
	assert.Equal(t, int32(1), disp.stops.Load(), "the queued stop must still be applied on reconnect")
}

// TestPendingDispatchTTL_AtLeastCreateUnconfirmedHold verifies O1: the
// pending-dispatch TTL is derived at reap time from
// startClaimSettings().CreateUnconfirmedHold (default 16m), so a pending
// create within the create unconfirmed hold window is not prematurely expired,
// while one older than the hold window is failed and scrubbed.
func TestPendingDispatchTTL_AtLeastCreateUnconfirmedHold(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)

	defaultHold := srv.startClaimSettings().CreateUnconfirmedHold
	require.GreaterOrEqual(t, defaultHold, 16*time.Minute)
	assert.GreaterOrEqual(t, srv.pendingDispatchTTL(), defaultHold)

	// Raising CreateUnconfirmedHold in server config raises pendingDispatchTTL
	// dynamically at reap time.
	raisedHold := defaultHold + 5*time.Minute
	srv.config.StartClaim = StartClaimSettings{CreateUnconfirmedHold: raisedHold}
	assert.GreaterOrEqual(t, srv.pendingDispatchTTL(), raisedHold)

	// Reset to default and verify a create row aged 12m (past the old 10m
	// constant, inside the 16m CreateUnconfirmedHold) survives reap, while
	// after CreateUnconfirmedHold+1m it is failed and its args are scrubbed.
	srv.config.StartClaim = StartClaimSettings{}
	_, broker, agent := setupOfflineBrokerAgent(t, s, "pending-ttl-hold")
	row := &store.BrokerDispatch{
		ID:       api.NewUUID(),
		BrokerID: broker.ID,
		Op:       "create",
		AgentID:  agent.ID,
		State:    store.DispatchStatePending,
		Args:     `{"home_files":"secret-event-body"}`,
	}
	require.NoError(t, s.InsertBrokerDispatch(ctx, row))

	now := time.Now()
	withinHoldCutoff := now.Add(12 * time.Minute).Add(-srv.pendingDispatchTTL())
	n, err := s.ReapExpiredPendingDispatch(ctx, withinHoldCutoff)
	require.NoError(t, err)
	assert.Equal(t, 0, n, "pending create within CreateUnconfirmedHold must not be expired")

	pastHoldCutoff := now.Add(srv.pendingDispatchTTL() + time.Minute).Add(-srv.pendingDispatchTTL())
	n, err = s.ReapExpiredPendingDispatch(ctx, pastHoldCutoff)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "pending create past CreateUnconfirmedHold must be expired and scrubbed")

	got, err := s.GetBrokerDispatch(ctx, row.ID)
	require.NoError(t, err)
	assert.Equal(t, store.DispatchStateFailed, got.State)
	assert.Empty(t, got.Args)
}

// TestApplyAgentUpdate_BlockedGitHubTokenKeysNeverTreatedAsRemovedFromCreateInputs
// verifies R4 / review-2 R3: because all blocked GitHub token keys are hidden
// from GET responses for every viewer, an env PATCH on a non-installation agent
// must not record any non-reserved blocked GitHub key as removed from
// CreateInputs.InlineConfig.Env, and GET must also hide SCION_USER_GITHUB_TOKEN.
func TestApplyAgentUpdate_BlockedGitHubTokenKeysNeverTreatedAsRemovedFromCreateInputs(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	ctx := context.Background()

	initialEnv := map[string]string{
		"GITHUB_TOKEN":            "ghp_secret_1",
		"GH_TOKEN":                "ghp_secret_2",
		"GH_ENTERPRISE_TOKEN":     "ghp_secret_3",
		"GITHUB_ENTERPRISE_TOKEN": "ghp_secret_4",
		"FOO":                     "bar",
	}
	copyEnv := func() map[string]string {
		out := make(map[string]string, len(initialEnv))
		for k, v := range initialEnv {
			out[k] = v
		}
		return out
	}

	appliedWithReserved := copyEnv()
	appliedWithReserved["SCION_USER_GITHUB_TOKEN"] = "scion_user_gh_hidden"

	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.Phase = string(state.PhaseCreated)
		a.AppliedConfig.Env = appliedWithReserved
		a.AppliedConfig.InlineConfig = &api.ScionConfig{Env: copyEnv()}
		a.AppliedConfig.CreateInputs = &store.AgentCreateInputs{
			InlineConfig: &api.ScionConfig{Env: copyEnv()},
		}
	})

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/agents/"+agent.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	for _, secretVal := range []string{"ghp_secret_1", "ghp_secret_2", "ghp_secret_3", "ghp_secret_4", "scion_user_gh_hidden"} {
		assert.NotContains(t, rec.Body.String(), secretVal, "GET must hide all blocked GitHub token keys")
	}

	rec = patchAgentConfig(t, srv, agent.ID, map[string]interface{}{
		"env": map[string]interface{}{"FOO": "bar"},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	updated, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	require.NotNil(t, updated.AppliedConfig.CreateInputs)
	require.NotNil(t, updated.AppliedConfig.CreateInputs.InlineConfig)
	ciEnv := updated.AppliedConfig.CreateInputs.InlineConfig.Env
	for k, want := range initialEnv {
		assert.Equal(t, want, ciEnv[k], "CreateInputs.InlineConfig.Env[%q] must not be recorded as removed", k)
	}
}

type blockedOnlyEnvGatherDispatcher struct {
	runIntentDispatcher
}

func (d *blockedOnlyEnvGatherDispatcher) DispatchAgentCreateWithGather(ctx context.Context, agent *store.Agent) (*CreateDispatchResult, error) {
	if agent.AppliedConfig == nil {
		agent.AppliedConfig = &store.AgentAppliedConfig{}
	}
	agent.AppliedConfig.GitHubTokenPreference = DispatchAgentGitHubTokenInstallation
	return envReqsResult(&RemoteEnvRequirementsResponse{
		Required: []string{"GITHUB_TOKEN", "GH_TOKEN"},
		Needs:    []string{"GITHUB_TOKEN", "GH_TOKEN"},
	}), nil
}

// TestCreateAgent_InstallationOnlyFailsClosedWhenBrokerNeedsOnlyBlockedGitHubKeys
// verifies O2: if a broker answers 202 with Needs containing only blocked
// GitHub token keys for an installation-only agent, the Hub fails closed with
// ErrBrokerRequestedBlockedGitHubEnv and rolls back the agent instead of
// treating the unstarted response as success.
func TestCreateAgent_InstallationOnlyFailsClosedWhenBrokerNeedsOnlyBlockedGitHubKeys(t *testing.T) {
	for _, gatherEnv := range []bool{false, true} {
		name := fmt.Sprintf("gatherEnv_%t", gatherEnv)
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			srv, s := testServer(t)
			srv.SetDispatcher(&blockedOnlyEnvGatherDispatcher{})

			proj := &store.Project{ID: tid("o2-proj-" + name), Name: "o2-" + name, Slug: "o2-" + name}
			require.NoError(t, s.CreateProject(ctx, proj))

			broker := &store.RuntimeBroker{
				ID:           tid("o2-broker-" + name),
				Name:         "o2-broker-" + name,
				Slug:         "o2-broker-" + name,
				Endpoint:     "http://localhost:9802",
				Status:       store.BrokerStatusOnline,
				Capabilities: &store.BrokerCapabilities{GitHubTokenInstallationOnly: true},
			}
			require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
			require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
				ProjectID:  proj.ID,
				BrokerID:   broker.ID,
				BrokerName: broker.Name,
				LocalPath:  "/tmp/o2-" + name,
				Status:     store.BrokerStatusOnline,
			}))

			body := map[string]any{
				"name":                  "inst-o2-" + name,
				"projectId":             proj.ID,
				"runtimeBrokerId":       broker.ID,
				"gatherEnv":             gatherEnv,
				"githubTokenPreference": DispatchAgentGitHubTokenInstallation,
			}
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+proj.ID+"/agents", body)
			require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), ErrBrokerRequestedBlockedGitHubEnv.Error())

			// Agent row must have been rolled back, not left Provisioning.
			agents, err := s.ListAgents(ctx, store.AgentFilter{ProjectID: proj.ID}, store.ListOptions{})
			require.NoError(t, err)
			assert.Empty(t, agents.Items, "failed create must roll back the agent row")
		})
	}
}

// TestFilterInstallationEnvGatherKeys_OnlyBlockedKeysBecomesEmpty verifies that
// filterInstallationEnvGatherKeys omits blocked GitHub keys for
// installation-only agents while leaving non-installation agents untouched.
func TestFilterInstallationEnvGatherKeys_OnlyBlockedKeysBecomesEmpty(t *testing.T) {
	instAg := &store.Agent{
		AppliedConfig: &store.AgentAppliedConfig{
			GitHubTokenPreference: DispatchAgentGitHubTokenInstallation,
		},
	}
	assert.Empty(t, filterInstallationEnvGatherKeys(instAg, []string{"GITHUB_TOKEN", "GH_TOKEN"}))
	assert.Equal(t, []string{"ANTHROPIC_API_KEY"}, filterInstallationEnvGatherKeys(instAg, []string{"GITHUB_TOKEN", "ANTHROPIC_API_KEY"}))

	normalAg := &store.Agent{
		AppliedConfig: &store.AgentAppliedConfig{
			GitHubTokenPreference: DispatchAgentGitHubTokenUserPreferred,
		},
	}
	assert.Equal(t, []string{"GITHUB_TOKEN", "GH_TOKEN"}, filterInstallationEnvGatherKeys(normalAg, []string{"GITHUB_TOKEN", "GH_TOKEN"}))
}
