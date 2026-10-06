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
// one that exists only in the other scope, one that exists nowhere, and an
// ungated key that does not exist. The audit items record the reason.
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

	// Get, both scopes: the same 404 body for every refused key.
	refusedBody := map[string]string{}
	for _, scope := range []string{"project", "user"} {
		for i, k := range keys {
			getRec := doRequestWithAgentToken(t, f.Server, http.MethodGet, "/api/v1/agents/"+f.AgentID+"/secrets/"+k+"?scope="+scope, nil, f.Token)
			require.Equal(t, http.StatusNotFound, getRec.Code, "get %s scope=%s: %s", k, scope, getRec.Body.String())
			if i == 0 {
				refusedBody[scope] = getRec.Body.String()
				continue
			}
			require.Equal(t, refusedBody[scope], getRec.Body.String(), "get %s scope=%s body", k, scope)
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

	// A refused key is also indistinguishable from a key that is not a
	// GitHub credential and simply does not exist: same 404 body on both
	// scopes. (This request does read metadata, so it comes after the
	// zero-read assertions above.)
	for _, scope := range []string{"project", "user"} {
		getRec := doRequestWithAgentToken(t, f.Server, http.MethodGet, "/api/v1/agents/"+f.AgentID+"/secrets/PLAIN_ABSENT?scope="+scope, nil, f.Token)
		require.Equal(t, http.StatusNotFound, getRec.Code, "get PLAIN_ABSENT scope=%s: %s", scope, getRec.Body.String())
		require.Equal(t, refusedBody[scope], getRec.Body.String(), "get PLAIN_ABSENT scope=%s body", scope)
	}
}

// TestAgentListSecrets_NamesOnly pins that the agent secret list endpoint
// carries no secret value for any key.
func TestAgentListSecrets_NamesOnly(t *testing.T) {
	f := gitCredentialSecretFixture(t, "git-cred-list", &store.AgentAppliedConfig{AllowGitCredentials: true})
	seedSecret(t, f.Server.secretBackend, "GITHUB_TOKEN", "fake-token-value", "", "", f.ProjectID)

	rec := doRequestWithAgentToken(t, f.Server, http.MethodGet, "/api/v1/agents/"+f.AgentID+"/secrets", nil, f.Token)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NotContains(t, rec.Body.String(), "fake-token-value")
	var raw struct {
		Secrets []map[string]any `json:"secrets"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	require.NotEmpty(t, raw.Secrets)
	for _, item := range raw.Secrets {
		require.NotContains(t, item, "value", "list item %v", item)
	}
}

// TestAgentListSecrets_GitCredentialKeys_RequireAllowance pins that the list
// leaves out GitHub credential keys (any case, project and user scope) for an
// agent without the allowance, so its response is byte-identical whether or
// not such keys exist, and that an allowed agent still sees them.
func TestAgentListSecrets_GitCredentialKeys_RequireAllowance(t *testing.T) {
	seedCreds := func(t *testing.T, f *materialFixture) {
		t.Helper()
		seedSecret(t, f.Server.secretBackend, "GITHUB_TOKEN", "fake-token", "", "", f.ProjectID)
		seedSecret(t, f.Server.secretBackend, "gh_org", "fake-org", "", "", f.ProjectID)
		_, _, err := f.Server.secretBackend.Set(context.Background(), &secret.SetSecretInput{
			Name: "GH_USER_CRED", Value: "fake-user-cred", SecretType: store.SecretTypeEnvironment, Target: "GH_USER_CRED",
			Scope: store.ScopeUser, ScopeID: f.UserID, AllowProgeny: true, CreatedBy: f.UserID, UpdatedBy: f.UserID,
		})
		require.NoError(t, err)
	}
	seedPlain := func(t *testing.T, f *materialFixture) {
		t.Helper()
		seedSecret(t, f.Server.secretBackend, "OTHER_SETTING", "fake-other", "", "", f.ProjectID)
		_, _, err := f.Server.secretBackend.Set(context.Background(), &secret.SetSecretInput{
			Name: "USER_SETTING", Value: "fake-user-setting", SecretType: store.SecretTypeEnvironment, Target: "USER_SETTING",
			Scope: store.ScopeUser, ScopeID: f.UserID, AllowProgeny: true, CreatedBy: f.UserID, UpdatedBy: f.UserID,
		})
		require.NoError(t, err)
	}
	list := func(t *testing.T, f *materialFixture, scope string) string {
		t.Helper()
		path := "/api/v1/agents/" + f.AgentID + "/secrets"
		if scope != "" {
			path += "?scope=" + scope
		}
		rec := doRequestWithAgentToken(t, f.Server, http.MethodGet, path, nil, f.Token)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		return rec.Body.String()
	}

	// List items carry only key, type and target, so two fixtures seeded
	// with the same keys give byte-identical bodies.
	for _, applied := range map[string]*store.AgentAppliedConfig{"unset": {}, "nil-applied-config": nil} {
		without := gitCredentialSecretFixture(t, "git-cred-list-hide", applied)
		seedPlain(t, without)
		with := gitCredentialSecretFixture(t, "git-cred-list-hide", applied)
		seedPlain(t, with)
		seedCreds(t, with)
		for _, scope := range []string{"", "project", "user"} {
			got := list(t, with, scope)
			require.Equal(t, list(t, without, scope), got, "scope=%q: list must not change when refused keys exist", scope)
			for _, k := range []string{"GITHUB_TOKEN", "gh_org", "GH_USER_CRED"} {
				require.NotContains(t, got, k, "scope=%q", scope)
			}
		}
		require.Contains(t, list(t, with, ""), "OTHER_SETTING")
		require.Contains(t, list(t, with, ""), "USER_SETTING")
	}

	allowed := gitCredentialSecretFixture(t, "git-cred-list-allowed", &store.AgentAppliedConfig{AllowGitCredentials: true})
	seedPlain(t, allowed)
	seedCreds(t, allowed)
	got := list(t, allowed, "")
	for _, k := range []string{"GITHUB_TOKEN", "gh_org", "GH_USER_CRED", "OTHER_SETTING", "USER_SETTING"} {
		require.Contains(t, got, k)
	}
}

// getRecordingBackend records the names passed to Get (value reads).
type getRecordingBackend struct {
	secret.SecretBackend
	gets []string
}

func (g *getRecordingBackend) Get(ctx context.Context, name, scope, scopeID string) (*secret.SecretWithValue, error) {
	g.gets = append(g.gets, name)
	return g.SecretBackend.Get(ctx, name, scope, scopeID)
}

// seedTargetCredSecrets seeds secrets whose names do not match the GitHub
// credential pattern but whose targets do (project environment, project
// variable, user environment), plus a file secret and a plain secret that
// are not affected.
func seedTargetCredSecrets(t *testing.T, f *materialFixture) {
	t.Helper()
	seedSecret(t, f.Server.secretBackend, "deploy-pat", "fake-deploy", store.SecretTypeEnvironment, "GITHUB_TOKEN", f.ProjectID)
	seedSecret(t, f.Server.secretBackend, "tool-cfg", "fake-tool", store.SecretTypeVariable, "gh_org", f.ProjectID)
	_, _, err := f.Server.secretBackend.Set(context.Background(), &secret.SetSecretInput{
		Name: "user-pat", Value: "fake-user-pat", SecretType: store.SecretTypeEnvironment, Target: "GH_TOKEN",
		Scope: store.ScopeUser, ScopeID: f.UserID, AllowProgeny: true, CreatedBy: f.UserID, UpdatedBy: f.UserID,
	})
	require.NoError(t, err)
}

func seedTargetPlainSecrets(t *testing.T, f *materialFixture) {
	t.Helper()
	seedSecret(t, f.Server.secretBackend, "cfg-file", "fake-file", store.SecretTypeFile, "/home/scion/.config/tool.conf", f.ProjectID)
	seedSecret(t, f.Server.secretBackend, "plain-setting", "fake-plain", store.SecretTypeEnvironment, "PLAIN_SETTING", f.ProjectID)
}

// TestAgentSecretRead_GitCredentialTarget_RequireAllowance pins that a
// secret whose name does not match but whose target names a GitHub
// credential (the env name the broker filter uses) is refused on fetch and
// get to an agent without the allowance, with the same answer as a missing
// key, after the metadata read and without a value read. File-type and
// plain secrets are unaffected, and an allowed agent reads them all.
func TestAgentSecretRead_GitCredentialTarget_RequireAllowance(t *testing.T) {
	for _, tc := range []struct {
		name    string
		applied *store.AgentAppliedConfig
	}{
		{"unset", &store.AgentAppliedConfig{}},
		{"nil-applied-config", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := gitCredentialSecretFixture(t, "git-cred-target-"+tc.name, tc.applied)
			seedTargetCredSecrets(t, f)
			seedTargetPlainSecrets(t, f)
			rec := &getRecordingBackend{SecretBackend: f.Server.secretBackend}
			f.Server.SetSecretBackend(rec)
			audit := newRecordingMaterialAuditor()
			f.Server.SetAuditLogger(audit)

			fetch := doRequestWithAgentToken(t, f.Server, http.MethodPost, "/api/v1/agent/secrets",
				secretFetchRequest{Keys: []string{"deploy-pat", "tool-cfg", "PLAIN_ABSENT", "cfg-file", "plain-setting"}}, f.Token)
			require.Equal(t, http.StatusOK, fetch.Code, fetch.Body.String())
			var resp secretFetchResponse
			require.NoError(t, json.NewDecoder(fetch.Body).Decode(&resp))
			require.Equal(t, []secretFetchResult{
				{Key: "deploy-pat", Status: "not_found", Error: "secret not found"},
				{Key: "tool-cfg", Status: "not_found", Error: "secret not found"},
				{Key: "PLAIN_ABSENT", Status: "not_found", Error: "secret not found"},
				{Key: "cfg-file", Value: "fake-file", Status: "ok"},
				{Key: "plain-setting", Value: "fake-plain", Status: "ok"},
			}, resp.Secrets)

			get := func(key, scope string) (int, string) {
				r := doRequestWithAgentToken(t, f.Server, http.MethodGet, "/api/v1/agents/"+f.AgentID+"/secrets/"+key+"?scope="+scope, nil, f.Token)
				return r.Code, r.Body.String()
			}
			absentCode, absentBody := get("PLAIN_ABSENT", "project")
			require.Equal(t, http.StatusNotFound, absentCode)
			for _, k := range []string{"deploy-pat", "tool-cfg"} {
				code, body := get(k, "project")
				require.Equal(t, http.StatusNotFound, code, "get %s", k)
				require.Equal(t, absentBody, body, "get %s body", k)
			}
			userAbsentCode, userAbsentBody := get("PLAIN_ABSENT", "user")
			require.Equal(t, http.StatusNotFound, userAbsentCode)
			code, body := get("user-pat", "user")
			require.Equal(t, http.StatusNotFound, code, "get user-pat")
			require.Equal(t, userAbsentBody, body, "get user-pat body")

			for _, k := range rec.gets {
				require.NotContains(t, []string{"deploy-pat", "tool-cfg", "user-pat"}, k, "value read for a refused secret")
			}
			refused := 0
			for _, e := range audit.events {
				for _, item := range e.Items {
					if item.Reason == ReasonGitCredentialNotAllowed {
						refused++
						require.False(t, item.Allowed, "key %s", item.Key)
					}
				}
			}
			require.Equal(t, 5, refused, "fetch 2 + get 3 refused items")
		})
	}

	allowed := gitCredentialSecretFixture(t, "git-cred-target-allowed", &store.AgentAppliedConfig{AllowGitCredentials: true})
	seedTargetCredSecrets(t, allowed)
	fetch := doRequestWithAgentToken(t, allowed.Server, http.MethodPost, "/api/v1/agent/secrets",
		secretFetchRequest{Keys: []string{"deploy-pat", "tool-cfg"}}, allowed.Token)
	require.Equal(t, http.StatusOK, fetch.Code, fetch.Body.String())
	var resp secretFetchResponse
	require.NoError(t, json.NewDecoder(fetch.Body).Decode(&resp))
	require.Equal(t, []secretFetchResult{
		{Key: "deploy-pat", Value: "fake-deploy", Status: "ok"},
		{Key: "tool-cfg", Value: "fake-tool", Status: "ok"},
	}, resp.Secrets)
	userGet := doRequestWithAgentToken(t, allowed.Server, http.MethodGet, "/api/v1/agents/"+allowed.AgentID+"/secrets/user-pat?scope=user", nil, allowed.Token)
	require.Equal(t, http.StatusOK, userGet.Code, userGet.Body.String())
}

// TestAgentListSecrets_GitCredentialTarget_RequireAllowance pins that the
// list leaves out secrets whose target names a GitHub credential for an
// agent without the allowance, so its body is byte-identical with and
// without them, and that an allowed agent still sees them.
func TestAgentListSecrets_GitCredentialTarget_RequireAllowance(t *testing.T) {
	list := func(t *testing.T, f *materialFixture, scope string) string {
		t.Helper()
		path := "/api/v1/agents/" + f.AgentID + "/secrets"
		if scope != "" {
			path += "?scope=" + scope
		}
		rec := doRequestWithAgentToken(t, f.Server, http.MethodGet, path, nil, f.Token)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		return rec.Body.String()
	}
	for name, applied := range map[string]*store.AgentAppliedConfig{"unset": {}, "nil-applied-config": nil} {
		without := gitCredentialSecretFixture(t, "git-cred-target-list-"+name, applied)
		seedTargetPlainSecrets(t, without)
		with := gitCredentialSecretFixture(t, "git-cred-target-list-"+name, applied)
		seedTargetPlainSecrets(t, with)
		seedTargetCredSecrets(t, with)
		for _, scope := range []string{"", "project", "user"} {
			got := list(t, with, scope)
			require.Equal(t, list(t, without, scope), got, "%s scope=%q", name, scope)
			for _, k := range []string{"deploy-pat", "tool-cfg", "user-pat"} {
				require.NotContains(t, got, k, "%s scope=%q", name, scope)
			}
		}
		require.Contains(t, list(t, with, ""), "cfg-file")
		require.Contains(t, list(t, with, ""), "plain-setting")
	}

	allowed := gitCredentialSecretFixture(t, "git-cred-target-list-allowed", &store.AgentAppliedConfig{AllowGitCredentials: true})
	seedTargetCredSecrets(t, allowed)
	got := list(t, allowed, "")
	for _, k := range []string{"deploy-pat", "tool-cfg", "user-pat"} {
		require.Contains(t, got, k)
	}
}

// TestAgentSecretRead_GitCredentialTarget_RecheckedOnReadRecord pins that
// the target rule is applied again to the record the value read returns: a
// record whose target names a GitHub credential is not returned to an agent
// without the allowance even if its metadata did not show that target.
func TestAgentSecretRead_GitCredentialTarget_RecheckedOnReadRecord(t *testing.T) {
	f := gitCredentialSecretFixture(t, "git-cred-target-recheck", &store.AgentAppliedConfig{})
	seedSecret(t, f.Server.secretBackend, "plain-setting", "fake-plain", store.SecretTypeEnvironment, "PLAIN_SETTING", f.ProjectID)
	race := &raceSecretBackend{SecretBackend: f.Server.secretBackend}
	race.overrideGet = func(sv *secret.SecretWithValue, err error) (*secret.SecretWithValue, error) {
		if sv != nil {
			changed := *sv
			changed.Target = "GITHUB_TOKEN"
			return &changed, err
		}
		return sv, err
	}
	f.Server.SetSecretBackend(race)

	fetch := doRequestWithAgentToken(t, f.Server, http.MethodPost, "/api/v1/agent/secrets",
		secretFetchRequest{Keys: []string{"plain-setting"}}, f.Token)
	require.Equal(t, http.StatusOK, fetch.Code, fetch.Body.String())
	var resp secretFetchResponse
	require.NoError(t, json.NewDecoder(fetch.Body).Decode(&resp))
	require.Equal(t, []secretFetchResult{{Key: "plain-setting", Status: "not_found", Error: "secret not found"}}, resp.Secrets)

	get := doRequestWithAgentToken(t, f.Server, http.MethodGet, "/api/v1/agents/"+f.AgentID+"/secrets/plain-setting", nil, f.Token)
	require.Equal(t, http.StatusNotFound, get.Code, get.Body.String())
	require.NotContains(t, get.Body.String(), "fake-plain")
}

// TestGitCredentialSecretDenied pins the metadata rule: the shared
// agent.IsGitCredentialSecret match plus the same allowance predicate.
func TestGitCredentialSecretDenied(t *testing.T) {
	allowed := &TargetFacts{Agent: &store.Agent{AppliedConfig: &store.AgentAppliedConfig{AllowGitCredentials: true}}}
	unset := &TargetFacts{Agent: &store.Agent{AppliedConfig: &store.AgentAppliedConfig{}}}
	cred := secret.SecretMeta{Name: "deploy-pat", SecretType: store.SecretTypeEnvironment, Target: "github_token"}
	file := secret.SecretMeta{Name: "cfg", SecretType: store.SecretTypeFile, Target: "GH_TOKEN"}
	plain := secret.SecretMeta{Name: "plain", SecretType: store.SecretTypeVariable, Target: "PLAIN"}

	require.False(t, gitCredentialSecretDenied(allowed, cred))
	for name, facts := range map[string]*TargetFacts{"unset": unset, "no-config": {Agent: &store.Agent{}}, "no-agent": {}, "nil-facts": nil} {
		require.True(t, gitCredentialSecretDenied(facts, cred), name)
		require.False(t, gitCredentialSecretDenied(facts, file), name)
		require.False(t, gitCredentialSecretDenied(facts, plain), name)
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
