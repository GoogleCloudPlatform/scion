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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTriggerResponseGuard_StructFields(t *testing.T) {
	now := time.Now().UTC()

	epResp := WebhookEndpointResponse{
		ID:            "ep-1",
		ProjectID:     "proj-1",
		Name:          "deploy-hook",
		Description:   "desc",
		URL:           "https://hub.example.com/api/v1/hooks/ep-1",
		Status:        store.WebhookEndpointStatusActive,
		SignatureMode: store.WebhookSignatureModeScionV1,
		SecretVersion: 1,
		CreatedBy:     "user-1",
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	epRaw, err := json.Marshal(epResp)
	require.NoError(t, err)
	var epMap map[string]any
	require.NoError(t, json.Unmarshal(epRaw, &epMap))
	for _, forbidden := range []string{
		"secret",
		"ownerPrincipalKind", "ownerPrincipalId",
		"owner_principal_kind", "owner_principal_id",
	} {
		assert.NotContains(t, epMap, forbidden, "WebhookEndpointResponse must not expose %q", forbidden)
	}

	trResp := buildTriggerResponse(store.Trigger{
		ID:             "tr-1",
		ProjectID:      "proj-1",
		Name:           "notify-lead",
		Status:         store.TriggerStatusActive,
		SourceKind:     store.TriggerSourceKindWebhook,
		SourceID:       "ep-1",
		MatchTypes:     []string{"webhook.received"},
		MatchDialect:   "cel",
		ActionType:     "message",
		ActionPayload:  `{"agentId":"ag-1","agentName":"lead","message":"hi","wake":true}`,
		SubjectDialect: "cel",
		SubjectMode:    store.TriggerSubjectModeNew,
		CreatedBy:      "user-1",
		InitiatorAttribution: store.InitiatorAttribution{
			InitiatorPrincipalKind:  "user",
			InitiatorPrincipalID:    "user-1",
			InitiatorCredentialKind: store.InitiatorCredentialKindUAT,
			InitiatorCredentialID:   "uat-secret-id",
			AttributionVersion:      1,
			AuthorizationRevision:   3,
		},
		AuthorityCeiling: store.EffectCeiling{
			Kind:          store.EffectCeilingBounded,
			Version:       1,
			PermissionIDs: []string{"trigger.create"},
		},
		Fixture: store.FixtureProvenance{
			RootID:   "ti-secret-id",
			IssuerID: "ti-issuer-id",
		},
		CreatedAt: now,
		UpdatedAt: now,
	})
	trRaw, err := json.Marshal(trResp)
	require.NoError(t, err)
	var trMap map[string]any
	require.NoError(t, json.Unmarshal(trRaw, &trMap))
	for _, forbidden := range []string{
		"initiatorPrincipalKind", "initiatorPrincipalId",
		"initiatorCredentialKind", "initiatorCredentialId",
		"attributionVersion", "authorityCeiling",
		"initiator_principal_kind", "initiator_principal_id",
		"initiator_credential_kind", "initiator_credential_id",
		"authority_ceiling",
		"fixture", "rootId", "issuerId",
		"fixtureRooted", "fixtureTestIdentityId",
		"fixture_rooted", "fixture_test_identity_id",
		"actionPayload", "action_payload",
	} {
		assert.NotContains(t, trMap, forbidden, "TriggerResponse must not expose %q", forbidden)
	}
	assert.Equal(t, float64(3), trMap["authorizationRevision"])
	matchMap, ok := trMap["match"].(map[string]any)
	require.True(t, ok, "TriggerResponse.match must serialize as an object")
	assert.Equal(t, []any{}, matchMap["rules"], "0-element TriggerResponse.match.rules must serialize as []")
	assert.NotContains(t, matchMap, "dialect", "0-element TriggerResponse.match must not mirror dialect")
	assert.NotContains(t, matchMap, "expr", "0-element TriggerResponse.match must not mirror expr")
	assert.NotContains(t, matchMap, "rulesBroken", "healthy TriggerResponse.match must omit rulesBroken")

	corruptRaw, err := json.Marshal(buildTriggerResponse(store.Trigger{
		ID:                "tr-corrupt",
		ProjectID:         "proj-1",
		Name:              "corrupt-rule",
		Status:            store.TriggerStatusPaused,
		SourceKind:        store.TriggerSourceKindWebhook,
		SourceID:          "ep-1",
		MatchTypes:        []string{"webhook.received"},
		MatchRule:         "legacy-must-not-leak",
		MatchRulesCorrupt: true,
		ActionType:        "message",
	}))
	require.NoError(t, err)
	assert.NotContains(t, string(corruptRaw), "legacy-must-not-leak")
	var corruptMap map[string]any
	require.NoError(t, json.Unmarshal(corruptRaw, &corruptMap))
	corruptMatch := corruptMap["match"].(map[string]any)
	assert.Equal(t, []any{}, corruptMatch["rules"])
	assert.Equal(t, true, corruptMatch["rulesBroken"])
	assert.NotContains(t, corruptMatch, "dialect")
	assert.NotContains(t, corruptMatch, "expr")

	oneRuleRaw, err := json.Marshal(buildTriggerResponse(store.Trigger{
		ID:         "tr-one",
		ProjectID:  "proj-1",
		Name:       "one-rule",
		Status:     store.TriggerStatusActive,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   "ep-1",
		MatchTypes: []string{"webhook.received"},
		MatchRules: []store.TriggerMatchRule{{Dialect: "cel", Expr: "event.type == 'webhook.received'"}},
		ActionType: "message",
	}))
	require.NoError(t, err)
	var oneRuleMap map[string]any
	require.NoError(t, json.Unmarshal(oneRuleRaw, &oneRuleMap))
	oneMatch := oneRuleMap["match"].(map[string]any)
	assert.Len(t, oneMatch["rules"], 1)
	assert.Equal(t, "cel", oneMatch["dialect"])
	assert.Equal(t, "event.type == 'webhook.received'", oneMatch["expr"])

	twoRuleRaw, err := json.Marshal(buildTriggerResponse(store.Trigger{
		ID:         "tr-two",
		ProjectID:  "proj-1",
		Name:       "two-rule",
		Status:     store.TriggerStatusActive,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   "ep-1",
		MatchTypes: []string{"webhook.received"},
		MatchRules: []store.TriggerMatchRule{
			{Dialect: "cel", Expr: "true"},
			{Dialect: "cel", Expr: "has(event.payload.pr)"},
		},
		ActionType: "message",
	}))
	require.NoError(t, err)
	var twoRuleMap map[string]any
	require.NoError(t, json.Unmarshal(twoRuleRaw, &twoRuleMap))
	twoMatch := twoRuleMap["match"].(map[string]any)
	assert.Len(t, twoMatch["rules"], 2)
	assert.NotContains(t, twoMatch, "dialect", "multi-element TriggerResponse.match must not mirror dialect")
	assert.NotContains(t, twoMatch, "expr", "multi-element TriggerResponse.match must not mirror expr")

	fireResp := buildTriggerFireResponse(store.TriggerFire{
		ID:         "fire-1",
		TriggerID:  "tr-1",
		EventID:    "evt-1",
		ProjectID:  "proj-1",
		ActionType: "message",
		Status:     store.TriggerFireStatusFired,
		CreatedAt:  now,
		StartedAt:  now,
		LeaseUntil: now.Add(time.Minute),
		ResultRef:  "msg-1",
	})
	fireRaw, err := json.Marshal(fireResp)
	require.NoError(t, err)
	var fireMap map[string]any
	require.NoError(t, json.Unmarshal(fireRaw, &fireMap))
	for _, forbidden := range []string{"leaseUntil", "lease_until", "executorKind", "executorId"} {
		assert.NotContains(t, fireMap, forbidden, "TriggerFireResponse must not expose %q", forbidden)
	}

	evtSummary := buildTriggerEventSummaryResponse(store.TriggerEvent{
		ID:           "evt-1",
		ProjectID:    "proj-1",
		SourceKind:   store.TriggerSourceKindWebhook,
		SourceID:     "ep-1",
		Type:         "webhook.received",
		Subject:      "webhook:ep-1",
		DedupKey:     "d-1",
		OccurredAt:   now,
		ReceivedAt:   now,
		Attributes:   map[string]string{"endpoint_name": "deploy-hook"},
		Headers:      map[string]string{"Content-Type": "application/json"},
		Payload:      `{"secret_body":true}`,
		PayloadBytes: 20,
		Status:       store.TriggerEventStatusProcessed,
		CreatedAt:    now,
	})
	evtSummaryRaw, err := json.Marshal(evtSummary)
	require.NoError(t, err)
	var evtSummaryMap map[string]any
	require.NoError(t, json.Unmarshal(evtSummaryRaw, &evtSummaryMap))
	for _, forbidden := range []string{"payload", "headers", "lists"} {
		assert.NotContains(t, evtSummaryMap, forbidden, "TriggerEventSummaryResponse must not expose %q when omitted", forbidden)
	}

	evtWithLists := buildTriggerEventDetailResponse(store.TriggerEvent{
		ID:         "evt-gh-1",
		ProjectID:  "proj-1",
		SourceKind: store.TriggerSourceKindGitHub,
		SourceID:   "12345",
		Type:       "github.pull_request.opened",
		Subject:    "github:acme/widgets#7",
		DedupKey:   "72d3162e-cc78-11e3-81ab-4c9367dc0958/proj-1",
		OccurredAt: now,
		ReceivedAt: now,
		Attributes: map[string]string{"provider": "github", "repo": "acme/widgets"},
		Lists:      map[string][]string{"labels": {"bug", "urgent"}},
		Headers:    map[string]string{"X-GitHub-Event": "pull_request"},
		Payload:    `{"action":"opened"}`,
		Status:     store.TriggerEventStatusPending,
		CreatedAt:  now,
	})
	assert.Equal(t, map[string][]string{"labels": {"bug", "urgent"}}, evtWithLists.Lists)
}

func TestWebhookEndpointResponses_NoSecretAfterCreate(t *testing.T) {
	f := setupTriggersTestFixture(t)
	ctx := context.Background()

	// Create endpoint as owner -> secret returned ONCE.
	createRec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{
		Name:        "guard-hook",
		Description: "guard test",
	})
	require.Equal(t, http.StatusCreated, createRec.Code, createRec.Body.String())
	var created WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(createRec.Body.Bytes(), &created))
	require.NotEmpty(t, created.Secret, "POST /webhook-endpoints must return secret once")
	sentinel := created.Secret

	// Create active + paused triggers referencing the endpoint.
	trCreateRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "guard-trigger",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: created.ID},
		Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "hello"},
	})
	require.Equal(t, http.StatusCreated, trCreateRec.Code, trCreateRec.Body.String())
	assert.NotContains(t, trCreateRec.Body.String(), sentinel)
	var tr TriggerResponse
	require.NoError(t, json.Unmarshal(trCreateRec.Body.Bytes(), &tr))

	now := time.Now().UTC()
	evt := &store.TriggerEvent{
		ID:           api.NewUUID(),
		ProjectID:    f.projectID,
		SourceKind:   store.TriggerSourceKindWebhook,
		SourceID:     created.ID,
		Type:         "webhook.received",
		Subject:      "webhook:" + created.ID,
		DedupKey:     "guard-deliv",
		OccurredAt:   now,
		ReceivedAt:   now,
		Attributes:   map[string]string{"endpoint_name": created.Name},
		Headers:      map[string]string{"Content-Type": "application/json"},
		Payload:      `{"ok":true}`,
		PayloadBytes: 11,
		Status:       store.TriggerEventStatusProcessed,
		CreatedAt:    now,
	}
	require.NoError(t, f.store.CreateTriggerEvent(ctx, evt))
	claimed, _, err := f.store.ClaimTriggerFire(ctx, &store.TriggerFire{
		ID:           api.NewUUID(),
		ProjectID:    f.projectID,
		TriggerID:    tr.ID,
		EventID:      evt.ID,
		Subject:      evt.Subject,
		ActionType:   store.TriggerActionMessage,
		Status:       store.TriggerFireStatusFiring,
		ExecutorKind: "trigger_drainer",
		ExecutorID:   "trigger_event:" + evt.ID,
		LeaseUntil:   now.Add(10 * time.Minute),
		StartedAt:    now,
		CreatedAt:    now,
	})
	require.NoError(t, err)
	require.True(t, claimed)

	// Exercise every 1b-1 and 1b-3 route and verify sentinel never appears.
	checks := []struct {
		name string
		call func() string
	}{
		{
			name: "GET /webhook-endpoints (owner)",
			call: func() string {
				rec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodGet, nil)
				require.Equal(t, http.StatusOK, rec.Code)
				return rec.Body.String()
			},
		},
		{
			name: "GET /webhook-endpoints (member)",
			call: func() string {
				rec := f.doWebhookEndpoint(t, f.memberAuth, "", http.MethodGet, nil)
				require.Equal(t, http.StatusOK, rec.Code)
				return rec.Body.String()
			},
		},
		{
			name: "GET /webhook-endpoints/{id} (owner)",
			call: func() string {
				rec := f.doWebhookEndpoint(t, f.ownerAuth, created.ID, http.MethodGet, nil)
				require.Equal(t, http.StatusOK, rec.Code)
				return rec.Body.String()
			},
		},
		{
			name: "GET /triggers",
			call: func() string {
				rec := f.doTrigger(t, f.ownerAuth, "", http.MethodGet, nil)
				require.Equal(t, http.StatusOK, rec.Code)
				return rec.Body.String()
			},
		},
		{
			name: "GET /triggers/{id}",
			call: func() string {
				rec := f.doTrigger(t, f.ownerAuth, tr.ID, http.MethodGet, nil)
				require.Equal(t, http.StatusOK, rec.Code)
				return rec.Body.String()
			},
		},
		{
			name: "PATCH /triggers/{id}",
			call: func() string {
				desc := "updated"
				rec := f.doTrigger(t, f.ownerAuth, tr.ID, http.MethodPatch, UpdateTriggerRequest{Description: &desc})
				require.Equal(t, http.StatusOK, rec.Code)
				return rec.Body.String()
			},
		},
		{
			name: "POST /triggers/{id}/pause",
			call: func() string {
				rec := f.doTrigger(t, f.ownerAuth, tr.ID+"/pause", http.MethodPost, nil)
				require.Equal(t, http.StatusOK, rec.Code)
				return rec.Body.String()
			},
		},
		{
			name: "POST /triggers/{id}/resume",
			call: func() string {
				rec := f.doTrigger(t, f.ownerAuth, tr.ID+"/resume", http.MethodPost, nil)
				require.Equal(t, http.StatusOK, rec.Code)
				return rec.Body.String()
			},
		},
		{
			name: "GET /triggers/{id}/history",
			call: func() string {
				rec := f.doTrigger(t, f.ownerAuth, tr.ID+"/history", http.MethodGet, nil)
				require.Equal(t, http.StatusOK, rec.Code)
				return rec.Body.String()
			},
		},
		{
			name: "GET /trigger-events",
			call: func() string {
				rec := f.doTriggerEvents(t, f.ownerAuth, "", http.MethodGet, nil)
				require.Equal(t, http.StatusOK, rec.Code)
				return rec.Body.String()
			},
		},
		{
			name: "GET /trigger-events/{id}",
			call: func() string {
				rec := f.doTriggerEvents(t, f.ownerAuth, evt.ID, http.MethodGet, nil)
				require.Equal(t, http.StatusOK, rec.Code)
				return rec.Body.String()
			},
		},
		{
			name: "DELETE /triggers/{id}",
			call: func() string {
				rec := f.doTrigger(t, f.ownerAuth, tr.ID, http.MethodDelete, nil)
				require.Equal(t, http.StatusNoContent, rec.Code)
				return rec.Body.String()
			},
		},
		{
			name: "DELETE /webhook-endpoints/{id}",
			call: func() string {
				rec := f.doWebhookEndpoint(t, f.ownerAuth, created.ID, http.MethodDelete, nil)
				require.Equal(t, http.StatusNoContent, rec.Code)
				return rec.Body.String()
			},
		},
	}

	for _, tc := range checks {
		body := tc.call()
		assert.NotContains(t, body, sentinel, "%s response must never contain endpoint secret", tc.name)
	}

	// Verify mutation audit records never carry the secret value.
	audits, _, err := f.store.ListMutationAudits(ctx, store.MutationAuditFilter{})
	require.NoError(t, err)
	require.NotEmpty(t, audits)
	for _, rec := range audits {
		assert.NotContains(t, rec.BeforeSummary, sentinel, "audit before_summary must not contain endpoint secret")
		assert.NotContains(t, rec.AfterSummary, sentinel, "audit after_summary must not contain endpoint secret")
	}
}
