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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOnDemandSecret_P2_TargetShadowing_StartCreateAndGather verifies that an
// on_demand secret shadows lower-scope AND progeny environment secrets that
// have a different key name but the same effective env target, on all three
// Hub delivery paths: start/restart (selectDeliverySecretsWith), create/provision
// (resolveSecrets), and harness gather (resolveAsNeededForKeys).
func TestOnDemandSecret_P2_TargetShadowing_StartCreateAndGather(t *testing.T) {
	ctx := context.Background()
	f := newChainFixture(t, "od-p2-target-shadow")
	enableAgentSecretsExperimentForTest(t, f.srv, f.store)
	if f.srv.GetSecretBackend() == nil {
		f.srv.SetSecretBackend(secret.NewLocalBackend(f.store, "test-hub", "test-signing-key-32-bytes-long!!"))
	}
	parent, _ := f.sessionParent(t, "od-p2-parent")
	child, _ := f.childOf(t, parent, "od-p2-child")
	sb := f.srv.GetSecretBackend()

	mustSet := func(in *secret.SetSecretInput) {
		t.Helper()
		_, _, err := sb.Set(ctx, in)
		require.NoError(t, err)
	}

	// Lower-scope project env secret with DIFFERENT name (PROJ_GH) targeting GITHUB_TOKEN.
	mustSet(&secret.SetSecretInput{
		Name: "PROJ_GH", Value: "proj-gh-always", SecretType: store.SecretTypeEnvironment,
		Target: "GITHUB_TOKEN", InjectionMode: store.InjectionModeAlways,
		Scope: store.ScopeProject, ScopeID: f.proj.ID,
	})
	// Lower-scope project as_needed env secret with DIFFERENT name (PROJ_GATHER_GH) targeting GATHER_TOK.
	mustSet(&secret.SetSecretInput{
		Name: "PROJ_GATHER_GH", Value: "proj-gather-val", SecretType: store.SecretTypeEnvironment,
		Target: "GATHER_TOK", InjectionMode: store.InjectionModeAsNeeded,
		Scope: store.ScopeProject, ScopeID: f.proj.ID,
	})
	// Progeny env secrets with DIFFERENT names targeting GITHUB_TOKEN and GATHER_TOK.
	mustSet(&secret.SetSecretInput{
		Name: "PARENT_GH", Value: "progeny-gh-always", SecretType: store.SecretTypeEnvironment,
		Target: "GITHUB_TOKEN", InjectionMode: store.InjectionModeAlways,
		Scope: store.ScopeUser, ScopeID: f.creator.ID, AllowProgeny: true, CreatedBy: f.creator.ID,
	})
	mustSet(&secret.SetSecretInput{
		Name: "PARENT_GATHER_GH", Value: "progeny-gather-val", SecretType: store.SecretTypeEnvironment,
		Target: "GATHER_TOK", InjectionMode: store.InjectionModeAsNeeded,
		Scope: store.ScopeUser, ScopeID: f.creator.ID, AllowProgeny: true, CreatedBy: f.creator.ID,
	})
	// Agent-scope on_demand secrets occupying GITHUB_TOKEN and GATHER_TOK.
	mustSet(&secret.SetSecretInput{
		Name: "GITHUB_TOKEN", Value: "agent-od-gh", SecretType: store.SecretTypeOnDemand,
		Target: "GITHUB_TOKEN", InjectionMode: store.InjectionModeAsNeeded,
		Scope: store.ScopeAgent, ScopeID: child.ID, ProjectID: f.proj.ID,
	})
	mustSet(&secret.SetSecretInput{
		Name: "GATHER_TOK", Value: "agent-od-gather", SecretType: store.SecretTypeOnDemand,
		Target: "GATHER_TOK", InjectionMode: store.InjectionModeAsNeeded,
		Scope: store.ScopeAgent, ScopeID: child.ID, ProjectID: f.proj.ID,
	})

	// Active controls (unshadowed project + progeny secrets for always and as_needed).
	mustSet(&secret.SetSecretInput{
		Name: "CTRL_PROJ_ALWAYS", Value: "ctrl-proj-always", SecretType: store.SecretTypeEnvironment,
		Target: "CTRL_PROJ_ALWAYS", InjectionMode: store.InjectionModeAlways,
		Scope: store.ScopeProject, ScopeID: f.proj.ID,
	})
	mustSet(&secret.SetSecretInput{
		Name: "CTRL_PROGENY_ALWAYS", Value: "ctrl-progeny-always", SecretType: store.SecretTypeEnvironment,
		Target: "CTRL_PROGENY_ALWAYS", InjectionMode: store.InjectionModeAlways,
		Scope: store.ScopeUser, ScopeID: f.creator.ID, AllowProgeny: true, CreatedBy: f.creator.ID,
	})
	mustSet(&secret.SetSecretInput{
		Name: "CTRL_PROJ_GATHER", Value: "ctrl-proj-gather", SecretType: store.SecretTypeEnvironment,
		Target: "CTRL_PROJ_GATHER", InjectionMode: store.InjectionModeAsNeeded,
		Scope: store.ScopeProject, ScopeID: f.proj.ID,
	})
	mustSet(&secret.SetSecretInput{
		Name: "CTRL_PROGENY_GATHER", Value: "ctrl-progeny-gather", SecretType: store.SecretTypeEnvironment,
		Target: "CTRL_PROGENY_GATHER", InjectionMode: store.InjectionModeAsNeeded,
		Scope: store.ScopeUser, ScopeID: f.creator.ID, AllowProgeny: true, CreatedBy: f.creator.ID,
	})

	// Clear OwnerID on a copy so ScopeUser is skipped and progeny fill is exercised.
	childNoOwner := *child
	childNoOwner.OwnerID = ""

	spy := &spyFetchValuesBackend{SecretBackend: sb}
	d := NewHTTPAgentDispatcher(f.store, false, nil)
	d.SetSecretBackend(spy)
	d.SetAuthzService(f.srv.authzService)
	d.SetAgentSecretsEnabledProvider(func() bool { return f.srv.experimentEnabled(experiments.AgentSecrets) })

	// 1. Start/restart path (selectDeliverySecretsWith).
	sel := newDeliverySelector(f.srv.authzService, &childNoOwner, PurposeDispatch)
	resolved, _, skipped, err := d.selectDeliverySecretsWith(ctx, sel, &childNoOwner, nil)
	require.NoError(t, err)
	assert.Empty(t, skipped)
	byName := make(map[string]ResolvedSecret, len(resolved))
	for _, r := range resolved {
		byName[r.Name] = r
	}
	assert.Contains(t, byName, "CTRL_PROJ_ALWAYS")
	assert.Contains(t, byName, "CTRL_PROGENY_ALWAYS")
	for _, forbidden := range []string{"GITHUB_TOKEN", "PROJ_GH", "PARENT_GH", "GATHER_TOK", "PROJ_GATHER_GH", "PARENT_GATHER_GH"} {
		assert.NotContains(t, byName, forbidden, "selectDeliverySecretsWith must exclude/shadow %s", forbidden)
		assert.NotContains(t, spy.fetchedNames, forbidden, "FetchValues must not be asked for %s", forbidden)
	}

	// 2. Create/provision path (resolveSecrets).
	createResolved, asNeededKeys, err := d.resolveSecrets(ctx, &childNoOwner)
	require.NoError(t, err)
	createByName := make(map[string]ResolvedSecret, len(createResolved))
	for _, r := range createResolved {
		createByName[r.Name] = r
	}
	assert.Contains(t, createByName, "CTRL_PROJ_ALWAYS")
	assert.Contains(t, createByName, "CTRL_PROGENY_ALWAYS")
	for _, forbidden := range []string{"GITHUB_TOKEN", "PROJ_GH", "PARENT_GH"} {
		assert.NotContains(t, createByName, forbidden, "resolveSecrets must exclude/shadow %s", forbidden)
	}
	assert.NotContains(t, asNeededKeys, "GATHER_TOK", "resolveSecrets must not advertise shadowed GATHER_TOK as_needed")
	assert.Contains(t, asNeededKeys, "CTRL_PROJ_GATHER")

	// 3. Harness gather path (resolveAsNeededForKeys).
	gathered, err := d.resolveAsNeededForKeys(ctx, &childNoOwner, []string{"GATHER_TOK", "GITHUB_TOKEN", "CTRL_PROJ_GATHER", "CTRL_PROGENY_GATHER"}, nil)
	require.NoError(t, err)
	assert.NotContains(t, gathered, "GATHER_TOK", "resolveAsNeededForKeys must not gather shadowed GATHER_TOK")
	assert.NotContains(t, gathered, "GITHUB_TOKEN", "resolveAsNeededForKeys must not gather on_demand GITHUB_TOKEN")
	assert.Equal(t, "ctrl-proj-gather", gathered["CTRL_PROJ_GATHER"])
	assert.Equal(t, "ctrl-progeny-gather", gathered["CTRL_PROGENY_GATHER"])
}

func TestOnDemandSecret_P2_DeliveryCensusExcludesOnDemand(t *testing.T) {
	ctx := context.Background()
	f := newChainFixture(t, "od-p2-census")
	parent, _ := f.sessionParent(t, "od-p2-census-parent")

	// Seed user-scope rows directly in store (including an on_demand row and
	// a deliverable environment row) to verify the census counts only
	// deliverable rows.
	require.NoError(t, f.store.CreateSecret(ctx, &store.Secret{
		ID: api.NewUUID(), Key: "DELIVERABLE_UNSHARED", EncryptedValue: "v1",
		SecretType: store.SecretTypeEnvironment, Target: "DELIVERABLE_UNSHARED",
		Scope: store.ScopeUser, ScopeID: f.creator.ID, InjectionMode: store.InjectionModeAlways,
	}))
	require.NoError(t, f.store.CreateSecret(ctx, &store.Secret{
		ID: api.NewUUID(), Key: "ONDEMAND_UNSHARED", EncryptedValue: "v2",
		SecretType: store.SecretTypeOnDemand, Target: "ONDEMAND_UNSHARED",
		Scope: store.ScopeUser, ScopeID: f.creator.ID, InjectionMode: store.InjectionModeAsNeeded,
	}))
	require.NoError(t, f.store.CreateSecret(ctx, &store.Secret{
		ID: api.NewUUID(), Key: "DELIVERABLE_SHARED", EncryptedValue: "v3",
		SecretType: store.SecretTypeEnvironment, Target: "DELIVERABLE_SHARED",
		Scope: store.ScopeUser, ScopeID: f.creator.ID, InjectionMode: store.InjectionModeAlways,
		AllowProgeny: true, CreatedBy: f.creator.ID,
	}))
	require.NoError(t, f.store.CreateSecret(ctx, &store.Secret{
		ID: api.NewUUID(), Key: "ONDEMAND_SHARED", EncryptedValue: "v4",
		SecretType: store.SecretTypeOnDemand, Target: "ONDEMAND_SHARED",
		Scope: store.ScopeUser, ScopeID: f.creator.ID, InjectionMode: store.InjectionModeAsNeeded,
		AllowProgeny: true, CreatedBy: f.creator.ID,
	}))

	names := f.srv.authzService.ownerUnsharedRowNames(ctx, f.creator.ID)
	require.NoError(t, names.err)
	assert.Equal(t, []string{"DELIVERABLE_UNSHARED"}, names.secrets)
	assert.Equal(t, []string{"DELIVERABLE_SHARED"}, names.sharedSecrets)

	facts := f.srv.authzService.launchDeliveryUATFacts(ctx, parent)
	require.NotNil(t, facts)
	assert.Equal(t, 1, facts.OwnerSharedRows, "launchDeliveryUATFacts must count only deliverable shared rows")
}

func TestOnDemandSecret_P2_Validators(t *testing.T) {
	srv, s, owner, _, project, agentX, _ := setupAgentSecretsTestFixture(t, true)
	ctx := context.Background()

	// 1. Agent-originated PUT: accepts on_demand at agent and project scope;
	// refuses user scope, non-key target, and variable (naming on_demand).
	t.Run("agent_put", func(t *testing.T) {
		scopes := []AgentTokenScope{ScopeAgentStatusUpdate, ScopeProjectSecretRead}

		// Agent scope on_demand succeeds.
		rr := doAsAgentWithScopes(t, srv, agentX, scopes, http.MethodPut, "/api/v1/agents/"+agentX.ID+"/secrets/AG_PUT_OD", AgentSetSecretRequest{
			Value:    "secret-val",
			Encoding: "raw",
			Scope:    store.ScopeAgent,
			Type:     store.SecretTypeOnDemand,
		})
		require.Equal(t, http.StatusCreated, rr.Code, "body=%s", rr.Body.String())

		// Project scope on_demand succeeds.
		rr = doAsAgentWithScopes(t, srv, agentX, scopes, http.MethodPut, "/api/v1/agents/"+agentX.ID+"/secrets/PROJ_PUT_OD", AgentSetSecretRequest{
			Value:    "secret-val",
			Encoding: "raw",
			Scope:    store.ScopeProject,
			Type:     store.SecretTypeOnDemand,
		})
		require.Equal(t, http.StatusCreated, rr.Code, "body=%s", rr.Body.String())

		// User scope on_demand refused (400).
		rr = doAsAgentWithScopes(t, srv, agentX, scopes, http.MethodPut, "/api/v1/agents/"+agentX.ID+"/secrets/USER_PUT_OD", AgentSetSecretRequest{
			Value:    "secret-val",
			Encoding: "raw",
			Scope:    store.ScopeUser,
			Type:     store.SecretTypeOnDemand,
		})
		assert.Equal(t, http.StatusBadRequest, rr.Code, "body=%s", rr.Body.String())

		// Non-key target refused (400).
		rr = doAsAgentWithScopes(t, srv, agentX, scopes, http.MethodPut, "/api/v1/agents/"+agentX.ID+"/secrets/BAD_TGT_OD", AgentSetSecretRequest{
			Value:    "secret-val",
			Encoding: "raw",
			Scope:    store.ScopeAgent,
			Type:     store.SecretTypeOnDemand,
			Target:   "OTHER_KEY",
		})
		assert.Equal(t, http.StatusBadRequest, rr.Code, "body=%s", rr.Body.String())

		// Type "variable" refused (400) with message naming on_demand.
		rr = doAsAgentWithScopes(t, srv, agentX, scopes, http.MethodPut, "/api/v1/agents/"+agentX.ID+"/secrets/VAR_REFUSED", AgentSetSecretRequest{
			Value:    "secret-val",
			Encoding: "raw",
			Scope:    store.ScopeAgent,
			Type:     "variable",
		})
		assert.Equal(t, http.StatusBadRequest, rr.Code, "body=%s", rr.Body.String())
		assert.Contains(t, rr.Body.String(), "on_demand")

		// agent_secrets.user_scope_only still blocks project-scope writes (403).
		srv.config.AgentSecretsUserScopeOnly = boolPtr(true)
		defer func() { srv.config.AgentSecretsUserScopeOnly = nil }()
		rr = doAsAgentWithScopes(t, srv, agentX, scopes, http.MethodPut, "/api/v1/agents/"+agentX.ID+"/secrets/PROJ_BLOCKED_OD", AgentSetSecretRequest{
			Value:    "secret-val",
			Encoding: "raw",
			Scope:    store.ScopeProject,
			Type:     store.SecretTypeOnDemand,
		})
		assert.Equal(t, http.StatusForbidden, rr.Code, "body=%s", rr.Body.String())
	})

	// 2. Scoped PUT: /api/v1/projects/{id}/secrets/{key} and /api/v1/users/me/secrets/{key}.
	t.Run("scoped_put", func(t *testing.T) {
		rr := doRequestAsUser(t, srv, owner, http.MethodPut, "/api/v1/projects/"+project.ID+"/secrets/SCOPED_PROJ_OD", SetSecretRequest{
			Value:    "v",
			Encoding: "raw",
			Type:     store.SecretTypeOnDemand,
		})
		require.Equal(t, http.StatusOK, rr.Code, "body=%s", rr.Body.String())

		// Reject injectionMode: always.
		rr = doRequestAsUser(t, srv, owner, http.MethodPut, "/api/v1/projects/"+project.ID+"/secrets/SCOPED_BAD_MODE", SetSecretRequest{
			Value:         "v",
			Encoding:      "raw",
			Type:          store.SecretTypeOnDemand,
			InjectionMode: store.InjectionModeAlways,
		})
		assert.Equal(t, http.StatusBadRequest, rr.Code)

		// Reject non-key target.
		rr = doRequestAsUser(t, srv, owner, http.MethodPut, "/api/v1/projects/"+project.ID+"/secrets/SCOPED_BAD_TGT", SetSecretRequest{
			Value:    "v",
			Encoding: "raw",
			Type:     store.SecretTypeOnDemand,
			Target:   "OTHER",
		})
		assert.Equal(t, http.StatusBadRequest, rr.Code)

		// Reject type "variable" with message naming on_demand.
		rr = doRequestAsUser(t, srv, owner, http.MethodPut, "/api/v1/projects/"+project.ID+"/secrets/SCOPED_VAR", SetSecretRequest{
			Value:    "v",
			Encoding: "raw",
			Type:     "variable",
		})
		assert.Equal(t, http.StatusBadRequest, rr.Code)
		assert.Contains(t, rr.Body.String(), "on_demand")

		// Reject on_demand at runtime_broker scope via scoped endpoint.
		broker := &store.RuntimeBroker{
			ID:     api.NewUUID(),
			Name:   "broker-p2-scoped",
			Slug:   "broker-p2-scoped",
			Status: store.BrokerStatusOnline,
		}
		require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
		rr = doRequest(t, srv, http.MethodPut, "/api/v1/runtime-brokers/"+broker.ID+"/secrets/SCOPED_BROKER_OD", SetSecretRequest{
			Value:    "v",
			Encoding: "raw",
			Type:     store.SecretTypeOnDemand,
		})
		assert.Equal(t, http.StatusBadRequest, rr.Code, "body=%s", rr.Body.String())
	})

	// 3. PATCH cannot change OUT of on_demand at a disallowed scope.
	t.Run("patch_out_of_on_demand_at_disallowed_scope", func(t *testing.T) {
		require.NoError(t, s.CreateSecret(ctx, &store.Secret{
			ID: api.NewUUID(), Key: "STRAGGLER_USER_OD", EncryptedValue: "v",
			SecretType: store.SecretTypeOnDemand, Target: "STRAGGLER_USER_OD",
			Scope: store.ScopeUser, ScopeID: owner.ID, InjectionMode: store.InjectionModeAsNeeded,
		}))
		rr := doRequestAsUser(t, srv, owner, http.MethodPatch, "/api/v1/secrets/STRAGGLER_USER_OD?scope=user&scopeId="+owner.ID, PatchSecretRequest{
			Type: store.SecretTypeEnvironment,
		})
		assert.Equal(t, http.StatusBadRequest, rr.Code, "body=%s", rr.Body.String())
	})

	// 4. Inline create validateCreateAgentSecrets.
	t.Run("inline_create", func(t *testing.T) {
		got, vErr := srv.validateCreateAgentSecrets([]AgentSecretInput{
			{Key: "INLINE_OD", Value: "secret", Encoding: "raw", Type: store.SecretTypeOnDemand},
			{Key: "INLINE_OD_EXPLICIT", Value: "secret", Encoding: "raw", Type: store.SecretTypeOnDemand, Target: "INLINE_OD_EXPLICIT", InjectionMode: store.InjectionModeAsNeeded},
		})
		require.Nil(t, vErr)
		require.Len(t, got, 2)
		assert.Equal(t, store.SecretTypeOnDemand, got[0].SecretType)
		assert.Equal(t, "INLINE_OD", got[0].Target)
		assert.Equal(t, store.InjectionModeAsNeeded, got[0].InjectionMode)

		_, vErr = srv.validateCreateAgentSecrets([]AgentSecretInput{
			{Key: "BAD_MODE", Value: "v", Encoding: "raw", Type: store.SecretTypeOnDemand, InjectionMode: store.InjectionModeAlways},
		})
		require.NotNil(t, vErr)

		_, vErr = srv.validateCreateAgentSecrets([]AgentSecretInput{
			{Key: "BAD_TARGET", Value: "v", Encoding: "raw", Type: store.SecretTypeOnDemand, Target: "OTHER"},
		})
		require.NotNil(t, vErr)

		_, vErr = srv.validateCreateAgentSecrets([]AgentSecretInput{
			{Key: "BAD_VAR", Value: "v", Encoding: "raw", Type: "variable"},
		})
		require.NotNil(t, vErr)
		assert.Contains(t, vErr.message, "on_demand")
	})

	// 5. Pending-secret approval refuses non-environment/non-file types.
	t.Run("pending_approval_refuses_non_env_file", func(t *testing.T) {
		// Agent attempting to write an on_demand secret at user scope (which
		// routes to pending secrets) is rejected with 400 before creating any
		// pending secret.
		scopes := []AgentTokenScope{ScopeAgentStatusUpdate, ScopeProjectSecretRead}
		rr := doAsAgentWithScopes(t, srv, agentX, scopes, http.MethodPut, "/api/v1/agents/"+agentX.ID+"/secrets/PENDING_OD_USER", AgentSetSecretRequest{
			Value:    "held-val",
			Encoding: "raw",
			Scope:    store.ScopeUser,
			Type:     store.SecretTypeOnDemand,
		})
		assert.Equal(t, http.StatusBadRequest, rr.Code, "body=%s", rr.Body.String())
		pendingRows, err := s.ListPendingSecretsByOwner(ctx, store.ScopeUser, owner.ID)
		require.NoError(t, err)
		for _, p := range pendingRows {
			assert.NotEqual(t, "PENDING_OD_USER", p.Key, "no pending secret must be created for on_demand")
		}
	})
}

func TestOnDemandSecret_P2_SecretRequestDefaultAndFulfill(t *testing.T) {
	f := newSecretRequestsFixture(t, true)
	ctx := context.Background()

	// 1. Send a secret request with NO type -> defaults to on_demand with empty target.
	rr := f.sendSecretRequest(t, "user:"+f.alice.Email, []messages.SecretRequestInput{
		{Key: "API_SECRET", Description: "Needed on demand"},
	}, false)
	require.Equal(t, http.StatusOK, rr.Code, "body=%s", rr.Body.String())
	var sendResp struct {
		MessageID string `json:"message_id"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &sendResp))
	require.NotEmpty(t, sendResp.MessageID)

	// Verify stored secret_request row defaulted to on_demand with empty target.
	rows, err := f.store.GetSecretRequestsByMessageID(ctx, sendResp.MessageID)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, messages.SecretRequestTypeOnDemand, rows[0].SecretType)
	assert.Empty(t, rows[0].Target)

	// 2. Fulfill the secret request -> creates agent-scope on_demand secret with target == key.
	rr = doRequestAsUser(t, f.srv, f.alice, http.MethodPost, "/api/v1/secret-requests/"+sendResp.MessageID+"/fulfill", map[string]any{
		"values": map[string]string{"API_SECRET": "fulfilled-od-val"},
	})
	require.Equal(t, http.StatusOK, rr.Code, "body=%s", rr.Body.String())

	meta, err := f.srv.secretBackend.GetMeta(ctx, "API_SECRET", store.ScopeAgent, f.agent.ID)
	require.NoError(t, err)
	assert.Equal(t, store.SecretTypeOnDemand, meta.SecretType)
	assert.Equal(t, "API_SECRET", meta.Target)
	assert.Equal(t, store.InjectionModeAsNeeded, meta.InjectionMode)

	// 3. Reject a secret request with on_demand and a non-key target.
	rr = f.sendSecretRequest(t, "user:"+f.alice.Email, []messages.SecretRequestInput{
		{Key: "BAD_REQ", Type: messages.SecretRequestTypeOnDemand, Target: "OTHER_TARGET"},
	}, false)
	assert.Equal(t, http.StatusBadRequest, rr.Code, "body=%s", rr.Body.String())

	// 4. O3: a pre-existing on_demand secret_requests row carrying a legacy
	// non-key target (e.g. migrated from variable) still fulfills cleanly and
	// forces the stored secret's Target to row.Key.
	origMsg, err := f.store.GetMessage(ctx, sendResp.MessageID)
	require.NoError(t, err)
	legacyMsgID := api.NewUUID()
	now := time.Now().UTC()
	promptRes, err := f.store.CreateSecretRequestPrompt(ctx, &store.SecretRequestPromptInput{
		Message: &store.Message{
			ID:             legacyMsgID,
			ProjectID:      origMsg.ProjectID,
			ConversationID: origMsg.ConversationID,
			ThreadID:       origMsg.ThreadID,
			Sender:         origMsg.Sender,
			SenderID:       origMsg.SenderID,
			Recipient:      origMsg.Recipient,
			RecipientID:    origMsg.RecipientID,
			Msg:            "Legacy migrated on_demand request",
			Type:           messages.TypeSecretRequest,
			DispatchState:  store.MessageDispatchDispatched,
		},
		AgentID:   f.agent.ID,
		AgentName: f.agent.Name,
		Requests: []messages.SecretRequest{
			{
				Key:        "LEGACY_TGT_OD",
				KeyNorm:    messages.SecretRequestKeyNorm("LEGACY_TGT_OD"),
				SecretType: messages.SecretRequestTypeOnDemand,
				Target:     "LEGACY_CUSTOM_TARGET",
			},
		},
		Now: now,
		TTL: time.Hour,
	})
	require.NoError(t, err)
	require.Len(t, promptRes.Requests, 1)
	require.Equal(t, "LEGACY_CUSTOM_TARGET", promptRes.Requests[0].Row.Target)

	rr = doRequestAsUser(t, f.srv, f.alice, http.MethodPost, "/api/v1/secret-requests/"+legacyMsgID+"/fulfill", map[string]any{
		"values": map[string]string{"LEGACY_TGT_OD": "legacy-fulfilled-val"},
	})
	require.Equal(t, http.StatusOK, rr.Code, "body=%s", rr.Body.String())

	legacyMeta, err := f.srv.secretBackend.GetMeta(ctx, "LEGACY_TGT_OD", store.ScopeAgent, f.agent.ID)
	require.NoError(t, err)
	assert.Equal(t, store.SecretTypeOnDemand, legacyMeta.SecretType)
	assert.Equal(t, "LEGACY_TGT_OD", legacyMeta.Target, "fulfillment must force target == key for on_demand rows")
}

func TestOnDemandSecret_P2_RuntimeReadAuthzAndList(t *testing.T) {
	srv, s, owner, _, project, agentX, _ := setupAgentSecretsTestFixture(t, true)
	ctx := context.Background()

	// Seed agent-scope and project-scope on_demand secrets.
	_, _, err := srv.secretBackend.Set(ctx, &secret.SetSecretInput{
		Name: "AGENT_OD_READ", Value: "agent-od-val", SecretType: store.SecretTypeOnDemand,
		Target: "AGENT_OD_READ", InjectionMode: store.InjectionModeAsNeeded,
		Scope: store.ScopeAgent, ScopeID: agentX.ID, ProjectID: project.ID, CreatedBy: owner.ID,
	})
	require.NoError(t, err)
	_, _, err = srv.secretBackend.Set(ctx, &secret.SetSecretInput{
		Name: "PROJ_OD_READ", Value: "proj-od-val", SecretType: store.SecretTypeOnDemand,
		Target: "PROJ_OD_READ", InjectionMode: store.InjectionModeAsNeeded,
		Scope: store.ScopeProject, ScopeID: project.ID, CreatedBy: owner.ID,
	})
	require.NoError(t, err)

	// 1. Agent with project:secret:read + project.secret_read can read both
	// agent-scope and project-scope on_demand secrets via GET and batch POST.
	withProjRead := []AgentTokenScope{ScopeAgentStatusUpdate, ScopeProjectSecretRead}

	rr := doAsAgentWithScopes(t, srv, agentX, withProjRead, http.MethodGet, "/api/v1/agents/"+agentX.ID+"/secrets/AGENT_OD_READ", nil)
	require.Equal(t, http.StatusOK, rr.Code, "body=%s", rr.Body.String())
	var agentGet AgentGetSecretResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &agentGet))
	dec, err := base64.StdEncoding.DecodeString(agentGet.Value)
	require.NoError(t, err)
	assert.Equal(t, "agent-od-val", string(dec))
	assert.Equal(t, store.SecretTypeOnDemand, agentGet.Type)
	assert.Empty(t, agentGet.Target)

	rr = doAsAgentWithScopes(t, srv, agentX, withProjRead, http.MethodGet, "/api/v1/agents/"+agentX.ID+"/secrets/PROJ_OD_READ", nil)
	require.Equal(t, http.StatusOK, rr.Code, "body=%s", rr.Body.String())
	var projGet AgentGetSecretResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &projGet))
	dec, err = base64.StdEncoding.DecodeString(projGet.Value)
	require.NoError(t, err)
	assert.Equal(t, "proj-od-val", string(dec))
	assert.Equal(t, store.SecretTypeOnDemand, projGet.Type)
	assert.Empty(t, projGet.Target)

	// Batch POST /api/v1/agent/secrets also returns both.
	rr = doAsAgentWithScopes(t, srv, agentX, withProjRead, http.MethodPost, "/api/v1/agent/secrets", map[string]any{
		"keys": []string{"AGENT_OD_READ", "PROJ_OD_READ"},
	})
	require.Equal(t, http.StatusOK, rr.Code, "body=%s", rr.Body.String())
	assert.Contains(t, rr.Body.String(), "AGENT_OD_READ")
	assert.Contains(t, rr.Body.String(), "PROJ_OD_READ")

	// List shows both with type=on_demand and empty target.
	rr = doAsAgentWithScopes(t, srv, agentX, withProjRead, http.MethodGet, "/api/v1/agents/"+agentX.ID+"/secrets", nil)
	require.Equal(t, http.StatusOK, rr.Code, "body=%s", rr.Body.String())
	var listResp AgentListSecretsResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &listResp))
	byKey := make(map[string]AgentSecretMeta, len(listResp.Secrets))
	for _, m := range listResp.Secrets {
		byKey[m.Key] = m
	}
	for _, k := range []string{"AGENT_OD_READ", "PROJ_OD_READ"} {
		m, ok := byKey[k]
		require.True(t, ok, "expected %s in list", k)
		assert.Equal(t, store.SecretTypeOnDemand, m.Type)
		assert.Empty(t, m.Target)
	}

	// 2. Agent WITHOUT project:secret:read can still read its own agent-scope
	// on_demand secret, but is REFUSED (403/404) for the project-scope one.
	withoutProjRead := []AgentTokenScope{ScopeAgentStatusUpdate}
	rr = doAsAgentWithScopes(t, srv, agentX, withoutProjRead, http.MethodGet, "/api/v1/agents/"+agentX.ID+"/secrets/AGENT_OD_READ", nil)
	assert.Equal(t, http.StatusOK, rr.Code, "agent-scope on_demand must still be readable without project:secret:read")

	rr = doAsAgentWithScopes(t, srv, agentX, withoutProjRead, http.MethodGet, "/api/v1/agents/"+agentX.ID+"/secrets/PROJ_OD_READ", nil)
	assert.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, rr.Code,
		"project-scope on_demand must be refused without project:secret:read; got %d body=%s", rr.Code, rr.Body.String())

	// 2b. O4: Agent WITH project:secret:read token scope but LACKING the
	// project.secret_read permission (delegator holds project-member, which
	// does not grant project.secret_read) is refused for project on_demand
	// while still able to read its own agent-scope on_demand secret.
	setBackfillCompleted(t, s)
	memberDelegatorID := api.NewUUID()
	createDCUser(t, s, memberDelegatorID, "member-delegator-od@example.com", project.ID, store.ProjectRoleMember)
	agentNoPerm := &store.Agent{
		ID:        api.NewUUID(),
		Slug:      "agent-no-proj-perm",
		Name:      "agent-no-proj-perm",
		ProjectID: project.ID,
		OwnerID:   memberDelegatorID,
		CreatedBy: memberDelegatorID,
		Ancestry:  []string{memberDelegatorID},
		Phase:     "running",
	}
	require.NoError(t, s.CreateAgent(ctx, agentNoPerm))
	seedRecordedDelegationEdge(t, s, store.DelegationPrincipalUser, memberDelegatorID, store.DelegationPrincipalAgent, agentNoPerm.ID,
		store.RoleScopeProject, project.ID, string(AgentRoleFull))
	_, _, err = srv.secretBackend.Set(ctx, &secret.SetSecretInput{
		Name: "OWN_OD_NO_PERM", Value: "own-od-val", SecretType: store.SecretTypeOnDemand,
		Target: "OWN_OD_NO_PERM", InjectionMode: store.InjectionModeAsNeeded,
		Scope: store.ScopeAgent, ScopeID: agentNoPerm.ID, ProjectID: project.ID, CreatedBy: owner.ID,
	})
	require.NoError(t, err)

	rr = doAsAgentWithScopes(t, srv, agentNoPerm, withProjRead, http.MethodGet, "/api/v1/agents/"+agentNoPerm.ID+"/secrets/OWN_OD_NO_PERM", nil)
	assert.Equal(t, http.StatusOK, rr.Code, "agent-scope on_demand must succeed even without project.secret_read")

	rr = doAsAgentWithScopes(t, srv, agentNoPerm, withProjRead, http.MethodGet, "/api/v1/agents/"+agentNoPerm.ID+"/secrets/PROJ_OD_READ", nil)
	assert.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, rr.Code,
		"project-scope on_demand must be refused when token holds project:secret:read but agent lacks project.secret_read permission; got %d body=%s",
		rr.Code, rr.Body.String())

	// 3. Check 9 (fetchAuthorizedValue): rejects a secret whose SecretType
	// changed between metadata inspection and value fetch.
	meta, err := srv.secretBackend.GetMeta(ctx, "AGENT_OD_READ", store.ScopeAgent, agentX.ID)
	require.NoError(t, err)
	staleMeta := *meta
	staleMeta.SecretType = store.SecretTypeEnvironment
	sv, reason := srv.fetchAuthorizedValue(ctx, ItemResult{
		Candidate: Candidate{
			Key:     meta.Name,
			Scope:   meta.Scope,
			ScopeID: meta.ScopeID,
			Meta:    staleMeta,
		},
		Allowed: true,
	})
	assert.Nil(t, sv)
	assert.Equal(t, ReasonRecordChanged, reason)
}

type spyValueAccessBackend struct {
	secret.SecretBackend
	mu           sync.Mutex
	getCalls     []string // "scope/name"
	fetchedNames []string // "scope/name"
}

func (b *spyValueAccessBackend) Get(ctx context.Context, name, scope, scopeID string) (*secret.SecretWithValue, error) {
	b.mu.Lock()
	b.getCalls = append(b.getCalls, scope+"/"+name)
	b.mu.Unlock()
	return b.SecretBackend.Get(ctx, name, scope, scopeID)
}

func (b *spyValueAccessBackend) FetchValues(ctx context.Context, metas []secret.SecretMeta) (map[string]secret.FetchResult, error) {
	b.mu.Lock()
	for _, m := range metas {
		b.fetchedNames = append(b.fetchedNames, m.Scope+"/"+m.Name)
	}
	b.mu.Unlock()
	return b.SecretBackend.FetchValues(ctx, metas)
}

func (b *spyValueAccessBackend) reset() {
	b.mu.Lock()
	b.getCalls = nil
	b.fetchedNames = nil
	b.mu.Unlock()
}

// TestOnDemandSecret_P2_NoAuthCreateAndStartShadowing verifies R1:
// (a) A project on_demand GITHUB_TOKEN is never value-fetched (Get or
//
//	FetchValues) on create for a NoAuth agent.
//
// (b) An agent-scope on_demand GITHUB_TOKEN shadows a lower-scope project (and
//
//	user) environment GITHUB_TOKEN on both create AND start/restart, with
//	neither token delivered in ResolvedEnv and neither value fetched.
//
// (c) Control: without the agent-scope on_demand row, project environment
//
//	GITHUB_TOKEN is delivered on both create and start.
func TestOnDemandSecret_P2_NoAuthCreateAndStartShadowing(t *testing.T) {
	ctx := context.Background()
	f := newChainFixture(t, "od-p2-noauth-r1")
	enableAgentSecretsExperimentForTest(t, f.srv, f.store)
	if f.srv.GetSecretBackend() == nil {
		f.srv.SetSecretBackend(secret.NewLocalBackend(f.store, "test-hub", "test-signing-key-32-bytes-long!!"))
	}
	baseSB := f.srv.GetSecretBackend()
	spy := &spyValueAccessBackend{SecretBackend: baseSB}

	parent, _ := f.sessionParent(t, "noauth-r1-agent")
	noAuthAgent := *parent
	noAuthAgent.AppliedConfig = &store.AgentAppliedConfig{NoAuth: true}

	d := NewHTTPAgentDispatcher(f.store, false, nil)
	d.SetSecretBackend(spy)
	d.SetAuthzService(f.srv.authzService)
	d.SetAgentSecretsEnabledProvider(func() bool { return f.srv.experimentEnabled(experiments.AgentSecrets) })

	startGitToken := func(t *testing.T) string {
		t.Helper()
		candidates, _, err := d.selectDeliverySecrets(ctx, newDeliverySelector(f.srv.authzService, &noAuthAgent, PurposeDispatch), &noAuthAgent)
		require.NoError(t, err)
		return pickNoAuthGitTokenFrom(candidates).value
	}

	// (a) Project on_demand GITHUB_TOKEN: never value-fetched on create or start.
	_, _, err := baseSB.Set(ctx, &secret.SetSecretInput{
		Name: noAuthGitTokenKey, Value: "proj-od-never-read", SecretType: store.SecretTypeOnDemand,
		Target: noAuthGitTokenKey, InjectionMode: store.InjectionModeAsNeeded,
		Scope: store.ScopeProject, ScopeID: f.proj.ID,
	})
	require.NoError(t, err)

	spy.reset()
	reqA, err := d.buildCreateRequestFor(ctx, &noAuthAgent, "test-r1-a", failOnSecretResolutionError)
	require.NoError(t, err)
	assert.NotContains(t, reqA.ResolvedEnv, noAuthGitTokenKey)
	assert.NotContains(t, spy.getCalls, store.ScopeProject+"/"+noAuthGitTokenKey, "create must not call Get for project on_demand GITHUB_TOKEN")
	assert.NotContains(t, spy.fetchedNames, store.ScopeProject+"/"+noAuthGitTokenKey, "create must not call FetchValues for project on_demand GITHUB_TOKEN")

	// (b) Agent-scope on_demand GITHUB_TOKEN + project & user environment
	// GITHUB_TOKEN -> no GITHUB_TOKEN in ResolvedEnv on create AND start/restart,
	// and none of their values are fetched.
	_, _, err = baseSB.Set(ctx, &secret.SetSecretInput{
		Name: noAuthGitTokenKey, Value: "proj-env-shadowed", SecretType: store.SecretTypeEnvironment,
		Target: noAuthGitTokenKey, InjectionMode: store.InjectionModeAlways,
		Scope: store.ScopeProject, ScopeID: f.proj.ID,
	})
	require.NoError(t, err)
	_, _, err = baseSB.Set(ctx, &secret.SetSecretInput{
		Name: noAuthGitTokenKey, Value: "user-env-shadowed", SecretType: store.SecretTypeEnvironment,
		Target: noAuthGitTokenKey, InjectionMode: store.InjectionModeAlways,
		Scope: store.ScopeUser, ScopeID: f.creator.ID,
	})
	require.NoError(t, err)
	_, _, err = baseSB.Set(ctx, &secret.SetSecretInput{
		Name: noAuthGitTokenKey, Value: "agent-od-shadow", SecretType: store.SecretTypeOnDemand,
		Target: noAuthGitTokenKey, InjectionMode: store.InjectionModeAsNeeded,
		Scope: store.ScopeAgent, ScopeID: parent.ID, ProjectID: f.proj.ID,
	})
	require.NoError(t, err)

	spy.reset()
	reqB, err := d.buildCreateRequestFor(ctx, &noAuthAgent, "test-r1-b", failOnSecretResolutionError)
	require.NoError(t, err)
	assert.NotContains(t, reqB.ResolvedEnv, noAuthGitTokenKey, "agent on_demand GITHUB_TOKEN must shadow project/user tokens on create")
	assert.Empty(t, spy.getCalls, "create must not read any shadowed GITHUB_TOKEN via Get")
	assert.Empty(t, spy.fetchedNames, "create must not read any shadowed GITHUB_TOKEN via FetchValues")

	spy.reset()
	assert.Empty(t, startGitToken(t), "agent on_demand GITHUB_TOKEN must shadow project/user tokens on start/restart")
	assert.Empty(t, spy.fetchedNames, "start must not fetch any shadowed GITHUB_TOKEN")

	// (c) Control: removing agent-scope on_demand restores project env delivery
	// on both create and start.
	require.NoError(t, baseSB.Delete(ctx, noAuthGitTokenKey, store.ScopeAgent, parent.ID))

	reqC, err := d.buildCreateRequestFor(ctx, &noAuthAgent, "test-r1-c", failOnSecretResolutionError)
	require.NoError(t, err)
	assert.Equal(t, "proj-env-shadowed", reqC.ResolvedEnv[noAuthGitTokenKey])
	assert.Equal(t, "proj-env-shadowed", startGitToken(t))
}
