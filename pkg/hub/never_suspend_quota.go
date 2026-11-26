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
	"errors"
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Row-only enforcement for LimitMaxNeverSuspendAgentsPerProject (design D6/§4.5).
//
// Unlike the reservation-based quotas (max_agents_per_project,
// max_agents_per_broker), never-suspend usage is defined directly by the live
// agent rows:
//
//	COUNT(*) FROM agents
//	WHERE project_id = ? AND suspend_policy = 'never' AND deleted_at IS NULL
//
// There are no usage_reservations rows and no reconcile pass. Every writer
// that can make an agent "never" (create, PATCH auto->never, restore of a
// flagged agent) runs lock -> count -> write -> commit under the per-project
// quota lock (in-process scope mutex + Postgres advisory lock), held until the
// transaction commits or rolls back. Clearing the flag, soft delete, and hard
// delete free a slot immediately by construction.

// resolveNeverSuspendLimit returns the effective positive cap for
// LimitMaxNeverSuspendAgentsPerProject in projectID for subjectID, or 0 when
// enforcement does not apply (quotaService nil, projectID empty, experiment
// off, limit undefined, enforcement disabled, or effective limit unlimited).
// Callers must short-circuit without taking a lock or running any count query
// when this returns 0.
func (s *Server) resolveNeverSuspendLimit(ctx context.Context, subjectID, projectID string) (int64, error) {
	if s.quotaService == nil || projectID == "" || !s.experimentEnabled(experiments.NeverSuspend) {
		return 0, nil
	}
	const limitName = store.LimitMaxNeverSuspendAgentsPerProject
	if !s.quotaService.isEnforced(limitName) {
		return 0, nil
	}
	limitDef, err := s.store.GetLimitDefinitionByName(ctx, limitName)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return 0, nil
		}
		return 0, fmt.Errorf("quota: lookup limit definition %q: %w", limitName, err)
	}
	if limitDef == nil {
		return 0, nil
	}
	if subjectID == "" {
		subjectID = projectID
	}
	effectiveLimit, err := s.quotaService.effectiveLimitForReserve(ctx, limitName, limitDef.ID, subjectID, store.QuotaScopeProject, projectID)
	if err != nil {
		return 0, fmt.Errorf("quota: resolve effective limit for %q: %w", limitName, err)
	}
	if effectiveLimit <= 0 {
		return 0, nil
	}
	return effectiveLimit, nil
}

// acquireNeverSuspendProjectLock acquires the in-process per-project scope
// mutex and the cross-replica Postgres advisory lock for
// LimitMaxNeverSuspendAgentsPerProject. The returned release func releases
// both locks and must be called after the enclosing transaction commits or
// rolls back.
func (qs *QuotaService) acquireNeverSuspendProjectLock(ctx context.Context, projectID string) (func(), error) {
	const limitName = store.LimitMaxNeverSuspendAgentsPerProject
	scopeLock := qs.lockForScope(limitName, store.QuotaScopeProject, projectID)
	scopeLock.Lock()

	locker, ok := qs.store.(store.AdvisoryLocker)
	if !ok {
		scopeLock.Unlock()
		return nil, fmt.Errorf("quota enforcement unavailable: store does not support advisory locks")
	}

	acquired, releaseAdvisory, err := locker.TryAdvisoryLockObject(ctx, store.LockQuotaEnforcement, store.StableProjectHash(projectID))
	if err != nil {
		scopeLock.Unlock()
		return nil, fmt.Errorf("quota: advisory lock for %q: %w", limitName, err)
	}
	if !acquired {
		_ = releaseAdvisory()
		scopeLock.Unlock()
		return nil, ErrQuotaLockContention
	}

	return func() {
		if releaseErr := releaseAdvisory(); releaseErr != nil {
			qs.logger.Warn("failed to release quota advisory lock",
				"limit", limitName, "error", releaseErr)
		}
		scopeLock.Unlock()
	}, nil
}

// withNeverSuspendQuotaLock resolves the effective never-suspend cap for
// (subjectID, projectID). When the cap is inactive or unlimited it calls
// fn(0) immediately with no lock and no count query. When finite, it acquires
// the per-project quota lock, invokes fn(effectiveLimit), and releases the
// lock only after fn returns (i.e., after fn's transaction commits or rolls
// back).
func (s *Server) withNeverSuspendQuotaLock(ctx context.Context, subjectID, projectID string, fn func(effectiveLimit int64) error) error {
	effectiveLimit, err := s.resolveNeverSuspendLimit(ctx, subjectID, projectID)
	if err != nil {
		return err
	}
	if effectiveLimit <= 0 {
		return fn(0)
	}
	release, err := s.quotaService.acquireNeverSuspendProjectLock(ctx, projectID)
	if err != nil {
		return err
	}
	defer release()
	return fn(effectiveLimit)
}

// checkNeverSuspendQuotaInTx checks whether projectID has headroom for one
// more never-suspend agent before the row is written/restored inside tx.
func checkNeverSuspendQuotaInTx(ctx context.Context, tx store.Store, projectID string, effectiveLimit int64) error {
	count, err := tx.CountNeverSuspendAgentsInProject(ctx, projectID)
	if err != nil {
		return fmt.Errorf("quota: count never-suspend agents for %q: %w", store.LimitMaxNeverSuspendAgentsPerProject, err)
	}
	if count >= effectiveLimit {
		return &QuotaExceededError{
			Limit:     store.LimitMaxNeverSuspendAgentsPerProject,
			ScopeType: store.QuotaScopeProject,
			ScopeID:   projectID,
			Count:     count,
			Max:       effectiveLimit,
		}
	}
	return nil
}

// checkNeverSuspendQuotaAfterWriteInTx checks whether projectID exceeded its
// never-suspend cap after a conditional write inside tx flipped an existing
// live row from "auto" to "never" (so the flipped row is already included in
// the count). Returning *QuotaExceededError rolls back the enclosing tx.
func checkNeverSuspendQuotaAfterWriteInTx(ctx context.Context, tx store.Store, projectID string, effectiveLimit int64) error {
	count, err := tx.CountNeverSuspendAgentsInProject(ctx, projectID)
	if err != nil {
		return fmt.Errorf("quota: count never-suspend agents for %q: %w", store.LimitMaxNeverSuspendAgentsPerProject, err)
	}
	if count > effectiveLimit {
		return &QuotaExceededError{
			Limit:     store.LimitMaxNeverSuspendAgentsPerProject,
			ScopeType: store.QuotaScopeProject,
			ScopeID:   projectID,
			Count:     count - 1,
			Max:       effectiveLimit,
		}
	}
	return nil
}

// checkNeverSuspendQuotaHeadroom performs an unlocked pre-check before an
// env-gather recreate tears down an existing "auto" provisioning row to create
// a "never" replacement, so an over-cap request fails before destroying the
// existing row. The subsequent commitAgentCreate performs the authoritative
// locked check-and-insert.
func (s *Server) checkNeverSuspendQuotaHeadroom(ctx context.Context, subjectID, projectID string) error {
	effectiveLimit, err := s.resolveNeverSuspendLimit(ctx, subjectID, projectID)
	if err != nil || effectiveLimit <= 0 {
		return err
	}
	return checkNeverSuspendQuotaInTx(ctx, s.store, projectID, effectiveLimit)
}
