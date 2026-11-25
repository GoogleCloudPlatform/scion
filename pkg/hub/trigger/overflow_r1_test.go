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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/durablequeue"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

type slowStorage struct {
	storage.Storage
	delay        time.Duration
	uploadCalled atomic.Int64
	deleteCalled atomic.Int64
	barrier      chan struct{} // optional: if non-nil, Upload waits for barrier close
	onUploaded   func(objectPath string)
}

func (s *slowStorage) Upload(ctx context.Context, p string, r io.Reader, o storage.UploadOptions) (*storage.Object, error) {
	s.uploadCalled.Add(1)
	if s.barrier != nil {
		select {
		case <-s.barrier:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	obj, err := s.Storage.Upload(ctx, p, r, o)
	if err == nil && s.onUploaded != nil {
		s.onUploaded(p)
	}
	return obj, err
}

func (s *slowStorage) Delete(ctx context.Context, p string) error {
	s.deleteCalled.Add(1)
	return s.Storage.Delete(ctx, p)
}

func r1Setup(t *testing.T, stor storage.Storage, maxBudget int64) (*entadapter.CompositeStore, *trigger.Ingress, *store.WebhookEndpoint, string, string) {
	t.Helper()
	ctx := context.Background()
	client := enttest.NewClient(t)
	t.Logf("dialect=%s", client.Driver().Dialect())
	cs := entadapter.NewCompositeStore(client)

	projA := uuid.NewString()
	require.NoError(t, cs.CreateProject(ctx, &store.Project{ID: projA, Name: "r1-a", Slug: "r1-a-" + projA[:8]}))
	projB := uuid.NewString()
	require.NoError(t, cs.CreateProject(ctx, &store.Project{ID: projB, Name: "r1-b", Slug: "r1-b-" + projB[:8]}))
	ep := &store.WebhookEndpoint{
		ID:                  uuid.NewString(),
		ProjectID:           projA,
		Name:                "r1-ep",
		OwnerPrincipalKind:  store.DelegationPrincipalUser,
		OwnerPrincipalID:    uuid.NewString(),
		OwnerCredentialKind: "session",
		AuthMode:            store.WebhookAuthModeHMAC,
		SignatureMode:       store.WebhookSignatureModeScionV1,
		MaxBodyBytes:        1024 * 1024,
		RatePerSecond:       1000,
		RateBurst:           1000,
		SecretVersion:       1,
		Status:              store.WebhookEndpointStatusActive,
	}
	require.NoError(t, cs.CreateWebhookEndpoint(ctx, ep))
	q := durablequeue.NewQueue(cs)
	ing := trigger.NewIngress(trigger.IngressConfig{
		Store:                      cs,
		QueueStore:                 cs,
		Queue:                      q,
		Storage:                    func() storage.Storage { return stor },
		MaxOverflowBytesPerProject: maxBudget,
		LoadSecret:                 func(_ context.Context, _, _ string, _ int) (string, error) { return "r1-secret-1234567890", nil },
		Enabled:                    func() bool { return true },
	})
	return cs, ing, ep, projA, projB
}

// TestOverflowIngress_SlowUploadDoesNotStallUnrelatedReads lifts R1 probe (a):
// on SQLite (MaxOpenConns=1) and Postgres, an unrelated read during a slow
// overflow upload must complete in < 500 ms.
func TestOverflowIngress_SlowUploadDoesNotStallUnrelatedReads(t *testing.T) {
	local, err := storage.NewLocal(storage.Config{Provider: storage.ProviderLocal, Bucket: "b", LocalPath: t.TempDir()})
	require.NoError(t, err)
	stor := &slowStorage{Storage: local, delay: 2 * time.Second}
	cs, ing, ep, _, projB := r1Setup(t, stor, 0)

	body := makeJSONBodyOfSize(200 * 1024)
	done := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		ing.ServeHTTP(rec, newSignedWebhookRequest(t, ep.ID, "r1-secret-1234567890", time.Now(), "r1-stall-1", "ev", "", body))
		done <- rec.Code
	}()
	time.Sleep(250 * time.Millisecond)
	start := time.Now()
	_, err = cs.GetProject(context.Background(), projB)
	elapsed := time.Since(start)
	require.NoError(t, err)
	code := <-done
	t.Logf("ingress code=%d; unrelated GetProject(projB) latency=%s", code, elapsed)
	require.Equal(t, http.StatusAccepted, code)
	require.Less(t, elapsed, 500*time.Millisecond, "an unrelated read must not wait for an object-storage upload")
}

// TestOverflowIngress_ConcurrentSameProjectSlowUploadsDoNotExhaustPool lifts R1 probe (b):
// on Postgres (pool=5) and SQLite, 8 concurrent same-project slow uploads must
// not delay an unrelated read in another project.
func TestOverflowIngress_ConcurrentSameProjectSlowUploadsDoNotExhaustPool(t *testing.T) {
	local, err := storage.NewLocal(storage.Config{Provider: storage.ProviderLocal, Bucket: "b", LocalPath: t.TempDir()})
	require.NoError(t, err)
	stor := &slowStorage{Storage: local, delay: 1500 * time.Millisecond}
	cs, ing, ep, _, projB := r1Setup(t, stor, 0)

	const n = 8
	body := makeJSONBodyOfSize(100 * 1024)
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := httptest.NewRecorder()
			ing.ServeHTTP(rec, newSignedWebhookRequest(t, ep.ID, "r1-secret-1234567890", time.Now(), fmt.Sprintf("r1-pool-%d", i), "ev", "", body))
			codes[i] = rec.Code
		}(i)
	}
	time.Sleep(350 * time.Millisecond)
	start := time.Now()
	_, err = cs.GetProject(context.Background(), projB)
	elapsed := time.Since(start)
	wg.Wait()
	t.Logf("codes=%v; unrelated GetProject(projB) latency=%s", codes, elapsed)
	require.NoError(t, err)
	for i, c := range codes {
		assert.Equal(t, http.StatusAccepted, c, "request %d", i)
	}
	require.Less(t, elapsed, 500*time.Millisecond, "an unrelated read must not wait behind same-project overflow uploads")
}

// TestOverflowIngress_SlowFirstAttemptRedelivery_202DuplicateOneObject covers R1 (c):
// a redelivery after a slow first attempt receives 202 duplicate and leaves at most one live object.
func TestOverflowIngress_SlowFirstAttemptRedelivery_202DuplicateOneObject(t *testing.T) {
	ctx := context.Background()
	local, err := storage.NewLocal(storage.Config{Provider: storage.ProviderLocal, Bucket: "b", LocalPath: t.TempDir()})
	require.NoError(t, err)
	stor := &slowStorage{Storage: local, delay: 200 * time.Millisecond}
	_, ing, ep, _, _ := r1Setup(t, stor, 0)

	body := makeJSONBodyOfSize(100 * 1024)
	rec1 := httptest.NewRecorder()
	ing.ServeHTTP(rec1, newSignedWebhookRequest(t, ep.ID, "r1-secret-1234567890", time.Now(), "r1-redeliv", "ev", "", body))
	require.Equal(t, http.StatusAccepted, rec1.Code)
	var resp1 trigger.IngressAcceptedResponse
	require.NoError(t, json.Unmarshal(rec1.Body.Bytes(), &resp1))
	assert.Equal(t, trigger.IngressStatusQueued, resp1.Status)

	rec2 := httptest.NewRecorder()
	ing.ServeHTTP(rec2, newSignedWebhookRequest(t, ep.ID, "r1-secret-1234567890", time.Now(), "r1-redeliv", "ev", "", body))
	require.Equal(t, http.StatusAccepted, rec2.Code)
	var resp2 trigger.IngressAcceptedResponse
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &resp2))
	assert.Equal(t, trigger.IngressStatusDuplicate, resp2.Status)
	assert.Equal(t, resp1.EventID, resp2.EventID)
	assert.Equal(t, int64(1), stor.uploadCalled.Load(), "pre-tx dedup must avoid re-uploading")

	listed, err := local.List(ctx, storage.ListOptions{Prefix: trigger.OverflowPayloadPrefix})
	require.NoError(t, err)
	assert.Len(t, listed.Objects, 1)
}

// TestOverflowIngress_DuplicateRace_OneRowOneSurvivingObject tests two goroutines
// with the same X-Scion-Delivery both missing the pre-tx dedup check and uploading:
// exactly one row is inserted, the second gets 202 duplicate, and its uploaded object is deleted.
func TestOverflowIngress_DuplicateRace_OneRowOneSurvivingObject(t *testing.T) {
	ctx := context.Background()
	local, err := storage.NewLocal(storage.Config{Provider: storage.ProviderLocal, Bucket: "b", LocalPath: t.TempDir()})
	require.NoError(t, err)

	// Hold both uploads until both have entered Upload (past pre-tx dedup).
	barrier := make(chan struct{})
	var entered atomic.Int64
	stor := &slowStorage{
		Storage: local,
		barrier: barrier,
	}
	cs, ing, ep, projA, _ := r1Setup(t, stor, 0)

	body := makeJSONBodyOfSize(100 * 1024)
	resps := make([]trigger.IngressAcceptedResponse, 2)
	codes := make([]int, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := httptest.NewRecorder()
			ing.ServeHTTP(rec, newSignedWebhookRequest(t, ep.ID, "r1-secret-1234567890", time.Now(), "r1-race-deliv", "ev", "", body))
			codes[i] = rec.Code
			_ = json.Unmarshal(rec.Body.Bytes(), &resps[i])
		}(i)
	}

	require.Eventually(t, func() bool {
		entered.Store(stor.uploadCalled.Load())
		return entered.Load() == 2
	}, 3*time.Second, 10*time.Millisecond, "both goroutines should enter Upload before barrier opens")
	close(barrier)
	wg.Wait()

	assert.Equal(t, http.StatusAccepted, codes[0])
	assert.Equal(t, http.StatusAccepted, codes[1])
	statuses := map[string]int{resps[0].Status: 1}
	statuses[resps[1].Status]++
	assert.Equal(t, 1, statuses[trigger.IngressStatusQueued])
	assert.Equal(t, 1, statuses[trigger.IngressStatusDuplicate])
	assert.Equal(t, resps[0].EventID, resps[1].EventID)

	// Exactly one DB row and one surviving object (the duplicate's object was deleted).
	evs, err := cs.ListTriggerEvents(ctx, store.TriggerEventFilter{ProjectID: projA}, store.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, evs.Items, 1)

	listed, err := local.List(ctx, storage.ListOptions{Prefix: trigger.OverflowPayloadPrefix})
	require.NoError(t, err)
	require.Len(t, listed.Objects, 1, "duplicate goroutine must delete its own uploaded object")
	assert.Equal(t, evs.Items[0].PayloadRef, listed.Objects[0].Name)
	assert.Equal(t, int64(1), stor.deleteCalled.Load())
}

// TestOverflowIngress_BudgetRefusalAfterUpload_429ObjectDeleted tests two concurrent
// 900 KiB requests racing on a 1500 KiB budget: both pass the pre-tx check and upload,
// one commits, the second hits the authoritative in-lock budget -> 429 and deletes its object.
func TestOverflowIngress_BudgetRefusalAfterUpload_429ObjectDeleted(t *testing.T) {
	ctx := context.Background()
	local, err := storage.NewLocal(storage.Config{Provider: storage.ProviderLocal, Bucket: "b", LocalPath: t.TempDir()})
	require.NoError(t, err)

	barrier := make(chan struct{})
	stor := &slowStorage{Storage: local, barrier: barrier}
	cs, ing, ep, projA, _ := r1Setup(t, stor, 1500*1024)

	body := makeJSONBodyOfSize(900 * 1024)
	codes := make([]int, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := httptest.NewRecorder()
			ing.ServeHTTP(rec, newSignedWebhookRequest(t, ep.ID, "r1-secret-1234567890", time.Now(), fmt.Sprintf("r1-budget-race-%d", i), "ev", "", body))
			codes[i] = rec.Code
		}(i)
	}

	require.Eventually(t, func() bool {
		return stor.uploadCalled.Load() == 2
	}, 3*time.Second, 10*time.Millisecond)
	close(barrier)
	wg.Wait()

	counts := map[int]int{}
	for _, c := range codes {
		counts[c]++
	}
	assert.Equal(t, 1, counts[http.StatusAccepted], "exactly one request should succeed")
	assert.Equal(t, 1, counts[http.StatusTooManyRequests], "second request must get 429 queue full")

	evs, err := cs.ListTriggerEvents(ctx, store.TriggerEventFilter{ProjectID: projA}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, evs.Items, 1)

	listed, err := local.List(ctx, storage.ListOptions{Prefix: trigger.OverflowPayloadPrefix})
	require.NoError(t, err)
	require.Len(t, listed.Objects, 1, "429-refused request must delete its uploaded object")
	assert.Equal(t, evs.Items[0].PayloadRef, listed.Objects[0].Name)
	assert.Equal(t, int64(1), stor.deleteCalled.Load())
}

// txFaultStore wraps CompositeStore to inject either a closure error (rollback
// certain -> delete uploaded object) or a post-closure COMMIT error (ambiguous
// -> must NOT delete uploaded object).
type txFaultStore struct {
	*entadapter.CompositeStore
	injectClosureErr error
	injectCommitErr  error
}

func (f *txFaultStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	if f.injectClosureErr != nil {
		return f.CompositeStore.WithTx(ctx, func(tx store.Store) error {
			// Invoke fn wrapper that injects an error inside fn.
			return fn(&closureErrTx{Store: tx, err: f.injectClosureErr})
		})
	}
	if f.injectCommitErr != nil {
		err := f.CompositeStore.WithTx(ctx, fn)
		if err != nil {
			return err
		}
		// fn returned nil, simulate COMMIT failing ambiguously on the client side.
		return f.injectCommitErr
	}
	return f.CompositeStore.WithTx(ctx, fn)
}

type closureErrTx struct {
	store.Store
	err error
}

func (c *closureErrTx) CreateTriggerEvent(_ context.Context, _ *store.TriggerEvent) error {
	return c.err
}

// TestOverflowIngress_ClosureErrorDeletes_CommitErrorDoesNotDelete verifies R1's
// rollback vs ambiguous-commit cleanup rule.
func TestOverflowIngress_ClosureErrorDeletes_CommitErrorDoesNotDelete(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	t.Logf("dialect=%s", client.Driver().Dialect())
	cs := entadapter.NewCompositeStore(client)
	local, err := storage.NewLocal(storage.Config{Provider: storage.ProviderLocal, Bucket: "b", LocalPath: t.TempDir()})
	require.NoError(t, err)
	stor := &slowStorage{Storage: local}

	projID := uuid.NewString()
	require.NoError(t, cs.CreateProject(ctx, &store.Project{ID: projID, Name: "fault-p", Slug: "fault-" + projID[:8]}))
	ep := &store.WebhookEndpoint{
		ID: uuid.NewString(), ProjectID: projID, Name: "fault-ep",
		OwnerPrincipalKind: store.DelegationPrincipalUser, OwnerPrincipalID: uuid.NewString(), OwnerCredentialKind: "session",
		AuthMode: store.WebhookAuthModeHMAC, SignatureMode: store.WebhookSignatureModeScionV1,
		MaxBodyBytes: 1024 * 1024, SecretVersion: 1, Status: store.WebhookEndpointStatusActive,
	}
	require.NoError(t, cs.CreateWebhookEndpoint(ctx, ep))
	q := durablequeue.NewQueue(cs)
	body := makeJSONBodyOfSize(100 * 1024)

	// Case A: closure returns an error -> rollback certain -> uploaded object IS deleted.
	faultA := &txFaultStore{CompositeStore: cs, injectClosureErr: errors.New("simulated insert error")}
	ingA := trigger.NewIngress(trigger.IngressConfig{
		Store: faultA, QueueStore: cs, Queue: q,
		Storage:    func() storage.Storage { return stor },
		LoadSecret: func(_ context.Context, _, _ string, _ int) (string, error) { return "fault-secret-1234567890", nil },
		Enabled:    func() bool { return true },
	})
	recA := httptest.NewRecorder()
	ingA.ServeHTTP(recA, newSignedWebhookRequest(t, ep.ID, "fault-secret-1234567890", time.Now(), "fault-closure", "ev", "", body))
	assert.Equal(t, http.StatusInternalServerError, recA.Code)
	assert.Equal(t, int64(1), stor.deleteCalled.Load(), "closure error must trigger best-effort delete")
	listedA, err := local.List(ctx, storage.ListOptions{Prefix: trigger.OverflowPayloadPrefix})
	require.NoError(t, err)
	assert.Empty(t, listedA.Objects, "uploaded object must be removed on closure error")

	// Case B: closure returns nil, COMMIT fails -> ambiguous -> must NOT delete object.
	faultB := &txFaultStore{CompositeStore: cs, injectCommitErr: errors.New("simulated ambiguous commit failure")}
	ingB := trigger.NewIngress(trigger.IngressConfig{
		Store: faultB, QueueStore: cs, Queue: q,
		Storage:    func() storage.Storage { return stor },
		LoadSecret: func(_ context.Context, _, _ string, _ int) (string, error) { return "fault-secret-1234567890", nil },
		Enabled:    func() bool { return true },
	})
	deletesBefore := stor.deleteCalled.Load()
	recB := httptest.NewRecorder()
	ingB.ServeHTTP(recB, newSignedWebhookRequest(t, ep.ID, "fault-secret-1234567890", time.Now(), "fault-commit", "ev", "", body))
	assert.Equal(t, http.StatusInternalServerError, recB.Code)
	assert.Equal(t, deletesBefore, stor.deleteCalled.Load(), "COMMIT error must NEVER delete uploaded object")
	listedB, err := local.List(ctx, storage.ListOptions{Prefix: trigger.OverflowPayloadPrefix})
	require.NoError(t, err)
	assert.Len(t, listedB.Objects, 1, "object from ambiguous commit must survive for sweep")
}

// TestOverflowIngress_TwoReplicasExactBudget verifies O1: two independent Ingress
// replicas sharing a DB enforce the 1500 KiB budget authoritatively with no cache lag.
func TestOverflowIngress_TwoReplicasExactBudget(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	t.Logf("dialect=%s", client.Driver().Dialect())
	cs := entadapter.NewCompositeStore(client)
	local, err := storage.NewLocal(storage.Config{Provider: storage.ProviderLocal, Bucket: "b", LocalPath: t.TempDir()})
	require.NoError(t, err)

	projID := uuid.NewString()
	require.NoError(t, cs.CreateProject(ctx, &store.Project{ID: projID, Name: "o1-p", Slug: "o1-" + projID[:8]}))
	ep := &store.WebhookEndpoint{
		ID: uuid.NewString(), ProjectID: projID, Name: "o1-ep",
		OwnerPrincipalKind: store.DelegationPrincipalUser, OwnerPrincipalID: uuid.NewString(), OwnerCredentialKind: "session",
		AuthMode: store.WebhookAuthModeHMAC, SignatureMode: store.WebhookSignatureModeScionV1,
		MaxBodyBytes: 1024 * 1024, SecretVersion: 1, Status: store.WebhookEndpointStatusActive,
	}
	require.NoError(t, cs.CreateWebhookEndpoint(ctx, ep))
	q := durablequeue.NewQueue(cs)

	mkReplica := func() *trigger.Ingress {
		return trigger.NewIngress(trigger.IngressConfig{
			Store: cs, QueueStore: cs, Queue: q,
			Storage:                    func() storage.Storage { return local },
			MaxOverflowBytesPerProject: 1500 * 1024,
			LoadSecret:                 func(_ context.Context, _, _ string, _ int) (string, error) { return "o1-secret-1234567890", nil },
			Enabled:                    func() bool { return true },
		})
	}
	replica1 := mkReplica()
	replica2 := mkReplica()
	body900K := makeJSONBodyOfSize(900 * 1024)

	rec1 := httptest.NewRecorder()
	replica1.ServeHTTP(rec1, newSignedWebhookRequest(t, ep.ID, "o1-secret-1234567890", time.Now(), "o1-r1", "ev", "", body900K))
	require.Equal(t, http.StatusAccepted, rec1.Code, rec1.Body.String())

	// Replica 2 sees replica 1's committed row immediately -> 429 queue full.
	rec2 := httptest.NewRecorder()
	replica2.ServeHTTP(rec2, newSignedWebhookRequest(t, ep.ID, "o1-secret-1234567890", time.Now(), "o1-r2", "ev", "", body900K))
	assert.Equal(t, http.StatusTooManyRequests, rec2.Code, rec2.Body.String())
	assert.Contains(t, rec2.Body.String(), "queue full")
}
