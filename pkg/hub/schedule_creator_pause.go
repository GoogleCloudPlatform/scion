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
	"log/slog"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// A recurring schedule runs under the authority recorded on it
// (InitiatorAttribution): the last authorizing mutator, not CreatedBy. When
// that authority is an agent that is deleted, or a user that is deleted or
// being deleted, every fire would be refused, so the schedule is paused
// with status reason creator_deleted instead (ptone/scion#5403,
// ptone/scion#2143). Resuming re-records the authority to the resumer and
// clears the reason.
//
// Three paths pause such a schedule: the agent delete engine pauses the
// schedules of the agent it deleted (pauseSchedulesAuthoredByAgent), a user
// delete pauses the schedules of the user (pauseSchedulesAuthoredByUser,
// after a plain delete and after a forced delete's revoke), and the
// schedule evaluator pauses one whose authority is gone when it comes due
// (executeSchedule, via scheduleAuthorityGone), for any delete path that
// did not.

// scheduleCreatorPausePageSize is the page size for scanning the active
// schedules whose authority is a deleted principal.
const scheduleCreatorPausePageSize = 200

// scheduleAuthorityPrincipal returns the principal the schedule's recorded
// authority resolves to, or the zero value for legacy attribution with no
// recorded principal.
func (s *Server) scheduleAuthorityPrincipal(sched store.Schedule) store.ScheduleAuthorityPrincipal {
	initiator := s.scheduledInitiator(sched.InitiatorAttribution)
	if initiator.LegacyUnknown || initiator.PrincipalID == "" {
		return store.ScheduleAuthorityPrincipal{}
	}
	return store.ScheduleAuthorityPrincipal{Kind: initiator.PrincipalKind, ID: initiator.PrincipalID}
}

// scheduleAuthorityGone reports whether the schedule's authority is an
// agent whose row is gone or soft-deleted, or a user whose row is gone or
// whose status is deleting. Any other authority (a live
// principal, a suspended user, legacy attribution) is not gone: the fire
// goes ahead and fire-time authority decides. A lookup error is returned as
// is; the caller then fires as usual.
func (s *Server) scheduleAuthorityGone(ctx context.Context, sched store.Schedule) (bool, error) {
	p := s.scheduleAuthorityPrincipal(sched)
	switch p.Kind {
	case store.DelegationPrincipalAgent:
		agent, err := s.store.GetAgent(ctx, p.ID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return true, nil
			}
			return false, err
		}
		return agent == nil || !agent.DeletedAt.IsZero(), nil
	case store.DelegationPrincipalUser:
		user, err := s.store.GetUser(ctx, p.ID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return true, nil
			}
			return false, err
		}
		return user == nil || user.Status == store.UserStatusDeleting, nil
	}
	return false, nil
}

// pauseScheduleWithReason pauses sched with a system status reason. The
// write is conditioned on the authorization revision sched was read with,
// so it does not land once a resume (which re-records the authority and
// bumps the revision) has landed first; that case returns
// store.ErrRevisionConflict.
func (s *Server) pauseScheduleWithReason(ctx context.Context, sched store.Schedule, reason string) error {
	prevRevision := sched.AuthorizationRevision
	prevRevisionKnown := sched.AuthorizationRevision != 0
	sched.Status = store.ScheduleStatusPaused
	sched.StatusReason = reason
	fields := store.ScheduleFieldMask{Status: true}
	return s.store.UpdateSchedule(ctx, &sched, fields, prevRevision, prevRevisionKnown, nil)
}

// pauseSchedulesAuthoredByAgent pauses, with status reason creator_deleted,
// every active schedule in the agent's project whose recorded authority is
// the agent. It is best effort: failures are logged, and the evaluator
// backstop pauses any schedule left active when it comes due.
func (s *Server) pauseSchedulesAuthoredByAgent(ctx context.Context, agent *store.Agent) {
	if agent == nil || agent.ID == "" || agent.ProjectID == "" {
		return
	}
	s.pauseSchedulesWithAuthority(ctx, agent.ProjectID,
		store.ScheduleAuthorityPrincipal{Kind: store.DelegationPrincipalAgent, ID: agent.ID},
		s.agentLifecycleLog.With("agent_id", agent.ID, "project_id", agent.ProjectID))
}

// pauseSchedulesAuthoredByUser pauses, with status reason creator_deleted,
// every active schedule in any project whose recorded authority is the
// user (ptone/scion#2143): the schedules do not block the user's
// delete. It runs after a plain user delete commits (afterUserDeleted) and
// after a forced delete's revoke commits. It is best effort: failures are
// logged, and the evaluator backstop pauses any schedule left active when
// it comes due.
func (s *Server) pauseSchedulesAuthoredByUser(ctx context.Context, userID string) {
	if userID == "" {
		return
	}
	s.pauseSchedulesWithAuthority(ctx, "",
		store.ScheduleAuthorityPrincipal{Kind: store.DelegationPrincipalUser, ID: userID},
		slog.Default().With("subsystem", "scheduler", "user_id", userID))
}

// pauseSchedulesWithAuthority pauses, with status reason creator_deleted,
// every active schedule whose recorded authority is principal, in projectID
// or, when projectID is empty, in every project. Each listed row is checked
// again against principal before it is paused. A schedule resumed (and so
// re-attributed) or deleted since it was listed is skipped.
func (s *Server) pauseSchedulesWithAuthority(ctx context.Context, projectID string, principal store.ScheduleAuthorityPrincipal, log *slog.Logger) {
	log = log.With("principal_kind", principal.Kind)

	// Collect first, then pause: pausing removes rows from the active set
	// being paged.
	var matches []store.Schedule
	opts := store.ListOptions{Limit: scheduleCreatorPausePageSize, SkipTotalCount: true}
	for {
		page, err := s.store.ListSchedules(ctx, store.ScheduleFilter{
			ProjectID:          projectID,
			Status:             store.ScheduleStatusActive,
			AuthorityPrincipal: principal,
		}, opts)
		if err != nil {
			log.Warn("Failed to list schedules whose authority is a deleted principal", "error", err)
			break
		}
		for _, sched := range page.Items {
			if s.scheduleAuthorityPrincipal(sched) == principal {
				matches = append(matches, sched)
			}
		}
		if page.NextCursor == "" || page.NextCursor == opts.Cursor {
			break
		}
		opts.Cursor = page.NextCursor
	}

	paused := 0
	for i, sched := range matches {
		// Stop once the step's context is done (cancelled, or past the
		// step timeout): every further write would fail and log. The
		// schedules left active are paused by the evaluator backstop
		// (pauseScheduleIfAuthorityGone) when they come due.
		if err := ctx.Err(); err != nil {
			log.Warn("Stopped pausing schedules whose authority is a deleted principal: context done",
				"unpaused", len(matches)-i, "error", err)
			break
		}
		err := s.pauseScheduleWithReason(ctx, sched, store.ScheduleStatusReasonCreatorDeleted)
		switch {
		case err == nil:
			paused++
		case errors.Is(err, store.ErrRevisionConflict), errors.Is(err, store.ErrNotFound):
			// Resumed (re-attributed) or deleted since it was read.
		default:
			log.Warn("Failed to pause schedule whose authority is a deleted principal",
				"schedule_id", sched.ID, "error", err)
		}
	}
	if paused > 0 {
		log.Info("Paused schedules whose authority is a deleted principal",
			"paused", paused, "status_reason", store.ScheduleStatusReasonCreatorDeleted)
	}
}

// pauseScheduleIfAuthorityGone is the evaluator backstop: it pauses a due
// schedule whose authority is gone (scheduleAuthorityGone), with status
// reason creator_deleted. It returns true when the schedule must not fire
// this tick: it was paused, or it changed since it was listed.
func (s *Server) pauseScheduleIfAuthorityGone(ctx context.Context, sched store.Schedule, log *slog.Logger) bool {
	gone, err := s.scheduleAuthorityGone(ctx, sched)
	if err != nil {
		log.Warn("schedule-evaluator: authority principal lookup failed; firing as usual", "error", err)
		return false
	}
	if !gone {
		return false
	}
	err = s.pauseScheduleWithReason(ctx, sched, store.ScheduleStatusReasonCreatorDeleted)
	switch {
	case err == nil:
		log.Warn("schedule paused: the principal its authority resolves to was deleted; resume it to re-record authority",
			"status_reason", store.ScheduleStatusReasonCreatorDeleted)
	case errors.Is(err, store.ErrRevisionConflict), errors.Is(err, store.ErrNotFound):
		// Resumed (re-attributed) or deleted since it was listed: skip
		// this tick; the next one sees the current row.
		log.Info("schedule-evaluator: schedule changed since listed; skipping this tick", "error", err)
	default:
		// Fall back to firing: the fire is refused and recorded as today,
		// and next_run_at advances, so the pause is retried next run
		// rather than every tick.
		log.Error("schedule-evaluator: failed to pause schedule whose authority principal was deleted", "error", err)
		return false
	}
	return true
}
