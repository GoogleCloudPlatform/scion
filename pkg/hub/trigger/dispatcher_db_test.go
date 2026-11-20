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
	"fmt"
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
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/GoogleCloudPlatform/scion/pkg/triggerrule"
)

func seedProjectForDispatch(t *testing.T, cs *entadapter.CompositeStore) string {
	t.Helper()
	projID := uuid.NewString()
	require.NoError(t, cs.CreateProject(context.Background(), &store.Project{
		ID:   projID,
		Name: "disp-" + projID[:8],
		Slug: "disp-" + projID[:8],
	}))
	return projID
}

func makeActiveTrigger(projID, epID, name string, matchTypes []string) *store.Trigger {
	return &store.Trigger{
		ID:             uuid.NewString(),
		ProjectID:      projID,
		Name:           name,
		SourceKind:     store.TriggerSourceKindWebhook,
		SourceID:       epID,
		MatchTypes:     matchTypes,
		MatchDialect:   store.TriggerDialectCEL,
		ActionType:     store.TriggerActionMessage,
		ActionPayload:  `{"agentId":"agent-1","agentName":"lead","message":"hello"}`,
		SubjectDialect: store.TriggerDialectCEL,
		SubjectMode:    store.TriggerSubjectModeNew,
		Status:         store.TriggerStatusActive,
		InitiatorAttribution: store.InitiatorAttribution{
			InitiatorPrincipalKind:  store.DelegationPrincipalUser,
			InitiatorPrincipalID:    "user-1",
			InitiatorCredentialKind: store.InitiatorCredentialKindSession,
			AttributionVersion:      1,
			AuthorizationRevision:   1,
		},
	}
}

func TestDispatcher_MatchNoMatchFlagOffAndOutcomeAccounting(t *testing.T) {
	ctx := context.Background()
	cs := entadapter.NewCompositeStore(enttest.NewClient(t))
	t0 := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	q := durablequeue.NewQueue(cs, durablequeue.WithQueueClock(func() time.Time { return t0 }))
	projID := seedProjectForDispatch(t, cs)
	epID := uuid.NewString()

	trFired := makeActiveTrigger(projID, epID, "tr-fired", []string{"webhook.received"})
	trRefused := makeActiveTrigger(projID, epID, "tr-refused", []string{"webhook.*"})
	trFailed := makeActiveTrigger(projID, epID, "tr-failed", []string{"webhook.received"})
	trSkipped := makeActiveTrigger(projID, epID, "tr-skipped", []string{"webhook.received"})
	trNoMatch := makeActiveTrigger(projID, epID, "tr-nomatch", []string{"webhook.deploy.*"})
	trPaused := makeActiveTrigger(projID, epID, "tr-paused", []string{"webhook.received"})
	trPaused.Status = store.TriggerStatusPaused

	for _, tr := range []*store.Trigger{trFired, trRefused, trFailed, trSkipped, trNoMatch, trPaused} {
		require.NoError(t, cs.CreateTrigger(ctx, tr))
	}

	var afterRecordCalls atomic.Int32
	core := &fakeCore{}
	core.fireFn = func(_ context.Context, req hubfire.FireRequest) hubfire.FireOutcome {
		switch req.Authority.DefinitionID {
		case trFired.ID:
			return hubfire.FireOutcome{
				Status:    hubfire.FireStatusFired,
				ResultRef: "msg-123",
				AfterRecord: func(context.Context) {
					afterRecordCalls.Add(1)
				},
			}
		case trRefused.ID:
			return hubfire.FireOutcome{Status: hubfire.FireStatusRefused, Error: "message refused"}
		case trFailed.ID:
			return hubfire.FireOutcome{Status: hubfire.FireStatusFailed, Error: "dispatch failed"}
		case trSkipped.ID:
			return hubfire.FireOutcome{Status: hubfire.FireStatusSkipped, Error: "target not running"}
		default:
			t.Errorf("unexpected trigger fired: %s", req.Authority.DefinitionID)
			return hubfire.FireOutcome{Status: hubfire.FireStatusFailed, Error: "unexpected"}
		}
	}

	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:  core,
		Store: cs,
		Now:   func() time.Time { return t0 },
	})
	require.NoError(t, err)

	drainer, err := disp.NewDrainer(trigger.DrainerOptions{
		QueueStore:   cs,
		Queue:        q,
		PollInterval: 20 * time.Millisecond,
	})
	require.NoError(t, err)

	ev := &store.TriggerEvent{
		ID:         uuid.NewString(),
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		Type:       "webhook.received",
		Subject:    "webhook:" + epID,
		DedupKey:   "deliv-1",
		OccurredAt: t0,
		ReceivedAt: t0,
		Attributes: map[string]string{"endpoint_name": "ci"},
		Headers:    map[string]string{"Content-Type": "application/json"},
		Payload:    `{"build":42}`,
	}
	require.NoError(t, cs.WithTx(ctx, func(tx store.Store) error {
		_, enqErr := trigger.EnqueueEvent(ctx, tx, tx, q, ev)
		return enqErr
	}))

	// Flag off: drainer claims nothing (§16.2).
	core.enabled.Store(false)
	offRes, err := drainer.DrainOnce(ctx)
	require.NoError(t, err)
	assert.Zero(t, offRes.Claimed)
	gotPending, err := cs.GetTriggerEvent(ctx, ev.ID)
	require.NoError(t, err)
	assert.Equal(t, store.TriggerEventStatusPending, gotPending.Status)

	// Enable hub.triggers and run drainer via Run(ctx) + Wake.
	core.enabled.Store(true)
	runCtx, cancelRun := context.WithCancel(ctx)
	doneRun := make(chan struct{})
	go func() {
		defer close(doneRun)
		drainer.Run(runCtx)
	}()
	q.Wake(trigger.EventQueueKind)

	require.Eventually(t, func() bool {
		got, gErr := cs.GetTriggerEvent(ctx, ev.ID)
		return gErr == nil && got.Status == store.TriggerEventStatusProcessed
	}, 5*time.Second, 15*time.Millisecond)

	cancelRun()
	<-doneRun

	gotEv, err := cs.GetTriggerEvent(ctx, ev.ID)
	require.NoError(t, err)
	assert.Equal(t, 4, gotEv.MatchCount, "four active triggers matched webhook.received")
	assert.Equal(t, 1, gotEv.FireCount, "one trigger succeeded with status fired")
	assert.Equal(t, int32(1), afterRecordCalls.Load())

	// Verify §5.5 outcome accounting on each trigger definition.
	chk := func(id string, wantFires, wantErrors int, wantStatus string) {
		t.Helper()
		got, gErr := cs.GetTrigger(ctx, id)
		require.NoError(t, gErr)
		assert.Equal(t, wantFires, got.FireCount)
		assert.Equal(t, wantErrors, got.ErrorCount)
		assert.Equal(t, wantStatus, got.LastFireStatus)
	}
	chk(trFired.ID, 1, 0, store.TriggerFireStatusFired)
	chk(trRefused.ID, 1, 1, store.TriggerFireStatusRefused)
	chk(trFailed.ID, 1, 1, store.TriggerFireStatusFailed)
	chk(trSkipped.ID, 1, 0, store.TriggerFireStatusSkipped)
	chk(trNoMatch.ID, 0, 0, "")
	chk(trPaused.ID, 0, 0, "")
}

func TestDispatcher_TwoConcurrentDrainers_UniqueTriggerFire(t *testing.T) {
	ctx := context.Background()
	cs := entadapter.NewCompositeStore(enttest.NewClient(t))
	t0 := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	q := durablequeue.NewQueue(cs, durablequeue.WithQueueClock(func() time.Time { return t0 }))
	projID := seedProjectForDispatch(t, cs)
	epID := uuid.NewString()

	const numTriggers = 3
	const numEvents = 6
	for i := 0; i < numTriggers; i++ {
		tr := makeActiveTrigger(projID, epID, fmt.Sprintf("conc-tr-%d", i), []string{"webhook.received"})
		require.NoError(t, cs.CreateTrigger(ctx, tr))
	}

	var mu sync.Mutex
	fireCalls := make(map[string]int)
	core := &fakeCore{}
	core.enabled.Store(true)
	core.fireFn = func(_ context.Context, req hubfire.FireRequest) hubfire.FireOutcome {
		key := req.Event.ID + "|" + req.Authority.DefinitionID
		mu.Lock()
		fireCalls[key]++
		mu.Unlock()
		time.Sleep(5 * time.Millisecond)
		return hubfire.FireOutcome{Status: hubfire.FireStatusFired}
	}

	mkDrainer := func(owner string) *durablequeue.Drainer {
		disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
			Core:  core,
			Store: cs,
			Now:   func() time.Time { return t0 },
		})
		require.NoError(t, err)
		d, err := disp.NewDrainer(trigger.DrainerOptions{
			QueueStore:   cs,
			Queue:        q,
			Owner:        owner,
			BatchSize:    2,
			PollInterval: 15 * time.Millisecond,
		})
		require.NoError(t, err)
		return d
	}

	d1 := mkDrainer("replica-a")
	d2 := mkDrainer("replica-b")

	runCtx, cancelRun := context.WithCancel(ctx)
	var wg sync.WaitGroup
	for _, d := range []*durablequeue.Drainer{d1, d2} {
		wg.Add(1)
		go func(dr *durablequeue.Drainer) {
			defer wg.Done()
			dr.Run(runCtx)
		}(d)
	}

	eventIDs := make([]string, numEvents)
	for i := 0; i < numEvents; i++ {
		ev := &store.TriggerEvent{
			ID:         uuid.NewString(),
			ProjectID:  projID,
			SourceKind: store.TriggerSourceKindWebhook,
			SourceID:   epID,
			Type:       "webhook.received",
			Subject:    "webhook:" + epID,
			DedupKey:   fmt.Sprintf("conc-deliv-%d", i),
			OccurredAt: t0,
			ReceivedAt: t0,
			Payload:    fmt.Sprintf(`{"seq":%d}`, i),
		}
		eventIDs[i] = ev.ID
		require.NoError(t, cs.WithTx(ctx, func(tx store.Store) error {
			_, enqErr := trigger.EnqueueEvent(ctx, tx, tx, q, ev)
			return enqErr
		}))
		q.Wake(trigger.EventQueueKind)
	}

	require.Eventually(t, func() bool {
		for _, id := range eventIDs {
			got, err := cs.GetTriggerEvent(ctx, id)
			if err != nil || got.Status != store.TriggerEventStatusProcessed {
				return false
			}
		}
		return true
	}, 10*time.Second, 20*time.Millisecond)

	cancelRun()
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	assert.Len(t, fireCalls, numEvents*numTriggers)
	for pair, count := range fireCalls {
		assert.Equal(t, 1, count, "pair %s fired more than once", pair)
	}

	fires, err := cs.ListTriggerFires(ctx, store.TriggerFireFilter{ProjectID: projID}, store.ListOptions{Limit: 100})
	require.NoError(t, err)
	assert.Equal(t, numEvents*numTriggers, fires.TotalCount)
}

func TestDispatcher_LostQueueLeaseRehandle_NoDoubleFire(t *testing.T) {
	ctx := context.Background()
	cs := entadapter.NewCompositeStore(enttest.NewClient(t))
	t0 := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	projID := seedProjectForDispatch(t, cs)
	epID := uuid.NewString()

	recDialect := &testDispatchDialect{name: "rec_rehandle"}
	_, rules := newTestTriggerEngineWithDialects(t, recDialect)

	trAlreadyFired := makeActiveTrigger(projID, epID, "tr-1-done", []string{"webhook.received"})
	trAlreadyFired.MatchRules = []store.TriggerMatchRule{
		{Dialect: "rec_rehandle", Expr: "done_elem_1"},
		{Dialect: "cel", Expr: `event.payload.retry == true`},
	}
	trLiveFiring := makeActiveTrigger(projID, epID, "tr-2-live", []string{"webhook.received"})
	trLiveFiring.MatchRules = []store.TriggerMatchRule{
		{Dialect: "rec_rehandle", Expr: "live_elem_1"},
		{Dialect: "cel", Expr: `event.payload.retry == true`},
	}
	trUnclaimed := makeActiveTrigger(projID, epID, "tr-3-unclaimed", []string{"webhook.received"})
	trUnclaimed.MatchRules = []store.TriggerMatchRule{
		{Dialect: "rec_rehandle", Expr: "unclaimed_elem_1"},
		{Dialect: "cel", Expr: `event.payload.retry == true`},
	}
	for _, tr := range []*store.Trigger{trAlreadyFired, trLiveFiring, trUnclaimed} {
		require.NoError(t, cs.CreateTrigger(ctx, tr))
	}

	ev := &store.TriggerEvent{
		ID:         uuid.NewString(),
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		Type:       "webhook.received",
		Subject:    "webhook:" + epID,
		DedupKey:   "lost-lease-1",
		OccurredAt: t0,
		ReceivedAt: t0,
		Payload:    `{"retry":true}`,
		Status:     store.TriggerEventStatusPending,
	}
	require.NoError(t, cs.CreateTriggerEvent(ctx, ev))

	// Pre-populate trigger_fires as left by a previous replica whose queue lease expired:
	// trAlreadyFired is terminal "fired", trLiveFiring is still "firing" with a live horizon.
	finished := t0.Add(time.Second)
	claimed, _, err := cs.ClaimTriggerFire(ctx, &store.TriggerFire{
		ID:             uuid.NewString(),
		ProjectID:      projID,
		TriggerID:      trAlreadyFired.ID,
		EventID:        ev.ID,
		Subject:        ev.Subject,
		ActionType:     store.TriggerActionMessage,
		Status:         store.TriggerFireStatusFired,
		MatchedThrough: "2/2",
		DecidedDialect: "cel",
		LeaseUntil:     t0.Add(10 * time.Minute),
		StartedAt:      t0,
		FinishedAt:     &finished,
	})
	require.NoError(t, err)
	require.True(t, claimed)

	claimed, _, err = cs.ClaimTriggerFire(ctx, &store.TriggerFire{
		ID:             uuid.NewString(),
		ProjectID:      projID,
		TriggerID:      trLiveFiring.ID,
		EventID:        ev.ID,
		Subject:        ev.Subject,
		ActionType:     store.TriggerActionMessage,
		Status:         store.TriggerFireStatusFiring,
		MatchedThrough: "2/2",
		DecidedDialect: "cel",
		LeaseUntil:     t0.Add(10 * time.Minute),
		StartedAt:      t0,
	})
	require.NoError(t, err)
	require.True(t, claimed)

	var firedTriggerIDs []string
	core := &fakeCore{}
	core.enabled.Store(true)
	core.fireFn = func(_ context.Context, req hubfire.FireRequest) hubfire.FireOutcome {
		firedTriggerIDs = append(firedTriggerIDs, req.Authority.DefinitionID)
		return hubfire.FireOutcome{Status: hubfire.FireStatusFired}
	}

	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:  core,
		Store: cs,
		Rules: rules,
		Now:   func() time.Time { return t0.Add(3 * time.Minute) },
	})
	require.NoError(t, err)

	raw, _ := json.Marshal(trigger.EventQueuePayload{Version: 1, EventID: ev.ID})
	res := disp.HandleItem(ctx, durablequeue.Item{
		Seq:       1,
		Kind:      trigger.EventQueueKind,
		DedupKey:  ev.ID,
		ProjectID: projID,
		Payload:   raw,
	}, nopLease{})
	assert.True(t, res.Done)
	assert.Equal(t, []string{trUnclaimed.ID}, firedTriggerIDs, "only unclaimed trigger may fire on re-handle")

	// Design §3.4 & §8 item 6: re-handle explicitly re-evaluates candidate triggers from element 1,
	// while idempotent ClaimTriggerFire prevents double-firing already-claimed triggers.
	assert.ElementsMatch(t, []string{"done_elem_1", "live_elem_1", "unclaimed_elem_1"}, recDialect.evalCalls)

	unclaimedFires, err := cs.ListTriggerFires(ctx, store.TriggerFireFilter{TriggerID: trUnclaimed.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, unclaimedFires.Items, 1)
	assert.Equal(t, store.TriggerFireStatusFired, unclaimedFires.Items[0].Status)
	assert.Equal(t, "2/2", unclaimedFires.Items[0].MatchedThrough)
	assert.Equal(t, "cel", unclaimedFires.Items[0].DecidedDialect)

	gotEv, err := cs.GetTriggerEvent(ctx, ev.ID)
	require.NoError(t, err)
	assert.Equal(t, store.TriggerEventStatusProcessed, gotEv.Status)
	assert.Equal(t, 3, gotEv.MatchCount)
	assert.Equal(t, 2, gotEv.FireCount, "counts both previously-fired and newly-fired triggers")

	// Re-handling once event is already processed is an immediate Done no-op.
	firedTriggerIDs = nil
	res2 := disp.HandleItem(ctx, durablequeue.Item{
		Seq:       1,
		Kind:      trigger.EventQueueKind,
		DedupKey:  ev.ID,
		ProjectID: projID,
		Payload:   raw,
	}, nopLease{})
	assert.True(t, res2.Done)
	assert.Empty(t, firedTriggerIDs)
}

func TestDispatcher_OnDropAgeAndPoison_MarksEventFailed(t *testing.T) {
	ctx := context.Background()
	cs := entadapter.NewCompositeStore(enttest.NewClient(t))
	cur := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	var clockMu sync.Mutex
	nowFn := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return cur
	}
	setNow := func(ts time.Time) {
		clockMu.Lock()
		cur = ts
		clockMu.Unlock()
	}

	q := durablequeue.NewQueue(cs, durablequeue.WithQueueClock(nowFn))
	projID := seedProjectForDispatch(t, cs)
	epID := uuid.NewString()

	core := &fakeCore{}
	core.enabled.Store(true)
	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:  core,
		Store: cs,
		Now:   nowFn,
	})
	require.NoError(t, err)
	drainer, err := disp.NewDrainer(trigger.DrainerOptions{
		QueueStore:   cs,
		Queue:        q,
		MaxAge:       72 * time.Hour,
		MaxPermanent: 2,
	})
	require.NoError(t, err)

	// 1. Age trim drops item and marks event failed "dropped by queue limits".
	oldEv := &store.TriggerEvent{
		ID:         uuid.NewString(),
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		Type:       "webhook.received",
		Subject:    "webhook:" + epID,
		DedupKey:   "age-drop-1",
		OccurredAt: cur,
		ReceivedAt: cur,
		Payload:    `{}`,
	}
	require.NoError(t, cs.WithTx(ctx, func(tx store.Store) error {
		_, enqErr := trigger.EnqueueEvent(ctx, tx, tx, q, oldEv)
		return enqErr
	}))
	setNow(cur.Add(73 * time.Hour))
	_, err = drainer.DrainOnce(ctx)
	require.NoError(t, err)

	gotOld, err := cs.GetTriggerEvent(ctx, oldEv.ID)
	require.NoError(t, err)
	assert.Equal(t, store.TriggerEventStatusFailed, gotOld.Status)
	assert.Equal(t, trigger.ErrEventDroppedByQueueLimits, gotOld.Error)

	// 2. Poison item (corrupt payload hitting MaxPermanent=2) marks event failed "processing failed".
	poisonTime := nowFn()
	poisonEv := &store.TriggerEvent{
		ID:         uuid.NewString(),
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		Type:       "webhook.received",
		Subject:    "webhook:" + epID,
		DedupKey:   "poison-1",
		OccurredAt: poisonTime,
		ReceivedAt: poisonTime,
		Payload:    `{}`,
	}
	require.NoError(t, cs.WithTx(ctx, func(tx store.Store) error {
		if err := tx.CreateTriggerEvent(ctx, poisonEv); err != nil {
			return err
		}
		_, enqErr := q.Enqueue(ctx, tx, durablequeue.Item{
			Kind:      trigger.EventQueueKind,
			DedupKey:  poisonEv.ID,
			ProjectID: projID,
			Payload:   []byte(`not-json`),
		})
		return enqErr
	}))

	_, err = drainer.DrainOnce(ctx)
	require.NoError(t, err)
	setNow(poisonTime.Add(10 * time.Second))
	_, err = drainer.DrainOnce(ctx)
	require.NoError(t, err)

	gotPoison, err := cs.GetTriggerEvent(ctx, poisonEv.ID)
	require.NoError(t, err)
	assert.Equal(t, store.TriggerEventStatusFailed, gotPoison.Status)
	assert.Equal(t, trigger.ErrEventProcessingFailed, gotPoison.Error)
}

func TestRunMaintenance_ReaperReconcileAndPurge(t *testing.T) {
	ctx := context.Background()
	cs := entadapter.NewCompositeStore(enttest.NewClient(t))
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	projID := seedProjectForDispatch(t, cs)
	epID := uuid.NewString()

	tr := makeActiveTrigger(projID, epID, "maint-tr", []string{"webhook.received"})
	require.NoError(t, cs.CreateTrigger(ctx, tr))

	// Recent event with an expired firing row (reaped) and a live firing row (kept).
	recentEv := &store.TriggerEvent{
		ID:         uuid.NewString(),
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		Type:       "webhook.received",
		Subject:    "webhook:" + epID,
		DedupKey:   "maint-recent",
		OccurredAt: now.Add(-15 * time.Minute),
		ReceivedAt: now.Add(-15 * time.Minute),
		CreatedAt:  now.Add(-15 * time.Minute),
		Payload:    `{}`,
		Status:     store.TriggerEventStatusProcessed,
	}
	require.NoError(t, cs.CreateTriggerEvent(ctx, recentEv))

	expiredFire := &store.TriggerFire{
		ID:         uuid.NewString(),
		ProjectID:  projID,
		TriggerID:  tr.ID,
		EventID:    recentEv.ID,
		Subject:    recentEv.Subject,
		ActionType: store.TriggerActionMessage,
		Status:     store.TriggerFireStatusFiring,
		LeaseUntil: now.Add(-time.Minute),
		StartedAt:  now.Add(-11 * time.Minute),
		CreatedAt:  now.Add(-11 * time.Minute),
	}
	claimed, _, err := cs.ClaimTriggerFire(ctx, expiredFire)
	require.NoError(t, err)
	require.True(t, claimed)

	// Orphaned pending event older than 72h (reconciled to failed), $<7d so not yet purged.
	orphanEv := &store.TriggerEvent{
		ID:         uuid.NewString(),
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		Type:       "webhook.received",
		Subject:    "webhook:" + epID,
		DedupKey:   "maint-orphan-4d",
		OccurredAt: now.Add(-96 * time.Hour),
		ReceivedAt: now.Add(-96 * time.Hour),
		CreatedAt:  now.Add(-96 * time.Hour),
		Payload:    `{}`,
		Status:     store.TriggerEventStatusPending,
	}
	require.NoError(t, cs.CreateTriggerEvent(ctx, orphanEv))

	// Old processed event (> 7d) to be purged.
	oldPurgeEv := &store.TriggerEvent{
		ID:         uuid.NewString(),
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		Type:       "webhook.received",
		Subject:    "webhook:" + epID,
		DedupKey:   "maint-old-8d",
		OccurredAt: now.Add(-8 * 24 * time.Hour),
		ReceivedAt: now.Add(-8 * 24 * time.Hour),
		CreatedAt:  now.Add(-8 * 24 * time.Hour),
		Payload:    `{}`,
		Status:     store.TriggerEventStatusProcessed,
	}
	require.NoError(t, cs.CreateTriggerEvent(ctx, oldPurgeEv))

	mres, err := trigger.RunMaintenance(ctx, cs, now)
	require.NoError(t, err)
	assert.Equal(t, 1, mres.FiresReaped)
	assert.Equal(t, 1, mres.EventsReconciled)
	assert.Equal(t, 1, mres.EventsPurged)

	gotFire, err := cs.GetTriggerFire(ctx, expiredFire.ID)
	require.NoError(t, err)
	assert.Equal(t, store.TriggerFireStatusFailed, gotFire.Status)
	assert.Equal(t, "hub restarted during fire", gotFire.Error)

	gotTr, err := cs.GetTrigger(ctx, tr.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, gotTr.FireCount)
	assert.Equal(t, 1, gotTr.ErrorCount)

	gotOrphan, err := cs.GetTriggerEvent(ctx, orphanEv.ID)
	require.NoError(t, err)
	assert.Equal(t, store.TriggerEventStatusFailed, gotOrphan.Status)
	assert.Equal(t, trigger.ErrEventDroppedByQueueLimits, gotOrphan.Error)

	_, err = cs.GetTriggerEvent(ctx, oldPurgeEv.ID)
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestDispatcher_CorruptStoredMatchRulesSQLite_SkipsAndNeverFires(t *testing.T) {
	ctx := context.Background()
	entClient := enttest.NewClient(t)
	cs := entadapter.NewCompositeStore(entClient)
	t0 := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	q := durablequeue.NewQueue(cs, durablequeue.WithQueueClock(func() time.Time { return t0 }))
	projID := seedProjectForDispatch(t, cs)
	epID := uuid.NewString()

	_, rules := newTestTriggerEngineWithDialects(t)

	trCorrupt := makeActiveTrigger(projID, epID, "tr-corrupt-sqlite", []string{"webhook.received"})
	trCorrupt.MatchRules = []store.TriggerMatchRule{{Dialect: "cel", Expr: `event.payload.ok == true`}}
	require.NoError(t, cs.CreateTrigger(ctx, trCorrupt))

	// Corrupt the stored match_rules JSON directly in SQLite (SEC-3b1rw-01).
	require.NoError(t, entClient.Trigger.UpdateOneID(uuid.MustParse(trCorrupt.ID)).
		SetMatchRules(`{corrupt-not-json-array`).
		Exec(ctx))

	trHealthy := makeActiveTrigger(projID, epID, "tr-healthy-sqlite", []string{"webhook.received"})
	trHealthy.MatchRules = []store.TriggerMatchRule{{Dialect: "cel", Expr: `event.payload.ok == true`}}
	require.NoError(t, cs.CreateTrigger(ctx, trHealthy))

	var firedIDs []string
	core := &fakeCore{}
	core.enabled.Store(true)
	core.fireFn = func(_ context.Context, req hubfire.FireRequest) hubfire.FireOutcome {
		firedIDs = append(firedIDs, req.Authority.DefinitionID)
		return hubfire.FireOutcome{Status: hubfire.FireStatusFired}
	}

	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:  core,
		Store: cs,
		Rules: rules,
		Now:   func() time.Time { return t0 },
	})
	require.NoError(t, err)
	drainer, err := disp.NewDrainer(trigger.DrainerOptions{
		QueueStore: cs,
		Queue:      q,
	})
	require.NoError(t, err)

	ev := &store.TriggerEvent{
		ID:         uuid.NewString(),
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		Type:       "webhook.received",
		Subject:    "webhook:" + epID,
		DedupKey:   "corrupt-sqlite-1",
		OccurredAt: t0,
		ReceivedAt: t0,
		Payload:    `{"ok":true}`,
	}
	require.NoError(t, cs.WithTx(ctx, func(tx store.Store) error {
		_, enqErr := trigger.EnqueueEvent(ctx, tx, tx, q, ev)
		return enqErr
	}))

	dRes, err := drainer.DrainOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, dRes.Claimed)

	assert.Equal(t, []string{trHealthy.ID}, firedIDs, "corrupt stored match_rules row must never fire")

	corruptFires, err := cs.ListTriggerFires(ctx, store.TriggerFireFilter{TriggerID: trCorrupt.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, corruptFires.Items, 1)
	assert.Equal(t, store.TriggerFireStatusSkipped, corruptFires.Items[0].Status)
	assert.Equal(t, triggerrule.SkipReasonEvalFailed, corruptFires.Items[0].Error)
	assert.Contains(t, corruptFires.Items[0].RuleTrace, triggerrule.MatchReasonCorruptRules)
}

func TestDispatcherDB_SubjectModeSkip_WakeInFlightNewAlwaysFiresAndFiresAfterEnd(t *testing.T) {
	ctx := context.Background()
	cs := entadapter.NewCompositeStore(enttest.NewClient(t))
	t0 := time.Date(2026, 11, 19, 14, 0, 0, 0, time.UTC)
	q := durablequeue.NewQueue(cs, durablequeue.WithQueueClock(func() time.Time { return t0 }))
	projID := seedProjectForDispatch(t, cs)
	epID := uuid.NewString()

	trSkip := makeActiveTrigger(projID, epID, "db-skip-tr", []string{"webhook.received"})
	trSkip.SubjectMode = store.TriggerSubjectModeSkip
	require.NoError(t, cs.CreateTrigger(ctx, trSkip))

	trNew := makeActiveTrigger(projID, epID, "db-new-tr", []string{"webhook.received"})
	trNew.SubjectMode = store.TriggerSubjectModeNew
	require.NoError(t, cs.CreateTrigger(ctx, trNew))

	enqueueEv := func(dedup, subject string) *store.TriggerEvent {
		ev := &store.TriggerEvent{
			ID:         uuid.NewString(),
			ProjectID:  projID,
			SourceKind: store.TriggerSourceKindWebhook,
			SourceID:   epID,
			Type:       "webhook.received",
			Subject:    subject,
			DedupKey:   dedup,
			OccurredAt: t0,
			ReceivedAt: t0,
			Attributes: map[string]string{"endpoint_name": "ci"},
			Payload:    `{"ok":true}`,
		}
		require.NoError(t, cs.WithTx(ctx, func(tx store.Store) error {
			_, enqErr := trigger.EnqueueEvent(ctx, tx, tx, q, ev)
			return enqErr
		}))
		return ev
	}

	ev1 := enqueueEv("skip-db-1", "webhook:"+epID+"/pr/101")

	wakeEntered := make(chan struct{})
	releaseWake := make(chan struct{})
	var (
		mu        sync.Mutex
		skipCalls []string
		newCalls  []string
	)

	core := &fakeCore{}
	core.enabled.Store(true)
	core.fireFn = func(fctx context.Context, req hubfire.FireRequest) hubfire.FireOutcome {
		mu.Lock()
		switch req.Authority.DefinitionID {
		case trSkip.ID:
			skipCalls = append(skipCalls, req.Event.ID)
		case trNew.ID:
			newCalls = append(newCalls, req.Event.ID)
		}
		mu.Unlock()

		if req.Authority.DefinitionID == trSkip.ID && req.Event.ID == ev1.ID {
			detached, cancelDetached := req.Tracked.Detach(fctx)
			defer cancelDetached()
			close(wakeEntered)
			select {
			case <-releaseWake:
			case <-detached.Done():
				return hubfire.FireOutcome{Status: hubfire.FireStatusFailed, Error: detached.Err().Error()}
			}
		}
		return hubfire.FireOutcome{Status: hubfire.FireStatusFired, ResultRef: "msg-" + req.Event.ID[:8]}
	}

	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:           core,
		Store:          cs,
		Now:            func() time.Time { return t0 },
		PerFireTimeout: 40 * time.Millisecond,
	})
	require.NoError(t, err)

	d1, err := disp.NewDrainer(trigger.DrainerOptions{QueueStore: cs, Queue: q, BatchSize: 1})
	require.NoError(t, err)
	d2, err := disp.NewDrainer(trigger.DrainerOptions{QueueStore: cs, Queue: q, BatchSize: 1})
	require.NoError(t, err)

	d1Err := make(chan error, 1)
	go func() {
		_, err := d1.DrainOnce(ctx)
		d1Err <- err
	}()

	select {
	case <-wakeEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for d1 wake fire to enter")
	}

	// Enqueue and drain ev2 on the same subject via d2 while d1's wake fire is still in flight.
	ev2 := enqueueEv("skip-db-2", "webhook:"+epID+"/pr/101")
	res2, err := d2.DrainOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, res2.Claimed)

	// Enqueue and drain evOther on a different subject via d2 while pr/101 is still busy.
	evOther := enqueueEv("skip-db-other", "webhook:"+epID+"/pr/202")
	resOther, err := d2.DrainOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, resOther.Claimed)

	// End ev1's wake fire.
	close(releaseWake)
	require.NoError(t, <-d1Err)

	// Enqueue and drain ev3 on pr/101 after ev1's wake fire has ended: must fire again.
	ev3 := enqueueEv("skip-db-3", "webhook:"+epID+"/pr/101")
	res3, err := d2.DrainOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, res3.Claimed)

	mu.Lock()
	assert.Equal(t, []string{ev1.ID, evOther.ID, ev3.ID}, skipCalls)
	assert.ElementsMatch(t, []string{ev1.ID, ev2.ID, evOther.ID, ev3.ID}, newCalls)
	mu.Unlock()

	fires, err := cs.ListTriggerFires(ctx, store.TriggerFireFilter{TriggerID: trSkip.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, fires.Items, 4)
	statusByEvent := make(map[string]store.TriggerFire, len(fires.Items))
	for _, item := range fires.Items {
		statusByEvent[item.EventID] = item
	}
	assert.Equal(t, store.TriggerFireStatusFired, statusByEvent[ev1.ID].Status)
	assert.Equal(t, store.TriggerFireStatusSkipped, statusByEvent[ev2.ID].Status)
	assert.Equal(t, trigger.ErrFireSubjectBusy, statusByEvent[ev2.ID].Error)
	assert.Equal(t, store.TriggerFireStatusFired, statusByEvent[evOther.ID].Status)
	assert.Equal(t, store.TriggerFireStatusFired, statusByEvent[ev3.ID].Status)
}
