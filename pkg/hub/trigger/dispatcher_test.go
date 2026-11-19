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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/GoogleCloudPlatform/scion/pkg/celengine"
	"github.com/GoogleCloudPlatform/scion/pkg/durablequeue"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger"
	"github.com/GoogleCloudPlatform/scion/pkg/ruleengine"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/triggerrule"
)

type fakeCore struct {
	enabled           atomic.Bool
	githubDisabled    atomic.Bool
	admitFn           func(ctx, itemCtx context.Context) (context.Context, context.Context, func(), bool)
	fireFn            func(ctx context.Context, req hubfire.FireRequest) hubfire.FireOutcome
	authorizeSourceFn func(ctx context.Context, ev *store.TriggerEvent) (bool, error)
}

func (c *fakeCore) Fire(ctx context.Context, req hubfire.FireRequest) hubfire.FireOutcome {
	if c.fireFn != nil {
		return c.fireFn(ctx, req)
	}
	return hubfire.FireOutcome{Status: hubfire.FireStatusFired}
}

func (c *fakeCore) Experiment(name string) bool {
	switch name {
	case experiments.Triggers:
		return c.enabled.Load()
	case experiments.GitHubTriggers:
		return c.enabled.Load() && !c.githubDisabled.Load()
	default:
		return false
	}
}

func (c *fakeCore) AdmitFire(ctx, itemCtx context.Context) (context.Context, context.Context, func(), bool) {
	if c.admitFn != nil {
		return c.admitFn(ctx, itemCtx)
	}
	return ctx, itemCtx, func() {}, true
}

func (c *fakeCore) AuthorizeEventSource(ctx context.Context, ev *store.TriggerEvent) (bool, error) {
	if c.authorizeSourceFn != nil {
		return c.authorizeSourceFn(ctx, ev)
	}
	return true, nil
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
		if len(filter.SourceIDs) > 0 && !slices.Contains(filter.SourceIDs, tr.SourceID) {
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
func (m *memTriggerStore) ProjectsWithTriggersBySourceKind(_ context.Context, projectIDs []string, sourceKind string) (map[string]bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]bool)
	for _, tr := range m.triggers {
		if tr.SourceKind == sourceKind && slices.Contains(projectIDs, tr.ProjectID) {
			out[tr.ProjectID] = true
		}
	}
	return out, nil
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

type testDispatchDialect struct {
	name        string
	maxEvalTime time.Duration
	mu          sync.Mutex
	evalCalls   []string
	maxBudgets  []time.Duration
	hasTrackers []bool
	fn          func(ctx context.Context, rule string, budget ruleengine.Budget) (ruleengine.Decision, error)
}

func (d *testDispatchDialect) Name() string { return d.name }

func (d *testDispatchDialect) Declares() ruleengine.Capabilities {
	maxTime := d.maxEvalTime
	if maxTime <= 0 {
		maxTime = 100 * time.Millisecond
	}
	return ruleengine.Capabilities{
		PerformsIO:  false,
		MaxEvalTime: maxTime,
		ResultKinds: []ruleengine.Kind{ruleengine.KindFilter},
	}
}

func (d *testDispatchDialect) Compile(_ context.Context, rule string, kind ruleengine.Kind, env ruleengine.EnvironmentRef) (ruleengine.Compiled, error) {
	return ruleengine.NewCompiled(env.Consumer, d.name, kind, rule), nil
}

func (d *testDispatchDialect) Evaluate(ctx context.Context, c ruleengine.Compiled, _ ruleengine.Input, budget ruleengine.Budget) (ruleengine.Decision, error) {
	rule, _ := c.Rep().(string)
	d.mu.Lock()
	d.evalCalls = append(d.evalCalls, rule)
	d.maxBudgets = append(d.maxBudgets, budget.MaxEvalTime)
	d.hasTrackers = append(d.hasTrackers, budget.Tracker != nil)
	d.mu.Unlock()
	if d.fn != nil {
		return d.fn(ctx, rule, budget)
	}
	return ruleengine.Decision{Kind: c.Kind(), Bool: true}, nil
}

func newTestTriggerEngineWithDialects(t *testing.T, extra ...ruleengine.Dialect) (*ruleengine.Engine, *triggerrule.Rules) {
	t.Helper()
	celEng, err := celengine.New(triggerrule.Env(), triggerrule.Stage1Limits(), triggerrule.Stage1CacheConfig())
	require.NoError(t, err)
	celDialect, err := ruleengine.NewCELDialect(celEng)
	require.NoError(t, err)
	re := ruleengine.NewEngine()
	require.NoError(t, re.Register(celDialect))
	grants := []ruleengine.DialectGrant{ruleengine.Grant(ruleengine.DialectCEL)}
	for _, ex := range extra {
		require.NoError(t, re.Register(ex))
		grants = append(grants, ruleengine.Grant(ex.Name()))
	}
	require.NoError(t, re.AllowConsumerDialects(triggerrule.ConsumerName, grants...))
	rules, err := triggerrule.New(re)
	require.NoError(t, err)
	return re, rules
}

type memFireAuditSink struct {
	mu      sync.Mutex
	records []trigger.FireAuditRecord
}

func (s *memFireAuditSink) RecordFire(_ context.Context, rec trigger.FireAuditRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, rec)
}

func (s *memFireAuditSink) snapshot() []trigger.FireAuditRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]trigger.FireAuditRecord, len(s.records))
	copy(out, s.records)
	return out
}

func TestDispatcher_MatchAllOutcomesTraceMetricsAndAudit(t *testing.T) {
	ctx := context.Background()
	ms := newMemTriggerStore()
	core := &fakeCore{}
	core.enabled.Store(true)

	uncertainDialect := &testDispatchDialect{
		name: "uncertain_test",
		fn: func(_ context.Context, rule string, _ ruleengine.Budget) (ruleengine.Decision, error) {
			if rule == "abstain_case" {
				return ruleengine.Decision{Trace: "model=probe,verdict=abstain"}, &ruleengine.UncertainError{Reason: "abstain"}
			}
			return ruleengine.Decision{Kind: ruleengine.KindFilter, Bool: true}, nil
		},
	}
	_, rules := newTestTriggerEngineWithDialects(t, uncertainDialect)

	projID := uuid.NewString()
	epID := uuid.NewString()
	evID := uuid.NewString()
	t0 := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

	trMatched := &store.Trigger{
		ID:         uuid.NewString(),
		ProjectID:  projID,
		Name:       "tr-matched",
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		MatchTypes: []string{"webhook.received"},
		MatchRules: []store.TriggerMatchRule{
			{Dialect: "cel", Expr: `event.attributes.branch == "main"`},
			{Dialect: "cel", Expr: `has(event.payload.pr) && event.payload.pr.number == 42`},
		},
		ActionType: store.TriggerActionMessage,
		Status:     store.TriggerStatusActive,
		InitiatorAttribution: store.InitiatorAttribution{
			AuthorizationRevision: 3,
		},
	}
	trFalseShortCircuit := &store.Trigger{
		ID:         uuid.NewString(),
		ProjectID:  projID,
		Name:       "tr-false",
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		MatchTypes: []string{"webhook.received"},
		MatchRules: []store.TriggerMatchRule{
			{Dialect: "cel", Expr: `event.attributes.branch == "release"`},
			{Dialect: "uncertain_test", Expr: `must_not_run`},
		},
		ActionType: store.TriggerActionMessage,
		Status:     store.TriggerStatusActive,
		InitiatorAttribution: store.InitiatorAttribution{
			AuthorizationRevision: 1,
		},
	}
	trUncertain := &store.Trigger{
		ID:         uuid.NewString(),
		ProjectID:  projID,
		Name:       "tr-uncertain",
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		MatchTypes: []string{"webhook.received"},
		MatchRules: []store.TriggerMatchRule{
			{Dialect: "cel", Expr: `event.attributes.branch == "main"`},
			{Dialect: "uncertain_test", Expr: `abstain_case`},
			{Dialect: "cel", Expr: `true`},
		},
		ActionType: store.TriggerActionMessage,
		Status:     store.TriggerStatusActive,
		InitiatorAttribution: store.InitiatorAttribution{
			AuthorizationRevision: 2,
		},
	}
	trFailedRuntime := &store.Trigger{
		ID:         uuid.NewString(),
		ProjectID:  projID,
		Name:       "tr-failed-runtime",
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		MatchTypes: []string{"webhook.received"},
		MatchRules: []store.TriggerMatchRule{
			{Dialect: "cel", Expr: `true`},
			{Dialect: "cel", Expr: `event.payload.missing_secret_key == "canary-value"`},
			{Dialect: "cel", Expr: `true`},
		},
		ActionType: store.TriggerActionMessage,
		Status:     store.TriggerStatusActive,
		InitiatorAttribution: store.InitiatorAttribution{
			AuthorizationRevision: 1,
		},
	}
	trNoLongerCompiles := &store.Trigger{
		ID:         uuid.NewString(),
		ProjectID:  projID,
		Name:       "tr-compile-broken",
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		MatchTypes: []string{"webhook.received"},
		MatchRules: []store.TriggerMatchRule{
			{Dialect: "cel", Expr: `invalid syntax ===`},
		},
		ActionType: store.TriggerActionMessage,
		Status:     store.TriggerStatusActive,
		InitiatorAttribution: store.InitiatorAttribution{
			AuthorizationRevision: 1,
		},
	}

	for _, tr := range []*store.Trigger{trMatched, trFalseShortCircuit, trUncertain, trFailedRuntime, trNoLongerCompiles} {
		require.NoError(t, ms.CreateTrigger(ctx, tr))
	}

	const secretPayloadCanary = "top-secret-webhook-body-9912"
	require.NoError(t, ms.CreateTriggerEvent(ctx, &store.TriggerEvent{
		ID:         evID,
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		Type:       "webhook.received",
		Subject:    "webhook:" + epID,
		DedupKey:   "d-outcomes",
		OccurredAt: t0,
		ReceivedAt: t0,
		Attributes: map[string]string{"branch": "main"},
		Payload:    `{"pr":{"number":42},"body":"` + secretPayloadCanary + `"}`,
		Status:     store.TriggerEventStatusPending,
	}))

	var firedIDs []string
	core.fireFn = func(_ context.Context, req hubfire.FireRequest) hubfire.FireOutcome {
		firedIDs = append(firedIDs, req.Authority.DefinitionID)
		return hubfire.FireOutcome{Status: hubfire.FireStatusFired, ResultRef: "msg-ok"}
	}

	var warnBuf, auditLogBuf bytes.Buffer
	warnLogger := slog.New(slog.NewJSONHandler(&warnBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	slogAudit := trigger.NewSlogAuditSink(trigger.SlogAuditConfig{
		Logger: slog.New(slog.NewJSONHandler(&auditLogBuf, &slog.HandlerOptions{Level: slog.LevelInfo})),
		Now:    func() time.Time { return t0 },
	})
	memAudit := &memFireAuditSink{}

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	otelMetrics, err := trigger.NewOTelRuleEvalMetrics(mp)
	require.NoError(t, err)

	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:        core,
		Store:       ms,
		Rules:       rules,
		RuleMetrics: otelMetrics,
		FireAudit: trigger.MultiFireAuditSink(
			slogAudit,
			memAudit,
		),
		Logger: warnLogger,
		Now:    func() time.Time { return t0 },
	})
	require.NoError(t, err)

	raw, err := json.Marshal(trigger.EventQueuePayload{Version: 1, EventID: evID})
	require.NoError(t, err)
	res := disp.HandleItem(ctx, durablequeue.Item{
		Seq:       1,
		Kind:      trigger.EventQueueKind,
		DedupKey:  evID,
		ProjectID: projID,
		Payload:   raw,
	}, nopLease{})
	require.NoError(t, res.Err)
	assert.True(t, res.Done)

	// Only trMatched fires; trFalseShortCircuit short-circuited before element 2.
	assert.Equal(t, []string{trMatched.ID}, firedIDs)
	assert.Equal(t, []string{"abstain_case"}, uncertainDialect.evalCalls, "element 2 of trFalse must not run after element 1 false")

	gotEv, err := ms.GetTriggerEvent(ctx, evID)
	require.NoError(t, err)
	assert.Equal(t, 1, gotEv.MatchCount, "match_count counts full matches only")
	assert.Equal(t, 1, gotEv.FireCount)

	firesRes, err := ms.ListTriggerFires(ctx, store.TriggerFireFilter{}, store.ListOptions{})
	require.NoError(t, err)
	byTrig := make(map[string]store.TriggerFire, len(firesRes.Items))
	for _, f := range firesRes.Items {
		byTrig[f.TriggerID] = f
	}
	assert.NotContains(t, byTrig, trFalseShortCircuit.ID, "false match must write no fire row")
	require.Len(t, byTrig, 4)

	// 1. matched -> fired, matched_through "2/2", decided_dialect "cel".
	fMatched := byTrig[trMatched.ID]
	assert.Equal(t, store.TriggerFireStatusFired, fMatched.Status)
	assert.Equal(t, "2/2", fMatched.MatchedThrough)
	assert.Equal(t, "cel", fMatched.DecidedDialect)
	var traceMatched triggerrule.RuleTrace
	require.NoError(t, json.Unmarshal([]byte(fMatched.RuleTrace), &traceMatched))
	require.Len(t, traceMatched.Elements, 2)
	assert.Equal(t, "matched", traceMatched.Elements[0].Class)
	assert.Equal(t, "matched", traceMatched.Elements[1].Class)
	assert.Empty(t, traceMatched.Unexecuted)
	assert.NotNil(t, traceMatched.BudgetRemainingMs)

	// 2. uncertain -> skipped "rule uncertain", matched_through "2/3", decided_dialect "uncertain_test".
	fUncertain := byTrig[trUncertain.ID]
	assert.Equal(t, store.TriggerFireStatusSkipped, fUncertain.Status)
	assert.Equal(t, triggerrule.SkipReasonRuleUncertain, fUncertain.Error)
	assert.Equal(t, "2/3", fUncertain.MatchedThrough)
	assert.Equal(t, "uncertain_test", fUncertain.DecidedDialect)
	var traceUnc triggerrule.RuleTrace
	require.NoError(t, json.Unmarshal([]byte(fUncertain.RuleTrace), &traceUnc))
	require.Len(t, traceUnc.Elements, 2)
	assert.Equal(t, "uncertain", traceUnc.Elements[1].Class)
	assert.Equal(t, "abstain", traceUnc.Elements[1].Reason)
	assert.Equal(t, []int{3}, traceUnc.Unexecuted)

	// 3. failed runtime -> skipped "rule evaluation failed", matched_through "2/3", decided_dialect "cel".
	fFailed := byTrig[trFailedRuntime.ID]
	assert.Equal(t, store.TriggerFireStatusSkipped, fFailed.Status)
	assert.Equal(t, triggerrule.SkipReasonEvalFailed, fFailed.Error)
	assert.Equal(t, "2/3", fFailed.MatchedThrough)
	assert.Equal(t, "cel", fFailed.DecidedDialect)
	assert.NotContains(t, fFailed.RuleTrace, "canary-value")
	assert.NotContains(t, fFailed.RuleTrace, secretPayloadCanary)
	var traceFail triggerrule.RuleTrace
	require.NoError(t, json.Unmarshal([]byte(fFailed.RuleTrace), &traceFail))
	require.Len(t, traceFail.Elements, 2)
	assert.Equal(t, "failed", traceFail.Elements[1].Class)
	assert.Equal(t, celengine.ReasonMissingInput, traceFail.Elements[1].Reason)
	assert.Equal(t, []int{3}, traceFail.Unexecuted)

	// 4. stored compile error -> skipped "rule no longer compiles" + slog.Warn.
	fCompile := byTrig[trNoLongerCompiles.ID]
	assert.Equal(t, store.TriggerFireStatusSkipped, fCompile.Status)
	assert.Equal(t, triggerrule.SkipReasonNoLongerCompiles, fCompile.Error)
	assert.Equal(t, "1/1", fCompile.MatchedThrough)
	assert.Equal(t, "cel", fCompile.DecidedDialect)
	assert.Contains(t, warnBuf.String(), trNoLongerCompiles.ID, "stored compile error must emit slog.Warn")
	assert.NotContains(t, warnBuf.String(), "invalid syntax ===", "warning log must remain content-free")

	// Skipped rules never increment ErrorCount on trigger definitions (§5.5, §6.3).
	for _, skipID := range []string{trUncertain.ID, trFailedRuntime.ID, trNoLongerCompiles.ID} {
		gotTr, gErr := ms.GetTrigger(ctx, skipID)
		require.NoError(t, gErr)
		assert.Equal(t, 1, gotTr.FireCount)
		assert.Equal(t, 0, gotTr.ErrorCount)
	}

	// Audit records carry matched_through and decided_dialect, and NEVER rule text or payload (§5).
	auditRecs := memAudit.snapshot()
	require.Len(t, auditRecs, 4)
	auditOut := auditLogBuf.String()
	assert.Contains(t, auditOut, `"event_type":"trigger.fire.fired"`)
	assert.Contains(t, auditOut, `"event_type":"trigger.fire.skipped"`)
	assert.Contains(t, auditOut, `"matched_through":"2/2"`)
	assert.Contains(t, auditOut, `"matched_through":"2/3"`)
	assert.Contains(t, auditOut, `"decided_dialect":"uncertain_test"`)
	for _, forbidden := range []string{
		secretPayloadCanary,
		"canary-value",
		"missing_secret_key",
		"event.attributes.branch",
		"invalid syntax ===",
	} {
		assert.NotContains(t, auditOut, forbidden, "audit log must never carry rule text or payload")
	}

	// Per-dialect evaluation counters & histogram (trigger_rule_eval_seconds{dialect, class}, §5).
	stats := disp.RuleEvalStats()
	assert.Equal(t, int64(4), stats[trigger.RuleEvalKey{Dialect: "cel", Class: "matched"}].Count)
	assert.Equal(t, int64(1), stats[trigger.RuleEvalKey{Dialect: "cel", Class: "false"}].Count)
	assert.Equal(t, int64(2), stats[trigger.RuleEvalKey{Dialect: "cel", Class: "failed"}].Count)
	assert.Equal(t, int64(1), stats[trigger.RuleEvalKey{Dialect: "uncertain_test", Class: "uncertain"}].Count)

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(ctx, &rm))
	var foundHist bool
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == trigger.MetricTriggerRuleEval {
				foundHist = true
				assert.Equal(t, "s", m.Unit, "histogram unit must be seconds for _seconds Prometheus export")
				hist, ok := m.Data.(metricdata.Histogram[float64])
				require.True(t, ok)
				assert.GreaterOrEqual(t, len(hist.DataPoints), 4)
			}
		}
	}
	assert.True(t, foundHist, "expected %s OTel histogram", trigger.MetricTriggerRuleEval)
}

func TestDispatcher_CorruptStoredMatchRules_SkipsAndNeverFires(t *testing.T) {
	ctx := context.Background()
	ms := newMemTriggerStore()
	core := &fakeCore{}
	core.enabled.Store(true)
	_, rules := newTestTriggerEngineWithDialects(t)

	projID := uuid.NewString()
	epID := uuid.NewString()
	evID := uuid.NewString()
	t0 := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

	trCorruptFlag := &store.Trigger{
		ID:                uuid.NewString(),
		ProjectID:         projID,
		Name:              "corrupt-json",
		SourceKind:        store.TriggerSourceKindWebhook,
		SourceID:          epID,
		MatchTypes:        []string{"webhook.received"},
		MatchRulesCorrupt: true,
		ActionType:        store.TriggerActionMessage,
		Status:            store.TriggerStatusActive,
	}
	trEmptyElem := &store.Trigger{
		ID:         uuid.NewString(),
		ProjectID:  projID,
		Name:       "corrupt-empty-elem",
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		MatchTypes: []string{"webhook.received"},
		MatchRules: []store.TriggerMatchRule{{Dialect: "cel", Expr: "   "}},
		ActionType: store.TriggerActionMessage,
		Status:     store.TriggerStatusActive,
	}
	nineElems := make([]store.TriggerMatchRule, triggerrule.MaxMatchRules+1)
	for i := range nineElems {
		nineElems[i] = store.TriggerMatchRule{Dialect: "cel", Expr: "true"}
	}
	trTooMany := &store.Trigger{
		ID:         uuid.NewString(),
		ProjectID:  projID,
		Name:       "corrupt-9-elems",
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		MatchTypes: []string{"webhook.received"},
		MatchRules: nineElems,
		ActionType: store.TriggerActionMessage,
		Status:     store.TriggerStatusActive,
	}
	trGood := &store.Trigger{
		ID:         uuid.NewString(),
		ProjectID:  projID,
		Name:       "healthy-neighbor",
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		MatchTypes: []string{"webhook.received"},
		MatchRules: []store.TriggerMatchRule{{Dialect: "cel", Expr: "true"}},
		ActionType: store.TriggerActionMessage,
		Status:     store.TriggerStatusActive,
	}

	for _, tr := range []*store.Trigger{trCorruptFlag, trEmptyElem, trTooMany, trGood} {
		require.NoError(t, ms.CreateTrigger(ctx, tr))
	}
	require.NoError(t, ms.CreateTriggerEvent(ctx, &store.TriggerEvent{
		ID:         evID,
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		Type:       "webhook.received",
		Subject:    "webhook:" + epID,
		DedupKey:   "d-corrupt",
		OccurredAt: t0,
		ReceivedAt: t0,
		Payload:    `{"ok":true}`,
		Status:     store.TriggerEventStatusPending,
	}))

	var firedIDs []string
	core.fireFn = func(_ context.Context, req hubfire.FireRequest) hubfire.FireOutcome {
		firedIDs = append(firedIDs, req.Authority.DefinitionID)
		return hubfire.FireOutcome{Status: hubfire.FireStatusFired}
	}

	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:  core,
		Store: ms,
		Rules: rules,
		Now:   func() time.Time { return t0 },
	})
	require.NoError(t, err)

	raw, _ := json.Marshal(trigger.EventQueuePayload{Version: 1, EventID: evID})
	res := disp.HandleItem(ctx, durablequeue.Item{
		Kind:     trigger.EventQueueKind,
		DedupKey: evID,
		Payload:  raw,
	}, nopLease{})
	require.NoError(t, res.Err)
	assert.True(t, res.Done)

	// SEC-3b1rw-01: corrupt triggers NEVER fire; healthy neighbor still fires.
	assert.Equal(t, []string{trGood.ID}, firedIDs)

	firesRes, err := ms.ListTriggerFires(ctx, store.TriggerFireFilter{}, store.ListOptions{})
	require.NoError(t, err)
	byTrig := make(map[string]store.TriggerFire, len(firesRes.Items))
	for _, f := range firesRes.Items {
		byTrig[f.TriggerID] = f
	}
	for _, tc := range []struct {
		id         string
		wantReason string
	}{
		{trCorruptFlag.ID, triggerrule.MatchReasonCorruptRules},
		{trEmptyElem.ID, triggerrule.MatchReasonCorruptRules},
		{trTooMany.ID, celengine.CategorySize},
	} {
		f, ok := byTrig[tc.id]
		require.True(t, ok, "expected skipped fire row for corrupt trigger %s", tc.id)
		assert.Equal(t, store.TriggerFireStatusSkipped, f.Status)
		assert.Equal(t, triggerrule.SkipReasonEvalFailed, f.Error)
		assert.Contains(t, f.RuleTrace, tc.wantReason)
	}
}

func TestDispatcher_SharedEventBudgetAlwaysPassedAndClampsAcrossTriggers(t *testing.T) {
	ctx := context.Background()
	ms := newMemTriggerStore()
	core := &fakeCore{}
	core.enabled.Store(true)

	slowDialect := &testDispatchDialect{
		name:        "slow_probe",
		maxEvalTime: 500 * time.Millisecond,
		fn: func(evalCtx context.Context, rule string, _ ruleengine.Budget) (ruleengine.Decision, error) {
			switch rule {
			case "spend_45ms":
				time.Sleep(45 * time.Millisecond)
				return ruleengine.Decision{Kind: ruleengine.KindFilter, Bool: true}, nil
			case "block_until_deadline":
				<-evalCtx.Done()
				return ruleengine.Decision{}, evalCtx.Err()
			default:
				return ruleengine.Decision{Kind: ruleengine.KindFilter, Bool: true}, nil
			}
		},
	}
	_, rules := newTestTriggerEngineWithDialects(t, slowDialect)

	projID := uuid.NewString()
	epID := uuid.NewString()
	evID := uuid.NewString()
	t0 := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

	tr1 := &store.Trigger{
		ID:         uuid.NewString(),
		ProjectID:  projID,
		Name:       "tr-1-spends-part",
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		MatchTypes: []string{"webhook.received"},
		MatchRules: []store.TriggerMatchRule{{Dialect: "slow_probe", Expr: "spend_45ms"}},
		ActionType: store.TriggerActionMessage,
		Status:     store.TriggerStatusActive,
	}
	tr2 := &store.Trigger{
		ID:         uuid.NewString(),
		ProjectID:  projID,
		Name:       "tr-2-exhausts-remainder",
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		MatchTypes: []string{"webhook.received"},
		MatchRules: []store.TriggerMatchRule{
			{Dialect: "slow_probe", Expr: "block_until_deadline"},
			{Dialect: "slow_probe", Expr: "never_reached"},
		},
		ActionType: store.TriggerActionMessage,
		Status:     store.TriggerStatusActive,
	}
	tr3 := &store.Trigger{
		ID:         uuid.NewString(),
		ProjectID:  projID,
		Name:       "tr-3-pre-exhausted",
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		MatchTypes: []string{"webhook.received"},
		MatchRules: []store.TriggerMatchRule{{Dialect: "slow_probe", Expr: "never_started"}},
		ActionType: store.TriggerActionMessage,
		Status:     store.TriggerStatusActive,
	}
	for _, tr := range []*store.Trigger{tr1, tr2, tr3} {
		require.NoError(t, ms.CreateTrigger(ctx, tr))
	}
	require.NoError(t, ms.CreateTriggerEvent(ctx, &store.TriggerEvent{
		ID:         evID,
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		Type:       "webhook.received",
		Subject:    "webhook:" + epID,
		DedupKey:   "d-shared-budget",
		OccurredAt: t0,
		ReceivedAt: t0,
		Payload:    `{"ok":true}`,
		Status:     store.TriggerEventStatusPending,
	}))

	const eventBudget = 80 * time.Millisecond
	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:       core,
		Store:      ms,
		Rules:      rules,
		RuleBudget: eventBudget,
		Now:        func() time.Time { return t0 },
	})
	require.NoError(t, err)

	raw, _ := json.Marshal(trigger.EventQueuePayload{Version: 1, EventID: evID})
	res := disp.HandleItem(ctx, durablequeue.Item{
		Kind:     trigger.EventQueueKind,
		DedupKey: evID,
		Payload:  raw,
	}, nopLease{})
	require.NoError(t, res.Err)
	assert.True(t, res.Done)

	// SEC-3b1rw-01 & design §8 item 4: WithBudget is always passed and clamps each
	// element's deadline to the remaining aggregate event budget (< 500ms dialect MaxEvalTime).
	require.Len(t, slowDialect.evalCalls, 2, "tr3 and element 2 of tr2 must not execute once budget is exhausted")
	assert.Equal(t, []bool{true, true}, slowDialect.hasTrackers, "dispatcher must always pass WithBudget tracker")
	assert.LessOrEqual(t, slowDialect.maxBudgets[0], eventBudget)
	assert.Less(t, slowDialect.maxBudgets[1], slowDialect.maxBudgets[0], "second trigger's element deadline must clamp to smaller remainder")

	firesRes, err := ms.ListTriggerFires(ctx, store.TriggerFireFilter{}, store.ListOptions{})
	require.NoError(t, err)
	byTrig := make(map[string]store.TriggerFire, len(firesRes.Items))
	for _, f := range firesRes.Items {
		byTrig[f.TriggerID] = f
	}
	assert.Equal(t, store.TriggerFireStatusFired, byTrig[tr1.ID].Status)
	assert.Equal(t, "1/1", byTrig[tr1.ID].MatchedThrough)

	assert.Equal(t, store.TriggerFireStatusSkipped, byTrig[tr2.ID].Status)
	assert.Equal(t, triggerrule.SkipReasonBudgetExhausted, byTrig[tr2.ID].Error)
	assert.Equal(t, "1/2", byTrig[tr2.ID].MatchedThrough)

	assert.Equal(t, store.TriggerFireStatusSkipped, byTrig[tr3.ID].Status)
	assert.Equal(t, triggerrule.SkipReasonBudgetExhausted, byTrig[tr3.ID].Error)
	assert.Equal(t, "1/1", byTrig[tr3.ID].MatchedThrough)
}

func TestDispatcher_UngrantedStoredDialectAtEval_SkipsFailClosed(t *testing.T) {
	ctx := context.Background()
	ms := newMemTriggerStore()
	core := &fakeCore{}
	core.enabled.Store(true)

	tempDialect := &testDispatchDialect{name: "temp_dialect"}
	re, rules := newTestTriggerEngineWithDialects(t, tempDialect)

	// Narrow consumer "trigger" back to Grant("cel") only before evaluation (§8 item 7, SEC-3b1rw-03).
	require.NoError(t, re.AllowConsumerDialects(triggerrule.ConsumerName, ruleengine.Grant(ruleengine.DialectCEL)))

	projID := uuid.NewString()
	epID := uuid.NewString()
	trID := uuid.NewString()
	evID := uuid.NewString()
	t0 := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

	require.NoError(t, ms.CreateTrigger(ctx, &store.Trigger{
		ID:         trID,
		ProjectID:  projID,
		Name:       "ungranted-tr",
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		MatchTypes: []string{"webhook.received"},
		MatchRules: []store.TriggerMatchRule{
			{Dialect: "cel", Expr: "true"},
			{Dialect: "temp_dialect", Expr: "allow"},
		},
		ActionType: store.TriggerActionMessage,
		Status:     store.TriggerStatusActive,
	}))
	require.NoError(t, ms.CreateTriggerEvent(ctx, &store.TriggerEvent{
		ID:         evID,
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		Type:       "webhook.received",
		Subject:    "webhook:" + epID,
		DedupKey:   "d-ungranted",
		OccurredAt: t0,
		ReceivedAt: t0,
		Payload:    `{}`,
		Status:     store.TriggerEventStatusPending,
	}))

	var fired bool
	core.fireFn = func(_ context.Context, _ hubfire.FireRequest) hubfire.FireOutcome {
		fired = true
		return hubfire.FireOutcome{Status: hubfire.FireStatusFired}
	}

	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:  core,
		Store: ms,
		Rules: rules,
		Now:   func() time.Time { return t0 },
	})
	require.NoError(t, err)

	raw, _ := json.Marshal(trigger.EventQueuePayload{Version: 1, EventID: evID})
	res := disp.HandleItem(ctx, durablequeue.Item{
		Kind:     trigger.EventQueueKind,
		DedupKey: evID,
		Payload:  raw,
	}, nopLease{})
	require.NoError(t, res.Err)
	assert.True(t, res.Done)
	assert.False(t, fired, "un-granted dialect at evaluation must never fire")
	assert.Empty(t, tempDialect.evalCalls)

	firesRes, err := ms.ListTriggerFires(ctx, store.TriggerFireFilter{}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, firesRes.Items, 1)
	assert.Equal(t, store.TriggerFireStatusSkipped, firesRes.Items[0].Status)
	assert.Equal(t, triggerrule.SkipReasonEvalFailed, firesRes.Items[0].Error)
	assert.Equal(t, "2/2", firesRes.Items[0].MatchedThrough)
	assert.Equal(t, "temp_dialect", firesRes.Items[0].DecidedDialect)
}

func TestDispatcher_SubjectRuleDerivationAndFallbacks(t *testing.T) {
	ctx := context.Background()
	ms := newMemTriggerStore()
	core := &fakeCore{}
	core.enabled.Store(true)
	_, rules := newTestTriggerEngineWithDialects(t)

	projID := uuid.NewString()
	epID := uuid.NewString()
	evID := uuid.NewString()
	t0 := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	longSourceSubj := "webhook:" + epID + "/src-" + strings.Repeat("z", 300)

	trValidSubj := &store.Trigger{
		ID:             uuid.NewString(),
		ProjectID:      projID,
		Name:           "tr-valid-subj",
		SourceKind:     store.TriggerSourceKindWebhook,
		SourceID:       epID,
		MatchTypes:     []string{"webhook.received"},
		MatchRules:     []store.TriggerMatchRule{{Dialect: "cel", Expr: `has(event.payload.service)`}},
		SubjectDialect: "cel",
		SubjectRule:    `"svc:" + string(event.payload.service)`,
		SubjectMode:    store.TriggerSubjectModeNew,
		ActionType:     store.TriggerActionMessage,
		Status:         store.TriggerStatusActive,
	}
	trEvalErrSubj := &store.Trigger{
		ID:             uuid.NewString(),
		ProjectID:      projID,
		Name:           "tr-err-subj",
		SourceKind:     store.TriggerSourceKindWebhook,
		SourceID:       epID,
		MatchTypes:     []string{"webhook.received"},
		MatchRules:     []store.TriggerMatchRule{{Dialect: "cel", Expr: "true"}},
		SubjectDialect: "cel",
		SubjectRule:    `string(event.payload.nonexistent_field)`,
		SubjectMode:    store.TriggerSubjectModeNew,
		ActionType:     store.TriggerActionMessage,
		Status:         store.TriggerStatusActive,
	}
	trOversizeSubj := &store.Trigger{
		ID:             uuid.NewString(),
		ProjectID:      projID,
		Name:           "tr-oversize-subj",
		SourceKind:     store.TriggerSourceKindWebhook,
		SourceID:       epID,
		MatchTypes:     []string{"webhook.received"},
		MatchRules:     []store.TriggerMatchRule{{Dialect: "cel", Expr: "true"}},
		SubjectDialect: "cel",
		SubjectRule:    `string(event.payload.big)`,
		SubjectMode:    store.TriggerSubjectModeNew,
		ActionType:     store.TriggerActionMessage,
		Status:         store.TriggerStatusActive,
	}
	trControlSubj := &store.Trigger{
		ID:             uuid.NewString(),
		ProjectID:      projID,
		Name:           "tr-ctrl-subj",
		SourceKind:     store.TriggerSourceKindWebhook,
		SourceID:       epID,
		MatchTypes:     []string{"webhook.received"},
		MatchRules:     []store.TriggerMatchRule{{Dialect: "cel", Expr: "true"}},
		SubjectDialect: "cel",
		SubjectRule:    `"bad\nline"`,
		SubjectMode:    store.TriggerSubjectModeNew,
		ActionType:     store.TriggerActionMessage,
		Status:         store.TriggerStatusActive,
	}

	for _, tr := range []*store.Trigger{trValidSubj, trEvalErrSubj, trOversizeSubj, trControlSubj} {
		require.NoError(t, ms.CreateTrigger(ctx, tr))
	}
	require.NoError(t, ms.CreateTriggerEvent(ctx, &store.TriggerEvent{
		ID:         evID,
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		Type:       "webhook.received",
		Subject:    longSourceSubj,
		DedupKey:   "d-subj",
		OccurredAt: t0,
		ReceivedAt: t0,
		Payload:    `{"service":"payments","big":"` + strings.Repeat("b", 300) + `"}`,
		Status:     store.TriggerEventStatusPending,
	}))

	deliveredSubj := make(map[string]string)
	core.fireFn = func(_ context.Context, req hubfire.FireRequest) hubfire.FireOutcome {
		deliveredSubj[req.Authority.DefinitionID] = req.Event.Subject
		return hubfire.FireOutcome{Status: hubfire.FireStatusFired}
	}

	var logBuf bytes.Buffer
	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:   core,
		Store:  ms,
		Rules:  rules,
		Logger: slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})),
		Now:    func() time.Time { return t0 },
	})
	require.NoError(t, err)

	raw, _ := json.Marshal(trigger.EventQueuePayload{Version: 1, EventID: evID})
	res := disp.HandleItem(ctx, durablequeue.Item{
		Kind:     trigger.EventQueueKind,
		DedupKey: evID,
		Payload:  raw,
	}, nopLease{})
	require.NoError(t, res.Err)
	assert.True(t, res.Done)

	firesRes, err := ms.ListTriggerFires(ctx, store.TriggerFireFilter{}, store.ListOptions{})
	require.NoError(t, err)
	byTrig := make(map[string]store.TriggerFire, len(firesRes.Items))
	for _, f := range firesRes.Items {
		byTrig[f.TriggerID] = f
	}
	require.Len(t, byTrig, 4)

	// Valid subject_rule derives trigger_fires.subject while keeping FireEvent.Subject
	// as the unchanged hub-shaped source event subject (stage1-design rev 8.34, L23).
	assert.Equal(t, "svc:payments", byTrig[trValidSubj.ID].Subject)
	assert.Equal(t, longSourceSubj, deliveredSubj[trValidSubj.ID])

	// Fallbacks preserve 256B truncation on trigger_fires.subject and full source subject on FireEvent.Subject.
	for _, fbID := range []string{trEvalErrSubj.ID, trOversizeSubj.ID, trControlSubj.ID} {
		assert.Equal(t, longSourceSubj[:trigger.MaxFireSubjectBytes], byTrig[fbID].Subject)
		assert.Equal(t, longSourceSubj, deliveredSubj[fbID])
		assert.Contains(t, logBuf.String(), fbID, "subject fallback must log warning for %s", fbID)
	}
}

func TestDispatcher_LegacyScalarMatchRuleAndEmptyRulesBehavior(t *testing.T) {
	ctx := context.Background()
	t0 := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

	t.Run("nil_engine_skips_legacy_scalar_and_fires_empty_rules", func(t *testing.T) {
		ms := newMemTriggerStore()
		var firedIDs []string
		core := &fakeCore{
			fireFn: func(_ context.Context, req hubfire.FireRequest) hubfire.FireOutcome {
				firedIDs = append(firedIDs, req.Authority.DefinitionID)
				return hubfire.FireOutcome{Status: hubfire.FireStatusFired}
			},
		}
		core.enabled.Store(true)

		projID := uuid.NewString()
		epID := uuid.NewString()
		evID := uuid.NewString()

		trLegacy := &store.Trigger{
			ID:           uuid.NewString(),
			ProjectID:    projID,
			Name:         "legacy-scalar-no-engine",
			SourceKind:   store.TriggerSourceKindWebhook,
			SourceID:     epID,
			MatchTypes:   []string{"webhook.received"},
			MatchDialect: store.TriggerDialectCEL,
			MatchRule:    `event.type == "webhook.received"`,
			ActionType:   store.TriggerActionMessage,
			Status:       store.TriggerStatusActive,
		}
		trEmpty := &store.Trigger{
			ID:         uuid.NewString(),
			ProjectID:  projID,
			Name:       "empty-rules-no-engine",
			SourceKind: store.TriggerSourceKindWebhook,
			SourceID:   epID,
			MatchTypes: []string{"webhook.received"},
			ActionType: store.TriggerActionMessage,
			Status:     store.TriggerStatusActive,
		}
		require.NoError(t, ms.CreateTrigger(ctx, trLegacy))
		require.NoError(t, ms.CreateTrigger(ctx, trEmpty))
		require.NoError(t, ms.CreateTriggerEvent(ctx, &store.TriggerEvent{
			ID:         evID,
			ProjectID:  projID,
			SourceKind: store.TriggerSourceKindWebhook,
			SourceID:   epID,
			Type:       "webhook.received",
			Subject:    "webhook:" + epID,
			DedupKey:   "d-legacy-nil",
			OccurredAt: t0,
			ReceivedAt: t0,
			Payload:    `{"ok":true}`,
			Status:     store.TriggerEventStatusPending,
		}))

		disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
			Core:  core,
			Store: ms,
			Now:   func() time.Time { return t0 },
		})
		require.NoError(t, err)

		raw, _ := json.Marshal(trigger.EventQueuePayload{Version: 1, EventID: evID})
		res := disp.HandleItem(ctx, durablequeue.Item{
			Kind:     trigger.EventQueueKind,
			DedupKey: evID,
			Payload:  raw,
		}, nopLease{})
		require.NoError(t, res.Err)
		assert.True(t, res.Done)
		assert.Equal(t, []string{trEmpty.ID}, firedIDs)

		fires, err := ms.ListTriggerFires(ctx, store.TriggerFireFilter{}, store.ListOptions{})
		require.NoError(t, err)
		byTrig := make(map[string]store.TriggerFire, len(fires.Items))
		for _, f := range fires.Items {
			byTrig[f.TriggerID] = f
		}
		assert.Equal(t, store.TriggerFireStatusSkipped, byTrig[trLegacy.ID].Status)
		assert.Equal(t, trigger.ErrFireRuleEvaluationFailed, byTrig[trLegacy.ID].Error)
		assert.Equal(t, store.TriggerFireStatusFired, byTrig[trEmpty.ID].Status)
	})

	t.Run("engine_skips_whitespace_legacy_scalar_and_evaluates_valid_legacy_scalar", func(t *testing.T) {
		ms := newMemTriggerStore()
		_, rules := newTestTriggerEngineWithDialects(t)
		var firedIDs []string
		core := &fakeCore{
			fireFn: func(_ context.Context, req hubfire.FireRequest) hubfire.FireOutcome {
				firedIDs = append(firedIDs, req.Authority.DefinitionID)
				return hubfire.FireOutcome{Status: hubfire.FireStatusFired}
			},
		}
		core.enabled.Store(true)

		projID := uuid.NewString()
		epID := uuid.NewString()
		evID := uuid.NewString()

		trWhitespaceLegacy := &store.Trigger{
			ID:           uuid.NewString(),
			ProjectID:    projID,
			Name:         "legacy-scalar-whitespace",
			SourceKind:   store.TriggerSourceKindWebhook,
			SourceID:     epID,
			MatchTypes:   []string{"webhook.received"},
			MatchDialect: store.TriggerDialectCEL,
			MatchRule:    "   ",
			ActionType:   store.TriggerActionMessage,
			Status:       store.TriggerStatusActive,
		}
		trValidLegacy := &store.Trigger{
			ID:           uuid.NewString(),
			ProjectID:    projID,
			Name:         "legacy-scalar-valid",
			SourceKind:   store.TriggerSourceKindWebhook,
			SourceID:     epID,
			MatchTypes:   []string{"webhook.received"},
			MatchDialect: store.TriggerDialectCEL,
			MatchRule:    `event.type == "webhook.received"`,
			ActionType:   store.TriggerActionMessage,
			Status:       store.TriggerStatusActive,
		}
		require.NoError(t, ms.CreateTrigger(ctx, trWhitespaceLegacy))
		require.NoError(t, ms.CreateTrigger(ctx, trValidLegacy))
		require.NoError(t, ms.CreateTriggerEvent(ctx, &store.TriggerEvent{
			ID:         evID,
			ProjectID:  projID,
			SourceKind: store.TriggerSourceKindWebhook,
			SourceID:   epID,
			Type:       "webhook.received",
			Subject:    "webhook:" + epID,
			DedupKey:   "d-legacy-eval",
			OccurredAt: t0,
			ReceivedAt: t0,
			Payload:    `{"ok":true}`,
			Status:     store.TriggerEventStatusPending,
		}))

		disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
			Core:  core,
			Store: ms,
			Rules: rules,
			Now:   func() time.Time { return t0 },
		})
		require.NoError(t, err)

		raw, _ := json.Marshal(trigger.EventQueuePayload{Version: 1, EventID: evID})
		res := disp.HandleItem(ctx, durablequeue.Item{
			Kind:     trigger.EventQueueKind,
			DedupKey: evID,
			Payload:  raw,
		}, nopLease{})
		require.NoError(t, res.Err)
		assert.True(t, res.Done)
		assert.Equal(t, []string{trValidLegacy.ID}, firedIDs)

		fires, err := ms.ListTriggerFires(ctx, store.TriggerFireFilter{}, store.ListOptions{})
		require.NoError(t, err)
		byTrig := make(map[string]store.TriggerFire, len(fires.Items))
		for _, f := range fires.Items {
			byTrig[f.TriggerID] = f
		}
		assert.Equal(t, store.TriggerFireStatusSkipped, byTrig[trWhitespaceLegacy.ID].Status)
		assert.Equal(t, trigger.ErrFireRuleEvaluationFailed, byTrig[trWhitespaceLegacy.ID].Error)
		assert.Equal(t, store.TriggerFireStatusFired, byTrig[trValidLegacy.ID].Status)
	})
}

func TestDispatcher_GitHubCandidateQueryMatchesEmptyAndExactSourceIDOnlyForGitHub(t *testing.T) {
	ms := newMemTriggerStore()
	now := time.Date(2026, 11, 18, 21, 0, 0, 0, time.UTC)

	var firedTriggerIDs []string
	var mu sync.Mutex
	core := &fakeCore{
		fireFn: func(_ context.Context, req hubfire.FireRequest) hubfire.FireOutcome {
			mu.Lock()
			firedTriggerIDs = append(firedTriggerIDs, req.Authority.DefinitionID)
			mu.Unlock()
			return hubfire.FireOutcome{Status: hubfire.FireStatusFired}
		},
	}
	core.enabled.Store(true)

	projID := uuid.NewString()
	ghWildcard := uuid.NewString()
	ghExact := uuid.NewString()
	ghOtherInst := uuid.NewString()
	whWildcard := uuid.NewString()
	whExact := uuid.NewString()
	epID := uuid.NewString()

	for _, tc := range []struct {
		id, name, kind, sourceID, matchType string
	}{
		{ghWildcard, "gh-wildcard", store.TriggerSourceKindGitHub, "", "github.issue_comment.created"},
		{ghExact, "gh-exact", store.TriggerSourceKindGitHub, "424242", "github.*"},
		{ghOtherInst, "gh-other-inst", store.TriggerSourceKindGitHub, "999999", "github.*"},
		{whWildcard, "wh-empty-id", store.TriggerSourceKindWebhook, "", "webhook.*"},
		{whExact, "wh-exact", store.TriggerSourceKindWebhook, epID, "webhook.*"},
	} {
		require.NoError(t, ms.CreateTrigger(context.Background(), &store.Trigger{
			ID:           tc.id,
			ProjectID:    projID,
			Name:         tc.name,
			SourceKind:   tc.kind,
			SourceID:     tc.sourceID,
			MatchTypes:   []string{tc.matchType},
			MatchDialect: store.TriggerDialectCEL,
			ActionType:   store.TriggerActionMessage,
			SubjectMode:  store.TriggerSubjectModeNew,
			Status:       store.TriggerStatusActive,
		}))
	}

	ghEvID := uuid.NewString()
	require.NoError(t, ms.CreateTriggerEvent(context.Background(), &store.TriggerEvent{
		ID:         ghEvID,
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindGitHub,
		SourceID:   "424242",
		Type:       "github.issue_comment.created",
		Subject:    "github:acme/widgets#7",
		DedupKey:   "d-gh",
		OccurredAt: now,
		ReceivedAt: now,
		Payload:    `{"action":"created"}`,
		Status:     store.TriggerEventStatusPending,
	}))

	whEvID := uuid.NewString()
	require.NoError(t, ms.CreateTriggerEvent(context.Background(), &store.TriggerEvent{
		ID:         whEvID,
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		Type:       "webhook.received",
		Subject:    "webhook:" + epID,
		DedupKey:   "d-wh",
		OccurredAt: now,
		ReceivedAt: now,
		Payload:    `{}`,
		Status:     store.TriggerEventStatusPending,
	}))

	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:  core,
		Store: ms,
		Now:   func() time.Time { return now },
	})
	require.NoError(t, err)

	ghRaw, _ := json.Marshal(trigger.EventQueuePayload{Version: 1, EventID: ghEvID})
	res := disp.HandleItem(context.Background(), durablequeue.Item{
		Kind:     trigger.EventQueueKind,
		DedupKey: ghEvID,
		Payload:  ghRaw,
	}, nopLease{})
	assert.True(t, res.Done)
	assert.ElementsMatch(t, []string{ghWildcard, ghExact}, firedTriggerIDs)

	firedTriggerIDs = nil
	whRaw, _ := json.Marshal(trigger.EventQueuePayload{Version: 1, EventID: whEvID})
	res = disp.HandleItem(context.Background(), durablequeue.Item{
		Kind:     trigger.EventQueueKind,
		DedupKey: whEvID,
		Payload:  whRaw,
	}, nopLease{})
	assert.True(t, res.Done)
	assert.Equal(t, []string{whExact}, firedTriggerIDs, "webhook lookup must remain exact source_id equality")
}

func TestDispatcher_GitHubSourceAuthorizerRefusesAllMatchedCandidatesOnce(t *testing.T) {
	ms := newMemTriggerStore()
	now := time.Date(2026, 11, 18, 21, 5, 0, 0, time.UTC)

	var (
		authCalls atomic.Int32
		fireCalls atomic.Int32
	)
	core := &fakeCore{
		authorizeSourceFn: func(_ context.Context, _ *store.TriggerEvent) (bool, error) {
			authCalls.Add(1)
			return false, nil
		},
		fireFn: func(_ context.Context, _ hubfire.FireRequest) hubfire.FireOutcome {
			fireCalls.Add(1)
			return hubfire.FireOutcome{Status: hubfire.FireStatusFired}
		},
	}
	core.enabled.Store(true)

	projID := uuid.NewString()
	trMatch1 := uuid.NewString()
	trMatch2 := uuid.NewString()
	trNonMatch := uuid.NewString()

	for _, tc := range []struct {
		id, name, matchType string
	}{
		{trMatch1, "gh-match-1", "github.issue_comment.created"},
		{trMatch2, "gh-match-2", "github.issue_comment.*"},
		{trNonMatch, "gh-nonmatch", "github.pull_request.opened"},
	} {
		require.NoError(t, ms.CreateTrigger(context.Background(), &store.Trigger{
			ID:           tc.id,
			ProjectID:    projID,
			Name:         tc.name,
			SourceKind:   store.TriggerSourceKindGitHub,
			SourceID:     "",
			MatchTypes:   []string{tc.matchType},
			MatchDialect: store.TriggerDialectCEL,
			ActionType:   store.TriggerActionMessage,
			SubjectMode:  store.TriggerSubjectModeNew,
			Status:       store.TriggerStatusActive,
		}))
	}

	// Event 1: no rule matches -> AuthorizeEventSource is not called.
	unmatchedEvID := uuid.NewString()
	require.NoError(t, ms.CreateTriggerEvent(context.Background(), &store.TriggerEvent{
		ID:         unmatchedEvID,
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindGitHub,
		SourceID:   "424242",
		Type:       "github.issues.closed",
		Subject:    "github:acme/widgets#1",
		DedupKey:   "d-unmatched",
		OccurredAt: now,
		ReceivedAt: now,
		Payload:    `{}`,
		Status:     store.TriggerEventStatusPending,
	}))

	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:  core,
		Store: ms,
		Now:   func() time.Time { return now },
	})
	require.NoError(t, err)

	rawUnmatched, _ := json.Marshal(trigger.EventQueuePayload{Version: 1, EventID: unmatchedEvID})
	res := disp.HandleItem(context.Background(), durablequeue.Item{
		Kind:     trigger.EventQueueKind,
		DedupKey: unmatchedEvID,
		Payload:  rawUnmatched,
	}, nopLease{})
	require.True(t, res.Done)
	assert.Zero(t, authCalls.Load(), "unmatched event should not check source authorization")
	assert.Zero(t, fireCalls.Load())

	// Event 2: two rules match -> AuthorizeEventSource runs once, refuses both matched rules, marks event processed.
	evID := uuid.NewString()
	require.NoError(t, ms.CreateTriggerEvent(context.Background(), &store.TriggerEvent{
		ID:         evID,
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindGitHub,
		SourceID:   "424242",
		Type:       "github.issue_comment.created",
		Subject:    "github:acme/widgets#12",
		DedupKey:   "d-refused",
		OccurredAt: now,
		ReceivedAt: now,
		Payload:    `{"action":"created"}`,
		Status:     store.TriggerEventStatusPending,
	}))
	rawEv, _ := json.Marshal(trigger.EventQueuePayload{Version: 1, EventID: evID})
	res = disp.HandleItem(context.Background(), durablequeue.Item{
		Kind:     trigger.EventQueueKind,
		DedupKey: evID,
		Payload:  rawEv,
	}, nopLease{})
	require.True(t, res.Done)
	assert.Equal(t, int32(1), authCalls.Load(), "source authorization runs once per event")
	assert.Zero(t, fireCalls.Load(), "Core.Fire must never run when association is unauthorized")

	ev, err := ms.GetTriggerEvent(context.Background(), evID)
	require.NoError(t, err)
	assert.Equal(t, store.TriggerEventStatusProcessed, ev.Status)
	assert.Equal(t, 2, ev.MatchCount)
	assert.Equal(t, 0, ev.FireCount)

	fires, err := ms.ListTriggerFires(context.Background(), store.TriggerFireFilter{}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, fires.Items, 2)
	gotTrIDs := make([]string, 0, 2)
	for _, f := range fires.Items {
		gotTrIDs = append(gotTrIDs, f.TriggerID)
		assert.Equal(t, store.TriggerFireStatusRefused, f.Status)
		assert.Equal(t, trigger.ErrFireGitHubAssociationUnauthorized, f.Error)
		assert.NotNil(t, f.FinishedAt)
	}
	assert.ElementsMatch(t, []string{trMatch1, trMatch2}, gotTrIDs)

	for _, id := range []string{trMatch1, trMatch2} {
		tr, gErr := ms.GetTrigger(context.Background(), id)
		require.NoError(t, gErr)
		assert.Equal(t, 1, tr.FireCount)
		assert.Equal(t, 1, tr.ErrorCount)
		assert.Equal(t, store.TriggerFireStatusRefused, tr.LastFireStatus)
		assert.Equal(t, trigger.ErrFireGitHubAssociationUnauthorized, tr.LastFireError)
	}
}

func TestDispatcher_GitHubEventWaitsWhenGitHubTriggersExperimentDisabled(t *testing.T) {
	ms := newMemTriggerStore()
	now := time.Date(2026, 11, 18, 21, 10, 0, 0, time.UTC)

	core := &fakeCore{}
	core.enabled.Store(true)
	core.githubDisabled.Store(true)

	projID := uuid.NewString()
	trID := uuid.NewString()
	evID := uuid.NewString()
	require.NoError(t, ms.CreateTrigger(context.Background(), &store.Trigger{
		ID:           trID,
		ProjectID:    projID,
		Name:         "gh-wait",
		SourceKind:   store.TriggerSourceKindGitHub,
		SourceID:     "",
		MatchTypes:   []string{"github.*"},
		MatchDialect: store.TriggerDialectCEL,
		ActionType:   store.TriggerActionMessage,
		SubjectMode:  store.TriggerSubjectModeNew,
		Status:       store.TriggerStatusActive,
	}))
	require.NoError(t, ms.CreateTriggerEvent(context.Background(), &store.TriggerEvent{
		ID:         evID,
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindGitHub,
		SourceID:   "424242",
		Type:       "github.issue_comment.created",
		Subject:    "github:acme/widgets#99",
		DedupKey:   "d-wait",
		OccurredAt: now,
		ReceivedAt: now,
		Payload:    `{}`,
		Status:     store.TriggerEventStatusPending,
	}))

	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:  core,
		Store: ms,
		Now:   func() time.Time { return now },
	})
	require.NoError(t, err)

	raw, _ := json.Marshal(trigger.EventQueuePayload{Version: 1, EventID: evID})
	res := disp.HandleItem(context.Background(), durablequeue.Item{
		Kind:     trigger.EventQueueKind,
		DedupKey: evID,
		Payload:  raw,
	}, nopLease{})
	assert.False(t, res.Done)
	assert.False(t, res.Permanent, "disabled github_triggers experiment must defer, not poison")
	assert.Error(t, res.Err)

	ev, err := ms.GetTriggerEvent(context.Background(), evID)
	require.NoError(t, err)
	assert.Equal(t, store.TriggerEventStatusPending, ev.Status)

	// Re-enabling hub.github_triggers processes the queued event.
	core.githubDisabled.Store(false)
	res = disp.HandleItem(context.Background(), durablequeue.Item{
		Kind:     trigger.EventQueueKind,
		DedupKey: evID,
		Payload:  raw,
	}, nopLease{})
	assert.True(t, res.Done)
	ev, err = ms.GetTriggerEvent(context.Background(), evID)
	require.NoError(t, err)
	assert.Equal(t, store.TriggerEventStatusProcessed, ev.Status)
	assert.Equal(t, 1, ev.FireCount)
}

func TestDispatcher_GitHubSourceAuthorizerTransientErrorLeavesPendingAndRetries(t *testing.T) {
	ms := newMemTriggerStore()
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)

	core := &fakeCore{}
	core.enabled.Store(true)
	var transientFail atomic.Bool
	transientFail.Store(true)
	var authCalls atomic.Int32
	core.authorizeSourceFn = func(_ context.Context, _ *store.TriggerEvent) (bool, error) {
		authCalls.Add(1)
		if transientFail.Load() {
			return false, errors.New("github 502 bad gateway")
		}
		return true, nil
	}

	projID := uuid.NewString()
	trID := uuid.NewString()
	evID := uuid.NewString()
	require.NoError(t, ms.CreateTrigger(context.Background(), &store.Trigger{
		ID:           trID,
		ProjectID:    projID,
		Name:         "gh-transient",
		SourceKind:   store.TriggerSourceKindGitHub,
		SourceID:     "",
		MatchTypes:   []string{"github.issue_comment.created"},
		MatchDialect: store.TriggerDialectCEL,
		ActionType:   store.TriggerActionMessage,
		SubjectMode:  store.TriggerSubjectModeNew,
		Status:       store.TriggerStatusActive,
	}))
	require.NoError(t, ms.CreateTriggerEvent(context.Background(), &store.TriggerEvent{
		ID:         evID,
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindGitHub,
		SourceID:   "777001",
		Type:       "github.issue_comment.created",
		Subject:    "github:acme/widgets#11",
		DedupKey:   "d-transient",
		OccurredAt: now,
		ReceivedAt: now,
		Payload:    `{}`,
		Status:     store.TriggerEventStatusPending,
	}))

	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:  core,
		Store: ms,
		Now:   func() time.Time { return now },
	})
	require.NoError(t, err)

	raw, _ := json.Marshal(trigger.EventQueuePayload{Version: 1, EventID: evID})

	// 1. Transient/inconclusive check error: item is NOT Done and NOT Permanent;
	// event stays pending with zero fire rows (no refused fire).
	res := disp.HandleItem(context.Background(), durablequeue.Item{
		Kind:     trigger.EventQueueKind,
		DedupKey: evID,
		Payload:  raw,
	}, nopLease{})
	assert.False(t, res.Done)
	assert.False(t, res.Permanent, "inconclusive association check must retry until MaxAge, not poison")
	assert.Error(t, res.Err)
	assert.Equal(t, int32(1), authCalls.Load())

	ev, err := ms.GetTriggerEvent(context.Background(), evID)
	require.NoError(t, err)
	assert.Equal(t, store.TriggerEventStatusPending, ev.Status)

	fires, err := ms.ListTriggerFires(context.Background(), store.TriggerFireFilter{ProjectID: projID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, fires.Items, "transient check failure must not record a refused fire")

	// 2. Retry after transient error clears: event processes and fires normally.
	transientFail.Store(false)
	res = disp.HandleItem(context.Background(), durablequeue.Item{
		Kind:     trigger.EventQueueKind,
		DedupKey: evID,
		Payload:  raw,
	}, nopLease{})
	assert.True(t, res.Done)
	assert.NoError(t, res.Err)
	assert.Equal(t, int32(2), authCalls.Load())

	ev, err = ms.GetTriggerEvent(context.Background(), evID)
	require.NoError(t, err)
	assert.Equal(t, store.TriggerEventStatusProcessed, ev.Status)
	assert.Equal(t, 1, ev.FireCount)

	fires, err = ms.ListTriggerFires(context.Background(), store.TriggerFireFilter{ProjectID: projID}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, fires.Items, 1)
	assert.Equal(t, store.TriggerFireStatusFired, fires.Items[0].Status)
}
