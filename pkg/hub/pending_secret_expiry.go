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

package hub

import (
	"context"
	"log/slog"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Pending secret expiry. Every read and decision path already treats a
// pending secret past its expiry as gone, so this sweep only removes the
// rows and releases their held values. It runs hourly on one replica
// (store.LockPendingSecretExpiry); a skipped tick is safe.
//
// The same tick releases held values no pending row references: a value
// held for a write whose row was never inserted, or one whose row was
// claimed by an approval that never finished. Only values the backend
// lists as this hub's held pending values (ListHeldPending) are
// considered, and only once they are older than pendingHeldValueGrace, so
// a value held just before its row is inserted is never released.

const (
	// pendingSecretExpiryIntervalMinutes is how often the sweep runs.
	pendingSecretExpiryIntervalMinutes = 60
	// pendingSecretExpiryBatch bounds the rows one store call deletes.
	pendingSecretExpiryBatch = 200
	// pendingSecretExpiryTimeout bounds one tick.
	pendingSecretExpiryTimeout = 2 * time.Minute
	// pendingHeldValueGrace is the minimum age of a held value no row
	// references before it is released.
	pendingHeldValueGrace = time.Hour
	// pendingSecretOrphanedValue is the audit reason of a released held
	// value no row referenced.
	pendingSecretOrphanedValue = "unreferenced_value"
)

// registerPendingSecretExpiry registers the "pending-secret-expiry" sweep.
// It must be called before the scheduler starts.
func (s *Server) registerPendingSecretExpiry(sched *Scheduler) {
	sched.RegisterRecurringSingleton("pending-secret-expiry", pendingSecretExpiryIntervalMinutes,
		store.LockPendingSecretExpiry, func(ctx context.Context) {
			ctx, cancel := context.WithTimeout(ctx, pendingSecretExpiryTimeout)
			defer cancel()
			now := time.Now()
			if _, err := s.expirePendingSecrets(ctx, now); err != nil {
				slog.Error("pending secret: expiry sweep failed", "error", err)
			}
			if _, err := s.releaseUnreferencedPendingValues(ctx, now); err != nil {
				slog.Error("pending secret: releasing unreferenced held values failed", "error", err)
			}
		})
}

// expirePendingSecrets deletes every pending secret expired at now, then,
// after each batch has committed, releases the held values and logs a
// secret_pending_expire audit event per row. It returns the number of rows
// deleted. Running it again deletes nothing more.
func (s *Server) expirePendingSecrets(ctx context.Context, now time.Time) (int, error) {
	total := 0
	for {
		rows, err := s.store.DeleteExpiredPendingSecrets(ctx, now, pendingSecretExpiryBatch)
		if err != nil {
			return total, err
		}
		for i := range rows {
			p := &rows[i]
			if s.secretBackend != nil {
				s.dropPendingValues(ctx, []string{p.ValueRef})
			}
			s.auditPendingSecret(ctx, PendingSecretEventExpire, p, PendingSecretActorSystem, "", "")
		}
		total += len(rows)
		if len(rows) < pendingSecretExpiryBatch {
			return total, nil
		}
	}
}

// releaseUnreferencedPendingValues releases this hub's held pending values
// that no pending row references and that were stored before now minus
// pendingHeldValueGrace, logging a secret_pending_expire audit event
// (actor system, reason unreferenced_value) for each. It returns how many
// it released. A value with a row, a younger value, and anything the
// backend does not list as a held pending value are left alone.
func (s *Server) releaseUnreferencedPendingValues(ctx context.Context, now time.Time) (int, error) {
	if s.secretBackend == nil {
		return 0, nil
	}
	held, err := s.secretBackend.ListHeldPending(ctx)
	if err != nil {
		return 0, err
	}
	cutoff := now.Add(-pendingHeldValueGrace)
	var candidates []secret.HeldPendingValue
	for _, h := range held {
		if h.Ref == "" || h.Created.IsZero() || h.Created.After(cutoff) {
			continue
		}
		candidates = append(candidates, h)
	}
	if len(candidates) == 0 {
		return 0, nil
	}
	refs := make([]string, len(candidates))
	for i, h := range candidates {
		refs[i] = h.Ref
	}
	inUse, err := s.store.PendingSecretValueRefsInUse(ctx, refs)
	if err != nil {
		return 0, err
	}
	released := 0
	for _, h := range candidates {
		if inUse[h.Ref] {
			continue
		}
		if err := s.secretBackend.DropPending(ctx, h.Ref); err != nil {
			slog.Warn("pending secret: releasing an unreferenced held value failed",
				"pending_id", h.PendingID, "error", err)
			continue
		}
		s.auditPendingSecret(ctx, PendingSecretEventExpire, &store.PendingSecret{ID: h.PendingID},
			PendingSecretActorSystem, "", pendingSecretOrphanedValue)
		released++
	}
	return released, nil
}
