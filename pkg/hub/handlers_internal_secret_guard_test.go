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
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInternalSecretGuard_HiddenAndRefusedAcrossScopes(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	srv.SetSecretBackend(secret.NewLocalBackendWithCipher(s, srv.hubID, srv.cipher))

	project := &store.Project{
		ID:      tid("proj_isg"),
		Name:    "Internal Secret Guard Project",
		Slug:    "internal-secret-guard-project",
		OwnerID: DevUserID,
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))

	agent := &store.Agent{
		ID:           tid("agent_isg"),
		Slug:         "isg-agent",
		Name:         "ISG Agent",
		ProjectID:    project.ID,
		OwnerID:      DevUserID,
		Phase:        string(state.PhaseRunning),
		StateVersion: 1,
		Created:      time.Now(),
		Updated:      time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))
	agentToken := srv.generateRecordedAgentToken(t, agent.ID, project.ID, []AgentTokenScope{ScopeProjectSecretRead}, []string{DevUserID})

	epID := tid("ep_isg")
	hubInternalRow, err := srv.newWebhookEndpointSecretRow(project.ID, epID, 1, "whsec-plaintext-value", DevUserID)
	require.NoError(t, err)
	require.NoError(t, s.CreateSecret(ctx, hubInternalRow))

	projInternalKey := "INTERNAL_PROJECT_KEY"
	require.NoError(t, s.CreateSecret(ctx, &store.Secret{
		ID:             tid("sec_proj_int"),
		Key:            projInternalKey,
		EncryptedValue: "sealed-proj",
		Scope:          store.ScopeProject,
		ScopeID:        project.ID,
		SecretType:     store.SecretTypeInternal,
	}))

	userInternalKey := "conduit_grant." + DevUserID + ".g1"
	require.NoError(t, s.CreateSecret(ctx, &store.Secret{
		ID:             tid("sec_user_int"),
		Key:            userInternalKey,
		EncryptedValue: "sealed-user",
		Scope:          store.ScopeUser,
		ScopeID:        DevUserID,
		SecretType:     store.SecretTypeInternal,
	}))

	// Seed one visible variable secret in each scope to confirm normal rows remain visible.
	for _, sc := range []struct {
		key     string
		scope   string
		scopeID string
	}{
		{"VISIBLE_HUB", store.ScopeHub, srv.hubID},
		{"VISIBLE_PROJ", store.ScopeProject, project.ID},
		{"VISIBLE_USER", store.ScopeUser, DevUserID},
	} {
		_, _, err := srv.secretBackend.Set(ctx, &secret.SetSecretInput{
			Name:       sc.key,
			Value:      "ok",
			SecretType: store.SecretTypeEnvironment,
			Target:     sc.key,
			Scope:      sc.scope,
			ScopeID:    sc.scopeID,
			CreatedBy:  DevUserID,
			UpdatedBy:  DevUserID,
		})
		require.NoError(t, err)
	}

	assertConflictInternal := func(t *testing.T, code int, body []byte) {
		t.Helper()
		require.Equal(t, http.StatusConflict, code, string(body))
		var errResp ErrorResponse
		require.NoError(t, json.Unmarshal(body, &errResp), string(body))
		assert.Equal(t, ErrCodeInternalSecret, errResp.Error.Code)
	}

	t.Run("hub scope list/get hides internal and put/patch/delete returns 409 internal_secret", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodGet, "/api/v1/secrets?scope=hub", nil)
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, []string{"VISIBLE_HUB"}, listedSecretNames(t, rec.Body.String()))

		escaped := url.PathEscape(hubInternalRow.Key)
		rec = doRequest(t, srv, http.MethodGet, "/api/v1/secrets/"+escaped+"?scope=hub", nil)
		assert.Equal(t, http.StatusNotFound, rec.Code)

		rec = doRequest(t, srv, http.MethodPut, "/api/v1/secrets/"+escaped, map[string]any{
			"value":    "overwrite",
			"encoding": "raw",
			"type":     "variable",
			"scope":    "hub",
		})
		assertConflictInternal(t, rec.Code, rec.Body.Bytes())

		desc := "tampered"
		rec = doRequest(t, srv, http.MethodPatch, "/api/v1/secrets/"+escaped+"?scope=hub", PatchSecretRequest{Description: &desc})
		assertConflictInternal(t, rec.Code, rec.Body.Bytes())

		rec = doRequest(t, srv, http.MethodDelete, "/api/v1/secrets/"+escaped+"?scope=hub", nil)
		assertConflictInternal(t, rec.Code, rec.Body.Bytes())

		stillThere, err := s.GetSecret(ctx, hubInternalRow.Key, store.ScopeHub, srv.hubID)
		require.NoError(t, err)
		plain, err := srv.unsealWebhookEndpointSecret(stillThere.EncryptedValue)
		require.NoError(t, err)
		assert.Equal(t, "whsec-plaintext-value", plain)
	})

	t.Run("project scope list/get hides internal and user/agent mutations return 409 internal_secret", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+project.ID+"/secrets", nil)
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, []string{"VISIBLE_PROJ"}, listedSecretNames(t, rec.Body.String()))

		rec = doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+project.ID+"/secrets/"+projInternalKey, nil)
		assert.Equal(t, http.StatusNotFound, rec.Code)

		rec = doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/secrets/"+projInternalKey, map[string]any{
			"value":    "overwrite",
			"encoding": "raw",
			"type":     "variable",
		})
		assertConflictInternal(t, rec.Code, rec.Body.Bytes())

		desc := "tampered"
		rec = doRequest(t, srv, http.MethodPatch, "/api/v1/projects/"+project.ID+"/secrets/"+projInternalKey, PatchSecretRequest{Description: &desc})
		assertConflictInternal(t, rec.Code, rec.Body.Bytes())

		rec = doRequest(t, srv, http.MethodDelete, "/api/v1/projects/"+project.ID+"/secrets/"+projInternalKey, nil)
		assertConflictInternal(t, rec.Code, rec.Body.Bytes())

		// Env var promotion and plain env var delete also preserve the internal secret.
		rec = doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/env/"+projInternalKey, map[string]any{
			"value":  "overwrite",
			"secret": true,
		})
		assertConflictInternal(t, rec.Code, rec.Body.Bytes())

		rec = doRequest(t, srv, http.MethodDelete, "/api/v1/projects/"+project.ID+"/env/"+projInternalKey, nil)
		assert.Equal(t, http.StatusNotFound, rec.Code)

		// Agent force-PUT at project scope is also refused with 409 internal_secret.
		rec = doRequestWithAgentToken(t, srv, http.MethodPut, "/api/v1/agents/"+agent.ID+"/secrets/"+projInternalKey, map[string]any{
			"value":    "overwrite",
			"encoding": "raw",
			"type":     "variable",
			"force":    true,
			"scope":    "project",
		}, agentToken)
		assertConflictInternal(t, rec.Code, rec.Body.Bytes())

		stillThere, err := s.GetSecret(ctx, projInternalKey, store.ScopeProject, project.ID)
		require.NoError(t, err)
		assert.Equal(t, "sealed-proj", stillThere.EncryptedValue)
	})

	t.Run("user scope list/includePending/get hides internal and user/agent mutations return 409 internal_secret", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodGet, "/api/v1/secrets?scope=user", nil)
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, []string{"VISIBLE_USER"}, listedSecretNames(t, rec.Body.String()))

		rec = doRequest(t, srv, http.MethodGet, "/api/v1/secrets?scope=user&includePending=true", nil)
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, []string{"VISIBLE_USER"}, listedSecretNames(t, rec.Body.String()))

		escaped := url.PathEscape(userInternalKey)
		rec = doRequest(t, srv, http.MethodGet, "/api/v1/secrets/"+escaped+"?scope=user", nil)
		assert.Equal(t, http.StatusNotFound, rec.Code)

		rec = doRequest(t, srv, http.MethodPut, "/api/v1/secrets/"+escaped, map[string]any{
			"value":    "overwrite",
			"encoding": "raw",
			"type":     "variable",
			"scope":    "user",
		})
		assertConflictInternal(t, rec.Code, rec.Body.Bytes())

		rec = doRequest(t, srv, http.MethodDelete, "/api/v1/secrets/"+escaped+"?scope=user", nil)
		assertConflictInternal(t, rec.Code, rec.Body.Bytes())

		rec = doRequestWithAgentToken(t, srv, http.MethodPut, "/api/v1/agents/"+agent.ID+"/secrets/"+escaped, map[string]any{
			"value":    "overwrite",
			"encoding": "raw",
			"type":     "variable",
			"scope":    "user",
		}, agentToken)
		assertConflictInternal(t, rec.Code, rec.Body.Bytes())
	})
}

func TestWebhookEndpointSecretHelpers_RoundTrip(t *testing.T) {
	srv, _ := testServer(t)
	plain, err := generateWebhookEndpointSecret()
	require.NoError(t, err)
	raw, err := base64.RawURLEncoding.DecodeString(plain)
	require.NoError(t, err)
	assert.Len(t, raw, webhookEndpointSecretBytes)

	row, err := srv.newWebhookEndpointSecretRow("proj-1", "ep-1", 1, plain, "user-1")
	require.NoError(t, err)
	assert.Equal(t, "webhook_endpoint.proj-1.ep-1.v1", row.Key)
	assert.Equal(t, store.ScopeHub, row.Scope)
	assert.Equal(t, srv.hubID, row.ScopeID)
	assert.Equal(t, store.SecretTypeInternal, row.SecretType)

	unsealed, err := srv.unsealWebhookEndpointSecret(row.EncryptedValue)
	require.NoError(t, err)
	assert.Equal(t, plain, unsealed)
}

func TestWebhookEndpointReservedSecretPrefix_RefusedAcrossGenericWrites(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	srv.SetSecretBackend(secret.NewLocalBackendWithCipher(s, srv.hubID, srv.cipher))

	project := &store.Project{
		ID:      tid("proj_werp"),
		Name:    "Webhook Reserved Prefix Project",
		Slug:    "webhook-reserved-prefix-project",
		OwnerID: DevUserID,
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))

	agent := &store.Agent{
		ID:           tid("agent_werp"),
		Slug:         "werp-agent",
		Name:         "WERP Agent",
		ProjectID:    project.ID,
		OwnerID:      DevUserID,
		Phase:        string(state.PhaseRunning),
		StateVersion: 1,
		Created:      time.Now(),
		Updated:      time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))
	agentToken := srv.generateRecordedAgentToken(t, agent.ID, project.ID, []AgentTokenScope{ScopeProjectSecretRead}, []string{DevUserID})

	// Unoccupied future rotation key (.v2) and sanitized/case-folded colliding forms.
	reservedKeys := []string{
		store.WebhookEndpointSecretKey(project.ID, tid("ep_werp"), 2),
		"webhook_endpoint.custom.key",
		"WEBHOOK_ENDPOINT." + project.ID + ".v2",
		"webhook_endpoint-" + project.ID + "-v2",
	}

	assertValidationReserved := func(t *testing.T, code int, body []byte) {
		t.Helper()
		require.Equal(t, http.StatusBadRequest, code, string(body))
		var errResp ErrorResponse
		require.NoError(t, json.Unmarshal(body, &errResp), string(body))
		assert.Equal(t, ErrCodeValidationError, errResp.Error.Code)
		assert.Contains(t, errResp.Error.Message, "webhook_endpoint.")
	}

	for _, key := range reservedKeys {
		escaped := url.PathEscape(key)

		// Global PUT /api/v1/secrets/{key} (hub, project, user scopes)
		for _, sc := range []struct {
			scope   string
			scopeID string
		}{
			{store.ScopeHub, ""},
			{store.ScopeProject, project.ID},
			{store.ScopeUser, ""},
		} {
			rec := doRequest(t, srv, http.MethodPut, "/api/v1/secrets/"+escaped, map[string]any{
				"value":    "planted",
				"encoding": "raw",
				"type":     "variable",
				"scope":    sc.scope,
				"scopeId":  sc.scopeID,
			})
			assertValidationReserved(t, rec.Code, rec.Body.Bytes())
		}

		// Scoped PUT /api/v1/projects/{id}/secrets/{key}
		rec := doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/secrets/"+escaped, map[string]any{
			"value":    "planted",
			"encoding": "raw",
			"type":     "variable",
		})
		assertValidationReserved(t, rec.Code, rec.Body.Bytes())

		// Env promotion and plain env PUT /api/v1/projects/{id}/env/{key}
		for _, isSecret := range []bool{true, false} {
			rec = doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/env/"+escaped, map[string]any{
				"value":  "planted",
				"secret": isSecret,
			})
			assertValidationReserved(t, rec.Code, rec.Body.Bytes())
		}

		// Agent PUT /api/v1/agents/{id}/secrets/{key} (project scope and pending user scope)
		for _, agScope := range []string{store.ScopeProject, store.ScopeUser} {
			rec = doRequestWithAgentToken(t, srv, http.MethodPut, "/api/v1/agents/"+agent.ID+"/secrets/"+escaped, map[string]any{
				"value":    "planted",
				"encoding": "raw",
				"type":     "variable",
				"scope":    agScope,
			}, agentToken)
			assertValidationReserved(t, rec.Code, rec.Body.Bytes())
		}

		// Confirm nothing was stored in hub, project, or user scope.
		_, err := s.GetSecret(ctx, key, store.ScopeHub, srv.hubID)
		assert.ErrorIs(t, err, store.ErrNotFound)
		_, err = s.GetSecret(ctx, key, store.ScopeProject, project.ID)
		assert.ErrorIs(t, err, store.ErrNotFound)
		_, err = s.GetSecret(ctx, key, store.ScopeUser, DevUserID)
		assert.ErrorIs(t, err, store.ErrNotFound)
	}

	// Non-reserved prefixes (such as ordinary user env/secret names like WEBHOOK_ENDPOINT_URL) still work normally.
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/secrets/WEBHOOK_ENDPOINT_URL", map[string]any{
		"value":    "https://example.com/hook",
		"encoding": "raw",
		"type":     "variable",
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}
