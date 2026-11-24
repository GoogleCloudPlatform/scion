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
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type spyFetchValuesBackend struct {
	secret.SecretBackend
	mu           sync.Mutex
	fetchedNames []string
}

func (b *spyFetchValuesBackend) FetchValues(ctx context.Context, metas []secret.SecretMeta) (map[string]secret.FetchResult, error) {
	b.mu.Lock()
	for _, m := range metas {
		b.fetchedNames = append(b.fetchedNames, m.Name)
	}
	b.mu.Unlock()
	return b.SecretBackend.FetchValues(ctx, metas)
}

func TestOnDemandSecret_UserCRUDValidationAndResponseOmission(t *testing.T) {
	srv, s, owner, _, project, agentX, _ := setupAgentSecretsTestFixture(t, true)
	ctx := context.Background()

	// Make an admin user so hub/broker scope tests reach validation (not 403).
	admin := &store.User{
		ID:          api.NewUUID(),
		Email:       "admin-od@example.com",
		DisplayName: "Admin",
		Role:        store.UserRoleAdmin,
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, admin))
	broker := &store.RuntimeBroker{
		ID:     api.NewUUID(),
		Name:   "broker-od",
		Slug:   "broker-od",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	// 1. Create on_demand at project scope: succeeds; response omits target & injectionMode.
	rr := doRequestAsUser(t, srv, owner, http.MethodPut, "/api/v1/secrets/PROJ_OD", SetSecretRequest{
		Value:    "proj-od-val",
		Encoding: "raw",
		Type:     store.SecretTypeOnDemand,
		Scope:    store.ScopeProject,
		ScopeID:  project.ID,
	})
	require.Equal(t, http.StatusOK, rr.Code, "body=%s", rr.Body.String())
	var putRaw struct {
		Secret map[string]any `json:"secret"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &putRaw))
	assert.Equal(t, store.SecretTypeOnDemand, putRaw.Secret["type"])
	_, hasTarget := putRaw.Secret["target"]
	_, hasInjectionMode := putRaw.Secret["injectionMode"]
	assert.False(t, hasTarget, "on_demand PUT response must omit target")
	assert.False(t, hasInjectionMode, "on_demand PUT response must omit injectionMode")

	// GET single and GET list also omit target & injectionMode.
	rr = doRequestAsUser(t, srv, owner, http.MethodGet, "/api/v1/secrets/PROJ_OD?scope=project&scopeId="+project.ID, nil)
	require.Equal(t, http.StatusOK, rr.Code)
	var getRaw map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &getRaw))
	assert.Equal(t, store.SecretTypeOnDemand, getRaw["type"])
	_, hasTarget = getRaw["target"]
	_, hasInjectionMode = getRaw["injectionMode"]
	assert.False(t, hasTarget, "on_demand GET response must omit target")
	assert.False(t, hasInjectionMode, "on_demand GET response must omit injectionMode")

	// 2. Create on_demand at agent scope with target==key and injectionMode==as_needed: succeeds.
	rr = doRequestAsUser(t, srv, owner, http.MethodPut, "/api/v1/secrets/AGENT_OD", SetSecretRequest{
		Value:         "agent-od-val",
		Encoding:      "raw",
		Type:          store.SecretTypeOnDemand,
		Target:        "AGENT_OD",
		InjectionMode: store.InjectionModeAsNeeded,
		Scope:         store.ScopeAgent,
		ScopeID:       agentX.ID,
	})
	require.Equal(t, http.StatusOK, rr.Code, "body=%s", rr.Body.String())

	// 3. Disallowed scopes for on_demand: user, hub, runtime_broker -> 400 validation_error.
	for _, tc := range []struct {
		name    string
		user    *store.User
		scope   string
		scopeID string
	}{
		{"user", owner, store.ScopeUser, owner.ID},
		{"hub", admin, store.ScopeHub, ""},
		{"runtime_broker", admin, store.ScopeRuntimeBroker, broker.ID},
	} {
		t.Run("reject_scope_"+tc.name, func(t *testing.T) {
			rr := doRequestAsUser(t, srv, tc.user, http.MethodPut, "/api/v1/secrets/BAD_SCOPE_OD", SetSecretRequest{
				Value:    "v",
				Encoding: "raw",
				Type:     store.SecretTypeOnDemand,
				Scope:    tc.scope,
				ScopeID:  tc.scopeID,
			})
			assert.Equal(t, http.StatusBadRequest, rr.Code, "body=%s", rr.Body.String())
			var errResp ErrorResponse
			require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &errResp))
			assert.Equal(t, ErrCodeValidationError, errResp.Error.Code)
		})
	}

	// 4. Reject injectionMode: always, non-key target, allowProgeny: true, type: variable, type: internal.
	invalidPutCases := []struct {
		name        string
		req         SetSecretRequest
		msgContains string
	}{
		{
			name: "injectionMode_always",
			req: SetSecretRequest{
				Value: "v", Encoding: "raw", Type: store.SecretTypeOnDemand,
				InjectionMode: store.InjectionModeAlways, Scope: store.ScopeProject, ScopeID: project.ID,
			},
		},
		{
			name: "non_key_target",
			req: SetSecretRequest{
				Value: "v", Encoding: "raw", Type: store.SecretTypeOnDemand,
				Target: "OTHER_TARGET", Scope: store.ScopeProject, ScopeID: project.ID,
			},
		},
		{
			name: "allowProgeny_true",
			req: SetSecretRequest{
				Value: "v", Encoding: "raw", Type: store.SecretTypeOnDemand,
				AllowProgeny: true, Scope: store.ScopeProject, ScopeID: project.ID,
			},
		},
		{
			name: "type_variable_removed",
			req: SetSecretRequest{
				Value: "v", Encoding: "raw", Type: "variable",
				Scope: store.ScopeProject, ScopeID: project.ID,
			},
			msgContains: "on_demand",
		},
		{
			name: "type_internal_refused",
			req: SetSecretRequest{
				Value: "v", Encoding: "raw", Type: store.SecretTypeInternal,
				Scope: store.ScopeProject, ScopeID: project.ID,
			},
		},
	}
	for _, tc := range invalidPutCases {
		t.Run("put_"+tc.name, func(t *testing.T) {
			rr := doRequestAsUser(t, srv, owner, http.MethodPut, "/api/v1/secrets/TEST_INVALID", tc.req)
			assert.Equal(t, http.StatusBadRequest, rr.Code, "body=%s", rr.Body.String())
			var errResp ErrorResponse
			require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &errResp))
			assert.Equal(t, ErrCodeValidationError, errResp.Error.Code)
			if tc.msgContains != "" {
				assert.Contains(t, errResp.Error.Message, tc.msgContains)
			}
		})
	}

	// 5. PATCH validation for on_demand.
	// Reject injectionMode: always on existing on_demand secret.
	rr = doRequestAsUser(t, srv, owner, http.MethodPatch, "/api/v1/secrets/PROJ_OD?scope=project&scopeId="+project.ID, PatchSecretRequest{
		InjectionMode: store.InjectionModeAlways,
	})
	assert.Equal(t, http.StatusBadRequest, rr.Code)

	// Reject non-key target on existing on_demand secret.
	rr = doRequestAsUser(t, srv, owner, http.MethodPatch, "/api/v1/secrets/PROJ_OD?scope=project&scopeId="+project.ID, PatchSecretRequest{
		Target: "OTHER_TARGET",
	})
	assert.Equal(t, http.StatusBadRequest, rr.Code)

	// Reject type: variable on PATCH with message naming on_demand.
	rr = doRequestAsUser(t, srv, owner, http.MethodPatch, "/api/v1/secrets/PROJ_OD?scope=project&scopeId="+project.ID, PatchSecretRequest{
		Type: "variable",
	})
	assert.Equal(t, http.StatusBadRequest, rr.Code)
	var patchVarErr ErrorResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &patchVarErr))
	assert.Equal(t, ErrCodeValidationError, patchVarErr.Error.Code)
	assert.Contains(t, patchVarErr.Error.Message, "on_demand")

	// Reject changing a user-scope environment secret into on_demand.
	rr = doRequestAsUser(t, srv, owner, http.MethodPut, "/api/v1/secrets/USER_ENV", SetSecretRequest{
		Value: "v", Encoding: "raw", Type: store.SecretTypeEnvironment, Scope: store.ScopeUser, ScopeID: owner.ID,
	})
	require.Equal(t, http.StatusOK, rr.Code)
	rr = doRequestAsUser(t, srv, owner, http.MethodPatch, "/api/v1/secrets/USER_ENV?scope=user&scopeId="+owner.ID, PatchSecretRequest{
		Type: store.SecretTypeOnDemand,
	})
	assert.Equal(t, http.StatusBadRequest, rr.Code)

	// Allow changing a project-scope environment secret to on_demand; response omits target & injectionMode.
	rr = doRequestAsUser(t, srv, owner, http.MethodPut, "/api/v1/secrets/PROJ_ENV_TO_OD", SetSecretRequest{
		Value: "v", Encoding: "raw", Type: store.SecretTypeEnvironment, InjectionMode: store.InjectionModeAlways,
		Scope: store.ScopeProject, ScopeID: project.ID,
	})
	require.Equal(t, http.StatusOK, rr.Code)
	rr = doRequestAsUser(t, srv, owner, http.MethodPatch, "/api/v1/secrets/PROJ_ENV_TO_OD?scope=project&scopeId="+project.ID, PatchSecretRequest{
		Type: store.SecretTypeOnDemand,
	})
	require.Equal(t, http.StatusOK, rr.Code, "body=%s", rr.Body.String())
	var patchRaw map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &patchRaw))
	assert.Equal(t, store.SecretTypeOnDemand, patchRaw["type"])
	_, hasTarget = patchRaw["target"]
	_, hasInjectionMode = patchRaw["injectionMode"]
	assert.False(t, hasTarget)
	assert.False(t, hasInjectionMode)
}

func TestOnDemandSecret_EndToEndStartExcludesAndRuntimeReadWorks(t *testing.T) {
	srv, s, owner, _, project, agentX, _ := setupAgentSecretsTestFixture(t, true)
	ctx := context.Background()

	// 1. Create a project on_demand secret and a project environment (always) secret via /api/v1/secrets.
	rr := doRequestAsUser(t, srv, owner, http.MethodPut, "/api/v1/secrets/PROJECT_ON_DEMAND", SetSecretRequest{
		Value:    "super-secret-on-demand",
		Encoding: "raw",
		Type:     store.SecretTypeOnDemand,
		Scope:    store.ScopeProject,
		ScopeID:  project.ID,
	})
	require.Equal(t, http.StatusOK, rr.Code, "body=%s", rr.Body.String())

	rr = doRequestAsUser(t, srv, owner, http.MethodPut, "/api/v1/secrets/PROJECT_ENV", SetSecretRequest{
		Value:         "env-always-val",
		Encoding:      "raw",
		Type:          store.SecretTypeEnvironment,
		InjectionMode: store.InjectionModeAlways,
		Scope:         store.ScopeProject,
		ScopeID:       project.ID,
	})
	require.Equal(t, http.StatusOK, rr.Code, "body=%s", rr.Body.String())

	// 2. Start-time delivery selection: assert PROJECT_ON_DEMAND is absent from
	// resolved secrets AND FetchValues was never asked for it.
	spy := &spyFetchValuesBackend{SecretBackend: srv.GetSecretBackend()}
	d := NewHTTPAgentDispatcher(s, false, nil)
	d.SetSecretBackend(spy)
	d.SetAuthzService(srv.authzService)
	d.SetAgentSecretsEnabledProvider(func() bool { return srv.experimentEnabled(experiments.AgentSecrets) })

	sel := newDeliverySelector(srv.authzService, agentX, PurposeDispatch)
	resolved, asNeeded, skipped, err := d.selectDeliverySecretsWith(ctx, sel, agentX, nil)
	require.NoError(t, err)
	assert.Empty(t, skipped)
	assert.Empty(t, asNeeded)

	byName := make(map[string]ResolvedSecret, len(resolved))
	for _, r := range resolved {
		byName[r.Name] = r
	}
	assert.Contains(t, byName, "PROJECT_ENV", "environment always secret must be delivered")
	assert.Equal(t, "env-always-val", byName["PROJECT_ENV"].Value)
	assert.NotContains(t, byName, "PROJECT_ON_DEMAND", "on_demand secret must not be delivered at start")
	assert.Contains(t, spy.fetchedNames, "PROJECT_ENV")
	assert.NotContains(t, spy.fetchedNames, "PROJECT_ON_DEMAND", "FetchValues must never be asked for an on_demand secret")

	// 3. Runtime read via GET /api/v1/agents/{id}/secrets/{key} succeeds.
	scopes := []AgentTokenScope{ScopeAgentStatusUpdate, ScopeProjectSecretRead}
	rr = doAsAgentWithScopes(t, srv, agentX, scopes, http.MethodGet, "/api/v1/agents/"+agentX.ID+"/secrets/PROJECT_ON_DEMAND", nil)
	require.Equal(t, http.StatusOK, rr.Code, "body=%s", rr.Body.String())

	var getResp AgentGetSecretResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &getResp))
	decoded, err := base64.StdEncoding.DecodeString(getResp.Value)
	require.NoError(t, err)
	assert.Equal(t, "PROJECT_ON_DEMAND", getResp.Key)
	assert.Equal(t, "super-secret-on-demand", string(decoded))
	assert.Equal(t, store.SecretTypeOnDemand, getResp.Type)
	assert.Empty(t, getResp.Target)
	assert.Equal(t, store.ScopeProject, getResp.Scope)

	// 4. Runtime list via GET /api/v1/agents/{id}/secrets?scope=project includes it with empty target.
	rr = doAsAgentWithScopes(t, srv, agentX, scopes, http.MethodGet, "/api/v1/agents/"+agentX.ID+"/secrets?scope=project", nil)
	require.Equal(t, http.StatusOK, rr.Code, "body=%s", rr.Body.String())
	var listResp AgentListSecretsResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &listResp))
	var found *AgentSecretMeta
	for i := range listResp.Secrets {
		if listResp.Secrets[i].Key == "PROJECT_ON_DEMAND" {
			found = &listResp.Secrets[i]
			break
		}
	}
	require.NotNil(t, found, "on_demand secret must appear in agent secret list")
	assert.Equal(t, store.SecretTypeOnDemand, found.Type)
	assert.Empty(t, found.Target)
}

func TestOnDemandSecret_StartDeliveryShadowsLowerScopesAndBlocksProgeny(t *testing.T) {
	srv, s, owner, _, project, agentX, _ := setupAgentSecretsTestFixture(t, true)
	ctx := context.Background()

	sb := srv.GetSecretBackend()
	mustSet := func(in *secret.SetSecretInput) {
		t.Helper()
		_, _, err := sb.Set(ctx, in)
		require.NoError(t, err)
	}

	// Subtest 1: agent on_demand shadows project-scope and user-scope
	// environment/always secrets without fetching their values, while a
	// higher-scope deliverable row over a lower-scope on_demand row still wins.
	t.Run("shadows_project_and_user_scopes", func(t *testing.T) {
		// Case 1: agent on_demand over project environment/always.
		mustSet(&secret.SetSecretInput{
			Name: "SHADOW_PROJ", Value: "project-env-val", SecretType: store.SecretTypeEnvironment,
			InjectionMode: store.InjectionModeAlways, Scope: store.ScopeProject, ScopeID: project.ID,
		})
		mustSet(&secret.SetSecretInput{
			Name: "SHADOW_PROJ", Value: "agent-od-val", SecretType: store.SecretTypeOnDemand,
			Target: "SHADOW_PROJ", InjectionMode: store.InjectionModeAsNeeded, Scope: store.ScopeAgent, ScopeID: agentX.ID, ProjectID: project.ID,
		})

		// Case 2: agent on_demand over user-scope environment/always.
		mustSet(&secret.SetSecretInput{
			Name: "SHADOW_USER", Value: "user-env-val", SecretType: store.SecretTypeEnvironment,
			InjectionMode: store.InjectionModeAlways, Scope: store.ScopeUser, ScopeID: owner.ID,
		})
		mustSet(&secret.SetSecretInput{
			Name: "SHADOW_USER", Value: "agent-od-user-val", SecretType: store.SecretTypeOnDemand,
			Target: "SHADOW_USER", InjectionMode: store.InjectionModeAsNeeded, Scope: store.ScopeAgent, ScopeID: agentX.ID, ProjectID: project.ID,
		})

		// Control 1: lower-scope on_demand does NOT shadow a higher-scope deliverable row.
		mustSet(&secret.SetSecretInput{
			Name: "HIGHER_ENV_WINS", Value: "project-od-val", SecretType: store.SecretTypeOnDemand,
			Target: "HIGHER_ENV_WINS", InjectionMode: store.InjectionModeAsNeeded, Scope: store.ScopeProject, ScopeID: project.ID,
		})
		mustSet(&secret.SetSecretInput{
			Name: "HIGHER_ENV_WINS", Value: "agent-env-val", SecretType: store.SecretTypeEnvironment,
			InjectionMode: store.InjectionModeAlways, Scope: store.ScopeAgent, ScopeID: agentX.ID, ProjectID: project.ID,
		})

		// Control 2: unshadowed project environment/always secret is delivered.
		mustSet(&secret.SetSecretInput{
			Name: "CONTROL_ENV", Value: "control-val", SecretType: store.SecretTypeEnvironment,
			InjectionMode: store.InjectionModeAlways, Scope: store.ScopeProject, ScopeID: project.ID,
		})

		spy := &spyFetchValuesBackend{SecretBackend: sb}
		d := NewHTTPAgentDispatcher(s, false, nil)
		d.SetSecretBackend(spy)
		d.SetAuthzService(srv.authzService)
		d.SetAgentSecretsEnabledProvider(func() bool { return srv.experimentEnabled(experiments.AgentSecrets) })

		sel := newDeliverySelector(srv.authzService, agentX, PurposeDispatch)
		resolved, asNeeded, skipped, err := d.selectDeliverySecretsWith(ctx, sel, agentX, nil)
		require.NoError(t, err)
		assert.Empty(t, skipped)
		assert.Empty(t, asNeeded)

		byName := make(map[string]ResolvedSecret, len(resolved))
		for _, r := range resolved {
			byName[r.Name] = r
		}

		assert.Contains(t, byName, "CONTROL_ENV")
		assert.Equal(t, "control-val", byName["CONTROL_ENV"].Value)
		assert.Contains(t, byName, "HIGHER_ENV_WINS")
		assert.Equal(t, "agent-env-val", byName["HIGHER_ENV_WINS"].Value)
		assert.Equal(t, store.ScopeAgent, byName["HIGHER_ENV_WINS"].Source)

		for _, shadowedKey := range []string{"SHADOW_PROJ", "SHADOW_USER"} {
			assert.NotContains(t, byName, shadowedKey, "%s must be shadowed and absent from delivered secrets", shadowedKey)
			assert.NotContains(t, spy.fetchedNames, shadowedKey, "%s must not be passed to FetchValues", shadowedKey)
		}
	})

	// Subtest 2: agent on_demand blocks progeny fill without fetching either
	// value. OwnerID is empty on progenyChild so ScopeUser is not queried
	// directly and only the progeny path can supply owner's allowProgeny rows.
	t.Run("blocks_progeny_fill", func(t *testing.T) {
		progenyChild := &store.Agent{
			ID:            api.NewUUID(),
			Slug:          "progeny-od-shadow",
			Name:          "progeny-od-shadow",
			ProjectID:     project.ID,
			CreatedBy:     agentX.ID,
			Ancestry:      []string{owner.ID, agentX.ID},
			Phase:         "running",
			AppliedConfig: &store.AgentAppliedConfig{AgentRole: string(AgentRoleFull)},
		}
		require.NoError(t, s.CreateAgent(ctx, progenyChild))

		mustSet(&secret.SetSecretInput{
			Name: "SHADOW_PROGENY", Value: "progeny-env-val", SecretType: store.SecretTypeEnvironment,
			InjectionMode: store.InjectionModeAlways, Scope: store.ScopeUser, ScopeID: owner.ID,
			AllowProgeny: true, CreatedBy: owner.ID,
		})
		mustSet(&secret.SetSecretInput{
			Name: "SHADOW_PROGENY", Value: "agent-od-progeny-val", SecretType: store.SecretTypeOnDemand,
			Target: "SHADOW_PROGENY", InjectionMode: store.InjectionModeAsNeeded, Scope: store.ScopeAgent, ScopeID: progenyChild.ID, ProjectID: project.ID,
		})

		// Unshadowed progeny secret proves the progeny path is active.
		mustSet(&secret.SetSecretInput{
			Name: "CONTROL_PROGENY", Value: "progeny-control-val", SecretType: store.SecretTypeEnvironment,
			InjectionMode: store.InjectionModeAlways, Scope: store.ScopeUser, ScopeID: owner.ID,
			AllowProgeny: true, CreatedBy: owner.ID,
		})

		spy := &spyFetchValuesBackend{SecretBackend: sb}
		d := NewHTTPAgentDispatcher(s, false, nil)
		d.SetSecretBackend(spy)
		d.SetAuthzService(srv.authzService)
		d.SetAgentSecretsEnabledProvider(func() bool { return srv.experimentEnabled(experiments.AgentSecrets) })

		sel := newDeliverySelector(srv.authzService, progenyChild, PurposeDispatch)
		resolved, asNeeded, skipped, err := d.selectDeliverySecretsWith(ctx, sel, progenyChild, nil)
		require.NoError(t, err)
		assert.Empty(t, skipped)
		assert.Empty(t, asNeeded)

		byName := make(map[string]ResolvedSecret, len(resolved))
		for _, r := range resolved {
			byName[r.Name] = r
		}

		assert.Contains(t, byName, "CONTROL_PROGENY", "unshadowed progeny secret must be delivered")
		assert.Equal(t, "progeny-control-val", byName["CONTROL_PROGENY"].Value)
		assert.NotContains(t, byName, "SHADOW_PROGENY", "SHADOW_PROGENY must be blocked by agent on_demand")
		assert.NotContains(t, spy.fetchedNames, "SHADOW_PROGENY", "SHADOW_PROGENY must not be passed to FetchValues")
	})
}
