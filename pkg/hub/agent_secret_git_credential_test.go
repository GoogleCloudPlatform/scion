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
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// gitCredentialSecretFixture is a materialFixture whose agent is created with
// the given applied config (nil: no applied config at all).
func gitCredentialSecretFixture(t *testing.T, name string, applied *store.AgentAppliedConfig) *materialFixture {
	t.Helper()
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))
	ctx := context.Background()

	projectID := tid("project-" + name)
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: projectID, Name: name, Slug: "proj-" + name, Created: time.Now(), Updated: time.Now(),
	}))
	userID := tid("user-" + name)
	createDCUser(t, s, userID, name+"@test.com", projectID, store.ProjectRoleOwner)

	agentID := tid("agent-" + name)
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: agentID, Slug: "agent-" + name, Name: "Agent " + name,
		ProjectID: projectID, Phase: string(state.PhaseRunning), StateVersion: 1,
		Ancestry:      []string{userID},
		AppliedConfig: applied,
		Created:       time.Now(), Updated: time.Now(),
	}))
	token, err := srv.agentTokenService.GenerateAgentToken(agentID, projectID, []AgentTokenScope{ScopeProjectSecretRead}, []string{userID})
	require.NoError(t, err)

	return &materialFixture{Server: srv, Store: s, ProjectID: projectID, UserID: userID, AgentID: agentID, Token: token}
}

// TestAgentSecretRead_GitCredentialKeys_RequireAllowance pins that the agent
// secret fetch (POST /api/v1/agent/secrets) and get
// (GET /api/v1/agents/{id}/secrets/{key}) endpoints return a GitHub
// credential key (GITHUB_TOKEN, GH_* in any case) only to an agent whose
// applied config allows GitHub credentials. An unset allowance and a missing
// applied config are both refused. Other keys are unaffected either way.
func TestAgentSecretRead_GitCredentialKeys_RequireAllowance(t *testing.T) {
	cases := []struct {
		name    string
		applied *store.AgentAppliedConfig
		allowed bool
	}{
		{name: "unset", applied: &store.AgentAppliedConfig{}, allowed: false},
		{name: "nil-applied-config", applied: nil, allowed: false},
		{name: "allowed", applied: &store.AgentAppliedConfig{AllowGitCredentials: true}, allowed: true},
	}
	credKeys := []string{"GITHUB_TOKEN", "gh_org", "GH_TOKEN"}
	const otherKey = "OTHER_SETTING"

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := gitCredentialSecretFixture(t, "git-cred-read-"+tc.name, tc.applied)
			ctx := context.Background()
			for _, k := range append(append([]string{}, credKeys...), otherKey) {
				seedSecret(t, f.Server.secretBackend, k, "fake-"+k, "", "", f.ProjectID)
			}
			_, _, err := f.Server.secretBackend.Set(ctx, &secret.SetSecretInput{
				Name: "GH_USER_CRED", Value: "fake-user-cred", SecretType: store.SecretTypeEnvironment, Target: "GH_USER_CRED",
				Scope: store.ScopeUser, ScopeID: f.UserID, AllowProgeny: true, CreatedBy: f.UserID, UpdatedBy: f.UserID,
			})
			require.NoError(t, err)

			// Fetch: every credential key plus one unrelated key.
			rec := doRequestWithAgentToken(t, f.Server, http.MethodPost, "/api/v1/agent/secrets",
				secretFetchRequest{Keys: append(append([]string{}, credKeys...), otherKey)}, f.Token)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var resp secretFetchResponse
			require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
			got := map[string]secretFetchResult{}
			for _, r := range resp.Secrets {
				got[r.Key] = r
			}
			for _, k := range credKeys {
				r := got[k]
				if tc.allowed {
					require.Equal(t, "ok", r.Status, "fetch %s", k)
					require.Equal(t, "fake-"+k, r.Value, "fetch %s", k)
				} else {
					require.Equal(t, secretFetchResult{Key: k, Status: "not_found", Error: "secret not found"}, r, "fetch %s", k)
				}
			}
			require.Equal(t, "ok", got[otherKey].Status, "unrelated key")
			require.Equal(t, "fake-"+otherKey, got[otherKey].Value, "unrelated key")

			// Get, project scope.
			for _, k := range []string{"GITHUB_TOKEN", "gh_org"} {
				getRec := doRequestWithAgentToken(t, f.Server, http.MethodGet, "/api/v1/agents/"+f.AgentID+"/secrets/"+k, nil, f.Token)
				if tc.allowed {
					require.Equal(t, http.StatusOK, getRec.Code, "get %s: %s", k, getRec.Body.String())
				} else {
					require.Equal(t, http.StatusNotFound, getRec.Code, "get %s: %s", k, getRec.Body.String())
				}
			}
			getRec := doRequestWithAgentToken(t, f.Server, http.MethodGet, "/api/v1/agents/"+f.AgentID+"/secrets/"+otherKey, nil, f.Token)
			require.Equal(t, http.StatusOK, getRec.Code, "get unrelated key: %s", getRec.Body.String())

			// Get, user scope.
			userRec := doRequestWithAgentToken(t, f.Server, http.MethodGet, "/api/v1/agents/"+f.AgentID+"/secrets/GH_USER_CRED?scope=user", nil, f.Token)
			if tc.allowed {
				require.Equal(t, http.StatusOK, userRec.Code, userRec.Body.String())
			} else {
				require.Equal(t, http.StatusNotFound, userRec.Code, userRec.Body.String())
			}
		})
	}
}

// TestAgentSecretRead_GitCredentialKeys_RefusedBeforeBackendRead pins that a
// refused GitHub credential key is decided from the key name and the
// allowance alone, on both scopes: no secret metadata or value is read, and
// the answer is byte-identical for a key that exists in the requested scope,
// one that exists only in the other scope, and one that exists nowhere. The
// audit items record the reason.
func TestAgentSecretRead_GitCredentialKeys_RefusedBeforeBackendRead(t *testing.T) {
	f := gitCredentialSecretFixture(t, "git-cred-no-read", &store.AgentAppliedConfig{})
	ctx := context.Background()
	// GITHUB_TOKEN exists in project scope only; GH_USER_CRED in user scope
	// only; GH_ABSENT nowhere.
	seedSecret(t, f.Server.secretBackend, "GITHUB_TOKEN", "fake-token", "", "", f.ProjectID)
	_, _, err := f.Server.secretBackend.Set(ctx, &secret.SetSecretInput{
		Name: "GH_USER_CRED", Value: "fake-user-cred", SecretType: store.SecretTypeEnvironment, Target: "GH_USER_CRED",
		Scope: store.ScopeUser, ScopeID: f.UserID, AllowProgeny: true, CreatedBy: f.UserID, UpdatedBy: f.UserID,
	})
	require.NoError(t, err)

	counting := &countingSecretBackend{SecretBackend: f.Server.secretBackend}
	f.Server.SetSecretBackend(counting)
	audit := newRecordingMaterialAuditor()
	f.Server.SetAuditLogger(audit)

	keys := []string{"GITHUB_TOKEN", "GH_USER_CRED", "GH_ABSENT"}

	// Fetch (project scope): every key is the plain not_found result.
	rec := doRequestWithAgentToken(t, f.Server, http.MethodPost, "/api/v1/agent/secrets",
		secretFetchRequest{Keys: keys}, f.Token)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp secretFetchResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	want := make([]secretFetchResult, 0, len(keys))
	for _, k := range keys {
		want = append(want, secretFetchResult{Key: k, Status: "not_found", Error: "secret not found"})
	}
	require.Equal(t, want, resp.Secrets)

	// Get, both scopes: the same 404 body for every key.
	for _, scope := range []string{"project", "user"} {
		var firstBody string
		for i, k := range keys {
			getRec := doRequestWithAgentToken(t, f.Server, http.MethodGet, "/api/v1/agents/"+f.AgentID+"/secrets/"+k+"?scope="+scope, nil, f.Token)
			require.Equal(t, http.StatusNotFound, getRec.Code, "get %s scope=%s: %s", k, scope, getRec.Body.String())
			if i == 0 {
				firstBody = getRec.Body.String()
				continue
			}
			require.Equal(t, firstBody, getRec.Body.String(), "get %s scope=%s body", k, scope)
		}
	}

	require.Zero(t, counting.getMetaCalls, "no metadata read for a refused key")
	require.Zero(t, counting.getCalls, "no value read for a refused key")

	require.Len(t, audit.events, 1+2*len(keys))
	for _, e := range audit.events {
		for _, item := range e.Items {
			require.False(t, item.Allowed)
			require.False(t, item.Selected)
			require.Equal(t, ReasonGitCredentialNotAllowed, item.Reason, "key %s", item.Key)
		}
	}
}

// TestAgentListSecrets_NamesOnly pins that the agent secret list endpoint,
// which is not value-gated, carries no secret value for any key, including
// a GitHub credential key, for an agent without the allowance.
func TestAgentListSecrets_NamesOnly(t *testing.T) {
	f := gitCredentialSecretFixture(t, "git-cred-list", &store.AgentAppliedConfig{})
	seedSecret(t, f.Server.secretBackend, "GITHUB_TOKEN", "fake-token-value", "", "", f.ProjectID)

	rec := doRequestWithAgentToken(t, f.Server, http.MethodGet, "/api/v1/agents/"+f.AgentID+"/secrets", nil, f.Token)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NotContains(t, rec.Body.String(), "fake-token-value")
	var raw struct {
		Secrets []map[string]any `json:"secrets"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	for _, item := range raw.Secrets {
		require.NotContains(t, item, "value", "list item %v", item)
	}
}

// TestGitCredentialKeyDenied pins the per-key decision: the same matcher as
// the broker env strip (case-insensitive) and the same predicate as the
// token refresh gate, refusing when the agent record or applied config is
// missing.
func TestGitCredentialKeyDenied(t *testing.T) {
	allowed := &TargetFacts{Agent: &store.Agent{AppliedConfig: &store.AgentAppliedConfig{AllowGitCredentials: true}}}
	unset := &TargetFacts{Agent: &store.Agent{AppliedConfig: &store.AgentAppliedConfig{}}}
	noConfig := &TargetFacts{Agent: &store.Agent{}}
	noAgent := &TargetFacts{}

	for _, key := range []string{"GITHUB_TOKEN", "github_token", "GH_TOKEN", "gh_org", "Gh_Owner__Repo"} {
		require.False(t, gitCredentialKeyDenied(allowed, key), "allowed %s", key)
		for name, facts := range map[string]*TargetFacts{"unset": unset, "no-config": noConfig, "no-agent": noAgent, "nil-facts": nil} {
			require.True(t, gitCredentialKeyDenied(facts, key), "%s %s", name, key)
		}
	}
	for _, key := range []string{"OTHER_SETTING", "MY_GITHUB_TOKEN", "GITHUB_TOKEN_X", "COPILOT_GITHUB_TOKEN"} {
		for name, facts := range map[string]*TargetFacts{"allowed": allowed, "unset": unset, "no-config": noConfig, "no-agent": noAgent, "nil-facts": nil} {
			require.False(t, gitCredentialKeyDenied(facts, key), "%s %s", name, key)
		}
	}
}

// TestSelectRuntimeMaterial_NilFactsRefused pins that selectRuntimeMaterial
// refuses every key, on both scopes, when it has no target facts, without
// reading secret metadata or values.
func TestSelectRuntimeMaterial_NilFactsRefused(t *testing.T) {
	f := gitCredentialSecretFixture(t, "nil-facts", &store.AgentAppliedConfig{AllowGitCredentials: true})
	seedSecret(t, f.Server.secretBackend, "GITHUB_TOKEN", "fake-token", "", "", f.ProjectID)
	seedSecret(t, f.Server.secretBackend, "OTHER_SETTING", "fake-other", "", "", f.ProjectID)
	counting := &countingSecretBackend{SecretBackend: f.Server.secretBackend}
	f.Server.SetSecretBackend(counting)

	for _, scope := range []string{store.ScopeProject, store.ScopeUser} {
		for _, key := range []string{"GITHUB_TOKEN", "OTHER_SETTING"} {
			var cache projectDecisionCache
			item, sv, _, _ := f.Server.selectRuntimeMaterial(context.Background(), nil, nil, scope, key, &cache)
			require.False(t, item.Allowed, "%s %s", scope, key)
			require.False(t, item.Selected, "%s %s", scope, key)
			require.Nil(t, sv, "%s %s", scope, key)
			require.Equal(t, ReasonTargetUnresolved, item.Reason, "%s %s", scope, key)
		}
	}
	require.Zero(t, counting.getMetaCalls)
	require.Zero(t, counting.getCalls)
}
