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
	"errors"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/predicate"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/runtimebroker"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// legacyRow matches a Runtime Broker row with no stored runtime target.
func legacyRow() predicate.RuntimeBroker {
	return runtimebroker.Or(runtimebroker.RuntimeTargetIDIsNil(), runtimebroker.RuntimeTargetIDEQ(""))
}

// SetRuntimeBrokerConversionPlan implements store.RuntimeBrokerStore. It
// reads the row once, then makes at most one conditional UPDATE, bound to the
// row ID, to the row being legacy and to the hash it read, so it never races
// the heartbeat's other row writes (it does not go through
// UpdateRuntimeBroker's read-modify-write) and never writes a flat row. In the
// steady state (an unchanged plan whose observed-at is fresh) it only reads.
// An UPDATE that matches no row because the row changed under it is retried
// from a fresh read.
func (s *ProjectStore) SetRuntimeBrokerConversionPlan(ctx context.Context, brokerID string, p *api.ConversionPlanPresentation, hash string, observedAt time.Time) (store.PresentationOutcome, error) {
	if p == nil {
		return "", errors.New("SetRuntimeBrokerConversionPlan: nil conversion plan")
	}
	uid, err := parseUUID(brokerID)
	if err != nil {
		return "", err
	}
	payload := marshalRawJSON(p)
	if payload == "" {
		return "", errors.New("SetRuntimeBrokerConversionPlan: conversion plan does not encode")
	}
	for attempt := 0; attempt < maxCASRetries; attempt++ {
		cur, err := s.client.RuntimeBroker.Get(ctx, uid)
		if err != nil {
			return "", mapError(err)
		}
		if cur.RuntimeTargetID != nil && *cur.RuntimeTargetID != "" {
			return "", store.ErrRuntimeBrokerNotLegacy
		}
		// The stored hash read above is part of the condition, so a
		// concurrent write between the read and the update matches no row.
		storedHash := runtimebroker.ConversionPlanPresentationHashEQ(cur.ConversionPlanPresentationHash)
		if cur.ConversionPlanPresentationHash == "" {
			storedHash = runtimebroker.Or(runtimebroker.ConversionPlanPresentationHashIsNil(), storedHash)
		}
		update := s.client.RuntimeBroker.Update().Where(runtimebroker.IDEQ(uid), legacyRow(), storedHash)
		outcome := store.PresentationConfirmed
		switch {
		case cur.ConversionPlanPresentationHash != hash:
			outcome = store.PresentationWritten
			update.SetConversionPlanPresentation(payload).
				SetConversionPlanPresentationHash(hash).
				SetConversionPlanObservedAt(observedAt)
		case cur.ConversionPlanObservedAt == nil || cur.ConversionPlanObservedAt.Before(observedAt.Add(-store.PresentationObservedRefresh)):
			update.SetConversionPlanObservedAt(observedAt)
		default:
			// Unchanged and observed recently: nothing to write.
			return store.PresentationConfirmed, nil
		}
		affected, err := update.Save(ctx)
		if err != nil {
			return "", mapError(err)
		}
		if affected == 1 {
			return outcome, nil
		}
		// No row matched: the row changed since the read (a concurrent
		// plan write, or it became flat or was deleted). Read again.
	}
	return "", store.ErrVersionConflict
}
