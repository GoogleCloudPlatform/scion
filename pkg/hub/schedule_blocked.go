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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// NotificationScheduleBlocked is the notification status for a scheduled
// dispatch_agent fire blocked by an existing agent row in phase error
// (ptone/scion#3701). One is created per blocked fire, not deduplicated.
const NotificationScheduleBlocked = "SCHEDULE_BLOCKED"

// scheduleBlockedError is a dispatch_agent fire's error when an agent row in
// phase error already holds the name. Deleting the row is the only remedy:
// the hub neither retries nor removes it. A row left by a refused
// create-failure cleanup has its message quoted. One-shot events get the same
// text (lead ruling on ptone/scion#3701).
func scheduleBlockedError(existing *store.Agent) error {
	detail := ""
	if isCreateCleanupRefusedRow(existing) {
		detail = " (" + existing.Message + ")"
	}
	return fmt.Errorf("agent %q already exists in project in phase error%s; delete the agent to resume this schedule",
		existing.Slug, detail)
}

// notifyScheduleBlocked creates a SCHEDULE_BLOCKED notification for the
// owner of evt (scheduleNotificationRecipient) and publishes it on that
// user's subject only. Only recurring-schedule fires notify; a one-shot
// event (no ScheduleID) records its error and nothing else (lead ruling on
// ptone/scion#3701). Every failure is logged and skipped: it never changes
// the fire's result.
func (s *Server) notifyScheduleBlocked(ctx context.Context, evt store.ScheduledEvent, existing *store.Agent) {
	if evt.ScheduleID == "" {
		return
	}
	log := slog.With("subsystem", "scheduler", "eventID", evt.ID,
		"scheduleID", evt.ScheduleID, "projectID", evt.ProjectID, "agentID", existing.ID)
	recipient := s.scheduleNotificationRecipient(ctx, evt.CreatedBy)
	if recipient == "" {
		log.Warn("Scheduler: blocked fire has no owning user to notify; skipping notification",
			"createdBy", evt.CreatedBy)
		return
	}

	name := evt.ScheduleID
	if sched, err := s.store.GetSchedule(ctx, evt.ScheduleID); err == nil && sched.Name != "" {
		name = sched.Name
	}
	message := fmt.Sprintf("Schedule %q is blocked: agent %q is in phase error. Delete the agent to resume this schedule.",
		name, existing.Slug)

	projectID := evt.ProjectID
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
