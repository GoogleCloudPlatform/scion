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

package entadapter

import (
	"context"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/message"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// bindableDispatchStates are the states a /deliver bind accepts: the row
// passed the message route's authorization and is not yet delivered. pending
// is included because the agent-DM path flips a row to dispatched only after
// the broker call returns, and an interrupt is delivered inside that call
// (freeze gap G1 of .design/conduit/v2.22-deliver-message.md).
var bindableDispatchStates = []string{store.MessageDispatchDispatched, store.MessageDispatchPending}

// finalDeliveryStates are the states a same-id call replays.
var finalDeliveryStates = map[string]bool{
	store.MessageDispatchDelivered: true,
	store.MessageDispatchPartial:   true,
	store.MessageDispatchFailed:    true,
}

// parseDeliveryUUIDs parses ids strictly: any invalid id is an error.
func parseDeliveryUUIDs(ids []string) ([]uuid.UUID, error) {
	out := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		u, err := parseUUID(id)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, nil
}

// BindMessageDelivery implements store.BrokerDispatchStore.
func (s *BrokerDispatchStore) BindMessageDelivery(ctx context.Context, recipientID, deliveryID string, messageIDs []string, now time.Time) (store.DeliveryBind, error) {
	uids, err := parseDeliveryUUIDs(messageIDs)
	if err != nil || len(uids) == 0 {
		return store.DeliveryBind{}, store.ErrNotFound
	}
	rows, err := s.client.Message.Query().
		Where(message.IDIn(uids...)).
		Select(message.FieldRecipientID, message.FieldDispatchState, message.FieldDeliveryID, message.FieldDeliveryOutcome).
		All(ctx)
	if err != nil {
		return store.DeliveryBind{}, mapError(err)
	}
	if len(rows) != len(uids) {
		return store.DeliveryBind{}, store.ErrNotFound
	}
	for _, r := range rows {
		if r.RecipientID != recipientID {
			return store.DeliveryBind{}, store.ErrNotFound
		}
	}

	tx, err := s.client.Tx(ctx)
	if err != nil {
		return store.DeliveryBind{}, mapError(err)
	}
	defer func() { _ = tx.Rollback() }()
	n, err := tx.Message.Update().
		Where(
			message.IDIn(uids...),
			message.RecipientIDEQ(recipientID),
			message.DispatchStateIn(bindableDispatchStates...),
			message.DeliveryIDIsNil(),
		).
		SetDispatchState(store.MessageDispatchDelivering).
		SetDeliveryID(deliveryID).
		SetDeliveringAt(now).
		Save(ctx)
	if err != nil {
		return store.DeliveryBind{}, mapError(err)
	}
	if n == len(uids) {
		if err := tx.Commit(); err != nil {
			return store.DeliveryBind{}, mapError(err)
		}
		return store.DeliveryBind{Result: store.DeliveryBound}, nil
	}
	_ = tx.Rollback()
	return classifyBoundRows(rows, deliveryID), nil
}

// classifyBoundRows decides a bind that did not move every row: all rows
// delivering under the same id is a forward, all final under the same id a
// replay, anything else a conflict.
func classifyBoundRows(rows []*ent.Message, deliveryID string) store.DeliveryBind {
	delivering, final := 0, 0
	outcome := ""
	for _, r := range rows {
		if r.DeliveryID == nil || *r.DeliveryID != deliveryID {
			return store.DeliveryBind{Result: store.DeliveryConflict}
		}
		switch {
		case r.DispatchState == store.MessageDispatchDelivering:
			delivering++
		case finalDeliveryStates[r.DispatchState]:
			final++
			if r.DeliveryOutcome != nil && outcome == "" {
				outcome = *r.DeliveryOutcome
			}
		}
	}
	switch {
	case delivering == len(rows):
		return store.DeliveryBind{Result: store.DeliveryForward}
	case final == len(rows) && outcome != "":
		return store.DeliveryBind{Result: store.DeliveryReplay, Outcome: outcome}
	}
	return store.DeliveryBind{Result: store.DeliveryConflict}
}

// UnbindMessageDelivery implements store.BrokerDispatchStore.
func (s *BrokerDispatchStore) UnbindMessageDelivery(ctx context.Context, deliveryID string, messageIDs []string) error {
	uids, err := parseDeliveryUUIDs(messageIDs)
	if err != nil {
		return err
	}
	_, err = s.client.Message.Update().
		Where(
			message.IDIn(uids...),
			message.DispatchStateEQ(store.MessageDispatchDelivering),
			message.DeliveryIDEQ(deliveryID),
		).
		SetDispatchState(store.MessageDispatchDispatched).
		ClearDeliveryID().
		ClearDeliveringAt().
		Save(ctx)
	return mapError(err)
}

// FinishMessageDelivery implements store.BrokerDispatchStore.
func (s *BrokerDispatchStore) FinishMessageDelivery(ctx context.Context, deliveryID string, messageIDs []string, state, outcome string) (int, error) {
	uids, err := parseDeliveryUUIDs(messageIDs)
	if err != nil {
		return 0, err
	}
	n, err := s.client.Message.Update().
		Where(
			message.IDIn(uids...),
			message.DispatchStateEQ(store.MessageDispatchDelivering),
			message.DeliveryIDEQ(deliveryID),
		).
		SetDispatchState(state).
		SetDeliveryOutcome(outcome).
		Save(ctx)
	return n, mapError(err)
}

// SweepStaleDeliveries implements store.BrokerDispatchStore.
func (s *BrokerDispatchStore) SweepStaleDeliveries(ctx context.Context, cutoff time.Time, outcome string) (int, error) {
	n, err := s.client.Message.Update().
		Where(
			message.DispatchStateEQ(store.MessageDispatchDelivering),
			message.DeliveringAtLT(cutoff),
		).
		SetDispatchState(store.MessageDispatchPartial).
		SetDeliveryOutcome(outcome).
		Save(ctx)
	return n, mapError(err)
}
