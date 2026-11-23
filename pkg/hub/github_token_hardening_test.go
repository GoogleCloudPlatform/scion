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
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
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
	st := newTestStore(t)

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
		err = disp.DispatchAgentStart(ctx, ag, "")
		require.ErrorIs(t, err, ErrBrokerLacksGitHubTokenInstallationOnly)
		assert.False(t, mockClient.startCalled)

		mockClient.restartCalled = false
		err = disp.DispatchAgentRestart(ctx, ag)
		require.ErrorIs(t, err, ErrBrokerLacksGitHubTokenInstallationOnly)
		assert.False(t, mockClient.restartCalled)
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
		require.NoError(t, disp.DispatchAgentStart(ctx, ag, ""))
		assert.True(t, mockClient.lastStartExtras.GitHubTokenInstallationOnly)

		mockClient.lastStartExtras = StartExtras{}
		require.NoError(t, disp.DispatchAgentRestart(ctx, ag))
		assert.True(t, mockClient.lastStartExtras.GitHubTokenInstallationOnly)
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
		st := newTestStore(t)
		secBackend := secret.NewLocalBackend(st, "test-hub")

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
		disp := NewHTTPAgentDispatcherWithClient(st, mockClient, false, nil)
		disp.SetSecretBackend(secBackend)

		instAg := &store.Agent{
			ID: api.NewUUID(), Name: "inst-file-ag", Slug: "inst-file-ag",
			OwnerID: userID, Ancestry: []string{userID}, ProjectID: proj.ID, RuntimeBrokerID: broker.ID,
			AppliedConfig: &store.AgentAppliedConfig{GitHubTokenPreference: DispatchAgentGitHubTokenInstallation},
		}
		require.NoError(t, st.CreateAgent(ctx, instAg))

		_, err := disp.DispatchAgentCreate(ctx, instAg)
		require.NoError(t, err)
		require.NotNil(t, mockClient.lastCreateReq)
		var createNames []string
		for _, s := range mockClient.lastCreateReq.ResolvedSecrets {
			createNames = append(createNames, s.Name)
			assert.False(t, api.IsBlockedInstallationGitHubCredentialFileTarget(s.Target),
				"create leaked credential file secret %q target=%q", s.Name, s.Target)
		}
		assert.Contains(t, createNames, "SAFE_FILE_SECRET")

		mockClient.lastResolvedSecrets = nil
		require.NoError(t, disp.DispatchAgentStart(ctx, instAg, ""))
		var startNames []string
		for _, s := range mockClient.lastResolvedSecrets {
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
		mockClient.lastCreateReq = nil
		_, err = disp.DispatchAgentCreate(ctx, userPrefAg)
		require.NoError(t, err)
		var upNames []string
		for _, s := range mockClient.lastCreateReq.ResolvedSecrets {
			upNames = append(upNames, s.Name)
		}
		assert.Contains(t, upNames, "MY_GH_HOSTS")
		assert.Contains(t, upNames, "MY_GIT_CREDS")
		assert.Contains(t, upNames, "MY_NETRC")
	})

	t.Run("pull_and_list_hide_credential_file_secrets", func(t *testing.T) {
		f := newMaterialSelectionFixture(t, "gh-file-pull")
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
