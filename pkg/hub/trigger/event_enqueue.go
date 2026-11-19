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

// Package trigger implements Scion's event-driven trigger ingress, durable
// queue producer/drainer, and event dispatcher without importing package hub
// (Stage 1 D-S1-1(c), ptone/scion#7961).
package trigger

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/durablequeue"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// EventQueueKind is the durablequeue kind for inbound trigger events (§6.2).
const EventQueueKind = "trigger.event"

// EventQueuePayloadVersion is the wire schema version of trigger.event queue
// item payloads (§6.2).
const EventQueuePayloadVersion = 1

// EventQueuePayload is the JSON payload stored in durable_queue_items for
// EventQueueKind. Consumers reload the authoritative trigger_events row by
// EventID and trust no other field from the queue payload (§6.2, §6.3).
type EventQueuePayload struct {
	Version int    `json:"v"`
	EventID string `json:"event_id"`
}

// EnqueueEvent creates the trigger_events row (with status forced to "pending")
// and enqueues its corresponding durablequeue item
// ({Kind: EventQueueKind, DedupKey: ev.ID, ProjectID: ev.ProjectID,
// Payload: {"v":1,"event_id":ev.ID}}) inside the caller's transaction (§6.2).
//
// Inside Store.WithTx(ctx, func(tx store.Store) error { ... }), callers pass tx
// for both tx and ts so the trigger_events row and durable_queue_items row
// commit or roll back together; after WithTx commits, callers call
// q.Wake(EventQueueKind).
//
// Duplicate errors are passed through without swallowing:
//   - a unique violation on trigger_events (such as (source_kind, source_id,
//     dedup_key)) returns store.ErrAlreadyExists from ts.CreateTriggerEvent;
//   - a duplicate (kind, dedup_key) in durable_queue_items returns
//     durablequeue.ErrDuplicate (which wraps store.ErrAlreadyExists) from
//     q.Enqueue.
func EnqueueEvent(
	ctx context.Context,
	tx store.Tx,
	ts store.TriggerStore,
	q durablequeue.Queue,
	ev *store.TriggerEvent,
) (int64, error) {
	if ts == nil {
		return 0, fmt.Errorf("%w: trigger store is nil", store.ErrInvalidInput)
	}
	if q == nil {
		return 0, fmt.Errorf("%w: durable queue is nil", store.ErrInvalidInput)
	}
	if ev == nil || ev.ID == "" || ev.ProjectID == "" {
		return 0, fmt.Errorf("%w: trigger event id and project_id are required", store.ErrInvalidInput)
	}

	row := *ev
	row.Status = store.TriggerEventStatusPending
	if err := ts.CreateTriggerEvent(ctx, &row); err != nil {
		return 0, err
	}

	payload, err := json.Marshal(EventQueuePayload{
		Version: EventQueuePayloadVersion,
		EventID: row.ID,
	})
	if err != nil {
		return 0, fmt.Errorf("marshal trigger event queue payload: %w", err)
	}

	seq, err := q.Enqueue(ctx, tx, durablequeue.Item{
		Kind:      EventQueueKind,
		DedupKey:  row.ID,
		ProjectID: row.ProjectID,
		Payload:   payload,
	})
	if err != nil {
		return 0, err
	}
	*ev = row
	return seq, nil
}
