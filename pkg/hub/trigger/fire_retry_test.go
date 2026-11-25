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
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/durablequeue"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeRetryQueue struct {
	mu      sync.Mutex
	items   []durablequeue.Item
	wakes   []string
	nextSeq int64
}

func (q *fakeRetryQueue) Enqueue(_ context.Context, _ store.Tx, item durablequeue.Item) (int64, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.nextSeq++
	item.Seq = q.nextSeq
	q.items = append(q.items, item)
	return item.Seq, nil
}

func (q *fakeRetryQueue) Wake(kind string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.wakes = append(q.wakes, kind)
}

type fakeRetryCore struct {
	mu          sync.Mutex
	fireFn      func(ctx context.Context, req hubfire.FireRequest) hubfire.FireOutcome
	targetState trigger.RetryTargetState
	targetFn    func(ctx context.Context, agentID string) (trigger.RetryTargetState, error)
	fireCalls   atomic.Int32
}

func (c *fakeRetryCore) Fire(ctx context.Context, req hubfire.FireRequest) hubfire.FireOutcome {
	c.fireCalls.Add(1)
	if c.fireFn != nil {
		return c.fireFn(ctx, req)
	}
	return hubfire.FireOutcome{Status: hubfire.FireStatusFired}
}

func (c *fakeRetryCore) Experiment(string) bool { return true }

func (c *fakeRetryCore) CheckRetryTarget(ctx context.Context, agentID string) (trigger.RetryTargetState, error) {
	if c.targetFn != nil {
		return c.targetFn(ctx, agentID)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.targetState, nil
}

func TestFireRetryBackoff(t *testing.T) {
	assert.Equal(t, 30*time.Second, trigger.FireRetryBackoff(0))
	assert.Equal(t, 30*time.Second, trigger.FireRetryBackoff(1))
	assert.Equal(t, 60*time.Second, trigger.FireRetryBackoff(2))
	assert.Equal(t, 120*time.Second, trigger.FireRetryBackoff(3))
	assert.Equal(t, 240*time.Second, trigger.FireRetryBackoff(4))
	assert.Equal(t, 5*time.Minute, trigger.FireRetryBackoff(5))
	assert.Equal(t, 5*time.Minute, trigger.FireRetryBackoff(10))
}

func TestDispatcher_WaitingParksAndEnqueuesRetry(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	ms := newMemTriggerStore()
	q := &fakeRetryQueue{}
	targetID := uuid.NewString()

	tr := store.Trigger{
		ID:            uuid.NewString(),
		ProjectID:     uuid.NewString(),
		Name:          "offline-msg",
		SourceKind:    store.TriggerSourceKindWebhook,
		SourceID:      uuid.NewString(),
		MatchTypes:    []string{"webhook.received"},
		ActionType:    store.TriggerActionMessage,
		ActionPayload: `{"agentId":"` + targetID + `","message":"hello","offline":"queue","queueTtlSeconds":3600}`,
		Status:        store.TriggerStatusActive,
		InitiatorAttribution: store.InitiatorAttribution{
			AuthorizationRevision: 2,
		},
		CreatedAt: now,
	}
	ms.triggers = append(ms.triggers, &tr)

	ev := store.TriggerEvent{
		ID:         uuid.NewString(),
		ProjectID:  tr.ProjectID,
		SourceKind: tr.SourceKind,
		SourceID:   tr.SourceID,
		Type:       "webhook.received",
		Subject:    "webhook:1",
		ReceivedAt: now,
		Status:     store.TriggerEventStatusPending,
	}
	ms.events[ev.ID] = &ev

	core := &fakeRetryCore{
		fireFn: func(_ context.Context, _ hubfire.FireRequest) hubfire.FireOutcome {
			return hubfire.FireOutcome{
				Status:        hubfire.FireStatusWaiting,
				Error:         trigger.ErrFireTargetNotRunning,
				TargetAgentID: targetID,
			}
		},
	}
	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:  core,
		Store: ms,
		Queue: q,
		Now:   func() time.Time { return now },
	})
	require.NoError(t, err)

	payload, _ := json.Marshal(trigger.EventQueuePayload{Version: trigger.EventQueuePayloadVersion, EventID: ev.ID})
	res := disp.HandleItem(ctx, durablequeue.Item{
		Kind:      trigger.EventQueueKind,
		DedupKey:  ev.ID,
		ProjectID: tr.ProjectID,
		Payload:   payload,
		Attempts:  1,
	}, nil)
	assert.True(t, res.Done)

	// Event is marked processed with fire_count=0 (waiting is not fired).
	assert.Equal(t, store.TriggerEventStatusProcessed, ms.events[ev.ID].Status)
	assert.Equal(t, 1, ms.events[ev.ID].MatchCount)
	assert.Equal(t, 0, ms.events[ev.ID].FireCount)

	// Trigger counters must not count waiting.
	assert.Equal(t, 0, tr.FireCount)
	assert.Equal(t, 0, tr.ErrorCount)

	// Exactly one waiting fire row and one trigger.fire_retry queue item.
	require.Len(t, ms.byFireID, 1)
	var fire *store.TriggerFire
	for _, f := range ms.byFireID {
		fire = f
	}
	assert.Equal(t, store.TriggerFireStatusWaiting, fire.Status)
	assert.Equal(t, targetID, fire.TargetAgentID)
	assert.Equal(t, 1, fire.Attempts)
	require.NotNil(t, fire.WaitUntil)
	assert.Equal(t, now.Add(3600*time.Second), *fire.WaitUntil)

	require.Len(t, q.items, 1)
	assert.Equal(t, trigger.FireRetryQueueKind, q.items[0].Kind)
	assert.Equal(t, trigger.FireRetryDedupPrefix+fire.ID, q.items[0].DedupKey)
	assert.Equal(t, now.Add(trigger.FireRetryInitialBackoff), q.items[0].NotBefore)
}

func TestHandleFireRetryItem_NudgeWhileLeasedReCheck(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	ms := newMemTriggerStore()
	targetID := uuid.NewString()

	tr := store.Trigger{
		ID:            uuid.NewString(),
		ProjectID:     uuid.NewString(),
		Name:          "nudge-race",
		SourceKind:    store.TriggerSourceKindWebhook,
		SourceID:      uuid.NewString(),
		ActionType:    store.TriggerActionMessage,
		ActionPayload: `{"agentId":"` + targetID + `","message":"hello","offline":"queue"}`,
		Status:        store.TriggerStatusActive,
		InitiatorAttribution: store.InitiatorAttribution{
			AuthorizationRevision: 1,
		},
	}
	ms.triggers = append(ms.triggers, &tr)

	ev := store.TriggerEvent{
		ID:         uuid.NewString(),
		ProjectID:  tr.ProjectID,
		SourceKind: tr.SourceKind,
		SourceID:   tr.SourceID,
		Type:       "webhook.received",
		Subject:    "s1",
		ReceivedAt: now,
		Status:     store.TriggerEventStatusProcessed,
	}
	ms.events[ev.ID] = &ev

	waitUntil := now.Add(time.Hour)
	fire := store.TriggerFire{
		ID:            uuid.NewString(),
		ProjectID:     tr.ProjectID,
		TriggerID:     tr.ID,
		EventID:       ev.ID,
		Subject:       "s1",
		ActionType:    store.TriggerActionMessage,
		Status:        store.TriggerFireStatusWaiting,
		TargetAgentID: targetID,
		WaitUntil:     &waitUntil,
		Attempts:      1,
		InitiatorAttribution: store.InitiatorAttribution{
			AuthorizationRevision: 1,
		},
	}
	ms.byFireID[fire.ID] = &fire

	// Simulate: initial pre-check sees offline; Fire runs and returns waiting;
	// while Fire was executing (holding the queue lease), the target transitioned
	// to running, so the post-Fire CheckRetryTarget sees RetryTargetDeliverable!
	var checkCalls atomic.Int32
	core := &fakeRetryCore{
		targetFn: func(_ context.Context, _ string) (trigger.RetryTargetState, error) {
			if checkCalls.Add(1) == 1 {
				return trigger.RetryTargetOffline, nil
			}
			return trigger.RetryTargetDeliverable, nil
		},
		fireFn: func(_ context.Context, _ hubfire.FireRequest) hubfire.FireOutcome {
			return hubfire.FireOutcome{
				Status:        hubfire.FireStatusWaiting,
				Error:         trigger.ErrFireTargetNotRunning,
				TargetAgentID: targetID,
			}
		},
	}

	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:  core,
		Store: ms,
		Now:   func() time.Time { return now },
	})
	require.NoError(t, err)

	qp, _ := json.Marshal(trigger.FireRetryQueuePayload{Version: trigger.FireRetryQueuePayloadVersion, FireID: fire.ID})
	res := disp.HandleFireRetryItem(ctx, durablequeue.Item{
		Kind:      trigger.FireRetryQueueKind,
		DedupKey:  trigger.FireRetryDedupPrefix + fire.ID,
		ProjectID: tr.ProjectID,
		Payload:   qp,
	}, nil)
	assert.False(t, res.Done)
	assert.Equal(t, now, res.RetryAt, "must schedule immediate retry (not 60s backoff) when target became deliverable during lease")
	assert.Equal(t, 2, ms.byFireID[fire.ID].Attempts)
}

func TestHandleFireRetryItem_LifecycleBranches(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)

	setup := func(t *testing.T) (*memTriggerStore, *fakeRetryCore, *trigger.Dispatcher, *store.Trigger, *store.TriggerFire, durablequeue.Item) {
		t.Helper()
		ms := newMemTriggerStore()
		targetID := uuid.NewString()
		tr := &store.Trigger{
			ID:            uuid.NewString(),
			ProjectID:     uuid.NewString(),
			Name:          "branch-test",
			SourceKind:    store.TriggerSourceKindWebhook,
			SourceID:      uuid.NewString(),
			ActionType:    store.TriggerActionMessage,
			ActionPayload: `{"agentId":"` + targetID + `","message":"hi","offline":"queue"}`,
			Status:        store.TriggerStatusActive,
			InitiatorAttribution: store.InitiatorAttribution{
				AuthorizationRevision: 3,
			},
		}
		ms.triggers = append(ms.triggers, tr)
		ev := &store.TriggerEvent{
			ID:         uuid.NewString(),
			ProjectID:  tr.ProjectID,
			SourceKind: tr.SourceKind,
			SourceID:   tr.SourceID,
			Type:       "webhook.received",
			Subject:    "s1",
			ReceivedAt: now,
			Status:     store.TriggerEventStatusProcessed,
		}
		ms.events[ev.ID] = ev
		wu := now.Add(10 * time.Minute)
		fire := &store.TriggerFire{
			ID:            uuid.NewString(),
			ProjectID:     tr.ProjectID,
			TriggerID:     tr.ID,
			EventID:       ev.ID,
			Subject:       "s1",
			ActionType:    store.TriggerActionMessage,
			Status:        store.TriggerFireStatusWaiting,
			TargetAgentID: targetID,
			WaitUntil:     &wu,
			Attempts:      1,
			InitiatorAttribution: store.InitiatorAttribution{
				AuthorizationRevision: 3,
			},
		}
		ms.byFireID[fire.ID] = fire
		core := &fakeRetryCore{targetState: trigger.RetryTargetOffline}
		disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
			Core:  core,
			Store: ms,
			Now:   func() time.Time { return now },
		})
		require.NoError(t, err)
		qp, _ := json.Marshal(trigger.FireRetryQueuePayload{Version: trigger.FireRetryQueuePayloadVersion, FireID: fire.ID})
		item := durablequeue.Item{
			Kind:      trigger.FireRetryQueueKind,
			DedupKey:  trigger.FireRetryDedupPrefix + fire.ID,
			ProjectID: tr.ProjectID,
			Payload:   qp,
		}
		return ms, core, disp, tr, fire, item
	}

	t.Run("past wait_until expires fire and does not call Core.Fire", func(t *testing.T) {
		ms, core, disp, _, fire, item := setup(t)
		past := now.Add(-time.Second)
		fire.WaitUntil = &past
		res := disp.HandleFireRetryItem(ctx, item, nil)
		assert.True(t, res.Done)
		assert.Zero(t, core.fireCalls.Load())
		assert.Equal(t, store.TriggerFireStatusExpired, ms.byFireID[fire.ID].Status)
		assert.Equal(t, trigger.ErrFireOfflineQueueExpired, ms.byFireID[fire.ID].Error)
	})

	t.Run("trigger paused while waiting skips with trigger changed", func(t *testing.T) {
		ms, core, disp, tr, fire, item := setup(t)
		tr.Status = store.TriggerStatusPaused
		res := disp.HandleFireRetryItem(ctx, item, nil)
		assert.True(t, res.Done)
		assert.Zero(t, core.fireCalls.Load())
		assert.Equal(t, store.TriggerFireStatusSkipped, ms.byFireID[fire.ID].Status)
		assert.Equal(t, trigger.ErrFireTriggerChanged, ms.byFireID[fire.ID].Error)
	})

	t.Run("trigger edited (revision mismatch) skips with trigger changed", func(t *testing.T) {
		ms, core, disp, tr, fire, item := setup(t)
		tr.AuthorizationRevision = 4
		res := disp.HandleFireRetryItem(ctx, item, nil)
		assert.True(t, res.Done)
		assert.Zero(t, core.fireCalls.Load())
		assert.Equal(t, store.TriggerFireStatusSkipped, ms.byFireID[fire.ID].Status)
		assert.Equal(t, trigger.ErrFireTriggerChanged, ms.byFireID[fire.ID].Error)
	})

	t.Run("target deleted skips with target deleted", func(t *testing.T) {
		ms, core, disp, _, fire, item := setup(t)
		core.targetState = trigger.RetryTargetDeleted
		res := disp.HandleFireRetryItem(ctx, item, nil)
		assert.True(t, res.Done)
		assert.Zero(t, core.fireCalls.Load())
		assert.Equal(t, store.TriggerFireStatusSkipped, ms.byFireID[fire.ID].Status)
		assert.Equal(t, trigger.ErrFireTargetDeleted, ms.byFireID[fire.ID].Error)
	})

	t.Run("event deleted skips with event deleted", func(t *testing.T) {
		ms, core, disp, _, fire, item := setup(t)
		delete(ms.events, fire.EventID)
		res := disp.HandleFireRetryItem(ctx, item, nil)
		assert.True(t, res.Done)
		assert.Zero(t, core.fireCalls.Load())
		assert.Equal(t, store.TriggerFireStatusSkipped, ms.byFireID[fire.ID].Status)
		assert.Equal(t, trigger.ErrFireEventDeleted, ms.byFireID[fire.ID].Error)
		assert.Equal(t, trigger.FireSkipReasonEventDeleted, trigger.NormalizeFireSkipReason(trigger.ErrFireEventDeleted))
	})

	t.Run("post-attempt target terminal skips without backoff", func(t *testing.T) {
		ms, core, disp, _, fire, item := setup(t)
		var checkCalls atomic.Int32
		core.targetFn = func(_ context.Context, _ string) (trigger.RetryTargetState, error) {
			if checkCalls.Add(1) == 1 {
				return trigger.RetryTargetOffline, nil
			}
			return trigger.RetryTargetTerminal, nil
		}
		core.fireFn = func(_ context.Context, _ hubfire.FireRequest) hubfire.FireOutcome {
			return hubfire.FireOutcome{
				Status:        hubfire.FireStatusWaiting,
				Error:         trigger.ErrFireTargetNotRunning,
				TargetAgentID: fire.TargetAgentID,
			}
		}
		res := disp.HandleFireRetryItem(ctx, item, nil)
		assert.True(t, res.Done)
		assert.NoError(t, res.Err)
		assert.Equal(t, int32(1), core.fireCalls.Load())
		assert.Equal(t, store.TriggerFireStatusSkipped, ms.byFireID[fire.ID].Status)
		assert.Equal(t, trigger.ErrFireTargetNotRunning, ms.byFireID[fire.ID].Error)
	})

	t.Run("non-waiting terminal statuses return Done", func(t *testing.T) {
		for _, status := range []string{
			store.TriggerFireStatusFired,
			store.TriggerFireStatusFailed,
			store.TriggerFireStatusRefused,
			store.TriggerFireStatusSkipped,
			store.TriggerFireStatusExpired,
		} {
			t.Run(status, func(t *testing.T) {
				_, core, disp, _, fire, item := setup(t)
				fire.Status = status
				res := disp.HandleFireRetryItem(ctx, item, nil)
				assert.True(t, res.Done)
				assert.NoError(t, res.Err)
				assert.Zero(t, core.fireCalls.Load())
			})
		}
	})

	t.Run("backoff bounded by wait_until", func(t *testing.T) {
		_, core, disp, _, fire, item := setup(t)
		soon := now.Add(15 * time.Second)
		fire.WaitUntil = &soon
		core.fireFn = func(_ context.Context, _ hubfire.FireRequest) hubfire.FireOutcome {
			return hubfire.FireOutcome{Status: hubfire.FireStatusWaiting, Error: trigger.ErrFireTargetNotRunning, TargetAgentID: fire.TargetAgentID}
		}
		res := disp.HandleFireRetryItem(ctx, item, nil)
		assert.False(t, res.Done)
		assert.Equal(t, soon, res.RetryAt, "RetryAt must be clamped to wait_until")
	})
}

type failingFinishTriggerStore struct {
	*memTriggerStore
	failures atomic.Int32
}

func (s *failingFinishTriggerStore) FinishTriggerFire(ctx context.Context, id, status, resultRef, targetAgentID, errMsg string, finishedAt time.Time) (bool, error) {
	if s.failures.Add(1) == 1 {
		return false, errors.New("injected: connection reset")
	}
	return s.memTriggerStore.FinishTriggerFire(ctx, id, status, resultRef, targetAgentID, errMsg, finishedAt)
}

type captureFireAuditSink struct {
	mu      sync.Mutex
	records []trigger.FireAuditRecord
}

func (s *captureFireAuditSink) RecordFire(_ context.Context, rec trigger.FireAuditRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, rec)
}

func TestHandleFireRetryItem_AtMostOnceOnFinishFailureAndReap(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	ms := newMemTriggerStore()
	targetID := uuid.NewString()

	tr := &store.Trigger{
		ID:            uuid.NewString(),
		ProjectID:     uuid.NewString(),
		Name:          "at-most-once-finish-fail",
		SourceKind:    store.TriggerSourceKindWebhook,
		SourceID:      uuid.NewString(),
		ActionType:    store.TriggerActionMessage,
		ActionPayload: `{"agentId":"` + targetID + `","message":"hi","offline":"queue"}`,
		Status:        store.TriggerStatusActive,
		InitiatorAttribution: store.InitiatorAttribution{
			AuthorizationRevision: 3,
		},
	}
	ms.triggers = append(ms.triggers, tr)
	ev := &store.TriggerEvent{
		ID:         uuid.NewString(),
		ProjectID:  tr.ProjectID,
		SourceKind: tr.SourceKind,
		SourceID:   tr.SourceID,
		Type:       "webhook.received",
		ReceivedAt: now,
		Status:     store.TriggerEventStatusProcessed,
	}
	ms.events[ev.ID] = ev
	wu := now.Add(time.Hour)
	fire := &store.TriggerFire{
		ID:            uuid.NewString(),
		ProjectID:     tr.ProjectID,
		TriggerID:     tr.ID,
		EventID:       ev.ID,
		ActionType:    store.TriggerActionMessage,
		Status:        store.TriggerFireStatusWaiting,
		TargetAgentID: targetID,
		WaitUntil:     &wu,
		Attempts:      1,
		InitiatorAttribution: store.InitiatorAttribution{
			AuthorizationRevision: 3,
		},
	}
	ms.byFireID[fire.ID] = fire

	var deliveries atomic.Int32
	core := &fakeRetryCore{
		targetState: trigger.RetryTargetDeliverable,
		fireFn: func(context.Context, hubfire.FireRequest) hubfire.FireOutcome {
			deliveries.Add(1)
			return hubfire.FireOutcome{
				Status:        hubfire.FireStatusFired,
				ResultRef:     "msg-1",
				TargetAgentID: targetID,
			}
		},
	}
	fs := &failingFinishTriggerStore{memTriggerStore: ms}
	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:  core,
		Store: fs,
		Now:   func() time.Time { return now },
	})
	require.NoError(t, err)

	qp, _ := json.Marshal(trigger.FireRetryQueuePayload{Version: trigger.FireRetryQueuePayloadVersion, FireID: fire.ID})
	item := durablequeue.Item{
		Kind:      trigger.FireRetryQueueKind,
		DedupKey:  trigger.FireRetryDedupPrefix + fire.ID,
		ProjectID: tr.ProjectID,
		Payload:   qp,
	}

	// Attempt 1: claims waiting -> firing, delivers once, then FinishTriggerFire fails.
	res1 := disp.HandleFireRetryItem(ctx, item, nil)
	assert.False(t, res1.Done)
	assert.Error(t, res1.Err)
	assert.Equal(t, int32(1), deliveries.Load())
	assert.Equal(t, store.TriggerFireStatusFiring, ms.byFireID[fire.ID].Status)
	assert.Equal(t, 2, ms.byFireID[fire.ID].Attempts)

	// Attempt 2 (drainer retry while still firing): keeps the queue item alive
	// until LeaseUntil (never re-delivers, never strands a live owner).
	res2 := disp.HandleFireRetryItem(ctx, item, nil)
	assert.False(t, res2.Done)
	assert.NoError(t, res2.Err)
	assert.Equal(t, now.Add(trigger.FireLeaseHorizon), res2.RetryAt)
	assert.Equal(t, int32(1), deliveries.Load(), "waiting fire must never be delivered twice")

	// Stuck firing row is recovered by ReapExpiredTriggerFires once lease_until expires.
	reaped, err := ms.ReapExpiredTriggerFires(ctx, now.Add(trigger.FireLeaseHorizon+time.Second), 10)
	require.NoError(t, err)
	assert.Equal(t, 1, reaped)
	assert.Equal(t, store.TriggerFireStatusFailed, ms.byFireID[fire.ID].Status)
	assert.Equal(t, "hub restarted during fire", ms.byFireID[fire.ID].Error)
	assert.Equal(t, int32(1), deliveries.Load())

	// Attempt 3 (after reap transitioned row to failed): completes the queue item.
	res3 := disp.HandleFireRetryItem(ctx, item, nil)
	assert.True(t, res3.Done)
	assert.NoError(t, res3.Err)
	assert.Equal(t, int32(1), deliveries.Load())
}

// barrierGetFireStore holds all N concurrent GetTriggerFire callers until every
// worker has read the row, guaranteeing all workers observe status=waiting
// before any worker reaches ClaimWaitingTriggerFire.
type barrierGetFireStore struct {
	*memTriggerStore
	total   int32
	arrived atomic.Int32
	release chan struct{}
}

func (s *barrierGetFireStore) GetTriggerFire(ctx context.Context, id string) (*store.TriggerFire, error) {
	f, err := s.memTriggerStore.GetTriggerFire(ctx, id)
	if err != nil {
		return nil, err
	}
	if s.arrived.Add(1) == s.total {
		close(s.release)
	}
	select {
	case <-s.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return f, nil
}

func TestHandleFireRetryItem_AtMostOnceUnderConcurrentLeaseLoss(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	ms := newMemTriggerStore()
	targetID := uuid.NewString()

	tr := &store.Trigger{
		ID:            uuid.NewString(),
		ProjectID:     uuid.NewString(),
		Name:          "at-most-once-concurrent",
		SourceKind:    store.TriggerSourceKindWebhook,
		SourceID:      uuid.NewString(),
		ActionType:    store.TriggerActionMessage,
		ActionPayload: `{"agentId":"` + targetID + `","message":"hi","offline":"queue"}`,
		Status:        store.TriggerStatusActive,
		InitiatorAttribution: store.InitiatorAttribution{
			AuthorizationRevision: 1,
		},
	}
	ms.triggers = append(ms.triggers, tr)
	ev := &store.TriggerEvent{
		ID:         uuid.NewString(),
		ProjectID:  tr.ProjectID,
		SourceKind: tr.SourceKind,
		SourceID:   tr.SourceID,
		Type:       "webhook.received",
		ReceivedAt: now,
		Status:     store.TriggerEventStatusProcessed,
	}
	ms.events[ev.ID] = ev
	wu := now.Add(time.Hour)
	fire := &store.TriggerFire{
		ID:            uuid.NewString(),
		ProjectID:     tr.ProjectID,
		TriggerID:     tr.ID,
		EventID:       ev.ID,
		ActionType:    store.TriggerActionMessage,
		Status:        store.TriggerFireStatusWaiting,
		TargetAgentID: targetID,
		WaitUntil:     &wu,
		Attempts:      1,
		InitiatorAttribution: store.InitiatorAttribution{
			AuthorizationRevision: 1,
		},
	}
	ms.byFireID[fire.ID] = fire

	const workers = 4
	bs := &barrierGetFireStore{
		memTriggerStore: ms,
		total:           workers,
		release:         make(chan struct{}),
	}

	var deliveries atomic.Int32
	core := &fakeRetryCore{
		targetState: trigger.RetryTargetDeliverable,
		fireFn: func(context.Context, hubfire.FireRequest) hubfire.FireOutcome {
			deliveries.Add(1)
			time.Sleep(15 * time.Millisecond)
			return hubfire.FireOutcome{
				Status:        hubfire.FireStatusFired,
				ResultRef:     "msg-concurrent",
				TargetAgentID: targetID,
			}
		},
	}
	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:  core,
		Store: bs,
		Now:   func() time.Time { return now },
	})
	require.NoError(t, err)

	qp, _ := json.Marshal(trigger.FireRetryQueuePayload{Version: trigger.FireRetryQueuePayloadVersion, FireID: fire.ID})
	item := durablequeue.Item{
		Kind:      trigger.FireRetryQueueKind,
		DedupKey:  trigger.FireRetryDedupPrefix + fire.ID,
		ProjectID: tr.ProjectID,
		Payload:   qp,
	}

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res := disp.HandleFireRetryItem(ctx, item, nil)
			assert.NoError(t, res.Err)
		}()
	}
	wg.Wait()

	assert.Equal(t, int32(1), deliveries.Load(), "concurrent retry handlers must deliver at most once")
	assert.Equal(t, store.TriggerFireStatusFired, ms.byFireID[fire.ID].Status)
}

// TestHandleFireRetryItem_LeaseLossDoesNotStrandWaitingFire lifts Review-2
// probe TestP2bRev2Probe_LeaseLossStrandsWaitingFire: if replica B handles the
// item while replica A holds the fire in status=firing, B returns RetryAt
// (LeaseUntil) instead of Done so the queue item survives; when A returns the
// fire to waiting, a later retry resolves it once the target is deliverable.
func TestHandleFireRetryItem_LeaseLossDoesNotStrandWaitingFire(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	ms := newMemTriggerStore()
	targetID := uuid.NewString()

	tr := &store.Trigger{
		ID:            uuid.NewString(),
		ProjectID:     uuid.NewString(),
		Name:          "lease-loss-liveness",
		SourceKind:    store.TriggerSourceKindWebhook,
		SourceID:      uuid.NewString(),
		ActionType:    store.TriggerActionMessage,
		ActionPayload: `{"agentId":"` + targetID + `","message":"hi","offline":"queue"}`,
		Status:        store.TriggerStatusActive,
		InitiatorAttribution: store.InitiatorAttribution{
			AuthorizationRevision: 1,
		},
	}
	ms.triggers = append(ms.triggers, tr)
	ev := &store.TriggerEvent{
		ID:         uuid.NewString(),
		ProjectID:  tr.ProjectID,
		SourceKind: tr.SourceKind,
		SourceID:   tr.SourceID,
		Type:       "webhook.received",
		ReceivedAt: now,
		Status:     store.TriggerEventStatusProcessed,
	}
	ms.events[ev.ID] = ev
	wu := now.Add(time.Hour)
	fire := &store.TriggerFire{
		ID:            uuid.NewString(),
		ProjectID:     tr.ProjectID,
		TriggerID:     tr.ID,
		EventID:       ev.ID,
		ActionType:    store.TriggerActionMessage,
		Status:        store.TriggerFireStatusWaiting,
		TargetAgentID: targetID,
		WaitUntil:     &wu,
		Attempts:      1,
		InitiatorAttribution: store.InitiatorAttribution{
			AuthorizationRevision: 1,
		},
	}
	ms.byFireID[fire.ID] = fire

	inFire := make(chan struct{})
	release := make(chan struct{})
	var (
		attempt    atomic.Int32
		deliveries atomic.Int32
	)
	core := &fakeRetryCore{
		targetState: trigger.RetryTargetOffline,
		fireFn: func(context.Context, hubfire.FireRequest) hubfire.FireOutcome {
			if attempt.Add(1) == 1 {
				close(inFire)
				<-release
				return hubfire.FireOutcome{
					Status:        hubfire.FireStatusWaiting,
					Error:         trigger.ErrFireTargetNotRunning,
					TargetAgentID: targetID,
				}
			}
			deliveries.Add(1)
			return hubfire.FireOutcome{
				Status:        hubfire.FireStatusFired,
				ResultRef:     "msg-resolved",
				TargetAgentID: targetID,
			}
		},
	}

	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:  core,
		Store: ms,
		Now:   func() time.Time { return now },
	})
	require.NoError(t, err)

	qp, _ := json.Marshal(trigger.FireRetryQueuePayload{Version: trigger.FireRetryQueuePayloadVersion, FireID: fire.ID})
	item := durablequeue.Item{
		Kind:      trigger.FireRetryQueueKind,
		DedupKey:  trigger.FireRetryDedupPrefix + fire.ID,
		ProjectID: tr.ProjectID,
		Payload:   qp,
	}

	resACh := make(chan durablequeue.Result, 1)
	go func() {
		resACh <- disp.HandleFireRetryItem(ctx, item, nil)
	}()
	<-inFire

	// Replica B runs while A owns the firing row: must NOT complete (delete)
	// the queue item, and must NOT call Core.Fire.
	resB := disp.HandleFireRetryItem(ctx, item, nil)
	assert.False(t, resB.Done, "handler seeing firing row must keep queue item alive")
	assert.NoError(t, resB.Err)
	assert.Equal(t, now.Add(trigger.FireLeaseHorizon), resB.RetryAt)
	assert.Equal(t, int32(1), attempt.Load())

	// Replica A finishes its attempt and returns the row to waiting.
	close(release)
	resA := <-resACh
	assert.False(t, resA.Done)
	assert.Equal(t, store.TriggerFireStatusWaiting, ms.byFireID[fire.ID].Status)

	// Later retry (item survived!) delivers once target is running and finishes.
	core.mu.Lock()
	core.targetState = trigger.RetryTargetDeliverable
	core.mu.Unlock()
	resC := disp.HandleFireRetryItem(ctx, item, nil)
	assert.True(t, resC.Done)
	assert.NoError(t, resC.Err)
	assert.Equal(t, int32(1), deliveries.Load())
	assert.Equal(t, store.TriggerFireStatusFired, ms.byFireID[fire.ID].Status)
}

func TestDispatcher_ParkWaitingFallbackEmitsAuditRecord(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	ms := newMemTriggerStore()
	auditSink := &captureFireAuditSink{}
	targetID := uuid.NewString()

	tr := store.Trigger{
		ID:            uuid.NewString(),
		ProjectID:     uuid.NewString(),
		Name:          "fallback-audit",
		SourceKind:    store.TriggerSourceKindWebhook,
		SourceID:      uuid.NewString(),
		MatchTypes:    []string{"webhook.received"},
		ActionType:    store.TriggerActionMessage,
		ActionPayload: `{"agentId":"` + targetID + `","message":"hello","offline":"queue"}`,
		Status:        store.TriggerStatusActive,
		CreatedAt:     now,
	}
	ms.triggers = append(ms.triggers, &tr)
	ev := store.TriggerEvent{
		ID:         uuid.NewString(),
		ProjectID:  tr.ProjectID,
		SourceKind: tr.SourceKind,
		SourceID:   tr.SourceID,
		Type:       "webhook.received",
		Subject:    "webhook:1",
		ReceivedAt: now,
		Status:     store.TriggerEventStatusPending,
	}
	ms.events[ev.ID] = &ev

	core := &fakeRetryCore{
		fireFn: func(_ context.Context, _ hubfire.FireRequest) hubfire.FireOutcome {
			return hubfire.FireOutcome{
				Status:        hubfire.FireStatusWaiting,
				Error:         trigger.ErrFireTargetNotRunning,
				TargetAgentID: targetID,
			}
		},
	}
	// Queue is intentionally nil to exercise the defensive fallback path.
	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:      core,
		Store:     ms,
		FireAudit: auditSink,
		Now:       func() time.Time { return now },
	})
	require.NoError(t, err)

	payload, _ := json.Marshal(trigger.EventQueuePayload{Version: trigger.EventQueuePayloadVersion, EventID: ev.ID})
	res := disp.HandleItem(ctx, durablequeue.Item{
		Kind:      trigger.EventQueueKind,
		DedupKey:  ev.ID,
		ProjectID: tr.ProjectID,
		Payload:   payload,
		Attempts:  1,
	}, nil)
	assert.True(t, res.Done)

	require.Len(t, auditSink.records, 1)
	assert.Equal(t, store.TriggerFireStatusSkipped, auditSink.records[0].Status)
	assert.Equal(t, trigger.FireSkipReasonTargetNotRunning, auditSink.records[0].Reason)
}
