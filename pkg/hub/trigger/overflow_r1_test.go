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

// TestOverflowIngress_TwoReplicasExactBudget verifies O1 (Nit-1): two independent
// Ingress replicas sharing a DB both clear the pre-tx advisory budget check
// concurrently (gated by a storage barrier), both upload 900 KiB, and the
// in-lock LockTriggerQuota + SumTriggerEventOverflowBytes check admits exactly
// one (202) and refuses the other (429 "queue full") while deleting the refused
// replica's uploaded object.
func TestOverflowIngress_TwoReplicasExactBudget(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	t.Logf("dialect=%s", client.Driver().Dialect())
	cs := entadapter.NewCompositeStore(client)
	local, err := storage.NewLocal(storage.Config{Provider: storage.ProviderLocal, Bucket: "b", LocalPath: t.TempDir()})
	require.NoError(t, err)

	barrier := make(chan struct{})
	stor := &slowStorage{Storage: local, barrier: barrier}

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
			Storage:                    func() storage.Storage { return stor },
			MaxOverflowBytesPerProject: 1500 * 1024,
			LoadSecret:                 func(_ context.Context, _, _ string, _ int) (string, error) { return "o1-secret-1234567890", nil },
			Enabled:                    func() bool { return true },
		})
	}
	replica1 := mkReplica()
	replica2 := mkReplica()
	body900K := makeJSONBodyOfSize(900 * 1024)

	var wg sync.WaitGroup
	recs := []*httptest.ResponseRecorder{httptest.NewRecorder(), httptest.NewRecorder()}
	wg.Add(2)
	go func() {
		defer wg.Done()
		replica1.ServeHTTP(recs[0], newSignedWebhookRequest(t, ep.ID, "o1-secret-1234567890", time.Now(), "o1-r1", "ev", "", body900K))
	}()
	go func() {
		defer wg.Done()
		replica2.ServeHTTP(recs[1], newSignedWebhookRequest(t, ep.ID, "o1-secret-1234567890", time.Now(), "o1-r2", "ev", "", body900K))
	}()

	// Wait until both replicas have passed the pre-tx advisory SUM (seeing 0)
	// and entered Upload concurrently.
	deadline := time.Now().Add(5 * time.Second)
	for stor.uploadCalled.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	require.Equal(t, int64(2), stor.uploadCalled.Load(), "both replicas must reach Upload past the pre-tx advisory check")
	close(barrier)
	wg.Wait()

	var n202, n429 int
	for _, rec := range recs {
		switch rec.Code {
		case http.StatusAccepted:
			n202++
		case http.StatusTooManyRequests:
			n429++
			assert.Contains(t, rec.Body.String(), "queue full")
		default:
			t.Fatalf("unexpected status %d: %s", rec.Code, rec.Body.String())
		}
	}
	assert.Equal(t, 1, n202, "exactly one replica must be admitted under the 1500 KiB budget")
	assert.Equal(t, 1, n429, "second replica must be refused by the in-lock budget check")
	assert.Equal(t, int64(1), stor.deleteCalled.Load(), "refused replica must delete its uploaded object")

	listed, err := local.List(ctx, storage.ListOptions{Prefix: trigger.OverflowPayloadPrefix})
	require.NoError(t, err)
	assert.Len(t, listed.Objects, 1, "only the admitted replica's object must remain")
}

// TestOverflowTimeouts_BoundedUnderOrphanGrace pins the O-B invariant locally:
// upload timeout + tx timeout must stay strictly below DefaultOverflowOrphanMinAge
// so the hourly orphan sweep can never delete an object whose ingress transaction
// is still in flight.
func TestOverflowTimeouts_BoundedUnderOrphanGrace(t *testing.T) {
	assert.Less(t, trigger.DefaultOverflowUploadTimeout+trigger.DefaultOverflowTxTimeout, trigger.DefaultOverflowOrphanMinAge,
		"upload + tx timeout must remain strictly below DefaultOverflowOrphanMinAge")
}

// blockingLockStore wraps CompositeStore to block LockTriggerQuota until ctx is
// cancelled, exercising O-B's explicit OverflowTxTimeout bound.
type blockingLockStore struct {
	*entadapter.CompositeStore
	lockEntered chan struct{}
}

func (b *blockingLockStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	return b.CompositeStore.WithTx(ctx, func(tx store.Store) error {
		return fn(&blockingLockTx{Store: tx, lockEntered: b.lockEntered})
	})
}

type blockingLockTx struct {
	store.Store
	lockEntered chan struct{}
}

func (b *blockingLockTx) LockTriggerQuota(ctx context.Context, _ string) error {
	close(b.lockEntered)
	<-ctx.Done()
	return ctx.Err()
}

// TestOverflowIngress_TxTimeoutDeletesUploadedObject verifies O-B: when the
// post-upload transaction blocks on LockTriggerQuota longer than OverflowTxTimeout,
// the closure returns context.DeadlineExceeded, the tx rolls back, and the
// uploaded object is deleted (never left to commit past the orphan grace window).
func TestOverflowIngress_TxTimeoutDeletesUploadedObject(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	t.Logf("dialect=%s", client.Driver().Dialect())
	cs := entadapter.NewCompositeStore(client)
	local, err := storage.NewLocal(storage.Config{Provider: storage.ProviderLocal, Bucket: "b", LocalPath: t.TempDir()})
	require.NoError(t, err)
	stor := &slowStorage{Storage: local}

	projID := uuid.NewString()
	require.NoError(t, cs.CreateProject(ctx, &store.Project{ID: projID, Name: "ob-p", Slug: "ob-" + projID[:8]}))
	ep := &store.WebhookEndpoint{
		ID: uuid.NewString(), ProjectID: projID, Name: "ob-ep",
		OwnerPrincipalKind: store.DelegationPrincipalUser, OwnerPrincipalID: uuid.NewString(), OwnerCredentialKind: "session",
		AuthMode: store.WebhookAuthModeHMAC, SignatureMode: store.WebhookSignatureModeScionV1,
		MaxBodyBytes: 1024 * 1024, SecretVersion: 1, Status: store.WebhookEndpointStatusActive,
	}
	require.NoError(t, cs.CreateWebhookEndpoint(ctx, ep))
	q := durablequeue.NewQueue(cs)

	lockEntered := make(chan struct{})
	blk := &blockingLockStore{CompositeStore: cs, lockEntered: lockEntered}
	ing := trigger.NewIngress(trigger.IngressConfig{
		Store:             blk,
		QueueStore:        cs,
		Queue:             q,
		Storage:           func() storage.Storage { return stor },
		OverflowTxTimeout: 100 * time.Millisecond,
		LoadSecret:        func(_ context.Context, _, _ string, _ int) (string, error) { return "ob-secret-1234567890", nil },
		Enabled:           func() bool { return true },
	})

	body := makeJSONBodyOfSize(100 * 1024)
	start := time.Now()
	rec := httptest.NewRecorder()
	// Bare background request ctx (no request timeout) proves OverflowTxTimeout bounds the wait.
	ing.ServeHTTP(rec, newSignedWebhookRequest(t, ep.ID, "ob-secret-1234567890", time.Now(), "ob-1", "ev", "", body))
	elapsed := time.Since(start)

	select {
	case <-lockEntered:
	default:
		t.Fatal("expected LockTriggerQuota to be entered")
	}
	assert.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	assert.Less(t, elapsed, 2*time.Second, "OverflowTxTimeout must bound the lock wait")
	assert.Equal(t, int64(1), stor.uploadCalled.Load())
	assert.Equal(t, int64(1), stor.deleteCalled.Load(), "timed-out tx closure must delete uploaded object")

	listed, err := local.List(ctx, storage.ListOptions{Prefix: trigger.OverflowPayloadPrefix})
	require.NoError(t, err)
	assert.Empty(t, listed.Objects, "uploaded object must be cleaned up on tx timeout")
	evs, err := cs.ListTriggerEvents(ctx, store.TriggerEventFilter{ProjectID: projID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, evs.Items, "timed-out tx must never commit a row")
}

// fullQueueStore wraps CompositeStore to report MaxPendingEventsPerProject
// from CountDurableQueuePending, exercising O-C's pre-upload pending-cap check.
type fullQueueStore struct {
	*entadapter.CompositeStore
	pendingCount int64
}

var _ store.DurableQueuePendingCounter = (*fullQueueStore)(nil)

func (f *fullQueueStore) CountDurableQueuePending(_ context.Context, _, _ string) (int64, error) {
	return f.pendingCount, nil
}

// TestOverflowIngress_PendingCapPreCheck_429SkipsUpload verifies O-C: when the
// project's pending queue is already at MaxPendingEventsPerProject, an overflow
// delivery is refused with 429 "queue full" before uploading to object storage
// (uploadCalled == 0).
func TestOverflowIngress_PendingCapPreCheck_429SkipsUpload(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	t.Logf("dialect=%s", client.Driver().Dialect())
	cs := entadapter.NewCompositeStore(client)
	local, err := storage.NewLocal(storage.Config{Provider: storage.ProviderLocal, Bucket: "b", LocalPath: t.TempDir()})
	require.NoError(t, err)
	stor := &slowStorage{Storage: local}

	projID := uuid.NewString()
	require.NoError(t, cs.CreateProject(ctx, &store.Project{ID: projID, Name: "oc-p", Slug: "oc-" + projID[:8]}))
	ep := &store.WebhookEndpoint{
		ID: uuid.NewString(), ProjectID: projID, Name: "oc-ep",
		OwnerPrincipalKind: store.DelegationPrincipalUser, OwnerPrincipalID: uuid.NewString(), OwnerCredentialKind: "session",
		AuthMode: store.WebhookAuthModeHMAC, SignatureMode: store.WebhookSignatureModeScionV1,
		MaxBodyBytes: 1024 * 1024, SecretVersion: 1, Status: store.WebhookEndpointStatusActive,
	}
	require.NoError(t, cs.CreateWebhookEndpoint(ctx, ep))
	q := durablequeue.NewQueue(cs)

	fq := &fullQueueStore{CompositeStore: cs, pendingCount: trigger.MaxPendingEventsPerProject}
	ing := trigger.NewIngress(trigger.IngressConfig{
		Store:      fq,
		QueueStore: cs,
		Queue:      q,
		Storage:    func() storage.Storage { return stor },
		LoadSecret: func(_ context.Context, _, _ string, _ int) (string, error) { return "oc-secret-1234567890", nil },
		Enabled:    func() bool { return true },
	})

	body := makeJSONBodyOfSize(100 * 1024)
	rec := httptest.NewRecorder()
	ing.ServeHTTP(rec, newSignedWebhookRequest(t, ep.ID, "oc-secret-1234567890", time.Now(), "oc-1", "ev", "", body))
	assert.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
	assert.Equal(t, "30", rec.Header().Get("Retry-After"))
	assert.Contains(t, rec.Body.String(), "queue full")
	assert.Equal(t, int64(0), stor.uploadCalled.Load(), "pre-upload pending-cap refusal must not upload to object storage")
}
