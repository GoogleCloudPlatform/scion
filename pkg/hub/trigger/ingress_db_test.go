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

package trigger_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/durablequeue"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

type wakeRecordingQueue struct {
	durablequeue.Queue
	wakes atomic.Int64
}

func (w *wakeRecordingQueue) Wake(kind string) {
	if kind == trigger.EventQueueKind {
		w.wakes.Add(1)
	}
	w.Queue.Wake(kind)
}

type memAuditSink struct {
	receipts []trigger.ReceiptAuditRecord
	failures []trigger.SignatureFailureAuditRecord
}

func (m *memAuditSink) RecordReceipt(_ context.Context, rec trigger.ReceiptAuditRecord) {
	m.receipts = append(m.receipts, rec)
}

func (m *memAuditSink) RecordSignatureFailure(_ context.Context, rec trigger.SignatureFailureAuditRecord) {
	m.failures = append(m.failures, rec)
}

func TestIngress_EndToEndStoreAndQueue(t *testing.T) {
	ctx := context.Background()
	cs := entadapter.NewCompositeStore(enttest.NewClient(t))
	now := time.Date(2026, 11, 16, 19, 0, 0, 0, time.UTC)

	baseQ := durablequeue.NewQueue(cs, durablequeue.WithQueueClock(func() time.Time { return now }))
	q := &wakeRecordingQueue{Queue: baseQ}
	audit := &memAuditSink{}

	projID := uuid.NewString()
	require.NoError(t, cs.CreateProject(ctx, &store.Project{
		ID:   projID,
		Name: "ingress-proj",
		Slug: "ingress-" + projID[:8],
	}))

	activeEp := &store.WebhookEndpoint{
		ID:                  uuid.NewString(),
		ProjectID:           projID,
		Name:                "deploy-ep",
		OwnerPrincipalKind:  store.DelegationPrincipalUser,
		OwnerPrincipalID:    uuid.NewString(),
		OwnerCredentialKind: "session",
		SecretVersion:       1,
		SignatureMode:       store.WebhookSignatureModeScionV1,
		Status:              store.WebhookEndpointStatusActive,
		CreatedBy:           "user-1",
	}
	require.NoError(t, cs.CreateWebhookEndpoint(ctx, activeEp))

	disabledEp := &store.WebhookEndpoint{
		ID:                  uuid.NewString(),
		ProjectID:           projID,
		Name:                "disabled-ep",
		OwnerPrincipalKind:  store.DelegationPrincipalUser,
		OwnerPrincipalID:    uuid.NewString(),
		OwnerCredentialKind: "session",
		SecretVersion:       1,
		SignatureMode:       store.WebhookSignatureModeScionV1,
		Status:              store.WebhookEndpointStatusDisabled,
		CreatedBy:           "user-1",
	}
	require.NoError(t, cs.CreateWebhookEndpoint(ctx, disabledEp))

	const epSecret = "dGVzdC13ZWJob29rLXNlY3JldC1zY2lvbi12MS0zMmJ5dGVz"
	var secretErr error
	var forcedEventID string
	ing := trigger.NewIngress(trigger.IngressConfig{
		Store:      cs,
		QueueStore: cs,
		Queue:      q,
		LoadSecret: func(_ context.Context, pID, eID string, ver int) (string, error) {
			if secretErr != nil {
				return "", secretErr
			}
			if pID == projID && eID == activeEp.ID && ver == 1 {
				return epSecret, nil
			}
			return "", errors.New("secret not found")
		},
		ClientIP:   func(r *http.Request) string { return "198.51.100.42" },
		Enabled:    func() bool { return true },
		Audit:      audit,
		Now:        func() time.Time { return now },
		NewEventID: func() string { return forcedEventID },
	})

	sendSigned := func(epID, delivery, eventHdr, subjectHdr, contentType string, rawBody []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/"+epID, bytes.NewReader(rawBody))
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
		req.Header.Set("User-Agent", "ScionSender/1.0")
		req.Header.Set("Authorization", "Bearer should-never-be-stored")
		req.Header.Set(trigger.HeaderSignature, trigger.SignScionV1(epSecret, now.Unix(), delivery, eventHdr, subjectHdr, rawBody))
		rec := httptest.NewRecorder()
		ing.ServeHTTP(rec, req)
		return rec
	}

	// 1. Happy path with X-Scion-Delivery, X-Scion-Event, and X-Scion-Subject.
	payload := []byte(`{"service":"payments","sha":"deadbeef"}`)
	rec := sendSigned(activeEp.ID, "deliv-100", "deploy.finished", "prod/payments", "application/json; charset=utf-8", payload)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	var resp trigger.IngressAcceptedResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, trigger.IngressStatusQueued, resp.Status)
	require.NotEmpty(t, resp.EventID)
	assert.Equal(t, int64(1), q.wakes.Load())

	ev, err := cs.GetTriggerEvent(ctx, resp.EventID)
	require.NoError(t, err)
	assert.Equal(t, projID, ev.ProjectID)
	assert.Equal(t, store.TriggerSourceKindWebhook, ev.SourceKind)
	assert.Equal(t, activeEp.ID, ev.SourceID)
	assert.Equal(t, "webhook.deploy.finished", ev.Type)
	assert.Equal(t, "webhook:"+activeEp.ID+"/prod/payments", ev.Subject)
	assert.Equal(t, "deliv-100", ev.DedupKey)
	assert.Equal(t, string(payload), ev.Payload)
	assert.Equal(t, len(payload), ev.PayloadBytes)
	assert.Equal(t, store.TriggerEventStatusPending, ev.Status)
	assert.Equal(t, map[string]string{
		"endpoint_name":  "deploy-ep",
		"event":          "deploy.finished",
		"delivery":       "deliv-100",
		"subject":        "prod/payments",
		"signature_mode": "scion_v1",
		"content_type":   "application/json; charset=utf-8",
		"payload_bytes":  strconv.Itoa(len(payload)),
		"remote_ip":      "198.51.100.42",
	}, ev.Attributes)
	assert.Equal(t, map[string]string{
		"Content-Type":     "application/json; charset=utf-8",
		"User-Agent":       "ScionSender/1.0",
		"X-Scion-Event":    "deploy.finished",
		"X-Scion-Delivery": "deliv-100",
		"X-Scion-Subject":  "prod/payments",
	}, ev.Headers)
	assert.NotContains(t, ev.Headers, trigger.HeaderSignature)
	assert.NotContains(t, ev.Headers, "Authorization")

	// 2a. Duplicate (source_kind, source_id, dedup_key) via X-Scion-Delivery returns 202 {"status":"duplicate"}
	// with same eventId and no second row or queue item.
	dupRec := sendSigned(activeEp.ID, "deliv-100", "deploy.finished", "prod/payments", "application/json", payload)
	require.Equal(t, http.StatusAccepted, dupRec.Code, dupRec.Body.String())
	var dupResp trigger.IngressAcceptedResponse
	require.NoError(t, json.Unmarshal(dupRec.Body.Bytes(), &dupResp))
	assert.Equal(t, resp.EventID, dupResp.EventID)
	assert.Equal(t, trigger.IngressStatusDuplicate, dupResp.Status)
	assert.Equal(t, int64(1), q.wakes.Load(), "duplicate delivery must not wake queue again")

	evs, err := cs.ListTriggerEvents(ctx, store.TriggerEventFilter{ProjectID: projID}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, evs.Items, 1)
	qStats, err := cs.DurableQueueStats(ctx, trigger.EventQueueKind, projID, now)
	require.NoError(t, err)
	assert.Equal(t, int64(1), qStats.Pending)

	// 2b. An event-ID collision in durable_queue_items (durablequeue.ErrDuplicate)
	// rolls back the trigger_events insert and returns 500 (never 202 "duplicate")
	// so the sender retries.
	preQueuedID := uuid.NewString()
	_, err = q.Enqueue(ctx, nil, durablequeue.Item{
		Kind:      trigger.EventQueueKind,
		DedupKey:  preQueuedID,
		ProjectID: projID,
		Payload:   []byte(`{"v":1,"event_id":"` + preQueuedID + `"}`),
	})
	require.NoError(t, err)
	forcedEventID = preQueuedID
	qDupRec := sendSigned(activeEp.ID, "deliv-queue-dup", "deploy.finished", "prod/payments", "application/json", payload)
	forcedEventID = ""
	require.Equal(t, http.StatusInternalServerError, qDupRec.Code, qDupRec.Body.String())
	assert.Equal(t, int64(1), q.wakes.Load(), "queue-duplicate error path must not wake queue")
	_, err = cs.GetTriggerEvent(ctx, preQueuedID)
	require.ErrorIs(t, err, store.ErrNotFound, "event row must roll back when queue Enqueue returns ErrDuplicate")

	// 2c. Event-ID collision on trigger_events.id with NO X-Scion-Delivery header
	// returns 500 (never 202 "duplicate") so the sender retries.
	forcedEventID = resp.EventID
	idCollisionNoDeliv := sendSigned(activeEp.ID, "", "deploy.finished", "prod/payments", "application/json", payload)
	forcedEventID = ""
	require.Equal(t, http.StatusInternalServerError, idCollisionNoDeliv.Code, idCollisionNoDeliv.Body.String())
	assert.Equal(t, int64(1), q.wakes.Load())

	// 2d. Event-ID collision on trigger_events.id even WITH a fresh X-Scion-Delivery
	// header (where no event exists for that delivery key) returns 500, never 202.
	forcedEventID = resp.EventID
	idCollisionNewDeliv := sendSigned(activeEp.ID, "deliv-fresh-never-stored", "deploy.finished", "prod/payments", "application/json", payload)
	forcedEventID = ""
	require.Equal(t, http.StatusInternalServerError, idCollisionNewDeliv.Code, idCollisionNewDeliv.Body.String())
	assert.Equal(t, int64(1), q.wakes.Load())

	// 3. Without X-Scion-Delivery, two identical signed bodies create two distinct events.
	noDeliv1 := sendSigned(activeEp.ID, "", "", "", "application/json", []byte(`{"ping":1}`))
	noDeliv2 := sendSigned(activeEp.ID, "", "", "", "application/json", []byte(`{"ping":1}`))
	require.Equal(t, http.StatusAccepted, noDeliv1.Code)
	require.Equal(t, http.StatusAccepted, noDeliv2.Code)
	var r1, r2 trigger.IngressAcceptedResponse
	require.NoError(t, json.Unmarshal(noDeliv1.Body.Bytes(), &r1))
	require.NoError(t, json.Unmarshal(noDeliv2.Body.Bytes(), &r2))
	assert.NotEqual(t, r1.EventID, r2.EventID)
	evDefault, err := cs.GetTriggerEvent(ctx, r1.EventID)
	require.NoError(t, err)
	assert.Equal(t, "webhook.received", evDefault.Type)
	assert.Equal(t, "webhook:"+activeEp.ID, evDefault.Subject)
	assert.Equal(t, "received", evDefault.Attributes["event"])

	// 4. Exact 64 KiB JSON body succeeds; 64 KiB + 1 returns 413 without row writes.
	padLen := trigger.MaxPayloadBytes - len(`{"pad":""}`)
	exact64KiB := []byte(`{"pad":"` + strings.Repeat("x", padLen) + `"}`)
	require.Len(t, exact64KiB, trigger.MaxPayloadBytes)
	rec64 := sendSigned(activeEp.ID, "deliv-64k", "", "", "application/json", exact64KiB)
	require.Equal(t, http.StatusAccepted, rec64.Code, rec64.Body.String())

	over64KiB := []byte(`{"pad":"` + strings.Repeat("x", padLen+1) + `"}`)
	require.Len(t, over64KiB, trigger.MaxPayloadBytes+1)
	rec413 := sendSigned(activeEp.ID, "deliv-64k-plus-1", "", "", "application/json", over64KiB)
	assert.Equal(t, http.StatusRequestEntityTooLarge, rec413.Code)

	// 5. Refusal ordering & no-row-write invariant across 404 (unknown/disabled), 415, 400, 401, 503.
	beforeEvents, err := cs.ListTriggerEvents(ctx, store.TriggerEventFilter{ProjectID: projID}, store.ListOptions{})
	require.NoError(t, err)
	beforeCount := beforeEvents.TotalCount

	// Unknown vs disabled endpoint -> identical 404 JSON body.
	unknownRec := sendSigned(uuid.NewString(), "", "", "", "application/json", payload)
	disabledRec := sendSigned(disabledEp.ID, "", "", "", "application/json", payload)
	assert.Equal(t, http.StatusNotFound, unknownRec.Code)
	assert.Equal(t, http.StatusNotFound, disabledRec.Code)
	assert.JSONEq(t, unknownRec.Body.String(), disabledRec.Body.String())

	// Non-JSON Content-Type -> 415; invalid JSON -> 400.
	rec415 := sendSigned(activeEp.ID, "", "", "", "text/plain", payload)
	assert.Equal(t, http.StatusUnsupportedMediaType, rec415.Code)
	recBadJSON := sendSigned(activeEp.ID, "", "", "", "application/json", []byte(`{not-json`))
	assert.Equal(t, http.StatusBadRequest, recBadJSON.Code)

	// Unsigned request with invalid X-Scion-Event -> 401 (never 400 to unsigned caller);
	// validly signed request with invalid X-Scion-Event -> 400 (§8.1 step 7).
	unsignedBadHdr := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/"+activeEp.ID, bytes.NewReader(payload))
	unsignedBadHdr.Header.Set("Content-Type", "application/json")
	unsignedBadHdr.Header.Set(trigger.HeaderEvent, "INVALID EVENT!!")
	recUnsignedBadHdr := httptest.NewRecorder()
	ing.ServeHTTP(recUnsignedBadHdr, unsignedBadHdr)
	assert.Equal(t, http.StatusUnauthorized, recUnsignedBadHdr.Code)

	recSignedBadHdr := sendSigned(activeEp.ID, "", "INVALID EVENT!!", "", "application/json", payload)
	assert.Equal(t, http.StatusBadRequest, recSignedBadHdr.Code)

	// Secret unseal failure (after cache invalidation) -> fail-closed 503.
	ing.InvalidateEndpoint(activeEp.ID)
	secretErr = errors.New("kms unseal error")
	rec503 := sendSigned(activeEp.ID, "", "", "", "application/json", payload)
	assert.Equal(t, http.StatusServiceUnavailable, rec503.Code)
	secretErr = nil

	afterEvents, err := cs.ListTriggerEvents(ctx, store.TriggerEventFilter{ProjectID: projID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, beforeCount, afterEvents.TotalCount, "refusals must write zero trigger_events rows")

	// Endpoint receive_count / reject_count are still 0 in DB before FlushCounters, then match after FlushCounters!
	epBeforeFlush, err := cs.GetWebhookEndpoint(ctx, activeEp.ID)
	require.NoError(t, err)
	assert.Zero(t, epBeforeFlush.ReceiveCount, "counters must accumulate in memory before periodic/explicit flush")
	assert.Zero(t, epBeforeFlush.RejectCount)

	require.NoError(t, ing.FlushCounters(ctx))
	epAfterFlush, err := cs.GetWebhookEndpoint(ctx, activeEp.ID)
	require.NoError(t, err)
	assert.Equal(t, 4, epAfterFlush.ReceiveCount) // 1 initial + 2 no-delivery + 1 exact-64KiB
	assert.Equal(t, 5, epAfterFlush.RejectCount)  // 413 + 415 + bad-JSON 400 + unsigned 401 + bad-header 400
	require.NotNil(t, epAfterFlush.LastReceivedAt)
}

func TestIngress_IPv6Slash48SprayDoesNotLockOutLegitimateSender(t *testing.T) {
	ctx := context.Background()
	cs := entadapter.NewCompositeStore(enttest.NewClient(t))
	now := time.Date(2026, 11, 17, 3, 0, 0, 0, time.UTC)

	q := durablequeue.NewQueue(cs, durablequeue.WithQueueClock(func() time.Time { return now }))
	projID := uuid.NewString()
	require.NoError(t, cs.CreateProject(ctx, &store.Project{
		ID:   projID,
		Name: "spray-proj",
		Slug: "spray-" + projID[:8],
	}))

	makeEndpoint := func(name string) *store.WebhookEndpoint {
		ep := &store.WebhookEndpoint{
			ID:                  uuid.NewString(),
			ProjectID:           projID,
			Name:                name,
			OwnerPrincipalKind:  store.DelegationPrincipalUser,
			OwnerPrincipalID:    uuid.NewString(),
			OwnerCredentialKind: "session",
			SecretVersion:       1,
			SignatureMode:       store.WebhookSignatureModeScionV1,
			Status:              store.WebhookEndpointStatusActive,
			CreatedBy:           "user-1",
		}
		require.NoError(t, cs.CreateWebhookEndpoint(ctx, ep))
		return ep
	}
	epHealthy := makeEndpoint("healthy-ep")
	epTargeted := makeEndpoint("targeted-ep")

	const epSecret = "c3ByYXktdGVzdC13ZWJob29rLXNlY3JldC0zMmJ5dGVz"
	lim := trigger.NewIngressLimiter(trigger.LimiterConfig{
		MaxEntries:       64,
		RejectionRate:    10,
		RejectionBurst:   25,
		PerEndpointRate:  1,
		PerEndpointBurst: 5,
		Now:              func() time.Time { return now },
	})
	cache := trigger.NewEndpointCache(trigger.EndpointCacheConfig{
		Now: func() time.Time { return now },
	})
	for _, id := range []string{epHealthy.ID, epTargeted.ID} {
		_, err := cache.LookupEndpoint(ctx, cs, id)
		require.NoError(t, err)
	}
	ing := trigger.NewIngress(trigger.IngressConfig{
		Store:      cs,
		QueueStore: cs,
		Queue:      q,
		LoadSecret: func(_ context.Context, _, _ string, _ int) (string, error) {
			return epSecret, nil
		},
		ClientIP: func(r *http.Request) string { return r.RemoteAddr },
		Limiter:  lim,
		Cache:    cache,
		Enabled:  func() bool { return true },
		Audit:    &memAuditSink{},
		Now:      func() time.Time { return now },
	})

	payload := []byte(`{"ok":true}`)

	// 1. Spray 100 distinct /64 subnets from one IPv6 /48 against unknown IDs at fixed `now`:
	// per-IP fills at 64 entries and fails open, while the rejection tier still caps unknown IDs at 25 (404 -> 429).
	notFoundCount, rateLimitedCount := 0, 0
	for i := 0; i < 100; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/"+uuid.NewString(), bytes.NewReader(payload))
		req.RemoteAddr = "2001:db8:48:" + strconv.FormatInt(int64(i+1), 16) + "::1"
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		ing.ServeHTTP(rec, req)
		switch rec.Code {
		case http.StatusNotFound:
			notFoundCount++
		case http.StatusTooManyRequests:
			rateLimitedCount++
		default:
			t.Fatalf("unexpected status %d on spray %d", rec.Code, i)
		}
	}
	assert.Equal(t, 25, notFoundCount)
	assert.Equal(t, 75, rateLimitedCount)
	assert.Equal(t, 64, lim.PerIPLen())

	// 2. With per-IP completely full AND rejection completely exhausted at the same timestamp,
	// a legitimate signed webhook from a brand-new sender IP to epHealthy still succeeds with 202.
	sendSignedFrom := func(epID, remoteIP, delivery string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/"+epID, bytes.NewReader(payload))
		req.RemoteAddr = remoteIP
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(trigger.HeaderDelivery, delivery)
		req.Header.Set(trigger.HeaderSignature, trigger.SignScionV1(epSecret, now.Unix(), delivery, "", "", payload))
		rec := httptest.NewRecorder()
		ing.ServeHTTP(rec, req)
		return rec
	}
	recHealthy1 := sendSignedFrom(epHealthy.ID, "198.51.100.200", "healthy-1")
	require.Equal(t, http.StatusAccepted, recHealthy1.Code, recHealthy1.Body.String())

	// 3. Even while per-IP fails open, spraying unsigned requests from new /64s against
	// epTargeted is still capped by epTargeted's per-endpoint bucket (5 -> 401, then 429),
	// without affecting epHealthy from another new sender IP.
	unauthCount, epLimitedCount := 0, 0
	for i := 0; i < 10; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/"+epTargeted.ID, bytes.NewReader(payload))
		req.RemoteAddr = "2001:db8:49:" + strconv.FormatInt(int64(i+1), 16) + "::1"
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		ing.ServeHTTP(rec, req)
		switch rec.Code {
		case http.StatusUnauthorized:
			unauthCount++
		case http.StatusTooManyRequests:
			epLimitedCount++
		default:
			t.Fatalf("unexpected status %d on targeted spray %d", rec.Code, i)
		}
	}
	assert.Equal(t, 5, unauthCount)
	assert.Equal(t, 5, epLimitedCount)

	recHealthy2 := sendSignedFrom(epHealthy.ID, "198.51.100.201", "healthy-2")
	require.Equal(t, http.StatusAccepted, recHealthy2.Code, recHealthy2.Body.String())
}

func TestIngress_BadSignatureFloodNeverChargesRejectionBucket(t *testing.T) {
	ctx := context.Background()
	cs := entadapter.NewCompositeStore(enttest.NewClient(t))
	now := time.Date(2026, 11, 17, 12, 30, 0, 0, time.UTC)

	q := durablequeue.NewQueue(cs, durablequeue.WithQueueClock(func() time.Time { return now }))
	projID := uuid.NewString()
	require.NoError(t, cs.CreateProject(ctx, &store.Project{
		ID:   projID,
		Name: "badsig-proj",
		Slug: "badsig-" + projID[:8],
	}))

	ep := &store.WebhookEndpoint{
		ID:                  uuid.NewString(),
		ProjectID:           projID,
		Name:                "known-ep",
		OwnerPrincipalKind:  store.DelegationPrincipalUser,
		OwnerPrincipalID:    uuid.NewString(),
		OwnerCredentialKind: "session",
		SecretVersion:       1,
		SignatureMode:       store.WebhookSignatureModeScionV1,
		Status:              store.WebhookEndpointStatusActive,
		CreatedBy:           "user-1",
	}
	require.NoError(t, cs.CreateWebhookEndpoint(ctx, ep))

	const epSecret = "YmFkc2lnLWZsb29kLXdlYmhvb2stc2VjcmV0LTMyYnl0ZXM"
	lim := trigger.NewIngressLimiter(trigger.LimiterConfig{
		PerIPRate:        0.001,
		PerIPBurst:       100,
		RejectionRate:    0.001,
		RejectionBurst:   2, // tiny shared rejection budget: even 2 charges would exhaust it
		PerEndpointRate:  0.001,
		PerEndpointBurst: 6,
		Now:              func() time.Time { return now },
	})
	cache := trigger.NewEndpointCache(trigger.EndpointCacheConfig{
		Now: func() time.Time { return now },
	})
	_, err := cache.LookupEndpoint(ctx, cs, ep.ID)
	require.NoError(t, err)
	ing := trigger.NewIngress(trigger.IngressConfig{
		Store:      cs,
		QueueStore: cs,
		Queue:      q,
		LoadSecret: func(_ context.Context, _, _ string, _ int) (string, error) {
			return epSecret, nil
		},
		ClientIP: func(r *http.Request) string { return r.RemoteAddr },
		Limiter:  lim,
		Cache:    cache,
		Enabled:  func() bool { return true },
		Audit:    &memAuditSink{},
		Now:      func() time.Time { return now },
	})

	payload := []byte(`{"attempt":"forged"}`)

	// Flood the known active endpoint with 10 bad-signature requests (>> RejectionBurst=2):
	// the first 6 reach HMAC verification and answer 401, and the remaining 4 hit ep's
	// per-endpoint bucket (429). None may touch the shared rejection bucket.
	unauthCount, epRateLimitedCount := 0, 0
	for i := range 10 {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/"+ep.ID, bytes.NewReader(payload))
		req.RemoteAddr = "198.51.100." + strconv.Itoa(10+i)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(trigger.HeaderSignature, trigger.SignScionV1("wrong-secret-key", now.Unix(), "", "", "", payload))
		rec := httptest.NewRecorder()
		ing.ServeHTTP(rec, req)
		switch rec.Code {
		case http.StatusUnauthorized:
			unauthCount++
		case http.StatusTooManyRequests:
			epRateLimitedCount++
		default:
			t.Fatalf("unexpected status %d on bad-signature request %d", rec.Code, i)
		}
	}
	assert.Equal(t, 6, unauthCount)
	assert.Equal(t, 4, epRateLimitedCount)

	// Both tokens of the shared rejection bucket (RejectionBurst=2) must still be intact:
	// two unknown-ID requests answer 404 (not 429), and only the 3rd hits 429.
	for i := range 2 {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/"+uuid.NewString(), bytes.NewReader(payload))
		req.RemoteAddr = "203.0.113." + strconv.Itoa(50+i)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		ing.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusNotFound, rec.Code, "unknown-ID request %d after bad-sig flood must get 404, not 429", i)
	}

	reqThirdUnknown := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/"+uuid.NewString(), bytes.NewReader(payload))
	reqThirdUnknown.RemoteAddr = "203.0.113.99"
	reqThirdUnknown.Header.Set("Content-Type", "application/json")
	recThirdUnknown := httptest.NewRecorder()
	ing.ServeHTTP(recThirdUnknown, reqThirdUnknown)
	assert.Equal(t, http.StatusTooManyRequests, recThirdUnknown.Code)
}
