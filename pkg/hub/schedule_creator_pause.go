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
// (InitiatorAttribution). When that authority is an agent and the agent is
// deleted, every fire would be refused, so the schedule is paused with
// status reason creator_deleted instead (ptone/scion#5403). Resuming
// re-records the authority to the resumer and clears the reason.
//
// Two paths pause such a schedule: the agent delete engine pauses the
// schedules of the agent it deleted (pauseSchedulesAuthoredByAgent), and
// the schedule evaluator pauses one whose agent is gone when it comes due
// (executeSchedule, via scheduleAuthorityAgentGone), for any delete path
// that did not.

// scheduleCreatorPausePageSize is the page size used to scan a project's
// active schedules for those authored by a deleted agent.
const scheduleCreatorPausePageSize = 200

// scheduleAuthorityAgentID returns the agent ID the schedule's recorded
// authority resolves to, or "" when the authority is not an agent (a user
// principal, or legacy attribution with no recorded principal).
func (s *Server) scheduleAuthorityAgentID(sched store.Schedule) string {
	initiator := s.scheduledInitiator(sched.InitiatorAttribution)
	if initiator.LegacyUnknown || initiator.PrincipalKind != store.DelegationPrincipalAgent {
		return ""
	}
	return initiator.PrincipalID
}

// scheduleAuthorityAgentGone reports whether the schedule's authority is an
// agent that no longer exists or is soft-deleted. A lookup error is
// returned as is; the caller then fires as usual and fire-time authority
// decides.
func (s *Server) scheduleAuthorityAgentGone(ctx context.Context, sched store.Schedule) (bool, error) {
	agentID := s.scheduleAuthorityAgentID(sched)
	if agentID == "" {
		return false, nil
	}
	agent, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return true, nil
		}
		return false, err
	}
	return agent == nil || !agent.DeletedAt.IsZero(), nil
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
	return s.store.UpdateSchedule(ctx, &sched, store.ScheduleFieldMask{Status: true},
		prevRevision, prevRevisionKnown, nil)
}

// pauseSchedulesAuthoredByAgent pauses, with status reason creator_deleted,
// every active schedule in the agent's project whose recorded authority is
// the agent. It is best effort: failures are logged, and the evaluator
// backstop pauses any schedule left active when it comes due.
func (s *Server) pauseSchedulesAuthoredByAgent(ctx context.Context, agent *store.Agent) {
	if agent == nil || agent.ID == "" || agent.ProjectID == "" {
		return
	}
	log := s.agentLifecycleLog.With("agent_id", agent.ID, "project_id", agent.ProjectID)

	// Collect first, then pause: pausing removes rows from the active set
	// being paged.
	var matches []store.Schedule
	opts := store.ListOptions{Limit: scheduleCreatorPausePageSize, SkipTotalCount: true}
	for {
		page, err := s.store.ListSchedules(ctx, store.ScheduleFilter{
			ProjectID: agent.ProjectID,
			Status:    store.ScheduleStatusActive,
		}, opts)
		if err != nil {
			log.Warn("Failed to list schedules authored by deleted agent", "error", err)
			break
		}
		for _, sched := range page.Items {
			if s.scheduleAuthorityAgentID(sched) == agent.ID {
				matches = append(matches, sched)
			}
		}
		if page.NextCursor == "" || page.NextCursor == opts.Cursor {
			break
		}
		opts.Cursor = page.NextCursor
	}

	paused := 0
	for _, sched := range matches {
		err := s.pauseScheduleWithReason(ctx, sched, store.ScheduleStatusReasonCreatorDeleted)
		switch {
		case err == nil:
			paused++
		case errors.Is(err, store.ErrRevisionConflict), errors.Is(err, store.ErrNotFound):
			// Resumed (re-attributed) or deleted since it was read.
		default:
			log.Warn("Failed to pause schedule authored by deleted agent",
				"schedule_id", sched.ID, "error", err)
		}
	}
	if paused > 0 {
		log.Info("Paused schedules authored by deleted agent",
			"paused", paused, "status_reason", store.ScheduleStatusReasonCreatorDeleted)
	}
}

// pauseScheduleIfAuthorityAgentGone is the evaluator backstop: it pauses a
// due schedule whose authority agent is gone, with status reason
// creator_deleted. It returns true when the schedule must not fire this tick:
// it was paused, or it changed since it was listed.
func (s *Server) pauseScheduleIfAuthorityAgentGone(ctx context.Context, sched store.Schedule, log *slog.Logger) bool {
	gone, err := s.scheduleAuthorityAgentGone(ctx, sched)
	if err != nil {
		log.Warn("schedule-evaluator: authority agent lookup failed; firing as usual", "error", err)
		return false
	}
	if !gone {
		return false
	}
	err = s.pauseScheduleWithReason(ctx, sched, store.ScheduleStatusReasonCreatorDeleted)
	switch {
	case err == nil:
		log.Warn("schedule paused: the agent its authority resolves to was deleted; resume it to re-record authority",
			"status_reason", store.ScheduleStatusReasonCreatorDeleted)
	case errors.Is(err, store.ErrRevisionConflict), errors.Is(err, store.ErrNotFound):
		// Resumed (re-attributed) or deleted since it was listed: skip
		// this tick; the next one sees the current row.
		log.Info("schedule-evaluator: schedule changed since listed; skipping this tick", "error", err)
	default:
		// Fall back to firing: the fire is refused and recorded as today,
		// and next_run_at advances, so the pause is retried next run
		// rather than every tick.
		log.Error("schedule-evaluator: failed to pause schedule whose authority agent was deleted", "error", err)
		return false
	}
	return true
}
