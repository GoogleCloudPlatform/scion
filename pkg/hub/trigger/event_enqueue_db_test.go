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
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/durablequeue"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

func TestEnqueueEvent_WithTxCommitRollbackAndDuplicates(t *testing.T) {
	ctx := context.Background()
	cs := entadapter.NewCompositeStore(enttest.NewClient(t))
	t0 := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	q := durablequeue.NewQueue(cs, durablequeue.WithQueueClock(func() time.Time { return t0 }))

	projID := uuid.NewString()
	require.NoError(t, cs.CreateProject(ctx, &store.Project{
		ID:   projID,
		Name: "enqueue-proj",
		Slug: "enqueue-" + projID[:8],
	}))
	epID := uuid.NewString()

	newEvent := func(dedupKey string) *store.TriggerEvent {
		return &store.TriggerEvent{
			ID:         uuid.NewString(),
			ProjectID:  projID,
			SourceKind: store.TriggerSourceKindWebhook,
			SourceID:   epID,
			Type:       "webhook.received",
			Subject:    "webhook:" + epID,
			DedupKey:   dedupKey,
			OccurredAt: t0,
			ReceivedAt: t0,
			Attributes: map[string]string{"event": "received"},
			Headers:    map[string]string{"Content-Type": "application/json"},
			Payload:    `{"hello":"world"}`,
		}
	}

	// 1. Caller rollback removes both trigger_events row and queue item.
	rbEv := newEvent("deliv-rollback")
	err := cs.WithTx(ctx, func(tx store.Store) error {
		if _, enqErr := trigger.EnqueueEvent(ctx, tx, tx, q, rbEv); enqErr != nil {
			return enqErr
		}
		return errors.New("caller aborts tx")
	})
	require.Error(t, err)
	_, err = cs.GetTriggerEvent(ctx, rbEv.ID)
	require.ErrorIs(t, err, store.ErrNotFound)
	stats, err := cs.DurableQueueStats(ctx, trigger.EventQueueKind, projID, t0)
	require.NoError(t, err)
	assert.Zero(t, stats.Pending)

	// 2. Commit persists pending event and matching trigger.event queue item.
	okEv := newEvent("deliv-commit")
	var seq int64
	require.NoError(t, cs.WithTx(ctx, func(tx store.Store) error {
		var enqErr error
		seq, enqErr = trigger.EnqueueEvent(ctx, tx, tx, q, okEv)
		return enqErr
	}))
	assert.Positive(t, seq)

	stored, err := cs.GetTriggerEvent(ctx, okEv.ID)
	require.NoError(t, err)
	assert.Equal(t, store.TriggerEventStatusPending, stored.Status)
	assert.Equal(t, `{"hello":"world"}`, stored.Payload)

	claimed, err := cs.ClaimDurableItems(ctx, trigger.EventQueueKind, "test-owner", t0, t0.Add(time.Minute), 10)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	assert.Equal(t, seq, claimed[0].Seq)
	assert.Equal(t, okEv.ID, claimed[0].DedupKey)
	assert.Equal(t, projID, claimed[0].ProjectID)
	var qp trigger.EventQueuePayload
	require.NoError(t, json.Unmarshal(claimed[0].Payload, &qp))
	assert.Equal(t, trigger.EventQueuePayload{Version: 1, EventID: okEv.ID}, qp)

	// 3. Duplicate (source_kind, source_id, dedup_key) rolls back without adding a queue item.
	dupDeliveryEv := newEvent("deliv-commit")
	err = cs.WithTx(ctx, func(tx store.Store) error {
		_, enqErr := trigger.EnqueueEvent(ctx, tx, tx, q, dupDeliveryEv)
		return enqErr
	})
	require.ErrorIs(t, err, store.ErrAlreadyExists)
	_, err = cs.GetTriggerEvent(ctx, dupDeliveryEv.ID)
	require.ErrorIs(t, err, store.ErrNotFound)
	stats, err = cs.DurableQueueStats(ctx, trigger.EventQueueKind, projID, t0)
	require.NoError(t, err)
	assert.Equal(t, int64(1), stats.Pending)

	// 4. Pre-existing queue item with the same (kind, dedup_key) surfaces
	// durablequeue.ErrDuplicate and rolls back the newly inserted trigger_events row.
	preQueuedID := uuid.NewString()
	_, err = q.Enqueue(ctx, nil, durablequeue.Item{
		Kind:      trigger.EventQueueKind,
		DedupKey:  preQueuedID,
		ProjectID: projID,
		Payload:   []byte(`{"v":1,"event_id":"` + preQueuedID + `"}`),
	})
	require.NoError(t, err)

	collidingEv := newEvent("deliv-queue-dup")
	collidingEv.ID = preQueuedID
	err = cs.WithTx(ctx, func(tx store.Store) error {
		_, enqErr := trigger.EnqueueEvent(ctx, tx, tx, q, collidingEv)
		return enqErr
	})
	require.ErrorIs(t, err, durablequeue.ErrDuplicate)
	_, err = cs.GetTriggerEvent(ctx, collidingEv.ID)
	require.ErrorIs(t, err, store.ErrNotFound, "event row must roll back when queue Enqueue returns ErrDuplicate")
}
