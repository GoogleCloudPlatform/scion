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
	"encoding/json"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTriggerUntrustedVarsDerive_MessageAndWebSaveRepro(t *testing.T) {
	f := setupTriggersTestFixture(t)

	epRec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{
		Name: "derive-msg-ep",
	})
	require.Equal(t, http.StatusCreated, epRec.Code, epRec.Body.String())
	var ep WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(epRec.Body.Bytes(), &ep))

	t.Run("web message-editor save with omitted untrustedVars derives and stores commit path", func(t *testing.T) {
		// Mirrors the exact JSON shape sent by web/src/components/triggers/trigger-list.ts
		// when saving a message rule with ${event.payload.commit} and no untrustedVars field.
		webBody := map[string]any{
			"name":        "web-msg-commit",
			"description": "",
			"source":      map[string]any{"kind": store.TriggerSourceKindWebhook, "id": ep.ID},
			"match":       map[string]any{"types": []string{"webhook.received"}, "rules": []any{}},
			"subject":     map[string]any{"mode": "new"},
			"action": map[string]any{
				"type":      store.TriggerActionMessage,
				"agentId":   f.targetAgent.ID,
				"message":   "CI at ${event.payload.commit}",
				"wake":      false,
				"interrupt": false,
				"plain":     false,
				"offline":   "skip",
			},
		}
		rec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, webBody)
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		var created TriggerResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
		assert.Equal(t, []string{"event.payload.commit"}, created.Action.UntrustedVars)

		// Persisted on GET.
		getRec := f.doTrigger(t, f.ownerAuth, created.ID, http.MethodGet, nil)
		require.Equal(t, http.StatusOK, getRec.Code, getRec.Body.String())
		var got TriggerResponse
		require.NoError(t, json.Unmarshal(getRec.Body.Bytes(), &got))
		assert.Equal(t, []string{"event.payload.commit"}, got.Action.UntrustedVars)
	})

	t.Run("omitted derives optional ${path?} and deduplicates in appearance order", func(t *testing.T) {
		rec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
			Name:   "msg-optional-dedup",
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
			Action: TriggerActionSpec{
				Type:    store.TriggerActionMessage,
				AgentID: f.targetAgent.ID,
				Message: "Repo ${event.attributes.repo} at ${event.payload.commit?} (${event.attributes.repo})",
			},
		})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		var created TriggerResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
		assert.Equal(t, []string{"event.attributes.repo", "event.payload.commit"}, created.Action.UntrustedVars)
	})

	t.Run("omitted with trusted-only placeholders stores no untrustedVars", func(t *testing.T) {
		rec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
			Name:   "msg-trusted-only",
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
			Action: TriggerActionSpec{
				Type:    store.TriggerActionMessage,
				AgentID: f.targetAgent.ID,
				Message: "Event ${event.id} from ${trigger.name}",
			},
		})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		var created TriggerResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
		assert.Empty(t, created.Action.UntrustedVars)
	})

	t.Run("supplied covering wildcard and narrower/empty refusals", func(t *testing.T) {
		// Supplied trailing .* wildcard is accepted and preserved verbatim.
		recOK := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
			Name:   "msg-wildcard-ok",
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
			Action: TriggerActionSpec{
				Type:          store.TriggerActionMessage,
				AgentID:       f.targetAgent.ID,
				Message:       "CI at ${event.payload.commit}",
				UntrustedVars: []string{"event.payload.*"},
			},
		})
		require.Equal(t, http.StatusCreated, recOK.Code, recOK.Body.String())
		var created TriggerResponse
		require.NoError(t, json.Unmarshal(recOK.Body.Bytes(), &created))
		assert.Equal(t, []string{"event.payload.*"}, created.Action.UntrustedVars)

		// Non-action PATCH (rename) leaves existing stored untrustedVars untouched.
		renameRec := f.doTrigger(t, f.ownerAuth, created.ID, http.MethodPatch, map[string]any{
			"name": "msg-wildcard-renamed",
		})
		require.Equal(t, http.StatusOK, renameRec.Code, renameRec.Body.String())
		var renamed TriggerResponse
		require.NoError(t, json.Unmarshal(renameRec.Body.Bytes(), &renamed))
		assert.Equal(t, []string{"event.payload.*"}, renamed.Action.UntrustedVars)

		// Supplied narrower list missing a placeholder -> 400 untrusted_var_not_allowed.
		recNarrow := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
			Name:   "msg-narrow-fail",
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
			Action: TriggerActionSpec{
				Type:          store.TriggerActionMessage,
				AgentID:       f.targetAgent.ID,
				Message:       "${event.attributes.repo} ${event.payload.commit}",
				UntrustedVars: []string{"event.attributes.repo"},
			},
		})
		require.Equal(t, http.StatusBadRequest, recNarrow.Code, recNarrow.Body.String())
		assert.Equal(t, "untrusted_var_not_allowed", decodeAPIErrorCode(t, recNarrow))

		// Explicit empty list [] -> 400 untrusted_var_not_allowed.
		recEmpty := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, map[string]any{
			"name":   "msg-empty-fail",
			"source": TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
			"action": map[string]any{
				"type":          store.TriggerActionMessage,
				"agentId":       f.targetAgent.ID,
				"message":       "CI at ${event.payload.commit}",
				"untrustedVars": []string{},
			},
		})
		require.Equal(t, http.StatusBadRequest, recEmpty.Code, recEmpty.Body.String())
		assert.Equal(t, "untrusted_var_not_allowed", decodeAPIErrorCode(t, recEmpty))
	})

	t.Run("PATCH adding or removing placeholders without untrustedVars re-derives", func(t *testing.T) {
		rec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
			Name:   "msg-patch-rederive",
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
			Action: TriggerActionSpec{
				Type:    store.TriggerActionMessage,
				AgentID: f.targetAgent.ID,
				Message: "Initial ${event.attributes.env}",
			},
		})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		var created TriggerResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
		assert.Equal(t, []string{"event.attributes.env"}, created.Action.UntrustedVars)

		// Add a second placeholder without untrustedVars -> re-derives both.
		patchAdd := f.doTrigger(t, f.ownerAuth, created.ID, http.MethodPatch, map[string]any{
			"action": map[string]any{
				"type":    store.TriggerActionMessage,
				"agentId": f.targetAgent.ID,
				"message": "Deploy ${event.attributes.env} at ${event.payload.commit?}",
			},
		})
		require.Equal(t, http.StatusOK, patchAdd.Code, patchAdd.Body.String())
		var added TriggerResponse
		require.NoError(t, json.Unmarshal(patchAdd.Body.Bytes(), &added))
		assert.Equal(t, []string{"event.attributes.env", "event.payload.commit"}, added.Action.UntrustedVars)

		// Remove all untrusted placeholders without untrustedVars -> clears list.
		patchClear := f.doTrigger(t, f.ownerAuth, created.ID, http.MethodPatch, map[string]any{
			"action": map[string]any{
				"type":    store.TriggerActionMessage,
				"agentId": f.targetAgent.ID,
				"message": "Plain trusted ${event.id}",
			},
		})
		require.Equal(t, http.StatusOK, patchClear.Code, patchClear.Body.String())
		var cleared TriggerResponse
		require.NoError(t, json.Unmarshal(patchClear.Body.Bytes(), &cleared))
		assert.Empty(t, cleared.Action.UntrustedVars)
	})
}

func TestTriggerUntrustedVarsDerive_DispatchAgentAndMultiAction(t *testing.T) {
	f := setupTriggersTestFixture(t)

	// Replay-protected webhook endpoint is required for dispatch_agent.
	epRec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{
		Name:                "derive-dispatch-ep",
		ReplayWindowSeconds: 300,
	})
	require.Equal(t, http.StatusCreated, epRec.Code, epRec.Body.String())
	var ep WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(epRec.Body.Bytes(), &ep))

	t.Run("dispatch_agent derives union across all slots including optional ${path?}", func(t *testing.T) {
		rec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
			Name:   "disp-derive-all-slots",
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
			Action: TriggerActionSpec{
				Type:      store.TriggerActionDispatchAgent,
				Template:  "developer",
				Task:      "Fix ${event.payload.title?} in ${trigger.name}",
				AgentName: "pr-${event.attributes.number}",
				Branch:    "fix/${event.attributes.number}-${event.attributes.branch}",
				Labels: map[string]string{
					"z_sha": "${event.payload.sha}",
					"a_env": "${event.attributes.env}",
				},
				ReuseMessage: "Follow-up ${event.payload.comment} on ${event.payload.title}",
			},
		})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		var created TriggerResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
		assert.Equal(t, []string{
			"event.payload.title",
			"event.attributes.number",
			"event.attributes.branch",
			"event.attributes.env",
			"event.payload.sha",
			"event.payload.comment",
		}, created.Action.UntrustedVars)

		// PATCH adding a new placeholder without untrustedVars re-derives.
		patchRec := f.doTrigger(t, f.ownerAuth, created.ID, http.MethodPatch, map[string]any{
			"action": map[string]any{
				"type":     store.TriggerActionDispatchAgent,
				"template": "developer",
				"task":     "Review ${event.payload.commit}",
			},
		})
		require.Equal(t, http.StatusOK, patchRec.Code, patchRec.Body.String())
		var updated TriggerResponse
		require.NoError(t, json.Unmarshal(patchRec.Body.Bytes(), &updated))
		assert.Equal(t, []string{"event.payload.commit"}, updated.Action.UntrustedVars)
	})

	t.Run("dispatch_agent supplied wildcard accepted and narrower list refused", func(t *testing.T) {
		recOK := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
			Name:   "disp-wildcard-ok",
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
			Action: TriggerActionSpec{
				Type:          store.TriggerActionDispatchAgent,
				Template:      "developer",
				Task:          "Review ${event.payload.commit}",
				UntrustedVars: []string{"event.payload.*"},
			},
		})
		require.Equal(t, http.StatusCreated, recOK.Code, recOK.Body.String())
		var created TriggerResponse
		require.NoError(t, json.Unmarshal(recOK.Body.Bytes(), &created))
		assert.Equal(t, []string{"event.payload.*"}, created.Action.UntrustedVars)

		recFail := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
			Name:   "disp-narrow-fail",
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
			Action: TriggerActionSpec{
				Type:          store.TriggerActionDispatchAgent,
				Template:      "developer",
				Task:          "Review ${event.payload.commit} by ${event.attributes.sender}",
				UntrustedVars: []string{"event.payload.commit"},
			},
		})
		require.Equal(t, http.StatusBadRequest, recFail.Code, recFail.Body.String())
		assert.Equal(t, "untrusted_var_not_allowed", decodeAPIErrorCode(t, recFail))
	})

	t.Run("multi-action actions[] derives per-action independently", func(t *testing.T) {
		actions := []TriggerActionSpec{
			{
				Type:    store.TriggerActionMessage,
				AgentID: f.targetAgent.ID,
				Message: "Notify ${event.attributes.repo}",
			},
			{
				Type:     store.TriggerActionDispatchAgent,
				Template: "developer",
				Task:     "Inspect ${event.payload.commit}",
			},
		}
		rec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
			Name:    "multi-derive",
			Source:  TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
			Actions: &actions,
		})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		var created TriggerResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
		require.Len(t, created.Actions, 2)
		assert.Equal(t, []string{"event.attributes.repo"}, created.Actions[0].UntrustedVars)
		assert.Equal(t, []string{"event.payload.commit"}, created.Actions[1].UntrustedVars)

		// Action 0 omits untrustedVars, action 1 supplies narrower list missing a path -> 400.
		badActions := []TriggerActionSpec{
			{
				Type:    store.TriggerActionMessage,
				AgentID: f.targetAgent.ID,
				Message: "Notify ${event.attributes.repo}",
			},
			{
				Type:          store.TriggerActionDispatchAgent,
				Template:      "developer",
				Task:          "Inspect ${event.payload.commit}",
				UntrustedVars: []string{"event.attributes.repo"},
			},
		}
		recBad := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
			Name:    "multi-narrow-fail",
			Source:  TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
			Actions: &badActions,
		})
		require.Equal(t, http.StatusBadRequest, recBad.Code, recBad.Body.String())
		assert.Equal(t, "untrusted_var_not_allowed", decodeAPIErrorCode(t, recBad))
	})
}

func TestTriggerUntrustedVarsDerive_WebhookDefaultTriggerToSelf(t *testing.T) {
	f := setupTriggersTestFixture(t)

	// Agent creating a webhook endpoint with defaultTrigger.toSelf and an
	// untrusted placeholder in message -> derives and stores untrustedVars.
	rec := f.doWebhookEndpoint(t, f.agentAAuth, "", http.MethodPost, map[string]any{
		"name": "self-ep-derive",
		"defaultTrigger": map[string]any{
			"toSelf":  true,
			"message": "CI at ${event.payload.commit}",
		},
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	require.NotNil(t, created.Trigger)

	getRec := f.doTrigger(t, f.ownerAuth, created.Trigger.ID, http.MethodGet, nil)
	require.Equal(t, http.StatusOK, getRec.Code, getRec.Body.String())
	var tr TriggerResponse
	require.NoError(t, json.Unmarshal(getRec.Body.Bytes(), &tr))
	assert.Equal(t, []string{"event.payload.commit"}, tr.Action.UntrustedVars)
}

func TestTriggerUntrustedVarsDerive_PerPrincipalAuthoringRegression(t *testing.T) {
	f := setupTriggersTestFixture(t)

	epRec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{
		Name:               "principal-derive-ep",
		AllowProjectAgents: true,
	})
	require.Equal(t, http.StatusCreated, epRec.Code, epRec.Body.String())
	var ep WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(epRec.Body.Bytes(), &ep))

	principals := []struct {
		name          string
		auth          Identity
		targetAgentID string
	}{
		{"admin", f.adminAuth, f.targetAgent.ID},
		{"member", f.memberAuth, f.targetAgent.ID},
		{"agent", f.agentAAuth, f.agentA.ID},
	}

	for _, p := range principals {
		t.Run(p.name, func(t *testing.T) {
			// Omitted untrustedVars derives ONLY the exact template paths (no widening).
			rec := f.doTrigger(t, p.auth, "", http.MethodPost, CreateTriggerRequest{
				Name:   "principal-derive-" + p.name,
				Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
				Action: TriggerActionSpec{
					Type:    store.TriggerActionMessage,
					AgentID: p.targetAgentID,
					Message: "Alert ${event.payload.commit}",
				},
			})
			require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
			var created TriggerResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
			assert.Equal(t, []string{"event.payload.commit"}, created.Action.UntrustedVars)

			// Supplied narrower list still refuses missing paths.
			recFail := f.doTrigger(t, p.auth, "", http.MethodPost, CreateTriggerRequest{
				Name:   "principal-narrow-" + p.name,
				Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
				Action: TriggerActionSpec{
					Type:          store.TriggerActionMessage,
					AgentID:       p.targetAgentID,
					Message:       "Alert ${event.payload.commit} ${event.attributes.repo}",
					UntrustedVars: []string{"event.attributes.repo"},
				},
			})
			require.Equal(t, http.StatusBadRequest, recFail.Code, recFail.Body.String())
			assert.Equal(t, "untrusted_var_not_allowed", decodeAPIErrorCode(t, recFail))
		})
	}
}

func TestRequireAgentAuthoringFloor_TypedNilIdentity(t *testing.T) {
	var typedNil *agentIdentityWrapper
	assert.NoError(t, requireAgentAuthoringFloor(nil))
	assert.NoError(t, requireAgentAuthoringFloor(typedNil))
}
