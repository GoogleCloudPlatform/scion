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
	"fmt"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/runtimebroker"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// presentationFenceAdvances reports whether a write with (epoch, seq) at now
// advances the stored fence of b. It mirrors the WHERE clause of
// SetRuntimeBrokerPresentation, which stays the authority; this read-side
// copy only tells a stale request from a concurrent change.
func presentationFenceAdvances(storedEpoch, storedSeq *int64, fencedAt *time.Time, epoch, seq int64, now time.Time) bool {
	switch {
	case storedEpoch == nil:
		return true
	case *storedEpoch < epoch:
		return true
	case *storedEpoch == epoch && (storedSeq == nil || *storedSeq < seq):
		return true
	case fencedAt != nil && fencedAt.Before(now.Add(-store.PresentationFenceRecovery)):
		return true
	}
	return false
}

// SetRuntimeBrokerPresentation implements store.RuntimeBrokerStore. Each
// attempt is one conditional UPDATE bound to the row ID, its runtime target
// ID and type, the fence and the stored hash, so it never races the heartbeat's
// other row writes (it does not go through UpdateRuntimeBroker's
// read-modify-write) and an older process can never overwrite a newer
// snapshot. An attempt that matches no row because the row changed under it
// is retried from a fresh read.
func (s *ProjectStore) SetRuntimeBrokerPresentation(ctx context.Context, brokerID, targetID string, p *api.RuntimeTargetPresentation, hash string, epoch, seq int64, observedAt time.Time) (store.PresentationOutcome, error) {
	if p == nil {
		return "", errors.New("SetRuntimeBrokerPresentation: nil presentation")
	}
	uid, err := parseUUID(brokerID)
	if err != nil {
		return "", err
	}
	payload := marshalRawJSON(p)
	if payload == "" {
		return "", errors.New("SetRuntimeBrokerPresentation: presentation does not encode")
	}
	recoveryCutoff := observedAt.Add(-store.PresentationFenceRecovery)
	fence := runtimebroker.Or(
		runtimebroker.PresentationEpochIsNil(),
		runtimebroker.PresentationEpochLT(epoch),
		runtimebroker.And(runtimebroker.PresentationEpochEQ(epoch), runtimebroker.Or(
			runtimebroker.PresentationSeqIsNil(), runtimebroker.PresentationSeqLT(seq))),
		runtimebroker.PresentationFencedAtLT(recoveryCutoff),
	)
	for attempt := 0; attempt < maxCASRetries; attempt++ {
		cur, err := s.client.RuntimeBroker.Get(ctx, uid)
		if err != nil {
			return "", mapError(err)
		}
		if cur.RuntimeTargetID == nil || *cur.RuntimeTargetID == "" {
			return "", store.ErrRuntimeBrokerNotFlat
		}
		if *cur.RuntimeTargetID != targetID {
			return "", fmt.Errorf("%w: reported runtime target %q, stored %q",
				store.ErrRuntimeTargetChanged, targetID, *cur.RuntimeTargetID)
		}
		reportedType := ""
		if p.Target != nil {
			reportedType = p.Target.Type
		}
		if reportedType == "" || reportedType != cur.RuntimeTargetType {
			return "", fmt.Errorf("%w: reported runtime target type %q, stored %q",
				store.ErrRuntimeTargetChanged, reportedType, cur.RuntimeTargetType)
		}
		if !presentationFenceAdvances(cur.PresentationEpoch, cur.PresentationSeq, cur.PresentationFencedAt, epoch, seq, observedAt) {
			return store.PresentationStale, nil
		}
		update := s.client.RuntimeBroker.Update().
			Where(runtimebroker.IDEQ(uid), runtimebroker.RuntimeTargetIDEQ(targetID),
				runtimebroker.RuntimeTargetTypeEQ(reportedType), fence).
			SetPresentationEpoch(epoch).
			SetPresentationSeq(seq).
			SetPresentationFencedAt(observedAt)
		// The stored hash read above is part of the condition too, so a
		// concurrent write between the read and this update matches no row.
		storedHash := runtimebroker.RuntimeTargetPresentationHashEQ(cur.RuntimeTargetPresentationHash)
		if cur.RuntimeTargetPresentationHash == "" {
			storedHash = runtimebroker.Or(runtimebroker.RuntimeTargetPresentationHashIsNil(), storedHash)
		}
		update.Where(storedHash)
		changed := cur.RuntimeTargetPresentationHash != hash
		outcome := store.PresentationConfirmed
		if changed {
			outcome = store.PresentationWritten
			update.SetRuntimeTargetPresentation(payload).
				SetRuntimeTargetPresentationHash(hash).
				SetPresentationObservedAt(observedAt)
		} else if cur.PresentationObservedAt == nil || cur.PresentationObservedAt.Before(observedAt.Add(-store.PresentationObservedRefresh)) {
			// Unchanged: only the fence is confirmed; observed-at moves
			// only when it is older than the refresh period.
			update.SetPresentationObservedAt(observedAt)
		}
		affected, err := update.Save(ctx)
		if err != nil {
			return "", mapError(err)
		}
		if affected == 1 {
			return outcome, nil
		}
		// No row matched: a concurrent write changed the fence, the hash or
		// the target since the read. Re-read and decide again.
	}
	return "", store.ErrVersionConflict
}
