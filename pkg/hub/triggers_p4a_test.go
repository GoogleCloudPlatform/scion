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
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/durablequeue"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestIsWebhookIngressRequest_RoutePosture(t *testing.T) {
	id := uuid.NewString()
	cases := []struct {
		name   string
		method string
		target string
		want   bool
	}{
		{"post_canonical_uuid", http.MethodPost, "/api/v1/hooks/" + id, true},
		{"get_canonical_uuid", http.MethodGet, "/api/v1/hooks/" + id, false},
		{"put_canonical_uuid", http.MethodPut, "/api/v1/hooks/" + id, false},
		{"patch_canonical_uuid", http.MethodPatch, "/api/v1/hooks/" + id, false},
		{"delete_canonical_uuid", http.MethodDelete, "/api/v1/hooks/" + id, false},
		{"post_uppercase_uuid", http.MethodPost, "/api/v1/hooks/" + strings.ToUpper(id), false},
		{"post_subpath", http.MethodPost, "/api/v1/hooks/" + id + "/extra", false},
		{"post_non_uuid_subpath", http.MethodPost, "/api/v1/hooks/x/y", false},
		{"post_encoded_slash", http.MethodPost, "/api/v1/hooks/" + id + "%2Fextra", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.target, nil)
			assert.Equal(t, tc.want, isWebhookIngressRequest(req))
			assert.False(t, isUnauthenticatedEndpoint(req.URL.Path),
				"hooks path must no longer be in isUnauthenticatedEndpoint")
		})
	}
}

func mintTestUserUAT(t *testing.T, srv *Server, s store.Store, user *store.User, projectID string, scopes []string) string {
	t.Helper()
	ctx := context.Background()
	ident := NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, "web")
	ctx = contextWithIdentity(ctx, ident)
	ctx = contextWithAuthType(ctx, AuthTypeJWT)
	ctx = contextWithCredentialContext(ctx, CredentialContext{Kind: CredentialKindInteractive})
	require.NotNil(t, srv.uatService, "uatService must be initialized")
	raw, uat, err := srv.uatService.CreateToken(ctx, user.ID, "p4a-test-uat-"+uuid.NewString()[:8], projectID, scopes, nil)
	require.NoError(t, err)
	require.NotNil(t, uat)
	return raw
}

func mintTestUserSessionJWT(t *testing.T, srv *Server, user *store.User) string {
	t.Helper()
	require.NotNil(t, srv.userTokenService, "userTokenService must be initialized")
	tok, _, err := srv.userTokenService.MintAccessToken(UserTokenSubject{
		UserID:            user.ID,
		Email:             user.Email,
		DisplayName:       user.DisplayName,
		Role:              user.Role,
		SessionGeneration: user.SessionGeneration,
		ClientType:        ClientTypeWeb,
	})
	require.NoError(t, err)
	return tok
}

func mintTestAgentJWT(t *testing.T, srv *Server, ag *store.Agent) string {
	t.Helper()
	require.NotNil(t, srv.agentTokenService, "agentTokenService must be initialized")
	tok, err := srv.GenerateAgentToken(ag.ID, ag.ProjectID, nil, AgentRoleBaseline, nil)
	require.NoError(t, err)
	return tok
}

func TestWebhookIngress_HubTokenAuthMatrix(t *testing.T) {
	f := setupTriggersTestFixture(t)
	ctx := context.Background()

	// Seed a bound agent, an owner agent, another same-project agent, and an
	// other-project agent.
	seedAgent := func(projID, slug string) *store.Agent {
		ag := &store.Agent{
			ID:        api.NewUUID(),
			Slug:      slug,
			Name:      slug,
			ProjectID: projID,
			Phase:     "running",
			OwnerID:   f.owner.ID,
		}
		require.NoError(t, f.store.CreateAgent(ctx, ag))
		return ag
	}
	boundAg := seedAgent(f.projectID, "p4a-bound-ag")
	otherSameProjAg := seedAgent(f.projectID, "p4a-peer-ag")
	otherProjAg := seedAgent(f.otherProjID, "p4a-foreign-ag")

	// Create a hub_token endpoint bound to boundAg (allowProjectAgents=false).
	epID := api.NewUUID()
	ep := &store.WebhookEndpoint{
		ID:                  epID,
		ProjectID:           f.projectID,
		Name:                "hub-token-ep",
		OwnerPrincipalKind:  store.DelegationPrincipalUser,
		OwnerPrincipalID:    f.owner.ID,
		OwnerCredentialKind: "api",
		BoundAgentID:        boundAg.ID,
		AuthMode:            store.WebhookAuthModeHubToken,
		AllowProjectAgents:  false,
		MaxBodyBytes:        store.DefaultWebhookMaxBodyBytes,
		ReplayWindowSeconds: store.DefaultWebhookReplayWindowSeconds,
		ConfigVersion:       1,
		SecretVersion:       1,
		SignatureMode:       store.WebhookSignatureModeScionV1,
		Status:              store.WebhookEndpointStatusActive,
		CreatedBy:           f.owner.ID,
	}
	require.NoError(t, f.store.CreateWebhookEndpoint(ctx, ep))

	// Also create a hub_token endpoint owned by boundAg to verify owner-agent access.
	ownedEpID := api.NewUUID()
	require.NoError(t, f.store.CreateWebhookEndpoint(ctx, &store.WebhookEndpoint{
		ID:                  ownedEpID,
		ProjectID:           f.projectID,
		Name:                "hub-token-owned-ep",
		OwnerPrincipalKind:  store.DelegationPrincipalAgent,
		OwnerPrincipalID:    boundAg.ID,
		OwnerCredentialKind: "agent_jwt",
		AuthMode:            store.WebhookAuthModeHubToken,
		MaxBodyBytes:        store.DefaultWebhookMaxBodyBytes,
		ReplayWindowSeconds: store.DefaultWebhookReplayWindowSeconds,
		ConfigVersion:       1,
		SecretVersion:       1,
		SignatureMode:       store.WebhookSignatureModeScionV1,
		Status:              store.WebhookEndpointStatusActive,
		CreatedBy:           boundAg.ID,
	}))

	postHook := func(endpointID, authHeader, agentTokenHeader, delivery string, body []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/"+endpointID, bytes.NewReader(body))
		req.RemoteAddr = "198.51.100.20:44321"
		req.Header.Set("Content-Type", "application/json")
		if authHeader != "" {
			req.Header.Set("Authorization", authHeader)
		}
		if agentTokenHeader != "" {
			req.Header.Set("X-Scion-Agent-Token", agentTokenHeader)
		}
		if delivery != "" {
			req.Header.Set(trigger.HeaderDelivery, delivery)
		}
		rec := httptest.NewRecorder()
		f.srv.Handler().ServeHTTP(rec, req)
		return rec
	}

	payload := []byte(`{"event":"ping"}`)

	// 1. Anonymous (no credential) -> 401 content-free.
	recAnon := postHook(epID, "", "", "ht-anon", payload)
	assert.Equal(t, http.StatusUnauthorized, recAnon.Code)

	// 2. Invalid bearer -> 401 before handler.
	recBadBearer := postHook(epID, "Bearer scion_pat_invalid_garbage_token", "", "ht-bad-bearer", payload)
	assert.Equal(t, http.StatusUnauthorized, recBadBearer.Code)

	// 3. Valid UAT WITHOUT webhook_endpoint:send -> 403.
	uatNoSend := mintTestUserUAT(t, f.srv, f.store, f.member, f.projectID, []string{"webhook_endpoint:read"})
	recNoSend := postHook(epID, "Bearer "+uatNoSend, "", "ht-uat-nosend", payload)
	assert.Equal(t, http.StatusForbidden, recNoSend.Code)

	// 4. Valid UAT WITH webhook_endpoint:send on WRONG project boundary -> 403.
	uatWrongProj := mintTestUserUAT(t, f.srv, f.store, f.owner, f.otherProjID, []string{"webhook_endpoint:send"})
	recWrongProj := postHook(epID, "Bearer "+uatWrongProj, "", "ht-uat-wrongproj", payload)
	assert.Equal(t, http.StatusForbidden, recWrongProj.Code)

	// 5. Valid UAT WITH webhook_endpoint:send on matching project -> 202 queued.
	uatSend := mintTestUserUAT(t, f.srv, f.store, f.member, f.projectID, []string{"webhook_endpoint:send"})
	recUATOK := postHook(epID, "Bearer "+uatSend, "", "ht-uat-ok", payload)
	require.Equal(t, http.StatusAccepted, recUATOK.Code, recUATOK.Body.String())
	var accUAT trigger.IngressAcceptedResponse
	require.NoError(t, json.Unmarshal(recUATOK.Body.Bytes(), &accUAT))
	assert.Equal(t, trigger.IngressStatusQueued, accUAT.Status)

	storedEv, err := f.store.GetTriggerEvent(ctx, accUAT.EventID)
	require.NoError(t, err)
	assert.Equal(t, store.WebhookAuthModeHubToken, storedEv.Attributes["signature_mode"])
	assert.Equal(t, store.DelegationPrincipalUser+":"+f.member.ID, storedEv.Attributes["sender"])
	assert.Empty(t, storedEv.Headers["Authorization"], "Authorization header must never be stored")

	// 5b. Duplicate X-Scion-Delivery on hub_token endpoint -> 202 duplicate.
	recDup := postHook(epID, "Bearer "+uatSend, "", "ht-uat-ok", payload)
	require.Equal(t, http.StatusAccepted, recDup.Code)
	var accDup trigger.IngressAcceptedResponse
	require.NoError(t, json.Unmarshal(recDup.Body.Bytes(), &accDup))
	assert.Equal(t, trigger.IngressStatusDuplicate, accDup.Status)
	assert.Equal(t, accUAT.EventID, accDup.EventID)

	// 6. Member session bearer -> 202 queued; viewer session bearer -> 403.
	memberJWT := mintTestUserSessionJWT(t, f.srv, f.member)
	recMember := postHook(epID, "Bearer "+memberJWT, "", "ht-member-jwt", payload)
	require.Equal(t, http.StatusAccepted, recMember.Code, recMember.Body.String())

	viewerJWT := mintTestUserSessionJWT(t, f.srv, f.viewer)
	recViewer := postHook(epID, "Bearer "+viewerJWT, "", "ht-viewer-jwt", payload)
	assert.Equal(t, http.StatusForbidden, recViewer.Code)

	// 7. Bound agent's own token -> 202 queued.
	boundTok := mintTestAgentJWT(t, f.srv, boundAg)
	recBound := postHook(epID, "", boundTok, "ht-bound-ag", payload)
	require.Equal(t, http.StatusAccepted, recBound.Code, recBound.Body.String())
	var accBound trigger.IngressAcceptedResponse
	require.NoError(t, json.Unmarshal(recBound.Body.Bytes(), &accBound))
	evBound, err := f.store.GetTriggerEvent(ctx, accBound.EventID)
	require.NoError(t, err)
	assert.Equal(t, store.DelegationPrincipalAgent+":"+boundAg.ID, evBound.Attributes["sender"])

	// 7b. Owner agent's own token -> 202 queued.
	recOwnerAg := postHook(ownedEpID, "", boundTok, "ht-owner-ag", payload)
	require.Equal(t, http.StatusAccepted, recOwnerAg.Code, recOwnerAg.Body.String())

	// 8. Other agent in SAME project when allowProjectAgents=false -> 403.
	peerTok := mintTestAgentJWT(t, f.srv, otherSameProjAg)
	recPeerDenied := postHook(epID, "", peerTok, "ht-peer-denied", payload)
	assert.Equal(t, http.StatusForbidden, recPeerDenied.Code)

	// 9. Enable allowProjectAgents via PATCH -> same-project peer agent now gets 202,
	//    while other-project agent is STILL refused 403!
	patchRec := f.doWebhookEndpoint(t, f.ownerAuth, epID, http.MethodPatch, map[string]any{
		"allowProjectAgents": true,
	})
	require.Equal(t, http.StatusOK, patchRec.Code, patchRec.Body.String())

	recPeerAllowed := postHook(epID, "", peerTok, "ht-peer-allowed", payload)
	require.Equal(t, http.StatusAccepted, recPeerAllowed.Code, recPeerAllowed.Body.String())

	foreignTok := mintTestAgentJWT(t, f.srv, otherProjAg)
	recForeign := postHook(epID, "", foreignTok, "ht-foreign-denied", payload)
	assert.Equal(t, http.StatusForbidden, recForeign.Code, "other-project agent must be refused even when allowProjectAgents=true")

	// 10. Denied credentials charge the endpoint's rate bucket (R2).
	now := time.Date(2026, 11, 23, 15, 0, 0, 0, time.UTC)
	tightEpID := api.NewUUID()
	require.NoError(t, f.store.CreateWebhookEndpoint(ctx, &store.WebhookEndpoint{
		ID:                  tightEpID,
		ProjectID:           f.projectID,
		Name:                "hub-token-tight-rate",
		OwnerPrincipalKind:  store.DelegationPrincipalUser,
		OwnerPrincipalID:    f.owner.ID,
		OwnerCredentialKind: "api",
		AuthMode:            store.WebhookAuthModeHubToken,
		RatePerSecond:       1,
		RateBurst:           2,
		MaxBodyBytes:        store.DefaultWebhookMaxBodyBytes,
		ReplayWindowSeconds: store.DefaultWebhookReplayWindowSeconds,
		ConfigVersion:       1,
		SecretVersion:       1,
		SignatureMode:       store.WebhookSignatureModeScionV1,
		Status:              store.WebhookEndpointStatusActive,
		CreatedBy:           f.owner.ID,
	}))
	dqs := f.store.(store.DurableQueueStore)
	f.srv.webhookIngress = trigger.NewIngress(trigger.IngressConfig{
		Store:             f.store,
		QueueStore:        dqs,
		Queue:             f.srv.durableQueue,
		LoadSecret:        f.srv.loadWebhookEndpointSecret,
		AuthorizeHubToken: f.srv.authorizeWebhookIngressHubToken,
		Enabled:           func() bool { return f.srv.experimentEnabled(experiments.Triggers) },
		Limiter: trigger.NewIngressLimiter(trigger.LimiterConfig{
			Now: func() time.Time { return now },
		}),
		Now: func() time.Time { return now },
	})
	// 2 denied requests consume the burst of 2; 3rd request hits 429!
	assert.Equal(t, http.StatusForbidden, postHook(tightEpID, "Bearer "+uatNoSend, "", "r1", payload).Code)
	assert.Equal(t, http.StatusForbidden, postHook(tightEpID, "Bearer "+uatNoSend, "", "r2", payload).Code)
	rec429 := postHook(tightEpID, "Bearer "+uatSend, "", "r3", payload)
	assert.Equal(t, http.StatusTooManyRequests, rec429.Code, "denied credentials must charge endpoint rate bucket")
}

func TestWebhookIngress_ReplayWindowAndBodyRateLimits(t *testing.T) {
	f := setupTriggersTestFixture(t)
	now := time.Date(2026, 11, 23, 16, 0, 0, 0, time.UTC)

	dqs := f.store.(store.DurableQueueStore)
	f.srv.webhookIngress = trigger.NewIngress(trigger.IngressConfig{
		Store:             f.store,
		QueueStore:        dqs,
		Queue:             f.srv.durableQueue,
		LoadSecret:        f.srv.loadWebhookEndpointSecret,
		AuthorizeHubToken: f.srv.authorizeWebhookIngressHubToken,
		Enabled:           func() bool { return f.srv.experimentEnabled(experiments.Triggers) },
		Limiter: trigger.NewIngressLimiter(trigger.LimiterConfig{
			Now: func() time.Time { return now },
		}),
		Now: func() time.Time { return now },
	})

	// Create endpoint with replayWindowSeconds=60, maxBodyBytes=256, rate=1/burst=2.
	recCreate := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{
		Name:                "bounded-ep",
		ReplayWindowSeconds: 60,
		MaxBodyBytes:        256,
		RateLimit:           &WebhookEndpointRateLimitSpec{PerSecond: 1, Burst: 2},
	})
	require.Equal(t, http.StatusCreated, recCreate.Code, recCreate.Body.String())
	var ep WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(recCreate.Body.Bytes(), &ep))
	assert.Equal(t, 60, ep.ReplayWindowSeconds)
	assert.Equal(t, 256, ep.MaxBodyBytes)
	require.NotNil(t, ep.RateLimit)
	assert.Equal(t, 1, ep.RateLimit.PerSecond)
	assert.Equal(t, 2, ep.RateLimit.Burst)

	// 1. Body > 256 bytes -> 413 (consumes 1 burst token).
	overBody := []byte(`{"p":"` + strings.Repeat("x", 260) + `"}`)
	rec413 := sendSignedHookHTTP(f.srv, ep.ID, ep.Secret, now.Unix(), "d-over", "", "", "application/json", "", nil, overBody)
	assert.Equal(t, http.StatusRequestEntityTooLarge, rec413.Code)

	// 2. Timestamp 65s old -> 401 under 60s replay window (consumes 2nd burst token).
	smallBody := []byte(`{"ok":true}`)
	recStale := sendSignedHookHTTP(f.srv, ep.ID, ep.Secret, now.Unix()-65, "d-stale", "", "", "application/json", "", nil, smallBody)
	assert.Equal(t, http.StatusUnauthorized, recStale.Code)

	// 3. Burst of 2 is exhausted -> 3rd request at same timestamp gets 429!
	rec429 := sendSignedHookHTTP(f.srv, ep.ID, ep.Secret, now.Unix()-55, "d-rate", "", "", "application/json", "", nil, smallBody)
	assert.Equal(t, http.StatusTooManyRequests, rec429.Code)

	// 4. Advance clock by 2s (refills 2 tokens); timestamp 55s old succeeds 202!
	now = now.Add(2 * time.Second)
	recOK := sendSignedHookHTTP(f.srv, ep.ID, ep.Secret, now.Unix()-55, "d-ok", "", "", "application/json", "", nil, smallBody)
	assert.Equal(t, http.StatusAccepted, recOK.Code, recOK.Body.String())
}

func TestWebhookEndpointPatch_ReplayProtectionAndLimits(t *testing.T) {
	f := setupTriggersTestFixture(t)
	ctx := context.Background()

	// Seed a hub_token endpoint whose signatureMode is body_only (so it IS
	// replay-protected only while auth_mode == hub_token).
	epID := api.NewUUID()
	require.NoError(t, f.store.CreateWebhookEndpoint(ctx, &store.WebhookEndpoint{
		ID:                  epID,
		ProjectID:           f.projectID,
		Name:                "patch-replay-ep",
		OwnerPrincipalKind:  store.DelegationPrincipalUser,
		OwnerPrincipalID:    f.owner.ID,
		OwnerCredentialKind: "api",
		AuthMode:            store.WebhookAuthModeHubToken,
		SignatureMode:       store.WebhookSignatureModeBodyOnly,
		MaxBodyBytes:        store.DefaultWebhookMaxBodyBytes,
		ReplayWindowSeconds: store.DefaultWebhookReplayWindowSeconds,
		ConfigVersion:       1,
		SecretVersion:       1,
		Status:              store.WebhookEndpointStatusActive,
		CreatedBy:           f.owner.ID,
	}))

	// Attach an active dispatch_agent trigger to this hub_token endpoint.
	createTrRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "dispatch-on-hub-token",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: epID},
		Action: TriggerActionSpec{
			Type:     store.TriggerActionDispatchAgent,
			Template: "default",
			Task:     "Handle event ${event.id}",
		},
		MaxFiresPerMinute:       10,
		MaxConcurrentDispatches: 3,
	})
	require.Equal(t, http.StatusCreated, createTrRec.Code, createTrRec.Body.String())
	var trResp TriggerResponse
	require.NoError(t, json.Unmarshal(createTrRec.Body.Bytes(), &trResp))
	assert.Equal(t, 10, trResp.MaxFiresPerMinute)
	assert.Equal(t, 3, trResp.MaxConcurrentDispatches)

	// 1. PATCH auth from hub_token -> hmac while signatureMode=body_only and an
	//    active dispatch_agent trigger exists -> 409 replay_protected_triggers_exist.
	patchConflict := f.doWebhookEndpoint(t, f.ownerAuth, epID, http.MethodPatch, map[string]any{
		"auth": store.WebhookAuthModeHMAC,
	})
	assert.Equal(t, http.StatusConflict, patchConflict.Code, patchConflict.Body.String())
	assert.Contains(t, patchConflict.Body.String(), "replay_protected_triggers_exist")

	// 2. Pause the dispatch_agent trigger -> PATCH now succeeds (200),
	//    and attempting to resume the trigger is refused 400 source_not_replay_protected.
	pauseRec := f.doTrigger(t, f.ownerAuth, trResp.ID+"/pause", http.MethodPost, nil)
	require.Equal(t, http.StatusOK, pauseRec.Code, pauseRec.Body.String())

	patchOK := f.doWebhookEndpoint(t, f.ownerAuth, epID, http.MethodPatch, map[string]any{
		"auth":                store.WebhookAuthModeHMAC,
		"replayWindowSeconds": 120,
		"maxBodyBytes":        32768,
		"rateLimit":           map[string]any{"perSecond": 5, "burst": 20},
	})
	require.Equal(t, http.StatusOK, patchOK.Code, patchOK.Body.String())
	var updatedEP WebhookEndpointResponse
	require.NoError(t, json.Unmarshal(patchOK.Body.Bytes(), &updatedEP))
	assert.Equal(t, store.WebhookAuthModeHMAC, updatedEP.AuthMode)
	assert.Equal(t, 120, updatedEP.ReplayWindowSeconds)
	assert.Equal(t, 32768, updatedEP.MaxBodyBytes)
	assert.GreaterOrEqual(t, updatedEP.ConfigVersion, 2)

	resumeRefused := f.doTrigger(t, f.ownerAuth, trResp.ID+"/resume", http.MethodPost, nil)
	assert.Equal(t, http.StatusBadRequest, resumeRefused.Code, resumeRefused.Body.String())
	assert.Contains(t, resumeRefused.Body.String(), "source_not_replay_protected")

	// 3. Constants cannot be exceeded on create or PATCH (400).
	badEpCases := []struct {
		name string
		body map[string]any
	}{
		{"body_exceeds_max", map[string]any{"maxBodyBytes": trigger.MaxEndpointBodyBytes + 1}},
		{"body_negative", map[string]any{"maxBodyBytes": -1}},
		{"rate_exceeds_max", map[string]any{"rateLimit": map[string]any{"perSecond": int(trigger.PerEndpointRatePerSecond) + 1}}},
		{"burst_exceeds_max", map[string]any{"rateLimit": map[string]any{"burst": trigger.PerEndpointBurst + 1}}},
		{"replay_too_small", map[string]any{"replayWindowSeconds": 10}},
		{"replay_too_large", map[string]any{"replayWindowSeconds": 901}},
		{"invalid_auth", map[string]any{"auth": "magic"}},
	}
	for _, tc := range badEpCases {
		t.Run("ep_patch_"+tc.name, func(t *testing.T) {
			rec := f.doWebhookEndpoint(t, f.ownerAuth, epID, http.MethodPatch, tc.body)
			assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		})
	}

	// Trigger limit bounds (400).
	badTrCases := []struct {
		name string
		body map[string]any
	}{
		{"fires_per_min_exceeds_600", map[string]any{"maxFiresPerMinute": store.MaxTriggerFiresPerMinute + 1}},
		{"concurrent_exceeds_50", map[string]any{"maxConcurrentDispatches": store.MaxTriggerConcurrentDispatches + 1}},
		{"fires_per_min_negative", map[string]any{"maxFiresPerMinute": -1}},
	}
	for _, tc := range badTrCases {
		t.Run("tr_patch_"+tc.name, func(t *testing.T) {
			rec := f.doTrigger(t, f.ownerAuth, trResp.ID, http.MethodPatch, tc.body)
			assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		})
	}

	// maxConcurrentDispatches on a message trigger -> 400.
	msgRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:                    "msg-with-conc",
		Source:                  TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: epID},
		Action:                  TriggerActionSpec{Type: store.TriggerActionMessage, AgentID: f.targetAgent.ID, Message: "hi"},
		MaxConcurrentDispatches: 2,
	})
	assert.Equal(t, http.StatusBadRequest, msgRec.Code, msgRec.Body.String())

	// 4. Authz on PATCH /webhook-endpoints/{id} (reconciled with P3):
	//    - Unrelated agent caller on user-owned endpoint -> 404 (hidden).
	//    - Owner agent caller on its own endpoint -> 200 for non-binding fields.
	//    - UAT without webhook_endpoint:update -> 403.
	//    - UAT with webhook_endpoint:update -> 200.
	recAgentPatch := f.doWebhookEndpoint(t, f.agentAAuth, epID, http.MethodPatch, map[string]any{"description": "nope"})
	assert.Equal(t, http.StatusNotFound, recAgentPatch.Code)

	recAgentCreate := f.doWebhookEndpoint(t, f.agentAAuth, "", http.MethodPost, map[string]any{"name": "agent-a-own-ep"})
	require.Equal(t, http.StatusCreated, recAgentCreate.Code, recAgentCreate.Body.String())
	var agentEp WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(recAgentCreate.Body.Bytes(), &agentEp))
	recAgentOwnPatch := f.doWebhookEndpoint(t, f.agentAAuth, agentEp.ID, http.MethodPatch, map[string]any{"description": "agent updated"})
	assert.Equal(t, http.StatusOK, recAgentOwnPatch.Code, recAgentOwnPatch.Body.String())

	projBoundary := TokenBoundary{Kind: BoundaryKindProject, ProjectID: f.projectID}
	uatNoUpdate := scheduleTestUAT(t, f.ownerAuth, projBoundary, "webhook_endpoint:read")
	recNoUpdate := f.doWebhookEndpoint(t, uatNoUpdate, epID, http.MethodPatch, map[string]any{"description": "nope"})
	assert.Equal(t, http.StatusForbidden, recNoUpdate.Code)

	uatWithUpdate := scheduleTestUAT(t, f.ownerAuth, projBoundary, "webhook_endpoint:update")
	recWithUpdate := f.doWebhookEndpoint(t, uatWithUpdate, epID, http.MethodPatch, map[string]any{"description": "updated"})
	assert.Equal(t, http.StatusOK, recWithUpdate.Code, recWithUpdate.Body.String())
}

func TestTriggerDispatchAgent_M1MissingDeliveryIDRefused(t *testing.T) {
	ctx := context.Background()
	f := newTriggerDispatchAgentFixture(t, "m1-delivery")
	userCtx := f.userAuth(t, f.sched.creator)

	// Create scion_v1 endpoint and a hub_token endpoint.
	hmacEp := f.createEndpoint(t, userCtx, "m1-hmac-ep")
	recHT := f.doJSON(t, userCtx, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/webhook-endpoints", f.sched.proj.ID),
		map[string]any{"name": "m1-hubtoken-ep", "auth": store.WebhookAuthModeHubToken})
	require.Equal(t, http.StatusCreated, recHT.Code, recHT.Body.String())
	var htEp WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(recHT.Body.Bytes(), &htEp))

	// Create dispatch_agent trigger on hmacEp.
	recTr := f.doJSON(t, userCtx, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/triggers", f.sched.proj.ID),
		CreateTriggerRequest{
			Name:   "m1-dispatch-hmac",
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: hmacEp.ID},
			Action: TriggerActionSpec{
				Type:     store.TriggerActionDispatchAgent,
				Template: "default",
				Task:     "Work on ${event.id}",
			},
		})
	require.Equal(t, http.StatusCreated, recTr.Code, recTr.Body.String())
	var trHMAC TriggerResponse
	require.NoError(t, json.Unmarshal(recTr.Body.Bytes(), &trHMAC))

	// Create dispatch_agent trigger on htEp.
	recTrHT := f.doJSON(t, userCtx, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/triggers", f.sched.proj.ID),
		CreateTriggerRequest{
			Name:   "m1-dispatch-hubtoken",
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: htEp.ID},
			Action: TriggerActionSpec{
				Type:     store.TriggerActionDispatchAgent,
				Template: "default",
				Task:     "Work on ${event.id}",
			},
		})
	require.Equal(t, http.StatusCreated, recTrHT.Code, recTrHT.Body.String())
	var trHT TriggerResponse
	require.NoError(t, json.Unmarshal(recTrHT.Body.Bytes(), &trHT))

	// 1. scion_v1 delivery WITHOUT X-Scion-Delivery -> fire is REFUSED ("missing delivery id").
	recNoDeliv := f.postSignedWebhook(t, hmacEp, "", "", "", []byte(`{"x":1}`))
	require.Equal(t, http.StatusAccepted, recNoDeliv.Code, recNoDeliv.Body.String())
	f.drainOneTriggerEvent(t)

	fires1, err := f.sched.store.ListTriggerFires(ctx, store.TriggerFireFilter{TriggerID: trHMAC.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, fires1.Items, 1)
	assert.Equal(t, store.TriggerFireStatusRefused, fires1.Items[0].Status)
	assert.Equal(t, "missing delivery id", fires1.Items[0].Error)

	// 2. scion_v1 delivery WITH X-Scion-Delivery -> fire SUCCEEDS, and replaying
	//    the exact request is deduplicated 202 "duplicate" at ingress!
	recWithDeliv := f.postSignedWebhook(t, hmacEp, "m1-deliv-1", "", "", []byte(`{"x":2}`))
	require.Equal(t, http.StatusAccepted, recWithDeliv.Code, recWithDeliv.Body.String())
	recReplay := f.postSignedWebhook(t, hmacEp, "m1-deliv-1", "", "", []byte(`{"x":2}`))
	require.Equal(t, http.StatusAccepted, recReplay.Code)
	var replayResp trigger.IngressAcceptedResponse
	require.NoError(t, json.Unmarshal(recReplay.Body.Bytes(), &replayResp))
	assert.Equal(t, trigger.IngressStatusDuplicate, replayResp.Status)

	f.drainOneTriggerEvent(t)
	fires2, err := f.sched.store.ListTriggerFires(ctx, store.TriggerFireFilter{TriggerID: trHMAC.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, fires2.Items, 2)
	assert.Equal(t, store.TriggerFireStatusFired, fires2.Items[0].Status)

	// 3. hub_token delivery WITHOUT X-Scion-Delivery -> fire SUCCEEDS (authenticated
	//    live credential, verified="hub-token").
	reqHT := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/"+htEp.ID, bytes.NewReader([]byte(`{"x":3}`)))
	reqHT.Header.Set("Content-Type", "application/json")
	reqHT = reqHT.WithContext(userCtx)
	recHTPost := httptest.NewRecorder()
	f.sched.srv.mux.ServeHTTP(recHTPost, reqHT)
	require.Equal(t, http.StatusAccepted, recHTPost.Code, recHTPost.Body.String())

	f.drainOneTriggerEvent(t)
	firesHT, err := f.sched.store.ListTriggerFires(ctx, store.TriggerFireFilter{TriggerID: trHT.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, firesHT.Items, 1)
	assert.Equal(t, store.TriggerFireStatusFired, firesHT.Items[0].Status)

	// Confirm the dispatched agent received ExternalVerifiedHubToken in its home file.
	f.client.mu.Lock()
	require.NotEmpty(t, f.client.calls)
	lastReq := f.client.calls[len(f.client.calls)-1]
	f.client.mu.Unlock()
	require.NotEmpty(t, lastReq.HomeFiles)
	var body messaging.EventBody
	require.NoError(t, json.Unmarshal(lastReq.HomeFiles[0].Content, &body))
	require.NotNil(t, body.External)
	assert.Equal(t, messaging.ExternalVerifiedHubToken, body.External.Verified)
}

func TestTriggerLimits_RateConcurrencyAndProjectCap(t *testing.T) {
	ctx := context.Background()
	f := newTriggerDispatchAgentFixture(t, "limits-p4a")
	userCtx := f.userAuth(t, f.sched.creator)
	ep := f.createEndpoint(t, userCtx, "limits-ep")

	drainWithMetrics := func(t *testing.T) {
		t.Helper()
		disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
			Store:   f.sched.store,
			Core:    serverTriggerCore{srv: f.sched.srv},
			Rules:   f.sched.srv.TriggerRules(),
			Metrics: serverTriggerMetrics{srv: f.sched.srv},
		})
		require.NoError(t, err)
		dqStore := f.sched.store.(store.DurableQueueStore)
		now := time.Now().UTC()
		rows, err := dqStore.ClaimDurableItems(ctx, trigger.EventQueueKind, "test-owner", now, now.Add(time.Minute), 20)
		require.NoError(t, err)
		require.NotEmpty(t, rows)
		for _, r := range rows {
			it := durablequeue.Item{
				Seq:       r.Seq,
				Kind:      r.Kind,
				DedupKey:  r.DedupKey,
				ProjectID: r.ProjectID,
				Payload:   []byte(r.Payload),
				Attempts:  r.Attempts,
			}
			res := disp.HandleItem(ctx, it, nil)
			require.True(t, res.Done, "err=%v", res.Err)
			ok, cErr := dqStore.CompleteDurableItem(ctx, r.Seq, "test-owner")
			require.NoError(t, cErr)
			require.True(t, ok)
		}
	}

	// 1. Per-trigger max_fires_per_minute = 2: 2 fires succeed, 3rd is skipped "rate limit", errorCount == 0.
	recRateTr := f.doJSON(t, userCtx, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/triggers", f.sched.proj.ID),
		CreateTriggerRequest{
			Name:   "rate-limited-tr",
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
			Match:  &TriggerMatchSpec{Types: []string{"webhook.rate"}},
			Action: TriggerActionSpec{
				Type:     store.TriggerActionDispatchAgent,
				Template: "default",
				Task:     "Rate test ${event.id}",
			},
			MaxFiresPerMinute:       2,
			MaxConcurrentDispatches: 10,
		})
	require.Equal(t, http.StatusCreated, recRateTr.Code, recRateTr.Body.String())
	var rateTr TriggerResponse
	require.NoError(t, json.Unmarshal(recRateTr.Body.Bytes(), &rateTr))

	for i := 1; i <= 3; i++ {
		rec := f.postSignedWebhook(t, ep, fmt.Sprintf("rate-deliv-%d", i), "rate", "", []byte(`{"n":1}`))
		require.Equal(t, http.StatusAccepted, rec.Code)
		drainWithMetrics(t)
	}
	rateFires, err := f.sched.store.ListTriggerFires(ctx, store.TriggerFireFilter{TriggerID: rateTr.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, rateFires.Items, 3)
	// Most recent fire (index 0) is skipped "rate limit".
	assert.Equal(t, store.TriggerFireStatusSkipped, rateFires.Items[0].Status)
	assert.Equal(t, trigger.ErrFireRateLimit, rateFires.Items[0].Error)
	gotRateTr, err := f.sched.store.GetTrigger(ctx, rateTr.ID)
	require.NoError(t, err)
	assert.Zero(t, gotRateTr.ErrorCount, "skipped fires must never increment errorCount")

	// 2. Per-trigger max_concurrent_dispatches = 2:
	//    Seed 2 live agents (running, provisioning) + 1 stopped + 1 soft-deleted
	//    with label trigger=<concTr.ID>. Live count is 2 -> next fire is skipped "concurrency limit".
	recConcTr := f.doJSON(t, userCtx, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/triggers", f.sched.proj.ID),
		CreateTriggerRequest{
			Name:   "conc-limited-tr",
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
			Match:  &TriggerMatchSpec{Types: []string{"webhook.conc"}},
			Action: TriggerActionSpec{
				Type:     store.TriggerActionDispatchAgent,
				Template: "default",
				Task:     "Conc test ${event.id}",
			},
			MaxConcurrentDispatches: 2,
		})
	require.Equal(t, http.StatusCreated, recConcTr.Code, recConcTr.Body.String())
	var concTr TriggerResponse
	require.NoError(t, json.Unmarshal(recConcTr.Body.Bytes(), &concTr))

	seedLabeledAgent := func(slug, phase string, deleted bool, labels map[string]string) {
		ag := &store.Agent{
			ID:        api.NewUUID(),
			Slug:      slug,
			Name:      slug,
			ProjectID: f.sched.proj.ID,
			Phase:     phase,
			OwnerID:   f.sched.creator.ID,
			Labels:    labels,
		}
		require.NoError(t, f.sched.store.CreateAgent(ctx, ag))
		if deleted {
			ag.DeletedAt = time.Now().UTC()
			require.NoError(t, f.sched.store.UpdateAgent(ctx, ag))
		}
	}
	seedLabeledAgent("conc-live-1", "running", false, map[string]string{"trigger": concTr.ID})
	seedLabeledAgent("conc-live-2", "provisioning", false, map[string]string{"trigger": concTr.ID})
	seedLabeledAgent("conc-stopped", "stopped", false, map[string]string{"trigger": concTr.ID})
	seedLabeledAgent("conc-deleted", "running", true, map[string]string{"trigger": concTr.ID})

	recConcPost := f.postSignedWebhook(t, ep, "conc-deliv-1", "conc", "", []byte(`{"n":1}`))
	require.Equal(t, http.StatusAccepted, recConcPost.Code)
	drainWithMetrics(t)

	concFires, err := f.sched.store.ListTriggerFires(ctx, store.TriggerFireFilter{TriggerID: concTr.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, concFires.Items, 1)
	assert.Equal(t, store.TriggerFireStatusSkipped, concFires.Items[0].Status)
	assert.Equal(t, trigger.ErrFireConcurrencyLimit, concFires.Items[0].Error)
	gotConcTr, err := f.sched.store.GetTrigger(ctx, concTr.ID)
	require.NoError(t, err)
	assert.Zero(t, gotConcTr.ErrorCount)

	// 3. Per-project live trigger-agent cap (25):
	//    Create a third trigger with high concurrency (50) and seed enough live
	//    trigger-labeled agents across the project to reach 25.
	recCapTr := f.doJSON(t, userCtx, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/triggers", f.sched.proj.ID),
		CreateTriggerRequest{
			Name:   "proj-cap-tr",
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
			Match:  &TriggerMatchSpec{Types: []string{"webhook.cap"}},
			Action: TriggerActionSpec{
				Type:     store.TriggerActionDispatchAgent,
				Template: "default",
				Task:     "Cap test ${event.id}",
			},
			MaxConcurrentDispatches: 50,
		})
	require.Equal(t, http.StatusCreated, recCapTr.Code, recCapTr.Body.String())
	var capTr TriggerResponse
	require.NoError(t, json.Unmarshal(recCapTr.Body.Bytes(), &capTr))

	// Seed 25 live trigger-labeled agents (plus 1 non-trigger agent that must not count).
	for i := 0; i < store.MaxLiveTriggerAgentsPerProject; i++ {
		seedLabeledAgent(fmt.Sprintf("cap-live-%d", i), "running", false, map[string]string{"trigger": "other-trigger-id"})
	}
	seedLabeledAgent("cap-non-trigger", "running", false, map[string]string{"env": "prod"})

	recCapPost1 := f.postSignedWebhook(t, ep, "cap-deliv-1", "cap", "", []byte(`{"n":1}`))
	require.Equal(t, http.StatusAccepted, recCapPost1.Code)
	drainWithMetrics(t)

	recCapPost2 := f.postSignedWebhook(t, ep, "cap-deliv-2", "cap", "", []byte(`{"n":2}`))
	require.Equal(t, http.StatusAccepted, recCapPost2.Code)
	drainWithMetrics(t)

	capFires, err := f.sched.store.ListTriggerFires(ctx, store.TriggerFireFilter{TriggerID: capTr.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, capFires.Items, 2)
	assert.Equal(t, store.TriggerFireStatusSkipped, capFires.Items[0].Status)
	assert.Equal(t, trigger.ErrFireProjectTriggerAgentCap, capFires.Items[0].Error)
	gotCapTr, err := f.sched.store.GetTrigger(ctx, capTr.ID)
	require.NoError(t, err)
	assert.Zero(t, gotCapTr.ErrorCount)

	// Verify once-per-hour notification cooldown recorded capTr.ID.
	f.sched.srv.triggers.mu.Lock()
	_, notified := f.sched.srv.triggers.projectCapNotify[capTr.ID]
	f.sched.srv.triggers.mu.Unlock()
	assert.True(t, notified, "project cap breach must record per-trigger 1h notification cooldown")

	// Verify metric skippedByReason counts for all 3 limit reasons.
	snap := f.sched.srv.TriggerSnapshotMetrics().GetSnapshot()
	assert.GreaterOrEqual(t, snap.Fires.SkippedByReason[trigger.FireSkipReasonRateLimit], int64(1))
	assert.GreaterOrEqual(t, snap.Fires.SkippedByReason[trigger.FireSkipReasonConcurrency], int64(1))
	assert.GreaterOrEqual(t, snap.Fires.SkippedByReason[trigger.FireSkipReasonProjectCap], int64(2))
}

// TestWebhookEndpointPatch_MemberForbiddenOwnerAdminAllowed covers R1 (option A)
// and N3 (hub_token endpoints do not mint or return an HMAC secret):
//   - Project members do NOT hold webhook_endpoint.update: PATCH on either the
//     owner's endpoint or the member's own endpoint returns 403 and does not
//     bump config_version.
//   - Project owners and admins can PATCH endpoints (200, config_version bumps).
//   - Project members retain webhook_endpoint.send: POST with member session
//     bearer to a hub_token endpoint returns 202.
//   - Creating a hub_token endpoint omits secret, sets secretVersion=0, stores
//     no secret row, and refuses PATCH back to hmac without a signing secret.
func TestWebhookEndpointPatch_MemberForbiddenOwnerAdminAllowed(t *testing.T) {
	f := setupTriggersTestFixture(t)
	ctx := context.Background()

	// Seed a project-admin user in the same project.
	adminUser := &store.User{
		ID:          api.NewUUID(),
		Email:       "p4a-admin@example.com",
		DisplayName: "P4a Admin",
		Role:        store.UserRoleMember,
		Status:      "active",
	}
	require.NoError(t, f.store.CreateUser(ctx, adminUser))
	adminRoleDef, err := f.store.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = f.store.CreateRoleBinding(ctx, &store.RoleBinding{
		ID:               api.NewUUID(),
		RoleDefinitionID: adminRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      adminUser.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          f.projectID,
	})
	require.NoError(t, err)
	adminAuth := NewAuthenticatedUser(adminUser.ID, adminUser.Email, adminUser.DisplayName, adminUser.Role, "web")

	// Owner creates an hmac endpoint (configVersion=1).
	ownerCreateRec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{
		Name: "owners-ep",
	})
	require.Equal(t, http.StatusCreated, ownerCreateRec.Code, ownerCreateRec.Body.String())
	var ownerEp WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(ownerCreateRec.Body.Bytes(), &ownerEp))
	require.Equal(t, store.WebhookAuthModeHMAC, ownerEp.AuthMode)
	require.NotEmpty(t, ownerEp.Secret)
	require.Equal(t, 1, ownerEp.SecretVersion)
	require.Equal(t, 1, ownerEp.ConfigVersion)

	// Member creates their own hmac endpoint (configVersion=1).
	memberCreateRec := f.doWebhookEndpoint(t, f.memberAuth, "", http.MethodPost, CreateWebhookEndpointRequest{
		Name: "members-ep",
	})
	require.Equal(t, http.StatusCreated, memberCreateRec.Code, memberCreateRec.Body.String())
	var memberEp WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(memberCreateRec.Body.Bytes(), &memberEp))
	require.Equal(t, 1, memberEp.ConfigVersion)

	// Member PATCH on owner's endpoint -> 403; endpoint unchanged.
	memberPatchOwner := f.doWebhookEndpoint(t, f.memberAuth, ownerEp.ID, http.MethodPatch, map[string]any{
		"auth":               store.WebhookAuthModeHubToken,
		"allowProjectAgents": true,
		"description":        "hijacked",
	})
	assert.Equal(t, http.StatusForbidden, memberPatchOwner.Code, memberPatchOwner.Body.String())
	storedOwnerEp, err := f.store.GetWebhookEndpoint(ctx, ownerEp.ID)
	require.NoError(t, err)
	assert.Equal(t, store.WebhookAuthModeHMAC, storedOwnerEp.AuthMode)
	assert.False(t, storedOwnerEp.AllowProjectAgents)
	assert.Equal(t, 1, storedOwnerEp.ConfigVersion)

	// Member PATCH on member's own endpoint -> 403; endpoint unchanged.
	memberPatchOwn := f.doWebhookEndpoint(t, f.memberAuth, memberEp.ID, http.MethodPatch, map[string]any{
		"description": "member-edit",
	})
	assert.Equal(t, http.StatusForbidden, memberPatchOwn.Code, memberPatchOwn.Body.String())
	storedMemberEp, err := f.store.GetWebhookEndpoint(ctx, memberEp.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, storedMemberEp.ConfigVersion)

	// Owner PATCH succeeds and bumps configVersion to 2.
	ownerPatch := f.doWebhookEndpoint(t, f.ownerAuth, ownerEp.ID, http.MethodPatch, map[string]any{
		"auth":        store.WebhookAuthModeHubToken,
		"description": "owner-updated",
	})
	require.Equal(t, http.StatusOK, ownerPatch.Code, ownerPatch.Body.String())
	var patchedByOwner WebhookEndpointResponse
	require.NoError(t, json.Unmarshal(ownerPatch.Body.Bytes(), &patchedByOwner))
	assert.Equal(t, store.WebhookAuthModeHubToken, patchedByOwner.AuthMode)
	assert.Equal(t, 2, patchedByOwner.ConfigVersion)

	// Admin PATCH succeeds and bumps configVersion to 3.
	adminPatch := f.doWebhookEndpoint(t, adminAuth, ownerEp.ID, http.MethodPatch, map[string]any{
		"description": "admin-updated",
	})
	require.Equal(t, http.StatusOK, adminPatch.Code, adminPatch.Body.String())
	var patchedByAdmin WebhookEndpointResponse
	require.NoError(t, json.Unmarshal(adminPatch.Body.Bytes(), &patchedByAdmin))
	assert.Equal(t, "admin-updated", patchedByAdmin.Description)
	assert.Equal(t, 3, patchedByAdmin.ConfigVersion)

	// Member retains webhook_endpoint.send: POST with member session bearer to
	// the hub_token endpoint returns 202.
	memberJWT := mintTestUserSessionJWT(t, f.srv, f.member)
	sendReq := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/"+ownerEp.ID, bytes.NewReader([]byte(`{"ok":true}`)))
	sendReq.RemoteAddr = "198.51.100.40:1234"
	sendReq.Header.Set("Content-Type", "application/json")
	sendReq.Header.Set("Authorization", "Bearer "+memberJWT)
	sendRec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(sendRec, sendReq)
	assert.Equal(t, http.StatusAccepted, sendRec.Code, sendRec.Body.String())

	// N3: Creating a hub_token endpoint does not mint or return an HMAC secret.
	htCreateRec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{
		Name:     "hub-token-no-secret",
		AuthMode: store.WebhookAuthModeHubToken,
	})
	require.Equal(t, http.StatusCreated, htCreateRec.Code, htCreateRec.Body.String())
	var rawResp map[string]any
	require.NoError(t, json.Unmarshal(htCreateRec.Body.Bytes(), &rawResp))
	_, hasSecretKey := rawResp["secret"]
	assert.False(t, hasSecretKey, "hub_token endpoint response must omit secret field")
	var htEp WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(htCreateRec.Body.Bytes(), &htEp))
	assert.Empty(t, htEp.Secret)
	assert.Equal(t, 0, htEp.SecretVersion)

	// No secret row was written for the hub_token endpoint.
	_, secErr := f.store.GetSecret(ctx, store.WebhookEndpointSecretKey(f.projectID, htEp.ID, 1), store.ScopeHub, f.srv.hubID)
	assert.ErrorIs(t, secErr, store.ErrNotFound)

	// Switching that secret-less hub_token endpoint to hmac via PATCH is refused 400.
	flipBack := f.doWebhookEndpoint(t, f.ownerAuth, htEp.ID, http.MethodPatch, map[string]any{
		"auth": store.WebhookAuthModeHMAC,
	})
	assert.Equal(t, http.StatusBadRequest, flipBack.Code, flipBack.Body.String())
}

// TestTriggerLimits_RateLimitConcurrentExactAdmission covers O1: when multiple
// fires of the same trigger are claimed concurrently ("firing") before reaching
// checkTriggerFireLimits, deterministic (created, id) ordering ensures exactly
// max_fires_per_minute fires are admitted (never mutual skip, never overshoot).
func TestTriggerLimits_RateLimitConcurrentExactAdmission(t *testing.T) {
	f := newTriggerDispatchAgentFixture(t, "rate-concurrent")
	ctx := context.Background()
	userCtx := f.userAuth(t, f.sched.creator)
	ep := f.createEndpoint(t, userCtx, "rate-conc-ep")

	targetAgent := &store.Agent{
		ID:        api.NewUUID(),
		Slug:      "rate-conc-target",
		Name:      "rate-conc-target",
		ProjectID: f.sched.proj.ID,
		OwnerID:   f.sched.creator.ID,
		Phase:     "running",
	}
	require.NoError(t, f.sched.store.CreateAgent(ctx, targetAgent))

	createMsgTrigger := func(name string, maxPerMin int) TriggerResponse {
		rec := f.doJSON(t, userCtx, http.MethodPost,
			fmt.Sprintf("/api/v1/projects/%s/triggers", f.sched.proj.ID),
			CreateTriggerRequest{
				Name:              name,
				Source:            TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
				Action:            TriggerActionSpec{Type: store.TriggerActionMessage, AgentID: targetAgent.ID, Message: "hi"},
				MaxFiresPerMinute: maxPerMin,
			})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		var tr TriggerResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &tr))
		return tr
	}

	claimFire := func(trID, dedupKey string) string {
		now := time.Now().UTC()
		ev := &store.TriggerEvent{
			ID: api.NewUUID(), ProjectID: f.sched.proj.ID, SourceKind: store.TriggerSourceKindWebhook,
			SourceID: ep.ID, Type: "webhook.received", Subject: "webhook:" + ep.ID, DedupKey: dedupKey,
			OccurredAt: now, ReceivedAt: now, Attributes: map[string]string{"signature_mode": "scion_v1"},
			Payload: `{}`, PayloadBytes: 2, Status: store.TriggerEventStatusProcessed, CreatedAt: now,
		}
		require.NoError(t, f.sched.store.CreateTriggerEvent(ctx, ev))
		fire := &store.TriggerFire{
			ID: api.NewUUID(), ProjectID: f.sched.proj.ID, TriggerID: trID, EventID: ev.ID,
			Subject: ev.Subject, ActionType: store.TriggerActionMessage, Status: store.TriggerFireStatusFiring,
			ExecutorKind: "trigger_drainer", ExecutorID: "trigger_event:" + ev.ID,
			LeaseUntil: now.Add(10 * time.Minute), StartedAt: now, CreatedAt: now,
		}
		ok, _, err := f.sched.store.ClaimTriggerFire(ctx, fire)
		require.NoError(t, err)
		require.True(t, ok)
		return fire.ID
	}

	// Case 1: max_fires_per_minute=1 with 2 pre-claimed fires (reproduces review-1 probe).
	tr1 := createMsgTrigger("rate-exact-1", 1)
	fireA := claimFire(tr1.ID, "exact-a")
	fireB := claimFire(tr1.ID, "exact-b")

	check := func(trID, fireID string, limit int) string {
		reason, err := f.sched.srv.checkTriggerFireLimits(ctx, hubfire.FireRequest{
			ProjectID:  f.sched.proj.ID,
			ActionType: store.TriggerActionMessage,
			Authority:  hubfire.RecordedAuthority{DefinitionID: trID, FireID: fireID},
			Event:      &hubfire.FireEvent{Trigger: &hubfire.FireTriggerRef{ID: trID, MaxFiresPerMinute: limit}},
		})
		require.NoError(t, err)
		return reason
	}
	ra, rb := check(tr1.ID, fireA, 1), check(tr1.ID, fireB, 1)
	admitted := 0
	skipped := 0
	for _, r := range []string{ra, rb} {
		if r == "" {
			admitted++
		} else if r == trigger.ErrFireRateLimit {
			skipped++
		}
	}
	assert.Equal(t, 1, admitted, "exactly 1 fire must be admitted (ra=%q rb=%q)", ra, rb)
	assert.Equal(t, 1, skipped, "exactly 1 fire must be skipped (ra=%q rb=%q)", ra, rb)

	// Case 2: max_fires_per_minute=2 with 5 concurrently checked pre-claimed fires.
	tr2 := createMsgTrigger("rate-exact-2", 2)
	fireIDs := make([]string, 5)
	for i := range fireIDs {
		fireIDs[i] = claimFire(tr2.ID, fmt.Sprintf("burst-%d", i))
	}
	results := make([]string, len(fireIDs))
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, fid := range fireIDs {
		wg.Add(1)
		go func(idx int, id string) {
			defer wg.Done()
			<-start
			results[idx] = check(tr2.ID, id, 2)
		}(i, fid)
	}
	close(start)
	wg.Wait()

	admitted2, skipped2 := 0, 0
	for _, r := range results {
		switch r {
		case "":
			admitted2++
		case trigger.ErrFireRateLimit:
			skipped2++
		}
	}
	assert.Equal(t, 2, admitted2, "exactly 2 of 5 concurrent fires must be admitted (results=%v)", results)
	assert.Equal(t, 3, skipped2, "exactly 3 of 5 concurrent fires must be skipped (results=%v)", results)
}

// TestTriggerLimits_TwoReplicaConcurrencyAndProjectCap covers O2: two separate
// hub Server replicas sharing the same enttest store (real PostgreSQL 17 under
// -tags integration + SCION_TEST_POSTGRES_URL) race concurrent dispatch_agent
// fires against max_concurrent_dispatches=2 and MaxLiveTriggerAgentsPerProject=25.
// LockTriggerQuota inside commitAgentCreate's OnCommitTx serializes commits
// across replicas with zero overshoot.
func TestTriggerLimits_TwoReplicaConcurrencyAndProjectCap(t *testing.T) {
	ctx := context.Background()
	f := newTriggerDispatchAgentFixture(t, "two-replica-limits")
	userCtx := f.userAuth(t, f.sched.creator)
	ep := f.createEndpoint(t, userCtx, "two-replica-ep")

	// Construct a second Server replica sharing the exact same underlying store
	// and mock broker dispatcher (simulating two hub replicas on shared Postgres).
	replica2, err := New(DefaultServerConfig(), f.sched.store)
	require.NoError(t, err)
	replica2.SetDispatcher(f.sched.srv.GetDispatcher())
	replica2.experiments = f.sched.srv.experiments
	t.Cleanup(func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = replica2.Shutdown(shutCtx)
	})

	// 1. Concurrency cap = 2 across 6 simultaneous fires split across 2 replicas.
	recConc := f.doJSON(t, userCtx, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/triggers", f.sched.proj.ID),
		CreateTriggerRequest{
			Name:   "two-replica-conc",
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
			Action: TriggerActionSpec{
				Type:     store.TriggerActionDispatchAgent,
				Template: "default",
				Task:     "Two-replica conc ${event.id}",
			},
			MaxConcurrentDispatches: 2,
		})
	require.Equal(t, http.StatusCreated, recConc.Code, recConc.Body.String())
	var concResp TriggerResponse
	require.NoError(t, json.Unmarshal(recConc.Body.Bytes(), &concResp))
	concTr, err := f.sched.store.GetTrigger(ctx, concResp.ID)
	require.NoError(t, err)

	runConcurrentFires := func(tr *store.Trigger, total int) (fired, skipped int, skipReasons []string) {
		outcomes := make([]hubfire.FireOutcome, total)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < total; i++ {
			srv := f.sched.srv
			if i%2 == 1 {
				srv = replica2
			}
			wg.Add(1)
			go func(idx int, s *Server) {
				defer wg.Done()
				<-start
				now := time.Now().UTC()
				req := hubfire.FireRequest{
					ProjectID: f.sched.proj.ID,
					Authority: hubfire.RecordedAuthority{
						Attribution:    tr.InitiatorAttribution,
						Ceiling:        tr.AuthorityCeiling,
						DefinitionKind: hubfire.DefinitionKindTrigger,
						DefinitionID:   tr.ID,
						FireID:         api.NewUUID(),
					},
					ActionType: store.TriggerActionDispatchAgent,
					Payload:    firstTriggerAction(*tr).Payload,
					Event: &hubfire.FireEvent{
						ID:         api.NewUUID(),
						ProjectID:  f.sched.proj.ID,
						SourceKind: store.TriggerSourceKindWebhook,
						SourceID:   ep.ID,
						Type:       "webhook.received",
						Subject:    "webhook:" + ep.ID,
						OccurredAt: now,
						ReceivedAt: now,
						Attributes: map[string]string{
							"endpoint_name":  ep.Name,
							"signature_mode": store.WebhookSignatureModeScionV1,
							"delivery":       fmt.Sprintf("two-rep-%s-%d", tr.ID[:8], idx),
						},
						Payload: []byte(`{}`),
						Trigger: &hubfire.FireTriggerRef{
							ID:                      tr.ID,
							Name:                    tr.Name,
							MaxFiresPerMinute:       tr.MaxFiresPerMinute,
							MaxConcurrentDispatches: tr.MaxConcurrentDispatches,
						},
					},
					FireAt: now,
				}
				outcomes[idx] = serverTriggerCore{srv: s}.Fire(ctx, req)
			}(i, srv)
		}
		close(start)
		wg.Wait()
		for _, out := range outcomes {
			switch out.Status {
			case hubfire.FireStatusFired:
				fired++
			case hubfire.FireStatusSkipped:
				skipped++
				skipReasons = append(skipReasons, out.Error)
			default:
				t.Errorf("unexpected fire status %q (error=%q)", out.Status, out.Error)
			}
		}
		return fired, skipped, skipReasons
	}

	firedConc, skippedConc, concReasons := runConcurrentFires(concTr, 6)
	assert.Equal(t, 2, firedConc, "two replicas must admit exactly max_concurrent_dispatches=2")
	assert.Equal(t, 4, skippedConc, "two replicas must skip the remaining 4 fires")
	for _, r := range concReasons {
		assert.Equal(t, trigger.ErrFireConcurrencyLimit, r)
	}

	// 2. Project cap = 25 across 2 replicas: seed live trigger agents up to 24,
	//    then race 4 concurrent fires across both replicas -> exactly 1 fired, 3 skipped.
	liveNow, err := f.sched.srv.countLiveAgentsMatching(ctx, store.AgentFilter{
		ProjectID: f.sched.proj.ID,
		LabelKeys: []string{triggerLabelKeyTrigger},
	})
	require.NoError(t, err)
	for i := liveNow; i < store.MaxLiveTriggerAgentsPerProject-1; i++ {
		slug := fmt.Sprintf("rep-cap-seed-%d", i)
		require.NoError(t, f.sched.store.CreateAgent(ctx, &store.Agent{
			ID:        api.NewUUID(),
			Slug:      slug,
			Name:      slug,
			ProjectID: f.sched.proj.ID,
			Phase:     "running",
			Labels:    map[string]string{triggerLabelKeyTrigger: "other-trigger"},
		}))
	}

	recCap := f.doJSON(t, userCtx, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/triggers", f.sched.proj.ID),
		CreateTriggerRequest{
			Name:   "two-replica-cap",
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
			Action: TriggerActionSpec{
				Type:     store.TriggerActionDispatchAgent,
				Template: "default",
				Task:     "Two-replica cap ${event.id}",
			},
			MaxConcurrentDispatches: 50,
		})
	require.Equal(t, http.StatusCreated, recCap.Code, recCap.Body.String())
	var capResp TriggerResponse
	require.NoError(t, json.Unmarshal(recCap.Body.Bytes(), &capResp))
	capTr, err := f.sched.store.GetTrigger(ctx, capResp.ID)
	require.NoError(t, err)

	firedCap, skippedCap, capReasons := runConcurrentFires(capTr, 4)
	assert.Equal(t, 1, firedCap, "two replicas must admit exactly 1 fire to reach project cap 25")
	assert.Equal(t, 3, skippedCap, "two replicas must skip the remaining 3 fires at project cap")
	for _, r := range capReasons {
		assert.Equal(t, trigger.ErrFireProjectTriggerAgentCap, r)
	}
}

// TestWebhookEndpointCache_CrossReplicaInvalidation covers O4: a PATCH on
// replica 1 publishes trigger.webhook_endpoint.changed over EventPublisher so
// replica 2 evicts its positive endpoint cache immediately without waiting for
// EndpointCacheTTL.
func TestWebhookEndpointCache_CrossReplicaInvalidation(t *testing.T) {
	f := setupTriggersTestFixture(t)
	pub := NewChannelEventPublisher()
	t.Cleanup(pub.Close)

	f.srv.SetEventPublisher(pub)
	replica2, err := New(DefaultServerConfig(), f.store)
	require.NoError(t, err)
	replica2.SetEventPublisher(pub)
	replica2.experiments = f.srv.experiments
	subCtx, cancelSub := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancelSub()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = replica2.Shutdown(shutCtx)
	})
	replica2.startWebhookEndpointInvalidationSubscriber(subCtx)

	// Create a hub_token endpoint on replica 1.
	createRec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{
		Name:     "cache-inval-ep",
		AuthMode: store.WebhookAuthModeHubToken,
	})
	require.Equal(t, http.StatusCreated, createRec.Code, createRec.Body.String())
	var ep WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(createRec.Body.Bytes(), &ep))

	// Warm replica 2's positive cache by sending an authorized request to replica 2.
	ownerJWT := mintTestUserSessionJWT(t, replica2, f.owner)
	sendToReplica2 := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/"+ep.ID, bytes.NewReader([]byte(`{"ping":1}`)))
		req.RemoteAddr = "198.51.100.55:9000"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+ownerJWT)
		rec := httptest.NewRecorder()
		replica2.Handler().ServeHTTP(rec, req)
		return rec
	}
	require.Equal(t, http.StatusAccepted, sendToReplica2().Code, "replica 2 should accept and cache active endpoint")

	// Disable the endpoint on replica 1 via PATCH.
	patchRec := f.doWebhookEndpoint(t, f.ownerAuth, ep.ID, http.MethodPatch, map[string]any{
		"status": store.WebhookEndpointStatusDisabled,
	})
	require.Equal(t, http.StatusOK, patchRec.Code, patchRec.Body.String())

	// Replica 2 must observe the invalidation promptly and return 404.
	require.Eventually(t, func() bool {
		return sendToReplica2().Code == http.StatusNotFound
	}, 2*time.Second, 20*time.Millisecond, "replica 2 must evict cached endpoint on cross-replica invalidation event")
}

// TestTriggerDispatchAgent_ReservedAuthorLabelDroppedAtFire covers P1c-5b
// security L1: even if a legacy trigger row stored before authoring validation
// carries a reserved label key (such as scion.dev/github-token-source-project),
// fireTriggerDispatchAgent drops it before merging labels.
func TestTriggerDispatchAgent_ReservedAuthorLabelDroppedAtFire(t *testing.T) {
	ctx := context.Background()
	f := newTriggerDispatchAgentFixture(t, "l1-reserved-label")
	userCtx := f.userAuth(t, f.sched.creator)
	ep := f.createEndpoint(t, userCtx, "l1-ep")

	payloadBytes, err := json.Marshal(DispatchAgentEventPayload{
		Template: "default",
		Task:     "L1 test ${event.id}",
		Labels: map[string]string{
			"env":                         "prod",
			githubTokenSourceProjectLabel: api.NewUUID(),
			triggerLabelKeyTrigger:        "spoofed-trigger-id",
		},
	})
	require.NoError(t, err)

	now := time.Now().UTC()
	trigID := api.NewUUID()
	out := f.sched.srv.fireAction(ctx, hubfire.FireRequest{
		ProjectID: f.sched.proj.ID,
		Authority: hubfire.RecordedAuthority{
			Attribution: store.InitiatorAttribution{
				InitiatorPrincipalKind:  store.DelegationPrincipalUser,
				InitiatorPrincipalID:    f.sched.creator.ID,
				InitiatorCredentialKind: store.InitiatorCredentialKindSession,
				AttributionVersion:      1,
				AuthorizationRevision:   1,
			},
			Ceiling:        store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
			DefinitionKind: hubfire.DefinitionKindTrigger,
			DefinitionID:   trigID,
			FireID:         api.NewUUID(),
		},
		ActionType: store.TriggerActionDispatchAgent,
		Payload:    string(payloadBytes),
		Event: &hubfire.FireEvent{
			ID:         api.NewUUID(),
			ProjectID:  f.sched.proj.ID,
			SourceKind: store.TriggerSourceKindWebhook,
			SourceID:   ep.ID,
			Type:       "webhook.received",
			Subject:    "webhook:" + ep.ID,
			OccurredAt: now,
			ReceivedAt: now,
			Attributes: map[string]string{
				"endpoint_name":  ep.Name,
				"signature_mode": store.WebhookSignatureModeScionV1,
				"delivery":       "l1-deliv-1",
			},
			Payload: []byte(`{}`),
			Trigger: &hubfire.FireTriggerRef{ID: trigID, Name: "l1-trig"},
		},
		FireAt: now,
	})
	require.Equal(t, hubfire.FireStatusFired, out.Status, out.Error)
	require.NotEmpty(t, out.TargetAgentID)

	ag, err := f.sched.store.GetAgent(ctx, out.TargetAgentID)
	require.NoError(t, err)
	assert.Equal(t, "prod", ag.Labels["env"])
	assert.Equal(t, trigID, ag.Labels[triggerLabelKeyTrigger], "hub provenance trigger label must win")
	_, hasReserved := ag.Labels[githubTokenSourceProjectLabel]
	assert.False(t, hasReserved, "reserved github-token-source-project author label must be dropped at fire time")
}

// TestAuthorizeDispatchAgentFire_IdentityRules covers P1c-5b security L2:
// authorizeDispatchAgentFire mirrors authorizeAgentCreate's identity rules
// (nil refused, cross-project agent refused, non-user/non-agent identity types
// default-denied).
func TestAuthorizeDispatchAgentFire_IdentityRules(t *testing.T) {
	ctx := context.Background()
	f := newTriggerDispatchAgentFixture(t, "l2-fire-identity")
	auth := FireAuthority{
		PrincipalKind: store.DelegationPrincipalAgent,
		PrincipalID:   api.NewUUID(),
	}

	// 1. Nil identity refused.
	_, _, err := f.sched.srv.authorizeDispatchAgentFire(
		ctx, f.sched.proj.ID, api.NewUUID(), hubfire.DefinitionKindTrigger, auth, nil, "", false,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unauthenticated")

	// 2. Cross-project agent identity refused even with ScopeAgentCreate.
	otherProjectID := api.NewUUID()
	foreignClaims := &AgentTokenClaims{
		ProjectID: otherProjectID,
		Scopes:    []AgentTokenScope{ScopeAgentCreate},
	}
	foreignClaims.Subject = auth.PrincipalID
	foreignAgent := &agentIdentityWrapper{foreignClaims}
	_, _, err = f.sched.srv.authorizeDispatchAgentFire(
		ctx, f.sched.proj.ID, api.NewUUID(), hubfire.DefinitionKindTrigger, auth, foreignAgent, "", false,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "belongs to project")

	// 3. Non-user / non-agent identity (e.g. broker) default-denied.
	brokerIdent := NewBrokerIdentity("broker-1")
	_, _, err = f.sched.srv.authorizeDispatchAgentFire(
		ctx, f.sched.proj.ID, api.NewUUID(), hubfire.DefinitionKindTrigger,
		FireAuthority{PrincipalKind: "broker", PrincipalID: brokerIdent.ID()},
		brokerIdent, "", false,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "may not create agents")
}

// TestTriggerFire_O1VerificationModeUsesRecordedEventAttr covers review-2 O1:
// fire-time M1 exemption, dispatch framing trailer, reuse-message trailer, and
// trigger render preview all derive verification mode from the hub-written
// event.attributes["signature_mode"] recorded at ingress, NOT the endpoint's
// later flipped auth_mode.
func TestTriggerFire_O1VerificationModeUsesRecordedEventAttr(t *testing.T) {
	ctx := context.Background()
	f := newTriggerDispatchAgentFixture(t, "o1-recorded-mode")
	userCtx := f.userAuth(t, f.sched.creator)
	ownerUser := hubMemberUser(t, f.sched.store, "o1-owner")
	grantFixtureRole(t, f.sched.bypassAgentsFixture, ownerUser.ID, store.ProjectRoleOwner)
	ownerCtx := f.userAuth(t, ownerUser)

	// Direction 1: HMAC-ingested event WITHOUT X-Scion-Delivery, then endpoint
	// flipped to hub_token before fire -> STILL refused "missing delivery id".
	hmacEp := f.createEndpoint(t, userCtx, "o1-hmac-ep")
	recTr := f.doJSON(t, userCtx, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/triggers", f.sched.proj.ID),
		CreateTriggerRequest{
			Name:   "o1-dispatch",
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: hmacEp.ID},
			Match:  &TriggerMatchSpec{Types: []string{"webhook.received"}},
			Action: TriggerActionSpec{
				Type:          store.TriggerActionDispatchAgent,
				AgentName:     "o1-worker",
				Template:      "default",
				Task:          "Work on ${event.id}: ${event.payload.hello}",
				ReuseMessage:  "Reuse ${event.id}: ${event.payload.hello}",
				UntrustedVars: []string{"event.payload.hello"},
			},
		})
	require.Equal(t, http.StatusCreated, recTr.Code, recTr.Body.String())
	var trResp TriggerResponse
	require.NoError(t, json.Unmarshal(recTr.Body.Bytes(), &trResp))

	// Post HMAC-signed webhook with empty delivery id (queued, not yet drained).
	recIngest := f.postSignedWebhook(t, hmacEp, "", "", "o1-subj", []byte(`{"k":"v"}`))
	require.Equal(t, http.StatusAccepted, recIngest.Code, recIngest.Body.String())

	// Flip endpoint to hub_token BEFORE draining the queued event.
	recFlipHT := f.doJSON(t, ownerCtx, http.MethodPatch,
		fmt.Sprintf("/api/v1/projects/%s/webhook-endpoints/%s", f.sched.proj.ID, hmacEp.ID),
		map[string]any{"auth": store.WebhookAuthModeHubToken})
	require.Equal(t, http.StatusOK, recFlipHT.Code, recFlipHT.Body.String())

	f.drainOneTriggerEvent(t)
	fires1, err := f.sched.store.ListTriggerFires(ctx, store.TriggerFireFilter{
		ProjectID: f.sched.proj.ID,
		TriggerID: trResp.ID,
	}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, fires1.Items, 1)
	assert.Equal(t, store.TriggerFireStatusRefused, fires1.Items[0].Status)
	assert.Equal(t, trigger.FireReplayReasonMissingDeliveryID, fires1.Items[0].Error)

	// Direction 2: hub_token-ingested event, then endpoint flipped back to hmac
	// before fire/render -> fire trailer, reuse-message trailer, and render
	// preview STILL show hub token (not scion_v1).
	ownerJWT := mintTestUserSessionJWT(t, f.sched.srv, f.sched.creator)
	htReq := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/"+hmacEp.ID, bytes.NewReader([]byte(`{"hello":"world"}`)))
	htReq.RemoteAddr = "198.51.100.77:9000"
	htReq.Header.Set("Content-Type", "application/json")
	htReq.Header.Set("Authorization", "Bearer "+ownerJWT)
	htRec := httptest.NewRecorder()
	f.sched.srv.Handler().ServeHTTP(htRec, htReq)
	require.Equal(t, http.StatusAccepted, htRec.Code, htRec.Body.String())

	var htIngestResp struct {
		EventID string `json:"eventId"`
	}
	require.NoError(t, json.Unmarshal(htRec.Body.Bytes(), &htIngestResp))
	require.NotEmpty(t, htIngestResp.EventID)

	// Flip endpoint back to hmac BEFORE draining/rendering.
	recFlipHMAC := f.doJSON(t, ownerCtx, http.MethodPatch,
		fmt.Sprintf("/api/v1/projects/%s/webhook-endpoints/%s", f.sched.proj.ID, hmacEp.ID),
		map[string]any{"auth": store.WebhookAuthModeHMAC})
	require.Equal(t, http.StatusOK, recFlipHMAC.Code, recFlipHMAC.Body.String())

	// 2a. Render preview must report "hub token" (not "scion_v1").
	renderRec := f.doJSON(t, userCtx, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/triggers/%s/render?eventId=%s", f.sched.proj.ID, trResp.ID, htIngestResp.EventID),
		nil)
	require.Equal(t, http.StatusOK, renderRec.Code, renderRec.Body.String())
	var renderResp TriggerRenderResponse
	require.NoError(t, json.Unmarshal(renderRec.Body.Bytes(), &renderResp))
	require.NotNil(t, renderResp.Slots.Task)
	assert.Contains(t, renderResp.Slots.Task.Rendered, "(hub token;")
	assert.NotContains(t, renderResp.Slots.Task.Rendered, "(scion_v1;")

	// 2b. Draining the hub_token-ingested event (without delivery id) succeeds
	// and the dispatched agent's task trailer reports "hub token".
	f.drainOneTriggerEvent(t)
	fires2, err := f.sched.store.ListTriggerFires(ctx, store.TriggerFireFilter{
		ProjectID: f.sched.proj.ID,
		TriggerID: trResp.ID,
	}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, fires2.Items, 2)
	var firedRow *store.TriggerFire
	for i := range fires2.Items {
		if fires2.Items[i].EventID == htIngestResp.EventID {
			firedRow = &fires2.Items[i]
			break
		}
	}
	require.NotNil(t, firedRow)
	assert.Equal(t, store.TriggerFireStatusFired, firedRow.Status, firedRow.Error)
	ag, err := f.sched.store.GetAgent(ctx, firedRow.TargetAgentID)
	require.NoError(t, err)
	assert.Contains(t, ag.AppliedConfig.Task, "(hub token;")
	assert.NotContains(t, ag.AppliedConfig.Task, "(scion_v1;")

	// 2c. Reuse-message rendering also derives hub_token from the event attribute.
	storedTr, err := f.sched.store.GetTrigger(ctx, trResp.ID)
	require.NoError(t, err)
	storedEv, err := f.sched.store.GetTriggerEvent(ctx, htIngestResp.EventID)
	require.NoError(t, err)
	reuseRendered, reuseRefusal, reuseErr := f.sched.srv.renderDispatchReuseMessage(ctx, hubfire.FireRequest{
		ProjectID:  f.sched.proj.ID,
		ActionType: store.TriggerActionDispatchAgent,
		Payload:    firstTriggerAction(*storedTr).Payload,
		Authority:  hubfire.RecordedAuthority{DefinitionKind: hubfire.DefinitionKindTrigger, DefinitionID: storedTr.ID},
		Event: &hubfire.FireEvent{
			ID:         storedEv.ID,
			ProjectID:  f.sched.proj.ID,
			SourceKind: storedEv.SourceKind,
			SourceID:   storedEv.SourceID,
			Type:       storedEv.Type,
			Subject:    storedEv.Subject,
			OccurredAt: storedEv.OccurredAt,
			ReceivedAt: storedEv.ReceivedAt,
			Attributes: storedEv.Attributes,
			Payload:    []byte(storedEv.Payload),
			Trigger:    &hubfire.FireTriggerRef{ID: storedTr.ID, Name: storedTr.Name},
		},
	})
	require.NoError(t, reuseErr)
	require.Nil(t, reuseRefusal)
	assert.Contains(t, reuseRendered, "Reuse "+storedEv.ID+": world")
}

// TestWebhookEndpoint_O2HubTokenRotateThenSwitchToHMAC covers review-2 O2:
// rotate-secret on a version-0 (hub_token) endpoint mints v1 with no grace
// window, returns it once, and allows PATCH auth=hmac followed by a signed POST.
func TestWebhookEndpoint_O2HubTokenRotateThenSwitchToHMAC(t *testing.T) {
	f := setupTriggersTestFixture(t)

	// 1. Create a hub_token endpoint -> secretVersion 0, no secret returned.
	createRec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{
		Name:     "o2-ht-ep",
		AuthMode: store.WebhookAuthModeHubToken,
	})
	require.Equal(t, http.StatusCreated, createRec.Code, createRec.Body.String())
	var created WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(createRec.Body.Bytes(), &created))
	assert.Empty(t, created.Secret)
	assert.Equal(t, 0, created.SecretVersion)

	// 2. Rotate secret on v0 endpoint -> mints v1 with NO grace window.
	rotRec := f.doWebhookEndpoint(t, f.ownerAuth, created.ID+"/rotate-secret", http.MethodPost, nil)
	require.Equal(t, http.StatusOK, rotRec.Code, rotRec.Body.String())
	var rotated WebhookEndpointRotatedResponse
	require.NoError(t, json.Unmarshal(rotRec.Body.Bytes(), &rotated))
	assert.Equal(t, 1, rotated.SecretVersion)
	assert.NotEmpty(t, rotated.Secret)
	assert.True(t, rotated.GraceWindowExpires.IsZero(), "v0->v1 mint must have no grace window")
	assert.Nil(t, rotated.SecretRotatedAt)
	assert.NotContains(t, rotRec.Body.String(), `"graceWindowExpires"`)

	// 3. PATCH auth=hmac now succeeds.
	patchRec := f.doWebhookEndpoint(t, f.ownerAuth, created.ID, http.MethodPatch, map[string]any{
		"auth": store.WebhookAuthModeHMAC,
	})
	require.Equal(t, http.StatusOK, patchRec.Code, patchRec.Body.String())
	var patched WebhookEndpointResponse
	require.NoError(t, json.Unmarshal(patchRec.Body.Bytes(), &patched))
	assert.Equal(t, store.WebhookAuthModeHMAC, patched.AuthMode)

	// 4. Signed POST using the minted v1 secret is accepted (202).
	body := []byte(`{"o2":"ok"}`)
	ts := time.Now().Unix()
	deliv := api.NewUUID()
	sig := trigger.SignScionV1(rotated.Secret, ts, deliv, "", "", body)
	postReq := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/"+created.ID, bytes.NewReader(body))
	postReq.RemoteAddr = "198.51.100.88:9000"
	postReq.Header.Set("Content-Type", "application/json")
	postReq.Header.Set("X-Scion-Delivery", deliv)
	postReq.Header.Set("X-Scion-Signature", sig)
	postRec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(postRec, postReq)
	assert.Equal(t, http.StatusAccepted, postRec.Code, postRec.Body.String())

	// 5. Rotating again (v1->v2) sets the 24h grace window.
	rot2Rec := f.doWebhookEndpoint(t, f.ownerAuth, created.ID+"/rotate-secret", http.MethodPost, nil)
	require.Equal(t, http.StatusOK, rot2Rec.Code, rot2Rec.Body.String())
	var rotated2 WebhookEndpointRotatedResponse
	require.NoError(t, json.Unmarshal(rot2Rec.Body.Bytes(), &rotated2))
	assert.Equal(t, 2, rotated2.SecretVersion)
	assert.False(t, rotated2.GraceWindowExpires.IsZero(), "v1->v2 rotation must include 24h grace window")
	assert.NotNil(t, rotated2.SecretRotatedAt)
}

// TestWebhookEndpointCache_O4CrossReplicaInvalidationOnAgentDelete covers
// review-2 O4: deleting a bound agent on replica 1 publishes cross-replica
// invalidations from webhook_endpoint_cascade.go so replica 2 evicts both the
// deleted agent-lifetime endpoint and the unbound persistent endpoint.
func TestWebhookEndpointCache_O4CrossReplicaInvalidationOnAgentDelete(t *testing.T) {
	ctx := context.Background()
	f := setupTriggersTestFixture(t)
	pub := NewChannelEventPublisher()
	t.Cleanup(pub.Close)

	f.srv.SetEventPublisher(pub)
	replica2, err := New(DefaultServerConfig(), f.store)
	require.NoError(t, err)
	replica2.SetEventPublisher(pub)
	replica2.experiments = f.srv.experiments
	subCtx, cancelSub := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancelSub()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = replica2.Shutdown(shutCtx)
	})
	replica2.startWebhookEndpointInvalidationSubscriber(subCtx)

	boundAgent := &store.Agent{
		ID:        api.NewUUID(),
		Slug:      "o4-bound-agent",
		Name:      "o4-bound-agent",
		ProjectID: f.projectID,
		Phase:     "running",
		OwnerID:   f.owner.ID,
	}
	require.NoError(t, f.store.CreateAgent(ctx, boundAgent))

	// Create an agent-lifetime bound endpoint and a persistent bound endpoint.
	createBound := func(name, lifetime string) WebhookEndpointCreatedResponse {
		rec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{
			Name:          name,
			AuthMode:      store.WebhookAuthModeHubToken,
			BoundAgentID:  boundAgent.ID,
			BoundLifetime: lifetime,
		})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		var ep WebhookEndpointCreatedResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ep))
		return ep
	}
	epAgentLife := createBound("o4-agent-life-ep", store.WebhookBoundLifetimeAgent)
	epPersistent := createBound("o4-persistent-ep", store.WebhookBoundLifetimePersistent)

	ownerJWT := mintTestUserSessionJWT(t, replica2, f.owner)
	sendToReplica2 := func(epID string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/"+epID, bytes.NewReader([]byte(`{"ping":1}`)))
		req.RemoteAddr = "198.51.100.66:9000"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+ownerJWT)
		rec := httptest.NewRecorder()
		replica2.Handler().ServeHTTP(rec, req)
		return rec
	}

	// Warm replica 2's positive cache for both endpoints.
	require.Equal(t, http.StatusAccepted, sendToReplica2(epAgentLife.ID).Code)
	require.Equal(t, http.StatusAccepted, sendToReplica2(epPersistent.ID).Code)

	// Observe invalidation events published on the shared publisher.
	invalCh, unsub := pub.Subscribe("trigger.webhook_endpoint.changed")
	defer unsub()

	// Delete the bound agent on replica 1 (soft-delete + cascade).
	require.NoError(t, f.store.DeleteAgent(ctx, boundAgent.ID))
	f.srv.releaseTriggerBindingsForAgent(ctx, boundAgent)

	// Both the agent-lifetime delete and the persistent unbind must publish
	// cross-replica invalidations.
	seen := map[string]bool{}
	deadline := time.After(2 * time.Second)
	for len(seen) < 2 {
		select {
		case ev := <-invalCh:
			var payload struct {
				EndpointID string `json:"endpointId"`
			}
			if json.Unmarshal(ev.Data, &payload) == nil && payload.EndpointID != "" {
				seen[payload.EndpointID] = true
			}
		case <-deadline:
			t.Fatalf("timed out waiting for cross-replica invalidation events; saw %v", seen)
		}
	}
	assert.True(t, seen[epAgentLife.ID], "expected invalidation for deleted agent-lifetime endpoint")
	assert.True(t, seen[epPersistent.ID], "expected invalidation for unbound persistent endpoint")

	// Replica 2 must promptly evict the deleted agent-lifetime endpoint (404).
	require.Eventually(t, func() bool {
		return sendToReplica2(epAgentLife.ID).Code == http.StatusNotFound
	}, 2*time.Second, 20*time.Millisecond, "replica 2 must evict agent-lifetime endpoint on agent delete cascade")

	// Persistent endpoint is unbound in store and remains active on replica 2.
	got, gErr := f.store.GetWebhookEndpoint(ctx, epPersistent.ID)
	require.NoError(t, gErr)
	assert.Empty(t, got.BoundAgentID)
	assert.Empty(t, got.BoundLifetime)
	assert.Equal(t, http.StatusAccepted, sendToReplica2(epPersistent.ID).Code)
}

// TestWebhookIngress_HubTokenAgentTokenScopeEnforced covers security-pass M1:
// hub_token ingress ActionSend requires ScopeAgentNotify (project:agent:notify)
// on the agent token. Readonly-role and status-only tokens (including the
// SignAgentToken default) are denied (403) on owned, bound, and
// allowProjectAgents endpoints, while baseline-role and full-role tokens are
// accepted (202).
func TestWebhookIngress_HubTokenAgentTokenScopeEnforced(t *testing.T) {
	f := setupTriggersTestFixture(t)
	ctx := context.Background()

	ag := &store.Agent{
		ID:        api.NewUUID(),
		Slug:      "sec-m1-agent",
		Name:      "sec-m1-agent",
		ProjectID: f.projectID,
		Phase:     "running",
		OwnerID:   f.owner.ID,
	}
	require.NoError(t, f.store.CreateAgent(ctx, ag))

	endpoints := []struct {
		name string
		ep   *store.WebhookEndpoint
	}{
		{
			name: "owned_endpoint",
			ep: &store.WebhookEndpoint{
				ID:                  api.NewUUID(),
				ProjectID:           f.projectID,
				Name:                "m1-owned-ep",
				OwnerPrincipalKind:  store.DelegationPrincipalAgent,
				OwnerPrincipalID:    ag.ID,
				OwnerCredentialKind: "api",
				AuthMode:            store.WebhookAuthModeHubToken,
				MaxBodyBytes:        store.DefaultWebhookMaxBodyBytes,
				ReplayWindowSeconds: store.DefaultWebhookReplayWindowSeconds,
				ConfigVersion:       1,
				SecretVersion:       0,
				SignatureMode:       store.WebhookSignatureModeScionV1,
				Status:              store.WebhookEndpointStatusActive,
				CreatedBy:           ag.ID,
			},
		},
		{
			name: "bound_endpoint",
			ep: &store.WebhookEndpoint{
				ID:                  api.NewUUID(),
				ProjectID:           f.projectID,
				Name:                "m1-bound-ep",
				OwnerPrincipalKind:  store.DelegationPrincipalUser,
				OwnerPrincipalID:    f.owner.ID,
				OwnerCredentialKind: "api",
				BoundAgentID:        ag.ID,
				BoundLifetime:       store.WebhookBoundLifetimePersistent,
				AuthMode:            store.WebhookAuthModeHubToken,
				MaxBodyBytes:        store.DefaultWebhookMaxBodyBytes,
				ReplayWindowSeconds: store.DefaultWebhookReplayWindowSeconds,
				ConfigVersion:       1,
				SecretVersion:       0,
				SignatureMode:       store.WebhookSignatureModeScionV1,
				Status:              store.WebhookEndpointStatusActive,
				CreatedBy:           f.owner.ID,
			},
		},
		{
			name: "allow_project_agents_endpoint",
			ep: &store.WebhookEndpoint{
				ID:                  api.NewUUID(),
				ProjectID:           f.projectID,
				Name:                "m1-proj-agents-ep",
				OwnerPrincipalKind:  store.DelegationPrincipalUser,
				OwnerPrincipalID:    f.owner.ID,
				OwnerCredentialKind: "api",
				AllowProjectAgents:  true,
				AuthMode:            store.WebhookAuthModeHubToken,
				MaxBodyBytes:        store.DefaultWebhookMaxBodyBytes,
				ReplayWindowSeconds: store.DefaultWebhookReplayWindowSeconds,
				ConfigVersion:       1,
				SecretVersion:       0,
				SignatureMode:       store.WebhookSignatureModeScionV1,
				Status:              store.WebhookEndpointStatusActive,
				CreatedBy:           f.owner.ID,
			},
		},
	}
	for _, e := range endpoints {
		require.NoError(t, f.store.CreateWebhookEndpoint(ctx, e.ep))
	}

	mint := func(scopes []AgentTokenScope) string {
		tok, cred, mErr := f.srv.agentTokenService.SignAgentToken(AgentTokenGrant{
			AgentID:   ag.ID,
			ProjectID: ag.ProjectID,
			Scopes:    scopes,
		}, "")
		require.NoError(t, mErr)
		require.NoError(t, f.store.CreateAgentCredential(ctx, cred))
		return tok
	}

	tokenCases := []struct {
		name       string
		tok        string
		wantStatus int
	}{
		{"readonly_role", mint(ScopesForRole(AgentRoleReadOnly)), http.StatusForbidden},
		{"default_status_only", mint(nil), http.StatusForbidden},
		{"explicit_status_only", mint([]AgentTokenScope{ScopeAgentStatusUpdate}), http.StatusForbidden},
		{"baseline_role", mint(ScopesForRole(AgentRoleBaseline)), http.StatusAccepted},
		{"full_role", mint(ScopesForRole(AgentRoleFull)), http.StatusAccepted},
	}

	for _, e := range endpoints {
		for _, tc := range tokenCases {
			t.Run(e.name+"/"+tc.name, func(t *testing.T) {
				req := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/"+e.ep.ID, bytes.NewReader([]byte(`{"m1":"check"}`)))
				req.RemoteAddr = "198.51.100.91:4000"
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("X-Scion-Agent-Token", tc.tok)
				rec := httptest.NewRecorder()
				f.srv.Handler().ServeHTTP(rec, req)
				assert.Equal(t, tc.wantStatus, rec.Code, rec.Body.String())
			})
		}
	}
}
