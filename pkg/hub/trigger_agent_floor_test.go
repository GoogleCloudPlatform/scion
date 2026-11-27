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
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func createRoleAgentForFloorTest(t *testing.T, f *triggersTestFixture, slug string, role AgentRole) (*store.Agent, AgentIdentity) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	ag := &store.Agent{
		ID:        tid("floor-agent-" + slug),
		Slug:      slug,
		Name:      slug,
		ProjectID: f.projectID,
		Phase:     string(state.PhaseRunning),
		CreatedBy: f.owner.ID,
		OwnerID:   f.owner.ID,
		Ancestry:  []string{f.owner.ID},
		AppliedConfig: &store.AgentAppliedConfig{
			AgentRole: string(role),
		},
	}
	require.NoError(t, f.store.CreateAgent(ctx, ag))
	require.NoError(t, f.store.CreateDelegationEdge(ctx, &store.DelegationEdge{
		ID:            api.NewUUID(),
		DelegatorType: store.DelegationPrincipalUser,
		DelegatorID:   f.owner.ID,
		DelegateType:  store.DelegationPrincipalAgent,
		DelegateID:    ag.ID,
		ScopeType:     store.RoleScopeProject,
		ScopeID:       f.projectID,
		Role:          string(role),
		EffectCeiling: store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
		Active:        true,
		CreatedAt:     now,
	}))
	jwtID := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: ag.ID},
		ProjectID: f.projectID,
		Scopes:    ScopesForRole(role),
	}}
	return ag, jwtID
}

func assertForbiddenMissingNotifyScope(t *testing.T, code int, body string) {
	t.Helper()
	assert.Equal(t, http.StatusForbidden, code, "expected 403, body=%s", body)
	assert.Contains(t, body, `"code":"forbidden"`)
	assert.Contains(t, body, "Missing required scope: project:agent:notify")
}

// TestTriggerAgentFloor_PrincipalRouteMatrix verifies Stage 2 §4.3
// (ptone/scion#8065): agents below baseline (readonly, none) are refused with
// 403 on every route that creates or widens trigger/webhook-endpoint authority,
// while reads and authority-reducing operations (delete, pause, disable-only
// PATCH) on owned objects remain allowed. Baseline agents, full agents, users,
// and UATs retain their existing behaviour.
func TestTriggerAgentFloor_PrincipalRouteMatrix(t *testing.T) {
	f := setupTriggersTestFixture(t)
	ctx := context.Background()

	roAgent, roAuth := createRoleAgentForFloorTest(t, f, "ro-agent", AgentRoleReadOnly)
	baseAgent, baseAuth := createRoleAgentForFloorTest(t, f, "base-agent", AgentRoleBaseline)
	fullAgent, fullAuth := createRoleAgentForFloorTest(t, f, "full-agent", AgentRoleFull)

	projBoundary := TokenBoundary{Kind: BoundaryKindProject, ProjectID: f.projectID}
	fullUAT := scheduleTestUAT(t, f.ownerAuth, projBoundary,
		"webhook_endpoint:list", "webhook_endpoint:read", "webhook_endpoint:create",
		"webhook_endpoint:update", "webhook_endpoint:delete",
		"trigger:list", "trigger:read", "trigger:create", "trigger:update", "trigger:delete",
		"agent:message",
	)

	// Seed an endpoint + trigger + event owned by roAgent (as if created while
	// baseline before being downgraded to readonly) so we can verify reads,
	// authority-reducing ops, and blocked widening ops on owned rows.
	now := time.Now().UTC().Truncate(time.Microsecond)
	roEp := &store.WebhookEndpoint{
		ID:                  api.NewUUID(),
		ProjectID:           f.projectID,
		Name:                "ro-owned-ep",
		OwnerPrincipalKind:  store.DelegationPrincipalAgent,
		OwnerPrincipalID:    roAgent.ID,
		OwnerCredentialKind: store.InitiatorCredentialKindAgent,
		AuthMode:            store.WebhookAuthModeHMAC,
		SignatureMode:       store.WebhookSignatureModeScionV1,
		SecretVersion:       1,
		Status:              store.WebhookEndpointStatusActive,
		BoundAgentID:        roAgent.ID,
		BoundLifetime:       store.WebhookBoundLifetimeAgent,
		CreatedBy:           roAgent.ID,
		CreatedAt:           now,
		UpdatedAt:           now,
	}
	require.NoError(t, f.store.CreateWebhookEndpoint(ctx, roEp))

	roDisabledEp := &store.WebhookEndpoint{
		ID:                  api.NewUUID(),
		ProjectID:           f.projectID,
		Name:                "ro-disabled-ep",
		OwnerPrincipalKind:  store.DelegationPrincipalAgent,
		OwnerPrincipalID:    roAgent.ID,
		OwnerCredentialKind: store.InitiatorCredentialKindAgent,
		AuthMode:            store.WebhookAuthModeHMAC,
		SignatureMode:       store.WebhookSignatureModeScionV1,
		SecretVersion:       1,
		Status:              store.WebhookEndpointStatusDisabled,
		CreatedBy:           roAgent.ID,
		CreatedAt:           now,
		UpdatedAt:           now,
	}
	require.NoError(t, f.store.CreateWebhookEndpoint(ctx, roDisabledEp))

	// Seed an endpoint owned by owner and bound to roAgent (bound non-owner).
	boundOnlyEp := &store.WebhookEndpoint{
		ID:                  api.NewUUID(),
		ProjectID:           f.projectID,
		Name:                "bound-only-ep",
		OwnerPrincipalKind:  store.DelegationPrincipalUser,
		OwnerPrincipalID:    f.owner.ID,
		OwnerCredentialKind: store.InitiatorCredentialKindSession,
		AuthMode:            store.WebhookAuthModeHMAC,
		SignatureMode:       store.WebhookSignatureModeScionV1,
		SecretVersion:       1,
		Status:              store.WebhookEndpointStatusActive,
		BoundAgentID:        roAgent.ID,
		BoundLifetime:       store.WebhookBoundLifetimeAgent,
		CreatedBy:           f.owner.ID,
		CreatedAt:           now,
		UpdatedAt:           now,
	}
	require.NoError(t, f.store.CreateWebhookEndpoint(ctx, boundOnlyEp))

	roMsgPayload, err := json.Marshal(MessageEventPayload{
		AgentID:   roAgent.ID,
		AgentName: roAgent.Name,
		Message:   "hello ${event.type}",
	})
	require.NoError(t, err)
	roAttr := store.InitiatorAttribution{
		AttributionVersion:      1,
		AuthorizationRevision:   1,
		InitiatorPrincipalKind:  store.DelegationPrincipalAgent,
		InitiatorPrincipalID:    roAgent.ID,
		InitiatorCredentialKind: store.InitiatorCredentialKindAgent,
	}
	roTrigger := &store.Trigger{
		ID:                   api.NewUUID(),
		ProjectID:            f.projectID,
		Name:                 "ro-owned-trigger",
		Status:               store.TriggerStatusActive,
		SourceKind:           store.TriggerSourceKindWebhook,
		SourceID:             roEp.ID,
		MatchTypes:           []string{"webhook.received"},
		SubjectMode:          store.TriggerSubjectModeNew,
		Actions:              []store.TriggerAction{{Type: store.TriggerActionMessage, Payload: string(roMsgPayload)}},
		InitiatorAttribution: roAttr,
		AuthorityCeiling:     store.EffectCeiling{Kind: store.EffectCeilingBounded, Version: 1},
		CreatedBy:            roAgent.ID,
		CreatedAt:            now,
		UpdatedAt:            now,
	}
	require.NoError(t, f.store.CreateTrigger(ctx, roTrigger))

	roPausedTrigger := &store.Trigger{
		ID:                   api.NewUUID(),
		ProjectID:            f.projectID,
		Name:                 "ro-paused-trigger",
		Status:               store.TriggerStatusPaused,
		SourceKind:           store.TriggerSourceKindWebhook,
		SourceID:             roEp.ID,
		MatchTypes:           []string{"webhook.received"},
		SubjectMode:          store.TriggerSubjectModeNew,
		Actions:              []store.TriggerAction{{Type: store.TriggerActionMessage, Payload: string(roMsgPayload)}},
		InitiatorAttribution: roAttr,
		AuthorityCeiling:     store.EffectCeiling{Kind: store.EffectCeilingBounded, Version: 1},
		CreatedBy:            roAgent.ID,
		CreatedAt:            now,
		UpdatedAt:            now,
	}
	require.NoError(t, f.store.CreateTrigger(ctx, roPausedTrigger))

	roEvent := &store.TriggerEvent{
		ID:          api.NewUUID(),
		ProjectID:   f.projectID,
		SourceKind:  store.TriggerSourceKindWebhook,
		SourceID:    roEp.ID,
		EventType:   "webhook.received",
		Status:      store.TriggerEventStatusProcessed,
		Payload:     `{"ok":true}`,
		ContentType: "application/json",
		ReceivedAt:  now,
	}
	require.NoError(t, f.store.CreateTriggerEvent(ctx, roEvent))

	t.Run("readonly_agent_refused_on_create_and_widening_routes", func(t *testing.T) {
		// 1. POST /webhook-endpoints (unbound)
		rec := f.doWebhookEndpoint(t, roAuth, "", http.MethodPost, CreateWebhookEndpointRequest{
			Name: "ro-unbound-ep",
		})
		assertForbiddenMissingNotifyScope(t, rec.Code, rec.Body.String())

		// 2. POST /webhook-endpoints (--to-self with composite trigger)
		rec = f.doWebhookEndpoint(t, roAuth, "", http.MethodPost, map[string]any{
			"name":          "ro-self-ep",
			"boundAgentId":  "self",
			"boundLifetime": "agent",
			"trigger":       map[string]any{"message": "hi"},
		})
		assertForbiddenMissingNotifyScope(t, rec.Code, rec.Body.String())

		// 3. PATCH /webhook-endpoints/{id} (description update on own endpoint)
		desc := "mutated"
		rec = f.doWebhookEndpoint(t, roAuth, roEp.ID, http.MethodPatch, UpdateWebhookEndpointRequest{
			Description: &desc,
		})
		assertForbiddenMissingNotifyScope(t, rec.Code, rec.Body.String())

		// 4. PATCH /webhook-endpoints/{id} (re-enable disabled own endpoint)
		activeStatus := store.WebhookEndpointStatusActive
		rec = f.doWebhookEndpoint(t, roAuth, roDisabledEp.ID, http.MethodPatch, UpdateWebhookEndpointRequest{
			Status: &activeStatus,
		})
		assertForbiddenMissingNotifyScope(t, rec.Code, rec.Body.String())

		// 5. PATCH /webhook-endpoints/{id} (mixed disable + description)
		disabledStatus := store.WebhookEndpointStatusDisabled
		rec = f.doWebhookEndpoint(t, roAuth, roEp.ID, http.MethodPatch, UpdateWebhookEndpointRequest{
			Status:      &disabledStatus,
			Description: &desc,
		})
		assertForbiddenMissingNotifyScope(t, rec.Code, rec.Body.String())

		// 6. PATCH /webhook-endpoints/{id} (empty body)
		rec = f.doWebhookEndpoint(t, roAuth, roEp.ID, http.MethodPatch, map[string]any{})
		assertForbiddenMissingNotifyScope(t, rec.Code, rec.Body.String())

		// 7. POST /webhook-endpoints/{id}/rotate-secret
		rec = f.doWebhookEndpoint(t, roAuth, roEp.ID+"/rotate-secret", http.MethodPost, nil)
		assertForbiddenMissingNotifyScope(t, rec.Code, rec.Body.String())

		// 8. POST /triggers (message-to-self)
		rec = f.doTrigger(t, roAuth, "", http.MethodPost, CreateTriggerRequest{
			Name:   "ro-new-trigger",
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: roEp.ID},
			Action: TriggerActionSpec{Type: store.TriggerActionMessage, AgentID: roAgent.ID, Message: "hi"},
		})
		assertForbiddenMissingNotifyScope(t, rec.Code, rec.Body.String())

		// 9. PATCH /triggers/{id}
		rec = f.doTrigger(t, roAuth, roTrigger.ID, http.MethodPatch, UpdateTriggerRequest{
			Description: &desc,
		})
		assertForbiddenMissingNotifyScope(t, rec.Code, rec.Body.String())

		// 10. POST /triggers/{id}/resume
		rec = f.doTrigger(t, roAuth, roPausedTrigger.ID+"/resume", http.MethodPost, nil)
		assertForbiddenMissingNotifyScope(t, rec.Code, rec.Body.String())
	})

	t.Run("readonly_agent_allowed_on_reads_and_authority_reducing_routes", func(t *testing.T) {
		// Reads stay at project:read.
		assert.Equal(t, http.StatusOK, f.doWebhookEndpoint(t, roAuth, "", http.MethodGet, nil).Code)
		assert.Equal(t, http.StatusOK, f.doWebhookEndpoint(t, roAuth, roEp.ID, http.MethodGet, nil).Code)
		assert.Equal(t, http.StatusOK, f.doTrigger(t, roAuth, "", http.MethodGet, nil).Code)
		assert.Equal(t, http.StatusOK, f.doTrigger(t, roAuth, roTrigger.ID, http.MethodGet, nil).Code)
		assert.Equal(t, http.StatusOK, f.doTrigger(t, roAuth, roTrigger.ID+"/history", http.MethodGet, nil).Code)
		assert.Equal(t, http.StatusOK, f.doTrigger(t, roAuth, roTrigger.ID+"/render?eventId="+roEvent.ID, http.MethodPost, nil).Code)
		assert.Equal(t, http.StatusOK, f.doTriggerEvents(t, roAuth, "", http.MethodGet, nil).Code)
		assert.Equal(t, http.StatusOK, f.doTriggerEvents(t, roAuth, roEvent.ID, http.MethodGet, nil).Code)
		assert.Equal(t, http.StatusOK, f.doTriggerFires(t, roAuth, "", http.MethodGet, nil).Code)

		// Disable-only PATCH on unowned endpoint -> 404 (no existence oracle);
		// on bound non-owner endpoint -> 403; on own endpoint -> 200 OK.
		disabledStatus := store.WebhookEndpointStatusDisabled
		assert.Equal(t, http.StatusNotFound, f.doWebhookEndpoint(t, roAuth, api.NewUUID(), http.MethodPatch, UpdateWebhookEndpointRequest{
			Status: &disabledStatus,
		}).Code)
		assert.Equal(t, http.StatusForbidden, f.doWebhookEndpoint(t, roAuth, boundOnlyEp.ID, http.MethodPatch, UpdateWebhookEndpointRequest{
			Status: &disabledStatus,
		}).Code)
		rec := f.doWebhookEndpoint(t, roAuth, roEp.ID, http.MethodPatch, UpdateWebhookEndpointRequest{
			Status: &disabledStatus,
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		// Pause own trigger -> 200 OK.
		rec = f.doTrigger(t, roAuth, roTrigger.ID+"/pause", http.MethodPost, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		// Delete own trigger and own endpoint -> 204 No Content.
		assert.Equal(t, http.StatusNoContent, f.doTrigger(t, roAuth, roTrigger.ID, http.MethodDelete, nil).Code)
		assert.Equal(t, http.StatusNoContent, f.doTrigger(t, roAuth, roPausedTrigger.ID, http.MethodDelete, nil).Code)
		assert.Equal(t, http.StatusNoContent, f.doWebhookEndpoint(t, roAuth, roEp.ID, http.MethodDelete, nil).Code)
		assert.Equal(t, http.StatusNoContent, f.doWebhookEndpoint(t, roAuth, roDisabledEp.ID, http.MethodDelete, nil).Code)
	})

	t.Run("none_agent_refused_everywhere", func(t *testing.T) {
		assert.Equal(t, http.StatusForbidden, f.doWebhookEndpoint(t, f.agentNoneJWT, "", http.MethodGet, nil).Code)
		assert.Equal(t, http.StatusForbidden, f.doWebhookEndpoint(t, f.agentNoneJWT, "", http.MethodPost, CreateWebhookEndpointRequest{Name: "none-ep"}).Code)
		assert.Equal(t, http.StatusForbidden, f.doTrigger(t, f.agentNoneJWT, "", http.MethodGet, nil).Code)
		assert.Equal(t, http.StatusForbidden, f.doTrigger(t, f.agentNoneJWT, "", http.MethodPost, CreateTriggerRequest{Name: "none-tr"}).Code)
	})

	t.Run("baseline_and_full_agents_allowed_as_today", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			agent *store.Agent
			auth  AgentIdentity
		}{
			{"baseline", baseAgent, baseAuth},
			{"full", fullAgent, fullAuth},
		} {
			t.Run(tc.name, func(t *testing.T) {
				// Composite --to-self create.
				epRec := f.doWebhookEndpoint(t, tc.auth, "", http.MethodPost, map[string]any{
					"name":          tc.name + "-self-ep",
					"boundAgentId":  "self",
					"boundLifetime": "agent",
					"trigger":       map[string]any{"message": "self ping"},
				})
				require.Equal(t, http.StatusCreated, epRec.Code, epRec.Body.String())
				var created WebhookEndpointCreatedResponse
				require.NoError(t, json.Unmarshal(epRec.Body.Bytes(), &created))

				// Unbound endpoint create.
				unboundRec := f.doWebhookEndpoint(t, tc.auth, "", http.MethodPost, CreateWebhookEndpointRequest{
					Name: tc.name + "-unbound-ep",
				})
				require.Equal(t, http.StatusCreated, unboundRec.Code, unboundRec.Body.String())
				var unbound WebhookEndpointCreatedResponse
				require.NoError(t, json.Unmarshal(unboundRec.Body.Bytes(), &unbound))

				// Update description + rotate-secret on own endpoint.
				desc := "updated by " + tc.name
				assert.Equal(t, http.StatusOK, f.doWebhookEndpoint(t, tc.auth, created.ID, http.MethodPatch, UpdateWebhookEndpointRequest{
					Description: &desc,
				}).Code)
				assert.Equal(t, http.StatusOK, f.doWebhookEndpoint(t, tc.auth, created.ID+"/rotate-secret", http.MethodPost, nil).Code)

				// Create message-to-self trigger, pause, resume, PATCH refused (agentTriggerEditAllowed=false), delete.
				trRec := f.doTrigger(t, tc.auth, "", http.MethodPost, CreateTriggerRequest{
					Name:   tc.name + "-msg-self",
					Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: unbound.ID},
					Action: TriggerActionSpec{Type: store.TriggerActionMessage, AgentID: tc.agent.ID, Message: "hi"},
				})
				require.Equal(t, http.StatusCreated, trRec.Code, trRec.Body.String())
				var tr TriggerResponse
				require.NoError(t, json.Unmarshal(trRec.Body.Bytes(), &tr))

				assert.Equal(t, http.StatusForbidden, f.doTrigger(t, tc.auth, tr.ID, http.MethodPatch, UpdateTriggerRequest{Description: &desc}).Code)
				assert.Equal(t, http.StatusOK, f.doTrigger(t, tc.auth, tr.ID+"/pause", http.MethodPost, nil).Code)
				assert.Equal(t, http.StatusOK, f.doTrigger(t, tc.auth, tr.ID+"/resume", http.MethodPost, nil).Code)
				assert.Equal(t, http.StatusNoContent, f.doTrigger(t, tc.auth, tr.ID, http.MethodDelete, nil).Code)
				assert.Equal(t, http.StatusNoContent, f.doWebhookEndpoint(t, tc.auth, unbound.ID, http.MethodDelete, nil).Code)
				assert.Equal(t, http.StatusNoContent, f.doWebhookEndpoint(t, tc.auth, created.ID, http.MethodDelete, nil).Code)
			})
		}
	})

	t.Run("users_and_uat_unchanged", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			auth Identity
		}{
			{"owner", f.ownerAuth},
			{"uat", fullUAT},
		} {
			t.Run(tc.name, func(t *testing.T) {
				epRec := f.doWebhookEndpoint(t, tc.auth, "", http.MethodPost, CreateWebhookEndpointRequest{
					Name: tc.name + "-floor-ep",
				})
				require.Equal(t, http.StatusCreated, epRec.Code, epRec.Body.String())
				var ep WebhookEndpointCreatedResponse
				require.NoError(t, json.Unmarshal(epRec.Body.Bytes(), &ep))

				desc := "user update"
				assert.Equal(t, http.StatusOK, f.doWebhookEndpoint(t, tc.auth, ep.ID, http.MethodPatch, UpdateWebhookEndpointRequest{
					Description: &desc,
				}).Code)
				assert.Equal(t, http.StatusOK, f.doWebhookEndpoint(t, tc.auth, ep.ID+"/rotate-secret", http.MethodPost, nil).Code)

				trRec := f.doTrigger(t, tc.auth, "", http.MethodPost, CreateTriggerRequest{
					Name:   tc.name + "-floor-tr",
					Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
					Action: TriggerActionSpec{Type: store.TriggerActionMessage, AgentID: f.targetAgent.ID, Message: "hi"},
				})
				require.Equal(t, http.StatusCreated, trRec.Code, trRec.Body.String())
				var tr TriggerResponse
				require.NoError(t, json.Unmarshal(trRec.Body.Bytes(), &tr))

				assert.Equal(t, http.StatusOK, f.doTrigger(t, tc.auth, tr.ID, http.MethodPatch, UpdateTriggerRequest{Description: &desc}).Code)
				assert.Equal(t, http.StatusOK, f.doTrigger(t, tc.auth, tr.ID+"/pause", http.MethodPost, nil).Code)
				assert.Equal(t, http.StatusOK, f.doTrigger(t, tc.auth, tr.ID+"/resume", http.MethodPost, nil).Code)
				assert.Equal(t, http.StatusNoContent, f.doTrigger(t, tc.auth, tr.ID, http.MethodDelete, nil).Code)
				assert.Equal(t, http.StatusNoContent, f.doWebhookEndpoint(t, tc.auth, ep.ID, http.MethodDelete, nil).Code)
			})
		}

		// Viewer remains 403 on create.
		assert.Equal(t, http.StatusForbidden, f.doWebhookEndpoint(t, f.viewerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{Name: "viewer-ep"}).Code)
		assert.Equal(t, http.StatusForbidden, f.doTrigger(t, f.viewerAuth, "", http.MethodPost, CreateTriggerRequest{Name: "viewer-tr"}).Code)
	})
}

// TestTriggerAgentFloor_FireTimeDowngradeRefused verifies that an agent-authored
// message-to-self trigger whose author is subsequently downgraded below baseline
// (readonly or none) is refused at fire time with errScheduledMessageRefused,
// while baseline/full agent triggers, user triggers, and agent schedules remain
// authorized.
func TestTriggerAgentFloor_FireTimeDowngradeRefused(t *testing.T) {
	f := setupTriggersTestFixture(t)
	ctx := context.Background()

	ag, agAuth := createRoleAgentForFloorTest(t, f, "downgrade-agent", AgentRoleBaseline)

	// Author a self endpoint + message-to-self trigger while baseline.
	epRec := f.doWebhookEndpoint(t, agAuth, "", http.MethodPost, map[string]any{
		"name":          "downgrade-self-ep",
		"boundAgentId":  "self",
		"boundLifetime": "agent",
		"trigger":       map[string]any{"message": "ping"},
	})
	require.Equal(t, http.StatusCreated, epRec.Code, epRec.Body.String())
	var created WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(epRec.Body.Bytes(), &created))

	storedTr, err := f.store.GetTrigger(ctx, created.TriggerID)
	require.NoError(t, err)
	acts, err := triggerActions(*storedTr)
	require.NoError(t, err)
	require.Len(t, acts, 1)

	runAuthorize := func(defKind string) (*FireRefusal, error) {
		rec := hubfire.RecordedAuthority{
			DefinitionKind: defKind,
			DefinitionID:   storedTr.ID,
			FireID:         api.NewUUID(),
			Attribution:    storedTr.InitiatorAttribution,
			Ceiling:        storedTr.AuthorityCeiling,
		}
		auth, identity, rErr := f.srv.resolveRecordedAuthority(ctx, rec, f.projectID, scheduledMessagePermission)
		if rErr != nil {
			return nil, rErr
		}
		ma := &messageAction{srv: f.srv}
		_, refusal, aErr := ma.Authorize(ctx, hubfire.FireRequest{
			ProjectID:     f.projectID,
			ActionType:    store.TriggerActionMessage,
			ActionPayload: acts[0].Payload,
			Authority:     rec,
		}, auth, identity)
		return refusal, aErr
	}

	// 1. While baseline: trigger fire is authorized.
	refusal, err := runAuthorize(hubfire.DefinitionKindTrigger)
	require.NoError(t, err)
	assert.Nil(t, refusal, "baseline agent trigger should be authorized at fire time")

	// 2. Downgrade agent role to readonly -> trigger fire is refused.
	ag.AppliedConfig.AgentRole = string(AgentRoleReadOnly)
	require.NoError(t, f.store.UpdateAgent(ctx, ag))

	refusal, err = runAuthorize(hubfire.DefinitionKindTrigger)
	require.NoError(t, err)
	require.NotNil(t, refusal, "readonly-downgraded agent trigger must be refused at fire time")
	assert.Equal(t, errScheduledMessageRefused.Error(), refusal.Reason)

	// 3. Downgrade agent role to none -> trigger fire is refused.
	ag.AppliedConfig.AgentRole = string(AgentRoleNone)
	require.NoError(t, f.store.UpdateAgent(ctx, ag))

	refusal, err = runAuthorize(hubfire.DefinitionKindTrigger)
	require.NoError(t, err)
	require.NotNil(t, refusal, "none-downgraded agent trigger must be refused at fire time")
	assert.Equal(t, errScheduledMessageRefused.Error(), refusal.Reason)

	// 4. Schedules are unaffected by the trigger-specific baseline floor.
	ag.AppliedConfig.AgentRole = string(AgentRoleReadOnly)
	require.NoError(t, f.store.UpdateAgent(ctx, ag))
	refusal, err = runAuthorize(hubfire.DefinitionKindSchedule)
	require.NoError(t, err)
	assert.Nil(t, refusal, "schedule fire must remain unaffected")

	// 5. Restore to full -> trigger fire is authorized again.
	ag.AppliedConfig.AgentRole = string(AgentRoleFull)
	require.NoError(t, f.store.UpdateAgent(ctx, ag))
	refusal, err = runAuthorize(hubfire.DefinitionKindTrigger)
	require.NoError(t, err)
	assert.Nil(t, refusal, "full agent trigger should be authorized at fire time")
}
