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
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/durablequeue"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

type fakeEnqueueTriggerStore struct {
	store.TriggerStore
	created   []*store.TriggerEvent
	createErr error
}

func (f *fakeEnqueueTriggerStore) CreateTriggerEvent(_ context.Context, ev *store.TriggerEvent) error {
	if f.createErr != nil {
		return f.createErr
	}
	cp := *ev
	f.created = append(f.created, &cp)
	return nil
}

type fakeEnqueueQueue struct {
	enqueued []enqueueCall
	seq      int64
	err      error
	wakes    []string
}

type enqueueCall struct {
	tx   store.Tx
	item durablequeue.Item
}

func (f *fakeEnqueueQueue) Enqueue(_ context.Context, tx store.Tx, item durablequeue.Item) (int64, error) {
	f.enqueued = append(f.enqueued, enqueueCall{tx: tx, item: item})
	if f.err != nil {
		return 0, f.err
	}
	f.seq++
	return f.seq, nil
}

func (f *fakeEnqueueQueue) Wake(kind string) {
	f.wakes = append(f.wakes, kind)
}

func sampleEvent() *store.TriggerEvent {
	return &store.TriggerEvent{
		ID:         uuid.NewString(),
		ProjectID:  uuid.NewString(),
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   uuid.NewString(),
		Type:       "webhook.received",
		Subject:    "webhook:test",
		DedupKey:   "deliv-1",
		Payload:    `{"ok":true}`,
	}
}

func TestEnqueueEvent_ItemShapeAndPendingStatus(t *testing.T) {
	ctx := context.Background()
	ts := &fakeEnqueueTriggerStore{}
	q := &fakeEnqueueQueue{}
	type txToken struct{ id string }
	tx := txToken{id: "caller-tx"}

	ev := sampleEvent()
	ev.Status = store.TriggerEventStatusProcessed // EnqueueEvent must force pending

	seq, err := trigger.EnqueueEvent(ctx, tx, ts, q, ev)
	require.NoError(t, err)
	assert.Equal(t, int64(1), seq)
	assert.Equal(t, store.TriggerEventStatusPending, ev.Status)

	require.Len(t, ts.created, 1)
	assert.Equal(t, store.TriggerEventStatusPending, ts.created[0].Status)
	assert.Equal(t, ev.ID, ts.created[0].ID)

	require.Len(t, q.enqueued, 1)
	call := q.enqueued[0]
	assert.Equal(t, tx, call.tx)
	assert.Equal(t, trigger.EventQueueKind, call.item.Kind)
	assert.Equal(t, "trigger.event", call.item.Kind)
	assert.Equal(t, ev.ID, call.item.DedupKey)
	assert.Equal(t, ev.ProjectID, call.item.ProjectID)
	assert.True(t, call.item.NotBefore.IsZero())
	assert.True(t, call.item.CreatedAt.IsZero())

	// Exact wire JSON shape per §6.2: {"v":1,"event_id":"<id>"} with no extra keys.
	assert.JSONEq(t, `{"v":1,"event_id":"`+ev.ID+`"}`, string(call.item.Payload))
	var raw map[string]any
	require.NoError(t, json.Unmarshal(call.item.Payload, &raw))
	assert.Len(t, raw, 2)

	var decoded trigger.EventQueuePayload
	require.NoError(t, json.Unmarshal(call.item.Payload, &decoded))
	assert.Equal(t, trigger.EventQueuePayloadVersion, decoded.Version)
	assert.Equal(t, ev.ID, decoded.EventID)

	// EnqueueEvent never calls Wake itself (the caller wakes after tx commit).
	assert.Empty(t, q.wakes)
}

func TestEnqueueEvent_PassesThroughQueueErrDuplicate(t *testing.T) {
	ctx := context.Background()
	ts := &fakeEnqueueTriggerStore{}
	q := &fakeEnqueueQueue{err: durablequeue.ErrDuplicate}
	ev := sampleEvent()
	ev.Status = ""

	seq, err := trigger.EnqueueEvent(ctx, nil, ts, q, ev)
	require.Error(t, err)
	assert.Zero(t, seq)
	assert.ErrorIs(t, err, durablequeue.ErrDuplicate)
	assert.ErrorIs(t, err, store.ErrAlreadyExists)
	// Caller's struct is untouched when enqueue fails.
	assert.Empty(t, ev.Status)
}

func TestEnqueueEvent_PassesThroughEventStoreConflictWithoutEnqueuing(t *testing.T) {
	ctx := context.Background()
	ts := &fakeEnqueueTriggerStore{createErr: store.ErrAlreadyExists}
	q := &fakeEnqueueQueue{}
	ev := sampleEvent()

	seq, err := trigger.EnqueueEvent(ctx, nil, ts, q, ev)
	require.Error(t, err)
	assert.Zero(t, seq)
	assert.ErrorIs(t, err, store.ErrAlreadyExists)
	assert.Empty(t, q.enqueued, "queue must not be touched if trigger_events insert fails")
}

func TestEnqueueEvent_ValidationErrors(t *testing.T) {
	ctx := context.Background()
	ts := &fakeEnqueueTriggerStore{}
	q := &fakeEnqueueQueue{}

	t.Run("nil store", func(t *testing.T) {
		_, err := trigger.EnqueueEvent(ctx, nil, nil, q, sampleEvent())
		require.ErrorIs(t, err, store.ErrInvalidInput)
	})
	t.Run("nil queue", func(t *testing.T) {
		_, err := trigger.EnqueueEvent(ctx, nil, ts, nil, sampleEvent())
		require.ErrorIs(t, err, store.ErrInvalidInput)
	})
	t.Run("nil event", func(t *testing.T) {
		_, err := trigger.EnqueueEvent(ctx, nil, ts, q, nil)
		require.ErrorIs(t, err, store.ErrInvalidInput)
	})
	t.Run("empty event id refuses before queue could mint random dedup key", func(t *testing.T) {
		ev := sampleEvent()
		ev.ID = ""
		_, err := trigger.EnqueueEvent(ctx, nil, ts, q, ev)
		require.ErrorIs(t, err, store.ErrInvalidInput)
		assert.Empty(t, q.enqueued)
	})
	t.Run("empty project id", func(t *testing.T) {
		ev := sampleEvent()
		ev.ProjectID = ""
		_, err := trigger.EnqueueEvent(ctx, nil, ts, q, ev)
		require.ErrorIs(t, err, store.ErrInvalidInput)
	})
	t.Run("store error propagated", func(t *testing.T) {
		sentinel := errors.New("db down")
		tsErr := &fakeEnqueueTriggerStore{createErr: sentinel}
		_, err := trigger.EnqueueEvent(ctx, nil, tsErr, q, sampleEvent())
		require.ErrorIs(t, err, sentinel)
	})
}
