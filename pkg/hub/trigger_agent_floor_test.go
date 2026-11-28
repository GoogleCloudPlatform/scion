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
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func createRoleAgentForFloorTest(t *testing.T, f *triggersTestFixture, slug string, role AgentRole) (*store.Agent, AgentIdentity, string) {
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
	edgeID := api.NewUUID()
	require.NoError(t, f.store.CreateDelegationEdge(ctx, &store.DelegationEdge{
		ID:            edgeID,
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
	return ag, jwtID, edgeID
}

func assertForbiddenAuthoringFloor(t *testing.T, code int, body string) {
	t.Helper()
	assert.Equal(t, http.StatusForbidden, code, "expected 403, body=%s", body)
	assert.Contains(t, body, `"code":"forbidden"`)
	assert.Contains(t, body, agentAuthoringFloorForbiddenMessage)
}

func seedROEndpointForFloorTest(t *testing.T, f *triggersTestFixture, ag *store.Agent, name string) *store.WebhookEndpoint {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	ep := &store.WebhookEndpoint{
		ID:                  api.NewUUID(),
		ProjectID:           f.projectID,
		Name:                name,
		OwnerPrincipalKind:  store.DelegationPrincipalAgent,
		OwnerPrincipalID:    ag.ID,
		OwnerCredentialKind: store.InitiatorCredentialKindAgent,
		AuthMode:            store.WebhookAuthModeHMAC,
		SignatureMode:       store.WebhookSignatureModeScionV1,
		SecretVersion:       1,
		Status:              store.WebhookEndpointStatusActive,
		CreatedBy:           ag.ID,
		CreatedAt:           now,
		UpdatedAt:           now,
	}
	require.NoError(t, f.store.CreateWebhookEndpoint(context.Background(), ep))
	return ep
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

	roAgent, roAuth, _ := createRoleAgentForFloorTest(t, f, "ro-agent", AgentRoleReadOnly)
	baseAgent, baseAuth, _ := createRoleAgentForFloorTest(t, f, "base-agent", AgentRoleBaseline)
	fullAgent, fullAuth, _ := createRoleAgentForFloorTest(t, f, "full-agent", AgentRoleFull)

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
		ID:           api.NewUUID(),
		ProjectID:    f.projectID,
		SourceKind:   store.TriggerSourceKindWebhook,
		SourceID:     roEp.ID,
		Type:         "webhook.received",
		Subject:      "floor-test-subject",
		DedupKey:     "floor-test-dedup",
		Status:       store.TriggerEventStatusProcessed,
		Payload:      `{"ok":true}`,
		PayloadBytes: len(`{"ok":true}`),
		OccurredAt:   now,
		ReceivedAt:   now,
		CreatedAt:    now,
	}
	require.NoError(t, f.store.CreateTriggerEvent(ctx, roEvent))

	t.Run("readonly_agent_refused_on_create_and_widening_routes", func(t *testing.T) {
		// 1. POST /webhook-endpoints (unbound)
		rec := f.doWebhookEndpoint(t, roAuth, "", http.MethodPost, CreateWebhookEndpointRequest{
			Name: "ro-unbound-ep",
		})
		assertForbiddenAuthoringFloor(t, rec.Code, rec.Body.String())

		// 2. POST /webhook-endpoints (--to-self with composite trigger)
		rec = f.doWebhookEndpoint(t, roAuth, "", http.MethodPost, map[string]any{
			"name":          "ro-self-ep",
			"boundAgentId":  "self",
			"boundLifetime": "agent",
			"trigger":       map[string]any{"message": "hi"},
		})
		assertForbiddenAuthoringFloor(t, rec.Code, rec.Body.String())

		// 3. PATCH /webhook-endpoints/{id} (description update on own endpoint)
		desc := "mutated"
		rec = f.doWebhookEndpoint(t, roAuth, roEp.ID, http.MethodPatch, UpdateWebhookEndpointRequest{
			Description: &desc,
		})
		assertForbiddenAuthoringFloor(t, rec.Code, rec.Body.String())

		// 4. PATCH /webhook-endpoints/{id} (re-enable disabled own endpoint)
		activeStatus := store.WebhookEndpointStatusActive
		rec = f.doWebhookEndpoint(t, roAuth, roDisabledEp.ID, http.MethodPatch, UpdateWebhookEndpointRequest{
			Status: &activeStatus,
		})
		assertForbiddenAuthoringFloor(t, rec.Code, rec.Body.String())

		// 5. PATCH /webhook-endpoints/{id} (mixed disable + description)
		disabledStatus := store.WebhookEndpointStatusDisabled
		rec = f.doWebhookEndpoint(t, roAuth, roEp.ID, http.MethodPatch, UpdateWebhookEndpointRequest{
			Status:      &disabledStatus,
			Description: &desc,
		})
		assertForbiddenAuthoringFloor(t, rec.Code, rec.Body.String())

		// 6. PATCH /webhook-endpoints/{id} (empty body)
		rec = f.doWebhookEndpoint(t, roAuth, roEp.ID, http.MethodPatch, map[string]any{})
		assertForbiddenAuthoringFloor(t, rec.Code, rec.Body.String())

		// 7. POST /webhook-endpoints/{id}/rotate-secret
		rec = f.doWebhookEndpoint(t, roAuth, roEp.ID+"/rotate-secret", http.MethodPost, nil)
		assertForbiddenAuthoringFloor(t, rec.Code, rec.Body.String())

		// 8. POST /triggers (message-to-self)
		rec = f.doTrigger(t, roAuth, "", http.MethodPost, CreateTriggerRequest{
			Name:   "ro-new-trigger",
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: roEp.ID},
			Action: TriggerActionSpec{Type: store.TriggerActionMessage, AgentID: roAgent.ID, Message: "hi"},
		})
		assertForbiddenAuthoringFloor(t, rec.Code, rec.Body.String())

		// 9. PATCH /triggers/{id}
		rec = f.doTrigger(t, roAuth, roTrigger.ID, http.MethodPatch, UpdateTriggerRequest{
			Description: &desc,
		})
		assertForbiddenAuthoringFloor(t, rec.Code, rec.Body.String())

		// 10. POST /triggers/{id}/resume
		rec = f.doTrigger(t, roAuth, roPausedTrigger.ID+"/resume", http.MethodPost, nil)
		assertForbiddenAuthoringFloor(t, rec.Code, rec.Body.String())
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
// (by role or by a tightened bounded delegation ceiling) is refused at fire
// time with errScheduledMessageRefused, that unset/unknown definition kinds
// fail closed, and that explicit schedule definitions and baseline/full agents
// remain authorized.
func TestTriggerAgentFloor_FireTimeDowngradeRefused(t *testing.T) {
	f := setupTriggersTestFixture(t)
	f.srv.authzService.mintDevAuthOverride = false
	ctx := context.Background()

	ag, agAuth, edgeID := createRoleAgentForFloorTest(t, f, "downgrade-agent", AgentRoleBaseline)

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
			ProjectID:  f.projectID,
			ActionType: store.TriggerActionMessage,
			Payload:    acts[0].Payload,
			Authority:  rec,
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

	// 3. Unset and unknown DefinitionKind fail closed for a below-baseline author.
	for _, kind := range []string{"", "future_kind"} {
		refusal, err = runAuthorize(kind)
		require.NoError(t, err)
		require.NotNil(t, refusal, "definition kind %q must fail closed for below-baseline author", kind)
		assert.Equal(t, errScheduledMessageRefused.Error(), refusal.Reason)
	}

	// 4. Downgrade agent role to none -> trigger fire is refused.
	ag.AppliedConfig.AgentRole = string(AgentRoleNone)
	require.NoError(t, f.store.UpdateAgent(ctx, ag))

	refusal, err = runAuthorize(hubfire.DefinitionKindTrigger)
	require.NoError(t, err)
	require.NotNil(t, refusal, "none-downgraded agent trigger must be refused at fire time")
	assert.Equal(t, errScheduledMessageRefused.Error(), refusal.Reason)

	// 5. Explicit schedules (recurring and one-shot) are unaffected by the floor.
	ag.AppliedConfig.AgentRole = string(AgentRoleReadOnly)
	require.NoError(t, f.store.UpdateAgent(ctx, ag))
	for _, schedKind := range []string{hubfire.DefinitionKindSchedule, hubfire.DefinitionKindScheduledEvent} {
		refusal, err = runAuthorize(schedKind)
		require.NoError(t, err)
		assert.Nil(t, refusal, "schedule kind %q must remain unaffected", schedKind)
	}

	// 6. Restore to full -> trigger fire is authorized again.
	ag.AppliedConfig.AgentRole = string(AgentRoleFull)
	require.NoError(t, f.store.UpdateAgent(ctx, ag))
	refusal, err = runAuthorize(hubfire.DefinitionKindTrigger)
	require.NoError(t, err)
	assert.Nil(t, refusal, "full agent trigger should be authorized at fire time")

	// 7. Bounded delegation ceiling vs revoked edge: agent.notify is in
	// selfOperationSet, so a bounded project.read ceiling still admits a
	// baseline agent (UAT-created baseline agents keep the floor), whereas
	// revoking the active delegation edge denies resolveRecordedAuthority.
	ag.AppliedConfig.AgentRole = string(AgentRoleBaseline)
	require.NoError(t, f.store.UpdateAgent(ctx, ag))
	now := time.Now().UTC()
	_, err = f.store.DeactivateDelegationEdge(ctx, store.DelegationPrincipalAgent, ag.ID, edgeID, store.Deactivation{
		Cause: store.EdgeDeactivationReincarnateReplaced,
		At:    &now,
		OpID:  api.NewUUID(),
	})
	require.NoError(t, err)
	boundedEdgeID := api.NewUUID()
	require.NoError(t, f.store.CreateDelegationEdge(ctx, &store.DelegationEdge{
		ID:            boundedEdgeID,
		DelegatorType: store.DelegationPrincipalUser,
		DelegatorID:   f.owner.ID,
		DelegateType:  store.DelegationPrincipalAgent,
		DelegateID:    ag.ID,
		ScopeType:     store.RoleScopeProject,
		ScopeID:       f.projectID,
		Role:          string(AgentRoleBaseline),
		EffectCeiling: store.EffectCeiling{
			Kind:              store.EffectCeilingBounded,
			Version:           permissions.CeilingVersionV1,
			PermissionIDs:     []string{"project.read"},
			BoundaryKind:      string(permissions.BoundaryKindProject),
			BoundaryProjectID: f.projectID,
		},
		Active:    true,
		CreatedAt: now,
	}))
	refusal, err = runAuthorize(hubfire.DefinitionKindTrigger)
	require.NoError(t, err)
	assert.Nil(t, refusal, "baseline agent under bounded project.read ceiling retains self-op agent.notify")
}

// TestTriggerAgentFloor_ReadonlyDisableMixedBodies verifies that every
// widening field mixed with status=disabled is refused for a readonly owner
// agent without mutating the row, whitespace-padded disable and explicit nulls
// are allowed as disable-only, and non-canonical case fails closed.
func TestTriggerAgentFloor_ReadonlyDisableMixedBodies(t *testing.T) {
	f := setupTriggersTestFixture(t)
	ro, roAuth, _ := createRoleAgentForFloorTest(t, f, "probe-ro", AgentRoleReadOnly)
	ep := seedROEndpointForFloorTest(t, f, ro, "probe-ro-ep")

	mixed := map[string]map[string]any{
		"boundAgentId":        {"status": "disabled", "boundAgentId": ro.ID},
		"boundAgentId_empty":  {"status": "disabled", "boundAgentId": ""},
		"boundLifetime":       {"status": "disabled", "boundLifetime": "agent"},
		"auth":                {"status": "disabled", "auth": "hub_token"},
		"allowProjectAgents":  {"status": "disabled", "allowProjectAgents": true},
		"allowProjectAgentsF": {"status": "disabled", "allowProjectAgents": false},
		"signatureMode":       {"status": "disabled", "signatureMode": "scion-v1"},
		"routingHints":        {"status": "disabled", "routingHints": map[string]any{"typePath": "/type"}},
		"routingHints_empty":  {"status": "disabled", "routingHints": map[string]any{}},
		"maxBodyBytes":        {"status": "disabled", "maxBodyBytes": 1024},
		"rateLimit":           {"status": "disabled", "rateLimit": map[string]any{"perMinute": 5}},
		"replayWindowSeconds": {"status": "disabled", "replayWindowSeconds": 60},
		"description":         {"status": "disabled", "description": ""},
		"status_active":       {"status": "active"},
		"status_uppercase":    {"status": "DISABLED"},
		"status_empty":        {"status": ""},
		"empty":               {},
	}
	for name, body := range mixed {
		t.Run(name, func(t *testing.T) {
			rec := f.doWebhookEndpoint(t, roAuth, ep.ID, http.MethodPatch, body)
			assertForbiddenAuthoringFloor(t, rec.Code, rec.Body.String())
		})
	}

	got, err := f.store.GetWebhookEndpoint(context.Background(), ep.ID)
	require.NoError(t, err)
	assert.Equal(t, store.WebhookEndpointStatusActive, got.Status, "no refused PATCH may have mutated the row")

	t.Run("whitespace_disable_allowed", func(t *testing.T) {
		rec := f.doWebhookEndpoint(t, roAuth, ep.ID, http.MethodPatch, map[string]any{"status": "  disabled "})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})
	t.Run("explicit_nulls_disable_allowed", func(t *testing.T) {
		rec := f.doWebhookEndpoint(t, roAuth, ep.ID, http.MethodPatch, map[string]any{"status": "disabled", "description": nil, "boundAgentId": nil})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})
	t.Run("readonly_cannot_reenable_after_disable", func(t *testing.T) {
		rec := f.doWebhookEndpoint(t, roAuth, ep.ID, http.MethodPatch, map[string]any{"status": "active"})
		assertForbiddenAuthoringFloor(t, rec.Code, rec.Body.String())
		got, err := f.store.GetWebhookEndpoint(context.Background(), ep.ID)
		require.NoError(t, err)
		assert.Equal(t, store.WebhookEndpointStatusDisabled, got.Status)
	})
	t.Run("readonly_nonexistent_widen_is_403_not_404", func(t *testing.T) {
		// Body-based refusal precedes row load: same answer for existing and
		// missing IDs, so no existence oracle.
		rec := f.doWebhookEndpoint(t, roAuth, api.NewUUID(), http.MethodPatch, map[string]any{"description": "x"})
		assertForbiddenAuthoringFloor(t, rec.Code, rec.Body.String())
	})
	t.Run("readonly_rotate_nonexistent_is_403", func(t *testing.T) {
		rec := f.doWebhookEndpoint(t, roAuth, api.NewUUID()+"/rotate-secret", http.MethodPost, nil)
		assertForbiddenAuthoringFloor(t, rec.Code, rec.Body.String())
	})
}

// TestTriggerAgentFloor_DowngradedAgentTrigger_UserResumeReattributes verifies
// that a readonly agent's pre-downgrade trigger is refused at fire time, and
// when the project owner resumes it, resume re-attributes to the owner so it
// fires under the owner's authority (user path unchanged).
func TestTriggerAgentFloor_DowngradedAgentTrigger_UserResumeReattributes(t *testing.T) {
	f := setupTriggersTestFixture(t)
	f.srv.authzService.mintDevAuthOverride = false
	ctx := context.Background()
	ag, agAuth, _ := createRoleAgentForFloorTest(t, f, "probe-dg", AgentRoleBaseline)

	epRec := f.doWebhookEndpoint(t, agAuth, "", http.MethodPost, map[string]any{
		"name": "probe-dg-ep", "boundAgentId": "self", "boundLifetime": "agent",
		"trigger": map[string]any{"message": "ping"},
	})
	require.Equal(t, http.StatusCreated, epRec.Code, epRec.Body.String())
	var created WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(epRec.Body.Bytes(), &created))

	authorize := func() *FireRefusal {
		tr, err := f.store.GetTrigger(ctx, created.TriggerID)
		require.NoError(t, err)
		acts, err := triggerActions(*tr)
		require.NoError(t, err)
		rec := hubfire.RecordedAuthority{
			DefinitionKind: hubfire.DefinitionKindTrigger, DefinitionID: tr.ID, FireID: api.NewUUID(),
			Attribution: tr.InitiatorAttribution, Ceiling: tr.AuthorityCeiling,
		}
		auth, identity, rErr := f.srv.resolveRecordedAuthority(ctx, rec, f.projectID, scheduledMessagePermission)
		require.NoError(t, rErr)
		_, refusal, aErr := (&messageAction{srv: f.srv}).Authorize(ctx, hubfire.FireRequest{
			ProjectID: f.projectID, ActionType: store.TriggerActionMessage, Payload: acts[0].Payload, Authority: rec,
		}, auth, identity)
		require.NoError(t, aErr)
		return refusal
	}

	require.Equal(t, http.StatusOK, f.doTrigger(t, agAuth, created.TriggerID+"/pause", http.MethodPost, nil).Code)
	ag.AppliedConfig.AgentRole = string(AgentRoleReadOnly)
	require.NoError(t, f.store.UpdateAgent(ctx, ag))

	require.NotNil(t, authorize(), "downgraded author must be refused")

	rec := f.doTrigger(t, f.ownerAuth, created.TriggerID+"/resume", http.MethodPost, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	tr, err := f.store.GetTrigger(ctx, created.TriggerID)
	require.NoError(t, err)
	assert.Equal(t, store.DelegationPrincipalUser, tr.InitiatorAttribution.InitiatorPrincipalKind)
	assert.Nil(t, authorize(), "owner-resumed trigger fires under owner authority")
}

// TestTriggerAgentFloor_StaleBaselineTokenAfterDowngrade_Observation documents
// that the request-path floor reads the bearer token's scopes (so a token
// minted while baseline keeps authoring until refresh/expiry), while the
// fire-time floor reads live state and refuses resulting fires.
func TestTriggerAgentFloor_StaleBaselineTokenAfterDowngrade_Observation(t *testing.T) {
	f := setupTriggersTestFixture(t)
	ctx := context.Background()
	ag, agAuth, _ := createRoleAgentForFloorTest(t, f, "probe-stale", AgentRoleBaseline)
	ag.AppliedConfig.AgentRole = string(AgentRoleReadOnly)
	require.NoError(t, f.store.UpdateAgent(ctx, ag))
	rec := f.doWebhookEndpoint(t, agAuth, "", http.MethodPost, CreateWebhookEndpointRequest{Name: "probe-stale-ep"})
	assert.Equal(t, http.StatusCreated, rec.Code, "documents token-scoped request floor")
}

// TestTriggerAgentFloor_MemberViewerUnchanged verifies that user callers
// (member, viewer) retain their existing policy behaviour and viewer refusals
// never cite the agent authoring floor.
func TestTriggerAgentFloor_MemberViewerUnchanged(t *testing.T) {
	f := setupTriggersTestFixture(t)
	mRec := f.doWebhookEndpoint(t, f.memberAuth, "", http.MethodPost, CreateWebhookEndpointRequest{Name: "probe-member-ep"})
	assert.Equal(t, http.StatusCreated, mRec.Code, mRec.Body.String())
	vRec := f.doWebhookEndpoint(t, f.viewerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{Name: "probe-viewer-ep"})
	assert.Equal(t, http.StatusForbidden, vRec.Code)
	assert.NotContains(t, vRec.Body.String(), agentAuthoringFloorForbiddenMessage, "user refusals must not cite the agent floor")
	assert.NotContains(t, vRec.Body.String(), "project:agent:notify", "user refusals must not cite the agent floor")
}
