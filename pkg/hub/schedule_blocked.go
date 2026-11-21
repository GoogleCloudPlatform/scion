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
	"log/slog"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// NotificationScheduleBlocked is the notification status for a scheduled
// dispatch_agent fire blocked by an existing agent row in phase error
// (ptone/scion#3701). One is created per blocked fire, not deduplicated.
const NotificationScheduleBlocked = "SCHEDULE_BLOCKED"

// isScheduleBlockedPlacementRefusal reports whether existing's delete would be
// refused for an inconsistent saved placement (ptone/scion#4844): its failed
// delete already recorded placement_conflict, its create-failure cleanup
// recorded a placement refusal, or its current saved placement fails the
// delete placement check.
func (s *Server) isScheduleBlockedPlacementRefusal(ctx context.Context, existing *store.Agent) bool {
	if existing == nil {
		return false
	}
	if existing.DeletionEffectiveCode(time.Now()) == store.DeletionCodePlacementConflict {
		return true
	}
	if isCreateCleanupRefusedRow(existing) && strings.Contains(existing.Message, inconsistentPlacementDocsURL) {
		return true
	}
	if s != nil && s.store != nil {
		if _, ok := placementRefusalOf(agentDeletePlacementCheck(ctx, s.store, existing)); ok {
			return true
		}
	}
	return false
}

// scheduleBlockedAgentError is the typed error returned by scheduleBlockedError
// when a dispatch_agent fire is blocked by an existing agent row in phase error
// (ptone/scion#3701). Typing it lets serverTriggerCore.Fire preserve its
// actionable remedy text without substring matching on arbitrary errors.
type scheduleBlockedAgentError struct {
	msg string
}

func (e *scheduleBlockedAgentError) Error() string {
	return e.msg
}

// scheduleBlockedError is a dispatch_agent fire's error when an agent row in
// phase error already holds the name. Deleting the row is the only remedy
// unless the row's saved placement is inconsistent (where ordinary and force
// delete also refuse it, ptone/scion#4844): the hub neither retries nor
// removes it. A row left by a refused create-failure cleanup has its message
// quoted. One-shot events get the same text (lead ruling on ptone/scion#3701).
func scheduleBlockedError(existing *store.Agent, placementRefused bool) error {
	detail := ""
	if isCreateCleanupRefusedRow(existing) {
		detail = " (" + existing.Message + ")"
	}
	if placementRefused {
		if detail != "" {
			return &scheduleBlockedAgentError{
				msg: fmt.Sprintf("agent %q already exists in project in phase error%s; ordinary and force delete cannot remove this agent while its saved placement is inconsistent",
					existing.Slug, detail),
			}
		}
		return &scheduleBlockedAgentError{
			msg: fmt.Sprintf("agent %q already exists in project in phase error; its saved placement is inconsistent, so ordinary and force delete cannot remove it (see %s)",
				existing.Slug, inconsistentPlacementDocsURL),
		}
	}
	return &scheduleBlockedAgentError{
		msg: fmt.Sprintf("agent %q already exists in project in phase error%s; delete the agent to resume this schedule",
			existing.Slug, detail),
	}
}

// notifyFireBlocked creates a SCHEDULE_BLOCKED notification for the owner of
// a recurring schedule fire (scheduleNotificationRecipient) blocked by an
// existing agent row in phase error (ptone/scion#3701) and publishes it on
// that user's subject only. Only recurring-schedule fires notify; a one-shot
// event (no ScheduleID) or Stage 1 trigger records its error and nothing else
// (lead ruling on ptone/scion#3701). Every failure is logged and skipped: it
// never changes the fire's result.
func (s *Server) notifyFireBlocked(ctx context.Context, req hubfire.FireRequest, existing *store.Agent, placementRefused bool) {
	if req.Authority.DefinitionKind != hubfire.DefinitionKindSchedule || req.Authority.DefinitionID == "" {
		return
	}
	scheduleID := req.Authority.DefinitionID
	createdBy := fireHistoryCreatorFromContext(ctx)
	log := slog.With("subsystem", "scheduler", "eventID", req.Authority.FireID,
		"scheduleID", scheduleID, "projectID", req.ProjectID, "agentID", existing.ID)
	recipient := s.scheduleNotificationRecipient(ctx, createdBy)
	if recipient == "" {
		log.Warn("Scheduler: blocked fire has no owning user to notify; skipping notification",
			"createdBy", createdBy)
		return
	}

	name := scheduleID
	if sched, err := s.store.GetSchedule(ctx, scheduleID); err == nil && sched.Name != "" {
		name = sched.Name
	}
	message := fmt.Sprintf("Schedule %q is blocked: agent %q is in phase error. Delete the agent to resume this schedule.",
		name, existing.Slug)
	if placementRefused {
		message = fmt.Sprintf("Schedule %q is blocked: agent %q is in phase error with an inconsistent saved placement. Ordinary and force delete cannot remove this agent; see %s.",
			name, existing.Slug, inconsistentPlacementDocsURL)
	}

	projectID := req.ProjectID
	if projectID == "" {
		projectID = uuid.Nil.String()
	}
	notif := &store.Notification{
		ID:             api.NewUUID(),
		SubscriptionID: uuid.Nil.String(),
		// The errored row, so the tray's "View agent" link opens the agent
		// to delete.
		AgentID:        existing.ID,
		ProjectID:      projectID,
		SubscriberType: store.SubscriberTypeUser,
		SubscriberID:   recipient,
		Status:         NotificationScheduleBlocked,
		Message:        message,
		CreatedAt:      time.Now(),
	}
	if err := s.store.CreateNotification(ctx, notif); err != nil {
		log.Error("Scheduler: failed to create schedule-blocked notification",
			"recipient", recipient, "error", err)
		return
	}
	if s.events != nil {
		s.events.PublishUserNotification(ctx, notif)
	}
	log.Info("Scheduler: schedule-blocked notification created",
		"notificationID", notif.ID, "recipient", recipient)
}

// scheduleNotificationRecipient resolves a schedule's CreatedBy to the user
// its notifications go to: the user itself, or for an agent creator the
// agent's owning user (its ancestry root, else its owner) when that is a
// user row. It returns "" when there is none. CreatedBy is only used to
// address the notification, never for authority.
func (s *Server) scheduleNotificationRecipient(ctx context.Context, createdBy string) string {
	if createdBy == "" {
		return ""
	}
	if _, err := s.store.GetUser(ctx, createdBy); err == nil {
		return createdBy
	} else if !errors.Is(err, store.ErrNotFound) {
		// A store failure, not a missing user: say so, then skip as for
		// no owning user.
		slog.Warn("Scheduler: looking up the schedule creator as a user failed; skipping notification",
			"subsystem", "scheduler", "createdBy", createdBy, "error", err)
		return ""
	}
	agent, err := s.store.GetAgent(ctx, createdBy)
	if err != nil {
		return ""
	}
	candidates := []string{}
	if len(agent.Ancestry) > 0 {
		candidates = append(candidates, agent.Ancestry[0])
	}
	candidates = append(candidates, agent.OwnerID)
	for _, id := range candidates {
		if id == "" || id == agent.ID {
			continue
		}
		if _, err := s.store.GetUser(ctx, id); err == nil {
			return id
		}
	}
	return ""
}
