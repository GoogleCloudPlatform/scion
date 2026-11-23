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
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type recordingP4SecretAuditLogger struct {
	*LogAuditLogger
	mu                sync.Mutex
	secretWriteEvents []*AgentSecretWriteEvent
}

func (l *recordingP4SecretAuditLogger) LogAgentSecretWriteEvent(ctx context.Context, e *AgentSecretWriteEvent) error {
	l.mu.Lock()
	cp := *e
	l.secretWriteEvents = append(l.secretWriteEvents, &cp)
	l.mu.Unlock()
	return l.LogAuditLogger.LogAgentSecretWriteEvent(ctx, e)
}

func setupP4AgentSecretsFixture(t *testing.T, enableExperiment bool) (*Server, store.Store, *store.User, *store.Project, *store.Agent, *store.Agent) {
	t.Helper()
	srv, s := testServer(t)
	if srv.GetSecretBackend() == nil {
		srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub", "test-signing-key-32-bytes-long!!"))
	}
	ctx := context.Background()

	if enableExperiment {
		raw := json.RawMessage(`{"overrides":{"hub.agent_secrets":true}}`)
		_, err := s.UpsertHubSetting(ctx, "experiments", raw, "test", -1, "managed")
		require.NoError(t, err)
		ops := NewOperationalSettings(s, emptyKoanf(), emptyKoanf())
		_, err = ops.Refresh(ctx)
		require.NoError(t, err)
		srv.SetOperationalSettings(ops)
		require.True(t, srv.experimentEnabled(experiments.AgentSecrets))
	}

	owner := &store.User{
		ID:          api.NewUUID(),
		Email:       "p4-owner@example.com",
		DisplayName: "P4 Owner",
		Role:        store.UserRoleMember,
		Status:      store.UserStatusActive,
	}
	require.NoError(t, s.CreateUser(ctx, owner))

	project := &store.Project{
		ID:      api.NewUUID(),
		Name:    "p4-proj",
		Slug:    "p4-proj",
		OwnerID: owner.ID,
	}
	require.NoError(t, s.CreateProject(ctx, project))

	ownerRD, err := s.GetRoleDefinitionByName(ctx, "project-owner", store.RoleScopeProject)
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

	agentX := &store.Agent{
		ID:        api.NewUUID(),
		Slug:      "p4-agent-x",
		Name:      "p4-agent-x",
		ProjectID: project.ID,
		OwnerID:   owner.ID,
		CreatedBy: owner.ID,
		Ancestry:  []string{owner.ID},
		Phase:     "running",
	}
	require.NoError(t, s.CreateAgent(ctx, agentX))

	agentY := &store.Agent{
		ID:        api.NewUUID(),
		Slug:      "p4-agent-y",
		Name:      "p4-agent-y",
		ProjectID: project.ID,
		OwnerID:   owner.ID,
		CreatedBy: owner.ID,
		Ancestry:  []string{owner.ID},
		Phase:     "running",
	}
	require.NoError(t, s.CreateAgent(ctx, agentY))

	return srv, s, owner, project, agentX, agentY
}

func doP4AsAgent(t *testing.T, srv *Server, agent *store.Agent, scopes []AgentTokenScope, method, target string, body any) *httptest.ResponseRecorder {
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

func TestAgentSelfWrite_PutReadOverwriteDelete(t *testing.T) {
	srv, _, _, _, agentX, _ := setupP4AgentSecretsFixture(t, true)
	scopes := []AgentTokenScope{ScopeAgentStatusUpdate}

	// PUT with scope=agent creates immediately (201, no pending step).
	rr := doP4AsAgent(t, srv, agentX, scopes, http.MethodPut,
		"/api/v1/agents/"+agentX.ID+"/secrets/MY_AGENT_KEY",
		AgentSetSecretRequest{
			Value: base64.StdEncoding.EncodeToString([]byte("secret-v1")),
			Scope: store.ScopeAgent,
		})
	require.Equal(t, http.StatusCreated, rr.Code, "body: %s", rr.Body.String())
	var putResp AgentSetSecretResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &putResp))
	assert.Equal(t, "MY_AGENT_KEY", putResp.Key)
	assert.Equal(t, store.ScopeAgent, putResp.Scope)
	assert.Equal(t, agentX.ID, putResp.ScopeID)

	// GET reads it back immediately.
	rr = doP4AsAgent(t, srv, agentX, scopes, http.MethodGet,
		"/api/v1/agents/"+agentX.ID+"/secrets/MY_AGENT_KEY?scope=agent", nil)
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	var getResp AgentGetSecretResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &getResp))
	decoded, err := base64.StdEncoding.DecodeString(getResp.Value)
	require.NoError(t, err)
	assert.Equal(t, "secret-v1", string(decoded))
	assert.Equal(t, store.ScopeAgent, getResp.Scope)

	// PUT again without force returns 409 Conflict.
	rr = doP4AsAgent(t, srv, agentX, scopes, http.MethodPut,
		"/api/v1/agents/"+agentX.ID+"/secrets/MY_AGENT_KEY",
		AgentSetSecretRequest{
			Value: base64.StdEncoding.EncodeToString([]byte("secret-v2")),
			Scope: store.ScopeAgent,
		})
	assert.Equal(t, http.StatusConflict, rr.Code)

	// PUT with force=true overwrites (204 No Content).
	rr = doP4AsAgent(t, srv, agentX, scopes, http.MethodPut,
		"/api/v1/agents/"+agentX.ID+"/secrets/MY_AGENT_KEY",
		AgentSetSecretRequest{
			Value: base64.StdEncoding.EncodeToString([]byte("secret-v2")),
			Scope: store.ScopeAgent,
			Force: true,
		})
	require.Equal(t, http.StatusNoContent, rr.Code, "body: %s", rr.Body.String())

	// GET reflects updated value.
	rr = doP4AsAgent(t, srv, agentX, scopes, http.MethodGet,
		"/api/v1/agents/"+agentX.ID+"/secrets/MY_AGENT_KEY?scope=agent", nil)
	require.Equal(t, http.StatusOK, rr.Code)
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &getResp))
	decoded, err = base64.StdEncoding.DecodeString(getResp.Value)
	require.NoError(t, err)
	assert.Equal(t, "secret-v2", string(decoded))

	// DELETE with scope=agent removes it (204 No Content).
	rr = doP4AsAgent(t, srv, agentX, scopes, http.MethodDelete,
		"/api/v1/agents/"+agentX.ID+"/secrets/MY_AGENT_KEY?scope=agent", nil)
	require.Equal(t, http.StatusNoContent, rr.Code, "body: %s", rr.Body.String())

	// GET now returns 404.
	rr = doP4AsAgent(t, srv, agentX, scopes, http.MethodGet,
		"/api/v1/agents/"+agentX.ID+"/secrets/MY_AGENT_KEY?scope=agent", nil)
	assert.Equal(t, http.StatusNotFound, rr.Code)

	// Second DELETE returns 404.
	rr = doP4AsAgent(t, srv, agentX, scopes, http.MethodDelete,
		"/api/v1/agents/"+agentX.ID+"/secrets/MY_AGENT_KEY?scope=agent", nil)
	assert.Equal(t, http.StatusNotFound, rr.Code)

	// DELETE with non-agent scope is rejected (400).
	for _, badScope := range []string{"", "project", "user"} {
		target := "/api/v1/agents/" + agentX.ID + "/secrets/MY_AGENT_KEY"
		if badScope != "" {
			target += "?scope=" + badScope
		}
		rr = doP4AsAgent(t, srv, agentX, scopes, http.MethodDelete, target, nil)
		assert.Equal(t, http.StatusBadRequest, rr.Code, "scope=%q body=%s", badScope, rr.Body.String())
	}
}

func TestAgentSelfWrite_CrossAgentAndUserAuthRefused(t *testing.T) {
	srv, _, owner, project, agentX, agentY := setupP4AgentSecretsFixture(t, true)
	scopes := []AgentTokenScope{ScopeAgentStatusUpdate}

	// Seed a secret on agentX.
	rr := doP4AsAgent(t, srv, agentX, scopes, http.MethodPut,
		"/api/v1/agents/"+agentX.ID+"/secrets/X_ONLY",
		AgentSetSecretRequest{
			Value: base64.StdEncoding.EncodeToString([]byte("x-secret")),
			Scope: store.ScopeAgent,
		})
	require.Equal(t, http.StatusCreated, rr.Code)

	// Agent Y (same project, same owner) cannot PUT or DELETE X's secrets.
	rr = doP4AsAgent(t, srv, agentY, scopes, http.MethodPut,
		"/api/v1/agents/"+agentX.ID+"/secrets/Y_INJECT",
		AgentSetSecretRequest{
			Value: base64.StdEncoding.EncodeToString([]byte("y-val")),
			Scope: store.ScopeAgent,
		})
	assert.Equal(t, http.StatusForbidden, rr.Code, "body: %s", rr.Body.String())

	rr = doP4AsAgent(t, srv, agentY, scopes, http.MethodDelete,
		"/api/v1/agents/"+agentX.ID+"/secrets/X_ONLY?scope=agent", nil)
	assert.Equal(t, http.StatusForbidden, rr.Code, "body: %s", rr.Body.String())

	// User session cannot use the agent route for PUT or DELETE.
	rr = doRequestAsUser(t, srv, owner, http.MethodPut,
		"/api/v1/agents/"+agentX.ID+"/secrets/USER_TRY",
		AgentSetSecretRequest{
			Value: base64.StdEncoding.EncodeToString([]byte("u-val")),
			Scope: store.ScopeAgent,
		})
	assert.Equal(t, http.StatusUnauthorized, rr.Code, "body: %s", rr.Body.String())

	rr = doRequestAsUser(t, srv, owner, http.MethodDelete,
		"/api/v1/agents/"+agentX.ID+"/secrets/X_ONLY?scope=agent", nil)
	assert.Equal(t, http.StatusUnauthorized, rr.Code, "body: %s", rr.Body.String())

	// User access token (UAT) with agent:update cannot use the agent route.
	uatKey, _, err := srv.uatService.CreateTokenWithParams(rs4MintContext(owner.ID), CreateTokenParams{
		UserID:   owner.ID,
		Name:     "p4-uat",
		Boundary: projectBoundary(project.ID),
		Scopes:   []string{"agent:update"},
	})
	require.NoError(t, err)

	doUAT := func(method, target string, body any) *httptest.ResponseRecorder {
		var buf bytes.Buffer
		if body != nil {
			require.NoError(t, json.NewEncoder(&buf).Encode(body))
		}
		req := httptest.NewRequest(method, target, &buf)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Authorization", "Bearer "+uatKey)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}

	rr = doUAT(http.MethodPut, "/api/v1/agents/"+agentX.ID+"/secrets/UAT_TRY",
		AgentSetSecretRequest{
			Value: base64.StdEncoding.EncodeToString([]byte("u-val")),
			Scope: store.ScopeAgent,
		})
	assert.Equal(t, http.StatusUnauthorized, rr.Code, "body: %s", rr.Body.String())

	rr = doUAT(http.MethodDelete, "/api/v1/agents/"+agentX.ID+"/secrets/X_ONLY?scope=agent", nil)
	assert.Equal(t, http.StatusUnauthorized, rr.Code, "body: %s", rr.Body.String())

	// Confirm X_ONLY is still intact.
	rr = doP4AsAgent(t, srv, agentX, scopes, http.MethodGet,
		"/api/v1/agents/"+agentX.ID+"/secrets/X_ONLY?scope=agent", nil)
	assert.Equal(t, http.StatusOK, rr.Code)
}

func TestAgentSelfWrite_StandingForbiddenRefused(t *testing.T) {
	srv, s, owner, _, agentX, _ := setupP4AgentSecretsFixture(t, true)
	scopes := []AgentTokenScope{ScopeAgentStatusUpdate}

	// Seed a secret while agentX is in good standing.
	rr := doP4AsAgent(t, srv, agentX, scopes, http.MethodPut,
		"/api/v1/agents/"+agentX.ID+"/secrets/BEFORE_SUSPEND",
		AgentSetSecretRequest{
			Value: base64.StdEncoding.EncodeToString([]byte("val")),
			Scope: store.ScopeAgent,
		})
	require.Equal(t, http.StatusCreated, rr.Code)

	// Suspend the owning user so agentStandingForbidden triggers.
	owner.Status = store.UserStatusSuspended
	require.NoError(t, s.UpdateUser(context.Background(), owner))

	rr = doP4AsAgent(t, srv, agentX, scopes, http.MethodPut,
		"/api/v1/agents/"+agentX.ID+"/secrets/AFTER_SUSPEND",
		AgentSetSecretRequest{
			Value: base64.StdEncoding.EncodeToString([]byte("val")),
			Scope: store.ScopeAgent,
		})
	assert.Equal(t, http.StatusForbidden, rr.Code, "body: %s", rr.Body.String())

	rr = doP4AsAgent(t, srv, agentX, scopes, http.MethodDelete,
		"/api/v1/agents/"+agentX.ID+"/secrets/BEFORE_SUSPEND?scope=agent", nil)
	assert.Equal(t, http.StatusForbidden, rr.Code, "body: %s", rr.Body.String())
}

func TestAgentSelfWrite_UserScopeOnlyDoesNotBlockAgentScope(t *testing.T) {
	srv, _, _, _, agentX, _ := setupP4AgentSecretsFixture(t, true)

	// Enable agent_secrets.user_scope_only alongside hub.agent_secrets.
	userScopeOnly := true
	srv.mu.Lock()
	srv.config.AgentSecretsUserScopeOnly = &userScopeOnly
	srv.mu.Unlock()
	require.True(t, srv.agentSecretsUserScopeOnly())
	require.True(t, srv.experimentEnabled(experiments.AgentSecrets))

	scopes := []AgentTokenScope{ScopeAgentStatusUpdate}

	// Project scope is still blocked by user_scope_only (403).
	rr := doP4AsAgent(t, srv, agentX, scopes, http.MethodPut,
		"/api/v1/agents/"+agentX.ID+"/secrets/PROJ_BLOCKED",
		AgentSetSecretRequest{
			Value: base64.StdEncoding.EncodeToString([]byte("val")),
			Scope: store.ScopeProject,
		})
	assert.Equal(t, http.StatusForbidden, rr.Code, "body: %s", rr.Body.String())

	// Agent scope is NOT blocked by user_scope_only.
	rr = doP4AsAgent(t, srv, agentX, scopes, http.MethodPut,
		"/api/v1/agents/"+agentX.ID+"/secrets/AGENT_ALLOWED",
		AgentSetSecretRequest{
			Value: base64.StdEncoding.EncodeToString([]byte("val")),
			Scope: store.ScopeAgent,
		})
	assert.Equal(t, http.StatusCreated, rr.Code, "body: %s", rr.Body.String())
}

func TestAgentSelfWrite_ReservedNamesAndAllowProgenyRejected(t *testing.T) {
	srv, _, _, _, agentX, _ := setupP4AgentSecretsFixture(t, true)
	scopes := []AgentTokenScope{ScopeAgentStatusUpdate}

	// Reserved env names rejected with 400.
	for _, key := range []string{"SCION_HUB_ENDPOINT", "GCE_METADATA_HOST", "systemone.instance.foo"} {
		rr := doP4AsAgent(t, srv, agentX, scopes, http.MethodPut,
			"/api/v1/agents/"+agentX.ID+"/secrets/"+key,
			AgentSetSecretRequest{
				Value: base64.StdEncoding.EncodeToString([]byte("val")),
				Scope: store.ScopeAgent,
			})
		assert.Equal(t, http.StatusBadRequest, rr.Code, "key=%s body=%s", key, rr.Body.String())
	}

	// allowProgeny=true at agent scope rejected with 400.
	tVal := true
	rr := doP4AsAgent(t, srv, agentX, scopes, http.MethodPut,
		"/api/v1/agents/"+agentX.ID+"/secrets/PROGENY_KEY",
		AgentSetSecretRequest{
			Value:        base64.StdEncoding.EncodeToString([]byte("val")),
			Scope:        store.ScopeAgent,
			AllowProgeny: &tVal,
		})
	assert.Equal(t, http.StatusBadRequest, rr.Code, "body: %s", rr.Body.String())
}

func TestAgentSelfWrite_ExperimentOffRefusesAgentScope(t *testing.T) {
	srv, _, _, _, agentX, _ := setupP4AgentSecretsFixture(t, false)
	scopes := []AgentTokenScope{ScopeAgentStatusUpdate}

	// PUT with scope=agent returns 400 invalid scope: agent.
	rr := doP4AsAgent(t, srv, agentX, scopes, http.MethodPut,
		"/api/v1/agents/"+agentX.ID+"/secrets/OFF_KEY",
		AgentSetSecretRequest{
			Value: base64.StdEncoding.EncodeToString([]byte("val")),
			Scope: store.ScopeAgent,
		})
	require.Equal(t, http.StatusBadRequest, rr.Code, "body: %s", rr.Body.String())
	var errResp ErrorResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &errResp))
	assert.Equal(t, ErrCodeInvalidRequest, errResp.Error.Code)
	assert.Equal(t, "invalid scope: agent", errResp.Error.Message)

	// DELETE with scope=agent returns 400 invalid scope: agent.
	rr = doP4AsAgent(t, srv, agentX, scopes, http.MethodDelete,
		"/api/v1/agents/"+agentX.ID+"/secrets/OFF_KEY?scope=agent", nil)
	require.Equal(t, http.StatusBadRequest, rr.Code, "body: %s", rr.Body.String())
	errResp = ErrorResponse{}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &errResp))
	assert.Equal(t, ErrCodeInvalidRequest, errResp.Error.Code)
	assert.Equal(t, "invalid scope: agent", errResp.Error.Message)

	// DELETE without scope=agent keeps pre-experiment 405 Method Not Allowed.
	rr = doP4AsAgent(t, srv, agentX, scopes, http.MethodDelete,
		"/api/v1/agents/"+agentX.ID+"/secrets/OFF_KEY", nil)
	assert.Equal(t, http.StatusMethodNotAllowed, rr.Code)

	// PUT with scope=project still works unchanged when experiment is off.
	rr = doP4AsAgent(t, srv, agentX, scopes, http.MethodPut,
		"/api/v1/agents/"+agentX.ID+"/secrets/PROJ_OK",
		AgentSetSecretRequest{
			Value: base64.StdEncoding.EncodeToString([]byte("proj-val")),
			Scope: store.ScopeProject,
		})
	assert.Equal(t, http.StatusCreated, rr.Code, "body: %s", rr.Body.String())
}

func TestAgentSelfWrite_AuditAndCanaryNeverLogged(t *testing.T) {
	srv, _, _, project, agentX, _ := setupP4AgentSecretsFixture(t, true)
	scopes := []AgentTokenScope{ScopeAgentStatusUpdate}

	var logBuf bytes.Buffer
	capturedLogger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	prevDefault := slog.Default()
	slog.SetDefault(capturedLogger)
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	auditor := &recordingP4SecretAuditLogger{
		LogAuditLogger: NewLogAuditLogger("[test-p4]", false),
	}
	srv.SetAuditLogger(auditor)

	canary := "canary-p4-secret-value-9f8e7d6c5b4a"
	canaryB64 := base64.StdEncoding.EncodeToString([]byte(canary))

	// Create (v1)
	rr := doP4AsAgent(t, srv, agentX, scopes, http.MethodPut,
		"/api/v1/agents/"+agentX.ID+"/secrets/CANARY_KEY",
		AgentSetSecretRequest{
			Value: canaryB64,
			Scope: store.ScopeAgent,
		})
	require.Equal(t, http.StatusCreated, rr.Code, "body: %s", rr.Body.String())

	// Overwrite (v2)
	canary2 := "canary-p4-secret-value-v2-1a2b3c4d"
	canary2B64 := base64.StdEncoding.EncodeToString([]byte(canary2))
	rr = doP4AsAgent(t, srv, agentX, scopes, http.MethodPut,
		"/api/v1/agents/"+agentX.ID+"/secrets/CANARY_KEY",
		AgentSetSecretRequest{
			Value: canary2B64,
			Scope: store.ScopeAgent,
			Force: true,
		})
	require.Equal(t, http.StatusNoContent, rr.Code)

	// Delete (v2)
	rr = doP4AsAgent(t, srv, agentX, scopes, http.MethodDelete,
		"/api/v1/agents/"+agentX.ID+"/secrets/CANARY_KEY?scope=agent", nil)
	require.Equal(t, http.StatusNoContent, rr.Code)

	auditor.mu.Lock()
	events := append([]*AgentSecretWriteEvent(nil), auditor.secretWriteEvents...)
	auditor.mu.Unlock()

	require.Len(t, events, 3)

	assert.Equal(t, "set", events[0].Operation)
	assert.Equal(t, agentX.ID, events[0].AgentID)
	assert.Equal(t, project.ID, events[0].ProjectID)
	assert.Equal(t, store.ScopeAgent, events[0].Scope)
	assert.Equal(t, agentX.ID, events[0].ScopeID)
	assert.Equal(t, "CANARY_KEY", events[0].SecretKey)
	assert.Equal(t, 1, events[0].Version)
	assert.True(t, events[0].Created)
	assert.True(t, events[0].Success)

	assert.Equal(t, "set", events[1].Operation)
	assert.Equal(t, 2, events[1].Version)
	assert.False(t, events[1].Created)
	assert.True(t, events[1].Success)

	assert.Equal(t, "delete", events[2].Operation)
	assert.Equal(t, agentX.ID, events[2].AgentID)
	assert.Equal(t, store.ScopeAgent, events[2].Scope)
	assert.Equal(t, agentX.ID, events[2].ScopeID)
	assert.Equal(t, "CANARY_KEY", events[2].SecretKey)
	assert.Equal(t, 2, events[2].Version)
	assert.True(t, events[2].Success)

	// Canary values (raw or base64) must never appear in audit events or captured logs.
	eventsJSON, err := json.Marshal(events)
	require.NoError(t, err)
	for _, needle := range []string{canary, canaryB64, canary2, canary2B64} {
		assert.NotContains(t, string(eventsJSON), needle, "canary leaked in audit events")
		assert.NotContains(t, logBuf.String(), needle, "canary leaked in captured slog output")
	}
}

type recordingSetSecretBackend struct {
	secret.SecretBackend
	mu     sync.Mutex
	inputs []*secret.SetSecretInput
}

func (b *recordingSetSecretBackend) Set(ctx context.Context, input *secret.SetSecretInput) (bool, *secret.SecretMeta, error) {
	b.mu.Lock()
	cp := *input
	b.inputs = append(b.inputs, &cp)
	b.mu.Unlock()
	return b.SecretBackend.Set(ctx, input)
}

func TestAgentSelfWrite_SetsProjectIDOnAgentScopeBackendInput(t *testing.T) {
	srv, _, _, project, agentX, _ := setupP4AgentSecretsFixture(t, true)
	rec := &recordingSetSecretBackend{SecretBackend: srv.GetSecretBackend()}
	srv.SetSecretBackend(rec)

	scopes := []AgentTokenScope{ScopeAgentStatusUpdate}
	rr := doP4AsAgent(t, srv, agentX, scopes, http.MethodPut,
		"/api/v1/agents/"+agentX.ID+"/secrets/PROJ_LABEL_KEY",
		AgentSetSecretRequest{
			Value: base64.StdEncoding.EncodeToString([]byte("val")),
			Scope: store.ScopeAgent,
		})
	require.Equal(t, http.StatusCreated, rr.Code, "body: %s", rr.Body.String())

	rec.mu.Lock()
	defer rec.mu.Unlock()
	require.Len(t, rec.inputs, 1)
	assert.Equal(t, store.ScopeAgent, rec.inputs[0].Scope)
	assert.Equal(t, agentX.ID, rec.inputs[0].ScopeID)
	assert.Equal(t, project.ID, rec.inputs[0].ProjectID, "agent self-write must populate ProjectID for GCP scion-project-id label attribution")
}

func TestAgentSelfWrite_FileTargetRestrictedToAgentHome(t *testing.T) {
	srv, _, _, _, agentX, _ := setupP4AgentSecretsFixture(t, true)
	scopes := []AgentTokenScope{ScopeAgentStatusUpdate}
	val := base64.StdEncoding.EncodeToString([]byte("file-contents"))

	// Agent-scope file target outside ~/ is refused with 400 validation error.
	rr := doP4AsAgent(t, srv, agentX, scopes, http.MethodPut,
		"/api/v1/agents/"+agentX.ID+"/secrets/ETC_FILE",
		AgentSetSecretRequest{
			Value:  val,
			Type:   store.SecretTypeFile,
			Target: "/etc/x",
			Scope:  store.ScopeAgent,
		})
	require.Equal(t, http.StatusBadRequest, rr.Code, "body: %s", rr.Body.String())
	var errResp ErrorResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &errResp))
	assert.Equal(t, ErrCodeValidationError, errResp.Error.Code)
	assert.Equal(t, "agent-written file secrets must target a path under the agent home (~/...)", errResp.Error.Message)

	// Agent-scope file target under ~/ is accepted.
	rr = doP4AsAgent(t, srv, agentX, scopes, http.MethodPut,
		"/api/v1/agents/"+agentX.ID+"/secrets/HOME_FILE",
		AgentSetSecretRequest{
			Value:  val,
			Type:   store.SecretTypeFile,
			Target: "~/x",
			Scope:  store.ScopeAgent,
		})
	require.Equal(t, http.StatusCreated, rr.Code, "body: %s", rr.Body.String())

	// Project-scope agent file write with /etc/x is unchanged.
	rr = doP4AsAgent(t, srv, agentX, scopes, http.MethodPut,
		"/api/v1/agents/"+agentX.ID+"/secrets/PROJ_FILE",
		AgentSetSecretRequest{
			Value:  val,
			Type:   store.SecretTypeFile,
			Target: "/etc/x",
			Scope:  store.ScopeProject,
		})
	require.Equal(t, http.StatusCreated, rr.Code, "body: %s", rr.Body.String())
}

type nilMetaSecretBackend struct {
	secret.SecretBackend
	deleted []string
}

func (b *nilMetaSecretBackend) GetMeta(_ context.Context, _, _, _ string) (*secret.SecretMeta, error) {
	return nil, nil
}

func (b *nilMetaSecretBackend) Delete(_ context.Context, name, _, _ string) error {
	b.deleted = append(b.deleted, name)
	return nil
}

func TestAgentSelfWrite_DeleteHandlesNilMeta(t *testing.T) {
	srv, _, _, _, agentX, _ := setupP4AgentSecretsFixture(t, true)
	stub := &nilMetaSecretBackend{SecretBackend: srv.GetSecretBackend()}
	srv.SetSecretBackend(stub)

	auditor := &recordingP4SecretAuditLogger{
		LogAuditLogger: NewLogAuditLogger("[test-p4-nilmeta]", false),
	}
	srv.SetAuditLogger(auditor)

	scopes := []AgentTokenScope{ScopeAgentStatusUpdate}
	rr := doP4AsAgent(t, srv, agentX, scopes, http.MethodDelete,
		"/api/v1/agents/"+agentX.ID+"/secrets/NIL_META_KEY?scope=agent", nil)
	require.Equal(t, http.StatusNoContent, rr.Code, "body: %s", rr.Body.String())
	assert.Equal(t, []string{"NIL_META_KEY"}, stub.deleted)

	auditor.mu.Lock()
	defer auditor.mu.Unlock()
	require.Len(t, auditor.secretWriteEvents, 1)
	assert.Equal(t, "delete", auditor.secretWriteEvents[0].Operation)
	assert.Equal(t, "NIL_META_KEY", auditor.secretWriteEvents[0].SecretKey)
	assert.Equal(t, 0, auditor.secretWriteEvents[0].Version)
	assert.True(t, auditor.secretWriteEvents[0].Success)
}
