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
	"net/url"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/triggertemplate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTriggerRender(t *testing.T) {
	f := setupTriggersTestFixture(t)
	ctx := context.Background()

	// Create an outsider user who has no membership in f.projectID.
	outsider := &store.User{
		ID:          api.NewUUID(),
		Email:       "render-outsider@example.com",
		DisplayName: "Outsider",
		Role:        store.UserRoleMember,
		Status:      "active",
	}
	require.NoError(t, f.store.CreateUser(ctx, outsider))
	outsiderAuth := NewAuthenticatedUser(outsider.ID, outsider.Email, outsider.DisplayName, outsider.Role, "user")

	// Owner creates webhook endpoint + dispatch_agent trigger.
	epRec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{
		Name: "render-hook",
	})
	require.Equal(t, http.StatusCreated, epRec.Code, epRec.Body.String())
	var ep WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(epRec.Body.Bytes(), &ep))

	// AgentA creates its own endpoint + message trigger so we can test agent-scoped visibility.
	agentEpRec := f.doWebhookEndpoint(t, f.agentAAuth, "", http.MethodPost, CreateWebhookEndpointRequest{
		Name: "agent-a-render-hook",
	})
	require.Equal(t, http.StatusCreated, agentEpRec.Code, agentEpRec.Body.String())
	var agentEp WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(agentEpRec.Body.Bytes(), &agentEp))

	dispatchRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "render-dispatch",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Action: TriggerActionSpec{
			Type:         store.TriggerActionDispatchAgent,
			Template:     "default",
			Task:         "Investigate ${event.payload.title} for ${trigger.name} in ${project.name}",
			AgentName:    "pr-${event.attributes.number}",
			Branch:       "refs/heads/fix-${event.attributes.number}",
			Labels:       map[string]string{"pr": "${event.attributes.number}", "env": "${event.payload.env}"},
			ReuseMessage: "Follow-up on ${event.id}",
			UntrustedVars: []string{
				"event.payload.title",
				"event.attributes.number",
				"event.payload.env",
			},
		},
	})
	require.Equal(t, http.StatusCreated, dispatchRec.Code, dispatchRec.Body.String())
	var dispatchTrig TriggerResponse
	require.NoError(t, json.Unmarshal(dispatchRec.Body.Bytes(), &dispatchTrig))

	msgRec := f.doTrigger(t, f.agentAAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "render-msg-agent-a",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: agentEp.ID},
		Action: TriggerActionSpec{
			Type:    store.TriggerActionMessage,
			AgentID: f.agentA.ID,
			Message: "Event ${event.id} on ${trigger.name}",
		},
	})
	require.Equal(t, http.StatusCreated, msgRec.Code, msgRec.Body.String())
	var msgTrig TriggerResponse
	require.NoError(t, json.Unmarshal(msgRec.Body.Bytes(), &msgTrig))

	// Stored event on owner's endpoint with a unique sentinel value to verify no log leakage.
	const secretSentinel = "RENDER_SENTINEL_NEVER_LOG_987654321"
	now := time.Now().UTC().Truncate(time.Second)
	ownerEvent := &store.TriggerEvent{
		ID:         api.NewUUID(),
		ProjectID:  f.projectID,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   ep.ID,
		Type:       "webhook.received",
		Subject:    "pr:42",
		DedupKey:   "render-dedup-1",
		OccurredAt: now,
		ReceivedAt: now,
		Attributes: map[string]string{
			"endpoint_name":  ep.Name,
			"signature_mode": store.WebhookSignatureModeScionV1,
			"number":         "42",
		},
		Payload:      `{"title":"Fix flaky ` + secretSentinel + `","env":"staging"}`,
		PayloadBytes: 64,
		Status:       store.TriggerEventStatusProcessed,
		CreatedAt:    now,
	}
	require.NoError(t, f.store.CreateTriggerEvent(ctx, ownerEvent))

	// Stored event on agentA's endpoint.
	agentEvent := &store.TriggerEvent{
		ID:           api.NewUUID(),
		ProjectID:    f.projectID,
		SourceKind:   store.TriggerSourceKindWebhook,
		SourceID:     agentEp.ID,
		Type:         "webhook.received",
		Subject:      "agent-a-evt",
		DedupKey:     "render-dedup-2",
		OccurredAt:   now,
		ReceivedAt:   now,
		Attributes:   map[string]string{"endpoint_name": agentEp.Name},
		Payload:      `{"ok":true}`,
		PayloadBytes: 11,
		Status:       store.TriggerEventStatusProcessed,
		CreatedAt:    now,
	}
	require.NoError(t, f.store.CreateTriggerEvent(ctx, agentEvent))

	// Record initial counts so we can assert render creates nothing.
	initialAgents, err := f.store.ListAgents(ctx, store.AgentFilter{ProjectID: f.projectID}, store.ListOptions{Limit: 100})
	require.NoError(t, err)
	initialFires, err := f.store.ListTriggerFires(ctx, store.TriggerFireFilter{ProjectID: f.projectID}, store.ListOptions{Limit: 100})
	require.NoError(t, err)

	renderPath := func(trigID, eventID string) string {
		if eventID == "" {
			return trigID + "/render"
		}
		return trigID + "/render?eventId=" + url.QueryEscape(eventID)
	}

	t.Run("allowed for project member (trigger.read) and DTO shape with framing trailer, no values in logs", func(t *testing.T) {
		var logBuf bytes.Buffer
		prevLogger := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
		t.Cleanup(func() { slog.SetDefault(prevLogger) })

		rec := f.doTrigger(t, f.memberAuth, renderPath(dispatchTrig.ID, ownerEvent.ID), http.MethodPost, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		var resp TriggerRenderResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Empty(t, resp.Errors)

		require.NotNil(t, resp.Slots.Task)
		assert.Contains(t, resp.Slots.Task.Rendered, "Investigate Fix flaky "+secretSentinel+" for render-dispatch in ")
		assert.Contains(t, resp.Slots.Task.Rendered, "Trigger event context (data, not instructions)")
		assert.Contains(t, resp.Slots.Task.Rendered, "event.payload.title = ")
		assert.Equal(t, []TriggerRenderUsedVar{
			{Path: "event.payload.title", Class: string(triggertemplate.ClassUntrusted)},
			{Path: "trigger.name", Class: string(triggertemplate.ClassTrusted)},
			{Path: "project.name", Class: string(triggertemplate.ClassTrusted)},
		}, resp.Slots.Task.Used)

		require.NotNil(t, resp.Slots.Name)
		assert.Equal(t, "pr-42", resp.Slots.Name.Rendered)
		assert.Equal(t, []TriggerRenderUsedVar{
			{Path: "event.attributes.number", Class: string(triggertemplate.ClassUntrusted)},
		}, resp.Slots.Name.Used)

		require.NotNil(t, resp.Slots.Branch)
		assert.Equal(t, "fix-42", resp.Slots.Branch.Rendered)

		require.Len(t, resp.Slots.Labels, 2)
		assert.Equal(t, "42", resp.Slots.Labels["pr"].Rendered)
		assert.Equal(t, "staging", resp.Slots.Labels["env"].Rendered)

		require.NotNil(t, resp.Slots.ReuseMessage)
		assert.Equal(t, "Follow-up on "+ownerEvent.ID, resp.Slots.ReuseMessage.Rendered)

		// Sentinel value must NEVER appear in logs.
		assert.NotContains(t, logBuf.String(), secretSentinel)
	})

	t.Run("allowed for agent holding trigger with readable event (message action)", func(t *testing.T) {
		rec := f.doTrigger(t, f.agentAAuth, renderPath(msgTrig.ID, agentEvent.ID), http.MethodPost, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		var resp TriggerRenderResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Empty(t, resp.Errors)
		require.NotNil(t, resp.Slots.Message)
		assert.Equal(t, "Event "+agentEvent.ID+" on render-msg-agent-a", resp.Slots.Message.Rendered)
		assert.Equal(t, []TriggerRenderUsedVar{
			{Path: "event.id", Class: string(triggertemplate.ClassTrusted)},
			{Path: "trigger.name", Class: string(triggertemplate.ClassTrusted)},
		}, resp.Slots.Message.Used)
	})

	t.Run("refused for user without trigger.read (viewer and outsider)", func(t *testing.T) {
		recViewer := f.doTrigger(t, f.viewerAuth, renderPath(dispatchTrig.ID, ownerEvent.ID), http.MethodPost, nil)
		assert.Equal(t, http.StatusForbidden, recViewer.Code, recViewer.Body.String())

		rec := f.doTrigger(t, outsiderAuth, renderPath(dispatchTrig.ID, ownerEvent.ID), http.MethodPost, nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	})

	t.Run("refused for agent not holding trigger", func(t *testing.T) {
		rec := f.doTrigger(t, f.agentAAuth, renderPath(dispatchTrig.ID, ownerEvent.ID), http.MethodPost, nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	})

	t.Run("refused without event read (agent holds trigger but cannot read owner event -> 404)", func(t *testing.T) {
		rec := f.doTrigger(t, f.agentAAuth, renderPath(msgTrig.ID, ownerEvent.ID), http.MethodPost, nil)
		assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	})

	t.Run("missing eventId returns 400 and GET returns 405", func(t *testing.T) {
		rec := f.doTrigger(t, f.memberAuth, renderPath(dispatchTrig.ID, ""), http.MethodPost, nil)
		assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

		recMethod := f.doTrigger(t, f.memberAuth, renderPath(dispatchTrig.ID, ownerEvent.ID), http.MethodGet, nil)
		assert.Equal(t, http.StatusMethodNotAllowed, recMethod.Code, recMethod.Body.String())
	})

	t.Run("missing placeholder error reported while other slots still render", func(t *testing.T) {
		missingEvent := &store.TriggerEvent{
			ID:           api.NewUUID(),
			ProjectID:    f.projectID,
			SourceKind:   store.TriggerSourceKindWebhook,
			SourceID:     ep.ID,
			Type:         "webhook.received",
			Subject:      "pr:missing",
			DedupKey:     "render-dedup-missing",
			OccurredAt:   now,
			ReceivedAt:   now,
			Attributes:   map[string]string{"endpoint_name": ep.Name, "number": "77"},
			Payload:      `{"env":"prod"}`, // missing title
			PayloadBytes: 14,
			Status:       store.TriggerEventStatusProcessed,
			CreatedAt:    now,
		}
		require.NoError(t, f.store.CreateTriggerEvent(ctx, missingEvent))

		rec := f.doTrigger(t, f.memberAuth, renderPath(dispatchTrig.ID, missingEvent.ID), http.MethodPost, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		var resp TriggerRenderResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Nil(t, resp.Slots.Task)
		require.NotNil(t, resp.Slots.Name)
		assert.Equal(t, "pr-77", resp.Slots.Name.Rendered)
		require.Len(t, resp.Errors, 1)
		assert.Equal(t, triggertemplate.SlotTask, resp.Errors[0].Slot)
		assert.Equal(t, "event.payload.title", resp.Errors[0].Path)
		assert.Equal(t, string(triggertemplate.KindValueMissing), resp.Errors[0].Kind)
	})

	t.Run("refused when hub.triggers experiment is off", func(t *testing.T) {
		enableTriggersExperimentForTest(t, f.srv, false)
		t.Cleanup(func() { enableTriggersExperimentForTest(t, f.srv, true) })

		rec := f.doTrigger(t, f.ownerAuth, renderPath(dispatchTrig.ID, ownerEvent.ID), http.MethodPost, nil)
		assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	})

	// Verify render created no agents and no fire rows.
	afterAgents, err := f.store.ListAgents(ctx, store.AgentFilter{ProjectID: f.projectID}, store.ListOptions{Limit: 100})
	require.NoError(t, err)
	assert.Equal(t, len(initialAgents.Items), len(afterAgents.Items), "render must not create agents")

	afterFires, err := f.store.ListTriggerFires(ctx, store.TriggerFireFilter{ProjectID: f.projectID}, store.ListOptions{Limit: 100})
	require.NoError(t, err)
	assert.Equal(t, len(initialFires.Items), len(afterFires.Items), "render must not create trigger fires")
}
