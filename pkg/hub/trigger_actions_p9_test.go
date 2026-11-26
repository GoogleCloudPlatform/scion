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
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agentcontrol"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/durablequeue"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestTriggerActions_APIContractAndScheduleRefusal(t *testing.T) {
	f := setupTriggersTestFixture(t)
	ctx := context.Background()

	epRec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{
		Name:          "p9-api-ep",
		SignatureMode: store.WebhookSignatureModeScionV1,
	})
	require.Equal(t, http.StatusCreated, epRec.Code, epRec.Body.String())
	var ep WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(epRec.Body.Bytes(), &ep))

	// 1. Create with actions[] (2 elements): response has actions (len 2) and omits "action".
	twoActions := []TriggerActionSpec{
		{Type: store.TriggerActionMessage, AgentID: f.targetAgent.ID, Message: "first"},
		{Type: store.TriggerActionMessage, AgentID: f.targetAgent.ID, Message: "second"},
	}
	rec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:    "p9-two-actions",
		Source:  TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Actions: &twoActions,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var rawResp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rawResp))
	_, hasScalarAction := rawResp["action"]
	assert.False(t, hasScalarAction, "multi-action response must omit scalar action field")

	var trigResp TriggerResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &trigResp))
	require.Len(t, trigResp.Actions, 2)
	assert.Equal(t, "first", trigResp.Actions[0].Message)
	assert.Equal(t, "second", trigResp.Actions[1].Message)

	// Stored row must leave legacy scalar columns empty.
	stored, err := f.store.GetTrigger(ctx, trigResp.ID)
	require.NoError(t, err)
	assert.Empty(t, stored.ActionType)
	assert.Empty(t, stored.ActionPayload)
	require.Len(t, stored.Actions, 2)

	// 2. Create with scalar action sugar: response carries BOTH actions (len 1) and mirrored action.
	scalarRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "p9-scalar-sugar",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Action: TriggerActionSpec{Type: store.TriggerActionMessage, AgentID: f.targetAgent.ID, Message: "solo"},
	})
	require.Equal(t, http.StatusCreated, scalarRec.Code, scalarRec.Body.String())
	var scalarRaw map[string]any
	require.NoError(t, json.Unmarshal(scalarRec.Body.Bytes(), &scalarRaw))
	assert.Contains(t, scalarRaw, "action", "single-action response must mirror scalar action")
	assert.Contains(t, scalarRaw, "actions", "single-action response must include actions list")

	var scalarResp TriggerResponse
	require.NoError(t, json.Unmarshal(scalarRec.Body.Bytes(), &scalarResp))
	require.Len(t, scalarResp.Actions, 1)
	assert.Equal(t, "solo", scalarResp.Action.Message)
	assert.Equal(t, "solo", scalarResp.Actions[0].Message)

	// 3. Mixing action + actions -> 400 invalid_action {category: ambiguous_actions}.
	ambigRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:    "p9-ambiguous",
		Source:  TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Action:  TriggerActionSpec{Type: store.TriggerActionMessage, AgentID: f.targetAgent.ID, Message: "a"},
		Actions: &twoActions,
	})
	require.Equal(t, http.StatusBadRequest, ambigRec.Code, ambigRec.Body.String())
	assert.Contains(t, ambigRec.Body.String(), ErrCodeInvalidAction)
	assert.Contains(t, ambigRec.Body.String(), "ambiguous_actions")

	// 4. Empty actions[] and > 8 actions[] -> 400 invalid_action.
	emptyActions := []TriggerActionSpec{}
	emptyRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:    "p9-empty",
		Source:  TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Actions: &emptyActions,
	})
	require.Equal(t, http.StatusBadRequest, emptyRec.Code, emptyRec.Body.String())
	assert.Contains(t, emptyRec.Body.String(), ErrCodeInvalidAction)

	nineActions := make([]TriggerActionSpec, 9)
	for i := range nineActions {
		nineActions[i] = TriggerActionSpec{Type: store.TriggerActionMessage, AgentID: f.targetAgent.ID, Message: "m"}
	}
	tooManyRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:    "p9-too-many",
		Source:  TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Actions: &nineActions,
	})
	require.Equal(t, http.StatusBadRequest, tooManyRec.Code, tooManyRec.Body.String())
	assert.Contains(t, tooManyRec.Body.String(), ErrCodeInvalidAction)

	// 5. PATCH updating actions bumps authorizationRevision; PATCH mixing action+actions -> 400 ambiguous_actions.
	reordered := []TriggerActionSpec{twoActions[1], twoActions[0]}
	patchRec := f.doTrigger(t, f.ownerAuth, trigResp.ID, http.MethodPatch, UpdateTriggerRequest{
		Actions: &reordered,
	})
	require.Equal(t, http.StatusOK, patchRec.Code, patchRec.Body.String())
	var patched TriggerResponse
	require.NoError(t, json.Unmarshal(patchRec.Body.Bytes(), &patched))
	assert.Equal(t, trigResp.AuthorizationRevision+1, patched.AuthorizationRevision)
	require.Len(t, patched.Actions, 2)
	assert.Equal(t, "second", patched.Actions[0].Message)
	assert.Equal(t, "first", patched.Actions[1].Message)

	patchAmbigRec := f.doTrigger(t, f.ownerAuth, trigResp.ID, http.MethodPatch, UpdateTriggerRequest{
		Action:  &twoActions[0],
		Actions: &twoActions,
	})
	require.Equal(t, http.StatusBadRequest, patchAmbigRec.Code, patchAmbigRec.Body.String())
	assert.Contains(t, patchAmbigRec.Body.String(), "ambiguous_actions")

	// 6. Schedules refuse actions on POST /schedules.
	schedBody := map[string]any{
		"name":      "sched-refuse-actions",
		"cronExpr":  "0 * * * *",
		"eventType": "message",
		"agentName": f.targetAgent.Name,
		"message":   "hi",
		"actions":   []map[string]any{{"type": "message", "agentName": f.targetAgent.Name, "message": "hi"}},
	}
	req := authoredRequest(t, f.ownerAuth, http.MethodPost, "/api/v1/projects/"+f.projectID+"/schedules", schedBody)
	schedRec := httptest.NewRecorder()
	f.srv.handleSchedules(schedRec, req, f.projectID, "")
	require.Equal(t, http.StatusBadRequest, schedRec.Code, schedRec.Body.String())
	assert.Contains(t, schedRec.Body.String(), "not supported for schedules")
}

func TestTriggerActions_PerActionAuthorityAndAgentMode(t *testing.T) {
	f := setupTriggersTestFixture(t)
	enableTriggerExecExperiments(t, f.srv)

	// Create one scion_v1 endpoint owned by agentA (so the agent can
	// create triggers on it) and one body_only endpoint owned by the user.
	agentEpRec := f.doWebhookEndpoint(t, f.agentAAuth, "", http.MethodPost, CreateWebhookEndpointRequest{
		Name:          "p9-agent-ep",
		SignatureMode: store.WebhookSignatureModeScionV1,
	})
	require.Equal(t, http.StatusCreated, agentEpRec.Code, agentEpRec.Body.String())
	var agentEp WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(agentEpRec.Body.Bytes(), &agentEp))

	// Baseline agent authoring [message(to-self), message(to-self)] -> allowed (201).
	okAgentActions := []TriggerActionSpec{
		{Type: store.TriggerActionMessage, AgentID: f.agentA.ID, Message: "self 1"},
		{Type: store.TriggerActionMessage, AgentID: f.agentA.ID, Message: "self 2"},
	}
	rec := f.doTrigger(t, f.agentAAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:    "p9-agent-ok",
		Source:  TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: agentEp.ID},
		Actions: &okAgentActions,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	// Baseline agent authoring [message(to-self), dispatch_agent] -> refused (403) because action 1 exceeds baseline rights.
	badAgentActions := []TriggerActionSpec{
		{Type: store.TriggerActionMessage, AgentID: f.agentA.ID, Message: "self ok"},
		{Type: store.TriggerActionDispatchAgent, Template: "default", Task: "should be refused"},
	}
	rec = f.doTrigger(t, f.agentAAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:    "p9-agent-refused-dispatch",
		Source:  TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: agentEp.ID},
		Actions: &badAgentActions,
	})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	// Baseline agent authoring [message(to-self), exec] -> refused (403).
	badAgentExec := []TriggerActionSpec{
		{Type: store.TriggerActionMessage, AgentID: f.agentA.ID, Message: "self ok"},
		{Type: store.TriggerActionExec, AgentID: f.agentA.ID, Command: []string{"echo", "hi"}},
	}
	rec = f.doTrigger(t, f.agentAAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:    "p9-agent-refused-exec",
		Source:  TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: agentEp.ID},
		Actions: &badAgentExec,
	})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	// Replay-protected-source rule applies if ANY action creates agents or runs commands.
	bodyOnlyEp := &store.WebhookEndpoint{
		ID:                  api.NewUUID(),
		ProjectID:           f.projectID,
		Name:                "p9-body-only",
		OwnerPrincipalKind:  store.DelegationPrincipalUser,
		OwnerPrincipalID:    f.owner.ID,
		OwnerCredentialKind: store.InitiatorCredentialKindSession,
		SignatureMode:       store.WebhookSignatureModeBodyOnly,
		SecretVersion:       1,
		Status:              store.WebhookEndpointStatusActive,
	}
	require.NoError(t, f.store.CreateWebhookEndpoint(context.Background(), bodyOnlyEp))

	mixedDispatch := []TriggerActionSpec{
		{Type: store.TriggerActionMessage, AgentID: f.targetAgent.ID, Message: "ok"},
		{Type: store.TriggerActionDispatchAgent, Template: "default", Task: "needs replay protection"},
	}
	rec = f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:    "p9-body-only-dispatch",
		Source:  TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: bodyOnlyEp.ID},
		Actions: &mixedDispatch,
	})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "source_not_replay_protected")

	mixedExec := []TriggerActionSpec{
		{Type: store.TriggerActionMessage, AgentID: f.targetAgent.ID, Message: "ok"},
		{Type: store.TriggerActionExec, AgentID: f.targetAgent.ID, Command: []string{"true"}},
	}
	rec = f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:    "p9-body-only-exec",
		Source:  TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: bodyOnlyEp.ID},
		Actions: &mixedExec,
	})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "source_not_replay_protected")

	// Pure message list on body_only is allowed, and maxConcurrentDispatches is refused on non-dispatch lists.
	rec = f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:                    "p9-msg-max-conc",
		Source:                  TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: bodyOnlyEp.ID},
		Actions:                 &okAgentActions,
		MaxConcurrentDispatches: 3,
	})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "maxConcurrentDispatches")
}

func TestTriggerActions_PerElementExecRedaction(t *testing.T) {
	f := setupTriggersTestFixture(t)
	enableTriggerExecExperiments(t, f.srv)

	epRec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{
		Name:          "p9-redaction-ep",
		SignatureMode: store.WebhookSignatureModeScionV1,
	})
	require.Equal(t, http.StatusCreated, epRec.Code, epRec.Body.String())
	var ep WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(epRec.Body.Bytes(), &ep))

	cmd0 := []string{"sh", "-c", "echo secret-token-1"}
	cmd1 := []string{"sh", "-c", "echo secret-token-2"}
	actions := []TriggerActionSpec{
		{Type: store.TriggerActionMessage, AgentID: f.targetAgent.ID, Message: "notify"},
		{Type: store.TriggerActionExec, AgentID: f.targetAgent.ID, Command: cmd0},
		{Type: store.TriggerActionExec, AgentID: f.targetAgent.ID, Command: cmd1},
	}
	createRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:    "p9-redaction-multi",
		Source:  TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Actions: &actions,
	})
	require.Equal(t, http.StatusCreated, createRec.Code, createRec.Body.String())
	var created TriggerResponse
	require.NoError(t, json.Unmarshal(createRec.Body.Bytes(), &created))
	require.Len(t, created.Actions, 3)

	// Owner has agent.attach -> sees full argv on both exec elements.
	assert.Equal(t, cmd0, created.Actions[1].Command)
	assert.False(t, created.Actions[1].CommandRedacted)
	assert.Equal(t, cmd1, created.Actions[2].Command)
	assert.False(t, created.Actions[2].CommandRedacted)

	// Member (triggers.read without agent.attach) sees fail-closed redaction on EVERY exec element, while message is intact.
	getRec := f.doTrigger(t, f.memberAuth, created.ID, http.MethodGet, nil)
	require.Equal(t, http.StatusOK, getRec.Code, getRec.Body.String())
	assert.NotContains(t, getRec.Body.String(), "secret-token-1")
	assert.NotContains(t, getRec.Body.String(), "secret-token-2")

	var viewerResp TriggerResponse
	require.NoError(t, json.Unmarshal(getRec.Body.Bytes(), &viewerResp))
	require.Len(t, viewerResp.Actions, 3)
	assert.Equal(t, "notify", viewerResp.Actions[0].Message)

	assert.True(t, viewerResp.Actions[1].CommandRedacted)
	assert.Empty(t, viewerResp.Actions[1].Command)
	assert.Equal(t, len(cmd0), viewerResp.Actions[1].Argc)
	assert.Equal(t, agentcontrol.ExecArgvDigest(cmd0), viewerResp.Actions[1].CommandSHA256)

	assert.True(t, viewerResp.Actions[2].CommandRedacted)
	assert.Empty(t, viewerResp.Actions[2].Command)
	assert.Equal(t, len(cmd1), viewerResp.Actions[2].Argc)
	assert.Equal(t, agentcontrol.ExecArgvDigest(cmd1), viewerResp.Actions[2].CommandSHA256)
}

func TestTriggerActions_RateLimitCountsMatchedEventsNotActions(t *testing.T) {
	f := setupTriggersTestFixture(t)
	ctx := context.Background()

	epRec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{
		Name:          "p9-ratelimit-ep",
		SignatureMode: store.WebhookSignatureModeScionV1,
	})
	require.Equal(t, http.StatusCreated, epRec.Code, epRec.Body.String())
	var ep WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(epRec.Body.Bytes(), &ep))

	// Two-action trigger with maxFiresPerMinute = 1.
	actions := []TriggerActionSpec{
		{Type: store.TriggerActionMessage, AgentID: f.targetAgent.ID, Message: "msg-0"},
		{Type: store.TriggerActionMessage, AgentID: f.targetAgent.ID, Message: "msg-1"},
	}
	createRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:              "p9-rate-limit-events",
		Source:            TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Actions:           &actions,
		MaxFiresPerMinute: 1,
	})
	require.Equal(t, http.StatusCreated, createRec.Code, createRec.Body.String())
	var created TriggerResponse
	require.NoError(t, json.Unmarshal(createRec.Body.Bytes(), &created))

	disp, err := f.srv.newTriggerDispatcher(nil)
	require.NoError(t, err)

	dispatchEvent := func(dedup string) string {
		now := time.Now().UTC()
		evID := api.NewUUID()
		require.NoError(t, f.store.CreateTriggerEvent(ctx, &store.TriggerEvent{
			ID:         evID,
			ProjectID:  f.projectID,
			SourceKind: store.TriggerSourceKindWebhook,
			SourceID:   ep.ID,
			Type:       "webhook.received",
			Subject:    "sub-" + dedup,
			DedupKey:   dedup,
			OccurredAt: now,
			ReceivedAt: now,
			Status:     store.TriggerEventStatusPending,
		}))
		qp, mErr := json.Marshal(trigger.EventQueuePayload{
			Version: trigger.EventQueuePayloadVersion,
			EventID: evID,
		})
		require.NoError(t, mErr)
		res := disp.HandleItem(ctx, durablequeue.Item{
			Seq:       1,
			Kind:      trigger.EventQueueKind,
			DedupKey:  evID,
			ProjectID: f.projectID,
			Payload:   qp,
			Attempts:  1,
		}, nil)
		require.True(t, res.Done)
		require.NoError(t, res.Err)
		return evID
	}

	ev1 := dispatchEvent("ev-1")
	ev2 := dispatchEvent("ev-2")

	// History endpoint must carry actionIndex and actionType.
	histRec := f.doTrigger(t, f.ownerAuth, created.ID+"/history", http.MethodGet, nil)
	require.Equal(t, http.StatusOK, histRec.Code, histRec.Body.String())
	var hist ListTriggerFiresResponse
	require.NoError(t, json.Unmarshal(histRec.Body.Bytes(), &hist))
	require.Len(t, hist.Fires, 4, "2 events x 2 actions = 4 fire rows")

	ev1ByIdx := map[int]TriggerFireResponse{}
	ev2ByIdx := map[int]TriggerFireResponse{}
	for _, fire := range hist.Fires {
		assert.Equal(t, store.TriggerActionMessage, fire.ActionType)
		switch fire.EventID {
		case ev1:
			ev1ByIdx[fire.ActionIndex] = fire
		case ev2:
			ev2ByIdx[fire.ActionIndex] = fire
		}
	}
	require.Len(t, ev1ByIdx, 2)
	// Both actions of event 1 must be admitted (neither skipped by rate limit),
	// even though maxFiresPerMinute=1 (rate limit counts matched events, not actions).
	assert.NotEqual(t, store.TriggerFireStatusSkipped, ev1ByIdx[0].Status)
	assert.NotEqual(t, trigger.ErrFireRateLimit, ev1ByIdx[0].Error)
	assert.NotEqual(t, store.TriggerFireStatusSkipped, ev1ByIdx[1].Status)
	assert.NotEqual(t, trigger.ErrFireRateLimit, ev1ByIdx[1].Error)

	require.Len(t, ev2ByIdx, 2)
	assert.Equal(t, store.TriggerFireStatusSkipped, ev2ByIdx[0].Status)
	assert.Equal(t, trigger.ErrFireRateLimit, ev2ByIdx[0].Error)
	assert.Equal(t, store.TriggerFireStatusSkipped, ev2ByIdx[1].Status)
	assert.Equal(t, trigger.ErrFireRateLimit, ev2ByIdx[1].Error)
}

// TestTriggerActions_SharedRateDecisionAcrossWindowSlide verifies R2:
// when a matched event's RateDecision is decided on action 0, action 1 reuses
// that exact decision even if the 60s window boundary or concurrent events
// change the DB count between action 0 and action 1.
func TestTriggerActions_SharedRateDecisionAcrossWindowSlide(t *testing.T) {
	f := setupTriggersTestFixture(t)
	ctx := context.Background()
	trigID := api.NewUUID()
	now := time.Now().UTC()

	// Seed one fired event in the window so maxFiresPerMinute=1 refuses action 0.
	priorEv := api.NewUUID()
	priorFire := &store.TriggerFire{
		ID:          api.NewUUID(),
		ProjectID:   f.projectID,
		TriggerID:   trigID,
		EventID:     priorEv,
		Subject:     "sub-prior",
		ActionType:  store.TriggerActionMessage,
		ActionIndex: 0,
		Status:      store.TriggerFireStatusFired,
		LeaseUntil:  now.Add(10 * time.Minute),
		StartedAt:   now,
		CreatedAt:   now,
	}
	claimed, _, err := f.store.ClaimTriggerFire(ctx, priorFire)
	require.NoError(t, err)
	require.True(t, claimed)

	decision := &hubfire.EventRateDecision{}
	evID := api.NewUUID()
	mkReq := func(idx int) hubfire.FireRequest {
		return hubfire.FireRequest{
			ProjectID:  f.projectID,
			ActionType: store.TriggerActionMessage,
			Authority: hubfire.RecordedAuthority{
				DefinitionKind: hubfire.DefinitionKindTrigger,
				DefinitionID:   trigID,
				FireID:         api.NewUUID(),
			},
			Event: &hubfire.FireEvent{
				ID:        evID,
				ProjectID: f.projectID,
				Trigger: &hubfire.FireTriggerRef{
					ID:                trigID,
					ActionIndex:       idx,
					MaxFiresPerMinute: 1,
					RateDecision:      decision,
				},
			},
		}
	}

	// Action 0 sees priorFire -> rate-limited, caches Skipped=true on decision.
	reason0, err := f.srv.checkTriggerFireLimits(ctx, mkReq(0))
	require.NoError(t, err)
	assert.Equal(t, trigger.ErrFireRateLimit, reason0)
	require.True(t, decision.Decided)
	require.True(t, decision.Skipped)

	// Simulate window slide: mark priorFire as skipped (so DB count drops to 0).
	// Action 1 MUST still get the same cached decision (rate-limited), never splitting the event.
	_, err = f.store.FinishTriggerFire(ctx, priorFire.ID, store.TriggerFireStatusSkipped, "", "", "expired", now)
	require.NoError(t, err)

	reason1, err := f.srv.checkTriggerFireLimits(ctx, mkReq(1))
	require.NoError(t, err)
	assert.Equal(t, trigger.ErrFireRateLimit, reason1, "sibling action 1 must share action 0's rate decision across window slide")
}

// TestTriggerActions_ExecCapsCountCorruptActiveRowConservatively verifies O3:
// a corrupt active trigger whose raw actions JSON or scalar action_type
// indicates exec still counts conservatively as 1 exec toward the project cap.
func TestTriggerActions_ExecCapsCountCorruptActiveRowConservatively(t *testing.T) {
	f := setupTriggersTestFixture(t)
	enableTriggerExecExperiments(t, f.srv)
	ctx := context.Background()

	epRec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{
		Name:          "p9-o3-cap-ep",
		SignatureMode: store.WebhookSignatureModeScionV1,
	})
	require.Equal(t, http.StatusCreated, epRec.Code, epRec.Body.String())
	var ep WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(epRec.Body.Bytes(), &ep))

	// Seed (triggerExecMaxActivePerProject - 1) valid exec triggers + 1 corrupt active exec trigger.
	now := time.Now().UTC()
	for i := 0; i < triggerExecMaxActivePerProject-1; i++ {
		require.NoError(t, f.store.CreateTrigger(ctx, &store.Trigger{
			ID:         api.NewUUID(),
			ProjectID:  f.projectID,
			Name:       "valid-exec-" + api.NewUUID()[:8],
			SourceKind: store.TriggerSourceKindWebhook,
			SourceID:   ep.ID,
			MatchTypes: []string{"webhook.received"},
			Actions: []store.TriggerAction{
				{Type: store.TriggerActionExec, Payload: `{"agentId":"` + f.targetAgent.ID + `","command":["true"]}`},
			},
			Status:    store.TriggerStatusActive,
			CreatedAt: now,
			UpdatedAt: now,
		}))
	}
	// Seed 1 corrupt active trigger with legacy scalar action_type="exec" and truncated payload.
	require.NoError(t, f.store.CreateTrigger(ctx, &store.Trigger{
		ID:            api.NewUUID(),
		ProjectID:     f.projectID,
		Name:          "corrupt-exec-row",
		SourceKind:    store.TriggerSourceKindWebhook,
		SourceID:      ep.ID,
		MatchTypes:    []string{"webhook.received"},
		ActionType:    store.TriggerActionExec,
		ActionPayload: `{"command":["truncated"`,
		Status:        store.TriggerStatusActive,
		CreatedAt:     now,
		UpdatedAt:     now,
	}))

	// Attempting to add one more exec action must be refused by the project exec cap.
	err := f.store.WithTx(ctx, func(tx store.Store) error {
		return checkTriggerExecCapsInTx(ctx, tx, f.projectID, []store.TriggerAction{
			{Type: store.TriggerActionExec, Payload: `{"agentId":"` + api.NewUUID() + `","command":["true"]}`},
		}, "")
	})
	require.ErrorIs(t, err, errTriggerExecProjectCapExceeded, "corrupt active exec trigger must count conservatively toward project exec cap")
}

// TestTriggerActions_TwoExecActionsSameAndDifferentTargets verifies R1 end-to-end
// through execAction: two exec actions on one event targeting the SAME running
// agent get distinct per-action overlap keys and sciontool ScheduleID UUIDs so
// both are admitted while action 0 is still running, while a second event
// firing action 0 is still refused "overlap".
func TestTriggerActions_TwoExecActionsSameAndDifferentTargets(t *testing.T) {
	f := newExecRouteFixture(t)
	enableTriggerExecExperiments(t, f.srv)
	f.srv.SetHubID("test-hub-p9-r1")
	f.srv.SetSecretBackend(secret.NewLocalBackendWithCipher(f.store, f.srv.hubID, f.srv.cipher))
	f.srv.config.HubEndpoint = "https://hub-public.example.com"
	f.startAgent(t)

	ctx := context.Background()
	now := time.Now().UTC()
	projectID := f.launched.ProjectID

	// Create webhook endpoint and 2-exec-action trigger through real HTTP authoring
	// so attribution and ceiling are recorded realistically.
	epReq := authoredRequest(t, f.owner, http.MethodPost, "/api/v1/projects/"+projectID+"/webhook-endpoints", CreateWebhookEndpointRequest{
		Name:          "p9-r1-ep",
		SignatureMode: store.WebhookSignatureModeScionV1,
	})
	epRec := httptest.NewRecorder()
	f.srv.handleWebhookEndpoints(epRec, epReq, projectID, "")
	require.Equal(t, http.StatusCreated, epRec.Code, epRec.Body.String())
	var ep WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(epRec.Body.Bytes(), &ep))

	twoExecs := []TriggerActionSpec{
		{Type: store.TriggerActionExec, AgentID: f.launched.ID, Command: []string{"/bin/sleep", "2"}, TimeoutSeconds: 30},
		{Type: store.TriggerActionExec, AgentID: f.launched.ID, Command: []string{"/bin/echo", "sibling-ok"}, TimeoutSeconds: 30},
	}
	trReq := authoredRequest(t, f.owner, http.MethodPost, "/api/v1/projects/"+projectID+"/triggers", CreateTriggerRequest{
		Name:    "p9-r1-two-exec-same-agent",
		Source:  TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Match:   &TriggerMatchSpec{Types: []string{"webhook.received"}},
		Actions: &twoExecs,
	})
	trRec := httptest.NewRecorder()
	f.srv.handleTriggers(trRec, trReq, projectID, "")
	require.Equal(t, http.StatusCreated, trRec.Code, trRec.Body.String())
	var trResp TriggerResponse
	require.NoError(t, json.Unmarshal(trRec.Body.Bytes(), &trResp))

	tr, err := f.store.GetTrigger(ctx, trResp.ID)
	require.NoError(t, err)
	require.Len(t, tr.Actions, 2)

	ev := &store.TriggerEvent{
		ID:         api.NewUUID(),
		ProjectID:  projectID,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   ep.ID,
		Type:       "webhook.received",
		Subject:    "r1-sub",
		DedupKey:   api.NewUUID(),
		OccurredAt: now,
		ReceivedAt: now,
		Status:     store.TriggerEventStatusPending,
	}
	require.NoError(t, f.store.CreateTriggerEvent(ctx, ev))

	fireActionAt := func(evID string, idx int) (string, hubfire.FireOutcome) {
		tf := &store.TriggerFire{
			ID:          api.NewUUID(),
			ProjectID:   projectID,
			TriggerID:   tr.ID,
			EventID:     evID,
			Subject:     "r1-sub",
			ActionType:  store.TriggerActionExec,
			ActionIndex: idx,
			Status:      store.TriggerFireStatusFiring,
			LeaseUntil:  now.Add(10 * time.Minute),
			StartedAt:   now,
			CreatedAt:   now,
		}
		ok, _, cErr := f.store.ClaimTriggerFire(ctx, tf)
		require.NoError(t, cErr)
		require.True(t, ok)

		out := f.srv.fireAction(ctx, hubfire.FireRequest{
			ProjectID:  projectID,
			ActionType: store.TriggerActionExec,
			Payload:    tr.Actions[idx].Payload,
			Authority: hubfire.RecordedAuthority{
				Attribution:    tr.InitiatorAttribution,
				Ceiling:        tr.AuthorityCeiling,
				DefinitionKind: hubfire.DefinitionKindTrigger,
				DefinitionID:   tr.ID,
				FireID:         tf.ID,
			},
			Event: &hubfire.FireEvent{
				ID:         evID,
				ProjectID:  projectID,
				SourceKind: store.TriggerSourceKindWebhook,
				SourceID:   ep.ID,
				Type:       "webhook.received",
				Subject:    "r1-sub",
				OccurredAt: now,
				ReceivedAt: now,
				Attributes: map[string]string{
					"delivery":       api.NewUUID(),
					"signature_mode": store.WebhookSignatureModeScionV1,
					"endpoint_name":  ep.Name,
				},
				Trigger: &hubfire.FireTriggerRef{
					ID:          tr.ID,
					Name:        tr.Name,
					ActionIndex: idx,
				},
			},
			FireAt:   now,
			Attempts: 1,
		})
		return tf.ID, out
	}

	fire0, out0 := fireActionAt(ev.ID, 0)
	require.Equal(t, hubfire.FireStatusFired, out0.Status, "exec action 0 must be admitted: err=%q", out0.Error)
	require.True(t, out0.Detached)

	// Action 1 targets the SAME agent while action 0 is still sleeping: must ALSO be admitted!
	fire1, out1 := fireActionAt(ev.ID, 1)
	require.Equal(t, hubfire.FireStatusFired, out1.Status, "sibling exec action 1 must be admitted while action 0 runs: err=%q", out1.Error)
	require.True(t, out1.Detached)

	run0 := f.getRun(t, fire0)
	assert.Equal(t, "trigger:"+tr.ID+":0", run0.OverlapKey)

	run1 := f.getRun(t, fire1)
	assert.Equal(t, "trigger:"+tr.ID+":1", run1.OverlapKey)

	// A second event firing action 0 while action 0 is still running MUST still be skipped "overlap".
	ev2 := &store.TriggerEvent{
		ID:         api.NewUUID(),
		ProjectID:  projectID,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   ep.ID,
		Type:       "webhook.received",
		Subject:    "r1-sub-2",
		DedupKey:   api.NewUUID(),
		OccurredAt: now.Add(10 * time.Millisecond),
		ReceivedAt: now.Add(10 * time.Millisecond),
		Status:     store.TriggerEventStatusPending,
	}
	require.NoError(t, f.store.CreateTriggerEvent(ctx, ev2))
	_, out0Dup := fireActionAt(ev2.ID, 0)
	assert.Equal(t, hubfire.FireStatusSkipped, out0Dup.Status)
	assert.Equal(t, "previous run in progress", out0Dup.Error)
}

// TestTriggerActions_RateDecisionSurvivesAcrossPasses lifts review 2's R2-bis
// hub probe (skip-then-window-slide with a fresh EventRateDecision in pass 2,
// plus admit-then-later-event across passes).
func TestTriggerActions_RateDecisionSurvivesAcrossPasses(t *testing.T) {
	f := setupTriggersTestFixture(t)
	ctx := context.Background()
	trigID := api.NewUUID()
	start := time.Now().UTC()

	claim := func(tid, ev string, idx int, at time.Time) *store.TriggerFire {
		tf := &store.TriggerFire{
			ID:          api.NewUUID(),
			ProjectID:   f.projectID,
			TriggerID:   tid,
			EventID:     ev,
			Subject:     "sub",
			ActionType:  store.TriggerActionMessage,
			ActionIndex: idx,
			Status:      store.TriggerFireStatusFiring,
			LeaseUntil:  at.Add(10 * time.Minute),
			StartedAt:   at,
			CreatedAt:   at,
		}
		ok, _, err := f.store.ClaimTriggerFire(ctx, tf)
		require.NoError(t, err)
		require.True(t, ok)
		return tf
	}

	// 1. Skip-then-window-slide across passes (review 2 hub probe).
	priorAt := start.Add(-59 * time.Second)
	prior := claim(trigID, api.NewUUID(), 0, priorAt)
	_, err := f.store.FinishTriggerFire(ctx, prior.ID, store.TriggerFireStatusFired, "", "", "", priorAt)
	require.NoError(t, err)

	evE := api.NewUUID()
	mkReq := func(tid, ev, fireID string, idx int, d *hubfire.EventRateDecision) hubfire.FireRequest {
		return hubfire.FireRequest{
			ProjectID:  f.projectID,
			ActionType: store.TriggerActionMessage,
			Authority: hubfire.RecordedAuthority{
				DefinitionKind: hubfire.DefinitionKindTrigger,
				DefinitionID:   tid,
				FireID:         fireID,
			},
			Event: &hubfire.FireEvent{
				ID:        ev,
				ProjectID: f.projectID,
				Trigger: &hubfire.FireTriggerRef{
					ID:                tid,
					ActionIndex:       idx,
					MaxFiresPerMinute: 1,
					RateDecision:      d,
				},
			},
		}
	}

	pass1 := &hubfire.EventRateDecision{}
	a0 := claim(trigID, evE, 0, start)
	reason0, err := f.srv.checkTriggerFireLimits(ctx, mkReq(trigID, evE, a0.ID, 0, pass1))
	require.NoError(t, err)
	require.Equal(t, trigger.ErrFireRateLimit, reason0, "pass 1: E a0 rate limited")
	_, err = f.store.FinishTriggerFire(ctx, a0.ID, store.TriggerFireStatusSkipped, "", "", trigger.ErrFireRateLimit, start)
	require.NoError(t, err)
	a1 := claim(trigID, evE, 1, start.Add(time.Millisecond))

	// Pass 2: fresh EventRateDecision after prior fire leaves the 60s window.
	time.Sleep(1500 * time.Millisecond)
	pass2 := &hubfire.EventRateDecision{}
	reason1, err := f.srv.checkTriggerFireLimits(ctx, mkReq(trigID, evE, a1.ID, 1, pass2))
	require.NoError(t, err)
	assert.Equal(t, trigger.ErrFireRateLimit, reason1, "pass 2: E a1 must share E a0's rate-limit skip decision across passes")

	// 2. Admit-then-later-event across passes: E2 a0 admitted at T-70s, G fired at T-5s,
	//    then E2 a1 checked in a fresh pass must still be admitted.
	trigID2 := api.NewUUID()
	evE2 := api.NewUUID()
	e2a0 := claim(trigID2, evE2, 0, start.Add(-70*time.Second))
	_, err = f.store.FinishTriggerFire(ctx, e2a0.ID, store.TriggerFireStatusFired, "", "", "", start.Add(-70*time.Second))
	require.NoError(t, err)
	g0 := claim(trigID2, api.NewUUID(), 0, start.Add(-5*time.Second))
	_, err = f.store.FinishTriggerFire(ctx, g0.ID, store.TriggerFireStatusFired, "", "", "", start.Add(-5*time.Second))
	require.NoError(t, err)

	e2a1 := claim(trigID2, evE2, 1, time.Now().UTC())
	reasonE2A1, err := f.srv.checkTriggerFireLimits(ctx, mkReq(trigID2, evE2, e2a1.ID, 1, &hubfire.EventRateDecision{}))
	require.NoError(t, err)
	assert.Empty(t, reasonE2A1, "pass 2: E2 a1 must reuse E2 a0's durable admit decision")

	// 3. End-to-end through HandleItem (reuse-deferral path):
	//    [message, dispatch_agent(subject_mode=reuse)] with maxFiresPerMinute=1.
	//    Pass 1: prior fire at now-59s rate-limits action 0; action 1 is deferred
	//    because its reused agent is still starting.
	//    Pass 2 (after 1.5s sleep so prior fire ages out, and agent becomes running):
	//    action 1 must STILL be skipped "rate limit" via durable sibling state.
	epRec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{
		Name:          "p9-r2bis-reuse-ep",
		SignatureMode: store.WebhookSignatureModeScionV1,
	})
	require.Equal(t, http.StatusCreated, epRec.Code, epRec.Body.String())
	var ep WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(epRec.Body.Bytes(), &ep))

	reuseActions := []TriggerActionSpec{
		{Type: store.TriggerActionMessage, AgentID: f.targetAgent.ID, Message: "msg-0"},
		{Type: store.TriggerActionDispatchAgent, Template: "default", Task: "task-1", ReuseMessage: "reuse-1"},
	}
	createRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:              "p9-r2bis-reuse-defer",
		Source:            TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Actions:           &reuseActions,
		Subject:           &TriggerSubjectSpec{Mode: store.TriggerSubjectModeReuse},
		MaxFiresPerMinute: 1,
	})
	require.Equal(t, http.StatusCreated, createRec.Code, createRec.Body.String())
	var created TriggerResponse
	require.NoError(t, json.Unmarshal(createRec.Body.Bytes(), &created))

	// Create a launching agent and bind a live claim for (created.ID, action_index=1, "sub-reuse").
	launchingAgent := &store.Agent{
		ID:        api.NewUUID(),
		Slug:      "p9-r2bis-launching-" + api.NewUUID()[:6],
		Name:      "p9-r2bis-launching",
		ProjectID: f.projectID,
		Template:  "default",
		Phase:     "starting",
		Created:   time.Now().UTC(),
		Updated:   time.Now().UTC(),
	}
	require.NoError(t, f.store.CreateAgent(ctx, launchingAgent))
	nowReuse := time.Now().UTC()
	subClaim, _, err := f.store.ClaimTriggerSubject(ctx, created.ID, 1, "sub-reuse", api.NewUUID(), api.NewUUID(), nowReuse, nowReuse.Add(-5*time.Minute))
	require.NoError(t, err)
	require.NotNil(t, subClaim)
	bound, err := f.store.BindTriggerSubjectClaim(ctx, subClaim.ID, subClaim.FireID, launchingAgent.ID, nowReuse)
	require.NoError(t, err)
	require.True(t, bound)

	// Seed prior fired event at now-55s on created.ID so pass 1 rate-limits
	// action 0, and after the real 10s ReuseDeferBackoff elapses, the prior
	// fire has aged out of the 60s window.
	priorReuseAt := nowReuse.Add(-55 * time.Second)
	priorReuse := claim(created.ID, api.NewUUID(), 0, priorReuseAt)
	_, err = f.store.FinishTriggerFire(ctx, priorReuse.ID, store.TriggerFireStatusFired, "", "", "", priorReuseAt)
	require.NoError(t, err)

	// Create event and run Pass 1 through HandleItem.
	evReuseID := api.NewUUID()
	require.NoError(t, f.store.CreateTriggerEvent(ctx, &store.TriggerEvent{
		ID:         evReuseID,
		ProjectID:  f.projectID,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   ep.ID,
		Type:       "webhook.received",
		Subject:    "sub-reuse",
		DedupKey:   api.NewUUID(),
		Attributes: map[string]string{
			"delivery":       api.NewUUID(),
			"signature_mode": store.WebhookSignatureModeScionV1,
		},
		OccurredAt: nowReuse,
		ReceivedAt: nowReuse,
		Status:     store.TriggerEventStatusPending,
	}))
	disp, err := f.srv.newTriggerDispatcher(nil)
	require.NoError(t, err)
	qp, err := json.Marshal(trigger.EventQueuePayload{
		Version: trigger.EventQueuePayloadVersion,
		EventID: evReuseID,
	})
	require.NoError(t, err)

	res1 := disp.HandleItem(ctx, durablequeue.Item{
		Seq:       1,
		Kind:      trigger.EventQueueKind,
		DedupKey:  evReuseID,
		ProjectID: f.projectID,
		Payload:   qp,
		Attempts:  1,
	}, nil)
	require.False(t, res1.Done, "pass 1 must defer action 1 while reused agent is starting")
	require.False(t, res1.RetryAt.IsZero())

	// Transition agent to running and wait past ReuseDeferBackoff (10s) so
	// both the deferred fire is ready for promotion AND the prior fire (-55s)
	// has aged out of the 60s window.
	launchingAgent.Phase = "running"
	require.NoError(t, f.store.UpdateAgent(ctx, launchingAgent))
	time.Sleep(10500 * time.Millisecond)

	// Pass 2: HandleItem re-handles the event with a fresh EventRateDecision.
	// Action 1 must reuse action 0's durable rate-limit skip decision.
	res2 := disp.HandleItem(ctx, durablequeue.Item{
		Seq:       1,
		Kind:      trigger.EventQueueKind,
		DedupKey:  evReuseID,
		ProjectID: f.projectID,
		Payload:   qp,
		Attempts:  2,
	}, nil)
	require.True(t, res2.Done)
	require.NoError(t, res2.Err)

	firesRes, err := f.store.ListTriggerFires(ctx, store.TriggerFireFilter{
		TriggerID: created.ID,
		EventID:   evReuseID,
	}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, firesRes.Items, 2)
	byIdx := map[int]store.TriggerFire{}
	for _, tf := range firesRes.Items {
		byIdx[tf.ActionIndex] = tf
	}
	assert.Equal(t, store.TriggerFireStatusSkipped, byIdx[0].Status)
	assert.Equal(t, trigger.ErrFireRateLimit, byIdx[0].Error)
	assert.Equal(t, store.TriggerFireStatusSkipped, byIdx[1].Status)
	assert.Equal(t, trigger.ErrFireRateLimit, byIdx[1].Error)
}

// TestTriggerActions_ExecRateCapIgnoresNonExecSiblingsInMixed8ActionTrigger
// verifies O-c: a mixed 8-action trigger (7 message + 1 exec) firing 5 events
// (35 message rows + 5 exec rows) does NOT trip the 30/min P8 exec rate cap.
func TestTriggerActions_ExecRateCapIgnoresNonExecSiblingsInMixed8ActionTrigger(t *testing.T) {
	f := newExecRouteFixture(t)
	enableTriggerExecExperiments(t, f.srv)
	f.srv.SetHubID("test-hub-p9-oc")
	f.srv.SetSecretBackend(secret.NewLocalBackendWithCipher(f.store, f.srv.hubID, f.srv.cipher))
	f.srv.config.HubEndpoint = "https://hub-public.example.com"
	f.startAgent(t)

	ctx := context.Background()
	now := time.Now().UTC()
	projectID := f.launched.ProjectID

	epReq := authoredRequest(t, f.owner, http.MethodPost, "/api/v1/projects/"+projectID+"/webhook-endpoints", CreateWebhookEndpointRequest{
		Name:          "p9-oc-ep",
		SignatureMode: store.WebhookSignatureModeScionV1,
	})
	epRec := httptest.NewRecorder()
	f.srv.handleWebhookEndpoints(epRec, epReq, projectID, "")
	require.Equal(t, http.StatusCreated, epRec.Code, epRec.Body.String())
	var ep WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(epRec.Body.Bytes(), &ep))

	mixed := make([]TriggerActionSpec, 8)
	for i := 0; i < 7; i++ {
		mixed[i] = TriggerActionSpec{Type: store.TriggerActionMessage, AgentID: f.launched.ID, Message: "notify"}
	}
	mixed[7] = TriggerActionSpec{Type: store.TriggerActionExec, AgentID: f.launched.ID, Command: []string{"/bin/echo", "ok"}, TimeoutSeconds: 30}

	trReq := authoredRequest(t, f.owner, http.MethodPost, "/api/v1/projects/"+projectID+"/triggers", CreateTriggerRequest{
		Name:    "p9-oc-mixed-8",
		Source:  TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Match:   &TriggerMatchSpec{Types: []string{"webhook.received"}},
		Actions: &mixed,
	})
	trRec := httptest.NewRecorder()
	f.srv.handleTriggers(trRec, trReq, projectID, "")
	require.Equal(t, http.StatusCreated, trRec.Code, trRec.Body.String())
	var trResp TriggerResponse
	require.NoError(t, json.Unmarshal(trRec.Body.Bytes(), &trResp))

	tr, err := f.store.GetTrigger(ctx, trResp.ID)
	require.NoError(t, err)

	// Seed 5 prior events x 7 message rows = 35 non-exec rows (> 30) + 5 exec rows (< 30).
	for e := 0; e < 5; e++ {
		evID := api.NewUUID()
		for idx := 0; idx < 8; idx++ {
			actType := store.TriggerActionMessage
			if idx == 7 {
				actType = store.TriggerActionExec
			}
			tf := &store.TriggerFire{
				ID:          api.NewUUID(),
				ProjectID:   projectID,
				TriggerID:   tr.ID,
				EventID:     evID,
				Subject:     "oc",
				ActionType:  actType,
				ActionIndex: idx,
				Status:      store.TriggerFireStatusFired,
				LeaseUntil:  now.Add(10 * time.Minute),
				StartedAt:   now,
				CreatedAt:   now,
			}
			ok, _, cErr := f.store.ClaimTriggerFire(ctx, tf)
			require.NoError(t, cErr)
			require.True(t, ok)
		}
	}

	// Event 6's exec action (index 7) must be admitted (5 exec fires < 30, ignoring 35 message rows).
	ev6 := api.NewUUID()
	tf6 := &store.TriggerFire{
		ID:          api.NewUUID(),
		ProjectID:   projectID,
		TriggerID:   tr.ID,
		EventID:     ev6,
		Subject:     "oc",
		ActionType:  store.TriggerActionExec,
		ActionIndex: 7,
		Status:      store.TriggerFireStatusFiring,
		LeaseUntil:  now.Add(10 * time.Minute),
		StartedAt:   now,
		CreatedAt:   now,
	}
	ok, _, err := f.store.ClaimTriggerFire(ctx, tf6)
	require.NoError(t, err)
	require.True(t, ok)

	out := f.srv.fireAction(ctx, hubfire.FireRequest{
		ProjectID:  projectID,
		ActionType: store.TriggerActionExec,
		Payload:    tr.Actions[7].Payload,
		Authority: hubfire.RecordedAuthority{
			Attribution:    tr.InitiatorAttribution,
			Ceiling:        tr.AuthorityCeiling,
			DefinitionKind: hubfire.DefinitionKindTrigger,
			DefinitionID:   tr.ID,
			FireID:         tf6.ID,
		},
		Event: &hubfire.FireEvent{
			ID:         ev6,
			ProjectID:  projectID,
			SourceKind: store.TriggerSourceKindWebhook,
			SourceID:   ep.ID,
			Type:       "webhook.received",
			Subject:    "oc",
			OccurredAt: now,
			ReceivedAt: now,
			Attributes: map[string]string{
				"delivery":       api.NewUUID(),
				"signature_mode": store.WebhookSignatureModeScionV1,
				"endpoint_name":  ep.Name,
			},
			Trigger: &hubfire.FireTriggerRef{
				ID:          tr.ID,
				Name:        tr.Name,
				ActionIndex: 7,
			},
		},
		FireAt:   now,
		Attempts: 1,
	})
	require.Equal(t, hubfire.FireStatusFired, out.Status, "exec action must not be rate-limited by non-exec sibling rows: err=%q", out.Error)
}
