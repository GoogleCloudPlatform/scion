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

package trigger_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/durablequeue"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

type fakeCore struct {
	enabled atomic.Bool
	admitFn func(ctx, itemCtx context.Context) (context.Context, context.Context, func(), bool)
	fireFn  func(ctx context.Context, req hubfire.FireRequest) hubfire.FireOutcome
}

func (c *fakeCore) Fire(ctx context.Context, req hubfire.FireRequest) hubfire.FireOutcome {
	if c.fireFn != nil {
		return c.fireFn(ctx, req)
	}
	return hubfire.FireOutcome{Status: hubfire.FireStatusFired}
}

func (c *fakeCore) Experiment(name string) bool {
	if name == experiments.Triggers {
		return c.enabled.Load()
	}
	return false
}

func (c *fakeCore) AdmitFire(ctx, itemCtx context.Context) (context.Context, context.Context, func(), bool) {
	if c.admitFn != nil {
		return c.admitFn(ctx, itemCtx)
	}
	return ctx, itemCtx, func() {}, true
}

type nopLease struct{}

func (nopLease) Renew(context.Context) (bool, error) { return true, nil }

// memTriggerStore is a minimal in-memory TriggerStore + DurableQueueStore for
// pure unit tests that also compile and run under -tags no_sqlite.
type memTriggerStore struct {
	mu       sync.Mutex
	events   map[string]*store.TriggerEvent
	triggers []*store.Trigger
	fires    map[string]*store.TriggerFire // key: eventID + "|" + triggerID
	byFireID map[string]*store.TriggerFire
}

func newMemTriggerStore() *memTriggerStore {
	return &memTriggerStore{
		events:   make(map[string]*store.TriggerEvent),
		fires:    make(map[string]*store.TriggerFire),
		byFireID: make(map[string]*store.TriggerFire),
	}
}

func (m *memTriggerStore) LockTriggerQuota(context.Context, string) error { return nil }
func (m *memTriggerStore) CreateWebhookEndpoint(context.Context, *store.WebhookEndpoint) error {
	return nil
}
func (m *memTriggerStore) GetWebhookEndpoint(context.Context, string) (*store.WebhookEndpoint, error) {
	return nil, store.ErrNotFound
}
func (m *memTriggerStore) GetWebhookEndpointByName(context.Context, string, string) (*store.WebhookEndpoint, error) {
	return nil, store.ErrNotFound
}
func (m *memTriggerStore) ListWebhookEndpoints(context.Context, store.WebhookEndpointFilter, store.ListOptions) (*store.ListResult[store.WebhookEndpoint], error) {
	return &store.ListResult[store.WebhookEndpoint]{}, nil
}
func (m *memTriggerStore) CountWebhookEndpoints(context.Context, string) (int, error) { return 0, nil }
func (m *memTriggerStore) FlushWebhookEndpointCounters(context.Context, string, int, int, *time.Time) error {
	return nil
}
func (m *memTriggerStore) DeleteWebhookEndpoint(context.Context, string, string) ([]string, []string, error) {
	return nil, nil, nil
}
func (m *memTriggerStore) CreateTrigger(_ context.Context, tr *store.Trigger) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *tr
	m.triggers = append(m.triggers, &cp)
	return nil
}
func (m *memTriggerStore) GetTrigger(_ context.Context, id string) (*store.Trigger, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, tr := range m.triggers {
		if tr.ID == id {
			cp := *tr
			return &cp, nil
		}
	}
	return nil, store.ErrNotFound
}
func (m *memTriggerStore) GetTriggerByName(context.Context, string, string) (*store.Trigger, error) {
	return nil, store.ErrNotFound
}
func (m *memTriggerStore) ListTriggers(_ context.Context, filter store.TriggerFilter, _ store.ListOptions) (*store.ListResult[store.Trigger], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var items []store.Trigger
	for _, tr := range m.triggers {
		if filter.ProjectID != "" && tr.ProjectID != filter.ProjectID {
			continue
		}
		if filter.SourceKind != "" && tr.SourceKind != filter.SourceKind {
			continue
		}
		if filter.SourceID != "" && tr.SourceID != filter.SourceID {
			continue
		}
		if filter.Status != "" && tr.Status != filter.Status {
			continue
		}
		items = append(items, *tr)
	}
	return &store.ListResult[store.Trigger]{Items: items, TotalCount: len(items)}, nil
}
func (m *memTriggerStore) CountTriggers(context.Context, store.TriggerFilter) (int, error) {
	return 0, nil
}
func (m *memTriggerStore) UpdateTrigger(context.Context, *store.Trigger, store.TriggerFieldMask, int, bool, *store.InitiatorAttribution) error {
	return nil
}
func (m *memTriggerStore) UpdateTriggerStatus(context.Context, string, string, string) error {
	return nil
}
func (m *memTriggerStore) RecordTriggerFire(_ context.Context, id string, firedAt time.Time, status, errMsg string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, tr := range m.triggers {
		if tr.ID == id {
			tr.FireCount++
			if status == store.TriggerFireStatusFailed || status == store.TriggerFireStatusRefused {
				tr.ErrorCount++
			}
			t := firedAt
			tr.LastFireAt = &t
			tr.LastFireStatus = status
			tr.LastFireError = errMsg
			return nil
		}
	}
	return store.ErrNotFound
}
func (m *memTriggerStore) DeleteTrigger(context.Context, string) error { return nil }
func (m *memTriggerStore) DeleteTriggersByProject(context.Context, string) (int, error) {
	return 0, nil
}

func (m *memTriggerStore) CreateTriggerEvent(_ context.Context, evt *store.TriggerEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *evt
	m.events[cp.ID] = &cp
	return nil
}
func (m *memTriggerStore) GetTriggerEvent(_ context.Context, id string) (*store.TriggerEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ev, ok := m.events[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	cp := *ev
	return &cp, nil
}
func (m *memTriggerStore) GetTriggerEventByDedupKey(context.Context, string, string, string) (*store.TriggerEvent, error) {
	return nil, store.ErrNotFound
}
func (m *memTriggerStore) GetTriggerEventByBodySHA256(context.Context, string, string, string) (*store.TriggerEvent, error) {
	return nil, store.ErrNotFound
}
func (m *memTriggerStore) ListTriggerEvents(context.Context, store.TriggerEventFilter, store.ListOptions) (*store.ListResult[store.TriggerEvent], error) {
	return &store.ListResult[store.TriggerEvent]{}, nil
}
func (m *memTriggerStore) CountPendingTriggerEvents(context.Context, string) (int, error) {
	return 0, nil
}
func (m *memTriggerStore) MarkTriggerEventProcessed(_ context.Context, id string, processedAt time.Time, matchCount, fireCount int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ev, ok := m.events[id]
	if !ok {
		return store.ErrNotFound
	}
	if ev.Status != store.TriggerEventStatusPending {
		return nil
	}
	ev.Status = store.TriggerEventStatusProcessed
	t := processedAt
	ev.ProcessedAt = &t
	ev.MatchCount = matchCount
	ev.FireCount = fireCount
	ev.Error = ""
	return nil
}
func (m *memTriggerStore) MarkTriggerEventFailed(_ context.Context, id string, processedAt time.Time, errMsg string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ev, ok := m.events[id]
	if !ok {
		return store.ErrNotFound
	}
	if ev.Status != store.TriggerEventStatusPending {
		return nil
	}
	ev.Status = store.TriggerEventStatusFailed
	t := processedAt
	ev.ProcessedAt = &t
	ev.Error = errMsg
	return nil
}
func (m *memTriggerStore) ReconcileOrphanedPendingTriggerEvents(context.Context, string, time.Time, time.Time, string, int) (int, error) {
	return 0, nil
}
func (m *memTriggerStore) ClaimTriggerFire(_ context.Context, fire *store.TriggerFire) (bool, *store.TriggerFire, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := fire.EventID + "|" + fire.TriggerID
	if ex, exists := m.fires[key]; exists {
		cp := *ex
		return false, &cp, nil
	}
	cp := *fire
	if cp.Status == "" {
		cp.Status = store.TriggerFireStatusFiring
	}
	m.fires[key] = &cp
	m.byFireID[cp.ID] = &cp
	*fire = cp
	return true, nil, nil
}
func (m *memTriggerStore) FinishTriggerFire(_ context.Context, id, status, resultRef, errMsg string, finishedAt time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.byFireID[id]
	if !ok || f.Status != store.TriggerFireStatusFiring {
		return false, nil
	}
	f.Status = status
	f.ResultRef = resultRef
	f.Error = errMsg
	t := finishedAt
	f.FinishedAt = &t
	return true, nil
}
func (m *memTriggerStore) GetTriggerFire(_ context.Context, id string) (*store.TriggerFire, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.byFireID[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	cp := *f
	return &cp, nil
}
func (m *memTriggerStore) ListTriggerFires(context.Context, store.TriggerFireFilter, store.ListOptions) (*store.ListResult[store.TriggerFire], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var items []store.TriggerFire
	for _, f := range m.byFireID {
		items = append(items, *f)
	}
	return &store.ListResult[store.TriggerFire]{Items: items, TotalCount: len(items)}, nil
}
func (m *memTriggerStore) HasLiveTriggerFireForSubject(context.Context, string, string, time.Time) (bool, error) {
	return false, nil
}
func (m *memTriggerStore) AgentHasFiredTriggerForEvent(context.Context, string, string) (bool, error) {
	return false, nil
}
func (m *memTriggerStore) ReapExpiredTriggerFires(context.Context, time.Time, int) (int, error) {
	return 0, nil
}
func (m *memTriggerStore) PurgeOldTriggerHistory(context.Context, time.Time) (int, int, error) {
	return 0, 0, nil
}

func TestDispatcher_WakeFireDetachAndPerFireBudget(t *testing.T) {
	ms := newMemTriggerStore()
	core := &fakeCore{}
	core.enabled.Store(true)

	projID := uuid.NewString()
	epID := uuid.NewString()
	trID := uuid.NewString()
	evID := uuid.NewString()
	t0 := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

	require.NoError(t, ms.CreateTrigger(context.Background(), &store.Trigger{
		ID:            trID,
		ProjectID:     projID,
		Name:          "wake-tr",
		SourceKind:    store.TriggerSourceKindWebhook,
		SourceID:      epID,
		MatchTypes:    []string{"webhook.*"},
		MatchDialect:  store.TriggerDialectCEL,
		ActionType:    store.TriggerActionMessage,
		ActionPayload: `{"agentId":"a1","message":"hi","wake":true}`,
		SubjectMode:   store.TriggerSubjectModeNew,
		Status:        store.TriggerStatusActive,
	}))
	longSubject := "webhook:" + epID + "/prod-" + strings.Repeat("s", 300)
	require.Greater(t, len(longSubject), trigger.MaxFireSubjectBytes)
	require.NoError(t, ms.CreateTriggerEvent(context.Background(), &store.TriggerEvent{
		ID:         evID,
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		Type:       "webhook.deploy.finished",
		Subject:    longSubject,
		DedupKey:   "d-1",
		OccurredAt: t0,
		ReceivedAt: t0,
		Attributes: map[string]string{"event": "deploy.finished"},
		Payload:    `{"ok":true}`,
		Status:     store.TriggerEventStatusPending,
	}))

	type ctxKey struct{}
	core.fireFn = func(ctx context.Context, req hubfire.FireRequest) hubfire.FireOutcome {
		// Wait past the short per-fire timeout while detached onto the item context.
		valCtx := context.WithValue(ctx, ctxKey{}, "kept")
		detached, cancel := req.Tracked.Detach(valCtx)
		defer cancel()

		assert.Equal(t, "kept", detached.Value(ctxKey{}))
		// Per-fire ctx expires in 25ms, while detached survives because item ctx has 2s.
		<-ctx.Done()
		assert.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
		assert.NoError(t, detached.Err(), "detached wake context must outlive per-fire timeout inside item budget")

		admittedCtx, doneAdmit, ok := req.Tracked.Admit(detached)
		assert.True(t, ok)
		defer doneAdmit()
		assert.NoError(t, admittedCtx.Err())

		assert.Equal(t, trigger.ExecutorKindTriggerDrainer, req.Executor.Kind)
		assert.Equal(t, trigger.ExecutorIDPrefixTriggerEvent+evID, req.Executor.ID)
		require.NotNil(t, req.Event)
		assert.Equal(t, longSubject, req.Event.Subject, "FireEvent carries full trigger_events.subject")
		require.NotNil(t, req.Event.Trigger)
		assert.Equal(t, trID, req.Event.Trigger.ID)
		assert.Equal(t, "wake-tr", req.Event.Trigger.Name)

		return hubfire.FireOutcome{Status: hubfire.FireStatusFired}
	}

	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:           core,
		Store:          ms,
		PerFireTimeout: 25 * time.Millisecond,
		Now:            func() time.Time { return t0 },
	})
	require.NoError(t, err)

	itemCtx, cancelItem := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelItem()

	raw, err := json.Marshal(trigger.EventQueuePayload{Version: 1, EventID: evID})
	require.NoError(t, err)
	res := disp.HandleItem(itemCtx, durablequeue.Item{
		Seq:       1,
		Kind:      trigger.EventQueueKind,
		DedupKey:  evID,
		ProjectID: projID,
		Payload:   raw,
	}, nopLease{})
	assert.True(t, res.Done)
	assert.NoError(t, res.Err)

	ev, err := ms.GetTriggerEvent(context.Background(), evID)
	require.NoError(t, err)
	assert.Equal(t, store.TriggerEventStatusProcessed, ev.Status)
	assert.Equal(t, 1, ev.MatchCount)
	assert.Equal(t, 1, ev.FireCount)

	fires, err := ms.ListTriggerFires(context.Background(), store.TriggerFireFilter{}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, fires.Items, 1)
	assert.Equal(t, trigger.MaxFireSubjectBytes, len(fires.Items[0].Subject), "trigger_fires.subject keeps 256-byte truncation")
	assert.Equal(t, longSubject[:trigger.MaxFireSubjectBytes], fires.Items[0].Subject)
}

func TestDispatcher_RuleBudgetExhaustedAndMalformedPayload(t *testing.T) {
	ms := newMemTriggerStore()
	core := &fakeCore{}
	core.enabled.Store(true)

	projID := uuid.NewString()
	epID := uuid.NewString()
	tr1 := uuid.NewString()
	tr2 := uuid.NewString()
	evID := uuid.NewString()

	cur := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	var step int
	clock := func() time.Time {
		step++
		// Advance clock past 1s rule budget right after JSON payload parse.
		if step >= 2 {
			return cur.Add(2 * time.Second)
		}
		return cur
	}

	require.NoError(t, ms.CreateTrigger(context.Background(), &store.Trigger{
		ID:           tr1,
		ProjectID:    projID,
		Name:         "match-tr",
		SourceKind:   store.TriggerSourceKindWebhook,
		SourceID:     epID,
		MatchTypes:   []string{"webhook.received"},
		MatchDialect: store.TriggerDialectCEL,
		ActionType:   store.TriggerActionMessage,
		SubjectMode:  store.TriggerSubjectModeNew,
		Status:       store.TriggerStatusActive,
	}))
	require.NoError(t, ms.CreateTrigger(context.Background(), &store.Trigger{
		ID:           tr2,
		ProjectID:    projID,
		Name:         "nonmatch-tr",
		SourceKind:   store.TriggerSourceKindWebhook,
		SourceID:     epID,
		MatchTypes:   []string{"webhook.other"},
		MatchDialect: store.TriggerDialectCEL,
		ActionType:   store.TriggerActionMessage,
		SubjectMode:  store.TriggerSubjectModeNew,
		Status:       store.TriggerStatusActive,
	}))
	require.NoError(t, ms.CreateTriggerEvent(context.Background(), &store.TriggerEvent{
		ID:         evID,
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		Type:       "webhook.received",
		Subject:    "webhook:" + epID,
		DedupKey:   "d-budget",
		OccurredAt: cur,
		ReceivedAt: cur,
		Payload:    `{"x":1}`,
		Status:     store.TriggerEventStatusPending,
	}))

	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:       core,
		Store:      ms,
		RuleBudget: time.Second,
		Now:        clock,
	})
	require.NoError(t, err)

	// Malformed payload is Permanent failure.
	badRes := disp.HandleItem(context.Background(), durablequeue.Item{
		Kind:    trigger.EventQueueKind,
		Payload: []byte(`{"v":2,"event_id":""}`),
	}, nopLease{})
	assert.True(t, badRes.Permanent)
	assert.False(t, badRes.Done)

	// Budget-exhausted event records skipped "rule budget exhausted" for matching trigger only,
	// without counting as an error on the trigger definition (§5.5, §6.3).
	raw, _ := json.Marshal(trigger.EventQueuePayload{Version: 1, EventID: evID})
	res := disp.HandleItem(context.Background(), durablequeue.Item{
		Kind:     trigger.EventQueueKind,
		DedupKey: evID,
		Payload:  raw,
	}, nopLease{})
	assert.True(t, res.Done)

	fires, err := ms.ListTriggerFires(context.Background(), store.TriggerFireFilter{}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, fires.Items, 1)
	assert.Equal(t, tr1, fires.Items[0].TriggerID)
	assert.Equal(t, store.TriggerFireStatusSkipped, fires.Items[0].Status)
	assert.Equal(t, trigger.ErrFireRuleBudgetExhausted, fires.Items[0].Error)

	gotTr1, err := ms.GetTrigger(context.Background(), tr1)
	require.NoError(t, err)
	assert.Equal(t, 1, gotTr1.FireCount)
	assert.Equal(t, 0, gotTr1.ErrorCount)
}

func TestDispatcher_WakeFireAdmitWaitPastPerFireTimeoutThenDetach(t *testing.T) {
	type abortKey struct{}

	makeAdmitter := func(shutdownCtx context.Context) func(ctx, itemCtx context.Context) (context.Context, context.Context, func(), bool) {
		return func(ctx, itemCtx context.Context) (context.Context, context.Context, func(), bool) {
			abortCtx, cancelAbort := context.WithCancel(context.Background())
			stopShutdown := context.AfterFunc(shutdownCtx, cancelAbort)
			stopItem := context.AfterFunc(itemCtx, cancelAbort)
			fireCtx, cancelFire := context.WithCancel(ctx)
			stopFire := context.AfterFunc(abortCtx, cancelFire)
			withAbort := context.WithValue(fireCtx, abortKey{}, abortCtx)
			return withAbort, abortCtx, func() {
				stopFire()
				stopItem()
				stopShutdown()
				cancelFire()
				cancelAbort()
			}, true
		}
	}

	t.Run("Admit then wait past PerFireTimeout then Detach stays alive and records fired", func(t *testing.T) {
		ms := newMemTriggerStore()
		shutdownCtx, cancelShutdown := context.WithCancel(context.Background())
		defer cancelShutdown()

		core := &fakeCore{admitFn: makeAdmitter(shutdownCtx)}
		core.enabled.Store(true)

		projID := uuid.NewString()
		epID := uuid.NewString()
		trID := uuid.NewString()
		evID := uuid.NewString()

		require.NoError(t, ms.CreateTrigger(context.Background(), &store.Trigger{
			ID:           trID,
			ProjectID:    projID,
			Name:         "real-order-wake",
			SourceKind:   store.TriggerSourceKindWebhook,
			SourceID:     epID,
			MatchTypes:   []string{"webhook.received"},
			MatchDialect: store.TriggerDialectCEL,
			ActionType:   store.TriggerActionMessage,
			SubjectMode:  store.TriggerSubjectModeNew,
			Status:       store.TriggerStatusActive,
		}))
		require.NoError(t, ms.CreateTriggerEvent(context.Background(), &store.TriggerEvent{
			ID:         evID,
			ProjectID:  projID,
			SourceKind: store.TriggerSourceKindWebhook,
			SourceID:   epID,
			Type:       "webhook.received",
			Subject:    "webhook:" + epID,
			Status:     store.TriggerEventStatusPending,
			OccurredAt: time.Now().UTC(),
		}))

		var detachedAlive bool
		core.fireFn = func(ctx context.Context, req hubfire.FireRequest) hubfire.FireOutcome {
			// Real deliverMessageAction order: Admit first, spend PerFireTimeout
			// during wake, then Detach for post-wake persist/dispatch.
			admittedCtx, doneAdmit, ok := req.Tracked.Admit(ctx)
			if !ok {
				return hubfire.FireOutcome{Status: hubfire.FireStatusFailed, Error: "not admitted"}
			}
			defer doneAdmit()

			<-admittedCtx.Done()
			if !assert.ErrorIs(t, admittedCtx.Err(), context.DeadlineExceeded) {
				return hubfire.FireOutcome{Status: hubfire.FireStatusFailed, Error: admittedCtx.Err().Error()}
			}

			detached, cancelDetached := req.Tracked.Detach(admittedCtx)
			defer cancelDetached()

			// context.AfterFunc invokes cancel in its own goroutine when abortCtx is
			// already done; wait across a short scheduling window so an R1 revert
			// (holding the expired admittedCtx as abortCtx) deterministically fails.
			select {
			case <-detached.Done():
				return hubfire.FireOutcome{Status: hubfire.FireStatusFailed, Error: detached.Err().Error()}
			case <-time.After(30 * time.Millisecond):
			}
			if err := detached.Err(); err != nil {
				return hubfire.FireOutcome{Status: hubfire.FireStatusFailed, Error: err.Error()}
			}
			assert.NotNil(t, detached.Value(abortKey{}), "context values from Admit must carry onto Detach")
			detachedAlive = true
			return hubfire.FireOutcome{Status: hubfire.FireStatusFired}
		}

		disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
			Core:           core,
			Store:          ms,
			PerFireTimeout: 20 * time.Millisecond,
		})
		require.NoError(t, err)

		itemCtx, cancelItem := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancelItem()

		raw, err := json.Marshal(trigger.EventQueuePayload{Version: 1, EventID: evID})
		require.NoError(t, err)
		res := disp.HandleItem(itemCtx, durablequeue.Item{
			Seq:       1,
			Kind:      trigger.EventQueueKind,
			DedupKey:  evID,
			ProjectID: projID,
			Payload:   raw,
		}, nopLease{})
		require.NoError(t, res.Err)
		assert.True(t, res.Done)
		assert.True(t, detachedAlive, "post-wake Detach must succeed with live context after PerFireTimeout")

		fires, err := ms.ListTriggerFires(context.Background(), store.TriggerFireFilter{TriggerID: trID}, store.ListOptions{})
		require.NoError(t, err)
		require.Len(t, fires.Items, 1)
		assert.Equal(t, store.TriggerFireStatusFired, fires.Items[0].Status)
	})

	t.Run("Admit then wait past PerFireTimeout then Detach cancels on shutdown abort", func(t *testing.T) {
		ms := newMemTriggerStore()
		shutdownCtx, cancelShutdown := context.WithCancel(context.Background())
		defer cancelShutdown()

		core := &fakeCore{admitFn: makeAdmitter(shutdownCtx)}
		core.enabled.Store(true)

		projID := uuid.NewString()
		epID := uuid.NewString()
		trID := uuid.NewString()
		evID := uuid.NewString()

		require.NoError(t, ms.CreateTrigger(context.Background(), &store.Trigger{
			ID:           trID,
			ProjectID:    projID,
			Name:         "shutdown-abort-wake",
			SourceKind:   store.TriggerSourceKindWebhook,
			SourceID:     epID,
			MatchTypes:   []string{"webhook.received"},
			MatchDialect: store.TriggerDialectCEL,
			ActionType:   store.TriggerActionMessage,
			SubjectMode:  store.TriggerSubjectModeNew,
			Status:       store.TriggerStatusActive,
		}))
		require.NoError(t, ms.CreateTriggerEvent(context.Background(), &store.TriggerEvent{
			ID:         evID,
			ProjectID:  projID,
			SourceKind: store.TriggerSourceKindWebhook,
			SourceID:   epID,
			Type:       "webhook.received",
			Subject:    "webhook:" + epID,
			Status:     store.TriggerEventStatusPending,
			OccurredAt: time.Now().UTC(),
		}))

		detachedReady := make(chan struct{})
		detachedErrCh := make(chan error, 1)
		core.fireFn = func(ctx context.Context, req hubfire.FireRequest) hubfire.FireOutcome {
			admittedCtx, doneAdmit, ok := req.Tracked.Admit(ctx)
			if !ok {
				return hubfire.FireOutcome{Status: hubfire.FireStatusFailed, Error: "not admitted"}
			}
			defer doneAdmit()

			<-admittedCtx.Done()
			detached, cancelDetached := req.Tracked.Detach(admittedCtx)
			defer cancelDetached()

			if err := detached.Err(); err != nil {
				detachedErrCh <- err
				close(detachedReady)
				return hubfire.FireOutcome{Status: hubfire.FireStatusFailed, Error: err.Error()}
			}
			close(detachedReady)
			<-detached.Done()
			detachedErrCh <- detached.Err()
			return hubfire.FireOutcome{Status: hubfire.FireStatusFailed, Error: detached.Err().Error()}
		}

		disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
			Core:           core,
			Store:          ms,
			PerFireTimeout: 20 * time.Millisecond,
		})
		require.NoError(t, err)

		itemCtx, cancelItem := context.WithCancel(context.Background())
		defer cancelItem()

		raw, err := json.Marshal(trigger.EventQueuePayload{Version: 1, EventID: evID})
		require.NoError(t, err)
		resCh := make(chan durablequeue.Result, 1)
		go func() {
			resCh <- disp.HandleItem(itemCtx, durablequeue.Item{
				Seq:       1,
				Kind:      trigger.EventQueueKind,
				DedupKey:  evID,
				ProjectID: projID,
				Payload:   raw,
			}, nopLease{})
		}()

		select {
		case <-detachedReady:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for detached context after PerFireTimeout")
		}
		select {
		case err := <-detachedErrCh:
			t.Fatalf("detached context exited prematurely before shutdown abort: %v", err)
		default:
		}

		cancelShutdown()

		select {
		case err := <-detachedErrCh:
			require.ErrorIs(t, err, context.Canceled)
		case <-time.After(time.Second):
			t.Fatal("detached context did not cancel on shutdown abort")
		}
		<-resCh
	})
}
