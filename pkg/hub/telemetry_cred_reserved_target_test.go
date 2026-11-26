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
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func enableAgentSecretsForReservedTest(t *testing.T, srv *Server, s store.Store) {
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

func TestReservedTelemetryCredFileTarget_HostedCreateAndPatchRejected(t *testing.T) {
	srv, s := testServer(t)
	srv.config.Workstation = false
	srv.SetSecretBackend(secret.NewLocalBackend(s, srv.hubID, "test-secret"))
	enableAgentSecretsForReservedTest(t, srv, s)

	ctx := context.Background()
	proj := &store.Project{
		ID:   tid("proj-tel-res"),
		Name: "tel-res-project",
		Slug: "tel-res-project",
	}
	require.NoError(t, s.CreateProject(ctx, proj))

	broker := &store.RuntimeBroker{
		ID:   tid("broker-tel-res"),
		Name: "tel-res-broker",
		Slug: "tel-res-broker",
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	valB64 := base64.StdEncoding.EncodeToString([]byte(`{"type":"service_account"}`))

	reservedTargets := []string{
		"~/.scion/telemetry-gcp-credentials.json",
		"~//.scion/./telemetry-gcp-credentials.json",
		"/home/scion/.scion/telemetry-gcp-credentials.json",
	}

	assertReservedTargetErr := func(t *testing.T, rec *httptest.ResponseRecorder) {
		t.Helper()
		require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
		var errResp ErrorResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
		assert.Equal(t, ErrCodeReservedTarget, errResp.Error.Code)
	}

	// 1. PUT /api/v1/secrets/{key} for non-hub scopes (user, project, runtime_broker).
	for _, target := range reservedTargets {
		for _, tc := range []struct {
			name    string
			scope   string
			scopeID string
		}{
			{"user", store.ScopeUser, ""},
			{"project", store.ScopeProject, proj.ID},
			{"broker", store.ScopeRuntimeBroker, broker.ID},
		} {
			t.Run("put_secrets_"+tc.name+"_"+target, func(t *testing.T) {
				rec := doRequest(t, srv, http.MethodPut, "/api/v1/secrets/MY_TELEM_CRED", SetSecretRequest{
					Value:   valB64,
					Type:    store.SecretTypeFile,
					Target:  target,
					Scope:   tc.scope,
					ScopeID: tc.scopeID,
				})
				assertReservedTargetErr(t, rec)
			})
		}
	}

	// 2. Scoped project endpoint: PUT /api/v1/projects/{id}/secrets/{key}.
	t.Run("put_project_scoped_endpoint", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+proj.ID+"/secrets/PROJ_TELEM_CRED", SetSecretRequest{
			Value:  valB64,
			Type:   store.SecretTypeFile,
			Target: "~/.scion/telemetry-gcp-credentials.json",
		})
		assertReservedTargetErr(t, rec)
	})

	// 3. PATCH updating an existing project file secret's target to the reserved path.
	t.Run("patch_project_secret_to_reserved_target", func(t *testing.T) {
		createRec := doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+proj.ID+"/secrets/PATCH_ME", SetSecretRequest{
			Value:  valB64,
			Type:   store.SecretTypeFile,
			Target: "~/.config/gcp/other.json",
		})
		require.Equal(t, http.StatusOK, createRec.Code, "body: %s", createRec.Body.String())

		patchRec := doRequest(t, srv, http.MethodPatch, "/api/v1/projects/"+proj.ID+"/secrets/PATCH_ME", UpdateSecretRequest{
			Target: "~/.scion/telemetry-gcp-credentials.json",
		})
		assertReservedTargetErr(t, patchRec)
	})

	// 4. Create-time inline agent secrets (validateCreateAgentSecrets).
	t.Run("create_agent_inline_secret_rejected", func(t *testing.T) {
		_, vErr := srv.validateCreateAgentSecrets([]AgentSecretInput{
			{
				Key:    "AGENT_TELEM_CRED",
				Value:  valB64,
				Type:   store.SecretTypeFile,
				Target: "~/.scion/telemetry-gcp-credentials.json",
			},
		})
		require.NotNil(t, vErr)
		assert.Equal(t, http.StatusBadRequest, vErr.status)
		assert.Equal(t, ErrCodeReservedTarget, vErr.code)
	})

	// 5. Agent self-service / secret request write constraints.
	t.Run("agent_secret_write_constraints_rejected", func(t *testing.T) {
		rec := httptest.NewRecorder()
		ok := srv.validateAgentSecretWriteConstraints(rec, ctx, tid("agent-1"), "AGENT_TELEM_CRED", store.SecretTypeFile, "~/.scion/telemetry-gcp-credentials.json")
		assert.False(t, ok)
		assertReservedTargetErr(t, rec)
	})

	// 6. Hub-scope scion-telemetry-gcp-credentials is still accepted.
	t.Run("hub_scope_telemetry_cred_allowed", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodPut, "/api/v1/secrets/scion-telemetry-gcp-credentials", SetSecretRequest{
			Value:  valB64,
			Type:   store.SecretTypeFile,
			Target: "~/.scion/telemetry-gcp-credentials.json",
			Scope:  store.ScopeHub,
		})
		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	})
}

func TestReservedTelemetryCredFileTarget_WorkstationModeUnchanged(t *testing.T) {
	srv, s := testServer(t)
	srv.config.Workstation = true
	srv.SetSecretBackend(secret.NewLocalBackend(s, srv.hubID, "test-secret"))
	enableAgentSecretsForReservedTest(t, srv, s)

	ctx := context.Background()
	proj := &store.Project{
		ID:   tid("proj-tel-ws"),
		Name: "tel-ws-project",
		Slug: "tel-ws-project",
	}
	require.NoError(t, s.CreateProject(ctx, proj))

	valB64 := base64.StdEncoding.EncodeToString([]byte(`{"type":"service_account"}`))

	// Project-scope file secret targeting the telemetry credential path succeeds in workstation mode.
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+proj.ID+"/secrets/WS_TELEM_CRED", SetSecretRequest{
		Value:  valB64,
		Type:   store.SecretTypeFile,
		Target: "~/.scion/telemetry-gcp-credentials.json",
	})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	// Inline agent-create secret also succeeds in workstation mode.
	validated, vErr := srv.validateCreateAgentSecrets([]AgentSecretInput{
		{
			Key:    "WS_AGENT_CRED",
			Value:  valB64,
			Type:   store.SecretTypeFile,
			Target: "~/.scion/telemetry-gcp-credentials.json",
		},
	})
	require.Nil(t, vErr)
	require.Len(t, validated, 1)
}

func TestReservedTelemetryCredFileTarget_ExistingRowsSkippedAtDispatchWithWarning(t *testing.T) {
	m := newLDFixture(t, ldOptions{})
	ctx := context.Background()

	// Seed hub-scope scion-telemetry-gcp-credentials (must still be delivered).
	_, _, err := m.backend.Set(ctx, &secret.SetSecretInput{
		Name:       "scion-telemetry-gcp-credentials",
		Value:      "hub-sa-json",
		SecretType: store.SecretTypeFile,
		Target:     "~/.scion/telemetry-gcp-credentials.json",
		Scope:      store.ScopeHub,
		ScopeID:    m.hubID,
	})
	require.NoError(t, err)

	// Seed pre-existing non-hub file secrets targeting the reserved path directly in backend.
	_, _, err = m.backend.Set(ctx, &secret.SetSecretInput{
		Name:       "LEGACY_PROJ_TELEM",
		Value:      "project-sa-json",
		SecretType: store.SecretTypeFile,
		Target:     "~/.scion/telemetry-gcp-credentials.json",
		Scope:      store.ScopeProject,
		ScopeID:    m.agent.ProjectID,
	})
	require.NoError(t, err)

	// Also seed a user-scope secret with the same well-known name to prove it
	// does not overwrite or displace the hub-scope telemetry credential.
	_, _, err = m.backend.Set(ctx, &secret.SetSecretInput{
		Name:       "scion-telemetry-gcp-credentials",
		Value:      "user-sa-json",
		SecretType: store.SecretTypeFile,
		Target:     "/home/scion/.scion/telemetry-gcp-credentials.json",
		Scope:      store.ScopeUser,
		ScopeID:    m.agent.OwnerID,
	})
	require.NoError(t, err)

	// Hosted mode: both resolveSecrets (create) and selectDeliverySecrets (start/restart)
	// skip the non-hub rows, emit warnings naming them, and still deliver the hub credential.
	m.d.SetHostedMode(true)

	t.Run("hosted_resolveSecrets", func(t *testing.T) {
		wCtx, _ := withDispatchWarnings(ctx)
		resolved, _, err := m.d.resolveSecrets(wCtx, m.agent)
		require.NoError(t, err)

		require.Len(t, resolved, 1, "expected only hub-scope telemetry credential, got %+v", resolved)
		assert.Equal(t, "scion-telemetry-gcp-credentials", resolved[0].Name)
		assert.Equal(t, store.ScopeHub, resolved[0].Source)
		assert.Equal(t, "hub-sa-json", resolved[0].Value)

		warnings := dispatchWarningsFromContext(wCtx)
		joined := strings.Join(warnings, "\n")
		assert.Contains(t, joined, "LEGACY_PROJ_TELEM")
		assert.Contains(t, joined, "scion-telemetry-gcp-credentials")
		assert.NotContains(t, joined, "project-sa-json")
		assert.NotContains(t, joined, "user-sa-json")
	})

	t.Run("hosted_selectDeliverySecrets", func(t *testing.T) {
		wCtx, _ := withDispatchWarnings(ctx)
		sel := newDeliverySelector(m.authz, m.agent, PurposeRestart)
		resolved, _, err := m.d.selectDeliverySecrets(wCtx, sel, m.agent)
		require.NoError(t, err)

		require.Len(t, resolved, 1, "expected only hub-scope telemetry credential, got %+v", resolved)
		assert.Equal(t, "scion-telemetry-gcp-credentials", resolved[0].Name)
		assert.Equal(t, store.ScopeHub, resolved[0].Source)
		assert.Equal(t, "hub-sa-json", resolved[0].Value)

		warnings := dispatchWarningsFromContext(wCtx)
		joined := strings.Join(warnings, "\n")
		assert.Contains(t, joined, "LEGACY_PROJ_TELEM")
		assert.Contains(t, joined, "scion-telemetry-gcp-credentials")
		assert.NotContains(t, joined, "project-sa-json")
		assert.NotContains(t, joined, "user-sa-json")
	})

	// Workstation mode: unchanged — non-hub rows are not skipped and no warning is emitted.
	m.d.SetHostedMode(false)

	t.Run("workstation_unchanged", func(t *testing.T) {
		wCtx, _ := withDispatchWarnings(ctx)
		resolved, _, err := m.d.resolveSecrets(wCtx, m.agent)
		require.NoError(t, err)
		assert.NotEmpty(t, resolved)
		assert.Empty(t, dispatchWarningsFromContext(wCtx))

		wCtx2, _ := withDispatchWarnings(ctx)
		sel := newDeliverySelector(m.authz, m.agent, PurposeRestart)
		resolved2, _, err := m.d.selectDeliverySecrets(wCtx2, sel, m.agent)
		require.NoError(t, err)
		assert.NotEmpty(t, resolved2)
		assert.Empty(t, dispatchWarningsFromContext(wCtx2))
	})
}
