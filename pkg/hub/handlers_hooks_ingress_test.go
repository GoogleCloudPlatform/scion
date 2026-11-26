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
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/durablequeue"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

func createTestWebhookEndpointViaAPI(t *testing.T, f *triggersTestFixture, projectID, name string) WebhookEndpointCreatedResponse {
	t.Helper()
	body, err := json.Marshal(CreateWebhookEndpointRequest{
		Name:        name,
		Description: "test endpoint " + name,
	})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+projectID+"/webhook-endpoints", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), f.ownerAuth))
	rec := httptest.NewRecorder()
	f.srv.mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var created WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	require.NotEmpty(t, created.ID)
	require.NotEmpty(t, created.Secret)
	return created
}

func sendSignedHookHTTP(
	srv *Server,
	endpointID, secret string,
	ts int64,
	delivery, eventHdr, subjectHdr, contentType, remoteAddr string,
	extraHeaders map[string]string,
	payload []byte,
) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/"+endpointID, bytes.NewReader(payload))
	if remoteAddr != "" {
		req.RemoteAddr = remoteAddr
	} else {
		req.RemoteAddr = "198.51.100.10:44321"
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if delivery != "" {
		req.Header.Set(trigger.HeaderDelivery, delivery)
	}
	if eventHdr != "" {
		req.Header.Set(trigger.HeaderEvent, eventHdr)
	}
	if subjectHdr != "" {
		req.Header.Set(trigger.HeaderSubject, subjectHdr)
	}
	if secret != "" {
		req.Header.Set(trigger.HeaderSignature, trigger.SignScionV1(secret, ts, delivery, eventHdr, subjectHdr, payload))
	}
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestWebhookIngress_ExperimentOffReturns404(t *testing.T) {
	f := setupTriggersTestFixture(t)
	ep := createTestWebhookEndpointViaAPI(t, f, f.projectID, "flag-off-hook")

	enableTriggersExperimentForTest(t, f.srv, false)

	payload := []byte(`{"ok":true}`)
	rec := sendSignedHookHTTP(f.srv, ep.ID, ep.Secret, time.Now().Unix(), "d-1", "", "", "application/json", "", nil, payload)
	assert.Equal(t, http.StatusNotFound, rec.Code)

	getReq := httptest.NewRequest(http.MethodGet, "/api/v1/hooks/"+ep.ID, nil)
	getReq.Header.Set("Authorization", "Bearer "+testDevToken)
	getRec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(getRec, getReq)
	assert.Equal(t, http.StatusNotFound, getRec.Code, "authenticated GET must also 404 (not 405) when hub.triggers is off")

	evs, err := f.store.ListTriggerEvents(context.Background(), store.TriggerEventFilter{ProjectID: f.projectID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Zero(t, evs.TotalCount)
}

func TestWebhookIngress_MethodPathAndMiddlewarePlumbing(t *testing.T) {
	f := setupTriggersTestFixture(t)
	ep := createTestWebhookEndpointViaAPI(t, f, f.projectID, "plumbing-hook")
	now := time.Now().Unix()
	payload := []byte(`{"hello":"world"}`)

	// 1. Dual-posture route (§7.1): isWebhookIngressRequest admits POST only.
	//    - Credential-less non-POST methods are rejected 401 by UnifiedAuthMiddleware.
	//    - Authenticated non-POST methods reach the handler and return 405 with Allow: POST.
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		anonReq := httptest.NewRequest(method, "/api/v1/hooks/"+ep.ID, nil)
		anonRec := httptest.NewRecorder()
		f.srv.Handler().ServeHTTP(anonRec, anonReq)
		assert.Equal(t, http.StatusUnauthorized, anonRec.Code, "credential-less %s must be 401", method)

		authReq := httptest.NewRequest(method, "/api/v1/hooks/"+ep.ID, nil)
		authReq.Header.Set("Authorization", "Bearer "+testDevToken)
		authRec := httptest.NewRecorder()
		f.srv.Handler().ServeHTTP(authRec, authReq)
		assert.Equal(t, http.StatusMethodNotAllowed, authRec.Code, "authenticated %s", method)
		assert.Equal(t, http.MethodPost, authRec.Header().Get("Allow"), "authenticated %s", method)
	}

	// 2. 36-char lowercase hex non-UUID matches unauth route pattern and is rejected 404 by handler.
	nonUUID36 := "550e8400e29b41d4a7164466554400000000"
	recNonUUID := sendSignedHookHTTP(f.srv, nonUUID36, ep.Secret, now, "", "", "", "application/json", "", nil, payload)
	assert.Equal(t, http.StatusNotFound, recNonUUID.Code)

	// 3. Exact unauthenticated-route match (§8.1 step 1): /api/v1/hooks/x/y and
	// /api/v1/hooks/<uuid>/extra are NOT unauthenticated and return 401 without auth.
	for _, path := range []string{
		"/api/v1/hooks/x/y",
		"/api/v1/hooks/" + ep.ID + "/extra",
		"/api/v1/hooks/" + strings.ToUpper(ep.ID),
	} {
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		f.srv.Handler().ServeHTTP(rec, req)
		assert.Equal(t, http.StatusUnauthorized, rec.Code, "subpath %s must require auth", path)
	}

	// 4. Dual-posture credential handling (§7.1) on an hmac endpoint:
	//    - Valid dev Bearer token WITHOUT X-Scion-Signature still returns 401 invalid_signature (hmac ignores identity).
	//    - Invalid Bearer token WITH valid X-Scion-Signature returns 401 BEFORE the handler runs (middleware rejects bad credentials).
	//    - Credential-less POST WITH valid X-Scion-Signature succeeds 202 (Stage 1 unchanged).
	recBearerNoSig := sendSignedHookHTTP(f.srv, ep.ID, "", now, "", "", "", "application/json", "", map[string]string{
		"Authorization": "Bearer " + testDevToken,
	}, payload)
	assert.Equal(t, http.StatusUnauthorized, recBearerNoSig.Code)

	recGarbageBearerValidSig := sendSignedHookHTTP(f.srv, ep.ID, ep.Secret, now, "deliv-bad-bearer", "", "", "application/json", "", map[string]string{
		"Authorization": "Bearer invalid-garbage-token",
	}, payload)
	assert.Equal(t, http.StatusUnauthorized, recGarbageBearerValidSig.Code, "invalid bearer must be rejected 401 before handler")

	recCredlessValidSig := sendSignedHookHTTP(f.srv, ep.ID, ep.Secret, now, "deliv-credless-ok", "", "", "application/json", "", nil, payload)
	require.Equal(t, http.StatusAccepted, recCredlessValidSig.Code, recCredlessValidSig.Body.String())

	// 5. proxyModeWriteGuard passes server-to-server webhook POSTs (no Origin, no Sec-Fetch-Site).
	f.srv.config.AuthMode = "proxy"
	t.Cleanup(func() { f.srv.config.AuthMode = "" })
	recProxyMode := sendSignedHookHTTP(f.srv, ep.ID, ep.Secret, now, "deliv-proxy-mode", "", "", "application/json", "", nil, payload)
	require.Equal(t, http.StatusAccepted, recProxyMode.Code, recProxyMode.Body.String())
}

func TestWebhookIngress_SignatureAndHeaderRefusals(t *testing.T) {
	f := setupTriggersTestFixture(t)
	ep := createTestWebhookEndpointViaAPI(t, f, f.projectID, "sig-hook")
	now := time.Now().UTC()
	payload := []byte(`{"build":42}`)

	// Missing signature -> baseline 401 body.
	recMissing := sendSignedHookHTTP(f.srv, ep.ID, "", now.Unix(), "", "", "", "application/json", "", nil, payload)
	require.Equal(t, http.StatusUnauthorized, recMissing.Code)
	want401Body := recMissing.Body.String()

	// Malformed signature, stale (-301s), future (+301s), wrong secret -> identical 401 body.
	badSigCases := []struct {
		name string
		hdr  string
	}{
		{name: "malformed", hdr: "not-a-valid-sig"},
		{name: "stale timestamp (-301s)", hdr: trigger.SignScionV1(ep.Secret, now.Unix()-301, "", "", "", payload)},
		{name: "future timestamp (+301s)", hdr: trigger.SignScionV1(ep.Secret, now.Unix()+301, "", "", "", payload)},
		{name: "wrong secret", hdr: trigger.SignScionV1("wrong-secret", now.Unix(), "", "", "", payload)},
		{
			name: "base64-decoded bytes key instead of ASCII secret string (rev 8.15)",
			hdr: func() string {
				decoded, err := base64.RawURLEncoding.DecodeString(ep.Secret)
				require.NoError(t, err)
				ts := strconv.FormatInt(now.Unix(), 10)
				mac := hmac.New(sha256.New, decoded)
				_, _ = mac.Write([]byte(fmt.Sprintf("v1\n%s\n\n\n\n%s", ts, string(payload))))
				return "t=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))
			}(),
		},
	}
	for _, tc := range badSigCases {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/"+ep.ID, bytes.NewReader(payload))
		req.RemoteAddr = "198.51.100.11:1234"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(trigger.HeaderSignature, tc.hdr)
		rec := httptest.NewRecorder()
		f.srv.Handler().ServeHTTP(rec, req)
		assert.Equal(t, http.StatusUnauthorized, rec.Code, tc.name)
		assert.JSONEq(t, want401Body, rec.Body.String(), "%s must return identical 401 body", tc.name)
	}

	// Re-labelled header with a valid signature over the body -> 401 identical body.
	validSigForDeploy := trigger.SignScionV1(ep.Secret, now.Unix(), "deliv-orig", "deploy.ok", "svc/a", payload)
	for _, relabel := range []struct {
		name     string
		delivery string
		event    string
		subject  string
	}{
		{name: "re-labelled X-Scion-Event", delivery: "deliv-orig", event: "deploy.hijack", subject: "svc/a"},
		{name: "re-labelled X-Scion-Subject", delivery: "deliv-orig", event: "deploy.ok", subject: "svc/b"},
		{name: "re-labelled X-Scion-Delivery", delivery: "deliv-new", event: "deploy.ok", subject: "svc/a"},
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/"+ep.ID, bytes.NewReader(payload))
		req.RemoteAddr = "198.51.100.12:1234"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(trigger.HeaderSignature, validSigForDeploy)
		req.Header.Set(trigger.HeaderDelivery, relabel.delivery)
		req.Header.Set(trigger.HeaderEvent, relabel.event)
		req.Header.Set(trigger.HeaderSubject, relabel.subject)
		rec := httptest.NewRecorder()
		f.srv.Handler().ServeHTTP(rec, req)
		assert.Equal(t, http.StatusUnauthorized, rec.Code, relabel.name)
		assert.JSONEq(t, want401Body, rec.Body.String(), relabel.name)
	}

	// Unsigned request with invalid X-Scion-Event -> 401 (not 400); validly signed -> 400 (§8.1 step 7).
	recUnsignedBadEvent := sendSignedHookHTTP(f.srv, ep.ID, "", now.Unix(), "", "BAD EVENT!", "", "application/json", "198.51.100.13:1234", nil, payload)
	assert.Equal(t, http.StatusUnauthorized, recUnsignedBadEvent.Code)
	assert.JSONEq(t, want401Body, recUnsignedBadEvent.Body.String())

	recSignedBadEvent := sendSignedHookHTTP(f.srv, ep.ID, ep.Secret, now.Unix(), "", "BAD EVENT!", "", "application/json", "198.51.100.13:1234", nil, payload)
	assert.Equal(t, http.StatusBadRequest, recSignedBadEvent.Code)

	recSignedLongDelivery := sendSignedHookHTTP(f.srv, ep.ID, ep.Secret, now.Unix(), strings.Repeat("d", 129), "", "", "application/json", "198.51.100.13:1234", nil, payload)
	assert.Equal(t, http.StatusBadRequest, recSignedLongDelivery.Code)

	recSignedControlSubject := sendSignedHookHTTP(f.srv, ep.ID, ep.Secret, now.Unix(), "", "", "bad\x07subject", "application/json", "198.51.100.13:1234", nil, payload)
	assert.Equal(t, http.StatusBadRequest, recSignedControlSubject.Code)

	// Secret unsealing failure (nil cipher after cache invalidation) -> fail-closed 503.
	f.srv.webhookIngress.InvalidateEndpoint(ep.ID)
	savedCipher := f.srv.cipher
	f.srv.cipher = nil
	recUnseal503 := sendSignedHookHTTP(f.srv, ep.ID, ep.Secret, now.Unix(), "", "", "", "application/json", "198.51.100.14:1234", nil, payload)
	f.srv.cipher = savedCipher
	assert.Equal(t, http.StatusServiceUnavailable, recUnseal503.Code)
	assert.NotContains(t, recUnseal503.Body.String(), ep.Secret)

	// Zero trigger_events or durable_queue_items rows written across all refusals.
	evs, err := f.store.ListTriggerEvents(context.Background(), store.TriggerEventFilter{ProjectID: f.projectID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Zero(t, evs.TotalCount)
	dqs := f.store.(store.DurableQueueStore)
	stats, err := dqs.DurableQueueStats(context.Background(), trigger.EventQueueKind, f.projectID, now)
	require.NoError(t, err)
	assert.Zero(t, stats.Pending)
}

func TestWebhookIngress_BodyBoundsAndMediaType(t *testing.T) {
	f := setupTriggersTestFixture(t)
	ep := createTestWebhookEndpointViaAPI(t, f, f.projectID, "bounds-hook")
	now := time.Now().Unix()

	// Non-JSON Content-Type -> 415.
	for _, ct := range []string{"text/plain", "application/xml", "application/x-www-form-urlencoded"} {
		rec := sendSignedHookHTTP(f.srv, ep.ID, ep.Secret, now, "", "", "", ct, "", nil, []byte(`{"a":1}`))
		assert.Equal(t, http.StatusUnsupportedMediaType, rec.Code, "Content-Type %s", ct)
	}
	// Missing Content-Type -> 415.
	recNoCT := sendSignedHookHTTP(f.srv, ep.ID, ep.Secret, now, "", "", "", "", "", nil, []byte(`{"a":1}`))
	assert.Equal(t, http.StatusUnsupportedMediaType, recNoCT.Code)

	// Invalid JSON / empty body -> 400.
	recBadJSON := sendSignedHookHTTP(f.srv, ep.ID, ep.Secret, now, "", "", "", "application/json", "", nil, []byte(`{"broken":`))
	assert.Equal(t, http.StatusBadRequest, recBadJSON.Code)
	recEmpty := sendSignedHookHTTP(f.srv, ep.ID, ep.Secret, now, "", "", "", "application/json", "", nil, nil)
	assert.Equal(t, http.StatusBadRequest, recEmpty.Code)

	// Oversize (> 64 KiB) -> 413; exact 64 KiB -> 202.
	padLen := trigger.MaxPayloadBytes - len(`{"p":""}`)
	overBody := []byte(`{"p":"` + strings.Repeat("a", padLen+1) + `"}`)
	require.Len(t, overBody, trigger.MaxPayloadBytes+1)
	rec413 := sendSignedHookHTTP(f.srv, ep.ID, ep.Secret, now, "deliv-over", "", "", "application/json", "", nil, overBody)
	assert.Equal(t, http.StatusRequestEntityTooLarge, rec413.Code)

	exactBody := []byte(`{"p":"` + strings.Repeat("a", padLen) + `"}`)
	require.Len(t, exactBody, trigger.MaxPayloadBytes)
	rec202 := sendSignedHookHTTP(f.srv, ep.ID, ep.Secret, now, "deliv-exact-64k", "", "", "application/json; charset=utf-8", "", nil, exactBody)
	require.Equal(t, http.StatusAccepted, rec202.Code, rec202.Body.String())

	evs, err := f.store.ListTriggerEvents(context.Background(), store.TriggerEventFilter{ProjectID: f.projectID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, evs.TotalCount, "only the exact-64KiB accepted request may persist an event row")
}

func TestWebhookIngress_UnknownDisabledAndDeletedEndpointsIdentical404(t *testing.T) {
	f := setupTriggersTestFixture(t)
	ctx := context.Background()
	now := time.Now().Unix()
	payload := []byte(`{"ping":true}`)

	// Seed a disabled endpoint via TriggerStore.
	disabledID := uuid.NewString()
	require.NoError(t, f.store.CreateWebhookEndpoint(ctx, &store.WebhookEndpoint{
		ID:                  disabledID,
		ProjectID:           f.projectID,
		Name:                "disabled-hook",
		OwnerPrincipalKind:  store.DelegationPrincipalUser,
		OwnerPrincipalID:    f.owner.ID,
		OwnerCredentialKind: "api",
		SecretVersion:       1,
		SignatureMode:       store.WebhookSignatureModeScionV1,
		Status:              store.WebhookEndpointStatusDisabled,
		CreatedBy:           f.owner.ID,
	}))

	recUnknown := sendSignedHookHTTP(f.srv, uuid.NewString(), "any", now, "", "", "", "application/json", "", nil, payload)
	recDisabled := sendSignedHookHTTP(f.srv, disabledID, "any", now, "", "", "", "application/json", "", nil, payload)
	assert.Equal(t, http.StatusNotFound, recUnknown.Code)
	assert.Equal(t, http.StatusNotFound, recDisabled.Code)
	assert.JSONEq(t, recUnknown.Body.String(), recDisabled.Body.String(),
		"unknown and disabled endpoints must return identical 404 body (no enumeration oracle)")

	// Create active endpoint, send valid POST (populates 5m positive + secret caches),
	// then DELETE endpoint via API -> cache invalidated immediately, next POST gets identical 404.
	active := createTestWebhookEndpointViaAPI(t, f, f.projectID, "ephemeral-hook")
	recOK := sendSignedHookHTTP(f.srv, active.ID, active.Secret, now, "d-before-del", "", "", "application/json", "", nil, payload)
	require.Equal(t, http.StatusAccepted, recOK.Code)

	delRec := f.doWebhookEndpoint(t, f.ownerAuth, active.ID, http.MethodDelete, nil)
	require.Equal(t, http.StatusNoContent, delRec.Code)

	recAfterDel := sendSignedHookHTTP(f.srv, active.ID, active.Secret, now, "d-after-del", "", "", "application/json", "", nil, payload)
	assert.Equal(t, http.StatusNotFound, recAfterDel.Code)
	assert.JSONEq(t, recUnknown.Body.String(), recAfterDel.Body.String())
}

func TestWebhookIngress_SplitBucketsIsolationAndIPv6Slash64(t *testing.T) {
	f := setupTriggersTestFixture(t)
	now := time.Date(2026, 11, 16, 20, 0, 0, 0, time.UTC)

	epProj1A := createTestWebhookEndpointViaAPI(t, f, f.projectID, "proj1-ep-a")
	epProj1B := createTestWebhookEndpointViaAPI(t, f, f.projectID, "proj1-ep-b")
	epProj2 := createTestWebhookEndpointViaAPI(t, f, f.otherProjID, "proj2-ep")

	// Install deterministic tight-burst Ingress on f.srv so all 5 split buckets
	// and trusted-proxy X-Forwarded-For / IPv6 /64 keying are exercised through srv.Handler().
	trustedNets := parseTrustedProxies([]string{"10.0.0.0/8"})
	dqs := f.store.(store.DurableQueueStore)
	f.srv.webhookIngress = trigger.NewIngress(trigger.IngressConfig{
		Store:      f.store,
		QueueStore: dqs,
		Queue:      f.srv.durableQueue,
		LoadSecret: f.srv.loadWebhookEndpointSecret,
		ClientIP: func(r *http.Request) string {
			return trustedClientIP(r, trustedNets)
		},
		Enabled: func() bool {
			return f.srv.experimentEnabled(experiments.Triggers)
		},
		Limiter: trigger.NewIngressLimiter(trigger.LimiterConfig{
			PerIPRate:               0.001, // effectively no refill during test
			PerIPBurst:              2,
			RejectionRate:           0.001,
			RejectionBurst:          3,
			PerEndpointRate:         0.001,
			PerEndpointBurst:        3,
			PerProjectVerifiedRate:  0.001,
			PerProjectVerifiedBurst: 4,
			HubVerifiedRate:         0.001,
			HubVerifiedBurst:        5,
			Now:                     func() time.Time { return now },
		}),
		Now: func() time.Time { return now },
	})

	payload := []byte(`{"ok":true}`)

	// 1. Per-IP bucket with IPv6 /64 keying (via trusted proxy 10.1.2.3):
	//    Two distinct /128 addresses in 2001:db8:cafe:1::/64 share the burst of 2!
	r1 := sendSignedHookHTTP(f.srv, epProj1A.ID, epProj1A.Secret, now.Unix(), "", "", "", "application/json", "10.1.2.3:9000", map[string]string{
		"X-Forwarded-For": "2001:db8:cafe:1::1",
	}, payload)
	require.Equal(t, http.StatusAccepted, r1.Code)

	r2 := sendSignedHookHTTP(f.srv, epProj1A.ID, epProj1A.Secret, now.Unix(), "", "", "", "application/json", "10.1.2.3:9000", map[string]string{
		"X-Forwarded-For": "2001:db8:cafe:1:dead:beef:1234:5678",
	}, payload)
	require.Equal(t, http.StatusAccepted, r2.Code)

	r3 := sendSignedHookHTTP(f.srv, epProj1A.ID, epProj1A.Secret, now.Unix(), "", "", "", "application/json", "10.1.2.3:9000", map[string]string{
		"X-Forwarded-For": "2001:db8:cafe:1::ffff",
	}, payload)
	assert.Equal(t, http.StatusTooManyRequests, r3.Code, "third request from same IPv6 /64 must hit 429")
	assert.Equal(t, "1", r3.Header().Get("Retry-After"))

	// 2. Junk flood to unknown IDs (from distinct client IPs) exhausts ONLY the
	//    hub-wide rejection bucket (burst 3 -> 4th gets 429), while known active
	//    endpoint epProj1B's signed request STILL succeeds 202!
	for i := 0; i < 3; i++ {
		rec := sendSignedHookHTTP(f.srv, uuid.NewString(), "junk", now.Unix(), "", "", "", "application/json", fmt.Sprintf("203.0.113.%d:4000", 10+i), nil, payload)
		assert.Equal(t, http.StatusNotFound, rec.Code)
	}
	recRej429 := sendSignedHookHTTP(f.srv, uuid.NewString(), "junk", now.Unix(), "", "", "", "application/json", "203.0.113.20:4000", nil, payload)
	assert.Equal(t, http.StatusTooManyRequests, recRej429.Code)
	assert.Equal(t, "1", recRej429.Header().Get("Retry-After"))

	// Signed request to epProj1B is completely unaffected by rejection-bucket exhaustion!
	recKnownOK := sendSignedHookHTTP(f.srv, epProj1B.ID, epProj1B.Secret, now.Unix(), "", "", "", "application/json", "203.0.113.30:4000", nil, payload)
	require.Equal(t, http.StatusAccepted, recKnownOK.Code, recKnownOK.Body.String())

	// 3. Bad-signature flood on epProj1A exhausts epProj1A's per-endpoint bucket
	//    (2 already used above + 1 bad sig = 3 -> next gets 429), leaving epProj1B unaffected (R2)!
	recBadSigA := sendSignedHookHTTP(f.srv, epProj1A.ID, "wrong-secret", now.Unix(), "", "", "", "application/json", "203.0.113.40:4000", nil, payload)
	assert.Equal(t, http.StatusUnauthorized, recBadSigA.Code)
	recEpA429 := sendSignedHookHTTP(f.srv, epProj1A.ID, epProj1A.Secret, now.Unix(), "", "", "", "application/json", "203.0.113.41:4000", nil, payload)
	assert.Equal(t, http.StatusTooManyRequests, recEpA429.Code)
	assert.Equal(t, "1", recEpA429.Header().Get("Retry-After"))

	// 4. Per-project verified bucket on Project 1 (burst 4: 2 used on epProj1A + 1 used on epProj1B = 3):
	//    One more signed request on epProj1B hits 4 (202), then the 5th verified request on Project 1
	//    hits per-project verified 429, while Project 2 (epProj2) STILL succeeds 202 (R3)!
	recProj1Fourth := sendSignedHookHTTP(f.srv, epProj1B.ID, epProj1B.Secret, now.Unix(), "", "", "", "application/json", "203.0.113.50:4000", nil, payload)
	require.Equal(t, http.StatusAccepted, recProj1Fourth.Code)

	recProj1Verified429 := sendSignedHookHTTP(f.srv, epProj1B.ID, epProj1B.Secret, now.Unix(), "", "", "", "application/json", "203.0.113.51:4000", nil, payload)
	assert.Equal(t, http.StatusTooManyRequests, recProj1Verified429.Code)
	assert.Equal(t, "1", recProj1Verified429.Header().Get("Retry-After"))

	// Project 2 is unaffected by Project 1's verified-bucket exhaustion, then hits
	// hub-wide verified bucket (burst 5: 4 used by Project 1 + 1 by Project 2 = 5 -> 6th gets 429)!
	recProj2OK := sendSignedHookHTTP(f.srv, epProj2.ID, epProj2.Secret, now.Unix(), "", "", "", "application/json", "203.0.113.60:4000", nil, payload)
	require.Equal(t, http.StatusAccepted, recProj2OK.Code)

	recHubVerified429 := sendSignedHookHTTP(f.srv, epProj2.ID, epProj2.Secret, now.Unix(), "", "", "", "application/json", "203.0.113.61:4000", nil, payload)
	assert.Equal(t, http.StatusTooManyRequests, recHubVerified429.Code)
	assert.Equal(t, "1", recHubVerified429.Header().Get("Retry-After"))
}

func TestWebhookIngress_HappyPathDuplicateAuditAndNoSecretLeak(t *testing.T) {
	f := setupTriggersTestFixture(t)
	ctx := context.Background()

	// Capture audit log records to verify receipt & failure audit format and ensure
	// neither secret material nor payload body ever appears in audit logs.
	var auditBuf bytes.Buffer
	auditLogger := slog.New(slog.NewJSONHandler(&auditBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	dqs := f.store.(store.DurableQueueStore)
	var forcedEventID string
	f.srv.webhookIngress = trigger.NewIngress(trigger.IngressConfig{
		Store:      f.store,
		QueueStore: dqs,
		Queue:      f.srv.durableQueue,
		LoadSecret: f.srv.loadWebhookEndpointSecret,
		Enabled:    func() bool { return f.srv.experimentEnabled(experiments.Triggers) },
		Audit:      trigger.NewSlogAuditSink(trigger.SlogAuditConfig{Logger: auditLogger}),
		NewEventID: func() string { return forcedEventID },
	})

	ep := createTestWebhookEndpointViaAPI(t, f, f.projectID, "ci-hook")
	assert.Equal(t, "https://hub-public.example.com/api/v1/hooks/"+ep.ID, ep.URL)
	sentinelSecret := ep.Secret
	sentinelBodySecret := "SUPER_SENSITIVE_BODY_MARKER_99182"
	payload := []byte(`{"marker":"` + sentinelBodySecret + `","ref":"refs/heads/main"}`)
	now := time.Now().UTC()

	// 1. Happy 202 queued + event row + queue item in one transaction.
	rec1 := sendSignedHookHTTP(f.srv, ep.ID, ep.Secret, now.Unix(), "gh-deliv-1", "push.completed", "repo/scion", "application/json", "198.51.100.77:5555", map[string]string{
		"User-Agent": "WebhookSender/2.0",
	}, payload)
	require.Equal(t, http.StatusAccepted, rec1.Code, rec1.Body.String())
	assert.NotContains(t, rec1.Body.String(), sentinelSecret)

	var accepted1 trigger.IngressAcceptedResponse
	require.NoError(t, json.Unmarshal(rec1.Body.Bytes(), &accepted1))
	assert.Equal(t, trigger.IngressStatusQueued, accepted1.Status)
	require.NotEmpty(t, accepted1.EventID)

	// 2a. Duplicate X-Scion-Delivery (trigger_events (source_kind, source_id, dedup_key) conflict -> store.ErrAlreadyExists)
	// returns 202 {"status":"duplicate"} with same eventId and no second row.
	recDup := sendSignedHookHTTP(f.srv, ep.ID, ep.Secret, now.Unix(), "gh-deliv-1", "push.completed", "repo/scion", "application/json", "198.51.100.77:5555", nil, payload)
	require.Equal(t, http.StatusAccepted, recDup.Code, recDup.Body.String())
	var acceptedDup trigger.IngressAcceptedResponse
	require.NoError(t, json.Unmarshal(recDup.Body.Bytes(), &acceptedDup))
	assert.Equal(t, accepted1.EventID, acceptedDup.EventID)
	assert.Equal(t, trigger.IngressStatusDuplicate, acceptedDup.Status)

	// 2b. An event-ID collision in durable_queue_items (durablequeue.ErrDuplicate)
	// rolls back the trigger_events insert and returns 500 (never 202 "duplicate")
	// so the sender retries.
	preQueuedID := uuid.NewString()
	_, err := f.srv.durableQueue.Enqueue(ctx, nil, durablequeue.Item{
		Kind:      trigger.EventQueueKind,
		DedupKey:  preQueuedID,
		ProjectID: f.projectID,
		Payload:   []byte(`{"v":1,"event_id":"` + preQueuedID + `"}`),
	})
	require.NoError(t, err)
	forcedEventID = preQueuedID
	recQueueDup := sendSignedHookHTTP(f.srv, ep.ID, ep.Secret, now.Unix(), "gh-deliv-queue-dup", "push.completed", "repo/scion", "application/json", "198.51.100.77:5555", nil, payload)
	forcedEventID = ""
	require.Equal(t, http.StatusInternalServerError, recQueueDup.Code, recQueueDup.Body.String())
	_, err = f.store.GetTriggerEvent(ctx, preQueuedID)
	require.ErrorIs(t, err, store.ErrNotFound, "event row must roll back when queue Enqueue returns ErrDuplicate")

	// 3. Trigger a signature failure as well so failure audit is also exercised.
	recFail := sendSignedHookHTTP(f.srv, ep.ID, "wrong-secret", now.Unix(), "gh-deliv-bad", "push.completed", "repo/scion", "application/json", "198.51.100.77:5555", nil, payload)
	assert.Equal(t, http.StatusUnauthorized, recFail.Code)
	assert.NotContains(t, recFail.Body.String(), sentinelSecret)

	// Inspect trigger_events strictly through TriggerStore (never raw SQL).
	listRes, err := f.store.ListTriggerEvents(ctx, store.TriggerEventFilter{ProjectID: f.projectID}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, listRes.Items, 1)

	storedEv, err := f.store.GetTriggerEvent(ctx, accepted1.EventID)
	require.NoError(t, err)
	assert.Equal(t, "webhook.push.completed", storedEv.Type)
	assert.Equal(t, "webhook:"+ep.ID+"/repo/scion", storedEv.Subject)
	assert.Equal(t, "gh-deliv-1", storedEv.DedupKey)
	assert.Equal(t, string(payload), storedEv.Payload)
	assert.Equal(t, store.TriggerEventStatusPending, storedEv.Status)
	assert.NotContains(t, fmt.Sprintf("%+v", storedEv.Headers), sentinelSecret)
	assert.NotContains(t, fmt.Sprintf("%+v", storedEv.Attributes), sentinelSecret)

	// Inspect durable queue items via DurableQueueStore (1 from rec1 + 1 pre-seeded for queue-dup check).
	claimed, err := dqs.ClaimDurableItems(ctx, trigger.EventQueueKind, "test-drainer", now.Add(time.Second), now.Add(2*time.Minute), 10)
	require.NoError(t, err)
	require.Len(t, claimed, 2)
	assert.Equal(t, accepted1.EventID, claimed[0].DedupKey)
	assert.Equal(t, f.projectID, claimed[0].ProjectID)

	// Flush accumulated counters via Close and verify GET /webhook-endpoints/{id} reflects counts without leaking secret.
	f.srv.webhookIngress.Close(ctx)
	getRec := f.doWebhookEndpoint(t, f.ownerAuth, ep.ID, http.MethodGet, nil)
	require.Equal(t, http.StatusOK, getRec.Code)
	assert.NotContains(t, getRec.Body.String(), sentinelSecret)
	var gotEp WebhookEndpointResponse
	require.NoError(t, json.Unmarshal(getRec.Body.Bytes(), &gotEp))
	assert.Equal(t, 1, gotEp.ReceiveCount)
	assert.Equal(t, 1, gotEp.RejectCount)
	require.NotNil(t, gotEp.LastReceivedAt)

	// Audit log contains neither endpoint secret nor payload body content.
	auditOutput := auditBuf.String()
	assert.Contains(t, auditOutput, trigger.AuditEventReceived)
	assert.Contains(t, auditOutput, trigger.AuditEventSignatureFailed)
	assert.NotContains(t, auditOutput, sentinelSecret, "audit log must never contain endpoint secret")
	assert.NotContains(t, auditOutput, sentinelBodySecret, "audit log must never contain webhook payload body")
}

func seedPendingQueueItems(t *testing.T, s store.Store, projectID string, count int) {
	t.Helper()
	ctx := context.Background()
	dqs, ok := s.(store.DurableQueueStore)
	require.True(t, ok)
	q := durablequeue.NewQueue(dqs)

	const batchSize = 500
	for start := 0; start < count; start += batchSize {
		end := start + batchSize
		if end > count {
			end = count
		}
		require.NoError(t, s.WithTx(ctx, func(tx store.Store) error {
			for i := start; i < end; i++ {
				id := fmt.Sprintf("seed-%s-%05d", projectID[:8], i)
				if _, err := q.Enqueue(ctx, tx, durablequeue.Item{
					Kind:      trigger.EventQueueKind,
					DedupKey:  id,
					ProjectID: projectID,
					Payload:   []byte(`{"v":1,"event_id":"` + id + `"}`),
				}); err != nil {
					return err
				}
			}
			return nil
		}))
	}
}

func TestWebhookIngress_PendingCap429AndCrossProjectIsolation(t *testing.T) {
	f := setupTriggersTestFixture(t)
	epProj1 := createTestWebhookEndpointViaAPI(t, f, f.projectID, "cap-proj1")
	epProj2 := createTestWebhookEndpointViaAPI(t, f, f.otherProjID, "cap-proj2")

	now := time.Now().Unix()
	payload := []byte(`{"event":"at-cap"}`)

	// Accept one delivery first, then seed MaxPendingEventsPerProject - 1 more
	// queue items so Project 1 stands at Pending == MaxPendingEventsPerProject (5,000).
	recInitial := sendSignedHookHTTP(f.srv, epProj1.ID, epProj1.Secret, now, "deliv-before-cap", "", "", "application/json", "", nil, payload)
	require.Equal(t, http.StatusAccepted, recInitial.Code, recInitial.Body.String())
	var initialAccepted trigger.IngressAcceptedResponse
	require.NoError(t, json.Unmarshal(recInitial.Body.Bytes(), &initialAccepted))
	assert.Equal(t, trigger.IngressStatusQueued, initialAccepted.Status)

	seedPendingQueueItems(t, f.store, f.projectID, trigger.MaxPendingEventsPerProject-1)

	// Project 1 at 5,000 pending items rejects a NEW delivery with 429 "queue full" (Retry-After: 30) and writes zero rows.
	recFull := sendSignedHookHTTP(f.srv, epProj1.ID, epProj1.Secret, now, "deliv-cap-overflow", "", "", "application/json", "", nil, payload)
	assert.Equal(t, http.StatusTooManyRequests, recFull.Code)
	assert.Equal(t, "30", recFull.Header().Get("Retry-After"))
	assert.Contains(t, recFull.Body.String(), "queue full")

	// §6.2: a duplicate redelivery of an already-accepted X-Scion-Delivery at the
	// pending cap MUST still answer 202 {"status":"duplicate"} (never 429).
	recDupAtCap := sendSignedHookHTTP(f.srv, epProj1.ID, epProj1.Secret, now, "deliv-before-cap", "", "", "application/json", "", nil, payload)
	require.Equal(t, http.StatusAccepted, recDupAtCap.Code, recDupAtCap.Body.String())
	var dupAtCap trigger.IngressAcceptedResponse
	require.NoError(t, json.Unmarshal(recDupAtCap.Body.Bytes(), &dupAtCap))
	assert.Equal(t, initialAccepted.EventID, dupAtCap.EventID)
	assert.Equal(t, trigger.IngressStatusDuplicate, dupAtCap.Status)

	evsProj1, err := f.store.ListTriggerEvents(context.Background(), store.TriggerEventFilter{ProjectID: f.projectID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, evsProj1.TotalCount, "neither 429 overflow nor 202 duplicate at cap may insert a second event row")

	// Project 2 on the same hub is completely unaffected!
	recProj2 := sendSignedHookHTTP(f.srv, epProj2.ID, epProj2.Secret, now, "deliv-proj2-ok", "", "", "application/json", "", nil, payload)
	require.Equal(t, http.StatusAccepted, recProj2.Code, recProj2.Body.String())
}

type ingressLockOrderStore struct {
	store.Store
	store.DurableQueueStore
	store.DurableQueuePendingCounter
	mu     sync.Mutex
	opsLog []string
}

func (w *ingressLockOrderStore) record(op string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.opsLog = append(w.opsLog, op)
}

func (w *ingressLockOrderStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	w.record("WithTx:begin")
	return w.Store.WithTx(ctx, func(tx store.Store) error {
		txDQS, _ := tx.(store.DurableQueueStore)
		txCounter, _ := tx.(store.DurableQueuePendingCounter)
		txWrap := &ingressLockOrderTxStore{
			Store:                      tx,
			DurableQueueStore:          txDQS,
			DurableQueuePendingCounter: txCounter,
			parent:                     w,
		}
		return fn(txWrap)
	})
}

func (w *ingressLockOrderStore) CountDurableQueuePending(ctx context.Context, kind, projectID string) (int64, error) {
	w.record("outer.CountDurableQueuePending")
	return w.DurableQueuePendingCounter.CountDurableQueuePending(ctx, kind, projectID)
}

func (w *ingressLockOrderStore) DurableQueueStats(ctx context.Context, kind, projectID string, now time.Time) (store.DurableQueueStats, error) {
	w.record("outer.DurableQueueStats")
	return w.DurableQueueStore.DurableQueueStats(ctx, kind, projectID, now)
}

func (w *ingressLockOrderStore) EnqueueDurableItem(ctx context.Context, tx store.Tx, row store.DurableQueueRow) (int64, error) {
	w.record("tx.EnqueueDurableItem")
	if wrappedTx, ok := tx.(*ingressLockOrderTxStore); ok {
		tx = wrappedTx.Store
	}
	return w.DurableQueueStore.EnqueueDurableItem(ctx, tx, row)
}

type ingressLockOrderTxStore struct {
	store.Store
	store.DurableQueueStore
	store.DurableQueuePendingCounter
	parent *ingressLockOrderStore
}

func (t *ingressLockOrderTxStore) LockTriggerQuota(ctx context.Context, projectID string) error {
	t.parent.record("tx.LockTriggerQuota")
	return t.Store.LockTriggerQuota(ctx, projectID)
}

func (t *ingressLockOrderTxStore) GetTriggerEventByDedupKey(ctx context.Context, sourceKind, sourceID, dedupKey string) (*store.TriggerEvent, error) {
	t.parent.record("tx.GetTriggerEventByDedupKey")
	return t.Store.GetTriggerEventByDedupKey(ctx, sourceKind, sourceID, dedupKey)
}

func (t *ingressLockOrderTxStore) CountDurableQueuePending(ctx context.Context, kind, projectID string) (int64, error) {
	t.parent.record("tx.CountDurableQueuePending")
	return t.DurableQueuePendingCounter.CountDurableQueuePending(ctx, kind, projectID)
}

func (t *ingressLockOrderTxStore) DurableQueueStats(ctx context.Context, kind, projectID string, now time.Time) (store.DurableQueueStats, error) {
	t.parent.record("tx.DurableQueueStats")
	return t.DurableQueueStore.DurableQueueStats(ctx, kind, projectID, now)
}

func (t *ingressLockOrderTxStore) CreateTriggerEvent(ctx context.Context, ev *store.TriggerEvent) error {
	t.parent.record("tx.CreateTriggerEvent")
	return t.Store.CreateTriggerEvent(ctx, ev)
}

func (t *ingressLockOrderTxStore) EnqueueDurableItem(ctx context.Context, tx store.Tx, row store.DurableQueueRow) (int64, error) {
	t.parent.record("tx.EnqueueDurableItem")
	// Unwrap tx to the underlying Ent CompositeStore transaction handle.
	return t.DurableQueueStore.EnqueueDurableItem(ctx, t.Store, row)
}

func TestWebhookIngress_QuotaLockBeforeQueueStatsInTx(t *testing.T) {
	f := setupTriggersTestFixture(t)
	ep := createTestWebhookEndpointViaAPI(t, f, f.projectID, "lock-order-hook")

	wrapped := &ingressLockOrderStore{
		Store:                      f.store,
		DurableQueueStore:          f.store.(store.DurableQueueStore),
		DurableQueuePendingCounter: f.store.(store.DurableQueuePendingCounter),
	}
	f.srv.webhookIngress = trigger.NewIngress(trigger.IngressConfig{
		Store:      wrapped,
		QueueStore: wrapped,
		Queue:      durablequeue.NewQueue(wrapped),
		LoadSecret: f.srv.loadWebhookEndpointSecret,
		Enabled:    func() bool { return f.srv.experimentEnabled(experiments.Triggers) },
	})

	rec := sendSignedHookHTTP(f.srv, ep.ID, ep.Secret, time.Now().Unix(), "deliv-lock-order", "", "", "application/json", "", nil, []byte(`{"lock":true}`))
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	wrapped.mu.Lock()
	ops := append([]string(nil), wrapped.opsLog...)
	wrapped.mu.Unlock()
	assert.Equal(t, []string{
		"WithTx:begin",
		"tx.LockTriggerQuota",
		"tx.GetTriggerEventByDedupKey",
		"tx.CountDurableQueuePending",
		"tx.CreateTriggerEvent",
		"tx.EnqueueDurableItem",
	}, ops, "ingress transaction must acquire LockTriggerQuota, check dedup key, and run single-query CountDurableQueuePending before inserting event/queue rows")
}

func runWebhookIngressConcurrentPendingCapAtLastSlot(t *testing.T, f *triggersTestFixture) {
	t.Helper()
	ctx := context.Background()
	ep := createTestWebhookEndpointViaAPI(t, f, f.projectID, "race-cap-hook")

	// Seed MaxPendingEventsPerProject - 1 (4,999) pending items so exactly 1 slot remains.
	seedPendingQueueItems(t, f.store, f.projectID, trigger.MaxPendingEventsPerProject-1)

	const contenders = 8
	now := time.Now().Unix()
	codes := make([]int, contenders)
	retryAfters := make([]string, contenders)

	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			payload := []byte(fmt.Sprintf(`{"contender":%d}`, idx))
			delivery := fmt.Sprintf("race-deliv-%d", idx)
			remote := fmt.Sprintf("198.51.100.%d:5000", 100+idx)
			<-start
			rec := sendSignedHookHTTP(f.srv, ep.ID, ep.Secret, now, delivery, "", "", "application/json", remote, nil, payload)
			codes[idx] = rec.Code
			retryAfters[idx] = rec.Header().Get("Retry-After")
		}(i)
	}
	close(start)
	wg.Wait()

	accepted, limited := 0, 0
	for i, c := range codes {
		switch c {
		case http.StatusAccepted:
			accepted++
		case http.StatusTooManyRequests:
			limited++
			assert.Equal(t, "30", retryAfters[i])
		default:
			t.Fatalf("unexpected concurrent pending-cap status %d (codes=%v)", c, codes)
		}
	}
	assert.Equal(t, 1, accepted, "exactly 1 concurrent request at Pending=4999 may win")
	assert.Equal(t, contenders-1, limited)

	dqs := f.store.(store.DurableQueueStore)
	stats, err := dqs.DurableQueueStats(ctx, trigger.EventQueueKind, f.projectID, time.Now().UTC())
	require.NoError(t, err)
	assert.Equal(t, int64(trigger.MaxPendingEventsPerProject), stats.Pending)

	evs, err := f.store.ListTriggerEvents(ctx, store.TriggerEventFilter{ProjectID: f.projectID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, evs.TotalCount)
}

// TestWebhookIngress_ConcurrentPendingCap_LastSlot runs the last-slot pending
// cap race on SQLite (single-writer serialized; see
// TestWebhookIngress_QuotaLockBeforeQueueStatsInTx for lock-order proof and
// TestWebhookIngress_ConcurrentPendingCap_Postgres for multi-connection
// PostgreSQL READ COMMITTED).
func TestWebhookIngress_ConcurrentPendingCap_LastSlot(t *testing.T) {
	f := setupTriggersTestFixture(t)
	runWebhookIngressConcurrentPendingCapAtLastSlot(t, f)
}

// TestWebhookIngress_ConcurrentPendingCap_Postgres runs the last-slot pending
// cap race on PostgreSQL READ COMMITTED (wired into HUB_INTEGRATION_POSTGRES_TESTS),
// verifying pg_advisory_xact_lock(LockTriggerQuota, StableProjectHash(projectID))
// serializes concurrent ingress writers at Pending = 4999 (ptone/scion#8028).
func TestWebhookIngress_ConcurrentPendingCap_Postgres(t *testing.T) {
	if !enttest.Active() {
		t.Skip("requires -tags integration and SCION_TEST_POSTGRES_URL")
	}
	srv, s := testServerWithStore(t, entadapter.NewCompositeStore(enttest.NewClient(t)))
	f := setupTriggersTestFixtureOnServer(t, srv, s)
	runWebhookIngressConcurrentPendingCapAtLastSlot(t, f)
}

func TestWebhookEndpointIngressURL_EmptyPublicEndpointReturnsRelativePath(t *testing.T) {
	f := setupTriggersTestFixture(t)
	f.srv.config.HubEndpoint = ""
	f.srv.config.HubEndpoints = nil

	created := createTestWebhookEndpointViaAPI(t, f, f.projectID, "relative-url-hook")
	wantURL := "/api/v1/hooks/" + created.ID
	assert.Equal(t, wantURL, created.URL, "unconfigured public hub endpoint must return relative path, never a fabricated host")

	getRec := f.doWebhookEndpoint(t, f.ownerAuth, created.ID, http.MethodGet, nil)
	require.Equal(t, http.StatusOK, getRec.Code)
	var got WebhookEndpointResponse
	require.NoError(t, json.Unmarshal(getRec.Body.Bytes(), &got))
	assert.Equal(t, wantURL, got.URL)

	listRec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodGet, nil)
	require.Equal(t, http.StatusOK, listRec.Code)
	var listed ListWebhookEndpointsResponse
	require.NoError(t, json.Unmarshal(listRec.Body.Bytes(), &listed))
	require.Len(t, listed.Endpoints, 1)
	assert.Equal(t, wantURL, listed.Endpoints[0].URL)
}

func TestWebhookIngress_SyntheticTestRegistryBootsAndAnswers404(t *testing.T) {
	st, err := newTestStore(t, ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	cfg := DefaultServerConfig()
	cfg.Experiments = testRegistry(t)
	srv, err := newTestHubServer(t, cfg, st)
	require.NoError(t, err, "New() with a synthetic test registry lacking hub.triggers must boot without panicking")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/550e8400-e29b-41d4-a716-446655440000", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestWebhookIngress_RefusesNonInternalSecretRow(t *testing.T) {
	f := setupTriggersTestFixture(t)
	ctx := context.Background()
	ep := createTestWebhookEndpointViaAPI(t, f, f.projectID, "non-internal-secret-hook")
	key := store.WebhookEndpointSecretKey(f.projectID, ep.ID, 1)
	hubID := f.srv.HubID()

	plantedPlaintext := base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	plantedSealed, err := f.srv.sealWebhookEndpointSecret(plantedPlaintext)
	require.NoError(t, err)

	payload := []byte(`{"planted":true}`)
	now := time.Now().Unix()

	for _, tc := range []struct {
		secretType string
		target     string
	}{
		{secretType: store.SecretTypeEnvironment},
		{secretType: store.SecretTypeOnDemand},
		{secretType: store.SecretTypeFile, target: "/tmp/planted-secret"},
	} {
		require.NoError(t, f.store.UpdateSecret(ctx, &store.Secret{
			Key:            key,
			Scope:          store.ScopeHub,
			ScopeID:        hubID,
			SecretType:     tc.secretType,
			Target:         tc.target,
			EncryptedValue: plantedSealed,
			UpdatedBy:      "test-admin",
		}))

		_, loadErr := f.srv.loadWebhookEndpointSecret(ctx, f.projectID, ep.ID, 1)
		require.ErrorIs(t, loadErr, errWebhookSecretTypeMismatch, "type=%s", tc.secretType)

		recPlanted := sendSignedHookHTTP(f.srv, ep.ID, plantedPlaintext, now, "deliv-"+tc.secretType, "push", "", "application/json", "", nil, payload)
		assert.Equal(t, http.StatusServiceUnavailable, recPlanted.Code, "non-internal secret row (%s) must fail closed with 503", tc.secretType)
		assert.NotContains(t, recPlanted.Body.String(), plantedPlaintext)
		assert.NotContains(t, recPlanted.Body.String(), ep.Secret)

		recOrig := sendSignedHookHTTP(f.srv, ep.ID, ep.Secret, now, "deliv-orig-"+tc.secretType, "push", "", "application/json", "", nil, payload)
		assert.Equal(t, http.StatusServiceUnavailable, recOrig.Code, "non-internal secret row (%s) must fail closed before HMAC check", tc.secretType)
	}

	evs, err := f.store.ListTriggerEvents(ctx, store.TriggerEventFilter{ProjectID: f.projectID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Zero(t, evs.TotalCount, "refused requests must persist no trigger_events rows")

	dqs, ok := f.store.(store.DurableQueueStore)
	require.True(t, ok)
	stats, err := dqs.DurableQueueStats(ctx, trigger.EventQueueKind, f.projectID, time.Now().UTC())
	require.NoError(t, err)
	assert.Zero(t, stats.Pending, "refused requests must enqueue no durable_queue_items rows")

	// Restoring SecretTypeInternal recovers immediately because secret-load
	// errors are never cached in EndpointCache.
	origSealed, err := f.srv.sealWebhookEndpointSecret(ep.Secret)
	require.NoError(t, err)
	require.NoError(t, f.store.UpdateSecret(ctx, &store.Secret{
		Key:            key,
		Scope:          store.ScopeHub,
		ScopeID:        hubID,
		SecretType:     store.SecretTypeInternal,
		EncryptedValue: origSealed,
		UpdatedBy:      "test-admin",
	}))

	recRecovered := sendSignedHookHTTP(f.srv, ep.ID, ep.Secret, time.Now().Unix(), "deliv-recovered", "push", "", "application/json", "", nil, payload)
	assert.Equal(t, http.StatusAccepted, recRecovered.Code, recRecovered.Body.String())
}

func TestWebhookIngress_MissingSecretVsWrongTypeDiagnostics(t *testing.T) {
	f := setupTriggersTestFixture(t)
	ctx := context.Background()

	ep := createTestWebhookEndpointViaAPI(t, f, f.projectID, "secret-diag-ep")
	key := store.WebhookEndpointSecretKey(f.projectID, ep.ID, 1)
	hubID := f.srv.HubID()

	var logBuf bytes.Buffer
	dqs := f.store.(store.DurableQueueStore)
	f.srv.webhookIngress = trigger.NewIngress(trigger.IngressConfig{
		Store:      f.store,
		QueueStore: dqs,
		Queue:      f.srv.durableQueue,
		LoadSecret: f.srv.loadWebhookEndpointSecret,
		Enabled:    func() bool { return f.srv.experimentEnabled(experiments.Triggers) },
		Logger:     slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelError})),
	})

	payload := []byte(`{"diag":true}`)
	now := time.Now().Unix()

	// 1. Wrong-type secret: internal error is errWebhookSecretTypeMismatch (not missing),
	//    log records the type mismatch, external response is content-free 503.
	sealed, err := f.srv.sealWebhookEndpointSecret(ep.Secret)
	require.NoError(t, err)
	require.NoError(t, f.store.UpdateSecret(ctx, &store.Secret{
		Key:            key,
		Scope:          store.ScopeHub,
		ScopeID:        hubID,
		SecretType:     store.SecretTypeEnvironment,
		EncryptedValue: sealed,
		UpdatedBy:      "test-admin",
	}))

	_, wrongTypeErr := f.srv.loadWebhookEndpointSecret(ctx, f.projectID, ep.ID, 1)
	require.ErrorIs(t, wrongTypeErr, errWebhookSecretTypeMismatch)
	assert.NotErrorIs(t, wrongTypeErr, errWebhookSecretMissing)

	recWrongType := sendSignedHookHTTP(f.srv, ep.ID, ep.Secret, now, "deliv-wrong-type", "push", "", "application/json", "", nil, payload)
	assert.Equal(t, http.StatusServiceUnavailable, recWrongType.Code)
	wrongTypeLog := logBuf.String()
	assert.Contains(t, wrongTypeLog, errWebhookSecretTypeMismatch.Error())
	assert.NotContains(t, wrongTypeLog, errWebhookSecretMissing.Error())

	// 2. Missing secret: delete the row -> internal error is errWebhookSecretMissing
	//    (and wraps store.ErrNotFound, NOT errWebhookSecretTypeMismatch), log records
	//    the missing secret, and external response is byte-identical content-free 503.
	logBuf.Reset()
	require.NoError(t, f.store.DeleteSecret(ctx, key, store.ScopeHub, hubID))

	_, missingErr := f.srv.loadWebhookEndpointSecret(ctx, f.projectID, ep.ID, 1)
	require.ErrorIs(t, missingErr, errWebhookSecretMissing)
	require.ErrorIs(t, missingErr, store.ErrNotFound)
	assert.NotErrorIs(t, missingErr, errWebhookSecretTypeMismatch)

	recMissing := sendSignedHookHTTP(f.srv, ep.ID, ep.Secret, now, "deliv-missing", "push", "", "application/json", "", nil, payload)
	assert.Equal(t, http.StatusServiceUnavailable, recMissing.Code)
	assert.Equal(t, recWrongType.Body.String(), recMissing.Body.String(), "external 503 body must remain identical and content-free")
	missingLog := logBuf.String()
	assert.Contains(t, missingLog, errWebhookSecretMissing.Error())
	assert.NotContains(t, missingLog, errWebhookSecretTypeMismatch.Error())
}

func TestWebhookIngress_BadSignatureFloodNeverChargesRejectionBucket(t *testing.T) {
	f := setupTriggersTestFixture(t)
	now := time.Date(2026, 11, 17, 12, 45, 0, 0, time.UTC)

	ep := createTestWebhookEndpointViaAPI(t, f, f.projectID, "badsig-isolation-ep")

	dqs := f.store.(store.DurableQueueStore)
	f.srv.webhookIngress = trigger.NewIngress(trigger.IngressConfig{
		Store:      f.store,
		QueueStore: dqs,
		Queue:      f.srv.durableQueue,
		LoadSecret: f.srv.loadWebhookEndpointSecret,
		Enabled:    func() bool { return f.srv.experimentEnabled(experiments.Triggers) },
		Limiter: trigger.NewIngressLimiter(trigger.LimiterConfig{
			PerIPRate:        0.001,
			PerIPBurst:       100,
			RejectionRate:    0.001,
			RejectionBurst:   2, // shared rejection bucket holds only 2 tokens
			PerEndpointRate:  0.001,
			PerEndpointBurst: 5,
			Now:              func() time.Time { return now },
		}),
		Now: func() time.Time { return now },
	})

	payload := []byte(`{"forged":true}`)

	// Flood known active endpoint `ep` with 8 bad-signature requests (5 -> 401, 3 -> 429 per-endpoint).
	unauthCount, rateLimitedCount := 0, 0
	for i := range 8 {
		rec := sendSignedHookHTTP(
			f.srv,
			ep.ID,
			"wrong-hmac-secret",
			now.Unix(),
			fmt.Sprintf("forged-%d", i),
			"push",
			"",
			"application/json",
			fmt.Sprintf("198.51.100.%d:4000", 10+i),
			nil,
			payload,
		)
		switch rec.Code {
		case http.StatusUnauthorized:
			unauthCount++
		case http.StatusTooManyRequests:
			rateLimitedCount++
		default:
			t.Fatalf("unexpected status %d on bad-signature flood request %d", rec.Code, i)
		}
	}
	assert.Equal(t, 5, unauthCount)
	assert.Equal(t, 3, rateLimitedCount)

	// Immediately afterwards, two unknown-ID requests still answer 404 (not 429),
	// proving signature failures on known endpoints never charge the shared rejection bucket.
	for i := range 2 {
		recUnknown := sendSignedHookHTTP(
			f.srv,
			uuid.NewString(),
			"junk",
			now.Unix(),
			"",
			"",
			"",
			"application/json",
			fmt.Sprintf("203.0.113.%d:4000", 20+i),
			nil,
			payload,
		)
		assert.Equal(t, http.StatusNotFound, recUnknown.Code, "unknown-ID request %d must answer 404, not 429", i)
	}

	// Exhausting those 2 unknown-ID tokens causes the 3rd unknown-ID request to hit 429.
	recRej429 := sendSignedHookHTTP(
		f.srv,
		uuid.NewString(),
		"junk",
		now.Unix(),
		"",
		"",
		"",
		"application/json",
		"203.0.113.99:4000",
		nil,
		payload,
	)
	assert.Equal(t, http.StatusTooManyRequests, recRej429.Code)
}
