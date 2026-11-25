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
	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger"
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

	// Viewer (no agent.attach) sees fail-closed redaction on EVERY exec element, while message is intact.
	getRec := f.doTrigger(t, f.viewerAuth, created.ID, http.MethodGet, nil)
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
	assert.Equal(t, store.TriggerFireStatusFired, ev1ByIdx[0].Status)
	assert.Equal(t, store.TriggerFireStatusFired, ev1ByIdx[1].Status)

	require.Len(t, ev2ByIdx, 2)
	assert.Equal(t, store.TriggerFireStatusSkipped, ev2ByIdx[0].Status)
	assert.Equal(t, trigger.ErrFireRateLimit, ev2ByIdx[0].Error)
	assert.Equal(t, store.TriggerFireStatusSkipped, ev2ByIdx[1].Status)
	assert.Equal(t, trigger.ErrFireRateLimit, ev2ByIdx[1].Error)
}
