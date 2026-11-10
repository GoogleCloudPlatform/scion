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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Pending secret expiry. Every read and decision path already treats a
// pending secret past its expiry as gone, so this sweep only removes the
// rows and releases their held values. It runs hourly on one replica
// (store.LockPendingSecretExpiry); a skipped tick is safe.

const (
	// pendingSecretExpiryIntervalMinutes is how often the sweep runs.
	pendingSecretExpiryIntervalMinutes = 60
	// pendingSecretExpiryBatch bounds the rows one store call deletes.
	pendingSecretExpiryBatch = 200
	// pendingSecretExpiryTimeout bounds one tick.
	pendingSecretExpiryTimeout = 2 * time.Minute
)

// registerPendingSecretExpiry registers the "pending-secret-expiry" sweep.
// It must be called before the scheduler starts.
func (s *Server) registerPendingSecretExpiry(sched *Scheduler) {
	sched.RegisterRecurringSingleton("pending-secret-expiry", pendingSecretExpiryIntervalMinutes,
		store.LockPendingSecretExpiry, func(ctx context.Context) {
			ctx, cancel := context.WithTimeout(ctx, pendingSecretExpiryTimeout)
			defer cancel()
			if _, err := s.expirePendingSecrets(ctx, time.Now()); err != nil {
				slog.Error("pending secret: expiry sweep failed", "error", err)
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
