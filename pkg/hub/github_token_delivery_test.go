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
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGitHubTokenDelivery_CreateStartRestartAndAsNeeded(t *testing.T) {
	ctx := t.Context()
	st := createTestStore(t)
	secBackend := secret.NewLocalBackend(st, "test-hub-id", "test-secret")

	installID := int64(54321)
	require.NoError(t, st.CreateGitHubInstallation(ctx, &store.GitHubInstallation{
		InstallationID: installID,
		AccountLogin:   "test-org",
		AccountType:    "Organization",
		AppID:          7,
		Status:         store.GitHubInstallationStatusActive,
	}))

	projWithApp := &store.Project{
		ID:                   tid("gh-pref-proj-app"),
		Name:                 "gh-pref-app",
		Slug:                 "gh-pref-app",
		GitHubInstallationID: &installID,
	}
	require.NoError(t, st.CreateProject(ctx, projWithApp))

	projNoApp := &store.Project{
		ID:   tid("gh-pref-proj-noapp"),
		Name: "gh-pref-noapp",
		Slug: "gh-pref-noapp",
	}
	require.NoError(t, st.CreateProject(ctx, projNoApp))

	broker := &store.RuntimeBroker{
		ID:       tid("gh-pref-broker"),
		Name:     "gh-pref-broker",
		Slug:     "gh-pref-broker",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	require.NoError(t, st.CreateRuntimeBroker(ctx, broker))

	userID := tid("gh-pref-user")
	require.NoError(t, st.CreateUser(ctx, &store.User{
		ID:          userID,
		Email:       "gh-pref@example.com",
		DisplayName: "GH Pref User",
		Role:        store.UserRoleAdmin,
		Status:      "active",
	}))

	// Store user-scope PAT in both env and secrets, plus GH_* aliases and an
	// as_needed GH_TOKEN env var, plus a non-GitHub secret that must survive.
	const userPAT = "ghp_user_secret_pat_never_deliver"
	const ghAliasPAT = "ghp_alias_pat_never_deliver"
	const gheAliasPAT = "ghp_ghe_alias_never_deliver"
	const gheTokenPAT = "ghp_github_ent_never_deliver"
	const otherSecret = "other-secret-must-survive"

	_, _, err := secBackend.Set(ctx, &secret.SetSecretInput{
		Name:          "GITHUB_TOKEN",
		Value:         userPAT,
		SecretType:    store.SecretTypeEnvironment,
		Target:        "GITHUB_TOKEN",
		Scope:         store.ScopeUser,
		ScopeID:       userID,
		InjectionMode: store.InjectionModeAlways,
	})
	require.NoError(t, err)
	_, _, err = secBackend.Set(ctx, &secret.SetSecretInput{
		Name:          "GH_TOKEN",
		Value:         ghAliasPAT,
		SecretType:    store.SecretTypeEnvironment,
		Target:        "GH_TOKEN",
		Scope:         store.ScopeUser,
		ScopeID:       userID,
		InjectionMode: store.InjectionModeAlways,
	})
	require.NoError(t, err)
	_, _, err = secBackend.Set(ctx, &secret.SetSecretInput{
		Name:          "GH_ENTERPRISE_TOKEN",
		Value:         gheAliasPAT,
		SecretType:    store.SecretTypeEnvironment,
		Target:        "GH_ENTERPRISE_TOKEN",
		Scope:         store.ScopeUser,
		ScopeID:       userID,
		InjectionMode: store.InjectionModeAlways,
	})
	require.NoError(t, err)
	_, _, err = secBackend.Set(ctx, &secret.SetSecretInput{
		Name:          "GITHUB_ENTERPRISE_TOKEN",
		Value:         gheTokenPAT,
		SecretType:    store.SecretTypeEnvironment,
		Target:        "GITHUB_ENTERPRISE_TOKEN",
		Scope:         store.ScopeUser,
		ScopeID:       userID,
		InjectionMode: store.InjectionModeAlways,
	})
	require.NoError(t, err)
	_, _, err = secBackend.Set(ctx, &secret.SetSecretInput{
		Name:          "UNRELATED_API_KEY",
		Value:         otherSecret,
		SecretType:    store.SecretTypeEnvironment,
		Target:        "UNRELATED_API_KEY",
		Scope:         store.ScopeUser,
		ScopeID:       userID,
		InjectionMode: store.InjectionModeAlways,
	})
	require.NoError(t, err)
	require.NoError(t, st.CreateEnvVar(ctx, &store.EnvVar{
		ID:            api.NewUUID(),
		Key:           "GH_TOKEN",
		Value:         ghAliasPAT,
		Scope:         store.ScopeUser,
		ScopeID:       userID,
		InjectionMode: store.InjectionModeAsNeeded,
	}))

	newDispatcher := func(minter *mockGitHubAppMinter) *HTTPAgentDispatcher {
		d := NewHTTPAgentDispatcherWithClient(st, &mockRuntimeBrokerClient{}, false, slog.Default())
		d.SetSecretBackend(secBackend)
		if minter != nil {
			d.SetGitHubAppMinter(minter)
		}
		return d
	}

	makeAgent := func(name, projectID, pref string) *store.Agent {
		return &store.Agent{
			ID:              api.NewUUID(),
			Name:            name,
			Slug:            name,
			OwnerID:         userID,
			Ancestry:        []string{userID},
			ProjectID:       projectID,
			RuntimeBrokerID: broker.ID,
			AppliedConfig: &store.AgentAppliedConfig{
				AgentRole:             string(AgentRoleFull),
				GitHubTokenPreference: pref,
				Env: map[string]string{
					"GITHUB_TOKEN":            userPAT,
					"GH_TOKEN":                ghAliasPAT,
					"GH_ENTERPRISE_TOKEN":     gheAliasPAT,
					"GITHUB_ENTERPRISE_TOKEN": gheTokenPAT,
					"KEEP_ME":                 "kept",
				},
			},
		}
	}

	assertNoPATOrAliases := func(t *testing.T, env map[string]string, secrets []ResolvedSecret) {
		t.Helper()
		for _, k := range []string{"GH_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN", "SCION_USER_GITHUB_TOKEN"} {
			_, present := env[k]
			assert.False(t, present, "env key %s must be absent", k)
		}
		for _, v := range env {
			assert.NotEqual(t, userPAT, v, "user PAT must never appear in env")
			assert.NotEqual(t, ghAliasPAT, v, "GH_TOKEN alias must never appear in env")
			assert.NotEqual(t, gheAliasPAT, v, "GH_ENTERPRISE_TOKEN alias must never appear in env")
			assert.NotEqual(t, gheTokenPAT, v, "GITHUB_ENTERPRISE_TOKEN alias must never appear in env")
		}
		for _, s := range secrets {
			assert.False(t, isBlockedInstallationGitHubEnvKey(s.Target), "secret target %q must be dropped", s.Target)
			assert.False(t, isBlockedInstallationGitHubEnvKey(s.Name), "secret name %q must be dropped", s.Name)
		}
	}

	t.Run("create_installation_with_app_drops_pat_and_injects_app_token", func(t *testing.T) {
		minter := &mockGitHubAppMinter{token: "ghs_install_tok_1", expiry: "2026-11-21T06:00:00Z"}
		d := newDispatcher(minter)
		ag := makeAgent("inst-app-create", projWithApp.ID, DispatchAgentGitHubTokenInstallation)

		req, err := d.buildCreateRequest(ctx, ag, "TestCreateInst")
		require.NoError(t, err)
		assert.True(t, minter.called)
		assert.Equal(t, "ghs_install_tok_1", req.ResolvedEnv["GITHUB_TOKEN"])
		assert.Equal(t, "true", req.ResolvedEnv["SCION_GITHUB_APP_ENABLED"])
		assert.Equal(t, api.EnvKindSecretInjected, req.EnvClassifications["GITHUB_TOKEN"])
		assert.Equal(t, "kept", req.ResolvedEnv["KEEP_ME"])
		assert.Equal(t, otherSecret, req.ResolvedEnv["UNRELATED_API_KEY"])
		assertNoPATOrAliases(t, req.ResolvedEnv, req.ResolvedSecrets)
	})

	t.Run("create_installation_without_app_delivers_no_github_token", func(t *testing.T) {
		minter := &mockGitHubAppMinter{token: "ghs_should_not_mint", expiry: "2026-11-21T06:00:00Z"}
		d := newDispatcher(minter)
		ag := makeAgent("inst-noapp-create", projNoApp.ID, DispatchAgentGitHubTokenInstallation)

		req, err := d.buildCreateRequest(ctx, ag, "TestCreateInstNoApp")
		require.NoError(t, err)
		assert.False(t, minter.called)
		_, hasGH := req.ResolvedEnv["GITHUB_TOKEN"]
		assert.False(t, hasGH, "GITHUB_TOKEN must be absent when project has no installation")
		_, hasEnabled := req.ResolvedEnv["SCION_GITHUB_APP_ENABLED"]
		assert.False(t, hasEnabled)
		assert.Equal(t, "kept", req.ResolvedEnv["KEEP_ME"])
		assert.Equal(t, otherSecret, req.ResolvedEnv["UNRELATED_API_KEY"])
		assertNoPATOrAliases(t, req.ResolvedEnv, req.ResolvedSecrets)
	})

	t.Run("create_user_preferred_keeps_today_precedence", func(t *testing.T) {
		minter := &mockGitHubAppMinter{token: "ghs_unused", expiry: "2026-11-21T06:00:00Z"}
		d := newDispatcher(minter)
		ag := makeAgent("user-pref-create", projWithApp.ID, DispatchAgentGitHubTokenUserPreferred)

		req, err := d.buildCreateRequest(ctx, ag, "TestCreateUserPref")
		require.NoError(t, err)
		assert.False(t, minter.called)
		assert.Equal(t, userPAT, req.ResolvedEnv["GITHUB_TOKEN"])
		assert.Equal(t, "true", req.ResolvedEnv["SCION_USER_GITHUB_TOKEN"])
		assert.Equal(t, "true", req.ResolvedEnv["SCION_GITHUB_APP_ENABLED"])
	})

	t.Run("start_and_restart_installation_drops_pat_and_injects_app_token", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			caller string
			site   mintSite
		}{
			{"start", "DispatchAgentStart", mintSiteStart},
			{"restart", "DispatchAgentRestart", mintSiteRestart},
		} {
			t.Run(tc.name+"_with_app", func(t *testing.T) {
				minter := &mockGitHubAppMinter{token: "ghs_lifecycle_tok", expiry: "2026-11-21T06:00:00Z"}
				d := newDispatcher(minter)
				ag := makeAgent("inst-life-"+tc.name, projWithApp.ID, DispatchAgentGitHubTokenInstallation)

				res, err := d.buildStartEnv(ctx, ag, tc.caller, tc.site)
				require.NoError(t, err)
				assert.True(t, minter.called)
				assert.Equal(t, "ghs_lifecycle_tok", res.env["GITHUB_TOKEN"])
				assert.Equal(t, "true", res.env["SCION_GITHUB_APP_ENABLED"])
				assert.Equal(t, api.EnvKindSecretInjected, res.classifications["GITHUB_TOKEN"])
				assert.Equal(t, otherSecret, res.env["UNRELATED_API_KEY"])
				assertNoPATOrAliases(t, res.env, res.secrets)
			})

			t.Run(tc.name+"_without_app", func(t *testing.T) {
				minter := &mockGitHubAppMinter{token: "ghs_unused", expiry: "2026-11-21T06:00:00Z"}
				d := newDispatcher(minter)
				ag := makeAgent("inst-life-noapp-"+tc.name, projNoApp.ID, DispatchAgentGitHubTokenInstallation)

				res, err := d.buildStartEnv(ctx, ag, tc.caller, tc.site)
				require.NoError(t, err)
				assert.False(t, minter.called)
				_, hasGH := res.env["GITHUB_TOKEN"]
				assert.False(t, hasGH, "GITHUB_TOKEN must be absent when no installation exists")
				assertNoPATOrAliases(t, res.env, res.secrets)
			})
		}
	})

	t.Run("resolve_as_needed_drops_github_tokens_for_installation", func(t *testing.T) {
		d := newDispatcher(nil)
		ag := makeAgent("inst-asneeded", projWithApp.ID, DispatchAgentGitHubTokenInstallation)
		got, err := d.resolveAsNeededForKeys(ctx, ag, []string{"GITHUB_TOKEN", "GH_TOKEN"}, nil)
		require.NoError(t, err)
		assert.Empty(t, got)
	})
}

func TestGitHubTokenPreference_PreservedAcrossReincarnateAndInteractiveUnchanged(t *testing.T) {
	ctx := t.Context()
	srv, st := newTestServerWithStore(t)

	proj := &store.Project{
		ID:   tid("reinc-ghpref-proj"),
		Name: "reinc-ghpref",
		Slug: "reinc-ghpref",
	}
	require.NoError(t, st.CreateProject(ctx, proj))

	// Interactive buildAppliedConfig leaves GitHubTokenPreference empty.
	interactiveAC := srv.buildAppliedConfig(CreateAgentRequest{Task: "hello"}, "user@example.com", AgentRoleFull)
	assert.Empty(t, interactiveAC.GitHubTokenPreference)
	require.NotNil(t, interactiveAC.CreateInputs)
	assert.Empty(t, interactiveAC.CreateInputs.GitHubTokenPreference)

	// Reincarnate preserves GitHubTokenPreference from CreateInputs.
	ag := &store.Agent{
		ID:        api.NewUUID(),
		Slug:      "reinc-gh-inst",
		Name:      "reinc-gh-inst",
		ProjectID: proj.ID,
		AppliedConfig: &store.AgentAppliedConfig{
			AgentRole:             string(AgentRoleFull),
			GitHubTokenPreference: DispatchAgentGitHubTokenInstallation,
			CreateInputs: &store.AgentCreateInputs{
				GitHubTokenPreference: DispatchAgentGitHubTokenInstallation,
			},
		},
	}
	fresh, _, err := srv.buildFreshAppliedConfig(ctx, ag, proj, "")
	require.NoError(t, err)
	assert.Equal(t, DispatchAgentGitHubTokenInstallation, fresh.GitHubTokenPreference)
	require.NotNil(t, fresh.CreateInputs)
	assert.Equal(t, DispatchAgentGitHubTokenInstallation, fresh.CreateInputs.GitHubTokenPreference)

	// Legacy fallback (CreateInputs == nil) also preserves GitHubTokenPreference.
	agLegacy := &store.Agent{
		ID:        api.NewUUID(),
		Slug:      "reinc-gh-legacy",
		Name:      "reinc-gh-legacy",
		ProjectID: proj.ID,
		AppliedConfig: &store.AgentAppliedConfig{
			AgentRole:             string(AgentRoleFull),
			GitHubTokenPreference: DispatchAgentGitHubTokenInstallation,
		},
	}
	freshLegacy, _, err := srv.buildFreshAppliedConfig(ctx, agLegacy, proj, "")
	require.NoError(t, err)
	assert.Equal(t, DispatchAgentGitHubTokenInstallation, freshLegacy.GitHubTokenPreference)
	require.NotNil(t, freshLegacy.CreateInputs)
	assert.Equal(t, DispatchAgentGitHubTokenInstallation, freshLegacy.CreateInputs.GitHubTokenPreference)

	// Sticky fail-closed (O1): if AppliedConfig says "installation" even when
	// CreateInputs is empty, reincarnation keeps "installation" on both fields.
	agStickyApplied := &store.Agent{
		ID:        api.NewUUID(),
		Slug:      "reinc-gh-sticky-applied",
		Name:      "reinc-gh-sticky-applied",
		ProjectID: proj.ID,
		AppliedConfig: &store.AgentAppliedConfig{
			AgentRole:             string(AgentRoleFull),
			GitHubTokenPreference: DispatchAgentGitHubTokenInstallation,
			CreateInputs:          &store.AgentCreateInputs{GitHubTokenPreference: ""},
		},
	}
	freshStickyApplied, _, err := srv.buildFreshAppliedConfig(ctx, agStickyApplied, proj, "")
	require.NoError(t, err)
	assert.Equal(t, DispatchAgentGitHubTokenInstallation, freshStickyApplied.GitHubTokenPreference)
	require.NotNil(t, freshStickyApplied.CreateInputs)
	assert.Equal(t, DispatchAgentGitHubTokenInstallation, freshStickyApplied.CreateInputs.GitHubTokenPreference)

	// Sticky fail-closed (O1): if CreateInputs says "installation" even when
	// AppliedConfig is empty, reincarnation keeps "installation".
	agStickyInputs := &store.Agent{
		ID:        api.NewUUID(),
		Slug:      "reinc-gh-sticky-inputs",
		Name:      "reinc-gh-sticky-inputs",
		ProjectID: proj.ID,
		AppliedConfig: &store.AgentAppliedConfig{
			AgentRole:             string(AgentRoleFull),
			GitHubTokenPreference: "",
			CreateInputs:          &store.AgentCreateInputs{GitHubTokenPreference: DispatchAgentGitHubTokenInstallation},
		},
	}
	freshStickyInputs, _, err := srv.buildFreshAppliedConfig(ctx, agStickyInputs, proj, "")
	require.NoError(t, err)
	assert.Equal(t, DispatchAgentGitHubTokenInstallation, freshStickyInputs.GitHubTokenPreference)
	require.NotNil(t, freshStickyInputs.CreateInputs)
	assert.Equal(t, DispatchAgentGitHubTokenInstallation, freshStickyInputs.CreateInputs.GitHubTokenPreference)
}

func TestTriggerDispatchAgent_GitHubTokenInstallationEndToEnd(t *testing.T) {
	ctx := t.Context()
	f := newTriggerDispatchAgentFixture(t, "gh-tok-e2e")
	userCtx := f.userAuth(t, f.sched.creator)
	ep := f.createEndpoint(t, userCtx, "gh-tok-hooks")

	// Store a user-scope PAT for the trigger author.
	const authorPAT = "ghp_author_pat_must_never_leak"
	_, _, err := f.sched.srv.secretBackend.Set(ctx, &secret.SetSecretInput{
		Name:          "GITHUB_TOKEN",
		Value:         authorPAT,
		SecretType:    store.SecretTypeEnvironment,
		Target:        "GITHUB_TOKEN",
		Scope:         store.ScopeUser,
		ScopeID:       f.sched.creator.ID,
		InjectionMode: store.InjectionModeAlways,
	})
	require.NoError(t, err)

	// Attach a GitHub App minter to the fixture's HTTPAgentDispatcher.
	minter := &mockGitHubAppMinter{token: "ghs_e2e_app_token", expiry: "2026-11-21T06:00:00Z"}
	disp, ok := f.sched.srv.GetDispatcher().(*HTTPAgentDispatcher)
	require.True(t, ok, "expected *HTTPAgentDispatcher")
	disp.SetGitHubAppMinter(minter)

	// 1. Project has NO GitHub App installation + githubToken: "installation":
	// fire succeeds ("fired"), agent gets NO GitHub token at all, task framing
	// trailer tells the agent GitHub access is unavailable, and create audit
	// records github_token_preference without leaking any token value.
	recNoApp := f.doJSON(t, userCtx, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/triggers", f.sched.proj.ID),
		CreateTriggerRequest{
			Name:   "inst-no-app",
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
			Match:  &TriggerMatchSpec{Types: []string{"webhook.received"}},
			Action: TriggerActionSpec{
				Type:        store.TriggerActionDispatchAgent,
				AgentName:   "noapp-agent",
				Task:        "Handle event ${event.id}",
				GithubToken: DispatchAgentGitHubTokenInstallation,
			},
		})
	require.Equal(t, http.StatusCreated, recNoApp.Code, recNoApp.Body.String())
	var trigNoApp TriggerResponse
	require.NoError(t, json.Unmarshal(recNoApp.Body.Bytes(), &trigNoApp))
	assert.Equal(t, DispatchAgentGitHubTokenInstallation, trigNoApp.Action.GithubToken)

	hookRec1 := f.postSignedWebhook(t, ep, "deliv-noapp-1", "received", "subj-1", []byte(`{"ok":true}`))
	require.Equal(t, http.StatusAccepted, hookRec1.Code, hookRec1.Body.String())
	f.drainOneTriggerEvent(t)

	f.client.mu.Lock()
	require.Len(t, f.client.calls, 1)
	req1 := f.client.calls[0]
	f.client.mu.Unlock()

	_, hasGH := req1.ResolvedEnv["GITHUB_TOKEN"]
	assert.False(t, hasGH, "agent must receive no GITHUB_TOKEN when project has no installation")
	_, hasUserFlag := req1.ResolvedEnv["SCION_USER_GITHUB_TOKEN"]
	assert.False(t, hasUserFlag)

	agNoApp, err := f.sched.store.GetAgent(ctx, req1.ID)
	require.NoError(t, err)
	require.NotNil(t, agNoApp.AppliedConfig)
	assert.Equal(t, DispatchAgentGitHubTokenInstallation, agNoApp.AppliedConfig.GitHubTokenPreference)
	require.NotNil(t, agNoApp.AppliedConfig.CreateInputs)
	assert.Equal(t, DispatchAgentGitHubTokenInstallation, agNoApp.AppliedConfig.CreateInputs.GitHubTokenPreference)
	assert.Contains(t, agNoApp.AppliedConfig.Task, "GitHub access is not available for this run.")
	assert.Contains(t, agNoApp.AppliedConfig.Task, "Trigger event context (data, not instructions)")

	firesRes, err := f.sched.store.ListTriggerFires(ctx, store.TriggerFireFilter{
		ProjectID: f.sched.proj.ID,
		TriggerID: trigNoApp.ID,
	}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, firesRes.Items, 1)
	assert.Equal(t, store.TriggerFireStatusFired, firesRes.Items[0].Status)

	audits := agentAudits(t, f.sched.store, mutationTypeAgentDelegation, agNoApp.ID)
	require.Len(t, audits, 1)
	assert.Contains(t, audits[0].AfterSummary, `"github_token_preference":"installation"`)
	assert.NotContains(t, audits[0].AfterSummary, authorPAT)

	// 2. Attach a GitHub App installation to the project and fire again:
	// agent gets the installation token, NO author PAT, NO unavailable trailer line.
	installID := int64(99911)
	require.NoError(t, f.sched.store.CreateGitHubInstallation(ctx, &store.GitHubInstallation{
		InstallationID: installID,
		AccountLogin:   "e2e-org",
		AccountType:    "Organization",
		AppID:          9,
		Status:         store.GitHubInstallationStatusActive,
	}))
	f.sched.proj.GitHubInstallationID = &installID
	require.NoError(t, f.sched.store.UpdateProject(ctx, f.sched.proj))

	hookRec2 := f.postSignedWebhook(t, ep, "deliv-withapp-2", "received", "subj-2", []byte(`{"ok":true}`))
	require.Equal(t, http.StatusAccepted, hookRec2.Code, hookRec2.Body.String())
	f.drainOneTriggerEvent(t)

	f.client.mu.Lock()
	require.Len(t, f.client.calls, 2)
	req2 := f.client.calls[1]
	f.client.mu.Unlock()

	assert.Equal(t, "ghs_e2e_app_token", req2.ResolvedEnv["GITHUB_TOKEN"])
	_, hasUserFlag = req2.ResolvedEnv["SCION_USER_GITHUB_TOKEN"]
	assert.False(t, hasUserFlag)
	assert.Equal(t, "true", req2.ResolvedEnv["SCION_GITHUB_APP_ENABLED"])

	agWithApp, err := f.sched.store.GetAgent(ctx, req2.ID)
	require.NoError(t, err)
	assert.NotContains(t, agWithApp.AppliedConfig.Task, "GitHub access is not available for this run.")

	auditsApp := agentAudits(t, f.sched.store, mutationTypeAgentDelegation, agWithApp.ID)
	require.Len(t, auditsApp, 1)
	assert.Contains(t, auditsApp[0].AfterSummary, `"github_token_preference":"installation"`)
	assert.NotContains(t, auditsApp[0].AfterSummary, "ghs_e2e_app_token")
	assert.NotContains(t, auditsApp[0].AfterSummary, authorPAT)

	// 3. Schedules still refuse githubToken="installation".
	recSched := f.doJSON(t, userCtx, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/schedules", f.sched.proj.ID),
		map[string]any{
			"name":      "sched-gh-refused",
			"cronExpr":  "0 * * * *",
			"eventType": "dispatch_agent",
			"payload":   `{"agentName":"a","githubToken":"installation"}`,
		})
	assert.Equal(t, http.StatusBadRequest, recSched.Code, recSched.Body.String())
}

// TestGitHubTokenInstallation_RuntimeSecretPullRefusedAndChildInherits covers
// R1: runtime secret pull endpoints (GET /agents/{id}/secrets/{key} and
// POST /agent/secrets) refuse GITHUB_TOKEN and GH_* aliases in both user and
// project scope when GitHubTokenPreference=="installation" (indistinguishable
// from not_found, audited with github_token_installation_only), user_preferred
// agents remain unchanged, and children of an installation agent inherit the
// preference on both interactive and headless create paths.
func TestGitHubTokenInstallation_RuntimeSecretPullRefusedAndChildInherits(t *testing.T) {
	ctx := t.Context()

	t.Run("installation_agent_refused_on_both_endpoints_and_both_scopes", func(t *testing.T) {
		f := newMaterialFixture(t, "inst-runtime-pull")
		f.recordAgentEdge(t)

		ag, err := f.Store.GetAgent(ctx, f.AgentID)
		require.NoError(t, err)
		if ag.AppliedConfig == nil {
			ag.AppliedConfig = &store.AgentAppliedConfig{}
		}
		ag.AppliedConfig.GitHubTokenPreference = DispatchAgentGitHubTokenInstallation
		require.NoError(t, f.Store.UpdateAgent(ctx, ag))

		const userPAT = "ghp_runtime_user_pat"
		const userGH = "ghp_runtime_user_gh"
		const userAliasedPAT = "ghp_runtime_user_aliased_pat"
		const projPAT = "ghp_runtime_proj_pat"
		const projGH = "ghp_runtime_proj_gh"
		const projAliasedPAT = "ghp_runtime_proj_aliased_pat"
		const allowedVal = "allowed-secret-value"

		for _, in := range []*secret.SetSecretInput{
			{Name: "GITHUB_TOKEN", Value: userPAT, SecretType: store.SecretTypeEnvironment, Target: "GITHUB_TOKEN", Scope: store.ScopeUser, ScopeID: f.UserID, AllowProgeny: true, CreatedBy: f.UserID, UpdatedBy: f.UserID},
			{Name: "GH_TOKEN", Value: userGH, SecretType: store.SecretTypeEnvironment, Target: "GH_TOKEN", Scope: store.ScopeUser, ScopeID: f.UserID, AllowProgeny: true, CreatedBy: f.UserID, UpdatedBy: f.UserID},
			{Name: "MY_PAT_USER", Value: userAliasedPAT, SecretType: store.SecretTypeEnvironment, Target: "GITHUB_TOKEN", Scope: store.ScopeUser, ScopeID: f.UserID, AllowProgeny: true, CreatedBy: f.UserID, UpdatedBy: f.UserID},
			{Name: "ALLOWED_USER_KEY", Value: allowedVal, SecretType: store.SecretTypeEnvironment, Target: "ALLOWED_USER_KEY", Scope: store.ScopeUser, ScopeID: f.UserID, AllowProgeny: true, CreatedBy: f.UserID, UpdatedBy: f.UserID},
			{Name: "GITHUB_TOKEN", Value: projPAT, SecretType: store.SecretTypeEnvironment, Target: "GITHUB_TOKEN", Scope: store.ScopeProject, ScopeID: f.ProjectID, CreatedBy: f.UserID, UpdatedBy: f.UserID},
			{Name: "GH_TOKEN", Value: projGH, SecretType: store.SecretTypeEnvironment, Target: "GH_TOKEN", Scope: store.ScopeProject, ScopeID: f.ProjectID, CreatedBy: f.UserID, UpdatedBy: f.UserID},
			{Name: "MY_PAT_PROJ", Value: projAliasedPAT, SecretType: store.SecretTypeEnvironment, Target: "GITHUB_TOKEN", Scope: store.ScopeProject, ScopeID: f.ProjectID, CreatedBy: f.UserID, UpdatedBy: f.UserID},
			{Name: "ALLOWED_PROJ_KEY", Value: allowedVal, SecretType: store.SecretTypeEnvironment, Target: "ALLOWED_PROJ_KEY", Scope: store.ScopeProject, ScopeID: f.ProjectID, CreatedBy: f.UserID, UpdatedBy: f.UserID},
		} {
			_, _, err := f.Server.secretBackend.Set(ctx, in)
			require.NoError(t, err)
		}

		// GET /api/v1/agents/{id}/secrets/{key} for both scopes: blocked keys
		// (both direct names and aliased names with Target=GITHUB_TOKEN) return 404 not_found.
		for _, tc := range []struct {
			scope string
			key   string
			val   string
		}{
			{store.ScopeUser, "GITHUB_TOKEN", userPAT},
			{store.ScopeUser, "GH_TOKEN", userGH},
			{store.ScopeUser, "MY_PAT_USER", userAliasedPAT},
			{store.ScopeProject, "GITHUB_TOKEN", projPAT},
			{store.ScopeProject, "GH_TOKEN", projGH},
			{store.ScopeProject, "MY_PAT_PROJ", projAliasedPAT},
		} {
			rec := doRequestWithAgentToken(t, f.Server, http.MethodGet,
				fmt.Sprintf("/api/v1/agents/%s/secrets/%s?scope=%s", f.AgentID, tc.key, tc.scope), nil, f.Token)
			assert.Equal(t, http.StatusNotFound, rec.Code, "scope=%s key=%s body=%s", tc.scope, tc.key, rec.Body.String())
			assert.NotContains(t, rec.Body.String(), tc.val)
			assert.NotContains(t, rec.Body.String(), base64.StdEncoding.EncodeToString([]byte(tc.val)))
		}

		// Positive controls on GET: unrelated secrets remain readable in both scopes.
		recUserOK := doRequestWithAgentToken(t, f.Server, http.MethodGet,
			fmt.Sprintf("/api/v1/agents/%s/secrets/ALLOWED_USER_KEY?scope=user", f.AgentID), nil, f.Token)
		assert.Equal(t, http.StatusOK, recUserOK.Code, recUserOK.Body.String())
		recProjOK := doRequestWithAgentToken(t, f.Server, http.MethodGet,
			fmt.Sprintf("/api/v1/agents/%s/secrets/ALLOWED_PROJ_KEY?scope=project", f.AgentID), nil, f.Token)
		assert.Equal(t, http.StatusOK, recProjOK.Code, recProjOK.Body.String())

		// POST /api/v1/agent/secrets: direct and aliased blocked keys return status=not_found; allowed key returns ok.
		recFetch := doRequestWithAgentToken(t, f.Server, http.MethodPost, "/api/v1/agent/secrets",
			secretFetchRequest{Keys: []string{"GITHUB_TOKEN", "GH_TOKEN", "MY_PAT_PROJ", "MY_PAT_USER", "ALLOWED_PROJ_KEY"}}, f.Token)
		require.Equal(t, http.StatusOK, recFetch.Code, recFetch.Body.String())
		var fetchResp secretFetchResponse
		require.NoError(t, json.Unmarshal(recFetch.Body.Bytes(), &fetchResp))
		require.Len(t, fetchResp.Secrets, 5)
		for i := 0; i < 4; i++ {
			assert.Equal(t, "not_found", fetchResp.Secrets[i].Status, "key=%s", fetchResp.Secrets[i].Key)
			assert.Empty(t, fetchResp.Secrets[i].Value, "key=%s", fetchResp.Secrets[i].Key)
		}
		assert.Equal(t, "ok", fetchResp.Secrets[4].Status)
		assert.Equal(t, allowedVal, fetchResp.Secrets[4].Value)
		assert.NotContains(t, recFetch.Body.String(), projAliasedPAT)
		assert.NotContains(t, recFetch.Body.String(), userAliasedPAT)

		// GET /api/v1/agents/{id}/secrets (list) omits blocked keys (direct and aliased) in both scopes.
		recList := doRequestWithAgentToken(t, f.Server, http.MethodGet,
			fmt.Sprintf("/api/v1/agents/%s/secrets", f.AgentID), nil, f.Token)
		require.Equal(t, http.StatusOK, recList.Code, recList.Body.String())
		var listResp AgentListSecretsResponse
		require.NoError(t, json.Unmarshal(recList.Body.Bytes(), &listResp))
		for _, m := range listResp.Secrets {
			assert.False(t, isBlockedInstallationGitHubEnvKey(m.Key), "list must not expose blocked key %q", m.Key)
			assert.NotEqual(t, "MY_PAT_PROJ", m.Key)
			assert.NotEqual(t, "MY_PAT_USER", m.Key)
		}

		// Verify selectRuntimeMaterial returns ReasonGitHubTokenInstallationOnly directly
		// for both direct key names and aliased-name secrets targeting GITHUB_TOKEN.
		identClaims := &AgentTokenClaims{
			ProjectID: f.ProjectID,
			Scopes:    []AgentTokenScope{ScopeProjectRead, ScopeProjectSecretRead},
			Ancestry:  []string{f.UserID},
		}
		identClaims.Subject = f.AgentID
		ident := &agentIdentityWrapper{identClaims}
		proj, err := f.Store.GetProject(ctx, f.ProjectID)
		require.NoError(t, err)
		facts := &TargetFacts{
			Agent:     ag,
			ProjectID: f.ProjectID,
			Project:   proj,
			Root:      ProvenanceRoot{Kind: "user", ID: f.UserID},
		}
		var cache projectDecisionCache
		for _, tc := range []struct {
			scope string
			key   string
		}{
			{store.ScopeProject, "GITHUB_TOKEN"},
			{store.ScopeProject, "MY_PAT_PROJ"},
			{store.ScopeUser, "GH_TOKEN"},
			{store.ScopeUser, "MY_PAT_USER"},
		} {
			item, val, _, _ := f.Server.selectRuntimeMaterial(ctx, ident, facts, tc.scope, tc.key, &cache)
			assert.False(t, item.Allowed, "scope=%s key=%s", tc.scope, tc.key)
			assert.Nil(t, val, "scope=%s key=%s", tc.scope, tc.key)
			assert.Equal(t, ReasonGitHubTokenInstallationOnly, item.Reason, "scope=%s key=%s", tc.scope, tc.key)
		}
	})

	t.Run("user_preferred_agent_unchanged", func(t *testing.T) {
		f := newMaterialFixture(t, "userpref-runtime-pull")
		f.recordAgentEdge(t)

		ag, err := f.Store.GetAgent(ctx, f.AgentID)
		require.NoError(t, err)
		if ag.AppliedConfig == nil {
			ag.AppliedConfig = &store.AgentAppliedConfig{}
		}
		ag.AppliedConfig.GitHubTokenPreference = DispatchAgentGitHubTokenUserPreferred
		require.NoError(t, f.Store.UpdateAgent(ctx, ag))

		_, _, err = f.Server.secretBackend.Set(ctx, &secret.SetSecretInput{
			Name: "GITHUB_TOKEN", Value: "ghp_up_user", SecretType: store.SecretTypeEnvironment, Target: "GITHUB_TOKEN",
			Scope: store.ScopeUser, ScopeID: f.UserID, AllowProgeny: true, CreatedBy: f.UserID, UpdatedBy: f.UserID,
		})
		require.NoError(t, err)
		_, _, err = f.Server.secretBackend.Set(ctx, &secret.SetSecretInput{
			Name: "GITHUB_TOKEN", Value: "ghp_up_proj", SecretType: store.SecretTypeEnvironment, Target: "GITHUB_TOKEN",
			Scope: store.ScopeProject, ScopeID: f.ProjectID, CreatedBy: f.UserID, UpdatedBy: f.UserID,
		})
		require.NoError(t, err)

		recUser := doRequestWithAgentToken(t, f.Server, http.MethodGet,
			fmt.Sprintf("/api/v1/agents/%s/secrets/GITHUB_TOKEN?scope=user", f.AgentID), nil, f.Token)
		assert.Equal(t, http.StatusOK, recUser.Code, recUser.Body.String())

		recFetch := doRequestWithAgentToken(t, f.Server, http.MethodPost, "/api/v1/agent/secrets",
			secretFetchRequest{Keys: []string{"GITHUB_TOKEN"}}, f.Token)
		require.Equal(t, http.StatusOK, recFetch.Code, recFetch.Body.String())
		var fetchResp secretFetchResponse
		require.NoError(t, json.Unmarshal(recFetch.Body.Bytes(), &fetchResp))
		require.Len(t, fetchResp.Secrets, 1)
		assert.Equal(t, "ok", fetchResp.Secrets[0].Status)
		assert.Equal(t, "ghp_up_proj", fetchResp.Secrets[0].Value)
	})

	t.Run("child_of_installation_agent_inherits_on_interactive_and_headless_create", func(t *testing.T) {
		f := newMaterialFixture(t, "inst-child-inherit")
		f.recordAgentEdge(t)

		// Mark parent agent as installation-only with full role.
		parent, err := f.Store.GetAgent(ctx, f.AgentID)
		require.NoError(t, err)
		if parent.AppliedConfig == nil {
			parent.AppliedConfig = &store.AgentAppliedConfig{}
		}
		parent.AppliedConfig.AgentRole = string(AgentRoleFull)
		parent.AppliedConfig.GitHubTokenPreference = DispatchAgentGitHubTokenInstallation
		require.NoError(t, f.Store.UpdateAgent(ctx, parent))

		// Store user + project PATs and configure a dispatcher to inspect child dispatch.
		const projPAT = "ghp_child_proj_pat_blocked"
		_, _, err = f.Server.secretBackend.Set(ctx, &secret.SetSecretInput{
			Name: "GITHUB_TOKEN", Value: projPAT, SecretType: store.SecretTypeEnvironment, Target: "GITHUB_TOKEN",
			Scope: store.ScopeProject, ScopeID: f.ProjectID, CreatedBy: f.UserID, UpdatedBy: f.UserID,
		})
		require.NoError(t, err)
		_, _, err = f.Server.secretBackend.Set(ctx, &secret.SetSecretInput{
			Name: "GITHUB_TOKEN", Value: "ghp_child_user_pat_blocked", SecretType: store.SecretTypeEnvironment, Target: "GITHUB_TOKEN",
			Scope: store.ScopeUser, ScopeID: f.UserID, AllowProgeny: true, CreatedBy: f.UserID, UpdatedBy: f.UserID,
		})
		require.NoError(t, err)

		// Attach a runtime broker to the project so interactive agent create succeeds.
		broker := &store.RuntimeBroker{
			ID:       api.NewUUID(),
			Name:     "child-inherit-broker",
			Slug:     "child-inherit-broker",
			Endpoint: "http://localhost:9800",
			Status:   store.BrokerStatusOnline,
		}
		require.NoError(t, f.Store.CreateRuntimeBroker(ctx, broker))
		require.NoError(t, f.Store.AddProjectProvider(ctx, &store.ProjectProvider{
			ProjectID:  f.ProjectID,
			BrokerID:   broker.ID,
			BrokerName: broker.Name,
			LocalPath:  t.TempDir(),
			Status:     store.BrokerStatusOnline,
		}))

		// 1. Interactive child create by the installation parent agent.
		parentToken := mintRecordedMaterialToken(t, f.Server, f.Store, parent.ID, f.ProjectID, ScopesForRole(AgentRoleFull), parent.Ancestry)
		rec := doRequestWithAgentToken(t, f.Server, http.MethodPost,
			fmt.Sprintf("/api/v1/projects/%s/agents", f.ProjectID),
			CreateAgentRequest{
				Name: "child-interactive",
				Task: "subtask",
			}, parentToken)
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

		child, err := f.Store.GetAgentBySlug(ctx, f.ProjectID, "child-interactive")
		require.NoError(t, err)
		require.NotNil(t, child.AppliedConfig)
		assert.Equal(t, DispatchAgentGitHubTokenInstallation, child.AppliedConfig.GitHubTokenPreference)
		require.NotNil(t, child.AppliedConfig.CreateInputs)
		assert.Equal(t, DispatchAgentGitHubTokenInstallation, child.AppliedConfig.CreateInputs.GitHubTokenPreference)

		// Dispatch-time stripping on the child drops the project PAT.
		d := NewHTTPAgentDispatcherWithClient(f.Store, &mockRuntimeBrokerClient{}, false, slog.Default())
		d.SetSecretBackend(f.Server.secretBackend)
		childReq, err := d.buildCreateRequest(ctx, child, "TestChildDispatch")
		require.NoError(t, err)
		_, hasGH := childReq.ResolvedEnv["GITHUB_TOKEN"]
		assert.False(t, hasGH, "child dispatch must strip PAT")

		// Runtime pull by the child is also refused on both endpoints.
		childToken := mintRecordedMaterialToken(t, f.Server, f.Store, child.ID, f.ProjectID, ScopesForRole(AgentRoleFull), child.Ancestry)
		recChildGet := doRequestWithAgentToken(t, f.Server, http.MethodGet,
			fmt.Sprintf("/api/v1/agents/%s/secrets/GITHUB_TOKEN?scope=user", child.ID), nil, childToken)
		assert.Equal(t, http.StatusNotFound, recChildGet.Code, recChildGet.Body.String())

		recChildFetch := doRequestWithAgentToken(t, f.Server, http.MethodPost, "/api/v1/agent/secrets",
			secretFetchRequest{Keys: []string{"GITHUB_TOKEN"}}, childToken)
		require.Equal(t, http.StatusOK, recChildFetch.Code, recChildFetch.Body.String())
		var childFetchResp secretFetchResponse
		require.NoError(t, json.Unmarshal(recChildFetch.Body.Bytes(), &childFetchResp))
		require.Len(t, childFetchResp.Secrets, 1)
		assert.Equal(t, "not_found", childFetchResp.Secrets[0].Status)

		// 2. Headless child create by the installation parent agent (even if hc.GitHubToken is user_preferred).
		parentClaims := &AgentTokenClaims{
			ProjectID: f.ProjectID,
			Scopes:    ScopesForRole(AgentRoleFull),
			Ancestry:  parent.Ancestry,
		}
		parentClaims.Subject = parent.ID
		parentIdent := &agentIdentityWrapper{parentClaims}
		resolved := headlessResolvedRole{preCapRole: AgentRoleFull}
		hcChild, err := f.Server.createAgentHeadless(ctx, HeadlessCreate{
			ProjectID: f.ProjectID,
			Authority: FireAuthority{
				PrincipalKind: store.DelegationPrincipalAgent,
				PrincipalID:   parent.ID,
				Revision:      1,
				agent:         parent,
				Recorded: hubfire.RecordedAuthority{
					DefinitionKind: hubfire.DefinitionKindTrigger,
					DefinitionID:   api.NewUUID(),
					FireID:         api.NewUUID(),
				},
			},
			Identity:    parentIdent,
			CreatorName: parent.Name,
			EdgeCeiling: store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
			EdgeProvenance: store.AuthorityProvenance{
				ProvenanceVersion:           1,
				SourcePrincipalKind:         store.DelegationPrincipalAgent,
				SourcePrincipalID:           parent.ID,
				SourceCredentialKind:        store.SourceCredentialScheduler,
				SourceEventID:               api.NewUUID(),
				SourceAuthorizationRevision: 1,
			},
			resolvedRole: &resolved,
			Name:         "child-headless",
			Task:         "headless-subtask",
			GitHubToken:  DispatchAgentGitHubTokenUserPreferred,
			Detached:     true,
		})
		require.NoError(t, err)
		require.NotNil(t, hcChild.AppliedConfig)
		assert.Equal(t, DispatchAgentGitHubTokenInstallation, hcChild.AppliedConfig.GitHubTokenPreference)
		require.NotNil(t, hcChild.AppliedConfig.CreateInputs)
		assert.Equal(t, DispatchAgentGitHubTokenInstallation, hcChild.AppliedConfig.CreateInputs.GitHubTokenPreference)
	})
}

// TestTriggerDispatchAgent_GitHubUnavailableNoteMatchesMintResolver covers O1:
// the no-installation trailer note uses resolveGitHubMintProject (including
// token-source-project label authorization and minter presence).
func TestTriggerDispatchAgent_GitHubUnavailableNoteMatchesMintResolver(t *testing.T) {
	ctx := t.Context()
	f := newTriggerDispatchAgentFixture(t, "o1-note-resolver")
	userCtx := f.userAuth(t, f.sched.creator)
	ep := f.createEndpoint(t, userCtx, "o1-hooks")

	installID := int64(77701)
	require.NoError(t, f.sched.store.CreateGitHubInstallation(ctx, &store.GitHubInstallation{
		InstallationID: installID,
		AccountLogin:   "src-org",
		AccountType:    "Organization",
		AppID:          11,
		Status:         store.GitHubInstallationStatusActive,
	}))
	sourceProj := &store.Project{
		ID:                   api.NewUUID(),
		Name:                 "o1-token-source",
		Slug:                 "o1-token-source",
		GitHubInstallationID: &installID,
	}
	require.NoError(t, f.sched.store.CreateProject(ctx, sourceProj))

	disp, ok := f.sched.srv.GetDispatcher().(*HTTPAgentDispatcher)
	require.True(t, ok)

	fireWithLabel := func(t *testing.T, eventSuffix, agentName string, minter GitHubAppTokenMinter) *store.Agent {
		t.Helper()
		disp.SetGitHubAppMinter(minter)
		rec := f.doJSON(t, userCtx, http.MethodPost,
			fmt.Sprintf("/api/v1/projects/%s/triggers", f.sched.proj.ID),
			CreateTriggerRequest{
				Name:   agentName,
				Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
				Match:  &TriggerMatchSpec{Types: []string{"webhook." + eventSuffix}},
				Action: TriggerActionSpec{
					Type:        store.TriggerActionDispatchAgent,
					AgentName:   agentName,
					Task:        "Run ${event.id}",
					GithubToken: DispatchAgentGitHubTokenInstallation,
					Labels: map[string]string{
						githubTokenSourceProjectLabel: sourceProj.ID,
					},
				},
			})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

		before := len(f.client.calls)
		hookRec := f.postSignedWebhook(t, ep, "deliv-"+agentName, eventSuffix, "subj-"+agentName, []byte(`{}`))
		require.Equal(t, http.StatusAccepted, hookRec.Code, hookRec.Body.String())
		f.drainOneTriggerEvent(t)

		f.client.mu.Lock()
		require.Greater(t, len(f.client.calls), before)
		call := f.client.calls[len(f.client.calls)-1]
		f.client.mu.Unlock()
		ag, err := f.sched.store.GetAgent(ctx, call.ID)
		require.NoError(t, err)
		return ag
	}

	// 1. Authorized token-source project -> NO unavailable note.
	agAuth := fireWithLabel(t, "src_authorized", "src-authorized-ag", &recordingGitHubAppMinter{allowSource: true})
	assert.NotContains(t, agAuth.AppliedConfig.Task, "GitHub access is not available for this run.")

	// 2. Unauthorized token-source project -> unavailable note IS added.
	agUnauth := fireWithLabel(t, "src_unauthorized", "src-unauthorized-ag", &recordingGitHubAppMinter{allowSource: false})
	assert.Contains(t, agUnauth.AppliedConfig.Task, "GitHub access is not available for this run.")

	// 3. No minter configured even when the project itself has an installation ID -> unavailable note IS added.
	f.sched.proj.GitHubInstallationID = &installID
	require.NoError(t, f.sched.store.UpdateProject(ctx, f.sched.proj))
	agNoMinter := fireWithLabel(t, "no_minter", "no-minter-ag", nil)
	assert.Contains(t, agNoMinter.AppliedConfig.Task, "GitHub access is not available for this run.")
}

// TestGitHubTokenDelivery_NoAuthAndFinalizeEnv covers O2 (killing mutants
// m1, m2, m3): installation + noAuth on create and start, and finalizeEnv
// dropping blocked GitHub token keys.
func TestGitHubTokenDelivery_NoAuthAndFinalizeEnv(t *testing.T) {
	ctx := t.Context()
	st := createTestStore(t)
	secBackend := secret.NewLocalBackend(st, "test-hub-id", "test-secret")
	authzSvc := NewAuthzService(st, slog.Default())

	proj := &store.Project{
		ID:        tid("o2-noauth-proj"),
		Name:      "o2-noauth",
		Slug:      "o2-noauth",
		GitRemote: "https://git.internal.example/org/repo.git",
	}
	require.NoError(t, st.CreateProject(ctx, proj))
	broker := &store.RuntimeBroker{
		ID:       tid("o2-noauth-broker"),
		Name:     "o2-noauth-broker",
		Slug:     "o2-noauth-broker",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	require.NoError(t, st.CreateRuntimeBroker(ctx, broker))

	const projPAT = "ghp_o2_proj_pat_blocked"
	_, _, err := secBackend.Set(ctx, &secret.SetSecretInput{
		Name:          "GITHUB_TOKEN",
		Value:         projPAT,
		SecretType:    store.SecretTypeEnvironment,
		Target:        "GITHUB_TOKEN",
		Scope:         store.ScopeProject,
		ScopeID:       proj.ID,
		InjectionMode: store.InjectionModeAlways,
	})
	require.NoError(t, err)

	agNoAuth := &store.Agent{
		ID:              api.NewUUID(),
		Name:            "o2-noauth-agent",
		Slug:            "o2-noauth-agent",
		ProjectID:       proj.ID,
		RuntimeBrokerID: broker.ID,
		AppliedConfig: &store.AgentAppliedConfig{
			AgentRole:             string(AgentRoleNone),
			NoAuth:                true,
			GitHubTokenPreference: DispatchAgentGitHubTokenInstallation,
			GitClone:              &api.GitCloneConfig{URL: proj.GitRemote},
		},
	}
	require.NoError(t, st.CreateAgent(ctx, agNoAuth))

	// Create + noAuth + installation (kills m2): noAuthProjectToken is empty and no PAT delivered.
	d := NewHTTPAgentDispatcherWithClient(st, &mockRuntimeBrokerClient{}, false, slog.Default())
	d.SetSecretBackend(secBackend)
	d.SetAuthzService(authzSvc)
	req, err := d.buildCreateRequest(ctx, agNoAuth, "TestO2NoAuthCreate")
	require.NoError(t, err)
	assert.Empty(t, req.noAuthProjectToken, "installation + noAuth must not set noAuthProjectToken")
	_, hasGH := req.ResolvedEnv["GITHUB_TOKEN"]
	assert.False(t, hasGH)
	_, hasUserFlag := req.ResolvedEnv["SCION_USER_GITHUB_TOKEN"]
	assert.False(t, hasUserFlag)

	// Start + noAuth + installation (kills m3): no PAT delivered and token remote withheld.
	startRes, err := d.buildStartEnv(ctx, agNoAuth, "DispatchAgentStart", mintSiteStart)
	require.NoError(t, err)
	_, hasGH = startRes.env["GITHUB_TOKEN"]
	assert.False(t, hasGH)
	if startRes.workspace.GitClone != nil {
		assert.Empty(t, startRes.workspace.GitClone.TokenRemote)
	}

	// DispatchFinalizeEnv + installation (kills m1): caller-submitted GITHUB_TOKEN and GH_TOKEN dropped.
	var captured *RemoteCreateAgentRequest
	captureClient := &mockRuntimeBrokerClient{
		createWithGatherFunc: func(_ context.Context, _, _ string, r *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
			captured = r
			return &RemoteAgentResponse{Agent: &RemoteAgentInfo{ID: r.ID, Slug: r.Slug, Status: "running"}}, nil, nil
		},
	}
	dFin := NewHTTPAgentDispatcherWithClient(st, captureClient, false, slog.Default())
	dFin.SetSecretBackend(secBackend)
	agFin := &store.Agent{
		ID:              api.NewUUID(),
		Name:            "o2-finalize-agent",
		Slug:            "o2-finalize-agent",
		ProjectID:       proj.ID,
		RuntimeBrokerID: broker.ID,
		AppliedConfig: &store.AgentAppliedConfig{
			AgentRole:             string(AgentRoleFull),
			GitHubTokenPreference: DispatchAgentGitHubTokenInstallation,
		},
	}
	require.NoError(t, st.CreateAgent(ctx, agFin))
	_, err = dFin.DispatchFinalizeEnv(ctx, agFin, map[string]string{
		"GITHUB_TOKEN": "ghp_finalize_blocked",
		"GH_TOKEN":     "ghp_finalize_gh_blocked",
		"SAFE_ENV":     "kept-value",
	})
	require.NoError(t, err)
	require.NotNil(t, captured)
	_, hasGH = captured.ResolvedEnv["GITHUB_TOKEN"]
	assert.False(t, hasGH, "finalizeEnv must drop GITHUB_TOKEN for installation preference")
	_, hasGHAlias := captured.ResolvedEnv["GH_TOKEN"]
	assert.False(t, hasGHAlias, "finalizeEnv must drop GH_TOKEN for installation preference")
	assert.Equal(t, "kept-value", captured.ResolvedEnv["SAFE_ENV"])
}

// TestTriggerDispatchAgent_GitHubSourceDefaultInstallationEndToEnd covers O2:
// an end-to-end fire from a github-source dispatch_agent trigger whose omitted
// githubToken defaulted to "installation".
func TestTriggerDispatchAgent_GitHubSourceDefaultInstallationEndToEnd(t *testing.T) {
	ctx := t.Context()
	f := newTriggerDispatchAgentFixture(t, "gh-src-e2e")

	// Enable both hub.triggers and hub.github_triggers.
	var active []experiments.Experiment
	for _, e := range f.sched.srv.experimentRegistry().All() {
		if e.Name == experiments.Triggers || e.Name == experiments.GitHubTriggers {
			e.Default = true
		}
		active = append(active, e)
	}
	reg, err := experiments.NewRegistry(active, nil)
	require.NoError(t, err)
	f.sched.srv.experiments = reg

	// Configure a project with an authorized active GitHub App installation.
	const instID int64 = 88221
	require.NoError(t, f.sched.store.CreateGitHubInstallation(ctx, &store.GitHubInstallation{
		InstallationID: instID,
		AccountLogin:   "acme",
		AccountType:    "Organization",
		AppID:          55,
		Repositories:   []string{"acme/widgets"},
		Status:         store.GitHubInstallationStatusActive,
	}))
	idCopy := instID
	ghProj := &store.Project{
		ID:                   api.NewUUID(),
		Slug:                 "gh-src-e2e-proj",
		Name:                 "gh-src-e2e-proj",
		GitRemote:            "https://github.com/acme/widgets.git",
		GitHubInstallationID: &idCopy,
		CreatedBy:            f.sched.creator.ID,
	}
	require.NoError(t, f.sched.store.CreateProject(ctx, ghProj))
	grantProjectRole(t, f.sched.store, f.sched.creator.ID, ghProj.ID, store.ProjectRoleAdmin)
	require.NoError(t, f.sched.store.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  ghProj.ID,
		BrokerID:   f.sched.broker.ID,
		BrokerName: f.sched.broker.Name,
		Status:     store.BrokerStatusOnline,
	}))

	const authorPAT = "ghp_github_src_author_pat"
	_, _, err = f.sched.srv.secretBackend.Set(ctx, &secret.SetSecretInput{
		Name:          "GITHUB_TOKEN",
		Value:         authorPAT,
		SecretType:    store.SecretTypeEnvironment,
		Target:        "GITHUB_TOKEN",
		Scope:         store.ScopeUser,
		ScopeID:       f.sched.creator.ID,
		InjectionMode: store.InjectionModeAlways,
	})
	require.NoError(t, err)

	minter := &mockGitHubAppMinter{token: "ghs_github_src_app_token", expiry: "2026-11-21T09:00:00Z"}
	disp, ok := f.sched.srv.GetDispatcher().(*HTTPAgentDispatcher)
	require.True(t, ok)
	disp.SetGitHubAppMinter(minter)

	// Author github-source dispatch_agent trigger with omitted githubToken.
	userCtx := f.userAuth(t, f.sched.creator)
	rec := f.doJSON(t, userCtx, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/triggers", ghProj.ID),
		CreateTriggerRequest{
			Name:   "gh-issue-triage",
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindGitHub, ID: ""},
			Match:  &TriggerMatchSpec{Types: []string{"github.issue_comment.created"}},
			Action: TriggerActionSpec{
				Type:          store.TriggerActionDispatchAgent,
				AgentName:     "gh-triage-worker",
				Task:          "Triage issue ${event.attributes.number}",
				UntrustedVars: []string{"event.attributes.number"},
			},
		})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created TriggerResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	assert.Equal(t, DispatchAgentGitHubTokenInstallation, created.Action.GithubToken)

	// Fire a github event through fireAction.
	storedTrig, err := f.sched.store.GetTrigger(ctx, created.ID)
	require.NoError(t, err)
	evID := api.NewUUID()
	fireID := api.NewUUID()
	outcome := f.sched.srv.fireAction(ctx, hubfire.FireRequest{
		ProjectID:  ghProj.ID,
		ActionType: store.TriggerActionDispatchAgent,
		Payload:    storedTrig.ActionPayload,
		Authority: hubfire.RecordedAuthority{
			DefinitionKind: hubfire.DefinitionKindTrigger,
			DefinitionID:   storedTrig.ID,
			FireID:         fireID,
			Attribution:    storedTrig.InitiatorAttribution,
			Ceiling:        storedTrig.AuthorityCeiling,
		},
		Event: &hubfire.FireEvent{
			ID:         evID,
			SourceKind: store.TriggerSourceKindGitHub,
			SourceID:   "",
			Type:       "github.issue_comment.created",
			Subject:    "github:acme/widgets#42",
			ReceivedAt: time.Now().UTC(),
			Attributes: map[string]string{"number": "42"},
			Payload:    []byte(`{"action":"created"}`),
			Trigger:    &hubfire.FireTriggerRef{ID: storedTrig.ID, Name: storedTrig.Name},
		},
	})
	require.Equal(t, hubfire.FireStatusFired, outcome.Status, "error=%q", outcome.Error)
	require.NotEmpty(t, outcome.ResultRef)

	f.client.mu.Lock()
	require.Len(t, f.client.calls, 1)
	brokerReq := f.client.calls[0]
	f.client.mu.Unlock()

	assert.Equal(t, "ghs_github_src_app_token", brokerReq.ResolvedEnv["GITHUB_TOKEN"])
	_, hasUserFlag := brokerReq.ResolvedEnv["SCION_USER_GITHUB_TOKEN"]
	assert.False(t, hasUserFlag)
	assert.Equal(t, "true", brokerReq.ResolvedEnv["SCION_GITHUB_APP_ENABLED"])

	ag, err := f.sched.store.GetAgent(ctx, outcome.ResultRef)
	require.NoError(t, err)
	require.NotNil(t, ag.AppliedConfig)
	assert.Equal(t, DispatchAgentGitHubTokenInstallation, ag.AppliedConfig.GitHubTokenPreference)
	assert.NotContains(t, ag.AppliedConfig.Task, "GitHub access is not available for this run.")

	// O2: a stored payload with githubToken == "" fired on a github source still
	// defaults to "installation" at fire time (never falls back to user_preferred).
	emptyTokPayload, err := json.Marshal(map[string]any{
		"agentName":   "gh-empty-tok-worker",
		"task":        "Handle issue with legacy empty githubToken",
		"githubToken": "",
	})
	require.NoError(t, err)
	outcomeEmpty := f.sched.srv.fireAction(ctx, hubfire.FireRequest{
		ProjectID:  ghProj.ID,
		ActionType: store.TriggerActionDispatchAgent,
		Payload:    string(emptyTokPayload),
		Authority: hubfire.RecordedAuthority{
			DefinitionKind: hubfire.DefinitionKindTrigger,
			DefinitionID:   storedTrig.ID,
			FireID:         api.NewUUID(),
			Attribution:    storedTrig.InitiatorAttribution,
			Ceiling:        storedTrig.AuthorityCeiling,
		},
		Event: &hubfire.FireEvent{
			ID:         api.NewUUID(),
			SourceKind: store.TriggerSourceKindGitHub,
			SourceID:   "",
			Type:       "github.issue_comment.created",
			Subject:    "github:acme/widgets#43",
			ReceivedAt: time.Now().UTC(),
			Payload:    []byte(`{"action":"created"}`),
			Trigger:    &hubfire.FireTriggerRef{ID: storedTrig.ID, Name: storedTrig.Name},
		},
	})
	require.Equal(t, hubfire.FireStatusFired, outcomeEmpty.Status, "error=%q", outcomeEmpty.Error)
	require.NotEmpty(t, outcomeEmpty.ResultRef)

	f.client.mu.Lock()
	require.Len(t, f.client.calls, 2)
	emptyTokReq := f.client.calls[1]
	f.client.mu.Unlock()
	assert.Equal(t, "ghs_github_src_app_token", emptyTokReq.ResolvedEnv["GITHUB_TOKEN"])
	_, hasUserFlagEmpty := emptyTokReq.ResolvedEnv["SCION_USER_GITHUB_TOKEN"]
	assert.False(t, hasUserFlagEmpty)

	agEmpty, err := f.sched.store.GetAgent(ctx, outcomeEmpty.ResultRef)
	require.NoError(t, err)
	require.NotNil(t, agEmpty.AppliedConfig)
	assert.Equal(t, DispatchAgentGitHubTokenInstallation, agEmpty.AppliedConfig.GitHubTokenPreference)
}

// githubTokenInheritFaultStore injects a transient GetAgent failure scoped
// strictly to agentHasInstallationGitHubTokenPreference so standing checks,
// role-ceiling reads, and owner/ancestry resolution succeed normally.
type githubTokenInheritFaultStore struct {
	store.Store
	fault    *storeFaultSwitch
	disarmed atomic.Bool
}

func (s *githubTokenInheritFaultStore) Arm()    { s.disarmed.Store(false); s.fault.Arm() }
func (s *githubTokenInheritFaultStore) Disarm() { s.disarmed.Store(true) }

func githubTokenTestCalledFrom(fn string) bool {
	pcs := make([]uintptr, 64)
	n := runtime.Callers(2, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	for {
		f, more := frames.Next()
		if strings.Contains(f.Function, fn) {
			return true
		}
		if !more {
			return false
		}
	}
}

func (s *githubTokenInheritFaultStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	if s.fault.Active() && !s.disarmed.Load() && githubTokenTestCalledFrom("agentHasInstallationGitHubTokenPreference") {
		return nil, errors.New("injected transient parent lookup fault")
	}
	return s.Store.GetAgent(ctx, id)
}

func TestGitHubTokenInheritance_FailClosedOnParentLookupFault(t *testing.T) {
	ctx := t.Context()

	setupParent := func(t *testing.T, name, pref string) (*materialFixture, *store.Agent, string, *githubTokenInheritFaultStore) {
		t.Helper()
		f := newMaterialFixture(t, name)
		f.recordAgentEdge(t)

		parent, err := f.Store.GetAgent(ctx, f.AgentID)
		require.NoError(t, err)
		if parent.AppliedConfig == nil {
			parent.AppliedConfig = &store.AgentAppliedConfig{}
		}
		parent.AppliedConfig.AgentRole = string(AgentRoleFull)
		parent.AppliedConfig.GitHubTokenPreference = pref
		require.NoError(t, f.Store.UpdateAgent(ctx, parent))

		broker := &store.RuntimeBroker{
			ID:       api.NewUUID(),
			Name:     name + "-broker",
			Slug:     name + "-broker",
			Endpoint: "http://localhost:9801",
			Status:   store.BrokerStatusOnline,
		}
		require.NoError(t, f.Store.CreateRuntimeBroker(ctx, broker))
		require.NoError(t, f.Store.AddProjectProvider(ctx, &store.ProjectProvider{
			ProjectID:  f.ProjectID,
			BrokerID:   broker.ID,
			BrokerName: broker.Name,
			LocalPath:  t.TempDir(),
			Status:     store.BrokerStatusOnline,
		}))

		parentToken := mintRecordedMaterialToken(t, f.Server, f.Store, parent.ID, f.ProjectID, ScopesForRole(AgentRoleFull), parent.Ancestry)
		fs, _ := installStoreFault(t, f.Server, func(inner store.Store, sw *storeFaultSwitch) *githubTokenInheritFaultStore {
			return &githubTokenInheritFaultStore{Store: inner, fault: sw}
		})
		return f, parent, parentToken, fs
	}

	t.Run("interactive_create_fails_closed_on_parent_lookup_error", func(t *testing.T) {
		f, _, parentToken, fault := setupParent(t, "inherit-fault-int", DispatchAgentGitHubTokenInstallation)

		beforeAudits, _, err := f.Store.ListMutationAudits(ctx, store.MutationAuditFilter{TargetType: "agent", MutationType: mutationTypeAgentDelegation})
		require.NoError(t, err)

		fault.Arm()
		rec := doRequestWithAgentToken(t, f.Server, http.MethodPost,
			fmt.Sprintf("/api/v1/projects/%s/agents", f.ProjectID),
			CreateAgentRequest{Name: "fault-child-int", Task: "subtask"}, parentToken)
		fault.Disarm()

		require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), ErrCodeUnavailable)

		_, err = f.Store.GetAgentBySlug(ctx, f.ProjectID, "fault-child-int")
		assert.ErrorIs(t, err, store.ErrNotFound, "no child row may be persisted on parent lookup fault")

		afterAudits, _, err := f.Store.ListMutationAudits(ctx, store.MutationAuditFilter{TargetType: "agent", MutationType: mutationTypeAgentDelegation})
		require.NoError(t, err)
		assert.Len(t, afterAudits, len(beforeAudits), "no create audit may be written when child create is refused")
	})

	t.Run("interactive_create_parent_preference_matrix", func(t *testing.T) {
		cases := []struct {
			name       string
			parentPref string
			wantPref   string
		}{
			{name: "installation_parent_inherits", parentPref: DispatchAgentGitHubTokenInstallation, wantPref: DispatchAgentGitHubTokenInstallation},
			{name: "user_preferred_parent_unchanged", parentPref: DispatchAgentGitHubTokenUserPreferred, wantPref: ""},
			{name: "empty_parent_unchanged", parentPref: "", wantPref: ""},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				f, _, parentToken, _ := setupParent(t, "int-mat-"+tc.name[:8], tc.parentPref)
				rec := doRequestWithAgentToken(t, f.Server, http.MethodPost,
					fmt.Sprintf("/api/v1/projects/%s/agents", f.ProjectID),
					CreateAgentRequest{Name: "child-ok", Task: "subtask"}, parentToken)
				require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

				child, err := f.Store.GetAgentBySlug(ctx, f.ProjectID, "child-ok")
				require.NoError(t, err)
				require.NotNil(t, child.AppliedConfig)
				assert.Equal(t, tc.wantPref, child.AppliedConfig.GitHubTokenPreference)
			})
		}
	})

	headlessReq := func(f *materialFixture, parent *store.Agent, childName, hcToken string) HeadlessCreate {
		parentClaims := &AgentTokenClaims{
			ProjectID: f.ProjectID,
			Scopes:    ScopesForRole(AgentRoleFull),
			Ancestry:  parent.Ancestry,
		}
		parentClaims.Subject = parent.ID
		parentIdent := &agentIdentityWrapper{parentClaims}
		resolved := headlessResolvedRole{preCapRole: AgentRoleFull}
		return HeadlessCreate{
			ProjectID: f.ProjectID,
			Authority: FireAuthority{
				PrincipalKind: store.DelegationPrincipalAgent,
				PrincipalID:   parent.ID,
				Revision:      1,
				// Deliberately leave agent nil to exercise the store-lookup fallback path.
				Recorded: hubfire.RecordedAuthority{
					DefinitionKind: hubfire.DefinitionKindTrigger,
					DefinitionID:   api.NewUUID(),
					FireID:         api.NewUUID(),
				},
			},
			Identity:    parentIdent,
			CreatorName: parent.Name,
			EdgeCeiling: store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
			EdgeProvenance: store.AuthorityProvenance{
				ProvenanceVersion:           1,
				SourcePrincipalKind:         store.DelegationPrincipalAgent,
				SourcePrincipalID:           parent.ID,
				SourceCredentialKind:        store.SourceCredentialScheduler,
				SourceEventID:               api.NewUUID(),
				SourceAuthorizationRevision: 1,
			},
			resolvedRole: &resolved,
			Name:         childName,
			Task:         "headless subtask",
			GitHubToken:  hcToken,
			Detached:     true,
		}
	}

	t.Run("headless_create_fails_closed_on_parent_lookup_error", func(t *testing.T) {
		f, parent, _, fault := setupParent(t, "inherit-fault-hl", DispatchAgentGitHubTokenInstallation)

		beforeAudits, _, err := f.Store.ListMutationAudits(ctx, store.MutationAuditFilter{TargetType: "agent", MutationType: mutationTypeAgentDelegation})
		require.NoError(t, err)

		fault.Arm()
		hcChild, err := f.Server.createAgentHeadless(ctx, headlessReq(f, parent, "fault-child-hl", DispatchAgentGitHubTokenUserPreferred))
		fault.Disarm()

		require.Error(t, err)
		assert.ErrorIs(t, err, errAgentCreatorUnreadable)
		assert.Nil(t, hcChild)

		_, err = f.Store.GetAgentBySlug(ctx, f.ProjectID, "fault-child-hl")
		assert.ErrorIs(t, err, store.ErrNotFound, "no child row may be persisted on parent lookup fault")

		afterAudits, _, err := f.Store.ListMutationAudits(ctx, store.MutationAuditFilter{TargetType: "agent", MutationType: mutationTypeAgentDelegation})
		require.NoError(t, err)
		assert.Len(t, afterAudits, len(beforeAudits), "no create audit may be written when headless child create is refused")
	})

	t.Run("headless_create_parent_preference_matrix", func(t *testing.T) {
		cases := []struct {
			name       string
			parentPref string
			hcToken    string
			wantPref   string
		}{
			{name: "installation_parent_overrides_user_preferred", parentPref: DispatchAgentGitHubTokenInstallation, hcToken: DispatchAgentGitHubTokenUserPreferred, wantPref: DispatchAgentGitHubTokenInstallation},
			{name: "user_preferred_parent_keeps_hc_token", parentPref: DispatchAgentGitHubTokenUserPreferred, hcToken: DispatchAgentGitHubTokenUserPreferred, wantPref: DispatchAgentGitHubTokenUserPreferred},
			{name: "empty_parent_keeps_empty", parentPref: "", hcToken: "", wantPref: ""},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				f, parent, _, _ := setupParent(t, "hl-mat-"+tc.name[:8], tc.parentPref)
				hcChild, err := f.Server.createAgentHeadless(ctx, headlessReq(f, parent, "hl-child-ok", tc.hcToken))
				require.NoError(t, err)
				require.NotNil(t, hcChild)
				require.NotNil(t, hcChild.AppliedConfig)
				assert.Equal(t, tc.wantPref, hcChild.AppliedConfig.GitHubTokenPreference)
			})
		}
	})
}

func TestGitHubTokenInstallation_EnvAndSecretMetadataDoorsGuarded(t *testing.T) {
	ctx := t.Context()

	setupFixture := func(t *testing.T, name, pref string) (*materialFixture, *githubTokenInheritFaultStore) {
		t.Helper()
		f := newMaterialFixture(t, name)
		f.recordAgentEdge(t)
		f.reissueToken(t, []AgentTokenScope{ScopeProjectRead, ScopeProjectSecretRead}, []string{f.UserID})

		ag, err := f.Store.GetAgent(ctx, f.AgentID)
		require.NoError(t, err)
		if ag.AppliedConfig == nil {
			ag.AppliedConfig = &store.AgentAppliedConfig{}
		}
		ag.AppliedConfig.GitHubTokenPreference = pref
		require.NoError(t, f.Store.UpdateAgent(ctx, ag))

		// Project plain env vars (blocked + allowed).
		require.NoError(t, f.Store.CreateEnvVar(ctx, &store.EnvVar{
			ID: api.NewUUID(), Key: "GITHUB_TOKEN", Value: "ghp_plain_proj_pat",
			Scope: store.ScopeProject, ScopeID: f.ProjectID, InjectionMode: "always",
			Created: time.Now(), Updated: time.Now(),
		}))
		require.NoError(t, f.Store.CreateEnvVar(ctx, &store.EnvVar{
			ID: api.NewUUID(), Key: "GH_TOKEN", Value: "ghp_plain_proj_gh",
			Scope: store.ScopeProject, ScopeID: f.ProjectID, InjectionMode: "always",
			Created: time.Now(), Updated: time.Now(),
		}))
		require.NoError(t, f.Store.CreateEnvVar(ctx, &store.EnvVar{
			ID: api.NewUUID(), Key: "ALLOWED_PROJ_ENV", Value: "allowed-proj-env-val",
			Scope: store.ScopeProject, ScopeID: f.ProjectID, InjectionMode: "always",
			Created: time.Now(), Updated: time.Now(),
		}))

		// Hub plain env vars (blocked + allowed).
		require.NoError(t, f.Store.CreateEnvVar(ctx, &store.EnvVar{
			ID: api.NewUUID(), Key: "GITHUB_TOKEN", Value: "ghp_plain_hub_pat",
			Scope: store.ScopeHub, ScopeID: f.Server.hubID, InjectionMode: "always",
			Created: time.Now(), Updated: time.Now(),
		}))
		require.NoError(t, f.Store.CreateEnvVar(ctx, &store.EnvVar{
			ID: api.NewUUID(), Key: "ALLOWED_HUB_ENV", Value: "allowed-hub-env-val",
			Scope: store.ScopeHub, ScopeID: f.Server.hubID, InjectionMode: "always",
			Created: time.Now(), Updated: time.Now(),
		}))

		// Project secrets (direct blocked, aliased Target=GITHUB_TOKEN, allowed).
		for _, in := range []*secret.SetSecretInput{
			{Name: "GITHUB_TOKEN", Value: "ghp_sec_proj_pat", SecretType: store.SecretTypeEnvironment, Target: "GITHUB_TOKEN", Scope: store.ScopeProject, ScopeID: f.ProjectID, CreatedBy: f.UserID, UpdatedBy: f.UserID},
			{Name: "MY_ENV_PAT", Value: "ghp_sec_proj_aliased", SecretType: store.SecretTypeEnvironment, Target: "GITHUB_TOKEN", Scope: store.ScopeProject, ScopeID: f.ProjectID, CreatedBy: f.UserID, UpdatedBy: f.UserID},
			{Name: "ALLOWED_PROJ_SECRET", Value: "allowed-proj-sec", SecretType: store.SecretTypeEnvironment, Target: "ALLOWED_PROJ_SECRET", Scope: store.ScopeProject, ScopeID: f.ProjectID, CreatedBy: f.UserID, UpdatedBy: f.UserID},
			{Name: "GITHUB_TOKEN", Value: "ghp_sec_hub_pat", SecretType: store.SecretTypeEnvironment, Target: "GITHUB_TOKEN", Scope: store.ScopeHub, ScopeID: f.Server.hubID, InjectionMode: store.InjectionModeAlways, CreatedBy: f.UserID, UpdatedBy: f.UserID},
			{Name: "ALLOWED_HUB_SECRET", Value: "allowed-hub-sec", SecretType: store.SecretTypeEnvironment, Target: "ALLOWED_HUB_SECRET", Scope: store.ScopeHub, ScopeID: f.Server.hubID, InjectionMode: store.InjectionModeAlways, CreatedBy: f.UserID, UpdatedBy: f.UserID},
		} {
			_, _, err := f.Server.secretBackend.Set(ctx, in)
			require.NoError(t, err)
		}

		fs, _ := installStoreFault(t, f.Server, func(inner store.Store, sw *storeFaultSwitch) *githubTokenInheritFaultStore {
			return &githubTokenInheritFaultStore{Store: inner, fault: sw}
		})
		return f, fs
	}

	t.Run("installation_agent_blocked_across_all_env_and_secret_metadata_doors", func(t *testing.T) {
		f, fault := setupFixture(t, "inst-env-doors", DispatchAgentGitHubTokenInstallation)

		// 1. Project env by key (nested + flat) -> 404 for blocked keys & aliased env secret fallback; 200 for allowed.
		for _, path := range []string{
			fmt.Sprintf("/api/v1/projects/%s/env/GITHUB_TOKEN", f.ProjectID),
			fmt.Sprintf("/api/v1/projects/%s/env/GH_TOKEN", f.ProjectID),
			fmt.Sprintf("/api/v1/projects/%s/env/MY_ENV_PAT", f.ProjectID),
			fmt.Sprintf("/api/v1/env/GITHUB_TOKEN?scope=project&scopeId=%s", f.ProjectID),
			fmt.Sprintf("/api/v1/env/GH_TOKEN?scope=project&scopeId=%s", f.ProjectID),
			fmt.Sprintf("/api/v1/env/MY_ENV_PAT?scope=project&scopeId=%s", f.ProjectID),
			"/api/v1/env/GITHUB_TOKEN?scope=hub",
		} {
			rec := doRequestWithAgentToken(t, f.Server, http.MethodGet, path, nil, f.Token)
			assert.Equal(t, http.StatusNotFound, rec.Code, "path=%s body=%s", path, rec.Body.String())
			assert.NotContains(t, rec.Body.String(), "ghp_plain_")
		}

		recProjEnvOK := doRequestWithAgentToken(t, f.Server, http.MethodGet,
			fmt.Sprintf("/api/v1/projects/%s/env/ALLOWED_PROJ_ENV", f.ProjectID), nil, f.Token)
		assert.Equal(t, http.StatusOK, recProjEnvOK.Code, recProjEnvOK.Body.String())
		assert.Contains(t, recProjEnvOK.Body.String(), "allowed-proj-env-val")

		recHubEnvOK := doRequestWithAgentToken(t, f.Server, http.MethodGet,
			"/api/v1/env/ALLOWED_HUB_ENV?scope=hub", nil, f.Token)
		assert.Equal(t, http.StatusOK, recHubEnvOK.Code, recHubEnvOK.Body.String())
		assert.Contains(t, recHubEnvOK.Body.String(), "allowed-hub-env-val")

		// 2. Env lists (project nested, project flat, hub flat) omit blocked keys and aliased env secrets; keep allowed.
		for _, tc := range []struct {
			path        string
			wantAllowed string
		}{
			{fmt.Sprintf("/api/v1/projects/%s/env", f.ProjectID), "ALLOWED_PROJ_ENV"},
			{fmt.Sprintf("/api/v1/env?scope=project&scopeId=%s", f.ProjectID), "ALLOWED_PROJ_ENV"},
			{"/api/v1/env?scope=hub", "ALLOWED_HUB_ENV"},
		} {
			rec := doRequestWithAgentToken(t, f.Server, http.MethodGet, tc.path, nil, f.Token)
			require.Equal(t, http.StatusOK, rec.Code, "path=%s body=%s", tc.path, rec.Body.String())
			assert.NotContains(t, rec.Body.String(), "ghp_plain_")
			var resp ListEnvVarsResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			foundAllowed := false
			for _, ev := range resp.EnvVars {
				assert.False(t, isBlockedInstallationGitHubEnvKey(ev.Key), "path=%s leaked key %q", tc.path, ev.Key)
				assert.NotEqual(t, "MY_ENV_PAT", ev.Key, "path=%s leaked aliased env secret", tc.path)
				if ev.Key == tc.wantAllowed {
					foundAllowed = true
				}
			}
			assert.True(t, foundAllowed, "path=%s missing allowed key %s", tc.path, tc.wantAllowed)
		}

		// 3. Secret metadata by key (project nested, project flat, hub flat) -> 404 for direct & aliased blocked keys.
		for _, path := range []string{
			fmt.Sprintf("/api/v1/projects/%s/secrets/GITHUB_TOKEN", f.ProjectID),
			fmt.Sprintf("/api/v1/projects/%s/secrets/MY_ENV_PAT", f.ProjectID),
			fmt.Sprintf("/api/v1/secrets/GITHUB_TOKEN?scope=project&scopeId=%s", f.ProjectID),
			fmt.Sprintf("/api/v1/secrets/MY_ENV_PAT?scope=project&scopeId=%s", f.ProjectID),
			"/api/v1/secrets/GITHUB_TOKEN?scope=hub",
		} {
			rec := doRequestWithAgentToken(t, f.Server, http.MethodGet, path, nil, f.Token)
			assert.Equal(t, http.StatusNotFound, rec.Code, "path=%s body=%s", path, rec.Body.String())
		}

		recProjSecOK := doRequestWithAgentToken(t, f.Server, http.MethodGet,
			fmt.Sprintf("/api/v1/projects/%s/secrets/ALLOWED_PROJ_SECRET", f.ProjectID), nil, f.Token)
		assert.Equal(t, http.StatusOK, recProjSecOK.Code, recProjSecOK.Body.String())

		// 4. Secret metadata lists (project nested, project flat, hub flat) omit direct & aliased blocked keys.
		for _, tc := range []struct {
			path        string
			wantAllowed string
		}{
			{fmt.Sprintf("/api/v1/projects/%s/secrets", f.ProjectID), "ALLOWED_PROJ_SECRET"},
			{fmt.Sprintf("/api/v1/secrets?scope=project&scopeId=%s", f.ProjectID), "ALLOWED_PROJ_SECRET"},
			{"/api/v1/secrets?scope=hub", "ALLOWED_HUB_SECRET"},
		} {
			rec := doRequestWithAgentToken(t, f.Server, http.MethodGet, tc.path, nil, f.Token)
			require.Equal(t, http.StatusOK, rec.Code, "path=%s body=%s", tc.path, rec.Body.String())
			var resp ListSecretsResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			foundAllowed := false
			for _, s := range resp.Secrets {
				assert.False(t, isBlockedInstallationGitHubEnvKey(s.Key), "path=%s leaked secret %q", tc.path, s.Key)
				assert.NotEqual(t, "MY_ENV_PAT", s.Key, "path=%s leaked aliased secret", tc.path)
				if s.Key == tc.wantAllowed {
					foundAllowed = true
				}
			}
			assert.True(t, foundAllowed, "path=%s missing allowed secret %s", tc.path, tc.wantAllowed)
		}

		// 5. Fail-closed on transient caller lookup error -> HTTP 503 ErrCodeUnavailable, no leak.
		fault.Arm()
		recFault := doRequestWithAgentToken(t, f.Server, http.MethodGet,
			fmt.Sprintf("/api/v1/projects/%s/env/GITHUB_TOKEN", f.ProjectID), nil, f.Token)
		fault.Disarm()
		assert.Equal(t, http.StatusServiceUnavailable, recFault.Code, recFault.Body.String())
		assert.Contains(t, recFault.Body.String(), ErrCodeUnavailable)
		assert.NotContains(t, recFault.Body.String(), "ghp_plain_proj_pat")

		// 6. Deleted caller (store.ErrNotFound) -> HTTP 403 without retry advice.
		require.NoError(t, f.Store.DeleteAgent(ctx, f.AgentID))
		recDeleted := doRequestWithAgentToken(t, f.Server, http.MethodGet,
			fmt.Sprintf("/api/v1/projects/%s/env/GITHUB_TOKEN", f.ProjectID), nil, f.Token)
		assert.Equal(t, http.StatusForbidden, recDeleted.Code, recDeleted.Body.String())
		assert.NotContains(t, recDeleted.Body.String(), "retry")
		assert.NotContains(t, recDeleted.Body.String(), "ghp_plain_proj_pat")
	})

	t.Run("user_preferred_agent_unchanged", func(t *testing.T) {
		f, _ := setupFixture(t, "up-env-doors", DispatchAgentGitHubTokenUserPreferred)

		recA := doRequestWithAgentToken(t, f.Server, http.MethodGet,
			fmt.Sprintf("/api/v1/projects/%s/env/GH_TOKEN", f.ProjectID), nil, f.Token)
		assert.Equal(t, http.StatusOK, recA.Code, recA.Body.String())
		assert.Contains(t, recA.Body.String(), "ghp_plain_proj_gh")

		recHub := doRequestWithAgentToken(t, f.Server, http.MethodGet,
			"/api/v1/env/GITHUB_TOKEN?scope=hub", nil, f.Token)
		assert.Equal(t, http.StatusOK, recHub.Code, recHub.Body.String())
		assert.Contains(t, recHub.Body.String(), "ghp_plain_hub_pat")

		recSec := doRequestWithAgentToken(t, f.Server, http.MethodGet,
			fmt.Sprintf("/api/v1/projects/%s/secrets/GITHUB_TOKEN", f.ProjectID), nil, f.Token)
		assert.Equal(t, http.StatusOK, recSec.Code, recSec.Body.String())
	})
}
