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
	"fmt"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/predicate"
	enttrigger "github.com/GoogleCloudPlatform/scion/pkg/ent/trigger"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/triggersubjectclaim"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func claimTimePtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

func entTriggerSubjectClaimToStore(e *ent.TriggerSubjectClaim) *store.TriggerSubjectClaim {
	out := &store.TriggerSubjectClaim{
		ID:            e.ID.String(),
		TriggerID:     e.TriggerID.String(),
		ActionIndex:   e.ActionIndex,
		Subject:       e.Subject,
		State:         e.State,
		EventID:       e.EventID.String(),
		FireID:        e.FireID.String(),
		ClaimedAt:     e.ClaimedAt.UTC(),
		BoundAt:       claimTimePtr(e.BoundAt),
		ReleasedAt:    claimTimePtr(e.ReleasedAt),
		ReleaseReason: e.ReleaseReason,
	}
	if e.AgentID != nil {
		out.AgentID = e.AgentID.String()
	}
	return out
}

// ClaimTriggerSubject atomically takes ownership of (triggerID, actionIndex, subject)
// via INSERT ... ON CONFLICT DO NOTHING, or conditional UPDATE when the existing
// row is released or is an abandoned unbound claim (agent_id IS NULL AND
// claimed_at < abandonedBefore). Returns (claimed, nil, nil) when taken, or
// (nil, existing, nil) when another active claim holds the subject (Stage 2 §5.6,
// action-arrays §1).
func (s *TriggerStore) ClaimTriggerSubject(
	ctx context.Context,
	triggerID string,
	actionIndex int,
	subject, eventID, fireID string,
	now, abandonedBefore time.Time,
) (*store.TriggerSubjectClaim, *store.TriggerSubjectClaim, error) {
	if actionIndex < 0 {
		return nil, nil, fmt.Errorf("%w: actionIndex must be non-negative", store.ErrInvalidInput)
	}
	if len(subject) > store.MaxTriggerSubjectBytes {
		return nil, nil, fmt.Errorf("%w: subject exceeds %d bytes", store.ErrInvalidInput, store.MaxTriggerSubjectBytes)
	}
	tid, err := parseUUID(triggerID)
	if err != nil {
		return nil, nil, err
	}
	eid, err := parseUUID(eventID)
	if err != nil {
		return nil, nil, err
	}
	fid, err := parseUUID(fireID)
	if err != nil {
		return nil, nil, err
	}

	now = now.UTC().Truncate(time.Microsecond)
	abandonedBefore = abandonedBefore.UTC().Truncate(time.Microsecond)

	const maxAttempts = 4
	for attempt := 0; attempt < maxAttempts; attempt++ {
		newID := uuid.New()
		create := s.client.TriggerSubjectClaim.Create().
			SetID(newID).
			SetTriggerID(tid).
			SetActionIndex(actionIndex).
			SetSubject(subject).
			SetState(store.TriggerSubjectClaimStateActive).
			SetEventID(eid).
			SetFireID(fid).
			SetClaimedAt(now)

		// CreateBulk + OnConflictColumns + DoNothing avoids aborting an
		// enclosing PostgreSQL transaction on unique conflict and tolerates
		// zero returned rows on both SQLite and PostgreSQL.
		if err := s.client.TriggerSubjectClaim.CreateBulk(create).
			OnConflictColumns(
				triggersubjectclaim.FieldTriggerID,
				triggersubjectclaim.FieldActionIndex,
				triggersubjectclaim.FieldSubject,
			).
			DoNothing().
			Exec(ctx); err != nil {
			return nil, nil, mapError(err)
		}

		row, err := s.client.TriggerSubjectClaim.Query().
			Where(
				triggersubjectclaim.TriggerIDEQ(tid),
				triggersubjectclaim.ActionIndexEQ(actionIndex),
				triggersubjectclaim.SubjectEQ(subject),
			).
			Only(ctx)
		if err != nil {
			if ent.IsNotFound(err) {
				// Row was concurrently purged between INSERT and SELECT; retry.
				continue
			}
			return nil, nil, mapError(err)
		}

		// If our INSERT won, our unique newID and fireID are on the row.
		if row.ID == newID && row.FireID == fid {
			return entTriggerSubjectClaimToStore(row), nil, nil
		}

		reclaimable := row.State == store.TriggerSubjectClaimStateReleased ||
			(row.State == store.TriggerSubjectClaimStateActive &&
				row.AgentID == nil &&
				row.ClaimedAt.UTC().Before(abandonedBefore))
		if !reclaimable {
			return nil, entTriggerSubjectClaimToStore(row), nil
		}

		affected, err := s.client.TriggerSubjectClaim.Update().
			Where(
				triggersubjectclaim.TriggerIDEQ(tid),
				triggersubjectclaim.ActionIndexEQ(actionIndex),
				triggersubjectclaim.SubjectEQ(subject),
				triggersubjectclaim.Or(
					triggersubjectclaim.StateEQ(store.TriggerSubjectClaimStateReleased),
					triggersubjectclaim.And(
						triggersubjectclaim.StateEQ(store.TriggerSubjectClaimStateActive),
						triggersubjectclaim.AgentIDIsNil(),
						triggersubjectclaim.ClaimedAtLT(abandonedBefore),
					),
				),
			).
			SetState(store.TriggerSubjectClaimStateActive).
			ClearAgentID().
			SetEventID(eid).
			SetFireID(fid).
			SetClaimedAt(now).
			ClearBoundAt().
			ClearReleasedAt().
			ClearReleaseReason().
			Save(ctx)
		if err != nil {
			return nil, nil, mapError(err)
		}
		if affected > 0 {
			return &store.TriggerSubjectClaim{
				ID:          row.ID.String(),
				TriggerID:   tid.String(),
				ActionIndex: actionIndex,
				Subject:     subject,
				State:       store.TriggerSubjectClaimStateActive,
				EventID:     eid.String(),
				FireID:      fid.String(),
				ClaimedAt:   now,
			}, nil, nil
		}

		// Another replica raced and took over (or purged) the row; re-read.
		current, qErr := s.client.TriggerSubjectClaim.Query().
			Where(
				triggersubjectclaim.TriggerIDEQ(tid),
				triggersubjectclaim.ActionIndexEQ(actionIndex),
				triggersubjectclaim.SubjectEQ(subject),
			).
			Only(ctx)
		if qErr != nil {
			if ent.IsNotFound(qErr) {
				continue
			}
			return nil, nil, mapError(qErr)
		}
		if current.State == store.TriggerSubjectClaimStateReleased {
			continue
		}
		return nil, entTriggerSubjectClaimToStore(current), nil
	}

	// Effectively unreachable: hitting maxAttempts requires another replica to
	// repeatedly purge or release-and-reclaim the exact same (trigger_id, action_index, subject)
	// row between every INSERT/UPDATE and SELECT in this loop (sustained livelock).
	return nil, nil, fmt.Errorf("%w: could not resolve trigger subject claim after concurrent retries", store.ErrVersionConflict)
}

// BindTriggerSubjectClaim sets agent_id and bound_at on an active claim held by fireID.
func (s *TriggerStore) BindTriggerSubjectClaim(ctx context.Context, claimID, fireID, agentID string, boundAt time.Time) (bool, error) {
	cid, err := parseUUID(claimID)
	if err != nil {
		return false, err
	}
	fid, err := parseUUID(fireID)
	if err != nil {
		return false, err
	}
	aid, err := parseUUID(agentID)
	if err != nil {
		return false, err
	}
	affected, err := s.client.TriggerSubjectClaim.Update().
		Where(
			triggersubjectclaim.IDEQ(cid),
			triggersubjectclaim.FireIDEQ(fid),
			triggersubjectclaim.StateEQ(store.TriggerSubjectClaimStateActive),
		).
		SetAgentID(aid).
		SetBoundAt(boundAt.UTC().Truncate(time.Microsecond)).
		Save(ctx)
	if err != nil {
		return false, mapError(err)
	}
	return affected > 0, nil
}

// ReleaseTriggerSubjectClaim transitions an active unbound claim (agent_id IS NULL)
// held by fireID to released. Bound claims are left intact so an ambiguous commit
// error on agent creation cannot release a claim that actually committed.
func (s *TriggerStore) ReleaseTriggerSubjectClaim(ctx context.Context, claimID, fireID string, releasedAt time.Time) (bool, error) {
	cid, err := parseUUID(claimID)
	if err != nil {
		return false, err
	}
	fid, err := parseUUID(fireID)
	if err != nil {
		return false, err
	}
	affected, err := s.client.TriggerSubjectClaim.Update().
		Where(
			triggersubjectclaim.IDEQ(cid),
			triggersubjectclaim.FireIDEQ(fid),
			triggersubjectclaim.StateEQ(store.TriggerSubjectClaimStateActive),
			triggersubjectclaim.AgentIDIsNil(),
		).
		SetState(store.TriggerSubjectClaimStateReleased).
		SetReleasedAt(releasedAt.UTC().Truncate(time.Microsecond)).
		Save(ctx)
	if err != nil {
		return false, mapError(err)
	}
	return affected > 0, nil
}

// GetActiveTriggerSubjectClaim returns the active claim row for
// (triggerID, actionIndex, subject), or store.ErrNotFound if none is active.
func (s *TriggerStore) GetActiveTriggerSubjectClaim(ctx context.Context, triggerID string, actionIndex int, subject string) (*store.TriggerSubjectClaim, error) {
	tid, err := parseUUID(triggerID)
	if err != nil {
		return nil, err
	}
	row, err := s.client.TriggerSubjectClaim.Query().
		Where(
			triggersubjectclaim.TriggerIDEQ(tid),
			triggersubjectclaim.ActionIndexEQ(actionIndex),
			triggersubjectclaim.SubjectEQ(subject),
			triggersubjectclaim.StateEQ(store.TriggerSubjectClaimStateActive),
		).
		Only(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return entTriggerSubjectClaimToStore(row), nil
}

// ReleaseTriggerSubjectClaimsForAgent releases every active claim bound to agentID.
// Idempotent when called on deletion retry.
func (s *TriggerStore) ReleaseTriggerSubjectClaimsForAgent(ctx context.Context, agentID string, releasedAt time.Time) (int, error) {
	aid, err := parseUUID(agentID)
	if err != nil {
		return 0, err
	}
	affected, err := s.client.TriggerSubjectClaim.Update().
		Where(
			triggersubjectclaim.AgentIDEQ(aid),
			triggersubjectclaim.StateEQ(store.TriggerSubjectClaimStateActive),
		).
		SetState(store.TriggerSubjectClaimStateReleased).
		SetReleasedAt(releasedAt.UTC().Truncate(time.Microsecond)).
		Save(ctx)
	if err != nil {
		return 0, mapError(err)
	}
	return affected, nil
}

// ReleaseTriggerSubjectClaimsForProjectBySourceKind releases every active
// subject claim belonging to triggers in projectID with the given sourceKind,
// recording release_reason (e.g. "association_lost" on
// installation_repositories.removed, Stage 3 §10.3). Idempotent on retry.
func (s *TriggerStore) ReleaseTriggerSubjectClaimsForProjectBySourceKind(
	ctx context.Context,
	projectID, sourceKind, reason string,
	releasedAt time.Time,
) (int, error) {
	pid, err := parseUUID(projectID)
	if err != nil {
		return 0, err
	}
	if sourceKind == "" {
		return 0, fmt.Errorf("%w: sourceKind is required", store.ErrInvalidInput)
	}
	upd := s.client.TriggerSubjectClaim.Update().
		Where(
			triggersubjectclaim.StateEQ(store.TriggerSubjectClaimStateActive),
			predicate.TriggerSubjectClaim(func(sel *entsql.Selector) {
				d := sel.Dialect()
				tr := entsql.Dialect(d).Table(enttrigger.Table).As("tr")
				sub := entsql.Dialect(d).Select(tr.C(enttrigger.FieldID)).
					From(tr).
					Where(entsql.And(
						entsql.ColumnsEQ(tr.C(enttrigger.FieldID), sel.C(triggersubjectclaim.FieldTriggerID)),
						entsql.EQ(tr.C(enttrigger.FieldProjectID), pid),
						entsql.EQ(tr.C(enttrigger.FieldSourceKind), sourceKind),
					))
				sel.Where(entsql.Exists(sub))
			}),
		).
		SetState(store.TriggerSubjectClaimStateReleased).
		SetReleasedAt(releasedAt.UTC().Truncate(time.Microsecond))
	if reason != "" {
		upd = upd.SetReleaseReason(reason)
	}
	affected, err := upd.Save(ctx)
	if err != nil {
		return 0, mapError(err)
	}
	return affected, nil
}

// terminalClaimReleaseGrace is the minimum age a bound claim must have before
// hourly maintenance will release it for a missing/terminal agent. Under
// PostgreSQL READ COMMITTED, EvalPlanQual re-checks the UPDATE predicate
// against the latest row version while the NOT EXISTS subplan still reads
// agents from the statement's older snapshot; requiring bound_at < now - 1m
// prevents maintenance from releasing a claim that was just re-bound to a
// newly inserted agent row invisible to the older snapshot.
const terminalClaimReleaseGrace = time.Minute

// ReleaseTerminalTriggerSubjectClaims releases:
//  1. Every active bound claim (bound at least terminalClaimReleaseGrace ago)
//     whose agent is missing, soft-deleted (deleted_at IS NOT NULL), or in a
//     terminal phase (stopped, error, halted).
//  2. Every active abandoned unbound claim (agent_id IS NULL AND
//     claimed_at < now - TriggerSubjectAbandonedClaimTTL), so subsequent
//     maintenance purge passes can delete it. Fresh unbound claims younger than
//     the TTL are left untouched so in-flight creates are not disturbed.
func (s *TriggerStore) ReleaseTerminalTriggerSubjectClaims(ctx context.Context, now time.Time) (int, error) {
	now = now.UTC().Truncate(time.Microsecond)
	boundBefore := now.Add(-terminalClaimReleaseGrace)
	abandonedBefore := now.Add(-store.TriggerSubjectAbandonedClaimTTL)
	affected, err := s.client.TriggerSubjectClaim.Update().
		Where(
			triggersubjectclaim.StateEQ(store.TriggerSubjectClaimStateActive),
			triggersubjectclaim.Or(
				triggersubjectclaim.And(
					triggersubjectclaim.AgentIDIsNil(),
					triggersubjectclaim.ClaimedAtLT(abandonedBefore),
				),
				triggersubjectclaim.And(
					triggersubjectclaim.AgentIDNotNil(),
					triggersubjectclaim.BoundAtLT(boundBefore),
					predicate.TriggerSubjectClaim(func(sel *entsql.Selector) {
						d := sel.Dialect()
						ag := entsql.Dialect(d).Table(agent.Table).As("ag")
						liveSub := entsql.Dialect(d).Select(ag.C(agent.FieldID)).
							From(ag).
							Where(entsql.And(
								entsql.ColumnsEQ(ag.C(agent.FieldID), sel.C(triggersubjectclaim.FieldAgentID)),
								entsql.IsNull(ag.C(agent.FieldDeletedAt)),
								entsql.NotIn(
									ag.C(agent.FieldPhase),
									string(state.PhaseStopped),
									string(state.PhaseError),
									string(state.PhaseHalted),
								),
							))
						sel.Where(entsql.Not(entsql.Exists(liveSub)))
					}),
				),
			),
		).
		SetState(store.TriggerSubjectClaimStateReleased).
		SetReleasedAt(now).
		Save(ctx)
	if err != nil {
		return 0, mapError(err)
	}
	return affected, nil
}

// PurgeOldTriggerSubjectClaims deletes released claim rows whose released_at
// is strictly before olderThan.
func (s *TriggerStore) PurgeOldTriggerSubjectClaims(ctx context.Context, olderThan time.Time) (int, error) {
	cutoff := olderThan.UTC().Truncate(time.Microsecond)
	deleted, err := s.client.TriggerSubjectClaim.Delete().
		Where(
			triggersubjectclaim.StateEQ(store.TriggerSubjectClaimStateReleased),
			triggersubjectclaim.ReleasedAtLT(cutoff),
		).
		Exec(ctx)
	if err != nil {
		return 0, mapError(err)
	}
	return deleted, nil
}
