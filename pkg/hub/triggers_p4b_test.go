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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestWebhookEndpoint_ProviderModesAndRoutingHintsAPI(t *testing.T) {
	f := setupTriggersTestFixture(t)

	// 1. Create github_v1 endpoint with routingHints.
	createGH := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, map[string]any{
		"name":          "gh-repo-hooks",
		"signatureMode": store.WebhookSignatureModeGitHubV1,
		"routingHints": map[string]any{
			"subjectPath": "/repository/full_name",
		},
	})
	require.Equal(t, http.StatusCreated, createGH.Code, createGH.Body.String())
	var ghResp WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(createGH.Body.Bytes(), &ghResp))
	assert.Equal(t, store.WebhookSignatureModeGitHubV1, ghResp.SignatureMode)
	assert.NotEmpty(t, ghResp.Secret)
	require.NotNil(t, ghResp.RoutingHints)
	assert.Equal(t, "/repository/full_name", ghResp.RoutingHints.SubjectPath)

	// 2. Create stripe_v1 endpoint with custom routingHints.
	createStripe := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, map[string]any{
		"name":          "stripe-hooks",
		"signatureMode": store.WebhookSignatureModeStripeV1,
		"routingHints": map[string]any{
			"typePath":    "/type",
			"subjectPath": "/data/object/customer",
			"dedupPath":   "/id",
		},
	})
	require.Equal(t, http.StatusCreated, createStripe.Code, createStripe.Body.String())
	var stripeResp WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(createStripe.Body.Bytes(), &stripeResp))
	assert.Equal(t, store.WebhookSignatureModeStripeV1, stripeResp.SignatureMode)
	require.NotNil(t, stripeResp.RoutingHints)
	assert.Equal(t, "/data/object/customer", stripeResp.RoutingHints.SubjectPath)

	// 3. Partial PATCH on routingHints preserves unset paths and clears empty strings.
	patchStripe := f.doWebhookEndpoint(t, f.ownerAuth, stripeResp.ID, http.MethodPatch, map[string]any{
		"routingHints": map[string]any{
			"subjectPath": "/data/object/id",
			"dedupPath":   "",
		},
	})
	require.Equal(t, http.StatusOK, patchStripe.Code, patchStripe.Body.String())
	var patchedStripe WebhookEndpointResponse
	require.NoError(t, json.Unmarshal(patchStripe.Body.Bytes(), &patchedStripe))
	require.NotNil(t, patchedStripe.RoutingHints)
	assert.Equal(t, "/type", patchedStripe.RoutingHints.TypePath, "unset field must be preserved on partial PATCH")
	assert.Equal(t, "/data/object/id", patchedStripe.RoutingHints.SubjectPath)
	assert.Empty(t, patchedStripe.RoutingHints.DedupPath, "empty string must clear the path")

	// 4. Invalid signatureMode and invalid routingHints pointer return 400.
	badMode := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, map[string]any{
		"name":          "bad-mode",
		"signatureMode": "unknown_v9",
	})
	assert.Equal(t, http.StatusBadRequest, badMode.Code)

	badHint := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, map[string]any{
		"name":          "bad-hint",
		"signatureMode": store.WebhookSignatureModeStripeV1,
		"routingHints": map[string]any{
			"typePath": "missing-leading-slash",
		},
	})
	assert.Equal(t, http.StatusBadRequest, badHint.Code)

	badPatchHint := f.doWebhookEndpoint(t, f.ownerAuth, stripeResp.ID, http.MethodPatch, map[string]any{
		"routingHints": map[string]any{
			"subjectPath": "/bad~2escape",
		},
	})
	assert.Equal(t, http.StatusBadRequest, badPatchHint.Code)
}

func TestTriggers_GitHubV1ReplayProtectionRules(t *testing.T) {
	f := setupTriggersTestFixture(t)
	enableTriggerExecExperiments(t, f.srv)
	ctx := context.Background()

	// Seed a target agent for message & exec triggers.
	targetAg := &store.Agent{
		ID:        api.NewUUID(),
		Slug:      "p4b-target-ag",
		Name:      "p4b-target-ag",
		ProjectID: f.projectID,
		Phase:     "running",
		OwnerID:   f.owner.ID,
	}
	require.NoError(t, f.store.CreateAgent(ctx, targetAg))

	// Create github_v1, stripe_v1, and scion_v1 endpoints.
	createEP := func(name, sigMode string) WebhookEndpointCreatedResponse {
		rec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, map[string]any{
			"name":          name,
			"signatureMode": sigMode,
		})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		var resp WebhookEndpointCreatedResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		return resp
	}
	ghEP := createEP("p4b-gh-ep", store.WebhookSignatureModeGitHubV1)
	stripeEP := createEP("p4b-stripe-ep", store.WebhookSignatureModeStripeV1)
	scionEP := createEP("p4b-scion-ep", store.WebhookSignatureModeScionV1)

	// 1. dispatch_agent on github_v1 -> 400 source_not_replay_protected.
	refusedDispatch := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "dispatch-on-github-v1",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ghEP.ID},
		Action: TriggerActionSpec{
			Type:     store.TriggerActionDispatchAgent,
			Template: "default",
			Task:     "Handle ${event.id}",
		},
	})
	assert.Equal(t, http.StatusBadRequest, refusedDispatch.Code, refusedDispatch.Body.String())
	assert.Contains(t, refusedDispatch.Body.String(), "source_not_replay_protected")

	// 2. exec on github_v1 -> 400 source_not_replay_protected.
	refusedExec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "exec-on-github-v1",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ghEP.ID},
		Action: TriggerActionSpec{
			Type:    store.TriggerActionExec,
			AgentID: targetAg.ID,
			Command: []string{"echo", "hi"},
		},
	})
	assert.Equal(t, http.StatusBadRequest, refusedExec.Code, refusedExec.Body.String())
	assert.Contains(t, refusedExec.Body.String(), "source_not_replay_protected")

	// 3. message on github_v1 -> 201 Created (github_v1 serves message rules only).
	okMsg := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "msg-on-github-v1",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ghEP.ID},
		Action: TriggerActionSpec{
			Type:    store.TriggerActionMessage,
			AgentID: targetAg.ID,
			Message: "GitHub event received",
		},
	})
	require.Equal(t, http.StatusCreated, okMsg.Code, okMsg.Body.String())
	var msgTr TriggerResponse
	require.NoError(t, json.Unmarshal(okMsg.Body.Bytes(), &msgTr))

	// 3b. PATCH updating the message trigger on github_v1 to dispatch_agent -> 400 source_not_replay_protected.
	patchToDispatch := f.doTrigger(t, f.ownerAuth, msgTr.ID, http.MethodPatch, UpdateTriggerRequest{
		Action: &TriggerActionSpec{
			Type:     store.TriggerActionDispatchAgent,
			Template: "default",
			Task:     "Handle ${event.id}",
		},
	})
	assert.Equal(t, http.StatusBadRequest, patchToDispatch.Code, patchToDispatch.Body.String())
	assert.Contains(t, patchToDispatch.Body.String(), "source_not_replay_protected")

	// 4. dispatch_agent on stripe_v1 -> 201 Created (replay-protected).
	okStripeDispatch := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "dispatch-on-stripe-v1",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: stripeEP.ID},
		Action: TriggerActionSpec{
			Type:     store.TriggerActionDispatchAgent,
			Template: "default",
			Task:     "Handle Stripe ${event.id}",
		},
	})
	assert.Equal(t, http.StatusCreated, okStripeDispatch.Code, okStripeDispatch.Body.String())

	// 5. dispatch_agent on scion_v1, then PATCH scion_v1 -> github_v1 -> 409 replay_protected_triggers_exist.
	okScionDispatch := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "dispatch-on-scion-v1",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: scionEP.ID},
		Action: TriggerActionSpec{
			Type:     store.TriggerActionDispatchAgent,
			Template: "default",
			Task:     "Handle ${event.id}",
		},
	})
	require.Equal(t, http.StatusCreated, okScionDispatch.Code, okScionDispatch.Body.String())
	var scionTr TriggerResponse
	require.NoError(t, json.Unmarshal(okScionDispatch.Body.Bytes(), &scionTr))

	flipConflict := f.doWebhookEndpoint(t, f.ownerAuth, scionEP.ID, http.MethodPatch, map[string]any{
		"signatureMode": store.WebhookSignatureModeGitHubV1,
	})
	assert.Equal(t, http.StatusConflict, flipConflict.Code, flipConflict.Body.String())
	assert.Contains(t, flipConflict.Body.String(), "replay_protected_triggers_exist")

	// 6. Pause the trigger -> PATCH to github_v1 succeeds -> resume refused 400 source_not_replay_protected.
	pauseRec := f.doTrigger(t, f.ownerAuth, scionTr.ID+"/pause", http.MethodPost, nil)
	require.Equal(t, http.StatusOK, pauseRec.Code, pauseRec.Body.String())

	flipOK := f.doWebhookEndpoint(t, f.ownerAuth, scionEP.ID, http.MethodPatch, map[string]any{
		"signatureMode": store.WebhookSignatureModeGitHubV1,
	})
	require.Equal(t, http.StatusOK, flipOK.Code, flipOK.Body.String())

	resumeRefused := f.doTrigger(t, f.ownerAuth, scionTr.ID+"/resume", http.MethodPost, nil)
	assert.Equal(t, http.StatusBadRequest, resumeRefused.Code, resumeRefused.Body.String())
	assert.Contains(t, resumeRefused.Body.String(), "source_not_replay_protected")

	// 7. Fire-time replay safety refuses a github_v1 event even if called directly.
	_, refusal, err := f.srv.checkTriggerFireReplaySafety(ctx, &hubfire.FireEvent{
		ID:         api.NewUUID(),
		ProjectID:  f.projectID,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   ghEP.ID,
		Attributes: map[string]string{
			"signature_mode": store.WebhookSignatureModeGitHubV1,
			"delivery":       "gh-deliv-99",
		},
	})
	require.NoError(t, err)
	require.NotNil(t, refusal)
	assert.Equal(t, trigger.FireReplayReasonSourceNotProtected, refusal.Reason)
}

func TestTriggerEventBody_ProviderVerifiedLabelsAndAttributes(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		mode     string
		wantVer  string
		delivery string
		event    string
		subject  string
	}{
		{
			mode:     store.WebhookSignatureModeGitHubV1,
			wantVer:  messaging.ExternalVerifiedGitHubV1,
			delivery: "gh-deliv-1",
			event:    "pull_request",
			subject:  "acme/repo",
		},
		{
			mode:     store.WebhookSignatureModeStripeV1,
			wantVer:  messaging.ExternalVerifiedStripeV1,
			delivery: "evt_123",
			event:    "invoice.payment_succeeded",
			subject:  "in_456",
		},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			ev := &hubfire.FireEvent{
				ID:         "ev-1",
				ProjectID:  "proj-1",
				SourceKind: store.TriggerSourceKindWebhook,
				SourceID:   "ep-1",
				Type:       "webhook." + tc.event,
				Subject:    "webhook:ep-1/" + tc.subject,
				OccurredAt: now,
				ReceivedAt: now,
				Attributes: map[string]string{
					"signature_mode": tc.mode,
					"delivery":       tc.delivery,
					"event":          tc.event,
					"subject":        tc.subject,
				},
				Payload: []byte(`{"ok":true}`),
			}
			body := buildDispatchedEventBody(ev)
			require.NotNil(t, body.External)
			assert.Equal(t, tc.wantVer, body.External.Verified)
			assert.Equal(t, tc.delivery, body.Attributes["delivery"])
			assert.Equal(t, tc.event, body.Attributes["event"])
			assert.Equal(t, tc.subject, body.Attributes["subject"])
		})
	}
}
