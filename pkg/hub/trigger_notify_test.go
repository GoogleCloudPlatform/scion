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
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/durablequeue"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

type notifyTestDispatcher struct {
	recordingDispatcher
	mu      sync.Mutex
	failIDs map[string]error
}

func (d *notifyTestDispatcher) DispatchAgentMessage(ctx context.Context, agent *store.Agent, message string, interrupt bool, structuredMsg *messages.StructuredMessage) error {
	d.mu.Lock()
	err := d.failIDs[agent.ID]
	d.mu.Unlock()
	if err != nil {
		return err
	}
	return d.recordingDispatcher.DispatchAgentMessage(ctx, agent, message, interrupt, structuredMsg)
}

// setFireNotifyClockForTest overrides the clock used for notify-on-failure
// cooldown windows and timestamps in deterministic tests. Passing nil restores
// time.Now.
func (s *Server) setFireNotifyClockForTest(fn func() time.Time) {
	if s == nil {
		return
	}
	s.triggers.mu.Lock()
	s.triggers.notifyNow = fn
	s.triggers.mu.Unlock()
}

func TestTriggerNotify_AuthoringValidationAndRevisionBump(t *testing.T) {
	f := setupTriggersTestFixture(t)
	ctx := context.Background()

	epRec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{
		Name:          "notify-auth-ep",
		SignatureMode: store.WebhookSignatureModeScionV1,
	})
	require.Equal(t, http.StatusCreated, epRec.Code, epRec.Body.String())
	var ep WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(epRec.Body.Bytes(), &ep))

	// Create a second agent in another project that f.member cannot message.
	otherAgent := &store.Agent{
		ID:        api.NewUUID(),
		Slug:      "other-proj-agent",
		Name:      "other-proj-agent",
		ProjectID: f.otherProjID,
		Phase:     string(state.PhaseRunning),
		OwnerID:   f.owner.ID,
	}
	require.NoError(t, f.store.CreateAgent(ctx, otherAgent))

	// 1. Author who cannot message the notify target agent (cross-project) cannot save the rule.
	unauthRec := f.doTrigger(t, f.memberAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "unauth-notify-target",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Action: TriggerActionSpec{Type: store.TriggerActionMessage, AgentID: f.targetAgent.ID, Message: "hi"},
		Notify: &TriggerNotifySpec{
			AgentID: otherAgent.ID,
		},
	})
	assert.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, unauthRec.Code, unauthRec.Body.String())

	// 2. Invalid notify.onOutcomes value ("fired") is rejected with 400.
	badOutcomeRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "bad-notify-outcome",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Action: TriggerActionSpec{Type: store.TriggerActionMessage, AgentID: f.targetAgent.ID, Message: "hi"},
		Notify: &TriggerNotifySpec{
			OnOutcomes: []string{"failed", "fired"},
		},
	})
	assert.Equal(t, http.StatusBadRequest, badOutcomeRec.Code, badOutcomeRec.Body.String())

	// 3. Valid notify policy saves, returns normalized defaults, and GET returns it.
	createRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "valid-notify-rule",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Action: TriggerActionSpec{Type: store.TriggerActionMessage, AgentID: f.targetAgent.ID, Message: "hi"},
		Notify: &TriggerNotifySpec{
			AgentName: f.agentA.Name,
		},
	})
	require.Equal(t, http.StatusCreated, createRec.Code, createRec.Body.String())
	var created TriggerResponse
	require.NoError(t, json.Unmarshal(createRec.Body.Bytes(), &created))
	assert.Equal(t, 1, created.AuthorizationRevision)
	require.NotNil(t, created.Notify)
	require.NotNil(t, created.Notify.UserNotice)
	assert.True(t, *created.Notify.UserNotice)
	assert.Equal(t, f.agentA.ID, created.Notify.AgentID)
	assert.Equal(t, f.agentA.Name, created.Notify.AgentName)
	assert.Equal(t, []string{"failed", "refused", "expired"}, created.Notify.OnOutcomes)

	// 4. PATCH changing notify bumps AuthorizationRevision and preserves unmodified notify fields.
	patchRec := f.doTrigger(t, f.ownerAuth, created.ID, http.MethodPatch, UpdateTriggerRequest{
		Notify: &TriggerNotifyPatchSpec{
			UserNotice: boolPtr(false),
		},
	})
	require.Equal(t, http.StatusOK, patchRec.Code, patchRec.Body.String())
	var patched TriggerResponse
	require.NoError(t, json.Unmarshal(patchRec.Body.Bytes(), &patched))
	assert.Equal(t, 2, patched.AuthorizationRevision, "notify edit must bump authorization revision")
	require.NotNil(t, patched.Notify)
	require.NotNil(t, patched.Notify.UserNotice)
	assert.False(t, *patched.Notify.UserNotice)
	assert.Equal(t, f.agentA.ID, patched.Notify.AgentID, "partial notify patch must preserve existing agentId")
	assert.Equal(t, []string{"failed", "refused", "expired"}, patched.Notify.OnOutcomes)

	// 5. PATCH trying to point notify at an unauthorized target is rejected.
	badPatchRec := f.doTrigger(t, f.ownerAuth, created.ID, http.MethodPatch, UpdateTriggerRequest{
		Notify: &TriggerNotifyPatchSpec{
			AgentID: &otherAgent.ID,
		},
	})
	assert.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, badPatchRec.Code, badPatchRec.Body.String())
}

func TestTriggerNotify_EndToEndCooldownUserAndAgentSinks(t *testing.T) {
	f := setupTriggersTestFixture(t)
	ctx := context.Background()

	now := time.Date(2026, 11, 27, 15, 0, 0, 0, time.UTC)
	var clockMu sync.Mutex
	curTime := now
	f.srv.setFireNotifyClockForTest(func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return curTime
	})

	disp := &notifyTestDispatcher{
		failIDs: map[string]error{
			f.targetAgent.ID: errors.New("simulated broker failure"),
		},
	}
	f.srv.SetDispatcher(disp)

	epRec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{
		Name:          "notify-e2e-ep",
		SignatureMode: store.WebhookSignatureModeScionV1,
	})
	require.Equal(t, http.StatusCreated, epRec.Code, epRec.Body.String())
	var ep WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(epRec.Body.Bytes(), &ep))

	trRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "notify-e2e-trig",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Action: TriggerActionSpec{Type: store.TriggerActionMessage, AgentID: f.targetAgent.ID, Message: "handle event"},
		Notify: &TriggerNotifySpec{
			UserNotice: boolPtr(true),
			AgentID:    f.agentA.ID,
		},
	})
	require.Equal(t, http.StatusCreated, trRec.Code, trRec.Body.String())
	var trResp TriggerResponse
	require.NoError(t, json.Unmarshal(trRec.Body.Bytes(), &trResp))

	trigDisp, err := f.srv.newTriggerDispatcher(nil)
	require.NoError(t, err)

	payloadSentinel := "TOP-SECRET-PAYLOAD-SENTINEL-98765"
	runOneEvent := func(deliv string) string {
		evID := api.NewUUID()
		require.NoError(t, f.store.CreateTriggerEvent(ctx, &store.TriggerEvent{
			ID:           evID,
			ProjectID:    f.projectID,
			SourceKind:   store.TriggerSourceKindWebhook,
			SourceID:     ep.ID,
			Type:         "webhook.received",
			Subject:      "webhook:" + ep.ID,
			DedupKey:     deliv,
			OccurredAt:   curTime,
			ReceivedAt:   curTime,
			Attributes:   map[string]string{"endpoint_name": ep.Name, "secret_attr": payloadSentinel},
			Payload:      `{"secret":"` + payloadSentinel + `"}`,
			PayloadBytes: len(payloadSentinel) + 14,
			Status:       store.TriggerEventStatusPending,
			CreatedAt:    curTime,
		}))
		qp, mErr := json.Marshal(trigger.EventQueuePayload{Version: trigger.EventQueuePayloadVersion, EventID: evID})
		require.NoError(t, mErr)
		res := trigDisp.HandleItem(ctx, durablequeue.Item{Payload: qp, Attempts: 1}, nil)
		require.True(t, res.Done)
		return evID
	}

	// Event 1 fails -> produces 1 user notice + 1 agent message + 1 notify fire row.
	runOneEvent("deliv-1")

	notifs, err := f.store.GetNotifications(ctx, store.SubscriberTypeUser, f.owner.ID, false)
	require.NoError(t, err)
	require.Len(t, notifs, 1, "expected exactly 1 user notice on first failure")
	assert.Equal(t, store.NotificationKindTriggerFireFailed, notifs[0].Kind)
	assert.Equal(t, store.NotificationResourceTrigger, notifs[0].ResourceType)
	assert.Equal(t, trResp.ID, notifs[0].ResourceID)
	assert.Equal(t, store.NotificationStatusTriggerFireFailed, notifs[0].Status)
	assert.Contains(t, notifs[0].Message, "notify-e2e-trig")
	assert.Contains(t, notifs[0].Message, "failed")
	assert.NotContains(t, notifs[0].Message, payloadSentinel, "user notice must never contain payload text")

	calls := disp.getCalls()
	require.Len(t, calls, 1, "expected exactly 1 agent notification message")
	assert.Equal(t, f.agentA.ID, calls[0].Agent.ID)
	assert.NotContains(t, calls[0].Message, payloadSentinel, "agent notification message must never contain payload text")
	require.NotNil(t, calls[0].StructuredMessage)
	assert.NotContains(t, calls[0].StructuredMessage.DeliveryText, payloadSentinel, "agent delivery envelope must never contain payload text")
	env := extractEnvelopeJSONFromDelivery(t, calls[0].StructuredMessage.DeliveryText)
	assert.Equal(t, FireNotifyEventType, env.Event.EventType)
	assert.Nil(t, env.Event.External, "synthetic trigger.fire_failed event must have no external_payload")
	assert.Equal(t, trResp.ID, env.Event.Attributes["trigger"])
	assert.Equal(t, "failed", env.Event.Attributes["outcome"])
	assert.NotEmpty(t, env.Event.Attributes["fire_id"])
	assert.NotEmpty(t, env.Event.Attributes["reason"])
	assert.NotContains(t, env.Event.Attributes["reason"], payloadSentinel)

	// Verify agent-message notify fire row is recorded in trigger_fires with
	// action_type="notify" and action_index=-1 and deterministic fixed-window
	// cooldown event_id, while internal user-notice cooldown claim rows (-2)
	// stay hidden from ListTriggerFires.
	fires, err := f.store.ListTriggerFires(ctx, store.TriggerFireFilter{
		ProjectID: f.projectID,
		TriggerID: trResp.ID,
	}, store.ListOptions{Limit: 20})
	require.NoError(t, err)
	var agentNotifyRows, userNotifyRows []store.TriggerFire
	for _, fr := range fires.Items {
		if fr.ActionType == store.TriggerActionNotify {
			switch fr.ActionIndex {
			case store.TriggerFireActionIndexNotify:
				agentNotifyRows = append(agentNotifyRows, fr)
			case store.TriggerFireActionIndexNotifyUser:
				userNotifyRows = append(userNotifyRows, fr)
			}
		}
	}
	require.Len(t, agentNotifyRows, 1)
	assert.Equal(t, store.TriggerFireStatusFired, agentNotifyRows[0].Status)
	win := fireNotifyCooldownWindow(now, DefaultFireNotifyCooldown)
	assert.Equal(t, TriggerNotifyCooldownEventID(trResp.ID, FireNotifySinkAgent, "failed", env.Event.Attributes["reason"], win), agentNotifyRows[0].EventID)
	assert.Empty(t, userNotifyRows, "internal user-notice cooldown claim rows must not pollute ListTriggerFires")

	// Event 2 fails 10 minutes later (within 1h cooldown) -> suppressed for BOTH sinks.
	clockMu.Lock()
	curTime = now.Add(10 * time.Minute)
	clockMu.Unlock()
	runOneEvent("deliv-2")

	notifsAfter2, err := f.store.GetNotifications(ctx, store.SubscriberTypeUser, f.owner.ID, false)
	require.NoError(t, err)
	assert.Len(t, notifsAfter2, 1, "second failure within cooldown must not emit a second user notice")
	assert.Len(t, disp.getCalls(), 1, "second failure within cooldown must not emit a second agent message")

	// Event 3 fails after cooldown expires (1h + 1s) -> emits 2nd user notice + 2nd agent message.
	clockMu.Lock()
	curTime = now.Add(time.Hour + time.Second)
	clockMu.Unlock()
	runOneEvent("deliv-3")

	notifsAfter3, err := f.store.GetNotifications(ctx, store.SubscriberTypeUser, f.owner.ID, false)
	require.NoError(t, err)
	assert.Len(t, notifsAfter3, 2, "failure after cooldown expiry must emit a new user notice")
	assert.Len(t, disp.getCalls(), 2, "failure after cooldown expiry must emit a new agent message")
}

func TestTriggerNotify_AgentCreatorDelegatingUserNoRecursionAndNoWake(t *testing.T) {
	f := setupTriggersTestFixture(t)
	ctx := context.Background()

	disp := &notifyTestDispatcher{}
	f.srv.SetDispatcher(disp)

	// Create a suspended notify target agent to prove notifications never wake it.
	suspendedNotifyAgent := &store.Agent{
		ID:        api.NewUUID(),
		Slug:      "suspended-notify-target",
		Name:      "suspended-notify-target",
		ProjectID: f.projectID,
		Phase:     string(state.PhaseSuspended),
		OwnerID:   f.owner.ID,
	}
	require.NoError(t, f.store.CreateAgent(ctx, suspendedNotifyAgent))

	// Create a trigger authored by agentA (whose owner/delegating user is f.owner).
	trID := api.NewUUID()
	require.NoError(t, f.store.CreateTrigger(ctx, &store.Trigger{
		ID:         trID,
		ProjectID:  f.projectID,
		Name:       "agent-authored-trig",
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   api.NewUUID(),
		MatchTypes: []string{"webhook.received"},
		Actions: []store.TriggerAction{
			{Type: store.TriggerActionMessage, Payload: `{"agentId":"` + f.agentA.ID + `","message":"hi"}`},
		},
		Notify: &store.TriggerNotifyPolicy{
			UserNotice: boolPtr(true),
			AgentID:    suspendedNotifyAgent.ID,
			AgentName:  suspendedNotifyAgent.Name,
		},
		Status:    store.TriggerStatusActive,
		CreatedBy: f.agentA.ID,
		InitiatorAttribution: store.InitiatorAttribution{
			InitiatorPrincipalKind:  store.DelegationPrincipalAgent,
			InitiatorPrincipalID:    f.agentA.ID,
			InitiatorCredentialKind: store.InitiatorCredentialKindAgent,
			AttributionVersion:      1,
			AuthorizationRevision:   1,
		},
		AuthorityCeiling: store.EffectCeiling{
			Kind:          store.EffectCeilingBounded,
			Version:       1,
			PermissionIDs: []string{scheduledMessagePermission},
		},
	}))

	tr, err := f.store.GetTrigger(ctx, trID)
	require.NoError(t, err)

	origFire := &store.TriggerFire{
		ID:          api.NewUUID(),
		ProjectID:   f.projectID,
		TriggerID:   tr.ID,
		EventID:     api.NewUUID(),
		ActionType:  store.TriggerActionMessage,
		ActionIndex: 0,
		Status:      store.TriggerFireStatusFailed,
		Error:       "message failed",
	}

	// Notify on origFire:
	// 1) User notice goes to f.owner.ID (agentA's delegating user).
	// 2) Agent sink targets suspendedNotifyAgent -> must NOT wake it; records a
	//    notify fire row that is refused/skipped and never re-notifies!
	f.srv.notifyTriggerFire(ctx, tr, origFire, hubfire.FireOutcome{
		Status: hubfire.FireStatusFailed,
		Error:  "message failed",
	})

	notifs, err := f.store.GetNotifications(ctx, store.SubscriberTypeUser, f.owner.ID, false)
	require.NoError(t, err)
	require.Len(t, notifs, 1, "agent creator's delegating user must receive the user notice")
	assert.Equal(t, store.NotificationKindTriggerFireFailed, notifs[0].Kind)
	assert.Empty(t, disp.getCalls(), "suspended notify target must not be woken or delivered to")

	// Direct recursive call with ActionType="notify" or Event.Type="trigger.fire_failed" must be a hard no-op.
	f.srv.setFireNotifyClockForTest(func() time.Time { return time.Now().UTC().Add(2 * time.Hour) })
	f.srv.NotifyFireOutcome(ctx, hubfire.FireRequest{
		ProjectID:  f.projectID,
		ActionType: store.TriggerActionNotify,
		Authority: hubfire.RecordedAuthority{
			Attribution:    tr.InitiatorAttribution,
			Ceiling:        tr.AuthorityCeiling,
			DefinitionKind: hubfire.DefinitionKindTrigger,
			DefinitionID:   tr.ID,
			FireID:         api.NewUUID(),
		},
	}, hubfire.FireOutcome{Status: hubfire.FireStatusFailed, Error: "notify failed"}, effectiveTriggerNotifyPolicy(tr))

	f.srv.NotifyFireOutcome(ctx, hubfire.FireRequest{
		ProjectID:  f.projectID,
		ActionType: store.TriggerActionMessage,
		Event:      &hubfire.FireEvent{Type: FireNotifyEventType},
		Authority: hubfire.RecordedAuthority{
			Attribution:    tr.InitiatorAttribution,
			Ceiling:        tr.AuthorityCeiling,
			DefinitionKind: hubfire.DefinitionKindTrigger,
			DefinitionID:   tr.ID,
			FireID:         api.NewUUID(),
		},
	}, hubfire.FireOutcome{Status: hubfire.FireStatusFailed, Error: "notify failed"}, effectiveTriggerNotifyPolicy(tr))

	notifsAfterRecursion, err := f.store.GetNotifications(ctx, store.SubscriberTypeUser, f.owner.ID, false)
	require.NoError(t, err)
	assert.Len(t, notifsAfterRecursion, 1, "notify rows must never re-notify")
}

func TestTriggerNotify_ExpiredProjectCapAndMultiAction(t *testing.T) {
	f := setupTriggersTestFixture(t)
	ctx := context.Background()

	now := time.Date(2026, 11, 27, 16, 0, 0, 0, time.UTC)
	curTime := now
	f.srv.setFireNotifyClockForTest(func() time.Time { return curTime })

	disp := &notifyTestDispatcher{
		failIDs: map[string]error{
			f.agentB.ID: errors.New("action 1 boom"),
		},
	}
	f.srv.SetDispatcher(disp)

	epRec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{
		Name:          "notify-multi-ep",
		SignatureMode: store.WebhookSignatureModeScionV1,
	})
	require.Equal(t, http.StatusCreated, epRec.Code, epRec.Body.String())
	var ep WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(epRec.Body.Bytes(), &ep))

	// P9 multi-action trigger: action 0 succeeds (targetAgent), action 1 fails (agentB).
	// Default onOutcomes (failed, refused, expired) — skipped is NOT opted in.
	twoActions := []TriggerActionSpec{
		{Type: store.TriggerActionMessage, AgentID: f.targetAgent.ID, Message: "ok action"},
		{Type: store.TriggerActionMessage, AgentID: f.agentB.ID, Message: "failing action"},
	}
	trRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:    "notify-multi-action",
		Source:  TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Actions: &twoActions,
		Notify: &TriggerNotifySpec{
			UserNotice: boolPtr(true),
			AgentID:    f.agentA.ID,
		},
	})
	require.Equal(t, http.StatusCreated, trRec.Code, trRec.Body.String())
	var trResp TriggerResponse
	require.NoError(t, json.Unmarshal(trRec.Body.Bytes(), &trResp))

	trigDisp, err := f.srv.newTriggerDispatcher(nil)
	require.NoError(t, err)

	evID := api.NewUUID()
	require.NoError(t, f.store.CreateTriggerEvent(ctx, &store.TriggerEvent{
		ID:         evID,
		ProjectID:  f.projectID,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   ep.ID,
		Type:       "webhook.received",
		Subject:    "s-multi",
		DedupKey:   "multi-1",
		OccurredAt: curTime,
		ReceivedAt: curTime,
		Payload:    `{"ok":true}`,
		Status:     store.TriggerEventStatusPending,
		CreatedAt:  curTime,
	}))
	qp, err := json.Marshal(trigger.EventQueuePayload{Version: trigger.EventQueuePayloadVersion, EventID: evID})
	require.NoError(t, err)
	res := trigDisp.HandleItem(ctx, durablequeue.Item{Payload: qp, Attempts: 1}, nil)
	require.True(t, res.Done)

	// 1 user notice + 1 agent message to agentA (plus 1 real message to targetAgent).
	notifs, err := f.store.GetNotifications(ctx, store.SubscriberTypeUser, f.owner.ID, false)
	require.NoError(t, err)
	require.Len(t, notifs, 1, "multi-action trigger with 1 failing action must produce 1 user notice")
	var notifyAgentMsgs int
	for _, c := range disp.getCalls() {
		if c.Agent.ID == f.agentA.ID {
			notifyAgentMsgs++
		}
	}
	assert.Equal(t, 1, notifyAgentMsgs, "multi-action trigger with 1 failing action must produce 1 agent notification")

	// Advance past cooldown and verify:
	// (a) regular skipped ("target not running") does NOT notify when skipped is not opted in;
	// (b) project trigger-agent cap skip DOES notify even when skipped is not opted in;
	// (c) expired outcome DOES notify.
	tr, err := f.store.GetTrigger(ctx, trResp.ID)
	require.NoError(t, err)

	curTime = now.Add(2 * time.Hour)
	f.srv.notifyTriggerFire(ctx, tr, &store.TriggerFire{
		ID: api.NewUUID(), ProjectID: f.projectID, TriggerID: tr.ID, EventID: api.NewUUID(),
		ActionType: store.TriggerActionMessage, ActionIndex: 0,
	}, hubfire.FireOutcome{Status: hubfire.FireStatusSkipped, Error: trigger.ErrFireTargetNotRunning})

	notifsAfterRegularSkip, err := f.store.GetNotifications(ctx, store.SubscriberTypeUser, f.owner.ID, false)
	require.NoError(t, err)
	assert.Len(t, notifsAfterRegularSkip, 1, "regular skipped must not notify unless opted in")

	f.srv.notifyTriggerFire(ctx, tr, &store.TriggerFire{
		ID: api.NewUUID(), ProjectID: f.projectID, TriggerID: tr.ID, EventID: api.NewUUID(),
		ActionType: store.TriggerActionDispatchAgent, ActionIndex: 0,
	}, hubfire.FireOutcome{Status: hubfire.FireStatusSkipped, Error: trigger.ErrFireProjectTriggerAgentCap})

	notifsAfterCap, err := f.store.GetNotifications(ctx, store.SubscriberTypeUser, f.owner.ID, false)
	require.NoError(t, err)
	assert.Len(t, notifsAfterCap, 2, "project trigger-agent cap skip must route to notify even without skipped opt-in")
	foundCap := false
	for _, n := range notifsAfterCap {
		if strings.Contains(n.Message, trigger.ErrFireProjectTriggerAgentCap) {
			foundCap = true
		}
	}
	assert.True(t, foundCap, "expected project trigger-agent cap notification")

	curTime = now.Add(4 * time.Hour)
	f.srv.notifyTriggerFire(ctx, tr, &store.TriggerFire{
		ID: api.NewUUID(), ProjectID: f.projectID, TriggerID: tr.ID, EventID: api.NewUUID(),
		ActionType: store.TriggerActionMessage, ActionIndex: 0,
	}, hubfire.FireOutcome{Status: hubfire.FireStatusExpired, Error: trigger.ErrFireOfflineQueueExpired})

	notifsAfterExpired, err := f.store.GetNotifications(ctx, store.SubscriberTypeUser, f.owner.ID, false)
	require.NoError(t, err)
	assert.Len(t, notifsAfterExpired, 3, "expired outcome must route to notify")
	foundExpired := false
	for _, n := range notifsAfterExpired {
		if strings.Contains(n.Message, "expired") {
			foundExpired = true
		}
	}
	assert.True(t, foundExpired, "expected expired notification")
}
