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
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func (f *triggersTestFixture) doTriggerEvents(t *testing.T, identity Identity, subPath, method string, body any) *httptest.ResponseRecorder {
	t.Helper()
	routeSubPath, query, hasQuery := strings.Cut(subPath, "?")
	path := "/api/v1/projects/" + f.projectID + "/trigger-events"
	if routeSubPath != "" {
		path += "/" + routeSubPath
	}
	if hasQuery {
		path += "?" + query
	}
	req := authoredRequest(t, identity, method, path, body)
	rec := httptest.NewRecorder()
	f.srv.handleTriggerEvents(rec, req, f.projectID, routeSubPath)
	return rec
}

func (f *triggersTestFixture) doTriggerFires(t *testing.T, identity Identity, subPath, method string, body any) *httptest.ResponseRecorder {
	t.Helper()
	routeSubPath, query, hasQuery := strings.Cut(subPath, "?")
	path := "/api/v1/projects/" + f.projectID + "/trigger-fires"
	if routeSubPath != "" {
		path += "/" + routeSubPath
	}
	if hasQuery {
		path += "?" + query
	}
	req := authoredRequest(t, identity, method, path, body)
	rec := httptest.NewRecorder()
	f.srv.handleTriggerFires(rec, req, f.projectID, routeSubPath)
	return rec
}

func TestTriggerEventsExperimentOff_Returns404(t *testing.T) {
	f := setupTriggersTestFixture(t)
	enableTriggersExperimentForTest(t, f.srv, false)

	for _, tc := range []struct {
		name string
		run  func() *httptest.ResponseRecorder
	}{
		{"GET /triggers/{id}/history", func() *httptest.ResponseRecorder {
			return f.doTrigger(t, f.ownerAuth, "some-id/history", http.MethodGet, nil)
		}},
		{"GET /trigger-events", func() *httptest.ResponseRecorder {
			return f.doTriggerEvents(t, f.ownerAuth, "", http.MethodGet, nil)
		}},
		{"GET /trigger-events/{id}", func() *httptest.ResponseRecorder {
			return f.doTriggerEvents(t, f.ownerAuth, "some-id", http.MethodGet, nil)
		}},
		{"GET /trigger-fires", func() *httptest.ResponseRecorder {
			return f.doTriggerFires(t, f.ownerAuth, "", http.MethodGet, nil)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := tc.run()
			assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
		})
	}
}

func TestTriggerHistoryAndEvents_PaginationFilteringAndSerializers(t *testing.T) {
	f := setupTriggersTestFixture(t)
	ctx := context.Background()

	epRec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{Name: "deploy-ep"})
	require.Equal(t, http.StatusCreated, epRec.Code, epRec.Body.String())
	var ep WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(epRec.Body.Bytes(), &ep))

	otherEpRec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{Name: "other-ep"})
	require.Equal(t, http.StatusCreated, otherEpRec.Code, otherEpRec.Body.String())
	var otherEp WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(otherEpRec.Body.Bytes(), &otherEp))

	trRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "deploy-trigger",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "deployed"},
	})
	require.Equal(t, http.StatusCreated, trRec.Code, trRec.Body.String())
	var tr TriggerResponse
	require.NoError(t, json.Unmarshal(trRec.Body.Bytes(), &tr))

	baseTime := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	ev1 := &store.TriggerEvent{
		ID:           api.NewUUID(),
		ProjectID:    f.projectID,
		SourceKind:   store.TriggerSourceKindWebhook,
		SourceID:     ep.ID,
		Type:         "webhook.received",
		Subject:      "webhook:" + ep.ID,
		DedupKey:     "deliv-1",
		OccurredAt:   baseTime,
		ReceivedAt:   baseTime,
		Attributes:   map[string]string{"endpoint_name": "deploy-ep", "signature_mode": "scion_v1"},
		Headers:      map[string]string{"Content-Type": "application/json", "X-Scion-Delivery": "deliv-1"},
		Payload:      `{"service":"payments","ok":true}`,
		PayloadBytes: len(`{"service":"payments","ok":true}`),
		Status:       store.TriggerEventStatusProcessed,
		MatchCount:   1,
		FireCount:    1,
		ProcessedAt:  triggerTestTimePtr(baseTime.Add(time.Second)),
		CreatedAt:    baseTime,
	}
	ev2 := &store.TriggerEvent{
		ID:           api.NewUUID(),
		ProjectID:    f.projectID,
		SourceKind:   store.TriggerSourceKindWebhook,
		SourceID:     ep.ID,
		Type:         "webhook.deploy.failed",
		Subject:      "webhook:" + ep.ID + "/payments",
		DedupKey:     "deliv-2",
		OccurredAt:   baseTime.Add(5 * time.Minute),
		ReceivedAt:   baseTime.Add(5 * time.Minute),
		Attributes:   map[string]string{"endpoint_name": "deploy-ep"},
		Headers:      map[string]string{"Content-Type": "application/json"},
		Payload:      `{"service":"payments","ok":false}`,
		PayloadBytes: len(`{"service":"payments","ok":false}`),
		Status:       store.TriggerEventStatusFailed,
		Error:        "dropped by queue limits",
		ProcessedAt:  triggerTestTimePtr(baseTime.Add(6 * time.Minute)),
		CreatedAt:    baseTime.Add(5 * time.Minute),
	}
	evOther := &store.TriggerEvent{
		ID:           api.NewUUID(),
		ProjectID:    f.projectID,
		SourceKind:   store.TriggerSourceKindWebhook,
		SourceID:     otherEp.ID,
		Type:         "webhook.received",
		Subject:      "webhook:" + otherEp.ID,
		DedupKey:     "deliv-other",
		OccurredAt:   baseTime.Add(10 * time.Minute),
		ReceivedAt:   baseTime.Add(10 * time.Minute),
		Attributes:   map[string]string{"endpoint_name": "other-ep"},
		Headers:      map[string]string{"Content-Type": "application/json"},
		Payload:      `{"other":1}`,
		PayloadBytes: len(`{"other":1}`),
		Status:       store.TriggerEventStatusPending,
		CreatedAt:    baseTime.Add(10 * time.Minute),
	}
	for _, ev := range []*store.TriggerEvent{ev1, ev2, evOther} {
		require.NoError(t, f.store.CreateTriggerEvent(ctx, ev))
	}

	// Seed two trigger_fires rows on tr for history pagination.
	fire1 := &store.TriggerFire{
		ID:           api.NewUUID(),
		ProjectID:    f.projectID,
		TriggerID:    tr.ID,
		EventID:      ev1.ID,
		Subject:      ev1.Subject,
		ActionType:   store.TriggerActionMessage,
		Status:       store.TriggerFireStatusFired,
		ExecutorKind: "trigger_drainer",
		ExecutorID:   "trigger_event:" + ev1.ID,
		LeaseUntil:   baseTime.Add(10 * time.Minute),
		StartedAt:    baseTime,
		FinishedAt:   triggerTestTimePtr(baseTime.Add(time.Second)),
		CreatedAt:    baseTime,
	}
	fire2 := &store.TriggerFire{
		ID:           api.NewUUID(),
		ProjectID:    f.projectID,
		TriggerID:    tr.ID,
		EventID:      ev2.ID,
		Subject:      ev2.Subject,
		ActionType:   store.TriggerActionMessage,
		Status:       store.TriggerFireStatusRefused,
		Error:        "message refused",
		ExecutorKind: "trigger_drainer",
		ExecutorID:   "trigger_event:" + ev2.ID,
		LeaseUntil:   baseTime.Add(15 * time.Minute),
		StartedAt:    baseTime.Add(5 * time.Minute),
		FinishedAt:   triggerTestTimePtr(baseTime.Add(5*time.Minute + time.Second)),
		CreatedAt:    baseTime.Add(5 * time.Minute),
	}
	for _, fire := range []*store.TriggerFire{fire1, fire2} {
		targetStatus := fire.Status
		targetErr := fire.Error
		finishedAt := *fire.FinishedAt
		fire.Status = store.TriggerFireStatusFiring
		fire.Error = ""
		fire.FinishedAt = nil
		claimed, _, err := f.store.ClaimTriggerFire(ctx, fire)
		require.NoError(t, err)
		require.True(t, claimed)
		transitioned, err := f.store.FinishTriggerFire(ctx, fire.ID, targetStatus, fire.ResultRef, fire.TargetAgentID, targetErr, finishedAt)
		require.NoError(t, err)
		require.True(t, transitioned)
	}

	// 1. GET /triggers/{id}/history keyset pagination.
	histPage1Rec := f.doTrigger(t, f.ownerAuth, tr.ID+"/history?limit=1", http.MethodGet, nil)
	require.Equal(t, http.StatusOK, histPage1Rec.Code, histPage1Rec.Body.String())
	var histPage1 ListTriggerFiresResponse
	require.NoError(t, json.Unmarshal(histPage1Rec.Body.Bytes(), &histPage1))
	assert.Equal(t, 2, histPage1.TotalCount)
	require.Len(t, histPage1.Fires, 1)
	assert.Equal(t, fire2.ID, histPage1.Fires[0].ID)
	assert.Equal(t, store.TriggerFireStatusRefused, histPage1.Fires[0].Status)
	assert.Equal(t, "message refused", histPage1.Fires[0].Error)
	require.NotEmpty(t, histPage1.NextCursor)

	histPage2Rec := f.doTrigger(t, f.ownerAuth, tr.ID+"/history?limit=1&cursor="+url.QueryEscape(histPage1.NextCursor), http.MethodGet, nil)
	require.Equal(t, http.StatusOK, histPage2Rec.Code, histPage2Rec.Body.String())
	var histPage2 ListTriggerFiresResponse
	require.NoError(t, json.Unmarshal(histPage2Rec.Body.Bytes(), &histPage2))
	require.Len(t, histPage2.Fires, 1)
	assert.Equal(t, fire1.ID, histPage2.Fires[0].ID)
	assert.Equal(t, store.TriggerFireStatusFired, histPage2.Fires[0].Status)

	// Method not allowed on history & trigger-events.
	assert.Equal(t, http.StatusMethodNotAllowed, f.doTrigger(t, f.ownerAuth, tr.ID+"/history", http.MethodPost, nil).Code)
	assert.Equal(t, http.StatusMethodNotAllowed, f.doTriggerEvents(t, f.ownerAuth, "", http.MethodPost, nil).Code)
	assert.Equal(t, http.StatusMethodNotAllowed, f.doTriggerEvents(t, f.ownerAuth, ev1.ID, http.MethodDelete, nil).Code)

	// 2. GET /trigger-events omits payload and headers, and filters by sourceId, status, since.
	listAllRec := f.doTriggerEvents(t, f.ownerAuth, "", http.MethodGet, nil)
	require.Equal(t, http.StatusOK, listAllRec.Code, listAllRec.Body.String())
	assert.NotContains(t, listAllRec.Body.String(), `"payload"`, "GET /trigger-events list rows must omit payload")
	assert.NotContains(t, listAllRec.Body.String(), `"headers"`, "GET /trigger-events list rows must omit headers")
	assert.NotContains(t, listAllRec.Body.String(), `"payments"`, "GET /trigger-events must not include body content")

	var listAll ListTriggerEventsResponse
	require.NoError(t, json.Unmarshal(listAllRec.Body.Bytes(), &listAll))
	assert.Equal(t, 3, listAll.TotalCount)
	require.Len(t, listAll.Events, 3)

	// Filter by sourceId + status.
	listFilteredRec := f.doTriggerEvents(t, f.ownerAuth, "?sourceId="+ep.ID+"&status=failed", http.MethodGet, nil)
	require.Equal(t, http.StatusOK, listFilteredRec.Code, listFilteredRec.Body.String())
	var listFiltered ListTriggerEventsResponse
	require.NoError(t, json.Unmarshal(listFilteredRec.Body.Bytes(), &listFiltered))
	require.Len(t, listFiltered.Events, 1)
	assert.Equal(t, ev2.ID, listFiltered.Events[0].ID)

	// Filter by since.
	sinceVal := url.QueryEscape(baseTime.Add(4 * time.Minute).Format(time.RFC3339))
	listSinceRec := f.doTriggerEvents(t, f.ownerAuth, "?since="+sinceVal, http.MethodGet, nil)
	require.Equal(t, http.StatusOK, listSinceRec.Code, listSinceRec.Body.String())
	var listSince ListTriggerEventsResponse
	require.NoError(t, json.Unmarshal(listSinceRec.Body.Bytes(), &listSince))
	require.Len(t, listSince.Events, 2)
	assert.Equal(t, evOther.ID, listSince.Events[0].ID)
	assert.Equal(t, ev2.ID, listSince.Events[1].ID)

	// Invalid status and invalid since -> 400.
	assert.Equal(t, http.StatusBadRequest, f.doTriggerEvents(t, f.ownerAuth, "?status=bogus", http.MethodGet, nil).Code)
	assert.Equal(t, http.StatusBadRequest, f.doTriggerEvents(t, f.ownerAuth, "?since=not-a-timestamp", http.MethodGet, nil).Code)

	// 3. GET /trigger-events/{id} returns full payload and headers.
	detailRec := f.doTriggerEvents(t, f.ownerAuth, ev1.ID, http.MethodGet, nil)
	require.Equal(t, http.StatusOK, detailRec.Code, detailRec.Body.String())
	var detail TriggerEventDetailResponse
	require.NoError(t, json.Unmarshal(detailRec.Body.Bytes(), &detail))
	assert.Equal(t, ev1.ID, detail.ID)
	assert.Equal(t, ep.ID, detail.Source.ID)
	assert.Equal(t, "deliv-1", detail.Headers["X-Scion-Delivery"])
	assert.JSONEq(t, `{"service":"payments","ok":true}`, string(detail.Payload))
}

func TestTriggerEventsAndHistory_AuthzMatrix(t *testing.T) {
	f := setupTriggersTestFixture(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	// Owner creates an endpoint + trigger; Agent A and Agent B each create their own endpoints + triggers.
	ownerEpRec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{Name: "owner-ep"})
	require.Equal(t, http.StatusCreated, ownerEpRec.Code)
	var ownerEp WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(ownerEpRec.Body.Bytes(), &ownerEp))

	ownerTrRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "owner-tr",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ownerEp.ID},
		Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "owner"},
	})
	require.Equal(t, http.StatusCreated, ownerTrRec.Code)
	var ownerTr TriggerResponse
	require.NoError(t, json.Unmarshal(ownerTrRec.Body.Bytes(), &ownerTr))

	epARec := f.doWebhookEndpoint(t, f.agentAAuth, "", http.MethodPost, CreateWebhookEndpointRequest{Name: "agent-a-ep"})
	require.Equal(t, http.StatusCreated, epARec.Code)
	var epA WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(epARec.Body.Bytes(), &epA))

	epADeletedRec := f.doWebhookEndpoint(t, f.agentAAuth, "", http.MethodPost, CreateWebhookEndpointRequest{Name: "agent-a-ep-deleted"})
	require.Equal(t, http.StatusCreated, epADeletedRec.Code)
	var epADeleted WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(epADeletedRec.Body.Bytes(), &epADeleted))

	trARec := f.doTrigger(t, f.agentAAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "agent-a-tr",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: epADeleted.ID},
		Action: TriggerActionSpec{Type: "message", AgentID: f.agentA.ID, Message: "self"},
	})
	require.Equal(t, http.StatusCreated, trARec.Code)
	var trA TriggerResponse
	require.NoError(t, json.Unmarshal(trARec.Body.Bytes(), &trA))

	epBRec := f.doWebhookEndpoint(t, f.agentBAuth, "", http.MethodPost, CreateWebhookEndpointRequest{Name: "agent-b-ep"})
	require.Equal(t, http.StatusCreated, epBRec.Code)
	var epB WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(epBRec.Body.Bytes(), &epB))

	seedEvent := func(sourceID, dedupKey string, offset time.Duration) *store.TriggerEvent {
		ts := now.Add(offset)
		ev := &store.TriggerEvent{
			ID:           api.NewUUID(),
			ProjectID:    f.projectID,
			SourceKind:   store.TriggerSourceKindWebhook,
			SourceID:     sourceID,
			Type:         "webhook.received",
			Subject:      "webhook:" + sourceID,
			DedupKey:     dedupKey,
			OccurredAt:   ts,
			ReceivedAt:   ts,
			Attributes:   map[string]string{"signature_mode": "scion_v1"},
			Headers:      map[string]string{"Content-Type": "application/json"},
			Payload:      `{"event":"` + dedupKey + `"}`,
			PayloadBytes: len(`{"event":"` + dedupKey + `"}`),
			Status:       store.TriggerEventStatusProcessed,
			CreatedAt:    ts,
		}
		require.NoError(t, f.store.CreateTriggerEvent(ctx, ev))
		return ev
	}

	ownerEvent := seedEvent(ownerEp.ID, "owner-evt", 0)
	agentAEndpointEvent := seedEvent(epA.ID, "agent-a-ep-evt", time.Second)                   // Branch 1: owned endpoint, NO trigger_fires row.
	firedByAgentATriggerEvent := seedEvent(epADeleted.ID, "agent-a-fired-evt", 2*time.Second) // Branch 2: endpoint will be deleted; visible ONLY via trigger_fires join!
	agentBEvent := seedEvent(epB.ID, "agent-b-ep-evt", 3*time.Second)

	// Record a trigger_fires row for Agent A's trigger trA on firedByAgentATriggerEvent,
	// then delete epADeleted so Branch 1 (owned endpoint lookup) CANNOT satisfy it — proving
	// Branch 2 (AgentHasFiredTriggerForEvent / trigger_fires join) authorizes both GET and LIST!
	claimed, _, err := f.store.ClaimTriggerFire(ctx, &store.TriggerFire{
		ID:           api.NewUUID(),
		ProjectID:    f.projectID,
		TriggerID:    trA.ID,
		EventID:      firedByAgentATriggerEvent.ID,
		Subject:      firedByAgentATriggerEvent.Subject,
		ActionType:   store.TriggerActionMessage,
		Status:       store.TriggerFireStatusFiring,
		ExecutorKind: "trigger_drainer",
		ExecutorID:   "trigger_event:" + firedByAgentATriggerEvent.ID,
		LeaseUntil:   now.Add(10 * time.Minute),
		StartedAt:    now,
		CreatedAt:    now,
	})
	require.NoError(t, err)
	require.True(t, claimed)
	delEpRec := f.doWebhookEndpoint(t, f.agentAAuth, epADeleted.ID, http.MethodDelete, nil)
	require.Equal(t, http.StatusNoContent, delEpRec.Code)

	t.Run("project roles: owner, admin, member 200; viewer 403", func(t *testing.T) {
		for _, caller := range []struct {
			name string
			auth UserIdentity
		}{
			{"owner", f.ownerAuth},
			{"admin", f.adminAuth},
			{"member", f.memberAuth},
		} {
			assert.Equal(t, http.StatusOK, f.doTrigger(t, caller.auth, ownerTr.ID+"/history", http.MethodGet, nil).Code, caller.name)
			assert.Equal(t, http.StatusOK, f.doTriggerEvents(t, caller.auth, "", http.MethodGet, nil).Code, caller.name)
			assert.Equal(t, http.StatusOK, f.doTriggerEvents(t, caller.auth, ownerEvent.ID, http.MethodGet, nil).Code, caller.name)
		}

		assert.Equal(t, http.StatusForbidden, f.doTrigger(t, f.viewerAuth, ownerTr.ID+"/history", http.MethodGet, nil).Code)
		assert.Equal(t, http.StatusForbidden, f.doTriggerEvents(t, f.viewerAuth, "", http.MethodGet, nil).Code)
		assert.Equal(t, http.StatusForbidden, f.doTriggerEvents(t, f.viewerAuth, ownerEvent.ID, http.MethodGet, nil).Code)
	})

	t.Run("UAT scopes and project boundary", func(t *testing.T) {
		projBoundary := TokenBoundary{Kind: BoundaryKindProject, ProjectID: f.projectID}
		wrongBoundary := TokenBoundary{Kind: BoundaryKindProject, ProjectID: f.otherProjID}

		noScopeUAT := scheduleTestUAT(t, f.ownerAuth, projBoundary, "project:read")
		assert.Equal(t, http.StatusForbidden, f.doTriggerEvents(t, noScopeUAT, "", http.MethodGet, nil).Code)
		assert.Equal(t, http.StatusForbidden, f.doTriggerEvents(t, noScopeUAT, ownerEvent.ID, http.MethodGet, nil).Code)
		assert.Equal(t, http.StatusForbidden, f.doTrigger(t, noScopeUAT, ownerTr.ID+"/history", http.MethodGet, nil).Code)

		listOnlyUAT := scheduleTestUAT(t, f.ownerAuth, projBoundary, "trigger:list")
		assert.Equal(t, http.StatusOK, f.doTriggerEvents(t, listOnlyUAT, "", http.MethodGet, nil).Code)
		assert.Equal(t, http.StatusForbidden, f.doTriggerEvents(t, listOnlyUAT, ownerEvent.ID, http.MethodGet, nil).Code)
		assert.Equal(t, http.StatusForbidden, f.doTrigger(t, listOnlyUAT, ownerTr.ID+"/history", http.MethodGet, nil).Code)

		readOnlyUAT := scheduleTestUAT(t, f.ownerAuth, projBoundary, "trigger:read")
		assert.Equal(t, http.StatusForbidden, f.doTriggerEvents(t, readOnlyUAT, "", http.MethodGet, nil).Code)
		assert.Equal(t, http.StatusOK, f.doTriggerEvents(t, readOnlyUAT, ownerEvent.ID, http.MethodGet, nil).Code)
		assert.Equal(t, http.StatusOK, f.doTrigger(t, readOnlyUAT, ownerTr.ID+"/history", http.MethodGet, nil).Code)

		wrongProjUAT := scheduleTestUAT(t, f.ownerAuth, wrongBoundary, "trigger:list", "trigger:read")
		assert.Equal(t, http.StatusForbidden, f.doTriggerEvents(t, wrongProjUAT, "", http.MethodGet, nil).Code)
		assert.Equal(t, http.StatusForbidden, f.doTriggerEvents(t, wrongProjUAT, ownerEvent.ID, http.MethodGet, nil).Code)
		assert.Equal(t, http.StatusForbidden, f.doTrigger(t, wrongProjUAT, ownerTr.ID+"/history", http.MethodGet, nil).Code)
	})

	t.Run("agent event visibility: user endpoint 404, own endpoint 200, fired-by-own-trigger 200", func(t *testing.T) {
		// Role none (no ScopeProjectRead) -> 403.
		assert.Equal(t, http.StatusForbidden, f.doTriggerEvents(t, f.agentNoneJWT, "", http.MethodGet, nil).Code)
		assert.Equal(t, http.StatusForbidden, f.doTriggerEvents(t, f.agentNoneJWT, agentAEndpointEvent.ID, http.MethodGet, nil).Code)

		// Missing event baseline for 404 body parity check.
		missingEvtRec := f.doTriggerEvents(t, f.agentAAuth, api.NewUUID(), http.MethodGet, nil)
		require.Equal(t, http.StatusNotFound, missingEvtRec.Code)

		// §15 / §16.7: Agent A GETs an event of a user's endpoint -> 404 identical to missing!
		ownerEvtRec := f.doTriggerEvents(t, f.agentAAuth, ownerEvent.ID, http.MethodGet, nil)
		assert.Equal(t, http.StatusNotFound, ownerEvtRec.Code)
		assert.JSONEq(t, missingEvtRec.Body.String(), ownerEvtRec.Body.String(),
			"agent GET on event of user's endpoint must return 404 byte-equivalent to missing event")

		// Agent A GETs an event of Agent B's endpoint -> 404 identical to missing!
		agentBEvtRec := f.doTriggerEvents(t, f.agentAAuth, agentBEvent.ID, http.MethodGet, nil)
		assert.Equal(t, http.StatusNotFound, agentBEvtRec.Code)
		assert.JSONEq(t, missingEvtRec.Body.String(), agentBEvtRec.Body.String(),
			"agent GET on event of another agent's endpoint must return 404 byte-equivalent to missing event")

		// Branch 1 (owned endpoint): Agent A GETs event received on epA -> 200!
		ownEpEvtRec := f.doTriggerEvents(t, f.agentAAuth, agentAEndpointEvent.ID, http.MethodGet, nil)
		require.Equal(t, http.StatusOK, ownEpEvtRec.Code, ownEpEvtRec.Body.String())
		var ownEpDetail TriggerEventDetailResponse
		require.NoError(t, json.Unmarshal(ownEpEvtRec.Body.Bytes(), &ownEpDetail))
		assert.Equal(t, agentAEndpointEvent.ID, ownEpDetail.ID)

		// §15 / §16.7 Branch 2 (fired-by-own-trigger): Agent A GETs event its own trigger fired -> 200!
		firedEvtRec := f.doTriggerEvents(t, f.agentAAuth, firedByAgentATriggerEvent.ID, http.MethodGet, nil)
		require.Equal(t, http.StatusOK, firedEvtRec.Code, firedEvtRec.Body.String())
		var firedDetail TriggerEventDetailResponse
		require.NoError(t, json.Unmarshal(firedEvtRec.Body.Bytes(), &firedDetail))
		assert.Equal(t, firedByAgentATriggerEvent.ID, firedDetail.ID)

		// Meanwhile Agent B GETs that same firedByAgentATriggerEvent -> 404!
		assert.Equal(t, http.StatusNotFound, f.doTriggerEvents(t, f.agentBAuth, firedByAgentATriggerEvent.ID, http.MethodGet, nil).Code)

		// Agent A GET /trigger-events returns scope="caller" and ONLY the 2 visible events.
		listARec := f.doTriggerEvents(t, f.agentAAuth, "", http.MethodGet, nil)
		require.Equal(t, http.StatusOK, listARec.Code, listARec.Body.String())
		var listA ListTriggerEventsResponse
		require.NoError(t, json.Unmarshal(listARec.Body.Bytes(), &listA))
		assert.Equal(t, "caller", listA.Scope)
		require.Len(t, listA.Events, 2)
		assert.ElementsMatch(t,
			[]string{agentAEndpointEvent.ID, firedByAgentATriggerEvent.ID},
			[]string{listA.Events[0].ID, listA.Events[1].ID},
		)

		// Agent A filtering ?sourceId=ownerEp.ID returns 0 events (no cross-principal leak).
		probeListRec := f.doTriggerEvents(t, f.agentAAuth, "?sourceId="+ownerEp.ID, http.MethodGet, nil)
		require.Equal(t, http.StatusOK, probeListRec.Code)
		var probeList ListTriggerEventsResponse
		require.NoError(t, json.Unmarshal(probeListRec.Body.Bytes(), &probeList))
		assert.Empty(t, probeList.Events)

		// Agent A trigger history: 200 on own trigger trA, 403 on owner's trigger ownerTr.
		assert.Equal(t, http.StatusOK, f.doTrigger(t, f.agentAAuth, trA.ID+"/history", http.MethodGet, nil).Code)
		assert.Equal(t, http.StatusForbidden, f.doTrigger(t, f.agentAAuth, ownerTr.ID+"/history", http.MethodGet, nil).Code)
	})
}

func triggerTestTimePtr(t time.Time) *time.Time {
	return &t
}

func TestProjectTriggerFires_PaginationFilteringAndAuthz(t *testing.T) {
	f := setupTriggersTestFixture(t)
	ctx := context.Background()

	// Create owner endpoint and two owner triggers.
	epRec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{Name: "fires-ep"})
	require.Equal(t, http.StatusCreated, epRec.Code, epRec.Body.String())
	var ep WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(epRec.Body.Bytes(), &ep))

	tr1Rec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "fires-tr-1",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "one"},
	})
	require.Equal(t, http.StatusCreated, tr1Rec.Code, tr1Rec.Body.String())
	var tr1 TriggerResponse
	require.NoError(t, json.Unmarshal(tr1Rec.Body.Bytes(), &tr1))

	tr2Rec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "fires-tr-2",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "two"},
	})
	require.Equal(t, http.StatusCreated, tr2Rec.Code, tr2Rec.Body.String())
	var tr2 TriggerResponse
	require.NoError(t, json.Unmarshal(tr2Rec.Body.Bytes(), &tr2))

	// Agent A creates its own endpoint and trigger.
	epARec := f.doWebhookEndpoint(t, f.agentAAuth, "", http.MethodPost, CreateWebhookEndpointRequest{Name: "agent-a-fires-ep"})
	require.Equal(t, http.StatusCreated, epARec.Code, epARec.Body.String())
	var epA WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(epARec.Body.Bytes(), &epA))

	trARec := f.doTrigger(t, f.agentAAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "agent-a-fires-tr",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: epA.ID},
		Action: TriggerActionSpec{Type: "message", AgentID: f.agentA.ID, Message: "self"},
	})
	require.Equal(t, http.StatusCreated, trARec.Code, trARec.Body.String())
	var trA TriggerResponse
	require.NoError(t, json.Unmarshal(trARec.Body.Bytes(), &trA))

	baseTime := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	ev1 := &store.TriggerEvent{
		ID:         api.NewUUID(),
		ProjectID:  f.projectID,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   ep.ID,
		Type:       "webhook.received",
		Subject:    "webhook:" + ep.ID,
		DedupKey:   "fires-deliv-1",
		OccurredAt: baseTime,
		ReceivedAt: baseTime,
		Status:     store.TriggerEventStatusProcessed,
		CreatedAt:  baseTime,
	}
	ev2 := &store.TriggerEvent{
		ID:         api.NewUUID(),
		ProjectID:  f.projectID,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epA.ID,
		Type:       "webhook.received",
		Subject:    "webhook:" + epA.ID,
		DedupKey:   "fires-deliv-2",
		OccurredAt: baseTime.Add(2 * time.Minute),
		ReceivedAt: baseTime.Add(2 * time.Minute),
		Status:     store.TriggerEventStatusProcessed,
		CreatedAt:  baseTime.Add(2 * time.Minute),
	}
	require.NoError(t, f.store.CreateTriggerEvent(ctx, ev1))
	require.NoError(t, f.store.CreateTriggerEvent(ctx, ev2))

	waitUntil := baseTime.Add(time.Hour)
	notBefore := baseTime.Add(5 * time.Minute)
	deferredAt := baseTime.Add(time.Minute)

	fire1 := &store.TriggerFire{
		ID:         api.NewUUID(),
		ProjectID:  f.projectID,
		TriggerID:  tr1.ID,
		EventID:    ev1.ID,
		Subject:    ev1.Subject,
		ActionType: store.TriggerActionMessage,
		Status:     store.TriggerFireStatusFired,
		ResultRef:  "msg-1",
		LeaseUntil: baseTime.Add(time.Minute),
		StartedAt:  baseTime,
		FinishedAt: triggerTestTimePtr(baseTime.Add(2 * time.Second)),
		CreatedAt:  baseTime,
	}
	fire2 := &store.TriggerFire{
		ID:         api.NewUUID(),
		ProjectID:  f.projectID,
		TriggerID:  tr2.ID,
		EventID:    ev1.ID,
		Subject:    ev1.Subject,
		ActionType: store.TriggerActionMessage,
		Status:     store.TriggerFireStatusDeferred,
		NotBefore:  &notBefore,
		DeferredAt: &deferredAt,
		LeaseUntil: baseTime.Add(2 * time.Minute),
		StartedAt:  baseTime.Add(time.Minute),
		CreatedAt:  baseTime.Add(time.Minute),
	}
	fireA := &store.TriggerFire{
		ID:            api.NewUUID(),
		ProjectID:     f.projectID,
		TriggerID:     trA.ID,
		EventID:       ev2.ID,
		Subject:       ev2.Subject,
		ActionType:    store.TriggerActionMessage,
		Status:        store.TriggerFireStatusWaiting,
		TargetAgentID: f.agentA.ID,
		WaitUntil:     &waitUntil,
		Attempts:      1,
		LeaseUntil:    baseTime.Add(3 * time.Minute),
		StartedAt:     baseTime.Add(2 * time.Minute),
		CreatedAt:     baseTime.Add(2 * time.Minute),
	}
	for _, tf := range []*store.TriggerFire{fire1, fire2, fireA} {
		claimed, _, err := f.store.ClaimTriggerFire(ctx, tf)
		require.NoError(t, err)
		require.True(t, claimed)
	}

	// Method not allowed and subpath 404.
	assert.Equal(t, http.StatusMethodNotAllowed, f.doTriggerFires(t, f.ownerAuth, "", http.MethodPost, nil).Code)
	assert.Equal(t, http.StatusNotFound, f.doTriggerFires(t, f.ownerAuth, "extra", http.MethodGet, nil).Code)

	// Invalid query params return 400.
	assert.Equal(t, http.StatusBadRequest, f.doTriggerFires(t, f.ownerAuth, "?status=bogus", http.MethodGet, nil).Code)
	assert.Equal(t, http.StatusBadRequest, f.doTriggerFires(t, f.ownerAuth, "?since=not-a-time", http.MethodGet, nil).Code)
	assert.Equal(t, http.StatusBadRequest, f.doTriggerFires(t, f.ownerAuth, "?triggerId=nonexistent-name", http.MethodGet, nil).Code)
	assert.Equal(t, http.StatusBadRequest, f.doTriggerFires(t, f.ownerAuth, "?eventId=not-a-uuid", http.MethodGet, nil).Code)

	// Keyset pagination across all 3 fires.
	page1Rec := f.doTriggerFires(t, f.ownerAuth, "?limit=2", http.MethodGet, nil)
	require.Equal(t, http.StatusOK, page1Rec.Code, page1Rec.Body.String())
	var page1 ListTriggerFiresResponse
	require.NoError(t, json.Unmarshal(page1Rec.Body.Bytes(), &page1))
	assert.Equal(t, "", page1.Scope)
	assert.Equal(t, 3, page1.TotalCount)
	require.Len(t, page1.Fires, 2)
	assert.NotEmpty(t, page1.NextCursor)
	assert.False(t, page1.ServerTime.IsZero())
	// Ordered newest first: fireA, fire2.
	assert.Equal(t, fireA.ID, page1.Fires[0].ID)
	assert.Equal(t, store.TriggerActionMessage, page1.Fires[0].ActionType)
	require.NotNil(t, page1.Fires[0].WaitUntil)
	assert.Equal(t, fire2.ID, page1.Fires[1].ID)
	require.NotNil(t, page1.Fires[1].NotBefore)
	require.NotNil(t, page1.Fires[1].DeferredAt)

	page2Rec := f.doTriggerFires(t, f.ownerAuth, "?limit=2&cursor="+url.QueryEscape(page1.NextCursor), http.MethodGet, nil)
	require.Equal(t, http.StatusOK, page2Rec.Code, page2Rec.Body.String())
	var page2 ListTriggerFiresResponse
	require.NoError(t, json.Unmarshal(page2Rec.Body.Bytes(), &page2))
	require.Len(t, page2.Fires, 1)
	assert.Equal(t, fire1.ID, page2.Fires[0].ID)
	assert.Empty(t, page2.NextCursor)

	// Filter by triggerId (UUID and name), status, since, eventId.
	byTrRec := f.doTriggerFires(t, f.ownerAuth, "?triggerId="+tr1.ID, http.MethodGet, nil)
	require.Equal(t, http.StatusOK, byTrRec.Code)
	var byTr ListTriggerFiresResponse
	require.NoError(t, json.Unmarshal(byTrRec.Body.Bytes(), &byTr))
	require.Len(t, byTr.Fires, 1)
	assert.Equal(t, fire1.ID, byTr.Fires[0].ID)

	byTrNameRec := f.doTriggerFires(t, f.ownerAuth, "?triggerId=fires-tr-2", http.MethodGet, nil)
	require.Equal(t, http.StatusOK, byTrNameRec.Code)
	var byTrName ListTriggerFiresResponse
	require.NoError(t, json.Unmarshal(byTrNameRec.Body.Bytes(), &byTrName))
	require.Len(t, byTrName.Fires, 1)
	assert.Equal(t, fire2.ID, byTrName.Fires[0].ID)

	byStatusRec := f.doTriggerFires(t, f.ownerAuth, "?status=waiting", http.MethodGet, nil)
	require.Equal(t, http.StatusOK, byStatusRec.Code)
	var byStatus ListTriggerFiresResponse
	require.NoError(t, json.Unmarshal(byStatusRec.Body.Bytes(), &byStatus))
	require.Len(t, byStatus.Fires, 1)
	assert.Equal(t, fireA.ID, byStatus.Fires[0].ID)

	bySinceRec := f.doTriggerFires(t, f.ownerAuth, "?since="+url.QueryEscape(baseTime.Add(time.Minute).Format(time.RFC3339)), http.MethodGet, nil)
	require.Equal(t, http.StatusOK, bySinceRec.Code)
	var bySince ListTriggerFiresResponse
	require.NoError(t, json.Unmarshal(bySinceRec.Body.Bytes(), &bySince))
	assert.Len(t, bySince.Fires, 2)

	byEventRec := f.doTriggerFires(t, f.ownerAuth, "?eventId="+ev1.ID, http.MethodGet, nil)
	require.Equal(t, http.StatusOK, byEventRec.Code)
	var byEvent ListTriggerFiresResponse
	require.NoError(t, json.Unmarshal(byEventRec.Body.Bytes(), &byEvent))
	assert.Len(t, byEvent.Fires, 2)

	// Authz matrix: owner/admin/member -> 200; viewer -> 403.
	for _, tc := range []struct {
		name string
		auth Identity
		want int
	}{
		{"owner", f.ownerAuth, http.StatusOK},
		{"admin", f.adminAuth, http.StatusOK},
		{"member", f.memberAuth, http.StatusOK},
		{"viewer", f.viewerAuth, http.StatusForbidden},
	} {
		assert.Equal(t, tc.want, f.doTriggerFires(t, tc.auth, "", http.MethodGet, nil).Code, tc.name)
	}

	// UAT scopes and project boundary.
	projBoundary := TokenBoundary{Kind: BoundaryKindProject, ProjectID: f.projectID}
	wrongBoundary := TokenBoundary{Kind: BoundaryKindProject, ProjectID: f.otherProjID}
	assert.Equal(t, http.StatusForbidden, f.doTriggerFires(t, scheduleTestUAT(t, f.ownerAuth, projBoundary, "project:read"), "", http.MethodGet, nil).Code)
	assert.Equal(t, http.StatusOK, f.doTriggerFires(t, scheduleTestUAT(t, f.ownerAuth, projBoundary, "trigger:list"), "", http.MethodGet, nil).Code)
	assert.Equal(t, http.StatusForbidden, f.doTriggerFires(t, scheduleTestUAT(t, f.ownerAuth, projBoundary, "trigger:read"), "", http.MethodGet, nil).Code)
	assert.Equal(t, http.StatusForbidden, f.doTriggerFires(t, scheduleTestUAT(t, f.ownerAuth, wrongBoundary, "trigger:list"), "", http.MethodGet, nil).Code)

	// Agent callers see only fires of triggers they hold (scope="caller").
	assert.Equal(t, http.StatusForbidden, f.doTriggerFires(t, f.agentNoneJWT, "", http.MethodGet, nil).Code)

	agentARec := f.doTriggerFires(t, f.agentAAuth, "", http.MethodGet, nil)
	require.Equal(t, http.StatusOK, agentARec.Code, agentARec.Body.String())
	var agentAFires ListTriggerFiresResponse
	require.NoError(t, json.Unmarshal(agentARec.Body.Bytes(), &agentAFires))
	assert.Equal(t, "caller", agentAFires.Scope)
	require.Len(t, agentAFires.Fires, 1)
	assert.Equal(t, fireA.ID, agentAFires.Fires[0].ID)

	// Agent A filtering by owner's trigger UUID sees 0 fires; filtering by owner's trigger name returns 400 (no name leak).
	agentAProbeUUID := f.doTriggerFires(t, f.agentAAuth, "?triggerId="+tr1.ID, http.MethodGet, nil)
	require.Equal(t, http.StatusOK, agentAProbeUUID.Code)
	var probeUUID ListTriggerFiresResponse
	require.NoError(t, json.Unmarshal(agentAProbeUUID.Body.Bytes(), &probeUUID))
	assert.Empty(t, probeUUID.Fires)

	assert.Equal(t, http.StatusBadRequest, f.doTriggerFires(t, f.agentAAuth, "?triggerId=fires-tr-1", http.MethodGet, nil).Code)

	// Agent B holds no triggers -> 0 fires.
	agentBRec := f.doTriggerFires(t, f.agentBAuth, "", http.MethodGet, nil)
	require.Equal(t, http.StatusOK, agentBRec.Code)
	var agentBFires ListTriggerFiresResponse
	require.NoError(t, json.Unmarshal(agentBRec.Body.Bytes(), &agentBFires))
	assert.Equal(t, "caller", agentBFires.Scope)
	assert.Empty(t, agentBFires.Fires)
}
