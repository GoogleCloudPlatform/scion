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
	"fmt"
	"log/slog"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Test identity expiry reaper (ptone/scion#4240, Phase 3b).
//
// Every minute, on one replica (LockTestIdentityReaper), while the hub runs
// with --enable-test-identities:
//
//  1. Each test identity that has expired and is still within its grace
//     period gets a membership-loss check (trigger test_identity_expiry),
//     and the outbox is drained at once. An expired identity is not
//     admitted to any project (testFixtureRejection in
//     userAdmittedToProjectAs), so its running agents are held and stopped
//     within one drain interval of the expiry.
//  2. Each identity past expires_at + testIdentityReaperGrace is purged
//     (purgeTestIdentity) as a system executor acting for its issuer: the
//     identity-level audit row is test_identity_expire, attributed to the
//     issuer with executor system:test_identity_reaper. A purge that does
//     not complete is retried on the next tick.

// testIdentityReaperIntervalMinutes is the reaper's cadence: the
// membership-loss drain interval.
const testIdentityReaperIntervalMinutes = membershipLossDrainInterval

// testIdentityReaperBatch bounds the identities one tick reads.
const testIdentityReaperBatch = 50

// testIdentityReaperGrace is how long after its expiry an identity is
// purged. Its agents are held from the expiry on. A variable so tests can
// change it.
var testIdentityReaperGrace = 5 * time.Minute

// registerTestIdentityReaper registers the reaper when the feature is on.
func (s *Server) registerTestIdentityReaper() {
	if !s.testIdentities.enabled || s.scheduler == nil {
		return
	}
	s.scheduler.RegisterRecurringSingleton("test-identity-reaper", testIdentityReaperIntervalMinutes,
		store.LockTestIdentityReaper, func(ctx context.Context) {
			if _, err := s.reapTestIdentities(ctx, s.testIdentities.clock()); err != nil {
				slog.Error("test identity reaper failed", "error", err)
			}
		})
}

// testIdentityReapResult is what one reaper tick did.
type testIdentityReapResult struct {
	// Held is the number of expired identities a check was enqueued for.
	Held int
	// Purged lists the identities whose purge completed.
	Purged []string
	// Incomplete lists the identities whose purge left work for a later
	// tick.
	Incomplete []string
}

// reapTestIdentities runs one reaper tick at now.
func (s *Server) reapTestIdentities(ctx context.Context, now time.Time) (testIdentityReapResult, error) {
	var res testIdentityReapResult
	users, err := s.store.ListExpiredTestFixtureUsers(ctx, now, testIdentityReaperBatch)
	if err != nil {
		return res, fmt.Errorf("list expired test identities: %w", err)
	}
	if len(users) == 0 {
		return res, nil
	}

	// 1. Hold: identities still within their grace period.
	var toHold []string
	for i := range users {
		u := &users[i]
		if u.ExpiresAt != nil && now.Before(u.ExpiresAt.Add(testIdentityReaperGrace)) {
			toHold = append(toHold, u.ID)
		}
	}
	if len(toHold) > 0 {
		actor := AuditActor{PrincipalKind: membershipLossSystemActorKind, PrincipalID: membershipLossSystemActorID}
		err := s.store.WithTx(ctx, func(tx store.Store) error {
			for _, id := range toHold {
				if err := enqueueMembershipLossTx(ctx, tx, id, "", store.MembershipLossTriggerTestIdentityExpiry, actor); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return res, fmt.Errorf("enqueue expired test identity checks: %w", err)
		}
		res.Held = len(toHold)
		s.drainMembershipLossChecks(ctx)
	}

	// 2. Purge: identities past their grace period.
	for i := range users {
		u := &users[i]
		if u.ExpiresAt != nil && now.Before(u.ExpiresAt.Add(testIdentityReaperGrace)) {
			continue
		}
		issuer := ""
		if u.IssuedBy != nil {
			issuer = *u.IssuedBy
		}
		run := fixturePurgeRun{
			rootID:      u.ID,
			issuerID:    issuer,
			user:        u,
			trigger:     testIdentityExpireMutation,
			executor:    fixturePurgeExecutorReaper,
			requestedBy: fixturePurgeExecutorReaper,
			actor:       AuditActor{PrincipalKind: "user", PrincipalID: issuer},
		}
		report, err := s.purgeTestIdentity(ctx, run)
		if err != nil {
			slog.Error("test identity reaper: purge failed", "test_identity_id", u.ID, "error", err)
			res.Incomplete = append(res.Incomplete, u.ID)
			continue
		}
		if report.Complete {
			res.Purged = append(res.Purged, u.ID)
			slog.Info("test identity reaper: expired identity purged", "test_identity_id", u.ID,
				"agents", report.Deleted.Agents, "projects", report.Deleted.Projects)
			continue
		}
		res.Incomplete = append(res.Incomplete, u.ID)
		slog.Warn("test identity reaper: purge incomplete; retrying next tick", "test_identity_id", u.ID,
			"not_done", len(report.NotDone), "unproven", len(report.Unproven), "identity_kept_reason", report.IdentityKeptReason)
	}
	return res, nil
}
