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
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/durablequeue"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

type recordingFireAuditSink struct {
	mu      sync.Mutex
	records []trigger.FireAuditRecord
}

func (s *recordingFireAuditSink) RecordFire(_ context.Context, rec trigger.FireAuditRecord) {
	s.mu.Lock()
	s.records = append(s.records, rec)
	s.mu.Unlock()
}

func (s *recordingFireAuditSink) snapshot() []trigger.FireAuditRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]trigger.FireAuditRecord, len(s.records))
	copy(out, s.records)
	return out
}

func TestDispatcher_MultiAction_FailureIsolationOrderingAndCounters(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

	ms := newMemTriggerStore()
	projID := uuid.NewString()
	epID := uuid.NewString()
	trigID := uuid.NewString()
	evID := uuid.NewString()

	tr := &store.Trigger{
		ID:         trigID,
		ProjectID:  projID,
		Name:       "multi-action-isolation",
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		MatchTypes: []string{"webhook.received"},
		Actions: []store.TriggerAction{
			{Type: store.TriggerActionDispatchAgent, Payload: `{"template":"dev","task":"step 0"}`},
			{Type: store.TriggerActionMessage, Payload: `{"agentId":"agent-1","message":"step 1"}`},
			{Type: store.TriggerActionMessage, Payload: `{"agentId":"agent-2","message":"step 2"}`},
		},
		SubjectMode: store.TriggerSubjectModeNew,
		Status:      store.TriggerStatusActive,
		CreatedAt:   now,
	}
	ms.triggers = append(ms.triggers, tr)
	ms.events[evID] = &store.TriggerEvent{
		ID:         evID,
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		Type:       "webhook.received",
		Subject:    "pr-42",
		OccurredAt: now,
		ReceivedAt: now,
		Status:     store.TriggerEventStatusPending,
	}

	var (
		orderMu    sync.Mutex
		firedOrder []int
		afterOrder []int
	)
	fc := &fakeCore{
		fireFn: func(_ context.Context, req hubfire.FireRequest) hubfire.FireOutcome {
			idx := req.Event.Trigger.ActionIndex
			orderMu.Lock()
			firedOrder = append(firedOrder, idx)
			orderMu.Unlock()
			after := func(_ context.Context) {
				orderMu.Lock()
				afterOrder = append(afterOrder, idx)
				orderMu.Unlock()
			}
			switch idx {
			case 0:
				return hubfire.FireOutcome{
					Status:      hubfire.FireStatusRefused,
					Error:       "dispatch_agent refused",
					AfterRecord: after,
				}
			case 1:
				return hubfire.FireOutcome{
					Status:      hubfire.FireStatusFired,
					ResultRef:   "msg-1",
					AfterRecord: after,
				}
			default:
				return hubfire.FireOutcome{
					Status:      hubfire.FireStatusFailed,
					Error:       "fire failed",
					AfterRecord: after,
				}
			}
		},
	}
	fc.enabled.Store(true)

	auditSink := &recordingFireAuditSink{}
	d, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:      fc,
		Store:     ms,
		FireAudit: auditSink,
		Now:       func() time.Time { return now },
	})
	require.NoError(t, err)

	payload, err := json.Marshal(trigger.EventQueuePayload{
		Version: trigger.EventQueuePayloadVersion,
		EventID: evID,
	})
	require.NoError(t, err)

	res := d.HandleItem(ctx, durablequeue.Item{
		Seq:       1,
		Kind:      trigger.EventQueueKind,
		DedupKey:  evID,
		ProjectID: projID,
		Payload:   payload,
		Attempts:  1,
	}, nopLease{})
	require.True(t, res.Done)
	require.NoError(t, res.Err)

	// Strictly executed in author order 0, 1, 2 despite refusal on 0 and failure on 2.
	assert.Equal(t, []int{0, 1, 2}, firedOrder)
	assert.Equal(t, []int{0, 1, 2}, afterOrder)

	// Three distinct fire rows with action_index 0, 1, 2 and independent outcomes.
	fires, err := ms.ListTriggerFires(ctx, store.TriggerFireFilter{EventID: evID, TriggerID: trigID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, fires.Items, 3)

	byIdx := map[int]store.TriggerFire{}
	for _, f := range fires.Items {
		byIdx[f.ActionIndex] = f
	}
	assert.Equal(t, store.TriggerFireStatusRefused, byIdx[0].Status)
	assert.Equal(t, store.TriggerActionDispatchAgent, byIdx[0].ActionType)
	assert.Equal(t, store.TriggerFireStatusFired, byIdx[1].Status)
	assert.Equal(t, "msg-1", byIdx[1].ResultRef)
	assert.Equal(t, store.TriggerActionMessage, byIdx[1].ActionType)
	assert.Equal(t, store.TriggerFireStatusFailed, byIdx[2].Status)
	assert.Equal(t, store.TriggerActionMessage, byIdx[2].ActionType)

	// Trigger counters aggregate across actions: 1 event = 1 fire_count and 1 error_count.
	updatedTr, err := ms.GetTrigger(ctx, trigID)
	require.NoError(t, err)
	assert.Equal(t, 1, updatedTr.FireCount, "trigger fire_count must aggregate once per matched event")
	assert.Equal(t, 1, updatedTr.ErrorCount, "event with refused+fired+failed actions must count one error")

	// Audit records carry action_index 0, 1, 2.
	auditRecs := auditSink.snapshot()
	require.Len(t, auditRecs, 3)
	assert.Equal(t, 0, auditRecs[0].ActionIndex)
	assert.Equal(t, 1, auditRecs[1].ActionIndex)
	assert.Equal(t, 2, auditRecs[2].ActionIndex)
}

func TestDispatcher_MultiAction_ReuseDispatchDoesNotBlockMessageSibling(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

	ms := newMemTriggerStore()
	projID := uuid.NewString()
	epID := uuid.NewString()
	trigID := uuid.NewString()
	evID := uuid.NewString()
	liveAgentID := uuid.NewString()

	// Pre-populate an active bound claim for action_index=0 on subject "pr-77".
	claimStore := &stubClaimStore{active: map[string]*store.TriggerSubjectClaim{}}
	agents := &fakeAgentLookup{agents: map[string]*store.Agent{
		liveAgentID: {ID: liveAgentID, Phase: "running"},
	}}
	claims := trigger.NewSubjectClaims(claimStore, agents, func() time.Time { return now })
	c, ex, err := claims.Claim(ctx, trigID, 0, "pr-77", uuid.NewString(), uuid.NewString())
	require.NoError(t, err)
	require.Nil(t, ex)
	require.NoError(t, claims.Bind(ctx, c, liveAgentID))

	tr := &store.Trigger{
		ID:         trigID,
		ProjectID:  projID,
		Name:       "reuse-plus-message",
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		MatchTypes: []string{"webhook.received"},
		Actions: []store.TriggerAction{
			{Type: store.TriggerActionDispatchAgent, Payload: `{"template":"dev","task":"work","reuseMessage":"reused"}`},
			{Type: store.TriggerActionMessage, Payload: `{"agentId":"observer-agent","message":"sibling notification"}`},
		},
		SubjectMode: store.TriggerSubjectModeReuse,
		Status:      store.TriggerStatusActive,
		CreatedAt:   now,
	}
	ms.triggers = append(ms.triggers, tr)
	ms.events[evID] = &store.TriggerEvent{
		ID:         evID,
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		Type:       "webhook.received",
		Subject:    "pr-77",
		OccurredAt: now,
		ReceivedAt: now,
		Status:     store.TriggerEventStatusPending,
	}

	var calls []hubfire.FireRequest
	fc := &fakeCore{
		fireFn: func(_ context.Context, req hubfire.FireRequest) hubfire.FireOutcome {
			calls = append(calls, req)
			return hubfire.FireOutcome{Status: hubfire.FireStatusFired, ResultRef: "ok"}
		},
	}
	fc.enabled.Store(true)

	d, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:          fc,
		Store:         ms,
		SubjectClaims: claims,
		Now:           func() time.Time { return now },
	})
	require.NoError(t, err)

	payload, err := json.Marshal(trigger.EventQueuePayload{
		Version: trigger.EventQueuePayloadVersion,
		EventID: evID,
	})
	require.NoError(t, err)

	res := d.HandleItem(ctx, durablequeue.Item{
		Seq:       1,
		Kind:      trigger.EventQueueKind,
		DedupKey:  evID,
		ProjectID: projID,
		Payload:   payload,
		Attempts:  1,
	}, nopLease{})
	require.True(t, res.Done)
	require.NoError(t, res.Err)

	// Both action 0 (reuse message to liveAgentID) and action 1 (sibling message to observer-agent) fired!
	require.Len(t, calls, 2)
	assert.Equal(t, 0, calls[0].Event.Trigger.ActionIndex)
	assert.Equal(t, store.TriggerActionMessage, calls[0].ActionType)
	assert.Contains(t, calls[0].Payload, liveAgentID)

	assert.Equal(t, 1, calls[1].Event.Trigger.ActionIndex)
	assert.Equal(t, store.TriggerActionMessage, calls[1].ActionType)
	assert.Contains(t, calls[1].Payload, "observer-agent")
}

func TestDispatcher_LegacyScalarRowUnchanged(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

	ms := newMemTriggerStore()
	projID := uuid.NewString()
	epID := uuid.NewString()
	trigID := uuid.NewString()
	evID := uuid.NewString()

	// Legacy row with ActionType/ActionPayload set and Actions empty.
	ms.triggers = append(ms.triggers, &store.Trigger{
		ID:            trigID,
		ProjectID:     projID,
		Name:          "legacy-scalar",
		SourceKind:    store.TriggerSourceKindWebhook,
		SourceID:      epID,
		MatchTypes:    []string{"webhook.received"},
		ActionType:    store.TriggerActionMessage,
		ActionPayload: `{"agentId":"legacy-agent","message":"hello"}`,
		SubjectMode:   store.TriggerSubjectModeNew,
		Status:        store.TriggerStatusActive,
		CreatedAt:     now,
	})
	ms.events[evID] = &store.TriggerEvent{
		ID:         evID,
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		Type:       "webhook.received",
		Subject:    "sub-1",
		OccurredAt: now,
		ReceivedAt: now,
		Status:     store.TriggerEventStatusPending,
	}

	var fired []hubfire.FireRequest
	fc := &fakeCore{
		fireFn: func(_ context.Context, req hubfire.FireRequest) hubfire.FireOutcome {
			fired = append(fired, req)
			return hubfire.FireOutcome{Status: hubfire.FireStatusFired, ResultRef: "msg-legacy"}
		},
	}
	fc.enabled.Store(true)

	d, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:  fc,
		Store: ms,
		Now:   func() time.Time { return now },
	})
	require.NoError(t, err)

	payload, err := json.Marshal(trigger.EventQueuePayload{
		Version: trigger.EventQueuePayloadVersion,
		EventID: evID,
	})
	require.NoError(t, err)

	res := d.HandleItem(ctx, durablequeue.Item{
		Seq:       1,
		Kind:      trigger.EventQueueKind,
		DedupKey:  evID,
		ProjectID: projID,
		Payload:   payload,
		Attempts:  1,
	}, nopLease{})
	require.True(t, res.Done)
	require.NoError(t, res.Err)

	require.Len(t, fired, 1)
	assert.Equal(t, 0, fired[0].Event.Trigger.ActionIndex)
	assert.Equal(t, store.TriggerActionMessage, fired[0].ActionType)
	assert.Contains(t, fired[0].Payload, "legacy-agent")
}

// TestDispatcher_MultiAction_RetryAfterEarlyReturnDoesNotDoubleCount verifies O1:
// if attempt 1 finishes action 0, flushes counters on an early return (e.g.
// lease lost before action 1), and attempt 2 retries and finishes action 1, the
// matched event must be counted once in trigger fire_count (never twice).
func TestDispatcher_MultiAction_RetryAfterEarlyReturnDoesNotDoubleCount(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)

	ms := newMemTriggerStore()
	projID := uuid.NewString()
	epID := uuid.NewString()
	trigID := uuid.NewString()
	evID := uuid.NewString()

	ms.triggers = append(ms.triggers, &store.Trigger{
		ID:         trigID,
		ProjectID:  projID,
		Name:       "o1-retry-counter",
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		MatchTypes: []string{"webhook.received"},
		Actions: []store.TriggerAction{
			{Type: store.TriggerActionMessage, Payload: `{"agentId":"agent-1","message":"step 0"}`},
			{Type: store.TriggerActionMessage, Payload: `{"agentId":"agent-2","message":"step 1"}`},
		},
		SubjectMode: store.TriggerSubjectModeNew,
		Status:      store.TriggerStatusActive,
		CreatedAt:   now,
	})
	ms.events[evID] = &store.TriggerEvent{
		ID:         evID,
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		Type:       "webhook.received",
		Subject:    "sub-1",
		OccurredAt: now,
		ReceivedAt: now,
		Status:     store.TriggerEventStatusPending,
	}

	var firedIndices []int
	fc := &fakeCore{
		fireFn: func(_ context.Context, req hubfire.FireRequest) hubfire.FireOutcome {
			firedIndices = append(firedIndices, req.Event.Trigger.ActionIndex)
			return hubfire.FireOutcome{Status: hubfire.FireStatusFired, ResultRef: "ok"}
		},
	}
	fc.enabled.Store(true)

	d, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:  fc,
		Store: ms,
		Now:   func() time.Time { return now },
	})
	require.NoError(t, err)

	payload, err := json.Marshal(trigger.EventQueuePayload{
		Version: trigger.EventQueuePayloadVersion,
		EventID: evID,
	})
	require.NoError(t, err)

	// Attempt 1: lease holds for action 0, then drops before action 1 -> early return + flushTrigCounters.
	leaseCalls := 0
	dropLease := &funcLease{renewFn: func(context.Context) (bool, error) {
		leaseCalls++
		return leaseCalls == 1, nil
	}}
	res1 := d.HandleItem(ctx, durablequeue.Item{
		Seq:       1,
		Kind:      trigger.EventQueueKind,
		DedupKey:  evID,
		ProjectID: projID,
		Payload:   payload,
		Attempts:  1,
	}, dropLease)
	require.False(t, res1.Done)
	require.Error(t, res1.Err)
	assert.Equal(t, []int{0}, firedIndices)

	trAfter1, err := ms.GetTrigger(ctx, trigID)
	require.NoError(t, err)
	assert.Equal(t, 1, trAfter1.FireCount)

	// Attempt 2: lease holds -> action 0 is already claimed/fired, action 1 runs.
	// FireCount must stay 1 (no double count).
	res2 := d.HandleItem(ctx, durablequeue.Item{
		Seq:       1,
		Kind:      trigger.EventQueueKind,
		DedupKey:  evID,
		ProjectID: projID,
		Payload:   payload,
		Attempts:  2,
	}, nopLease{})
	require.True(t, res2.Done)
	require.NoError(t, res2.Err)
	assert.Equal(t, []int{0, 1}, firedIndices)

	trAfter2, err := ms.GetTrigger(ctx, trigID)
	require.NoError(t, err)
	assert.Equal(t, 1, trAfter2.FireCount, "retry after early-return flush must not double-count the matched event")
}

type funcLease struct {
	renewFn func(context.Context) (bool, error)
}

func (l *funcLease) Renew(ctx context.Context) (bool, error) { return l.renewFn(ctx) }

// TestFireRetry_ActionIndexGreaterThanZeroAndTypeSwapSkip verifies O2:
// offline-queue retry of action_index=1 executes actions[1] (not actions[0]),
// and skips "trigger changed" when an edit changes the action type at index 1.
func TestFireRetry_ActionIndexGreaterThanZeroAndTypeSwapSkip(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)

	ms := newMemTriggerStore()
	projID := uuid.NewString()
	epID := uuid.NewString()
	trigID := uuid.NewString()
	evID := uuid.NewString()
	agentID := uuid.NewString()

	tr := &store.Trigger{
		ID:         trigID,
		ProjectID:  projID,
		Name:       "o2-retry-idx1",
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		MatchTypes: []string{"webhook.received"},
		Actions: []store.TriggerAction{
			{Type: store.TriggerActionDispatchAgent, Payload: `{"template":"dev","task":"step 0"}`},
			{Type: store.TriggerActionMessage, Payload: `{"agentId":"` + agentID + `","message":"queued sibling","offline":"queue"}`},
		},
		SubjectMode: store.TriggerSubjectModeNew,
		Status:      store.TriggerStatusActive,
		InitiatorAttribution: store.InitiatorAttribution{
			AuthorizationRevision: 3,
		},
		CreatedAt: now,
	}
	ms.triggers = append(ms.triggers, tr)
	ms.events[evID] = &store.TriggerEvent{
		ID:         evID,
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		Type:       "webhook.received",
		Subject:    "sub-retry",
		OccurredAt: now,
		ReceivedAt: now,
		Status:     store.TriggerEventStatusProcessed,
	}

	fireID := uuid.NewString()
	ms.fires[evID+"|"+trigID+"|1"] = &store.TriggerFire{
		ID:            fireID,
		ProjectID:     projID,
		TriggerID:     trigID,
		EventID:       evID,
		Subject:       "sub-retry",
		ActionType:    store.TriggerActionMessage,
		ActionIndex:   1,
		Status:        store.TriggerFireStatusWaiting,
		TargetAgentID: agentID,
		LeaseUntil:    now.Add(time.Hour),
		StartedAt:     now,
		CreatedAt:     now,
		InitiatorAttribution: store.InitiatorAttribution{
			AuthorizationRevision: 3,
		},
	}
	ms.byFireID[fireID] = ms.fires[evID+"|"+trigID+"|1"]

	var gotReq *hubfire.FireRequest
	rc := &fakeRetryCore{
		targetState: trigger.RetryTargetDeliverable,
		fireFn: func(_ context.Context, req hubfire.FireRequest) hubfire.FireOutcome {
			cp := req
			gotReq = &cp
			return hubfire.FireOutcome{Status: hubfire.FireStatusFired, ResultRef: "msg-retried"}
		},
	}
	q := &fakeRetryQueue{}
	d, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:  rc,
		Store: ms,
		Queue: q,
		Now:   func() time.Time { return now.Add(time.Minute) },
	})
	require.NoError(t, err)

	retryPayload, err := json.Marshal(trigger.FireRetryQueuePayload{
		Version: trigger.FireRetryQueuePayloadVersion,
		FireID:  fireID,
	})
	require.NoError(t, err)

	res := d.HandleFireRetryItem(ctx, durablequeue.Item{
		Seq:       1,
		Kind:      trigger.FireRetryQueueKind,
		DedupKey:  fireID,
		ProjectID: projID,
		Payload:   retryPayload,
		Attempts:  1,
	}, nopLease{})
	require.True(t, res.Done)
	require.NoError(t, res.Err)
	require.NotNil(t, gotReq, "retry must execute action[1]")
	assert.Equal(t, 1, gotReq.Event.Trigger.ActionIndex)
	assert.Equal(t, store.TriggerActionMessage, gotReq.ActionType)
	assert.Contains(t, gotReq.Payload, "queued sibling")

	// Now test index-changes-meaning: a second waiting fire at index 1 when the
	// trigger's actions[1] was swapped to dispatch_agent (even at the same revision
	// or bumped revision) must be skipped "trigger changed".
	fireID2 := uuid.NewString()
	ms.fires[evID+"|"+trigID+"|1b"] = &store.TriggerFire{
		ID:            fireID2,
		ProjectID:     projID,
		TriggerID:     trigID,
		EventID:       evID,
		Subject:       "sub-retry-2",
		ActionType:    store.TriggerActionMessage,
		ActionIndex:   1,
		Status:        store.TriggerFireStatusWaiting,
		TargetAgentID: agentID,
		LeaseUntil:    now.Add(time.Hour),
		StartedAt:     now,
		CreatedAt:     now,
		InitiatorAttribution: store.InitiatorAttribution{
			AuthorizationRevision: 3,
		},
	}
	ms.byFireID[fireID2] = ms.fires[evID+"|"+trigID+"|1b"]
	tr.Actions[1] = store.TriggerAction{Type: store.TriggerActionDispatchAgent, Payload: `{"template":"dev","task":"swapped"}`}

	retryPayload2, err := json.Marshal(trigger.FireRetryQueuePayload{
		Version: trigger.FireRetryQueuePayloadVersion,
		FireID:  fireID2,
	})
	require.NoError(t, err)

	prevCalls := rc.fireCalls.Load()
	res2 := d.HandleFireRetryItem(ctx, durablequeue.Item{
		Seq:       2,
		Kind:      trigger.FireRetryQueueKind,
		DedupKey:  fireID2,
		ProjectID: projID,
		Payload:   retryPayload2,
		Attempts:  1,
	}, nopLease{})
	require.True(t, res2.Done)
	require.NoError(t, res2.Err)
	assert.Equal(t, prevCalls, rc.fireCalls.Load(), "swapped action type at index 1 must not fire")
	assert.Equal(t, store.TriggerFireStatusSkipped, ms.byFireID[fireID2].Status)
	assert.Equal(t, "trigger changed", ms.byFireID[fireID2].Error)
}
