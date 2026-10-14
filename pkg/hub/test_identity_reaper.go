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
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Test identity expiry reaper (ptone/scion#4240, Phase 3b).
//
// Every minute, on one replica (LockTestIdentityReaper), while the hub runs
// with --test-mode=member or above:
//
//  1. Each test identity that has expired and is still within its grace
//     period gets a membership-loss check (trigger test_identity_expiry),
//     and the outbox is drained at once. This uses its own query, so it
//     never waits behind identities whose purge cannot complete. An expired identity is not
//     admitted to any project (testFixtureRejection in
//     userAdmittedToProjectAs), so its running agents are held and stopped
//     within one drain interval of the expiry.
//  2. The next batch of identities past expires_at + testIdentityReaperGrace
//     (testIdentityReaperCursor: a keyset cursor that moves past every
//     identity tried and wraps at the end) is purged
//     (purgeTestIdentity) as a system executor acting for its issuer: the
//     identity-level audit row is test_identity_expire, attributed to the
//     issuer with executor system:test_identity_reaper. A purge that does
//     not complete is retried on the next tick.

// testIdentityReaperIntervalMinutes is the reaper's cadence: the
// membership-loss drain interval.
const testIdentityReaperIntervalMinutes = membershipLossDrainInterval

// testIdentityReaperBatch bounds the identities one tick purges. A
// variable so tests can lower it.
var testIdentityReaperBatch = 50

// testIdentityReaperHoldLimit bounds the identities one tick holds. Only
// identities that expired within the grace period are held, so the set is
// small (issuance is capped and rate limited).
const testIdentityReaperHoldLimit = 1000

// testIdentityReaperGrace is how long after its expiry an identity is
// purged. Its agents are held from the expiry on. A variable so tests can
// change it.
var testIdentityReaperGrace = 5 * time.Minute

// testIdentityReaperCursor is where the next purge batch starts, in
// (expires_at, id) order. Each tick resumes after the last identity it
// tried, whatever that purge's outcome, and wraps to the start once a
// batch comes back short, so identities whose purge cannot complete never
// keep later ones from being purged. It lives in the process: the reaper
// is a singleton per tick, and a replica that takes over starts from the
// beginning.
type testIdentityReaperCursor struct {
	mu        sync.Mutex
	expiresAt time.Time
	id        string
}

func (c *testIdentityReaperCursor) get() (time.Time, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.expiresAt, c.id
}

func (c *testIdentityReaperCursor) set(expiresAt time.Time, id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.expiresAt, c.id = expiresAt, id
}

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

// reapTestIdentities runs one reaper tick at now: hold the identities that
// expired within the grace period, then purge the next batch of those past
// it.
func (s *Server) reapTestIdentities(ctx context.Context, now time.Time) (testIdentityReapResult, error) {
	var res testIdentityReapResult
	held, err := s.holdExpiredTestIdentities(ctx, now)
	res.Held = held
	if err != nil {
		return res, err
	}

	cursor := s.testIdentities.reaperCursor
	if cursor == nil {
		cursor = &testIdentityReaperCursor{}
	}
	afterExpiresAt, afterID := cursor.get()
	batch := testIdentityReaperBatch
	users, err := s.store.ListExpiredTestFixtureUsers(ctx, store.ExpiredTestFixtureQuery{
		ExpiredAtOrBefore: now.Add(-testIdentityReaperGrace),
		AfterExpiresAt:    afterExpiresAt,
		AfterID:           afterID,
		Limit:             batch,
	})
	if err != nil {
		return res, fmt.Errorf("list test identities to purge: %w", err)
	}
	for i := range users {
		u := &users[i]
		if u.ExpiresAt != nil {
			cursor.set(*u.ExpiresAt, u.ID)
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
		slog.Warn("test identity reaper: purge incomplete; retrying on a later tick", "test_identity_id", u.ID,
			"not_done", len(report.NotDone), "unproven", len(report.Unproven), "identity_kept_reason", report.IdentityKeptReason)
	}
	if len(users) < batch {
		// The end of the list: the next tick starts from the beginning.
		cursor.set(time.Time{}, "")
	}
	return res, nil
}

// holdExpiredTestIdentities enqueues a membership-loss check (trigger
// test_identity_expiry) for every test identity that expired within the
// grace period, and drains the outbox. Its own query, independent of the
// purge backlog, so a newly expired identity is always held on the next
// tick.
func (s *Server) holdExpiredTestIdentities(ctx context.Context, now time.Time) (int, error) {
	users, err := s.store.ListExpiredTestFixtureUsers(ctx, store.ExpiredTestFixtureQuery{
		ExpiredAtOrBefore: now,
		ExpiredAfter:      now.Add(-testIdentityReaperGrace),
		Limit:             testIdentityReaperHoldLimit,
	})
	if err != nil {
		return 0, fmt.Errorf("list expired test identities to hold: %w", err)
	}
	if len(users) == 0 {
		return 0, nil
	}
	actor := AuditActor{PrincipalKind: membershipLossSystemActorKind, PrincipalID: membershipLossSystemActorID}
	err = s.store.WithTx(ctx, func(tx store.Store) error {
		for i := range users {
			if err := enqueueMembershipLossTx(ctx, tx, users[i].ID, "", store.MembershipLossTriggerTestIdentityExpiry, actor); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("enqueue expired test identity checks: %w", err)
	}
	s.drainMembershipLossChecks(ctx)
	return len(users), nil
}
