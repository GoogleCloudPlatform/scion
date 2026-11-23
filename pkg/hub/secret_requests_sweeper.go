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

	"github.com/GoogleCloudPlatform/scion/pkg/msgaction"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const (
	// secretRequestSweepIntervalMinutes is how often the expiry & stale-claim
	// sweeper runs.
	secretRequestSweepIntervalMinutes = 1
	// secretRequestSweepBatch bounds rows processed per store call.
	secretRequestSweepBatch = 200
	// secretRequestSweepTimeout bounds a single sweep tick.
	secretRequestSweepTimeout = 30 * time.Second
)

// registerSecretRequestExpiry registers the "secret-request-expiry" recurring
// singleton job. Must be called before the scheduler starts.
func (s *Server) registerSecretRequestExpiry(sched *Scheduler) {
	sched.RegisterRecurringSingleton("secret-request-expiry", secretRequestSweepIntervalMinutes,
		store.LockSecretRequestExpiry, func(ctx context.Context) {
			ctx, cancel := context.WithTimeout(ctx, secretRequestSweepTimeout)
			defer cancel()
			if _, _, err := s.sweepSecretRequests(ctx, time.Now().UTC()); err != nil {
				slog.Error("secret requests: expiry/claim sweep failed", "error", err)
			}
		})
}

// sweepSecretRequests expires overdue pending secret requests (sending NO
// message to the agent) and recovers stale fulfilling claims when
// SecretRequestKindSpec.ClaimRecovery == ClaimRecoveryToPending (C1).
// Returns (expiredCount, recoveredCount, err).
func (s *Server) sweepSecretRequests(ctx context.Context, now time.Time) (int, int, error) {
	now = now.UTC()
	expiredTotal := 0
	for {
		rows, err := s.store.ExpireSecretRequests(ctx, now, secretRequestSweepBatch)
		if err != nil {
			return expiredTotal, 0, err
		}
		for _, row := range rows {
			s.publishSecretRequestUpdated(ctx, row, nil)
			details := map[string]string{"secret_type": row.SecretType}
			if row.Target != "" {
				details["target"] = row.Target
			}
			s.emitMessageActionEvent(ctx, &MessageActionEvent{
				Kind:      msgaction.KindSecretRequest,
				EventType: MessageActionEventExpired,
				RowID:     row.ID,
				MessageID: row.MessageID,
				ProjectID: row.ProjectID,
				AgentID:   row.AgentID,
				AgentName: row.AgentName,
				Key:       row.Key,
				Version:   row.Version,
				ActorKind: MessageActionActorHub,
				Details:   details,
				Timestamp: now,
			})
		}
		expiredTotal += len(rows)
		if len(rows) < secretRequestSweepBatch {
			break
		}
	}

	recoveredTotal := 0
	// Honor C1: recover stale claims only when ClaimRecovery == ClaimRecoveryToPending.
	if msgaction.SecretRequestKindSpec.ClaimRecovery == msgaction.ClaimRecoveryToPending && msgaction.SecretRequestKindSpec.ClaimTTL > 0 {
		cutoff := now.Add(-msgaction.SecretRequestKindSpec.ClaimTTL)
		for {
			rows, err := s.store.RecoverStaleSecretRequestClaims(ctx, cutoff, secretRequestSweepBatch)
			if err != nil {
				return expiredTotal, recoveredTotal, err
			}
			for _, row := range rows {
				s.publishSecretRequestUpdated(ctx, row, nil)
				details := map[string]string{"secret_type": row.SecretType}
				if row.Target != "" {
					details["target"] = row.Target
				}
				s.emitMessageActionEvent(ctx, &MessageActionEvent{
					Kind:      msgaction.KindSecretRequest,
					EventType: msgaction.EventClaimRecovered,
					RowID:     row.ID,
					MessageID: row.MessageID,
					ProjectID: row.ProjectID,
					AgentID:   row.AgentID,
					AgentName: row.AgentName,
					Key:       row.Key,
					Version:   row.Version,
					ActorKind: MessageActionActorHub,
					Details:   details,
					Timestamp: now,
				})
			}
			recoveredTotal += len(rows)
			if len(rows) < secretRequestSweepBatch {
				break
			}
		}
	}

	return expiredTotal, recoveredTotal, nil
}
