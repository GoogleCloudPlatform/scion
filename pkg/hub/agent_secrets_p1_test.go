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
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type recordingSecretAuditLogger struct {
	*LogAuditLogger
	mu               sync.Mutex
	secretReadEvents []*AgentSecretReadEvent
	selectionEvents  []*MaterialSelectionEvent
}

func (l *recordingSecretAuditLogger) LogAgentSecretReadEvent(_ context.Context, e *AgentSecretReadEvent) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	cp := *e
	l.secretReadEvents = append(l.secretReadEvents, &cp)
	return nil
}

func (l *recordingSecretAuditLogger) LogMaterialSelectionEvent(_ context.Context, e *MaterialSelectionEvent) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	cp := *e
	l.selectionEvents = append(l.selectionEvents, &cp)
	return nil
}

func enableAgentSecretsExperimentForTest(t *testing.T, srv *Server, s store.Store) {
	t.Helper()
	ctx := context.Background()
	raw := json.RawMessage(`{"overrides":{"hub.agent_secrets":true}}`)
	_, err := s.UpsertHubSetting(ctx, "experiments", raw, "test", -1, "managed")
	require.NoError(t, err)
	ops := NewOperationalSettings(s, emptyKoanf(), emptyKoanf())
	_, err = ops.Refresh(ctx)
	require.NoError(t, err)
	srv.SetOperationalSettings(ops)
	require.True(t, srv.experimentEnabled(experiments.AgentSecrets))
}

func setupAgentSecretsTestFixture(t *testing.T, enableExperiment bool) (*Server, store.Store, *store.User, *store.User, *store.Project, *store.Agent, *store.Agent) {
	t.Helper()
	srv, s := testServer(t)
	if srv.GetSecretBackend() == nil {
		srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub", "test-signing-key-32-bytes-long!!"))
	}
	ctx := context.Background()

	if enableExperiment {
		enableAgentSecretsExperimentForTest(t, srv, s)
	}

	owner := &store.User{
		ID:          api.NewUUID(),
		Email:       "owner@example.com",
		DisplayName: "Owner",
		Role:        store.UserRoleMember,
		Status:      store.UserStatusActive,
	}
	require.NoError(t, s.CreateUser(ctx, owner))

	viewer := &store.User{
		ID:          api.NewUUID(),
		Email:       "viewer@example.com",
		DisplayName: "Viewer",
		Role:        store.UserRoleMember,
		Status:      store.UserStatusActive,
	}
	require.NoError(t, s.CreateUser(ctx, viewer))

	project := &store.Project{
		ID:      api.NewUUID(),
		Name:    "test-proj",
		Slug:    "test-proj",
		OwnerID: owner.ID,
	}
	require.NoError(t, s.CreateProject(ctx, project))

	ownerRD, err := s.GetRoleDefinitionByName(ctx, "project-owner", store.RoleScopeProject)
	require.NoError(t, err)
	viewerRD, err := s.GetRoleDefinitionByName(ctx, "project-viewer", store.RoleScopeProject)
	require.NoError(t, err)

	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		ID:               api.NewUUID(),
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      owner.ID,
		RoleDefinitionID: ownerRD.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          project.ID,
	})
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		ID:               api.NewUUID(),
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      viewer.ID,
		RoleDefinitionID: viewerRD.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          project.ID,
	})
	require.NoError(t, err)

	agentX := &store.Agent{
		ID:        api.NewUUID(),
		Slug:      "agent-x",
		Name:      "agent-x",
		ProjectID: project.ID,
		OwnerID:   owner.ID,
		CreatedBy: owner.ID,
		Ancestry:  []string{owner.ID},
		Phase:     "running",
	}
	require.NoError(t, s.CreateAgent(ctx, agentX))

	agentY := &store.Agent{
		ID:        api.NewUUID(),
		Slug:      "agent-y",
		Name:      "agent-y",
		ProjectID: project.ID,
		OwnerID:   owner.ID,
		CreatedBy: owner.ID,
		Ancestry:  []string{owner.ID},
		Phase:     "running",
	}
	require.NoError(t, s.CreateAgent(ctx, agentY))

	return srv, s, owner, viewer, project, agentX, agentY
}

func doAsAgentWithScopes(t *testing.T, srv *Server, agent *store.Agent, scopes []AgentTokenScope, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&buf).Encode(body))
	}
	req := httptest.NewRequest(method, target, &buf)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	tok := srv.generateRecordedAgentToken(t, agent.ID, agent.ProjectID, scopes, agent.Ancestry)
	req.Header.Set("X-Scion-Agent-Token", tok)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	return rr
}

func TestAgentSecrets_ExperimentOffRejectedEverywhere(t *testing.T) {
	srv, _, owner, _, _, agentX, _ := setupAgentSecretsTestFixture(t, false)

	// Human routes reject scope=agent with 400 when experiment is off.
	rr := doRequestAsUser(t, srv, owner, http.MethodPut, "/api/v1/secrets/FOO", SetSecretRequest{
		Value:    "secret-val",
		Encoding: "raw",
		Scope:    store.ScopeAgent,
		ScopeID:  agentX.ID,
	})
	assert.Equal(t, http.StatusBadRequest, rr.Code, "body: %s", rr.Body.String())
	assert.Contains(t, rr.Body.String(), "invalid scope")

	rr = doRequestAsUser(t, srv, owner, http.MethodGet, "/api/v1/secrets?scope=agent&scopeId="+agentX.ID, nil)
	assert.Equal(t, http.StatusBadRequest, rr.Code)

	rr = doRequestAsUser(t, srv, owner, http.MethodDelete, "/api/v1/secrets/FOO?scope=agent&scopeId="+agentX.ID, nil)
	assert.Equal(t, http.StatusBadRequest, rr.Code)

	// Agent runtime routes reject scope=agent with 400 when experiment is off.
	scopes := []AgentTokenScope{ScopeAgentStatusUpdate, ScopeProjectSecretRead}
	rr = doAsAgentWithScopes(t, srv, agentX, scopes, http.MethodGet, "/api/v1/agents/"+agentX.ID+"/secrets?scope=agent", nil)
	assert.Equal(t, http.StatusBadRequest, rr.Code)

	rr = doAsAgentWithScopes(t, srv, agentX, scopes, http.MethodGet, "/api/v1/agents/"+agentX.ID+"/secrets/FOO?scope=agent", nil)
	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestAgentSecrets_HumanManagementAuthzAndValidation(t *testing.T) {
	srv, _, owner, viewer, _, agentX, _ := setupAgentSecretsTestFixture(t, true)

	// Plain env vars never accept scope=agent even with experiment on.
	rr := doRequestAsUser(t, srv, owner, http.MethodGet, "/api/v1/env?scope=agent&scopeId="+agentX.ID, nil)
	assert.Equal(t, http.StatusBadRequest, rr.Code)

	// allowProgeny is rejected (400) at agent scope.
	rr = doRequestAsUser(t, srv, owner, http.MethodPut, "/api/v1/secrets/FOO", SetSecretRequest{
		Value:        "val",
		Encoding:     "raw",
		Scope:        store.ScopeAgent,
		ScopeID:      agentX.ID,
		AllowProgeny: true,
	})
	assert.Equal(t, http.StatusBadRequest, rr.Code, "body: %s", rr.Body.String())

	// Reserved names rejected (400) at agent scope.
	for _, reserved := range []string{"SCION_AUTH_TOKEN", "GCE_METADATA_HOST"} {
		rr = doRequestAsUser(t, srv, owner, http.MethodPut, "/api/v1/secrets/"+reserved, SetSecretRequest{
			Value:    "val",
			Encoding: "raw",
			Scope:    store.ScopeAgent,
			ScopeID:  agentX.ID,
		})
		assert.Equal(t, http.StatusBadRequest, rr.Code, "reserved %s should be rejected", reserved)
	}

	// Viewer (agent.read only) cannot set agent secret -> 403.
	rr = doRequestAsUser(t, srv, viewer, http.MethodPut, "/api/v1/secrets/FOO", SetSecretRequest{
		Value:    "top-secret-value-123",
		Encoding: "raw",
		Scope:    store.ScopeAgent,
		ScopeID:  agentX.ID,
	})
	assert.Equal(t, http.StatusForbidden, rr.Code)

	// Owner (agent.update) sets agent secret -> 200, value never returned.
	rr = doRequestAsUser(t, srv, owner, http.MethodPut, "/api/v1/secrets/FOO", SetSecretRequest{
		Value:       "top-secret-value-123",
		Encoding:    "raw",
		Scope:       store.ScopeAgent,
		ScopeID:     agentX.ID,
		Description: "agent secret desc",
	})
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	assert.NotContains(t, rr.Body.String(), "top-secret-value-123")

	// Viewer (agent.read) can list and get metadata, never sees value.
	rr = doRequestAsUser(t, srv, viewer, http.MethodGet, "/api/v1/secrets?scope=agent&scopeId="+agentX.ID, nil)
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	assert.Contains(t, rr.Body.String(), `"FOO"`)
	assert.NotContains(t, rr.Body.String(), "top-secret-value-123")

	rr = doRequestAsUser(t, srv, viewer, http.MethodGet, "/api/v1/secrets/FOO?scope=agent&scopeId="+agentX.ID, nil)
	require.Equal(t, http.StatusOK, rr.Code)
	assert.NotContains(t, rr.Body.String(), "top-secret-value-123")

	// Viewer cannot PATCH or DELETE -> 403.
	newDesc := "changed"
	rr = doRequestAsUser(t, srv, viewer, http.MethodPatch, "/api/v1/secrets/FOO?scope=agent&scopeId="+agentX.ID, PatchSecretRequest{
		Description: &newDesc,
	})
	assert.Equal(t, http.StatusForbidden, rr.Code)

	rr = doRequestAsUser(t, srv, viewer, http.MethodDelete, "/api/v1/secrets/FOO?scope=agent&scopeId="+agentX.ID, nil)
	assert.Equal(t, http.StatusForbidden, rr.Code)

	// Owner PATCH with allowProgeny=true -> 400.
	allowTrue := true
	rr = doRequestAsUser(t, srv, owner, http.MethodPatch, "/api/v1/secrets/FOO?scope=agent&scopeId="+agentX.ID, PatchSecretRequest{
		AllowProgeny: &allowTrue,
	})
	assert.Equal(t, http.StatusBadRequest, rr.Code)

	// Owner DELETE succeeds -> 204.
	rr = doRequestAsUser(t, srv, owner, http.MethodDelete, "/api/v1/secrets/FOO?scope=agent&scopeId="+agentX.ID, nil)
	assert.Equal(t, http.StatusNoContent, rr.Code)
}

func TestAgentSecrets_RuntimeReadIsolationPrecedenceAndAudit(t *testing.T) {
	srv, _, owner, _, project, agentX, agentY := setupAgentSecretsTestFixture(t, true)
	auditLog := &recordingSecretAuditLogger{}
	srv.auditLogger = auditLog

	const (
		agentVal   = "agent-x-only-super-secret"
		projectVal = "project-shared-secret"
	)

	// Human sets FOO at project scope and FOO + X_ONLY at agent X scope.
	rr := doRequestAsUser(t, srv, owner, http.MethodPut, "/api/v1/secrets/FOO", SetSecretRequest{
		Value: projectVal, Encoding: "raw", Scope: store.ScopeProject, ScopeID: project.ID,
	})
	require.Equal(t, http.StatusOK, rr.Code)

	rr = doRequestAsUser(t, srv, owner, http.MethodPut, "/api/v1/secrets/FOO", SetSecretRequest{
		Value: agentVal, Encoding: "raw", Scope: store.ScopeAgent, ScopeID: agentX.ID,
	})
	require.Equal(t, http.StatusOK, rr.Code)

	rr = doRequestAsUser(t, srv, owner, http.MethodPut, "/api/v1/secrets/X_ONLY", SetSecretRequest{
		Value: agentVal, Encoding: "raw", Scope: store.ScopeAgent, ScopeID: agentX.ID,
	})
	require.Equal(t, http.StatusOK, rr.Code)

	fullScopes := []AgentTokenScope{ScopeAgentStatusUpdate, ScopeProjectSecretRead}
	noProjReadScopes := []AgentTokenScope{ScopeAgentStatusUpdate}

	// 1. Agent X lists all secrets: sees both agent and project with scope set.
	rr = doAsAgentWithScopes(t, srv, agentX, fullScopes, http.MethodGet, "/api/v1/agents/"+agentX.ID+"/secrets", nil)
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	var listResp AgentListSecretsResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &listResp))
	foundScopes := map[string][]string{}
	for _, s := range listResp.Secrets {
		foundScopes[s.Key] = append(foundScopes[s.Key], s.Scope)
	}
	assert.Contains(t, foundScopes["FOO"], store.ScopeAgent)
	assert.Contains(t, foundScopes["FOO"], store.ScopeProject)
	assert.Equal(t, []string{store.ScopeAgent}, foundScopes["X_ONLY"])

	// 2. Agent X GET FOO (default): resolves agent first.
	rr = doAsAgentWithScopes(t, srv, agentX, fullScopes, http.MethodGet, "/api/v1/agents/"+agentX.ID+"/secrets/FOO", nil)
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	var getResp AgentGetSecretResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &getResp))
	decoded, err := base64.StdEncoding.DecodeString(getResp.Value)
	require.NoError(t, err)
	assert.Equal(t, agentVal, string(decoded))
	assert.Equal(t, store.ScopeAgent, getResp.Scope)

	// 3. Agent X GET FOO?scope=project: pins to project scope.
	rr = doAsAgentWithScopes(t, srv, agentX, fullScopes, http.MethodGet, "/api/v1/agents/"+agentX.ID+"/secrets/FOO?scope=project", nil)
	require.Equal(t, http.StatusOK, rr.Code)
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &getResp))
	decoded, err = base64.StdEncoding.DecodeString(getResp.Value)
	require.NoError(t, err)
	assert.Equal(t, projectVal, string(decoded))
	assert.Equal(t, store.ScopeProject, getResp.Scope)

	// 4. Agent X WITHOUT project:secret:read can still read its own agent-scope secrets.
	rr = doAsAgentWithScopes(t, srv, agentX, noProjReadScopes, http.MethodGet, "/api/v1/agents/"+agentX.ID+"/secrets/X_ONLY", nil)
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &getResp))
	decoded, err = base64.StdEncoding.DecodeString(getResp.Value)
	require.NoError(t, err)
	assert.Equal(t, agentVal, string(decoded))

	rr = doAsAgentWithScopes(t, srv, agentX, noProjReadScopes, http.MethodGet, "/api/v1/agents/"+agentX.ID+"/secrets?scope=agent", nil)
	require.Equal(t, http.StatusOK, rr.Code)

	// 5. Init fetch POST /api/v1/agent/secrets resolves agent scope first.
	rr = doAsAgentWithScopes(t, srv, agentX, fullScopes, http.MethodPost, "/api/v1/agent/secrets", secretFetchRequest{Keys: []string{"FOO", "X_ONLY"}})
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	var fetchResp secretFetchResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &fetchResp))
	require.Len(t, fetchResp.Secrets, 2)
	assert.Equal(t, "ok", fetchResp.Secrets[0].Status)
	assert.Equal(t, agentVal, fetchResp.Secrets[0].Value)
	assert.Equal(t, "ok", fetchResp.Secrets[1].Status)
	assert.Equal(t, agentVal, fetchResp.Secrets[1].Value)

	// 6. Agent Y (same project, same owner) cannot read X's secrets.
	rr = doAsAgentWithScopes(t, srv, agentY, fullScopes, http.MethodGet, "/api/v1/agents/"+agentX.ID+"/secrets/X_ONLY", nil)
	assert.Equal(t, http.StatusForbidden, rr.Code)

	rr = doAsAgentWithScopes(t, srv, agentY, fullScopes, http.MethodGet, "/api/v1/agents/"+agentY.ID+"/secrets/X_ONLY", nil)
	assert.Equal(t, http.StatusNotFound, rr.Code)

	rr = doAsAgentWithScopes(t, srv, agentY, fullScopes, http.MethodPost, "/api/v1/agent/secrets", secretFetchRequest{Keys: []string{"X_ONLY"}})
	require.Equal(t, http.StatusOK, rr.Code)
	var fetchRespY secretFetchResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &fetchRespY))
	require.Len(t, fetchRespY.Secrets, 1)
	assert.Equal(t, "not_found", fetchRespY.Secrets[0].Status)
	assert.Empty(t, fetchRespY.Secrets[0].Value)

	// 7. Audit records agent, key, scope, version — never the secret value.
	auditLog.mu.Lock()
	defer auditLog.mu.Unlock()
	foundAgentAudit := false
	for _, ev := range auditLog.secretReadEvents {
		raw, _ := json.Marshal(ev)
		assert.NotContains(t, string(raw), agentVal, "audit event must never contain secret value")
		if ev.AgentID == agentX.ID && ev.SecretKey == "FOO" && ev.Scope == store.ScopeAgent && ev.Success {
			foundAgentAudit = true
			assert.GreaterOrEqual(t, ev.Version, 1, "audit event must record secret version")
		}
	}
	assert.True(t, foundAgentAudit, "expected audit event for agent-scope FOO read")
}

func TestAgentSecrets_DeliveryPrecedenceInjectionModesAndSkippedWarning(t *testing.T) {
	srv, s, owner, _, project, agentX, _ := setupAgentSecretsTestFixture(t, true)
	ctx := context.Background()

	// Seed project, user, and agent secrets.
	_, _, err := srv.secretBackend.Set(ctx, &secret.SetSecretInput{
		Name: "SHARED", Value: "project-val", Scope: store.ScopeProject, ScopeID: project.ID, InjectionMode: store.InjectionModeAlways,
	})
	require.NoError(t, err)
	_, _, err = srv.secretBackend.Set(ctx, &secret.SetSecretInput{
		Name: "SHARED", Value: "user-val", Scope: store.ScopeUser, ScopeID: owner.ID, InjectionMode: store.InjectionModeAlways,
	})
	require.NoError(t, err)
	_, _, err = srv.secretBackend.Set(ctx, &secret.SetSecretInput{
		Name: "SHARED", Value: "agent-val", Scope: store.ScopeAgent, ScopeID: agentX.ID, ProjectID: project.ID, InjectionMode: store.InjectionModeAlways,
	})
	require.NoError(t, err)
	_, _, err = srv.secretBackend.Set(ctx, &secret.SetSecretInput{
		Name: "DECLARED_ONLY", Value: "declared-val", Scope: store.ScopeAgent, ScopeID: agentX.ID, ProjectID: project.ID, InjectionMode: store.InjectionModeAsNeeded,
	})
	require.NoError(t, err)
	_, _, err = srv.secretBackend.Set(ctx, &secret.SetSecretInput{
		Name: "UNDECLARED", Value: "undeclared-val", Scope: store.ScopeAgent, ScopeID: agentX.ID, ProjectID: project.ID, InjectionMode: store.InjectionModeAsNeeded,
	})
	require.NoError(t, err)
	_, _, err = srv.secretBackend.Set(ctx, &secret.SetSecretInput{
		Name: "SSH_KEY", Value: "pem-data", SecretType: store.SecretTypeFile, Target: "~/.ssh/id_test", Scope: store.ScopeAgent, ScopeID: agentX.ID, ProjectID: project.ID,
	})
	require.NoError(t, err)
	_, _, err = srv.secretBackend.Set(ctx, &secret.SetSecretInput{
		Name: "CFG_VAR", Value: `{"a":1}`, SecretType: store.SecretTypeVariable, Target: "cfg_var", Scope: store.ScopeAgent, ScopeID: agentX.ID, ProjectID: project.ID,
	})
	require.NoError(t, err)

	d := NewHTTPAgentDispatcher(s, false, nil)
	d.SetSecretBackend(srv.secretBackend)
	d.SetAuthzService(srv.authzService)
	d.SetAgentSecretsEnabledProvider(func() bool { return srv.experimentEnabled(experiments.AgentSecrets) })
	sel := newDeliverySelector(srv.authzService, agentX, PurposeRestart)
	gathered := &reprovisionGatheredEnv{
		keys: map[string]string{"DECLARED_ONLY": "DECLARED_ONLY"},
	}

	// With experiment ON: agent-scope SHARED overrides user/project, file/variable
	// are always delivered, DECLARED_ONLY is in asNeededEnv, UNDECLARED is omitted.
	resolved, asNeeded, skipped, err := d.selectDeliverySecretsWith(ctx, sel, agentX, gathered)
	require.NoError(t, err)
	assert.Empty(t, skipped)

	byName := map[string]ResolvedSecret{}
	for _, r := range resolved {
		byName[r.Name] = r
	}
	assert.Equal(t, "agent-val", byName["SHARED"].Value)
	assert.Equal(t, store.ScopeAgent, byName["SHARED"].Source)
	assert.Equal(t, "pem-data", byName["SSH_KEY"].Value)
	assert.Equal(t, "~/.ssh/id_test", byName["SSH_KEY"].Target)
	assert.Equal(t, `{"a":1}`, byName["CFG_VAR"].Value)
	assert.Equal(t, "declared-val", asNeeded["DECLARED_ONLY"])
	_, hasUndeclared := asNeeded["UNDECLARED"]
	assert.False(t, hasUndeclared)

	// With experiment OFF: agent-scope secrets are skipped by delivery.
	_, err = s.UpsertHubSetting(ctx, "experiments", json.RawMessage(`{"overrides":{"hub.agent_secrets":false}}`), "test", -1, "managed")
	require.NoError(t, err)
	_, err = srv.operationalSettings.Load().Refresh(ctx)
	require.NoError(t, err)

	selOff := newDeliverySelector(srv.authzService, agentX, PurposeRestart)
	resolvedOff, asNeededOff, _, err := d.selectDeliverySecretsWith(ctx, selOff, agentX, gathered)
	require.NoError(t, err)
	for _, r := range resolvedOff {
		assert.NotEqual(t, store.ScopeAgent, r.Source, "agent-scope secret must not be delivered when experiment is off")
	}
	_, hasDeclaredOff := asNeededOff["DECLARED_ONLY"]
	assert.False(t, hasDeclaredOff)

	// Skipped-secret warning naming for agent scope: named for editors, counted for others.
	sk := []skippedSecret{{Name: "MY_AGENT_SECRET", Scope: store.ScopeAgent, Class: skipClassAccess}}
	assert.Contains(t, formatSkippedSecretWarningFor(sk, "started", false, true), "MY_AGENT_SECRET (agent)")
	warnNonEditor := formatSkippedSecretWarningFor(sk, "started", false, false)
	assert.NotContains(t, warnNonEditor, "MY_AGENT_SECRET")
	assert.Contains(t, warnNonEditor, "1 agent-scope secret")
}

func TestAgentSecrets_LifecyclePersistenceHardDeletePurgeAndCleanup(t *testing.T) {
	srv, s, _, _, project, agentX, agentY := setupAgentSecretsTestFixture(t, true)
	ctx := context.Background()

	// Set secrets on agentX and agentY.
	_, _, err := srv.secretBackend.Set(ctx, &secret.SetSecretInput{
		Name: "KEEP_SOFT", Value: "v1", Scope: store.ScopeAgent, ScopeID: agentX.ID, ProjectID: project.ID,
	})
	require.NoError(t, err)
	_, _, err = srv.secretBackend.Set(ctx, &secret.SetSecretInput{
		Name: "PURGE_ME", Value: "v2", Scope: store.ScopeAgent, ScopeID: agentY.ID, ProjectID: project.ID,
	})
	require.NoError(t, err)

	// 1. Soft delete agentX: secrets MUST persist.
	err = s.WithTx(ctx, func(tx store.Store) error {
		return srv.softDeleteAgentTx(ctx, tx, agentX, AuditActor{})
	})
	require.NoError(t, err)
	_, err = s.GetSecret(ctx, "KEEP_SOFT", store.ScopeAgent, agentX.ID)
	require.NoError(t, err, "agent secrets must survive soft delete")

	// 2. Hard delete agentX: local DB rows MUST be deleted in transaction.
	err = s.WithTx(ctx, func(tx store.Store) error {
		return srv.hardDeleteAgentTx(ctx, tx, agentX, AuditActor{})
	})
	require.NoError(t, err)
	_, err = s.GetSecret(ctx, "KEEP_SOFT", store.ScopeAgent, agentX.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "hard delete must remove agent-scope secrets")

	// 3. Retention purge of soft-deleted agentY: local DB rows MUST be removed.
	past := time.Now().Add(-48 * time.Hour)
	agentY.DeletedAt = past
	require.NoError(t, s.UpdateAgent(ctx, agentY))
	n, err := s.PurgeDeletedAgentsByID(ctx, []string{agentY.ID}, time.Now().Add(-24*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	_, err = s.GetSecret(ctx, "PURGE_ME", store.ScopeAgent, agentY.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "retention purge must remove agent-scope secrets")

	// 4. Post-commit cleanup removes secrets via backend.Delete.
	_, _, err = srv.secretBackend.Set(ctx, &secret.SetSecretInput{
		Name: "EXT_KEY", Value: "v3", Scope: store.ScopeAgent, ScopeID: agentX.ID, ProjectID: project.ID,
	})
	require.NoError(t, err)
	assert.True(t, srv.removeAgentScopedSecrets(ctx, agentX.ID, []string{"EXT_KEY"}))
	_, err = s.GetSecret(ctx, "EXT_KEY", store.ScopeAgent, agentX.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

type spyDeleteSecretBackend struct {
	secret.SecretBackend
	mu      sync.Mutex
	deleted []string // "scope/scopeID/name"
}

func (b *spyDeleteSecretBackend) Delete(ctx context.Context, name, scope, scopeID string) error {
	b.mu.Lock()
	b.deleted = append(b.deleted, scope+"/"+scopeID+"/"+name)
	b.mu.Unlock()
	return b.SecretBackend.Delete(ctx, name, scope, scopeID)
}

func TestAgentSecrets_ProjectDeletionRemovesAgentSecretsAndCallsExternalCleanup(t *testing.T) {
	srv, s, owner, _, project, agentX, _ := setupAgentSecretsTestFixture(t, true)
	ctx := context.Background()

	spy := &spyDeleteSecretBackend{SecretBackend: srv.GetSecretBackend()}
	srv.SetSecretBackend(spy)

	// Create a second project + agent whose secrets must remain untouched.
	otherProject := &store.Project{
		ID:      api.NewUUID(),
		Name:    "other-proj",
		Slug:    "other-proj",
		OwnerID: owner.ID,
	}
	require.NoError(t, s.CreateProject(ctx, otherProject))
	otherAgent := &store.Agent{
		ID:        api.NewUUID(),
		Slug:      "other-agent",
		Name:      "other-agent",
		ProjectID: otherProject.ID,
		OwnerID:   owner.ID,
		CreatedBy: owner.ID,
		Ancestry:  []string{owner.ID},
		Phase:     "running",
	}
	require.NoError(t, s.CreateAgent(ctx, otherAgent))

	_, _, err := spy.Set(ctx, &secret.SetSecretInput{
		Name: "PROJ_DEL_KEY", Value: "secret-val", Scope: store.ScopeAgent, ScopeID: agentX.ID, ProjectID: project.ID,
	})
	require.NoError(t, err)
	_, _, err = spy.Set(ctx, &secret.SetSecretInput{
		Name: "OTHER_KEEP_KEY", Value: "other-val", Scope: store.ScopeAgent, ScopeID: otherAgent.ID, ProjectID: otherProject.ID,
	})
	require.NoError(t, err)

	// Force-delete the project as owner (with agents still present).
	rr := doRequestAsUser(t, srv, owner, http.MethodDelete, "/api/v1/projects/"+project.ID+"?force=true", nil)
	require.True(t, rr.Code == http.StatusNoContent || rr.Code == http.StatusOK,
		"expected 204 or 200 on project delete, got %d: %s", rr.Code, rr.Body.String())

	// Agent-scope secret DB row for deleted project's agent must be gone.
	_, err = s.GetSecret(ctx, "PROJ_DEL_KEY", store.ScopeAgent, agentX.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "project delete must remove agent-scope secret rows in transaction")

	// External backend cleanup (Delete) must have been called for the agent secret.
	spy.mu.Lock()
	deletedCalls := append([]string(nil), spy.deleted...)
	spy.mu.Unlock()
	assert.Contains(t, deletedCalls, store.ScopeAgent+"/"+agentX.ID+"/PROJ_DEL_KEY",
		"project delete must invoke post-commit external cleanup for agent-scope secrets")

	// Other project's agent secret must remain intact.
	_, err = s.GetSecret(ctx, "OTHER_KEEP_KEY", store.ScopeAgent, otherAgent.ID)
	require.NoError(t, err, "other project's agent secret must not be affected")
}

// TestAgentSecrets_AsNeededEnvVarBeatsAgentSecret is the R1 regression test:
// a stored as_needed env var wins over an as_needed agent-scope secret for the
// same key (matching the always path and envGatherSourceLabel), while agent
// scope still beats lower secret scopes for keys only in the secret store.
func TestAgentSecrets_AsNeededEnvVarBeatsAgentSecret(t *testing.T) {
	srv, s, _, _, project, agentX, _ := setupAgentSecretsTestFixture(t, true)
	ctx := context.Background()

	// Project-scope as_needed plain env var REVIEW_FOO.
	_, err := s.UpsertEnvVar(ctx, &store.EnvVar{
		ID:            api.NewUUID(),
		Key:           "REVIEW_FOO",
		Value:         "env-val",
		Scope:         store.ScopeProject,
		ScopeID:       project.ID,
		InjectionMode: store.InjectionModeAsNeeded,
	})
	require.NoError(t, err)

	// Agent-scope as_needed secret REVIEW_FOO must NOT overwrite the env var.
	_, _, err = srv.secretBackend.Set(ctx, &secret.SetSecretInput{
		Name:          "REVIEW_FOO",
		Value:         "agent-secret-val",
		Scope:         store.ScopeAgent,
		ScopeID:       agentX.ID,
		ProjectID:     project.ID,
		InjectionMode: store.InjectionModeAsNeeded,
	})
	require.NoError(t, err)

	// For a key only in secrets, agent scope still overrides project scope.
	_, _, err = srv.secretBackend.Set(ctx, &secret.SetSecretInput{
		Name:          "SECRET_ONLY",
		Value:         "proj-secret-val",
		Scope:         store.ScopeProject,
		ScopeID:       project.ID,
		InjectionMode: store.InjectionModeAsNeeded,
	})
	require.NoError(t, err)
	_, _, err = srv.secretBackend.Set(ctx, &secret.SetSecretInput{
		Name:          "SECRET_ONLY",
		Value:         "agent-secret-only-val",
		Scope:         store.ScopeAgent,
		ScopeID:       agentX.ID,
		ProjectID:     project.ID,
		InjectionMode: store.InjectionModeAsNeeded,
	})
	require.NoError(t, err)

	d := NewHTTPAgentDispatcher(s, false, nil)
	d.SetSecretBackend(srv.secretBackend)
	d.SetAuthzService(srv.authzService)
	d.SetAgentSecretsEnabledProvider(func() bool { return srv.experimentEnabled(experiments.AgentSecrets) })

	got, err := d.resolveAsNeededForKeys(ctx, agentX, []string{"REVIEW_FOO", "SECRET_ONLY"}, nil)
	require.NoError(t, err)
	assert.Equal(t, "env-val", got["REVIEW_FOO"], "stored env var must win over as_needed agent-scope secret")
	assert.Equal(t, "agent-secret-only-val", got["SECRET_ONLY"], "agent-scope secret must still beat project-scope secret")
}

// TestAgentSecrets_ScopedTokens pins the UAT bearer disposition of
// agent.secret.read and agent.secret.write on /api/v1/secrets?scope=agent:
// agent:read admits metadata reads, agent:update admits writes, unrelated
// selectors are refused, and tokens bounded to another project are refused.
func TestAgentSecrets_ScopedTokens(t *testing.T) {
	srv, s, owner, _, project, agentX, _ := setupAgentSecretsTestFixture(t, true)
	ctx := context.Background()

	otherProject := &store.Project{
		ID:      api.NewUUID(),
		Name:    "other-uat-proj",
		Slug:    "other-uat-proj",
		OwnerID: owner.ID,
	}
	require.NoError(t, s.CreateProject(ctx, otherProject))
	ownerRD, err := s.GetRoleDefinitionByName(ctx, "project-owner", store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		ID:               api.NewUUID(),
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      owner.ID,
		RoleDefinitionID: ownerRD.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          otherProject.ID,
	})
	require.NoError(t, err)

	mint := func(name string, boundary TokenBoundary, scopes ...string) string {
		t.Helper()
		key, _, err := srv.uatService.CreateTokenWithParams(rs4MintContext(owner.ID), CreateTokenParams{
			UserID:   owner.ID,
			Name:     name,
			Boundary: boundary,
			Scopes:   scopes,
		})
		require.NoError(t, err)
		return key
	}

	doUAT := func(key, method, target string, body any) *httptest.ResponseRecorder {
		t.Helper()
		var buf bytes.Buffer
		if body != nil {
			require.NoError(t, json.NewEncoder(&buf).Encode(body))
		}
		req := httptest.NewRequest(method, target, &buf)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Authorization", "Bearer "+key)
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		return rr
	}

	readTok := mint("agent-read-tok", projectBoundary(project.ID), "agent:read")
	writeTok := mint("agent-write-tok", projectBoundary(project.ID), "agent:update")
	unrelatedTok := mint("unrelated-tok", projectBoundary(project.ID), "project:read")
	otherProjReadTok := mint("other-proj-read-tok", projectBoundary(otherProject.ID), "agent:read")
	otherProjWriteTok := mint("other-proj-write-tok", projectBoundary(otherProject.ID), "agent:update")

	// agent:update admits PUT.
	rr := doUAT(writeTok, http.MethodPut, "/api/v1/secrets/UAT_KEY", SetSecretRequest{
		Value: "uat-val", Encoding: "raw", Scope: store.ScopeAgent, ScopeID: agentX.ID,
	})
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	// agent:read refuses PUT, PATCH, DELETE; admits GET list & GET key.
	rr = doUAT(readTok, http.MethodPut, "/api/v1/secrets/UAT_KEY2", SetSecretRequest{
		Value: "v", Encoding: "raw", Scope: store.ScopeAgent, ScopeID: agentX.ID,
	})
	assert.Equal(t, http.StatusForbidden, rr.Code)

	rr = doUAT(readTok, http.MethodGet, "/api/v1/secrets?scope=agent&scopeId="+agentX.ID, nil)
	assert.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	rr = doUAT(readTok, http.MethodGet, "/api/v1/secrets/UAT_KEY?scope=agent&scopeId="+agentX.ID, nil)
	assert.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	desc := "updated-by-uat"
	rr = doUAT(readTok, http.MethodPatch, "/api/v1/secrets/UAT_KEY?scope=agent&scopeId="+agentX.ID, PatchSecretRequest{Description: &desc})
	assert.Equal(t, http.StatusForbidden, rr.Code)

	rr = doUAT(readTok, http.MethodDelete, "/api/v1/secrets/UAT_KEY?scope=agent&scopeId="+agentX.ID, nil)
	assert.Equal(t, http.StatusForbidden, rr.Code)

	// Unrelated selector refused on read and write.
	rr = doUAT(unrelatedTok, http.MethodGet, "/api/v1/secrets?scope=agent&scopeId="+agentX.ID, nil)
	assert.Equal(t, http.StatusForbidden, rr.Code)
	rr = doUAT(unrelatedTok, http.MethodPut, "/api/v1/secrets/UAT_KEY3", SetSecretRequest{
		Value: "v", Encoding: "raw", Scope: store.ScopeAgent, ScopeID: agentX.ID,
	})
	assert.Equal(t, http.StatusForbidden, rr.Code)

	// Token bounded to another project refused on read and write.
	rr = doUAT(otherProjReadTok, http.MethodGet, "/api/v1/secrets?scope=agent&scopeId="+agentX.ID, nil)
	assert.Equal(t, http.StatusForbidden, rr.Code)
	rr = doUAT(otherProjWriteTok, http.MethodPut, "/api/v1/secrets/UAT_KEY4", SetSecretRequest{
		Value: "v", Encoding: "raw", Scope: store.ScopeAgent, ScopeID: agentX.ID,
	})
	assert.Equal(t, http.StatusForbidden, rr.Code)

	// agent:update admits PATCH and DELETE.
	rr = doUAT(writeTok, http.MethodPatch, "/api/v1/secrets/UAT_KEY?scope=agent&scopeId="+agentX.ID, PatchSecretRequest{Description: &desc})
	assert.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	rr = doUAT(writeTok, http.MethodDelete, "/api/v1/secrets/UAT_KEY?scope=agent&scopeId="+agentX.ID, nil)
	assert.Equal(t, http.StatusNoContent, rr.Code)
}

// TestAgentSecrets_HardDeleteRouteAndPurgeExpiredAgentsCallExternalCleanup
// exercises the real hard-delete DELETE route (deletionEngine.finish) and
// purgeExpiredAgents call sites with a spy backend, proving that both remove
// DB rows and invoke backend.Delete for agent-scoped secrets.
func TestAgentSecrets_HardDeleteRouteAndPurgeExpiredAgentsCallExternalCleanup(t *testing.T) {
	srv, s, owner, _, project, agentX, agentY := setupAgentSecretsTestFixture(t, true)
	ctx := context.Background()

	spy := &spyDeleteSecretBackend{SecretBackend: srv.GetSecretBackend()}
	srv.SetSecretBackend(spy)

	_, _, err := spy.Set(ctx, &secret.SetSecretInput{
		Name: "HARD_DEL_KEY", Value: "v1", Scope: store.ScopeAgent, ScopeID: agentX.ID, ProjectID: project.ID,
	})
	require.NoError(t, err)
	_, _, err = spy.Set(ctx, &secret.SetSecretInput{
		Name: "PURGE_EXP_KEY", Value: "v2", Scope: store.ScopeAgent, ScopeID: agentY.ID, ProjectID: project.ID,
	})
	require.NoError(t, err)

	// 1. Hard delete agentX through the real DELETE /api/v1/agents/{id} route
	// (default soft_delete_retention is 0 -> hard delete via deletionEngine.finish).
	rr := doRequestAsUser(t, srv, owner, http.MethodDelete, "/api/v1/agents/"+agentX.ID+"?deleteFiles=true", nil)
	require.True(t, rr.Code == http.StatusNoContent || rr.Code == http.StatusOK,
		"expected 204 or 200 on agent hard delete, got %d: %s", rr.Code, rr.Body.String())

	_, err = s.GetSecret(ctx, "HARD_DEL_KEY", store.ScopeAgent, agentX.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "hard delete route must remove agent-scope DB row")

	spy.mu.Lock()
	deletedAfterHard := append([]string(nil), spy.deleted...)
	spy.mu.Unlock()
	assert.Contains(t, deletedAfterHard, store.ScopeAgent+"/"+agentX.ID+"/HARD_DEL_KEY",
		"hard delete route must invoke post-commit backend.Delete via deletionEngine.finish")

	// 2. Retention purge of soft-deleted agentY through purgeExpiredAgents.
	agentY.DeletedAt = time.Now().Add(-48 * time.Hour)
	require.NoError(t, s.UpdateAgent(ctx, agentY))
	purged, err := srv.purgeExpiredAgents(ctx, time.Now().Add(-24*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 1, purged)

	_, err = s.GetSecret(ctx, "PURGE_EXP_KEY", store.ScopeAgent, agentY.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "purgeExpiredAgents must remove agent-scope DB row")

	spy.mu.Lock()
	deletedAfterPurge := append([]string(nil), spy.deleted...)
	spy.mu.Unlock()
	assert.Contains(t, deletedAfterPurge, store.ScopeAgent+"/"+agentY.ID+"/PURGE_EXP_KEY",
		"purgeExpiredAgents must invoke post-commit backend.Delete")
}

// TestAgentSecrets_EnvGatherRecreateCarriesOverAndCleansUpOldID proves that
// Branch 3 (env-gather recreate of a provisioning agent) carries agent-scope
// secrets over to the replacement agent's new ID with identical metadata and
// values, leaves nothing under the old ID, and invokes post-commit external
// backend.Delete for the old ID.
func TestAgentSecrets_EnvGatherRecreateCarriesOverAndCleansUpOldID(t *testing.T) {
	ctx := context.Background()
	disp := &createAgentDispatcher{
		envReqs: &RemoteEnvRequirementsResponse{Needs: []string{"CARRY_KEY"}},
	}
	srv, s, project := setupCreateAgentServer(t, disp)
	spy := &spyDeleteSecretBackend{SecretBackend: secret.NewLocalBackend(s, "test-hub")}
	srv.SetSecretBackend(spy)

	// First create with GatherEnv=true leaves the agent in PhaseProvisioning
	// (202 Accepted) waiting for CARRY_KEY.
	body := CreateAgentRequest{Name: "eg-carry-agent", ProjectID: project.ID, Task: "t", GatherEnv: true}
	rec1 := doRequest(t, srv, http.MethodPost, "/api/v1/agents", body)
	require.Equal(t, http.StatusAccepted, rec1.Code, rec1.Body.String())
	require.NotNil(t, disp.capturedAgent)
	oldID := disp.capturedAgent.ID

	// User sets an agent-scoped secret on the provisioning agent before retrying.
	_, _, err := spy.Set(ctx, &secret.SetSecretInput{
		Name:          "CARRY_KEY",
		Value:         "super-secret-carry-val",
		SecretType:    secret.TypeFile,
		Target:        "/tmp/carry.pem",
		Scope:         secret.ScopeAgent,
		ScopeID:       oldID,
		ProjectID:     project.ID,
		Description:   "carry over across env-gather recreate",
		InjectionMode: store.InjectionModeAsNeeded,
	})
	require.NoError(t, err)

	// Second create with GatherEnv=true triggers Branch 3 (env-gather recreate):
	// snapshots CARRY_KEY, deletes oldID (DB rows + external backend.Delete),
	// creates a new agent under a new ID, and restores CARRY_KEY onto the new ID.
	disp.envReqs = nil
	rec2 := doRequest(t, srv, http.MethodPost, "/api/v1/agents", body)
	require.True(t, rec2.Code == http.StatusCreated || rec2.Code == http.StatusAccepted || rec2.Code == http.StatusOK,
		"expected 2xx on env-gather recreate, got %d: %s", rec2.Code, rec2.Body.String())
	require.NotNil(t, disp.capturedAgent)
	newID := disp.capturedAgent.ID
	require.NotEqual(t, oldID, newID, "recreate must assign a new agent ID")

	// New agent holds the carried-over secret with identical metadata and value.
	carried, err := spy.Get(ctx, "CARRY_KEY", secret.ScopeAgent, newID)
	require.NoError(t, err, "new agent must have the carried-over agent-scope secret")
	assert.Equal(t, "super-secret-carry-val", carried.Value)
	assert.Equal(t, secret.TypeFile, carried.SecretType)
	assert.Equal(t, "/tmp/carry.pem", carried.Target)
	assert.Equal(t, store.InjectionModeAsNeeded, carried.InjectionMode)
	assert.Equal(t, "carry over across env-gather recreate", carried.Description)

	// Nothing remains under the old agent ID in store or backend.
	_, err = s.GetSecret(ctx, "CARRY_KEY", store.ScopeAgent, oldID)
	assert.ErrorIs(t, err, store.ErrNotFound, "old agent ID must have no remaining secret rows")
	oldList, err := spy.List(ctx, secret.Filter{Scope: secret.ScopeAgent, ScopeID: oldID})
	require.NoError(t, err)
	assert.Empty(t, oldList)

	// Spy backend observed the post-commit external delete for the old agent ID.
	spy.mu.Lock()
	deleted := append([]string(nil), spy.deleted...)
	spy.mu.Unlock()
	assert.Contains(t, deleted, store.ScopeAgent+"/"+oldID+"/CARRY_KEY",
		"env-gather recreate must invoke post-commit backend.Delete for old agent ID")
}
