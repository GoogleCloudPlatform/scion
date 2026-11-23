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
