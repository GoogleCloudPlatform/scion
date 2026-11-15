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
	"slices"
	"time"

	"entgo.io/ent/dialect"

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

// bindBeforeTxHook, when set by a test, runs right before each bind
// transaction starts: the last point a concurrent writer can act before
// this call reads and decides.
var bindBeforeTxHook func()

// bindAttempts bounds BindMessageDelivery's transactions when its
// conditional update loses to a concurrent writer (which the row lock
// should prevent; kept as a safety net).
const bindAttempts = 3

// BindMessageDelivery implements store.BrokerDispatchStore.
//
// It locks, reads, decides and writes in one transaction (review P3-R1):
// the rows are read inside the transaction (SELECT ... FOR UPDATE on
// Postgres; SQLite runs every transaction on its single writer connection),
// so the outcome is classified from their current state, never from a
// snapshot taken before a concurrent same-id bind. Existence and recipient
// are checked in that same read. A call that does not bind never forwards,
// unbinds or finishes.
func (s *BrokerDispatchStore) BindMessageDelivery(ctx context.Context, recipientID, deliveryID string, messageIDs []string, now time.Time) (store.DeliveryBind, error) {
	uids, err := parseDeliveryUUIDs(messageIDs)
	if err != nil || len(uids) == 0 {
		return store.DeliveryBind{}, store.ErrNotFound
	}
	for attempt := 0; attempt < bindAttempts; attempt++ {
		res, retry, err := s.bindOnce(ctx, recipientID, deliveryID, uids, now)
		if err != nil || !retry {
			return res, err
		}
	}
	return store.DeliveryBind{}, store.ErrDeliveryContended
}

// bindOnce is one transaction of BindMessageDelivery. retry reports that
// the conditional update lost to a concurrent writer after the read showed
// the rows bindable.
func (s *BrokerDispatchStore) bindOnce(ctx context.Context, recipientID, deliveryID string, uids []uuid.UUID, now time.Time) (store.DeliveryBind, bool, error) {
	if bindBeforeTxHook != nil {
		bindBeforeTxHook()
	}
	tx, err := s.client.Tx(ctx)
	if err != nil {
		return store.DeliveryBind{}, false, mapError(err)
	}
	defer func() { _ = tx.Rollback() }()
	q := tx.Message.Query().Where(message.IDIn(uids...))
	if s.client.Driver().Dialect() == dialect.Postgres {
		q = q.ForUpdate()
	}
	rows, err := q.Select(message.FieldRecipientID, message.FieldDispatchState, message.FieldDeliveryID, message.FieldDeliveryOutcome).All(ctx)
	if err != nil {
		return store.DeliveryBind{}, false, mapError(err)
	}
	if len(rows) != len(uids) {
		return store.DeliveryBind{}, false, store.ErrNotFound
	}
	for _, r := range rows {
		if r.RecipientID != recipientID {
			return store.DeliveryBind{}, false, store.ErrNotFound
		}
	}
	if !allBindable(rows) {
		return classifyBoundRows(rows, deliveryID), false, nil
	}
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
		ClearNotReached().
		ClearNotReachedAt().
		Save(ctx)
	if err != nil {
		return store.DeliveryBind{}, false, mapError(err)
	}
	if n != len(uids) {
		return store.DeliveryBind{}, true, nil
	}
	if err := tx.Commit(); err != nil {
		return store.DeliveryBind{}, false, mapError(err)
	}
	return store.DeliveryBind{Result: store.DeliveryBound}, false, nil
}

// allBindable reports whether every row may be bound: a bindable state and
// no delivery id.
func allBindable(rows []*ent.Message) bool {
	for _, r := range rows {
		if r.DeliveryID != nil || !slices.Contains(bindableDispatchStates, r.DispatchState) {
			return false
		}
	}
	return true
}

// classifyBoundRows decides, from the rows' current state, a bind that
// cannot move every row. All rows must carry this delivery id; otherwise it
// is a conflict. Then:
//   - any row still delivering: the owner is still running (in flight),
//     even if another row was already failed under it;
//   - every row final: a replay of the aggregated outcome
//     (aggregateOutcome);
//   - anything else: a conflict.
func classifyBoundRows(rows []*ent.Message, deliveryID string) store.DeliveryBind {
	delivering := 0
	outcomes := make([]*string, 0, len(rows))
	for _, r := range rows {
		if r.DeliveryID == nil || *r.DeliveryID != deliveryID {
			return store.DeliveryBind{Result: store.DeliveryConflict}
		}
		switch {
		case r.DispatchState == store.MessageDispatchDelivering:
			delivering++
		case finalDeliveryStates[r.DispatchState]:
			outcomes = append(outcomes, r.DeliveryOutcome)
		default:
			return store.DeliveryBind{Result: store.DeliveryConflict}
		}
	}
	if delivering > 0 {
		return store.DeliveryBind{Result: store.DeliveryInFlight}
	}
	return store.DeliveryBind{Result: store.DeliveryReplay, Outcome: aggregateOutcome(outcomes)}
}

// aggregateOutcome is the outcome a replay of a whole flush reports from
// its rows' stored outcomes, conservatively (review P4-R1): one row's
// outcome is never reported for the others. When every row stored the same
// outcome, that is it (all delivered is delivered; a homogeneous partial
// keeps its stage). If any row has none, or the rows differ (for example
// one row failed while the rest were delivered), it is partial/unknown.
func aggregateOutcome(outcomes []*string) string {
	if len(outcomes) == 0 || outcomes[0] == nil {
		return store.DeliveryOutcomeUnknownPartial
	}
	first := *outcomes[0]
	for _, o := range outcomes[1:] {
		if o == nil || *o != first {
			return store.DeliveryOutcomeUnknownPartial
		}
	}
	return first
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
