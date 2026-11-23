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
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// failingCreateTriggerStore wraps store.Store and injects a failure on
// CreateTrigger inside WithTx to test composite create atomicity (§4.3, §15 P3).
type failingCreateTriggerStore struct {
	store.Store
	fault *storeFaultSwitch
}

func (f *failingCreateTriggerStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	return f.Store.WithTx(ctx, func(tx store.Store) error {
		return fn(&failingCreateTriggerStore{Store: tx, fault: f.fault})
	})
}

func (f *failingCreateTriggerStore) CreateTrigger(ctx context.Context, tr *store.Trigger) error {
	if f.fault.Active() {
		return errors.New("injected CreateTrigger failure")
	}
	return f.Store.CreateTrigger(ctx, tr)
}

func TestWebhookEndpointBoundCreateAndCap(t *testing.T) {
	t.Run("to_self_bind_and_restrictions", func(t *testing.T) {
		f := setupTriggersTestFixture(t)
		ctx := context.Background()

		// 1. Agent binds to self with composite trigger -> 201, both created.
		rec := f.doWebhookEndpoint(t, f.agentAAuth, "", http.MethodPost, map[string]any{
			"name":          "self-hook",
			"boundAgentId":  "self",
			"boundLifetime": "agent",
			"trigger": map[string]any{
				"message": "ping self",
			},
		})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		var created WebhookEndpointCreatedResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
		assert.Equal(t, f.agentA.ID, created.BoundAgentID)
		assert.Equal(t, store.WebhookBoundLifetimeAgent, created.BoundLifetime)
		require.NotNil(t, created.BoundAgent)
		assert.Equal(t, f.agentA.ID, created.BoundAgent.ID)
		assert.Equal(t, f.agentA.Name, created.BoundAgent.Name)
		assert.NotEmpty(t, created.Secret)
		assert.NotEmpty(t, created.TriggerID)
		require.NotNil(t, created.Trigger)
		assert.Equal(t, f.agentA.ID, created.Trigger.Action.AgentID)

		// 2. Agent cannot bind another agent -> 403 bind_self_only.
		rec = f.doWebhookEndpoint(t, f.agentAAuth, "", http.MethodPost, map[string]any{
			"name":          "other-agent-hook",
			"boundAgentId":  f.agentB.ID,
			"boundLifetime": "agent",
		})
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "bind_self_only")

		// 3. Invalid binding combinations -> 400 invalid_binding.
		for _, body := range []map[string]any{
			{"name": "bad-1", "boundAgentId": f.agentA.ID},
			{"name": "bad-2", "boundLifetime": "agent"},
			{"name": "bad-3", "boundAgentId": f.agentA.ID, "boundLifetime": "forever"},
			{"name": "bad-4", "boundAgentId": "self", "boundLifetime": "agent"}, // user passing "self"
		} {
			rec = f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, body)
			assert.Equal(t, http.StatusBadRequest, rec.Code, "body=%v resp=%s", body, rec.Body.String())
			assert.Contains(t, rec.Body.String(), "invalid_binding")
		}

		// 4. Cap of 5 bound endpoints per agent (agentA already has 1 above; add 4 more, 6th fails 409).
		for i := 2; i <= store.MaxBoundWebhookEndpointsPerAgent; i++ {
			rec = f.doWebhookEndpoint(t, f.agentAAuth, "", http.MethodPost, map[string]any{
				"name":          fmt.Sprintf("self-hook-%d", i),
				"boundAgentId":  "self",
				"boundLifetime": "agent",
			})
			require.Equal(t, http.StatusCreated, rec.Code, "i=%d resp=%s", i, rec.Body.String())
		}
		rec = f.doWebhookEndpoint(t, f.agentAAuth, "", http.MethodPost, map[string]any{
			"name":          "self-hook-6",
			"boundAgentId":  "self",
			"boundLifetime": "agent",
		})
		assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), ErrCodeLimitExceeded)

		// Verify DB has exactly 5 bound endpoints for agentA.
		cnt, err := f.store.CountBoundWebhookEndpoints(ctx, f.projectID, f.agentA.ID)
		require.NoError(t, err)
		assert.Equal(t, store.MaxBoundWebhookEndpointsPerAgent, cnt)
	})

	t.Run("composite_create_atomicity_fault_injection", func(t *testing.T) {
		srv, s := testServer(t)
		_, fault := installStoreFault(t, srv, func(inner store.Store, sw *storeFaultSwitch) *failingCreateTriggerStore {
			return &failingCreateTriggerStore{Store: inner, fault: sw}
		})
		f := setupTriggersTestFixtureOnServer(t, srv, s)
		ctx := context.Background()

		fault.Arm()
		rec := f.doWebhookEndpoint(t, f.agentAAuth, "", http.MethodPost, map[string]any{
			"name":          "atomic-fail-hook",
			"boundAgentId":  "self",
			"boundLifetime": "agent",
			"trigger": map[string]any{
				"message": "should roll back",
			},
		})
		assert.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())

		// Neither endpoint, secret, nor trigger should remain in the store.
		eps, err := f.store.ListWebhookEndpoints(ctx, store.WebhookEndpointFilter{ProjectID: f.projectID}, store.ListOptions{Limit: 10})
		require.NoError(t, err)
		assert.Empty(t, eps.Items, "rolled-back composite create must leave no webhook endpoint")

		trs, err := f.store.ListTriggers(ctx, store.TriggerFilter{ProjectID: f.projectID}, store.ListOptions{Limit: 10})
		require.NoError(t, err)
		assert.Empty(t, trs.Items, "rolled-back composite create must leave no trigger")

		secs, err := f.store.ListSecrets(ctx, store.SecretFilter{Scope: store.ScopeHub, ScopeID: f.srv.HubID()})
		require.NoError(t, err)
		for _, sec := range secs {
			assert.NotContains(t, sec.Key, "webhook_endpoint."+f.projectID, "rolled-back composite create must leave no secret row")
		}
	})
}

func TestWebhookEndpointBoundAgentAccess(t *testing.T) {
	f := setupTriggersTestFixture(t)
	ctx := context.Background()

	// Owner creates an endpoint bound to agentB (so agentB is bound, NOT owner).
	rec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, map[string]any{
		"name":          "user-bound-to-b",
		"boundAgentId":  f.agentB.ID,
		"boundLifetime": "agent",
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var epResp WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &epResp))

	// Create a trigger event on that endpoint.
	ev := &store.TriggerEvent{
		ID:         api.NewUUID(),
		ProjectID:  f.projectID,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epResp.ID,
		Type:       "webhook.received",
		Subject:    "webhook:" + epResp.ID,
		DedupKey:   api.NewUUID(),
		OccurredAt: time.Now().UTC(),
		ReceivedAt: time.Now().UTC(),
		Payload:    `{"hello":"world"}`,
		Status:     store.TriggerEventStatusProcessed,
	}
	require.NoError(t, f.store.CreateTriggerEvent(ctx, ev))

	// Bound agent (agentB) CAN read the endpoint and list it.
	rec = f.doWebhookEndpoint(t, f.agentBAuth, "/"+epResp.ID, http.MethodGet, nil)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = f.doWebhookEndpoint(t, f.agentBAuth, "", http.MethodGet, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var listResp ListWebhookEndpointsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &listResp))
	require.Len(t, listResp.Endpoints, 1)
	assert.Equal(t, epResp.ID, listResp.Endpoints[0].ID)

	// Bound agent (agentB) CAN read and list events of its bound endpoint.
	evReq := httptest.NewRequest(http.MethodGet, "/api/v1/projects/"+f.projectID+"/trigger-events/"+ev.ID, nil)
	evReq = evReq.WithContext(contextWithIdentity(evReq.Context(), f.agentBAuth))
	evRec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(evRec, evReq)
	assert.Equal(t, http.StatusOK, evRec.Code, evRec.Body.String())

	evListReq := httptest.NewRequest(http.MethodGet, "/api/v1/projects/"+f.projectID+"/trigger-events", nil)
	evListReq = evListReq.WithContext(contextWithIdentity(evListReq.Context(), f.agentBAuth))
	evListRec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(evListRec, evListReq)
	require.Equal(t, http.StatusOK, evListRec.Code)
	var evListResp ListTriggerEventsResponse
	require.NoError(t, json.Unmarshal(evListRec.Body.Bytes(), &evListResp))
	require.Len(t, evListResp.Events, 1)
	assert.Equal(t, ev.ID, evListResp.Events[0].ID)

	// Bound non-owner agent (agentB) CANNOT modify, rotate, or delete -> 403.
	rec = f.doWebhookEndpoint(t, f.agentBAuth, "/"+epResp.ID, http.MethodPatch, map[string]any{"description": "nope"})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	rec = f.doWebhookEndpoint(t, f.agentBAuth, "/"+epResp.ID+"/rotate-secret", http.MethodPost, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	rec = f.doWebhookEndpoint(t, f.agentBAuth, "/"+epResp.ID, http.MethodDelete, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	// Unrelated agent (agentA) gets 404 (no probing) on GET/PATCH/rotate/DELETE and sees nothing in lists.
	for _, tc := range []struct {
		method string
		suffix string
		body   any
	}{
		{http.MethodGet, "/" + epResp.ID, nil},
		{http.MethodPatch, "/" + epResp.ID, map[string]any{"description": "nope"}},
		{http.MethodPost, "/" + epResp.ID + "/rotate-secret", nil},
		{http.MethodDelete, "/" + epResp.ID, nil},
	} {
		rec = f.doWebhookEndpoint(t, f.agentAAuth, tc.suffix, tc.method, tc.body)
		assert.Equal(t, http.StatusNotFound, rec.Code, "%s %s resp=%s", tc.method, tc.suffix, rec.Body.String())
	}

	// Owner agent (agentA creates --to-self) CAN update description/status and rotate-secret,
	// but CANNOT change binding via PATCH (403).
	rec = f.doWebhookEndpoint(t, f.agentAAuth, "", http.MethodPost, map[string]any{
		"name":          "agent-a-owned",
		"boundAgentId":  "self",
		"boundLifetime": "agent",
	})
	require.Equal(t, http.StatusCreated, rec.Code)
	var ownedResp WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ownedResp))

	rec = f.doWebhookEndpoint(t, f.agentAAuth, "/"+ownedResp.ID, http.MethodPatch, map[string]any{
		"description": "updated by owner agent",
		"status":      "disabled",
	})
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = f.doWebhookEndpoint(t, f.agentAAuth, "/"+ownedResp.ID, http.MethodPatch, map[string]any{
		"boundLifetime": "persistent",
	})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	rec = f.doWebhookEndpoint(t, f.agentAAuth, "/"+ownedResp.ID+"/rotate-secret", http.MethodPost, nil)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestWebhookEndpointUpdateAndDisable(t *testing.T) {
	f := setupTriggersTestFixture(t)

	rec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, map[string]any{
		"name":        "patch-hook",
		"description": "v1",
	})
	require.Equal(t, http.StatusCreated, rec.Code)
	var created WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))

	// Viewer cannot PATCH -> 403.
	rec = f.doWebhookEndpoint(t, f.viewerAuth, "/"+created.ID, http.MethodPatch, map[string]any{
		"description": "viewer-nope",
	})
	assert.Equal(t, http.StatusForbidden, rec.Code)

	// Member CAN PATCH description, status=disabled, and bind to agentA.
	rec = f.doWebhookEndpoint(t, f.memberAuth, "/"+created.ID, http.MethodPatch, map[string]any{
		"description":   "v2",
		"status":        "disabled",
		"boundAgentId":  f.agentA.ID,
		"boundLifetime": "persistent",
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var updated WebhookEndpointResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &updated))
	assert.Equal(t, "v2", updated.Description)
	assert.Equal(t, store.WebhookEndpointStatusDisabled, updated.Status)
	assert.Equal(t, f.agentA.ID, updated.BoundAgentID)
	assert.Equal(t, store.WebhookBoundLifetimePersistent, updated.BoundLifetime)
	assert.GreaterOrEqual(t, updated.ConfigVersion, 2)

	// Disabled endpoint returns 404 at ingress immediately (cache invalidated).
	body := []byte(`{"test":1}`)
	now := time.Now().UTC()
	hookReq := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/"+created.ID, bytes.NewReader(body))
	hookReq.Header.Set("Content-Type", "application/json")
	hookReq.Header.Set(trigger.HeaderSignature, trigger.SignScionV1(created.Secret, now.Unix(), "del-1", "", "", body))
	hookReq.Header.Set(trigger.HeaderDelivery, "del-1")
	hookRec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(hookRec, hookReq)
	assert.Equal(t, http.StatusNotFound, hookRec.Code)

	// Re-enable endpoint -> ingress returns 202.
	rec = f.doWebhookEndpoint(t, f.memberAuth, "/"+created.ID, http.MethodPatch, map[string]any{
		"status": "active",
	})
	require.Equal(t, http.StatusOK, rec.Code)

	hookReq2 := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/"+created.ID, bytes.NewReader(body))
	hookReq2.Header.Set("Content-Type", "application/json")
	hookReq2.Header.Set(trigger.HeaderSignature, trigger.SignScionV1(created.Secret, now.Unix(), "del-2", "", "", body))
	hookReq2.Header.Set(trigger.HeaderDelivery, "del-2")
	hookRec2 := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(hookRec2, hookReq2)
	assert.Equal(t, http.StatusAccepted, hookRec2.Code, hookRec2.Body.String())

	// Unbind via PATCH boundAgentId="".
	rec = f.doWebhookEndpoint(t, f.memberAuth, "/"+created.ID, http.MethodPatch, map[string]any{
		"boundAgentId": "",
	})
	require.Equal(t, http.StatusOK, rec.Code)
	var unbound WebhookEndpointResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &unbound))
	assert.Empty(t, unbound.BoundAgentID)
	assert.Empty(t, unbound.BoundLifetime)
}

func TestWebhookEndpointSecretRotation_GraceWindowAndMaintenance(t *testing.T) {
	f := setupTriggersTestFixture(t)
	ctx := context.Background()

	rec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, map[string]any{
		"name": "rotate-hook",
	})
	require.Equal(t, http.StatusCreated, rec.Code)
	var created WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	secretV1 := created.Secret

	// Rotate secret -> returns v2 secret once, sets SecretVersion=2 and SecretRotatedAt.
	rec = f.doWebhookEndpoint(t, f.ownerAuth, "/"+created.ID+"/rotate-secret", http.MethodPost, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var rotated WebhookEndpointRotatedResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rotated))
	assert.Equal(t, 2, rotated.SecretVersion)
	assert.NotEmpty(t, rotated.Secret)
	assert.NotEqual(t, secretV1, rotated.Secret)
	require.NotNil(t, rotated.SecretRotatedAt)
	assert.True(t, rotated.GraceWindowExpires.After(*rotated.SecretRotatedAt))
	secretV2 := rotated.Secret

	// Both v2 and v1 verify at ingress within grace window.
	sendHook := func(sec, del string) *httptest.ResponseRecorder {
		body := []byte(`{"rot":true}`)
		now := time.Now().UTC()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/"+created.ID, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(trigger.HeaderDelivery, del)
		req.Header.Set(trigger.HeaderSignature, trigger.SignScionV1(sec, now.Unix(), del, "", "", body))
		r := httptest.NewRecorder()
		f.srv.Handler().ServeHTTP(r, req)
		return r
	}
	assert.Equal(t, http.StatusAccepted, sendHook(secretV2, "rot-v2-in-grace").Code)
	assert.Equal(t, http.StatusAccepted, sendHook(secretV1, "rot-v1-in-grace").Code)

	// Maintenance within grace keeps v1.
	res, err := trigger.RunMaintenance(ctx, f.store, rotated.SecretRotatedAt.Add(12*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 0, res.RotatedSecretsPurged)

	// Maintenance after 24h grace deletes v1 and preserves v2.
	res, err = trigger.RunMaintenance(ctx, f.store, rotated.SecretRotatedAt.Add(store.WebhookSecretRotationGraceWindow+time.Minute))
	require.NoError(t, err)
	assert.Equal(t, 1, res.RotatedSecretsPurged)

	_, err = f.store.GetSecret(ctx, store.WebhookEndpointSecretKey(f.projectID, created.ID, 1), store.ScopeHub, f.srv.HubID())
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = f.store.GetSecret(ctx, store.WebhookEndpointSecretKey(f.projectID, created.ID, 2), store.ScopeHub, f.srv.HubID())
	require.NoError(t, err)

	// Invalidate cache so ingress reloads; v1 now fails with 401 while v2 succeeds.
	f.srv.webhookIngress.InvalidateEndpoint(created.ID)
	assert.Equal(t, http.StatusAccepted, sendHook(secretV2, "rot-v2-post-purge").Code)
	assert.Equal(t, http.StatusUnauthorized, sendHook(secretV1, "rot-v1-post-purge").Code)
}

func TestAgentDelete_ReleaseTriggerBindingsCascade(t *testing.T) {
	f := setupTriggersTestFixture(t)
	ctx := context.Background()

	// 1. Agent-lifetime endpoint bound to agentA, with agentA's own trigger + owner's trigger.
	rec := f.doWebhookEndpoint(t, f.agentAAuth, "", http.MethodPost, map[string]any{
		"name":          "ep-agent-cascade",
		"boundAgentId":  "self",
		"boundLifetime": "agent",
		"trigger": map[string]any{
			"message": "own rule",
		},
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var epAgentResp WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &epAgentResp))
	ownTriggerID := epAgentResp.TriggerID

	trOther := &store.Trigger{
		ID:             api.NewUUID(),
		ProjectID:      f.projectID,
		Name:           "other-author-rule",
		SourceKind:     store.TriggerSourceKindWebhook,
		SourceID:       epAgentResp.ID,
		MatchTypes:     []string{"webhook.received"},
		ActionType:     store.TriggerActionMessage,
		ActionPayload:  `{"agentId":"` + f.agentB.ID + `","agentName":"` + f.agentB.Slug + `","message":"hi"}`,
		SubjectDialect: store.TriggerDialectCEL,
		SubjectMode:    store.TriggerSubjectModeNew,
		Status:         store.TriggerStatusActive,
		CreatedBy:      f.owner.ID,
		InitiatorAttribution: store.InitiatorAttribution{
			AttributionVersion:      1,
			InitiatorPrincipalKind:  store.DelegationPrincipalUser,
			InitiatorPrincipalID:    f.owner.ID,
			InitiatorCredentialKind: store.InitiatorCredentialKindSession,
			AuthorizationRevision:   1,
		},
	}
	require.NoError(t, f.store.CreateTrigger(ctx, trOther))

	// 2. Persistent endpoint bound to agentA, with a rule targeting agentA and an unrelated rule.
	rec = f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, map[string]any{
		"name":          "ep-persist-cascade",
		"boundAgentId":  f.agentA.ID,
		"boundLifetime": "persistent",
		"trigger": map[string]any{
			"message": "target agentA",
		},
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var epPersistResp WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &epPersistResp))
	persistTargetTriggerID := epPersistResp.TriggerID

	trUnrelated := &store.Trigger{
		ID:             api.NewUUID(),
		ProjectID:      f.projectID,
		Name:           "persist-unrelated-rule",
		SourceKind:     store.TriggerSourceKindWebhook,
		SourceID:       epPersistResp.ID,
		MatchTypes:     []string{"webhook.received"},
		ActionType:     store.TriggerActionMessage,
		ActionPayload:  `{"agentId":"` + f.agentB.ID + `","agentName":"` + f.agentB.Slug + `","message":"hi"}`,
		SubjectDialect: store.TriggerDialectCEL,
		SubjectMode:    store.TriggerSubjectModeNew,
		Status:         store.TriggerStatusActive,
		CreatedBy:      f.owner.ID,
		InitiatorAttribution: store.InitiatorAttribution{
			AttributionVersion:      1,
			InitiatorPrincipalKind:  store.DelegationPrincipalUser,
			InitiatorPrincipalID:    f.owner.ID,
			InitiatorCredentialKind: store.InitiatorCredentialKindSession,
			AuthorizationRevision:   1,
		},
	}
	require.NoError(t, f.store.CreateTrigger(ctx, trUnrelated))

	// 3. Soft-delete agentA through the HTTP agent delete route (which runs runAgentDeletion).
	delReq := httptest.NewRequest(http.MethodDelete, "/api/v1/projects/"+f.projectID+"/agents/"+f.agentA.ID+"?force=true", nil)
	delReq = delReq.WithContext(contextWithIdentity(delReq.Context(), f.ownerAuth))
	delRec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(delRec, delReq)
	require.Equal(t, http.StatusNoContent, delRec.Code, delRec.Body.String())

	// Verify agent-lifetime endpoint, its secret row, and agentA's own rule are deleted.
	_, err := f.store.GetWebhookEndpoint(ctx, epAgentResp.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = f.store.GetSecret(ctx, store.WebhookEndpointSecretKey(f.projectID, epAgentResp.ID, 1), store.ScopeHub, f.srv.HubID())
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = f.store.GetTrigger(ctx, ownTriggerID)
	assert.ErrorIs(t, err, store.ErrNotFound)

	// Other author's rule on the agent-lifetime endpoint is paused with source_deleted.
	gotOther, err := f.store.GetTrigger(ctx, trOther.ID)
	require.NoError(t, err)
	assert.Equal(t, store.TriggerStatusPaused, gotOther.Status)
	assert.Equal(t, store.TriggerStatusReasonSourceDeleted, gotOther.StatusReason)

	// Persistent endpoint survives, is unbound, secret intact; target rule paused target_deleted; unrelated active.
	gotPersist, err := f.store.GetWebhookEndpoint(ctx, epPersistResp.ID)
	require.NoError(t, err)
	assert.Empty(t, gotPersist.BoundAgentID)
	assert.Empty(t, gotPersist.BoundLifetime)
	_, err = f.store.GetSecret(ctx, store.WebhookEndpointSecretKey(f.projectID, epPersistResp.ID, 1), store.ScopeHub, f.srv.HubID())
	require.NoError(t, err)

	gotPersistTarget, err := f.store.GetTrigger(ctx, persistTargetTriggerID)
	require.NoError(t, err)
	assert.Equal(t, store.TriggerStatusPaused, gotPersistTarget.Status)
	assert.Equal(t, store.TriggerStatusReasonTargetDeleted, gotPersistTarget.StatusReason)

	gotUnrelated, err := f.store.GetTrigger(ctx, trUnrelated.ID)
	require.NoError(t, err)
	assert.Equal(t, store.TriggerStatusActive, gotUnrelated.Status)

	// 4. Verify audit records exist for cascade mutations.
	audits, _, err := f.store.ListMutationAudits(ctx, store.MutationAuditFilter{Limit: 100})
	require.NoError(t, err)
	auditByType := map[string]int{}
	for _, a := range audits {
		if a.TargetID == epAgentResp.ID || a.TargetID == epPersistResp.ID || a.TargetID == ownTriggerID || a.TargetID == trOther.ID {
			auditByType[a.MutationType]++
		}
	}
	assert.Equal(t, 1, auditByType[mutationTypeWebhookEndpointDelete])
	assert.Equal(t, 1, auditByType[mutationTypeWebhookEndpointUnbind])
	assert.Equal(t, 1, auditByType[mutationTypeTriggerDelete])
	assert.GreaterOrEqual(t, auditByType[mutationTypeTriggerPause], 1)

	// 5. Idempotency on retry: running releaseTriggerBindingsForAgent again adds no duplicate audits.
	f.srv.releaseTriggerBindingsForAgent(ctx, f.agentA)
	auditsAfter, _, err := f.store.ListMutationAudits(ctx, store.MutationAuditFilter{Limit: 100})
	require.NoError(t, err)
	assert.Equal(t, len(audits), len(auditsAfter), "cascade retry must be idempotent with no duplicate audits")

	// 6. Restoring soft-deleted agentA does NOT resurrect deleted endpoint/rules or rebind persistent endpoint.
	deletedAgent, err := f.store.GetAgentIncludingDeleted(ctx, f.agentA.ID)
	require.NoError(t, err)
	if !deletedAgent.DeletedAt.IsZero() {
		require.NoError(t, f.srv.restoreAgentTx(ctx, deletedAgent, AuditActor{}))
	}
	_, err = f.store.GetWebhookEndpoint(ctx, epAgentResp.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = f.store.GetTrigger(ctx, ownTriggerID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	gotPersistAfterRestore, err := f.store.GetWebhookEndpoint(ctx, epPersistResp.ID)
	require.NoError(t, err)
	assert.Empty(t, gotPersistAfterRestore.BoundAgentID)
}
