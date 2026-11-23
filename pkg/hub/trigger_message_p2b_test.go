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
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/durablequeue"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/triggertemplate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTriggerMessageP2b_AuthoringAndScheduleRefusals(t *testing.T) {
	f := setupTriggersTestFixture(t)

	epRec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{
		Name: "p2b-author-ep",
	})
	require.Equal(t, http.StatusCreated, epRec.Code, epRec.Body.String())
	var ep WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(epRec.Body.Bytes(), &ep))

	t.Run("valid message template with trusted and opted-in untrusted vars + offline queue default TTL", func(t *testing.T) {
		rec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
			Name:   "msg-p2b-valid",
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
			Action: TriggerActionSpec{
				Type:          "message",
				AgentID:       f.targetAgent.ID,
				Message:       "Trigger ${trigger.name}: PR #${event.attributes.number} (${event.payload.title})",
				Offline:       MessageOfflineQueue,
				UntrustedVars: []string{"event.attributes.number", "event.payload.title", "event.attributes.number"},
			},
		})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		var tr TriggerResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &tr))
		assert.Equal(t, MessageOfflineQueue, tr.Action.Offline)
		assert.Equal(t, DefaultMessageQueueTTLSeconds, tr.Action.QueueTTLSeconds)
		assert.Equal(t, []string{"event.attributes.number", "event.payload.title"}, tr.Action.UntrustedVars)
	})

	t.Run("refuses missing untrusted opt-in", func(t *testing.T) {
		rec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
			Name:   "msg-p2b-no-optin",
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
			Action: TriggerActionSpec{
				Type:    "message",
				AgentID: f.targetAgent.ID,
				Message: "PR #${event.attributes.number}",
			},
		})
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "untrusted_var_not_allowed")
	})

	t.Run("refuses plain with untrusted placeholders", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			vars []string
		}{
			{"with untrustedVars", []string{"event.subject"}},
			{"without untrustedVars", nil},
		} {
			t.Run(tc.name, func(t *testing.T) {
				rec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
					Name:   "msg-p2b-plain-" + tc.name,
					Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
					Action: TriggerActionSpec{
						Type:          "message",
						AgentID:       f.targetAgent.ID,
						Message:       "Subj: ${event.subject}",
						Plain:         true,
						UntrustedVars: tc.vars,
					},
				})
				require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
				assert.Contains(t, rec.Body.String(), "plain_with_untrusted")
			})
		}
	})

	t.Run("allows plain with trusted-only placeholders", func(t *testing.T) {
		rec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
			Name:   "msg-p2b-plain-trusted",
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
			Action: TriggerActionSpec{
				Type:    "message",
				AgentID: f.targetAgent.ID,
				Message: "Event ${event.id} from ${trigger.name}",
				Plain:   true,
			},
		})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	})

	t.Run("refuses invalid template syntax and post-phase result.*", func(t *testing.T) {
		for _, msg := range []string{"bad ${", "post ${result.agent_id}"} {
			rec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
				Name:   "msg-p2b-bad-tmpl",
				Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
				Action: TriggerActionSpec{
					Type:    "message",
					AgentID: f.targetAgent.ID,
					Message: msg,
				},
			})
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), "invalid_template")
		}
	})

	t.Run("refuses invalid offline and queueTtlSeconds", func(t *testing.T) {
		cases := []struct {
			offline string
			ttl     int
		}{
			{"invalid", 0},
			{MessageOfflineSkip, 120},
			{"", 120},
			{MessageOfflineQueue, 59},
			{MessageOfflineQueue, MaxMessageQueueTTLSeconds + 1},
		}
		for _, tc := range cases {
			rec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
				Name:   "msg-p2b-bad-offline",
				Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
				Action: TriggerActionSpec{
					Type:            "message",
					AgentID:         f.targetAgent.ID,
					Message:         "hi",
					Offline:         tc.offline,
					QueueTTLSeconds: tc.ttl,
				},
			})
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), "invalid_value")
		}
	})

	t.Run("schedules refuse offline, queueTtlSeconds, and untrustedVars", func(t *testing.T) {
		badPayloads := []string{
			`{"agentId":"` + f.targetAgent.ID + `","message":"hi","offline":"queue"}`,
			`{"agentId":"` + f.targetAgent.ID + `","message":"hi","queueTtlSeconds":300}`,
			`{"agentId":"` + f.targetAgent.ID + `","message":"hi","untrustedVars":["event.subject"]}`,
		}
		for _, raw := range badPayloads {
			// Recurring schedule
			req := authoredRequest(t, f.ownerAuth, http.MethodPost, "/api/v1/projects/"+f.projectID+"/schedules", map[string]any{
				"name":      "sched-refuse",
				"cronExpr":  "0 * * * *",
				"eventType": "message",
				"payload":   raw,
			})
			rec := httptest.NewRecorder()
			f.srv.handleSchedules(rec, req, f.projectID, "")
			assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), "not supported for schedules")

			// One-shot scheduled event
			req2 := authoredRequest(t, f.ownerAuth, http.MethodPost, "/api/v1/projects/"+f.projectID+"/scheduled-events", map[string]any{
				"eventType": "message",
				"fireIn":    "5m",
				"payload":   raw,
			})
			rec2 := httptest.NewRecorder()
			f.srv.handleScheduledEvents(rec2, req2, f.projectID, "")
			assert.Equal(t, http.StatusBadRequest, rec2.Code, rec2.Body.String())
			assert.Contains(t, rec2.Body.String(), "not supported for schedules")
		}
	})
}

func TestTriggerMessageP2b_TemplatingFireEnvelopeAuditAndRender(t *testing.T) {
	f := setupTriggersTestFixture(t)
	enableWriteDenySwitch(t, f.srv)
	ctx := context.Background()

	disp := &recordingDispatcher{}
	f.srv.SetDispatcher(disp)

	done := f.srv.startTriggerDrainer(ctx)
	require.NotNil(t, done)
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		f.srv.stopTriggerDrainer(stopCtx)
	})

	epRec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{
		Name: "p2b-tmpl-ep",
	})
	require.Equal(t, http.StatusCreated, epRec.Code, epRec.Body.String())
	var ep WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(epRec.Body.Bytes(), &ep))

	// Create two message triggers: one valid, one referencing a missing required field.
	okTrigRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "msg-tmpl-ok",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Match:  &TriggerMatchSpec{Types: []string{"webhook.received"}},
		Action: TriggerActionSpec{
			Type:          "message",
			AgentID:       f.targetAgent.ID,
			Message:       "PR #${event.attributes.number} on ${trigger.name}: ${event.payload.title}",
			UntrustedVars: []string{"event.attributes.number", "event.payload.title"},
		},
	})
	require.Equal(t, http.StatusCreated, okTrigRec.Code, okTrigRec.Body.String())
	var okTrig TriggerResponse
	require.NoError(t, json.Unmarshal(okTrigRec.Body.Bytes(), &okTrig))

	badTrigRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "msg-tmpl-missing",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Match:  &TriggerMatchSpec{Types: []string{"webhook.received"}},
		Action: TriggerActionSpec{
			Type:          "message",
			AgentID:       f.targetAgent.ID,
			Message:       "Missing ${event.payload.does_not_exist}",
			UntrustedVars: []string{"event.payload.does_not_exist"},
		},
	})
	require.Equal(t, http.StatusCreated, badTrigRec.Code, badTrigRec.Body.String())
	var badTrig TriggerResponse
	require.NoError(t, json.Unmarshal(badTrigRec.Body.Bytes(), &badTrig))

	// Capture slog output to assert render refusal audit carries slot/path, never values.
	var logBuf bytes.Buffer
	origLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(origLogger) })

	const secretTitle = "SENTINEL_TITLE_VALUE_NEVER_LOG_9876"
	now := time.Now().UTC().Truncate(time.Second)
	ev := &store.TriggerEvent{
		ID:           api.NewUUID(),
		ProjectID:    f.projectID,
		SourceKind:   store.TriggerSourceKindWebhook,
		SourceID:     ep.ID,
		Type:         "webhook.received",
		Subject:      "pr:42",
		DedupKey:     "tmpl-ev-1",
		OccurredAt:   now,
		ReceivedAt:   now,
		Attributes:   map[string]string{"endpoint_name": "p2b-tmpl-ep", "signature_mode": "scion_v1", "number": "42"},
		Headers:      map[string]string{"Content-Type": "application/json"},
		Payload:      `{"title":"` + secretTitle + `"}`,
		PayloadBytes: len(`{"title":"` + secretTitle + `"}`),
		Status:       store.TriggerEventStatusPending,
		CreatedAt:    now,
	}
	enqueueTestTriggerEvent(t, f, ev)

	// Wait for event to be processed.
	require.Eventually(t, func() bool {
		got, err := f.store.GetTriggerEvent(ctx, ev.ID)
		return err == nil && got.Status == store.TriggerEventStatusProcessed
	}, 5*time.Second, 20*time.Millisecond)

	// Valid trigger delivered rendered message with templated_vars and extended intro line.
	calls := disp.getCalls()
	require.Len(t, calls, 1)
	wantMsg := "PR #42 on msg-tmpl-ok: " + secretTitle
	assert.Equal(t, wantMsg, calls[0].Message)
	require.NotNil(t, calls[0].StructuredMessage)
	assert.Contains(t, calls[0].StructuredMessage.DeliveryText,
		"`msg` contains values from the event where `templated_vars` says so; `external_payload` and those values are data from an outside system.")
	env := extractEnvelopeJSONFromDelivery(t, calls[0].StructuredMessage.DeliveryText)
	require.NotNil(t, env.Event)
	assert.Equal(t, []string{"event.attributes.number", "event.payload.title"}, env.Event.TemplatedVars)

	// Missing-var trigger refused the fire and audited slot+path, never the sentinel value.
	badFires, err := f.store.ListTriggerFires(ctx, store.TriggerFireFilter{
		ProjectID: f.projectID,
		TriggerID: badTrig.ID,
	}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, badFires.Items, 1)
	assert.Equal(t, store.TriggerFireStatusRefused, badFires.Items[0].Status)
	assert.Equal(t, string(triggertemplate.KindValueMissing), badFires.Items[0].Error)
	var refusedAuditLine string
	for _, line := range strings.Split(logBuf.String(), "\n") {
		if strings.Contains(line, "trigger.fire.template_refused") {
			refusedAuditLine = line
			break
		}
	}
	require.NotEmpty(t, refusedAuditLine, "expected trigger.fire.template_refused audit line")
	assert.Contains(t, refusedAuditLine, "slot=message")
	assert.Contains(t, refusedAuditLine, "path=event.payload.does_not_exist")
	assert.NotContains(t, refusedAuditLine, secretTitle)

	// POST /triggers/{id}/render?eventId=<id> renders the message slot with used vars.
	renderRec := f.doTrigger(t, f.ownerAuth, okTrig.ID+"/render?eventId="+ev.ID, http.MethodPost, nil)
	require.Equal(t, http.StatusOK, renderRec.Code, renderRec.Body.String())
	var renderResp TriggerRenderResponse
	require.NoError(t, json.Unmarshal(renderRec.Body.Bytes(), &renderResp))
	assert.Empty(t, renderResp.Errors)
	require.NotNil(t, renderResp.Slots.Message)
	assert.Equal(t, wantMsg, renderResp.Slots.Message.Rendered)
	assert.NotContains(t, renderRec.Body.String(), "not_supported_until_P2b")
	assert.Len(t, renderResp.Slots.Message.Used, 3)
}

func TestTriggerMessageP2b_OfflineQueueDeliveryNudgeExpiryAndGuards(t *testing.T) {
	f := setupTriggersTestFixture(t)
	enableWriteDenySwitch(t, f.srv)
	ctx := context.Background()

	disp := &recordingDispatcher{}
	f.srv.SetDispatcher(disp)

	dqStore, ok := f.store.(store.DurableQueueStore)
	require.True(t, ok)
	q := f.srv.TriggerEventQueue()
	require.NotNil(t, q)

	// Create webhook endpoint.
	epRec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{
		Name: "p2b-offline-ep",
	})
	require.Equal(t, http.StatusCreated, epRec.Code, epRec.Body.String())
	var ep WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(epRec.Body.Bytes(), &ep))

	// Create offline-queued message trigger with template.
	trRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "msg-offline-queue",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Match:  &TriggerMatchSpec{Types: []string{"webhook.received"}},
		Action: TriggerActionSpec{
			Type:            "message",
			AgentID:         f.targetAgent.ID,
			Message:         "Queued PR #${event.attributes.number}",
			Offline:         MessageOfflineQueue,
			QueueTTLSeconds: 120,
			UntrustedVars:   []string{"event.attributes.number"},
		},
	})
	require.Equal(t, http.StatusCreated, trRec.Code, trRec.Body.String())
	var tr TriggerResponse
	require.NoError(t, json.Unmarshal(trRec.Body.Bytes(), &tr))

	setAgentPhase := func(phase state.Phase) {
		ag, err := f.store.GetAgent(ctx, f.targetAgent.ID)
		require.NoError(t, err)
		ag.Phase = string(phase)
		ag.Activity = ""
		if phase == state.PhaseRunning {
			ag.Activity = string(state.ActivityIdle)
		}
		require.NoError(t, f.store.UpdateAgent(ctx, ag))
	}

	// Start fast-polling event drainer, retry drainer, and agent-status nudger.
	dCtx, dCancel := context.WithCancel(ctx)
	t.Cleanup(dCancel)

	tDisp, err := f.srv.newTriggerDispatcher(q)
	require.NoError(t, err)
	fastOpts := trigger.DrainerOptions{
		QueueStore:   dqStore,
		Queue:        q,
		Owner:        "test-hub-1",
		PollInterval: 30 * time.Millisecond,
	}
	evDrainer, err := tDisp.NewDrainer(fastOpts)
	require.NoError(t, err)
	retryDrainer1, err := tDisp.NewFireRetryDrainer(fastOpts)
	require.NoError(t, err)
	// Second replica retry drainer to prove two replicas nudging deliver exactly once.
	fastOpts2 := fastOpts
	fastOpts2.Owner = "test-hub-2"
	retryDrainer2, err := tDisp.NewFireRetryDrainer(fastOpts2)
	require.NoError(t, err)

	var wg sync.WaitGroup
	wg.Add(4)
	go func() { defer wg.Done(); evDrainer.Run(dCtx) }()
	go func() { defer wg.Done(); retryDrainer1.Run(dCtx) }()
	go func() { defer wg.Done(); retryDrainer2.Run(dCtx) }()
	go func() { defer wg.Done(); f.srv.runTriggerAgentStatusNudger(dCtx, q) }()
	t.Cleanup(func() {
		dCancel()
		wg.Wait()
	})

	// 1. Suspended target -> waiting -> resume -> nudged & delivered once across two replicas!
	setAgentPhase(state.PhaseSuspended)
	now := time.Now().UTC().Truncate(time.Second)
	ev1 := &store.TriggerEvent{
		ID:         api.NewUUID(),
		ProjectID:  f.projectID,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   ep.ID,
		Type:       "webhook.received",
		Subject:    "pr:101",
		DedupKey:   "offline-ev-1",
		OccurredAt: now,
		ReceivedAt: now,
		Attributes: map[string]string{"endpoint_name": "p2b-offline-ep", "signature_mode": "scion_v1", "number": "101"},
		Status:     store.TriggerEventStatusPending,
		CreatedAt:  now,
	}
	enqueueTestTriggerEvent(t, f, ev1)

	var waitingFire store.TriggerFire
	require.Eventually(t, func() bool {
		fires, lErr := f.store.ListTriggerFires(ctx, store.TriggerFireFilter{
			ProjectID: f.projectID,
			TriggerID: tr.ID,
			EventID:   ev1.ID,
		}, store.ListOptions{Limit: 5})
		if lErr != nil || len(fires.Items) != 1 {
			return false
		}
		waitingFire = fires.Items[0]
		return waitingFire.Status == store.TriggerFireStatusWaiting
	}, 5*time.Second, 20*time.Millisecond)

	assert.Equal(t, 1, waitingFire.Attempts)
	assert.Equal(t, f.targetAgent.ID, waitingFire.TargetAgentID)
	require.NotNil(t, waitingFire.WaitUntil)
	assert.Empty(t, disp.getCalls())

	// Trigger counters must not count waiting.
	loadedTr, err := f.store.GetTrigger(ctx, tr.ID)
	require.NoError(t, err)
	assert.Equal(t, 0, loadedTr.FireCount)
	assert.Equal(t, 0, loadedTr.ErrorCount)

	// Transition agent to running and publish agent status (plus concurrent second nudge).
	setAgentPhase(state.PhaseRunning)
	agRunning, err := f.store.GetAgent(ctx, f.targetAgent.ID)
	require.NoError(t, err)
	f.srv.events.PublishAgentStatus(ctx, agRunning)
	// Simulate second replica nudging concurrently.
	f.srv.nudgeWaitingFiresForAgent(ctx, q, f.targetAgent.ID)

	// Delivered within one poll interval, and EXACTLY once!
	require.Eventually(t, func() bool {
		got, gErr := f.store.GetTriggerFire(ctx, waitingFire.ID)
		return gErr == nil && got.Status == store.TriggerFireStatusFired
	}, 3*time.Second, 20*time.Millisecond)

	calls := disp.getCalls()
	require.Len(t, calls, 1, "two replicas nudging must deliver exactly once")
	assert.Equal(t, "Queued PR #101", calls[0].Message)

	// 2. Target in error phase -> immediately skipped (never waiting).
	setAgentPhase(state.PhaseError)
	evErr := &store.TriggerEvent{
		ID:         api.NewUUID(),
		ProjectID:  f.projectID,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   ep.ID,
		Type:       "webhook.received",
		Subject:    "pr:102",
		DedupKey:   "offline-ev-error",
		OccurredAt: now,
		ReceivedAt: now,
		Attributes: map[string]string{"endpoint_name": "p2b-offline-ep", "signature_mode": "scion_v1", "number": "102"},
		Status:     store.TriggerEventStatusPending,
		CreatedAt:  now,
	}
	enqueueTestTriggerEvent(t, f, evErr)
	require.Eventually(t, func() bool {
		fires, lErr := f.store.ListTriggerFires(ctx, store.TriggerFireFilter{
			ProjectID: f.projectID,
			TriggerID: tr.ID,
			EventID:   evErr.ID,
		}, store.ListOptions{Limit: 5})
		return lErr == nil && len(fires.Items) == 1 && fires.Items[0].Status == store.TriggerFireStatusSkipped
	}, 5*time.Second, 20*time.Millisecond)

	// 3. Target suspended -> waiting -> trigger paused -> nudged -> skipped "trigger changed".
	setAgentPhase(state.PhaseSuspended)
	evPause := &store.TriggerEvent{
		ID:         api.NewUUID(),
		ProjectID:  f.projectID,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   ep.ID,
		Type:       "webhook.received",
		Subject:    "pr:103",
		DedupKey:   "offline-ev-pause",
		OccurredAt: now,
		ReceivedAt: now,
		Attributes: map[string]string{"endpoint_name": "p2b-offline-ep", "signature_mode": "scion_v1", "number": "103"},
		Status:     store.TriggerEventStatusPending,
		CreatedAt:  now,
	}
	enqueueTestTriggerEvent(t, f, evPause)
	var pauseFireID string
	require.Eventually(t, func() bool {
		fires, lErr := f.store.ListTriggerFires(ctx, store.TriggerFireFilter{
			ProjectID: f.projectID,
			TriggerID: tr.ID,
			EventID:   evPause.ID,
		}, store.ListOptions{Limit: 5})
		if lErr == nil && len(fires.Items) == 1 && fires.Items[0].Status == store.TriggerFireStatusWaiting {
			pauseFireID = fires.Items[0].ID
			return true
		}
		return false
	}, 5*time.Second, 20*time.Millisecond)

	// Pause trigger, then nudge.
	pauseRec := f.doTrigger(t, f.ownerAuth, tr.ID+"/pause", http.MethodPost, nil)
	require.Equal(t, http.StatusOK, pauseRec.Code, pauseRec.Body.String())
	f.srv.nudgeWaitingFiresForAgent(ctx, q, f.targetAgent.ID)

	require.Eventually(t, func() bool {
		got, gErr := f.store.GetTriggerFire(ctx, pauseFireID)
		return gErr == nil && got.Status == store.TriggerFireStatusSkipped && got.Error == trigger.ErrFireTriggerChanged
	}, 3*time.Second, 20*time.Millisecond)

	// 4. Resume trigger, verify TTL expiry -> expired and counted as error.
	resumeRec := f.doTrigger(t, f.ownerAuth, tr.ID+"/resume", http.MethodPost, nil)
	require.Equal(t, http.StatusOK, resumeRec.Code, resumeRec.Body.String())

	pastReceived := time.Now().UTC().Add(-5 * time.Minute)
	evExp := &store.TriggerEvent{
		ID:         api.NewUUID(),
		ProjectID:  f.projectID,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   ep.ID,
		Type:       "webhook.received",
		Subject:    "pr:104",
		DedupKey:   "offline-ev-exp",
		OccurredAt: pastReceived,
		ReceivedAt: pastReceived, // wait_until = pastReceived + 120s is already in the past!
		Attributes: map[string]string{"endpoint_name": "p2b-offline-ep", "signature_mode": "scion_v1", "number": "104"},
		Status:     store.TriggerEventStatusPending,
		CreatedAt:  pastReceived,
	}
	enqueueTestTriggerEvent(t, f, evExp)

	require.Eventually(t, func() bool {
		fires, lErr := f.store.ListTriggerFires(ctx, store.TriggerFireFilter{
			ProjectID: f.projectID,
			TriggerID: tr.ID,
			EventID:   evExp.ID,
		}, store.ListOptions{Limit: 5})
		return lErr == nil && len(fires.Items) == 1 && fires.Items[0].Status == store.TriggerFireStatusExpired
	}, 5*time.Second, 20*time.Millisecond)

	loadedTr, err = f.store.GetTrigger(ctx, tr.ID)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, loadedTr.ErrorCount, 1, "expired fire must increment trigger ErrorCount")
}

func TestTriggerMessageP2b_NudgeWhileLeasedRealQueue(t *testing.T) {
	f := setupTriggersTestFixture(t)
	enableWriteDenySwitch(t, f.srv)
	ctx := context.Background()

	disp := &recordingDispatcher{}
	f.srv.SetDispatcher(disp)

	dqStore, ok := f.store.(store.DurableQueueStore)
	require.True(t, ok)
	q := f.srv.TriggerEventQueue()
	require.NotNil(t, q)

	epRec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{
		Name: "p2b-lease-ep",
	})
	require.Equal(t, http.StatusCreated, epRec.Code, epRec.Body.String())
	var ep WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(epRec.Body.Bytes(), &ep))

	trRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "msg-nudge-while-leased",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Match:  &TriggerMatchSpec{Types: []string{"webhook.received"}},
		Action: TriggerActionSpec{
			Type:            "message",
			AgentID:         f.targetAgent.ID,
			Message:         "Leased race message",
			Offline:         MessageOfflineQueue,
			QueueTTLSeconds: 300,
		},
	})
	require.Equal(t, http.StatusCreated, trRec.Code, trRec.Body.String())
	var tr TriggerResponse
	require.NoError(t, json.Unmarshal(trRec.Body.Bytes(), &tr))

	// Put target in suspended state.
	ag, err := f.store.GetAgent(ctx, f.targetAgent.ID)
	require.NoError(t, err)
	ag.Phase = string(state.PhaseSuspended)
	ag.Activity = ""
	require.NoError(t, f.store.UpdateAgent(ctx, ag))

	// Create a waiting fire + queue item directly due now.
	now := time.Now().UTC().Truncate(time.Second)
	ev := &store.TriggerEvent{
		ID:         api.NewUUID(),
		ProjectID:  f.projectID,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   ep.ID,
		Type:       "webhook.received",
		Subject:    "s1",
		DedupKey:   "lease-race-ev",
		OccurredAt: now,
		ReceivedAt: now,
		Attributes: map[string]string{"endpoint_name": "p2b-lease-ep", "signature_mode": "scion_v1"},
		Status:     store.TriggerEventStatusProcessed,
		CreatedAt:  now,
	}
	require.NoError(t, f.store.CreateTriggerEvent(ctx, ev))

	storedTr, err := f.store.GetTrigger(ctx, tr.ID)
	require.NoError(t, err)
	waitUntil := now.Add(5 * time.Minute)
	fire := &store.TriggerFire{
		ID:                   api.NewUUID(),
		ProjectID:            f.projectID,
		TriggerID:            tr.ID,
		EventID:              ev.ID,
		Subject:              "s1",
		ActionType:           store.TriggerActionMessage,
		Status:               store.TriggerFireStatusWaiting,
		TargetAgentID:        f.targetAgent.ID,
		WaitUntil:            &waitUntil,
		Attempts:             1,
		LeaseUntil:           now.Add(10 * time.Minute),
		StartedAt:            now,
		CreatedAt:            now,
		InitiatorAttribution: storedTr.InitiatorAttribution,
	}
	claimed, _, err := f.store.ClaimTriggerFire(ctx, fire)
	require.NoError(t, err)
	require.True(t, claimed)

	qp, _ := json.Marshal(trigger.FireRetryQueuePayload{Version: trigger.FireRetryQueuePayloadVersion, FireID: fire.ID})
	_, err = q.Enqueue(ctx, nil, durablequeue.Item{
		Kind:      trigger.FireRetryQueueKind,
		DedupKey:  trigger.FireRetryDedupPrefix + fire.ID,
		ProjectID: f.projectID,
		Payload:   qp,
		NotBefore: now.Add(-time.Second),
	})
	require.NoError(t, err)

	// Wrap serverTriggerCore so the first Fire pauses while holding the lease,
	// letting us transition the target to running and call q.Nudge (confirming
	// Nudge returns n=0 because the item is leased!).
	enteredFire := make(chan struct{})
	releaseFire := make(chan struct{})
	var firstFireOnce sync.Once
	wrappedCore := &hookedTriggerCore{
		inner: serverTriggerCore{srv: f.srv},
		onFire: func(ctx context.Context, req hubfire.FireRequest, inner func(context.Context, hubfire.FireRequest) hubfire.FireOutcome) hubfire.FireOutcome {
			out := inner(ctx, req)
			firstFireOnce.Do(func() {
				close(enteredFire)
				<-releaseFire
			})
			return out
		},
	}

	tDisp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Store: f.store,
		Queue: q,
		Core:  wrappedCore,
	})
	require.NoError(t, err)
	rd, err := tDisp.NewFireRetryDrainer(trigger.DrainerOptions{
		QueueStore:   dqStore,
		Queue:        q,
		Owner:        "lease-test-hub",
		PollInterval: 30 * time.Millisecond,
	})
	require.NoError(t, err)

	dCtx, dCancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		rd.Run(dCtx)
	}()
	t.Cleanup(func() {
		dCancel()
		<-done
	})

	// Wait until the first retry attempt holds the queue lease and has returned waiting.
	select {
	case <-enteredFire:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for retry handler to enter Fire")
	}

	// While leased: transition target to running and nudge!
	ag, err = f.store.GetAgent(ctx, f.targetAgent.ID)
	require.NoError(t, err)
	ag.Phase = string(state.PhaseRunning)
	ag.Activity = string(state.ActivityIdle)
	require.NoError(t, f.store.UpdateAgent(ctx, ag))

	nudged, err := q.Nudge(ctx, trigger.FireRetryQueueKind, []string{trigger.FireRetryDedupPrefix + fire.ID})
	require.NoError(t, err)
	assert.Equal(t, 0, nudged, "nudge while item is leased must update 0 rows")

	// Release the first attempt: its post-Fire CheckRetryTarget sees running
	// and schedules an immediate retry instead of the 60s backoff!
	close(releaseFire)

	require.Eventually(t, func() bool {
		got, gErr := f.store.GetTriggerFire(ctx, fire.ID)
		return gErr == nil && got.Status == store.TriggerFireStatusFired
	}, 3*time.Second, 20*time.Millisecond, "fire must deliver immediately without waiting for 60s backoff")
	require.Len(t, disp.getCalls(), 1)
	assert.Equal(t, "Leased race message", disp.getCalls()[0].Message)
}

type hookedTriggerCore struct {
	inner  serverTriggerCore
	onFire func(ctx context.Context, req hubfire.FireRequest, inner func(context.Context, hubfire.FireRequest) hubfire.FireOutcome) hubfire.FireOutcome
}

func (h *hookedTriggerCore) Fire(ctx context.Context, req hubfire.FireRequest) hubfire.FireOutcome {
	return h.onFire(ctx, req, h.inner.Fire)
}

func (h *hookedTriggerCore) Experiment(name string) bool {
	return h.inner.Experiment(name)
}

func (h *hookedTriggerCore) CheckRetryTarget(ctx context.Context, agentID string) (trigger.RetryTargetState, error) {
	return h.inner.CheckRetryTarget(ctx, agentID)
}
