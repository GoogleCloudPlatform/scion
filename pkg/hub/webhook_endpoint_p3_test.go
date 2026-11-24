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
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
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
	rec = f.doWebhookEndpoint(t, f.agentBAuth, epResp.ID, http.MethodGet, nil)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = f.doWebhookEndpoint(t, f.agentBAuth, "", http.MethodGet, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var listResp ListWebhookEndpointsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &listResp))
	require.Len(t, listResp.Endpoints, 1)
	assert.Equal(t, epResp.ID, listResp.Endpoints[0].ID)

	// Bound agent (agentB) CAN read and list events of its bound endpoint.
	evReq := authoredRequest(t, f.agentBAuth, http.MethodGet, "/api/v1/projects/"+f.projectID+"/trigger-events/"+ev.ID, nil)
	evRec := httptest.NewRecorder()
	f.srv.handleTriggerEvents(evRec, evReq, f.projectID, ev.ID)
	assert.Equal(t, http.StatusOK, evRec.Code, evRec.Body.String())

	evListReq := authoredRequest(t, f.agentBAuth, http.MethodGet, "/api/v1/projects/"+f.projectID+"/trigger-events", nil)
	evListRec := httptest.NewRecorder()
	f.srv.handleTriggerEvents(evListRec, evListReq, f.projectID, "")
	require.Equal(t, http.StatusOK, evListRec.Code)
	var evListResp ListTriggerEventsResponse
	require.NoError(t, json.Unmarshal(evListRec.Body.Bytes(), &evListResp))
	require.Len(t, evListResp.Events, 1)
	assert.Equal(t, ev.ID, evListResp.Events[0].ID)

	// Bound non-owner agent (agentB) CANNOT modify, rotate, or delete -> 403.
	rec = f.doWebhookEndpoint(t, f.agentBAuth, epResp.ID, http.MethodPatch, map[string]any{"description": "nope"})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	rec = f.doWebhookEndpoint(t, f.agentBAuth, epResp.ID+"/rotate-secret", http.MethodPost, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	rec = f.doWebhookEndpoint(t, f.agentBAuth, epResp.ID, http.MethodDelete, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	// Unrelated agent (agentA) gets 404 (no probing) on GET/PATCH/rotate/DELETE and sees nothing in lists.
	for _, tc := range []struct {
		method string
		suffix string
		body   any
	}{
		{http.MethodGet, epResp.ID, nil},
		{http.MethodPatch, epResp.ID, map[string]any{"description": "nope"}},
		{http.MethodPost, epResp.ID + "/rotate-secret", nil},
		{http.MethodDelete, epResp.ID, nil},
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

	rec = f.doWebhookEndpoint(t, f.agentAAuth, ownedResp.ID, http.MethodPatch, map[string]any{
		"description": "updated by owner agent",
		"status":      "disabled",
	})
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = f.doWebhookEndpoint(t, f.agentAAuth, ownedResp.ID, http.MethodPatch, map[string]any{
		"boundLifetime": "persistent",
	})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	// O-B: owner agent cannot unbind or self-bind via PATCH either (users only).
	rec = f.doWebhookEndpoint(t, f.agentAAuth, ownedResp.ID, http.MethodPatch, map[string]any{
		"boundAgentId":  "",
		"boundLifetime": "",
	})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	rec = f.doWebhookEndpoint(t, f.agentAAuth, ownedResp.ID, http.MethodPatch, map[string]any{
		"boundAgentId":  "self",
		"boundLifetime": "persistent",
	})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	rec = f.doWebhookEndpoint(t, f.agentAAuth, ownedResp.ID+"/rotate-secret", http.MethodPost, nil)
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

	// Member creates its own endpoint (Stage 1 create/list/read still allowed).
	rec = f.doWebhookEndpoint(t, f.memberAuth, "", http.MethodPost, map[string]any{
		"name": "member-own-hook",
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var memberCreated WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &memberCreated))

	// R1 regression (option A): project-member does NOT hold webhook_endpoint.update
	// and is refused (403) on PATCH, bind, and rotate-secret for BOTH an endpoint
	// someone else owns AND one the member created.
	for _, epID := range []string{created.ID, memberCreated.ID} {
		rec = f.doWebhookEndpoint(t, f.memberAuth, epID, http.MethodPatch, map[string]any{
			"description": "member-nope",
		})
		assert.Equal(t, http.StatusForbidden, rec.Code, "member PATCH desc ep=%s", epID)

		rec = f.doWebhookEndpoint(t, f.memberAuth, epID, http.MethodPatch, map[string]any{
			"boundAgentId":  f.agentA.ID,
			"boundLifetime": "agent",
		})
		assert.Equal(t, http.StatusForbidden, rec.Code, "member PATCH bind ep=%s", epID)

		rec = f.doWebhookEndpoint(t, f.memberAuth, epID+"/rotate-secret", http.MethodPost, nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, "member rotate-secret ep=%s", epID)
	}

	// Viewer cannot PATCH -> 403.
	rec = f.doWebhookEndpoint(t, f.viewerAuth, created.ID, http.MethodPatch, map[string]any{
		"description": "viewer-nope",
	})
	assert.Equal(t, http.StatusForbidden, rec.Code)

	// Project admin CAN PATCH description, status=disabled, and bind to agentA, and rotate-secret.
	rec = f.doWebhookEndpoint(t, f.adminAuth, created.ID, http.MethodPatch, map[string]any{
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
	require.NotNil(t, updated.BoundAgent)
	assert.Equal(t, f.agentA.ID, updated.BoundAgent.ID)
	assert.Equal(t, store.WebhookBoundLifetimePersistent, updated.BoundLifetime)
	assert.GreaterOrEqual(t, updated.ConfigVersion, 2)

	// Admin CAN rotate-secret on any endpoint in the project (including member's).
	rec = f.doWebhookEndpoint(t, f.adminAuth, memberCreated.ID+"/rotate-secret", http.MethodPost, nil)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

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

	// Owner re-enables endpoint -> ingress returns 202.
	rec = f.doWebhookEndpoint(t, f.ownerAuth, created.ID, http.MethodPatch, map[string]any{
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

	// Owner unbinds via PATCH boundAgentId="".
	rec = f.doWebhookEndpoint(t, f.ownerAuth, created.ID, http.MethodPatch, map[string]any{
		"boundAgentId": "",
	})
	require.Equal(t, http.StatusOK, rec.Code)
	var unbound WebhookEndpointResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &unbound))
	assert.Nil(t, unbound.BoundAgent)
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
	rec = f.doWebhookEndpoint(t, f.ownerAuth, created.ID+"/rotate-secret", http.MethodPost, nil)
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
	// O1: enable soft-delete retention so the delete is a real soft delete and
	// the restore step runs unconditionally.
	f.srv.config.SoftDeleteRetention = 72 * time.Hour
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

	// 3. Soft-delete agentA through the agent delete handler (which runs runAgentDeletion).
	delReq := authoredRequest(t, f.ownerAuth, http.MethodDelete, "/api/v1/projects/"+f.projectID+"/agents/"+f.agentA.ID, nil)
	delRec := httptest.NewRecorder()
	f.srv.deleteAgent(delRec, delReq, f.agentA.ID)
	require.Equal(t, http.StatusNoContent, delRec.Code, delRec.Body.String())

	deletedAgent, err := f.store.GetAgent(ctx, f.agentA.ID)
	require.NoError(t, err, "soft delete must retain the agent row")
	require.False(t, deletedAgent.DeletedAt.IsZero(), "expected soft delete with deleted_at set")

	// Verify agent-lifetime endpoint, its secret row, and agentA's own rule are deleted.
	_, err = f.store.GetWebhookEndpoint(ctx, epAgentResp.ID)
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
	require.NoError(t, f.srv.restoreAgentTx(ctx, deletedAgent, AuditActor{}))
	_, err = f.store.GetWebhookEndpoint(ctx, epAgentResp.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = f.store.GetTrigger(ctx, ownTriggerID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	gotPersistAfterRestore, err := f.store.GetWebhookEndpoint(ctx, epPersistResp.ID)
	require.NoError(t, err)
	assert.Empty(t, gotPersistAfterRestore.BoundAgentID)
}

// TestWebhookEndpointMaintenanceSweep_OrphanedAgentLifetime verifies the O2
// hourly-maintenance backstop sweep for agent-lifetime endpoints whose bound
// agent is soft-deleted or missing (idempotent, audited, same cascade code).
func TestWebhookEndpointMaintenanceSweep_OrphanedAgentLifetime(t *testing.T) {
	f := setupTriggersTestFixture(t)
	f.srv.config.SoftDeleteRetention = 72 * time.Hour
	ctx := context.Background()

	// Soft-delete agentA first, then simulate an orphaned agent-lifetime endpoint
	// left behind by a transient tail-step error, plus a persistent endpoint on agentA,
	// plus an agent-lifetime endpoint bound to a missing agent UUID, plus a live endpoint on agentB.
	delReq := authoredRequest(t, f.ownerAuth, http.MethodDelete, "/api/v1/projects/"+f.projectID+"/agents/"+f.agentA.ID, nil)
	delRec := httptest.NewRecorder()
	f.srv.deleteAgent(delRec, delReq, f.agentA.ID)
	require.Equal(t, http.StatusNoContent, delRec.Code, delRec.Body.String())

	mkBoundEP := func(name, ownerKind, ownerID, boundAgentID, lifetime string) *store.WebhookEndpoint {
		t.Helper()
		ep := &store.WebhookEndpoint{
			ID:                  api.NewUUID(),
			ProjectID:           f.projectID,
			Name:                name,
			OwnerPrincipalKind:  ownerKind,
			OwnerPrincipalID:    ownerID,
			OwnerCredentialKind: store.InitiatorCredentialKindSession,
			SecretVersion:       1,
			SignatureMode:       store.WebhookSignatureModeScionV1,
			Status:              store.WebhookEndpointStatusActive,
			BoundAgentID:        boundAgentID,
			BoundLifetime:       lifetime,
			CreatedBy:           ownerID,
		}
		require.NoError(t, f.store.CreateWebhookEndpoint(ctx, ep))
		secRow, err := f.srv.newWebhookEndpointSecretRow(f.projectID, ep.ID, 1, "secret-"+name, ownerID)
		require.NoError(t, err)
		require.NoError(t, f.store.CreateSecret(ctx, secRow))
		return ep
	}

	epOrphanSoft := mkBoundEP("orphan-soft", store.DelegationPrincipalAgent, f.agentA.ID, f.agentA.ID, store.WebhookBoundLifetimeAgent)
	missingAgentID := api.NewUUID()
	epOrphanMissing := mkBoundEP("orphan-missing", store.DelegationPrincipalUser, f.owner.ID, missingAgentID, store.WebhookBoundLifetimeAgent)
	epLive := mkBoundEP("live-agent-ep", store.DelegationPrincipalAgent, f.agentB.ID, f.agentB.ID, store.WebhookBoundLifetimeAgent)
	epPersist := mkBoundEP("persist-soft-ep", store.DelegationPrincipalUser, f.owner.ID, f.agentA.ID, store.WebhookBoundLifetimePersistent)

	// Trigger authored by agentA on epOrphanSoft (must be deleted) and trigger authored by owner (must be paused source_deleted).
	trOwn := &store.Trigger{
		ID:             api.NewUUID(),
		ProjectID:      f.projectID,
		Name:           "sweep-own-rule",
		SourceKind:     store.TriggerSourceKindWebhook,
		SourceID:       epOrphanSoft.ID,
		MatchTypes:     []string{"webhook.received"},
		ActionType:     store.TriggerActionMessage,
		ActionPayload:  `{"agentId":"` + f.agentA.ID + `","message":"hi"}`,
		SubjectDialect: store.TriggerDialectCEL,
		SubjectMode:    store.TriggerSubjectModeNew,
		Status:         store.TriggerStatusActive,
		CreatedBy:      f.agentA.ID,
		InitiatorAttribution: store.InitiatorAttribution{
			AttributionVersion:      1,
			InitiatorPrincipalKind:  store.DelegationPrincipalAgent,
			InitiatorPrincipalID:    f.agentA.ID,
			InitiatorCredentialKind: store.InitiatorCredentialKindAgent,
			AuthorizationRevision:   1,
		},
	}
	require.NoError(t, f.store.CreateTrigger(ctx, trOwn))

	trOther := &store.Trigger{
		ID:             api.NewUUID(),
		ProjectID:      f.projectID,
		Name:           "sweep-other-rule",
		SourceKind:     store.TriggerSourceKindWebhook,
		SourceID:       epOrphanSoft.ID,
		MatchTypes:     []string{"webhook.received"},
		ActionType:     store.TriggerActionMessage,
		ActionPayload:  `{"agentId":"` + f.agentB.ID + `","message":"hi"}`,
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

	// First sweep cleans both orphaned agent-lifetime endpoints AND unbinds
	// the orphaned persistent endpoint, writing audits for each.
	swept := f.srv.sweepOrphanedBoundWebhookEndpoints(ctx)
	assert.Equal(t, 3, swept)

	_, err := f.store.GetWebhookEndpoint(ctx, epOrphanSoft.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = f.store.GetSecret(ctx, store.WebhookEndpointSecretKey(f.projectID, epOrphanSoft.ID, 1), store.ScopeHub, f.srv.HubID())
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = f.store.GetTrigger(ctx, trOwn.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)

	gotOther, err := f.store.GetTrigger(ctx, trOther.ID)
	require.NoError(t, err)
	assert.Equal(t, store.TriggerStatusPaused, gotOther.Status)
	assert.Equal(t, store.TriggerStatusReasonSourceDeleted, gotOther.StatusReason)

	_, err = f.store.GetWebhookEndpoint(ctx, epOrphanMissing.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)

	// Live agent-lifetime endpoint remains untouched; persistent orphan is unbound with secret intact.
	gotLive, err := f.store.GetWebhookEndpoint(ctx, epLive.ID)
	require.NoError(t, err)
	assert.Equal(t, f.agentB.ID, gotLive.BoundAgentID)

	gotPersist, err := f.store.GetWebhookEndpoint(ctx, epPersist.ID)
	require.NoError(t, err)
	assert.Empty(t, gotPersist.BoundAgentID)
	assert.Empty(t, gotPersist.BoundLifetime)
	_, err = f.store.GetSecret(ctx, store.WebhookEndpointSecretKey(f.projectID, epPersist.ID, 1), store.ScopeHub, f.srv.HubID())
	require.NoError(t, err)

	audits1, _, err := f.store.ListMutationAudits(ctx, store.MutationAuditFilter{Limit: 200})
	require.NoError(t, err)
	delAudits := 0
	unbindAudits := 0
	for _, a := range audits1 {
		if (a.TargetID == epOrphanSoft.ID || a.TargetID == epOrphanMissing.ID) && a.MutationType == mutationTypeWebhookEndpointDelete {
			delAudits++
		}
		if a.TargetID == epPersist.ID && a.MutationType == mutationTypeWebhookEndpointUnbind {
			unbindAudits++
		}
	}
	assert.Equal(t, 2, delAudits)
	assert.Equal(t, 1, unbindAudits)

	// Second sweep is idempotent (0 swept, no new audits).
	assert.Equal(t, 0, f.srv.sweepOrphanedBoundWebhookEndpoints(ctx))
	audits2, _, err := f.store.ListMutationAudits(ctx, store.MutationAuditFilter{Limit: 200})
	require.NoError(t, err)
	assert.Equal(t, len(audits1), len(audits2))

	// O1 restore race: list -> restore -> delete/unbind leaves both endpoints intact.
	epRaceAgent := mkBoundEP("race-agent-ep", store.DelegationPrincipalUser, f.owner.ID, f.agentA.ID, store.WebhookBoundLifetimeAgent)
	epRacePersist := mkBoundEP("race-persist-ep", store.DelegationPrincipalUser, f.owner.ID, f.agentA.ID, store.WebhookBoundLifetimePersistent)
	orphansBeforeRestore, err := f.store.ListOrphanedBoundWebhookEndpoints(ctx, 100)
	require.NoError(t, err)
	require.Len(t, orphansBeforeRestore, 2)

	deletedAgentA, err := f.store.GetAgent(ctx, f.agentA.ID)
	require.NoError(t, err)
	require.NoError(t, f.srv.restoreAgentTx(ctx, deletedAgentA, AuditActor{}))

	for _, o := range orphansBeforeRestore {
		ag := &store.Agent{ID: o.BoundAgentID, ProjectID: o.ProjectID}
		if o.BoundLifetime == store.WebhookBoundLifetimeAgent {
			assert.False(t, f.srv.deleteAgentLifetimeWebhookEndpoint(ctx, ag, o), "must not delete after restore")
		} else {
			assert.False(t, f.srv.unbindPersistentWebhookEndpoint(ctx, ag, o), "must not unbind after restore")
		}
	}
	_, err = f.store.GetWebhookEndpoint(ctx, epRaceAgent.ID)
	require.NoError(t, err, "agent-lifetime endpoint must survive restore race")
	gotRacePersist, err := f.store.GetWebhookEndpoint(ctx, epRacePersist.ID)
	require.NoError(t, err)
	assert.Equal(t, f.agentA.ID, gotRacePersist.BoundAgentID, "persistent endpoint must remain bound after restore race")
}

// TestWebhookEndpointPatch_AgentLifetimeRequiresDeletePermission verifies R1
// (Review 2): PATCHing an endpoint into bound_lifetime="agent" (new agent or
// persistent/unbound -> agent) requires webhook_endpoint.delete so callers with
// webhook_endpoint.update only (e.g. project-admin) cannot delete endpoints
// indirectly by binding them to an agent they can delete.
func TestWebhookEndpointPatch_AgentLifetimeRequiresDeletePermission(t *testing.T) {
	f := setupTriggersTestFixture(t)
	f.srv.config.SoftDeleteRetention = 72 * time.Hour
	ctx := context.Background()

	adminAgent := &store.Agent{
		ID:        api.NewUUID(),
		Slug:      "admin-agent-r1",
		Name:      "admin-agent-r1",
		ProjectID: f.projectID,
		Phase:     string(state.PhaseRunning),
		CreatedBy: f.admin.ID,
		OwnerID:   f.admin.ID,
		Ancestry:  []string{f.admin.ID},
	}
	require.NoError(t, f.store.CreateAgent(ctx, adminAgent))

	rec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, map[string]any{"name": "owners-hook-r1"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))

	// Baseline: project-admin lacks webhook_endpoint.delete.
	rec = f.doWebhookEndpoint(t, f.adminAuth, created.ID, http.MethodDelete, nil)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	// 1. Admin PATCH binding with boundLifetime=agent -> 403, row unchanged.
	rec = f.doWebhookEndpoint(t, f.adminAuth, created.ID, http.MethodPatch, map[string]any{
		"boundAgentId":  adminAgent.ID,
		"boundLifetime": store.WebhookBoundLifetimeAgent,
	})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	gotAfterRefusal, err := f.store.GetWebhookEndpoint(ctx, created.ID)
	require.NoError(t, err)
	assert.Empty(t, gotAfterRefusal.BoundAgentID)
	assert.Empty(t, gotAfterRefusal.BoundLifetime)

	// Deleting adminAgent does not affect the owner's endpoint.
	delReq := authoredRequest(t, f.adminAuth, http.MethodDelete, "/api/v1/projects/"+f.projectID+"/agents/"+adminAgent.ID, nil)
	delRec := httptest.NewRecorder()
	f.srv.deleteAgent(delRec, delReq, adminAgent.ID)
	require.Equal(t, http.StatusNoContent, delRec.Code, delRec.Body.String())
	_, err = f.store.GetWebhookEndpoint(ctx, created.ID)
	require.NoError(t, err, "owner's endpoint must survive admin agent deletion")

	// 2. Admin PATCH binding with boundLifetime=persistent -> 200 (update only).
	rec = f.doWebhookEndpoint(t, f.adminAuth, created.ID, http.MethodPatch, map[string]any{
		"boundAgentId":  f.agentA.ID,
		"boundLifetime": store.WebhookBoundLifetimePersistent,
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// 3. Admin PATCH upgrading existing persistent binding to lifetime=agent -> 403, stays persistent.
	rec = f.doWebhookEndpoint(t, f.adminAuth, created.ID, http.MethodPatch, map[string]any{
		"boundLifetime": store.WebhookBoundLifetimeAgent,
	})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	gotStillPersist, err := f.store.GetWebhookEndpoint(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, f.agentA.ID, gotStillPersist.BoundAgentID)
	assert.Equal(t, store.WebhookBoundLifetimePersistent, gotStillPersist.BoundLifetime)

	// 4. Owner (holds webhook_endpoint.delete) PATCH lifetime=agent -> 200.
	rec = f.doWebhookEndpoint(t, f.ownerAuth, created.ID, http.MethodPatch, map[string]any{
		"boundLifetime": store.WebhookBoundLifetimeAgent,
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	gotOwnerUpdated, err := f.store.GetWebhookEndpoint(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, f.agentA.ID, gotOwnerUpdated.BoundAgentID)
	assert.Equal(t, store.WebhookBoundLifetimeAgent, gotOwnerUpdated.BoundLifetime)

	// Admin PATCH rebinding an existing agent-lifetime endpoint to another agent -> 403.
	rec = f.doWebhookEndpoint(t, f.adminAuth, created.ID, http.MethodPatch, map[string]any{
		"boundAgentId": f.agentB.ID,
	})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

type patchRaceState struct {
	armed atomic.Bool
	hook  func(tx store.Store)
}

type patchRaceLockStore struct {
	store.Store
	st *patchRaceState
}

func (r *patchRaceLockStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	if r.st.armed.CompareAndSwap(true, false) && r.st.hook != nil {
		// Commit the concurrent owner change BEFORE the admin's transaction
		// opens (after the admin's pre-tx snapshot read), so a rollback of the
		// admin's refused transaction does not roll back the owner's write.
		r.st.hook(r.Store)
	}
	return r.Store.WithTx(ctx, fn)
}

// TestWebhookEndpointPatch_SnapshotRace verifies O-A (Review 3): if the owner
// concurrently downgrades an agent-lifetime endpoint to persistent between an
// admin's pre-transaction snapshot read and LockTriggerQuota, the handler
// re-reads inside the transaction, treats the admin's agent-lifetime PATCH as
// a change requiring webhook_endpoint.delete, refuses with 403, and does not
// revert the owner's persistent downgrade.
func TestWebhookEndpointPatch_SnapshotRace(t *testing.T) {
	srv, s := testServer(t)
	st := &patchRaceState{}
	installStoreFault(t, srv, func(inner store.Store, _ *storeFaultSwitch) *patchRaceLockStore {
		return &patchRaceLockStore{Store: inner, st: st}
	})
	f := setupTriggersTestFixtureOnServer(t, srv, s)
	ctx := context.Background()

	rec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, map[string]any{
		"name":          "patch-race-ep",
		"boundAgentId":  f.agentA.ID,
		"boundLifetime": store.WebhookBoundLifetimeAgent,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var ep WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ep))

	st.hook = func(tx store.Store) {
		cur, err := tx.GetWebhookEndpoint(ctx, ep.ID)
		require.NoError(t, err)
		cur.BoundLifetime = store.WebhookBoundLifetimePersistent
		require.NoError(t, tx.UpdateWebhookEndpoint(ctx, cur, store.WebhookEndpointFieldMask{Binding: true}))
	}
	st.armed.Store(true)

	// Admin (no webhook_endpoint.delete) sends a PATCH that looked like a no-op
	// against the pre-tx snapshot, but is now a persistent->agent upgrade.
	rec = f.doWebhookEndpoint(t, f.adminAuth, ep.ID, http.MethodPatch, map[string]any{
		"boundAgentId":  f.agentA.ID,
		"boundLifetime": store.WebhookBoundLifetimeAgent,
	})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	got, err := s.GetWebhookEndpoint(ctx, ep.ID)
	require.NoError(t, err)
	assert.Equal(t, store.WebhookBoundLifetimePersistent, got.BoundLifetime,
		"owner's concurrent persistent downgrade must not be reverted")
}
