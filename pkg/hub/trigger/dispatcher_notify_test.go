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

type notifyCaptureCall struct {
	triggerID   string
	fireID      string
	actionIndex int
	outcome     hubfire.FireOutcome
}

type notifyCaptureCore struct {
	*fakeCore
	mu    sync.Mutex
	calls []notifyCaptureCall
}

var _ trigger.FireOutcomeNotifier = (*notifyCaptureCore)(nil)

func (c *notifyCaptureCore) NotifyFireOutcome(_ context.Context, tr *store.Trigger, fire *store.TriggerFire, outcome hubfire.FireOutcome) {
	c.mu.Lock()
	defer c.mu.Unlock()
	tid := ""
	if tr != nil {
		tid = tr.ID
	} else if fire != nil {
		tid = fire.TriggerID
	}
	c.calls = append(c.calls, notifyCaptureCall{
		triggerID:   tid,
		fireID:      fire.ID,
		actionIndex: fire.ActionIndex,
		outcome:     outcome,
	})
}

func TestDispatcher_FireOutcomeNotifier_MultiActionAndExpired(t *testing.T) {
	ctx := context.Background()
	st := newMemTriggerStore()
	projID := uuid.NewString()
	epID := uuid.NewString()
	trID := uuid.NewString()
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)

	st.triggers = append(st.triggers, &store.Trigger{
		ID:         trID,
		ProjectID:  projID,
		Name:       "multi-notify",
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		MatchTypes: []string{"webhook.received"},
		Actions: []store.TriggerAction{
			{Type: store.TriggerActionMessage, Payload: `{"agentId":"a1","message":"ok"}`},
			{Type: store.TriggerActionMessage, Payload: `{"agentId":"a2","message":"boom"}`},
		},
		SubjectMode: store.TriggerSubjectModeNew,
		Status:      store.TriggerStatusActive,
	})

	evID := uuid.NewString()
	st.events[evID] = &store.TriggerEvent{
		ID:         evID,
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		Type:       "webhook.received",
		Subject:    "s1",
		Status:     store.TriggerEventStatusPending,
		OccurredAt: now,
		ReceivedAt: now,
	}

	fc := &fakeCore{}
	fc.enabled.Store(true)
	fc.fireFn = func(_ context.Context, req hubfire.FireRequest) hubfire.FireOutcome {
		if req.Event != nil && req.Event.Trigger != nil && req.Event.Trigger.ActionIndex == 1 {
			return hubfire.FireOutcome{Status: hubfire.FireStatusFailed, Error: "message failed"}
		}
		return hubfire.FireOutcome{Status: hubfire.FireStatusFired, ResultRef: "msg-1"}
	}
	nc := &notifyCaptureCore{fakeCore: fc}

	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:  nc,
		Store: st,
		Now:   func() time.Time { return now },
	})
	require.NoError(t, err)

	payload, err := json.Marshal(trigger.EventQueuePayload{Version: trigger.EventQueuePayloadVersion, EventID: evID})
	require.NoError(t, err)
	res := disp.HandleItem(ctx, durablequeue.Item{Payload: payload, Attempts: 1}, nil)
	require.True(t, res.Done)

	nc.mu.Lock()
	calls := append([]notifyCaptureCall(nil), nc.calls...)
	nc.mu.Unlock()
	require.Len(t, calls, 2)
	assert.Equal(t, 0, calls[0].actionIndex)
	assert.Equal(t, hubfire.FireStatusFired, calls[0].outcome.Status)
	assert.Equal(t, 1, calls[1].actionIndex)
	assert.Equal(t, hubfire.FireStatusFailed, calls[1].outcome.Status)
	assert.Equal(t, "message failed", calls[1].outcome.Error)

	// Expired waiting fire via HandleFireRetryItem also routes to FireOutcomeNotifier.
	waitFireID := uuid.NewString()
	waitUntil := now.Add(-time.Minute)
	wf := &store.TriggerFire{
		ID:          waitFireID,
		ProjectID:   projID,
		TriggerID:   trID,
		EventID:     evID,
		Subject:     "s1",
		ActionType:  store.TriggerActionMessage,
		ActionIndex: 0,
		Status:      store.TriggerFireStatusWaiting,
		WaitUntil:   &waitUntil,
		Attempts:    1,
	}
	st.fires[waitFireID] = wf
	st.byFireID[waitFireID] = wf
	retryPayload, err := json.Marshal(trigger.FireRetryQueuePayload{Version: trigger.FireRetryQueuePayloadVersion, FireID: waitFireID})
	require.NoError(t, err)
	retryRes := disp.HandleFireRetryItem(ctx, durablequeue.Item{Payload: retryPayload, Attempts: 1}, nil)
	require.True(t, retryRes.Done)

	nc.mu.Lock()
	calls = append([]notifyCaptureCall(nil), nc.calls...)
	nc.mu.Unlock()
	require.Len(t, calls, 3)
	assert.Equal(t, waitFireID, calls[2].fireID)
	assert.Equal(t, hubfire.FireStatusExpired, calls[2].outcome.Status)
	assert.Equal(t, trigger.ErrFireOfflineQueueExpired, calls[2].outcome.Error)
}
