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
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/durablequeue"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/GoogleCloudPlatform/scion/pkg/triggerrule"
)

// gcsFakeStorage wraps a storage.Storage (typically LocalStorage) to present
// as ProviderGCS and allow deterministic fault injection on Upload/Delete for
// testing the Stage 2 §8 upload-failure 503 and purge-retry paths.
type gcsFakeStorage struct {
	storage.Storage
	mu          sync.Mutex
	failUpload  error
	failDelete  map[string]error
	uploadCalls atomic.Int64
	deleteCalls atomic.Int64
	signedCalls atomic.Int64
}

func (f *gcsFakeStorage) Provider() storage.Provider { return storage.ProviderGCS }

func (f *gcsFakeStorage) GenerateSignedURL(ctx context.Context, objectPath string, opts storage.SignedURLOptions) (*storage.SignedURL, error) {
	f.signedCalls.Add(1)
	return f.Storage.GenerateSignedURL(ctx, objectPath, opts)
}

func (f *gcsFakeStorage) Upload(ctx context.Context, objectPath string, reader io.Reader, opts storage.UploadOptions) (*storage.Object, error) {
	f.uploadCalls.Add(1)
	f.mu.Lock()
	err := f.failUpload
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return f.Storage.Upload(ctx, objectPath, reader, opts)
}

func (f *gcsFakeStorage) Delete(ctx context.Context, objectPath string) error {
	f.deleteCalls.Add(1)
	f.mu.Lock()
	err := f.failDelete[objectPath]
	f.mu.Unlock()
	if err != nil {
		return err
	}
	return f.Storage.Delete(ctx, objectPath)
}

type spyPayloadMetrics struct {
	mu      sync.Mutex
	results map[string]int
	bytes   []int
}

func (s *spyPayloadMetrics) RecordIngressEvent(sourceKind, result string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.results == nil {
		s.results = make(map[string]int)
	}
	s.results[sourceKind+":"+result]++
}

func (s *spyPayloadMetrics) ObserveIngressPayloadBytes(_ string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bytes = append(s.bytes, n)
}

func makeJSONBodyOfSize(targetBytes int) []byte {
	const prefix = `{"marker":"overflow-test","blob":"`
	const suffix = `"}`
	padLen := targetBytes - len(prefix) - len(suffix)
	if padLen < 0 {
		padLen = 0
	}
	var b bytes.Buffer
	b.Grow(targetBytes)
	b.WriteString(prefix)
	b.WriteString(strings.Repeat("a", padLen))
	b.WriteString(suffix)
	return b.Bytes()
}

func newSignedWebhookRequest(t *testing.T, epID, secret string, now time.Time, delivery, eventHdr, subjectHdr string, body []byte) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/"+epID, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if delivery != "" {
		req.Header.Set(trigger.HeaderDelivery, delivery)
	}
	if eventHdr != "" {
		req.Header.Set(trigger.HeaderEvent, eventHdr)
	}
	if subjectHdr != "" {
		req.Header.Set(trigger.HeaderSubject, subjectHdr)
	}
	sig := trigger.SignScionV1(secret, now.Unix(), delivery, eventHdr, subjectHdr, body)
	req.Header.Set(trigger.HeaderSignature, sig)
	return req
}

func TestOverflowPayloadObjectPath_Validation(t *testing.T) {
	pid := uuid.NewString()
	eid := uuid.NewString()

	got, err := trigger.OverflowPayloadObjectPath(pid, eid)
	require.NoError(t, err)
	assert.Equal(t, "trigger-events/"+pid+"/"+eid+".json", got)
	assert.True(t, trigger.IsValidOverflowPayloadPath(pid, got))
	assert.False(t, trigger.IsValidOverflowPayloadPath(uuid.NewString(), got), "cross-project path must be rejected")

	for _, bad := range []string{
		"",
		"trigger-events/../secret.json",
		"trigger-events/" + pid + "/../" + eid + ".json",
		"trigger-events/" + pid + "/" + eid + ".txt",
		"other-prefix/" + pid + "/" + eid + ".json",
		"trigger-events/" + strings.ToUpper(pid) + "/" + eid + ".json",
	} {
		assert.False(t, trigger.IsValidOverflowPayloadPath(pid, bad), "expected invalid path: %q", bad)
	}

	_, err = trigger.OverflowPayloadObjectPath("not-a-uuid", eid)
	assert.Error(t, err)
	_, err = trigger.OverflowPayloadObjectPath(pid, "../escape")
	assert.Error(t, err)
}

func TestEffectiveMaxBodyBytes_DefaultAndCeiling(t *testing.T) {
	assert.Equal(t, store.DefaultWebhookMaxBodyBytes, trigger.EffectiveMaxBodyBytes(0))
	assert.Equal(t, store.DefaultWebhookMaxBodyBytes, trigger.EffectiveMaxBodyBytes(-1))
	assert.Equal(t, 16*1024, trigger.EffectiveMaxBodyBytes(16*1024))
	assert.Equal(t, 1024*1024, trigger.EffectiveMaxBodyBytes(1024*1024))
	assert.Equal(t, 1024*1024, trigger.EffectiveMaxBodyBytes(2*1024*1024))
}

func TestOverflowIngress_LocalStorage_InlineVsOverflow(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	t.Logf("dialect=%s", client.Driver().Dialect())
	cs := entadapter.NewCompositeStore(client)
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

	localStor, err := storage.NewLocal(storage.Config{
		Provider:  storage.ProviderLocal,
		Bucket:    "test-bucket",
		LocalPath: t.TempDir(),
	})
	require.NoError(t, err)

	projID := uuid.NewString()
	require.NoError(t, cs.CreateProject(ctx, &store.Project{
		ID:   projID,
		Name: "overflow-proj",
		Slug: "overflow-" + projID[:8],
	}))

	secret := "test-overflow-secret-1234567890"
	ep1MiB := &store.WebhookEndpoint{
		ID:                  uuid.NewString(),
		ProjectID:           projID,
		Name:                "big-ep",
		OwnerPrincipalKind:  store.DelegationPrincipalUser,
		OwnerPrincipalID:    uuid.NewString(),
		OwnerCredentialKind: "session",
		AuthMode:            store.WebhookAuthModeHMAC,
		SignatureMode:       store.WebhookSignatureModeScionV1,
		MaxBodyBytes:        1024 * 1024,
		SecretVersion:       1,
		Status:              store.WebhookEndpointStatusActive,
	}
	require.NoError(t, cs.CreateWebhookEndpoint(ctx, ep1MiB))

	epDefault := &store.WebhookEndpoint{
		ID:                  uuid.NewString(),
		ProjectID:           projID,
		Name:                "default-ep",
		OwnerPrincipalKind:  store.DelegationPrincipalUser,
		OwnerPrincipalID:    uuid.NewString(),
		OwnerCredentialKind: "session",
		AuthMode:            store.WebhookAuthModeHMAC,
		SignatureMode:       store.WebhookSignatureModeScionV1,
		MaxBodyBytes:        0, // default 64 KiB
		SecretVersion:       1,
		Status:              store.WebhookEndpointStatusActive,
	}
	require.NoError(t, cs.CreateWebhookEndpoint(ctx, epDefault))

	q := durablequeue.NewQueue(cs, durablequeue.WithQueueClock(func() time.Time { return now }))
	metrics := &spyPayloadMetrics{}
	ing := trigger.NewIngress(trigger.IngressConfig{
		Store:      cs,
		QueueStore: cs,
		Queue:      q,
		Storage:    func() storage.Storage { return localStor },
		LoadSecret: func(_ context.Context, _, _ string, _ int) (string, error) { return secret, nil },
		Enabled:    func() bool { return true },
		Metrics:    metrics,
		Now:        func() time.Time { return now },
	})

	// 1. Exact 64 KiB body on 1 MiB endpoint -> stored inline unchanged.
	body64K := makeJSONBodyOfSize(64 * 1024)
	rec64 := httptest.NewRecorder()
	ing.ServeHTTP(rec64, newSignedWebhookRequest(t, ep1MiB.ID, secret, now, "deliv-64k", "build.done", "job-1", body64K))
	require.Equal(t, http.StatusAccepted, rec64.Code, rec64.Body.String())
	var resp64 trigger.IngressAcceptedResponse
	require.NoError(t, json.Unmarshal(rec64.Body.Bytes(), &resp64))
	ev64, err := cs.GetTriggerEvent(ctx, resp64.EventID)
	require.NoError(t, err)
	assert.Equal(t, string(body64K), ev64.Payload)
	assert.Empty(t, ev64.PayloadRef)
	assert.Equal(t, len(body64K), ev64.PayloadBytes)
	assert.NotContains(t, ev64.Attributes, "payload_ref")

	// 2. 900 KiB body on 1 MiB endpoint -> stored out of line in LocalStorage.
	body900K := makeJSONBodyOfSize(900 * 1024)
	rec900 := httptest.NewRecorder()
	ing.ServeHTTP(rec900, newSignedWebhookRequest(t, ep1MiB.ID, secret, now, "deliv-900k", "build.done", "job-2", body900K))
	require.Equal(t, http.StatusAccepted, rec900.Code, rec900.Body.String())
	var resp900 trigger.IngressAcceptedResponse
	require.NoError(t, json.Unmarshal(rec900.Body.Bytes(), &resp900))
	ev900, err := cs.GetTriggerEvent(ctx, resp900.EventID)
	require.NoError(t, err)
	assert.Empty(t, ev900.Payload, "overflow event row must have empty inline payload")
	wantRef := "trigger-events/" + projID + "/" + resp900.EventID + ".json"
	assert.Equal(t, wantRef, ev900.PayloadRef)
	assert.Equal(t, len(body900K), ev900.PayloadBytes)
	assert.Equal(t, strconv.Itoa(len(body900K)), ev900.Attributes["payload_bytes"])
	assert.Equal(t, wantRef, ev900.Attributes["payload_ref"])

	// Verify stored object bytes match byte-for-byte.
	rc, _, err := localStor.Download(ctx, wantRef)
	require.NoError(t, err)
	storedBytes, err := io.ReadAll(rc)
	_ = rc.Close()
	require.NoError(t, err)
	assert.Equal(t, body900K, storedBytes)

	// 3. 900 KiB body on default (64 KiB) endpoint -> 413.
	recDefault413 := httptest.NewRecorder()
	ing.ServeHTTP(recDefault413, newSignedWebhookRequest(t, epDefault.ID, secret, now, "deliv-too-big-default", "build.done", "", body900K))
	assert.Equal(t, http.StatusRequestEntityTooLarge, recDefault413.Code)

	// 4. > 1 MiB body on 1 MiB endpoint -> 413.
	bodyOver1M := makeJSONBodyOfSize(1024*1024 + 64)
	recOver1M := httptest.NewRecorder()
	ing.ServeHTTP(recOver1M, newSignedWebhookRequest(t, ep1MiB.ID, secret, now, "deliv-over-1m", "build.done", "", bodyOver1M))
	assert.Equal(t, http.StatusRequestEntityTooLarge, recOver1M.Code)

	// Metrics assertions: accepted vs accepted_overflow + bytes histogram.
	assert.Equal(t, 1, metrics.results["webhook:accepted"])
	assert.Equal(t, 1, metrics.results["webhook:accepted_overflow"])
	assert.Equal(t, []int{len(body64K), len(body900K)}, metrics.bytes)
	counts := ing.Counts()
	assert.Equal(t, int64(1), counts.Accepted)
	assert.Equal(t, int64(1), counts.AcceptedOverflow)
}

func TestOverflowIngress_GCSFakeUploadFailure503_AndRetryDedup(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	t.Logf("dialect=%s", client.Driver().Dialect())
	cs := entadapter.NewCompositeStore(client)
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

	localStor, err := storage.NewLocal(storage.Config{
		Provider:  storage.ProviderLocal,
		Bucket:    "gcs-fake-bucket",
		LocalPath: t.TempDir(),
	})
	require.NoError(t, err)
	gcsFake := &gcsFakeStorage{
		Storage:    localStor,
		failUpload: errors.New("googleapi: Error 503: backendError"),
	}

	projID := uuid.NewString()
	require.NoError(t, cs.CreateProject(ctx, &store.Project{
		ID:   projID,
		Name: "gcs-fake-proj",
		Slug: "gcs-fake-" + projID[:8],
	}))
	secret := "gcs-fake-secret-1234567890"
	ep := &store.WebhookEndpoint{
		ID:                  uuid.NewString(),
		ProjectID:           projID,
		Name:                "gcs-ep",
		OwnerPrincipalKind:  store.DelegationPrincipalUser,
		OwnerPrincipalID:    uuid.NewString(),
		OwnerCredentialKind: "session",
		AuthMode:            store.WebhookAuthModeHMAC,
		SignatureMode:       store.WebhookSignatureModeScionV1,
		MaxBodyBytes:        1024 * 1024,
		SecretVersion:       1,
		Status:              store.WebhookEndpointStatusActive,
	}
	require.NoError(t, cs.CreateWebhookEndpoint(ctx, ep))

	q := durablequeue.NewQueue(cs, durablequeue.WithQueueClock(func() time.Time { return now }))
	ing := trigger.NewIngress(trigger.IngressConfig{
		Store:      cs,
		QueueStore: cs,
		Queue:      q,
		Storage:    func() storage.Storage { return gcsFake },
		LoadSecret: func(_ context.Context, _, _ string, _ int) (string, error) { return secret, nil },
		Enabled:    func() bool { return true },
		Now:        func() time.Time { return now },
	})

	body := makeJSONBodyOfSize(128 * 1024)

	// Attempt 1: GCS upload fails -> 503, DB transaction rolled back (no event row).
	rec1 := httptest.NewRecorder()
	ing.ServeHTTP(rec1, newSignedWebhookRequest(t, ep.ID, secret, now, "deliv-gcs-1", "deploy", "", body))
	assert.Equal(t, http.StatusServiceUnavailable, rec1.Code, rec1.Body.String())
	assert.Equal(t, int64(1), ing.Counts().FailedStorage)
	_, gErr := cs.GetTriggerEventByDedupKey(ctx, store.TriggerSourceKindWebhook, ep.ID, "deliv-gcs-1")
	assert.ErrorIs(t, gErr, store.ErrNotFound, "failed upload must roll back trigger_events insert")

	// Attempt 2: GCS recovers -> sender retries with same X-Scion-Delivery -> 202 queued.
	gcsFake.mu.Lock()
	gcsFake.failUpload = nil
	gcsFake.mu.Unlock()

	rec2 := httptest.NewRecorder()
	ing.ServeHTTP(rec2, newSignedWebhookRequest(t, ep.ID, secret, now, "deliv-gcs-1", "deploy", "", body))
	require.Equal(t, http.StatusAccepted, rec2.Code, rec2.Body.String())
	var resp2 trigger.IngressAcceptedResponse
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &resp2))
	assert.Equal(t, trigger.IngressStatusQueued, resp2.Status)

	// Attempt 3: duplicate retry -> 202 duplicate without another storage upload.
	uploadsBefore := gcsFake.uploadCalls.Load()
	rec3 := httptest.NewRecorder()
	ing.ServeHTTP(rec3, newSignedWebhookRequest(t, ep.ID, secret, now, "deliv-gcs-1", "deploy", "", body))
	require.Equal(t, http.StatusAccepted, rec3.Code, rec3.Body.String())
	var resp3 trigger.IngressAcceptedResponse
	require.NoError(t, json.Unmarshal(rec3.Body.Bytes(), &resp3))
	assert.Equal(t, trigger.IngressStatusDuplicate, resp3.Status)
	assert.Equal(t, resp2.EventID, resp3.EventID)
	assert.Equal(t, uploadsBefore, gcsFake.uploadCalls.Load(), "duplicate redelivery must not re-upload")
	assert.Equal(t, int64(0), gcsFake.signedCalls.Load(), "signed URLs must never be issued")
}

func TestOverflowIngress_PerProjectByteBudget429(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	t.Logf("dialect=%s", client.Driver().Dialect())
	cs := entadapter.NewCompositeStore(client)
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

	localStor, err := storage.NewLocal(storage.Config{
		Provider:  storage.ProviderLocal,
		Bucket:    "budget-bucket",
		LocalPath: t.TempDir(),
	})
	require.NoError(t, err)

	mkProjectAndEP := func(name string) (string, *store.WebhookEndpoint) {
		pid := uuid.NewString()
		require.NoError(t, cs.CreateProject(ctx, &store.Project{ID: pid, Name: name, Slug: name + "-" + pid[:8]}))
		ep := &store.WebhookEndpoint{
			ID:                  uuid.NewString(),
			ProjectID:           pid,
			Name:                name + "-ep",
			OwnerPrincipalKind:  store.DelegationPrincipalUser,
			OwnerPrincipalID:    uuid.NewString(),
			OwnerCredentialKind: "session",
			AuthMode:            store.WebhookAuthModeHMAC,
			SignatureMode:       store.WebhookSignatureModeScionV1,
			MaxBodyBytes:        1024 * 1024,
			SecretVersion:       1,
			Status:              store.WebhookEndpointStatusActive,
		}
		require.NoError(t, cs.CreateWebhookEndpoint(ctx, ep))
		return pid, ep
	}

	_, epA := mkProjectAndEP("budget-a")
	_, epB := mkProjectAndEP("budget-b")
	secret := "budget-secret-1234567890"

	q := durablequeue.NewQueue(cs, durablequeue.WithQueueClock(func() time.Time { return now }))
	metrics := &spyPayloadMetrics{}
	ing := trigger.NewIngress(trigger.IngressConfig{
		Store:                      cs,
		QueueStore:                 cs,
		Queue:                      q,
		Storage:                    func() storage.Storage { return localStor },
		MaxOverflowBytesPerProject: 1500 * 1024, // fits one 900 KiB body, not two
		LoadSecret:                 func(_ context.Context, _, _ string, _ int) (string, error) { return secret, nil },
		Enabled:                    func() bool { return true },
		Metrics:                    metrics,
		Now:                        func() time.Time { return now },
	})

	body900K := makeJSONBodyOfSize(900 * 1024)

	// 1st 900 KiB body in project A -> 202 queued.
	rec1 := httptest.NewRecorder()
	ing.ServeHTTP(rec1, newSignedWebhookRequest(t, epA.ID, secret, now, "budget-d1", "ev", "", body900K))
	require.Equal(t, http.StatusAccepted, rec1.Code, rec1.Body.String())

	// 2nd 900 KiB body in project A -> 429 queue full (1800 KiB > 1500 KiB).
	rec2 := httptest.NewRecorder()
	ing.ServeHTTP(rec2, newSignedWebhookRequest(t, epA.ID, secret, now, "budget-d2", "ev", "", body900K))
	assert.Equal(t, http.StatusTooManyRequests, rec2.Code, rec2.Body.String())
	assert.Equal(t, "30", rec2.Header().Get("Retry-After"))
	assert.Contains(t, rec2.Body.String(), "queue full")
	assert.Equal(t, 1, metrics.results["webhook:rejected_queue_full"])

	// Duplicate redelivery of d1 at budget -> still 202 duplicate!
	recDup := httptest.NewRecorder()
	ing.ServeHTTP(recDup, newSignedWebhookRequest(t, epA.ID, secret, now, "budget-d1", "ev", "", body900K))
	assert.Equal(t, http.StatusAccepted, recDup.Code, recDup.Body.String())
	var dupResp trigger.IngressAcceptedResponse
	require.NoError(t, json.Unmarshal(recDup.Body.Bytes(), &dupResp))
	assert.Equal(t, trigger.IngressStatusDuplicate, dupResp.Status)

	// Project B has its own independent budget -> 202 queued.
	recB := httptest.NewRecorder()
	ing.ServeHTTP(recB, newSignedWebhookRequest(t, epB.ID, secret, now, "budget-b1", "ev", "", body900K))
	assert.Equal(t, http.StatusAccepted, recB.Code, recB.Body.String())
}

func TestOverflowDispatcher_CELHasPayloadFalse_AndTruncatedPreview(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	ms := newMemTriggerStore()
	projID := uuid.NewString()
	epID := uuid.NewString()
	evID := uuid.NewString()

	localStor, err := storage.NewLocal(storage.Config{
		Provider:  storage.ProviderLocal,
		Bucket:    "disp-bucket",
		LocalPath: t.TempDir(),
	})
	require.NoError(t, err)

	body900K := makeJSONBodyOfSize(900 * 1024)
	objPath, err := trigger.OverflowPayloadObjectPath(projID, evID)
	require.NoError(t, err)
	_, err = localStor.Upload(ctx, objPath, bytes.NewReader(body900K), storage.UploadOptions{ContentType: "application/json"})
	require.NoError(t, err)

	// Trigger 1 checks has(event.payload) -> must NOT match overflow event.
	trHasPayload := uuid.NewString()
	require.NoError(t, ms.CreateTrigger(ctx, &store.Trigger{
		ID:            trHasPayload,
		ProjectID:     projID,
		Name:          "requires-payload",
		SourceKind:    store.TriggerSourceKindWebhook,
		SourceID:      epID,
		MatchTypes:    []string{"webhook.received"},
		MatchRules:    []store.TriggerMatchRule{{Dialect: store.TriggerDialectCEL, Expr: `has(event.payload)`}},
		ActionType:    store.TriggerActionMessage,
		ActionPayload: `{"agentId":"a1","message":"should-not-fire"}`,
		SubjectMode:   store.TriggerSubjectModeNew,
		Status:        store.TriggerStatusActive,
	}))

	// Trigger 2 checks !has(event.payload) + attributes.payload_bytes + payload_ref -> MUST match.
	trAttrMatch := uuid.NewString()
	require.NoError(t, ms.CreateTrigger(ctx, &store.Trigger{
		ID:         trAttrMatch,
		ProjectID:  projID,
		Name:       "matches-overflow-attrs",
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		MatchTypes: []string{"webhook.received"},
		MatchRules: []store.TriggerMatchRule{{
			Dialect: store.TriggerDialectCEL,
			Expr:    `!has(event.payload) && event.attributes.payload_bytes == "` + strconv.Itoa(len(body900K)) + `" && event.attributes.payload_ref.startsWith("trigger-events/")`,
		}},
		ActionType:    store.TriggerActionMessage,
		ActionPayload: `{"agentId":"a1","message":"overflow-fired"}`,
		SubjectMode:   store.TriggerSubjectModeNew,
		Status:        store.TriggerStatusActive,
	}))

	require.NoError(t, ms.CreateTriggerEvent(ctx, &store.TriggerEvent{
		ID:         evID,
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		Type:       "webhook.received",
		Subject:    "webhook:" + epID,
		DedupKey:   "disp-overflow-1",
		OccurredAt: now,
		ReceivedAt: now,
		Attributes: map[string]string{
			"signature_mode": store.WebhookSignatureModeScionV1,
			"payload_bytes":  strconv.Itoa(len(body900K)),
			"payload_ref":    objPath,
		},
		Payload:      "",
		PayloadRef:   objPath,
		PayloadBytes: len(body900K),
		Status:       store.TriggerEventStatusPending,
	}))

	var firedReqs []hubfire.FireRequest
	core := &fakeCore{
		fireFn: func(_ context.Context, req hubfire.FireRequest) hubfire.FireOutcome {
			firedReqs = append(firedReqs, req)
			return hubfire.FireOutcome{Status: hubfire.FireStatusFired}
		},
	}
	core.enabled.Store(true)

	rules, err := triggerrule.NewDefault()
	require.NoError(t, err)
	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:    core,
		Store:   ms,
		Storage: func() storage.Storage { return localStor },
		Rules:   rules,
		Now:     func() time.Time { return now },
	})
	require.NoError(t, err)

	payloadBytes, err := json.Marshal(trigger.EventQueuePayload{Version: trigger.EventQueuePayloadVersion, EventID: evID})
	require.NoError(t, err)
	res := disp.HandleItem(ctx, durablequeue.Item{Payload: payloadBytes, Attempts: 1}, nil)
	assert.True(t, res.Done)
	require.Len(t, firedReqs, 1, "only the !has(event.payload) trigger should fire")
	assert.Equal(t, trAttrMatch, firedReqs[0].Authority.DefinitionID)
	require.NotNil(t, firedReqs[0].Event)
	assert.Equal(t, objPath, firedReqs[0].Event.PayloadRef)
	assert.Equal(t, len(body900K), firedReqs[0].Event.PayloadBytes)
	assert.LessOrEqual(t, len(firedReqs[0].Event.Payload), messaging.MaxExternalPayloadInlineBytes+4)
	assert.True(t, bytes.HasPrefix(body900K, firedReqs[0].Event.Payload[:messaging.MaxExternalPayloadInlineBytes]))

	// ExternalPayload constructed from the preview has truncated=true, bytes=900KiB, text=16KiB prefix.
	ext := messaging.NewTruncatedExternalPayload(messaging.ExternalVerifiedScionV1, firedReqs[0].Event.Payload, firedReqs[0].Event.PayloadBytes)
	assert.True(t, ext.Truncated)
	assert.Equal(t, len(body900K), ext.Bytes)
	assert.Equal(t, string(body900K[:messaging.MaxExternalPayloadInlineBytes]), ext.Text)
	assert.Empty(t, ext.JSON)
}

func TestOverflowMaintenance_PurgeRetryAndOrphanSweep(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	t.Logf("dialect=%s", client.Driver().Dialect())
	cs := entadapter.NewCompositeStore(client)
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

	localStor, err := storage.NewLocal(storage.Config{
		Provider:  storage.ProviderLocal,
		Bucket:    "maint-bucket",
		LocalPath: t.TempDir(),
	})
	require.NoError(t, err)
	gcsFake := &gcsFakeStorage{
		Storage:    localStor,
		failDelete: make(map[string]error),
	}

	projID := uuid.NewString()
	epID := uuid.NewString()
	putObj := func(eid string, modTime time.Time) string {
		ref, pErr := trigger.OverflowPayloadObjectPath(projID, eid)
		require.NoError(t, pErr)
		_, uErr := localStor.Upload(ctx, ref, strings.NewReader(`{"k":"v"}`), storage.UploadOptions{ContentType: "application/json"})
		require.NoError(t, uErr)
		require.NoError(t, os.Chtimes(localStor.ObjectFSPath(ref), modTime, modTime))
		return ref
	}

	// 1. Expired event whose object deletes cleanly.
	eidExpiredOK := uuid.NewString()
	refExpiredOK := putObj(eidExpiredOK, now.Add(-8*24*time.Hour))
	require.NoError(t, cs.CreateTriggerEvent(ctx, &store.TriggerEvent{
		ID: eidExpiredOK, ProjectID: projID, SourceKind: store.TriggerSourceKindWebhook, SourceID: epID,
		Type: "webhook.received", Subject: "webhook:" + epID, DedupKey: "exp-ok",
		OccurredAt: now.Add(-8 * 24 * time.Hour), ReceivedAt: now.Add(-8 * 24 * time.Hour),
		PayloadRef: refExpiredOK, PayloadBytes: 9, Status: store.TriggerEventStatusProcessed,
		CreatedAt: now.Add(-8 * 24 * time.Hour),
	}))

	// 2. Expired event whose object delete fails on pass 1 and must be retried on pass 2.
	eidExpiredRetry := uuid.NewString()
	refExpiredRetry := putObj(eidExpiredRetry, now.Add(-8*24*time.Hour))
	gcsFake.failDelete[refExpiredRetry] = errors.New("transient delete error")
	require.NoError(t, cs.CreateTriggerEvent(ctx, &store.TriggerEvent{
		ID: eidExpiredRetry, ProjectID: projID, SourceKind: store.TriggerSourceKindWebhook, SourceID: epID,
		Type: "webhook.received", Subject: "webhook:" + epID, DedupKey: "exp-retry",
		OccurredAt: now.Add(-8 * 24 * time.Hour), ReceivedAt: now.Add(-8 * 24 * time.Hour),
		PayloadRef: refExpiredRetry, PayloadBytes: 9, Status: store.TriggerEventStatusProcessed,
		CreatedAt: now.Add(-8 * 24 * time.Hour),
	}))

	// 3. Live event (1h old) -> row AND object must survive.
	eidLive := uuid.NewString()
	refLive := putObj(eidLive, now.Add(-time.Hour))
	require.NoError(t, cs.CreateTriggerEvent(ctx, &store.TriggerEvent{
		ID: eidLive, ProjectID: projID, SourceKind: store.TriggerSourceKindWebhook, SourceID: epID,
		Type: "webhook.received", Subject: "webhook:" + epID, DedupKey: "live-1",
		OccurredAt: now.Add(-time.Hour), ReceivedAt: now.Add(-time.Hour),
		PayloadRef: refLive, PayloadBytes: 9, Status: store.TriggerEventStatusProcessed,
		CreatedAt: now.Add(-time.Hour),
	}))

	// 4. Old orphan object (no DB row, 10 minutes old > 5m grace) -> must be swept on pass 1.
	refOrphanOld := putObj(uuid.NewString(), now.Add(-10*time.Minute))

	// 5. Fresh orphan object (no DB row, 30s old < 5m grace) -> must be SPARED on pass 1, swept once older.
	refOrphanFresh := putObj(uuid.NewString(), now.Add(-30*time.Second))

	// Pass 1:
	res1, err := trigger.RunMaintenanceWithStorage(ctx, cs, gcsFake, now, trigger.MaintenanceOptions{})
	require.NoError(t, err)
	assert.Equal(t, 2, res1.EventsPurged)
	assert.Equal(t, 1, res1.OverflowObjectsPurged)
	assert.Equal(t, 1, res1.OverflowOrphansSwept)
	assert.Equal(t, 2, res1.OverflowDeleteErrors, "failed during purge AND failed during pass-1 orphan sweep")

	exists, _ := localStor.Exists(ctx, refExpiredOK)
	assert.False(t, exists, "refExpiredOK must be deleted")
	exists, _ = localStor.Exists(ctx, refOrphanOld)
	assert.False(t, exists, "refOrphanOld must be swept")
	exists, _ = localStor.Exists(ctx, refLive)
	assert.True(t, exists, "refLive must survive")
	exists, _ = localStor.Exists(ctx, refOrphanFresh)
	assert.True(t, exists, "refOrphanFresh inside grace window must survive")
	exists, _ = localStor.Exists(ctx, refExpiredRetry)
	assert.True(t, exists, "refExpiredRetry still exists until next hourly sweep")

	// Pass 2 (1 hour later, delete error cleared): sweeps BOTH refExpiredRetry and refOrphanFresh!
	gcsFake.mu.Lock()
	delete(gcsFake.failDelete, refExpiredRetry)
	gcsFake.mu.Unlock()

	res2, err := trigger.RunMaintenanceWithStorage(ctx, cs, gcsFake, now.Add(time.Hour), trigger.MaintenanceOptions{})
	require.NoError(t, err)
	assert.Equal(t, 2, res2.OverflowOrphansSwept)
	assert.Equal(t, 0, res2.OverflowDeleteErrors)

	exists, _ = localStor.Exists(ctx, refExpiredRetry)
	assert.False(t, exists, "refExpiredRetry must be deleted on hourly retry sweep")
	exists, _ = localStor.Exists(ctx, refOrphanFresh)
	assert.False(t, exists, "refOrphanFresh must be swept once past grace window")
	exists, _ = localStor.Exists(ctx, refLive)
	assert.True(t, exists, "refLive must still survive")
}
