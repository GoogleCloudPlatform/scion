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

	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Pending secret notifications. When an agent saves a user-scope secret
// that waits for approval, the owner gets an in-app notification of kind
// store.NotificationKindSecretPending, stored and published on the owner's
// own subject like the artifact review summary. At most one is created per
// (owner, writing agent) per pendingSecretNotifyInterval; a write inside
// that window adds no notification, and the next one reports how many of
// that agent's secrets await approval and how many of those replace an
// existing secret. A write that would replace an approved secret always
// notifies, outside that window, at most once per (owner, writing agent,
// key) per interval, so no replacement goes unannounced. The text names
// the agent, its project, the key, its type and the time; never the value.

// pendingSecretNotifyInterval is the minimum time between two pending
// secret notifications for the same owner and writing agent.
const pendingSecretNotifyInterval = time.Hour

// pendingSecretNotifyLabelMaxRunes caps each agent-chosen name (agent slug,
// project name, key) in the message, ellipsis included.
const pendingSecretNotifyLabelMaxRunes = 64

// pendingSecretNotifyNamespace derives the notification id from (owner,
// writing agent, hour), so two writes racing in the same hour store one
// notification.
var pendingSecretNotifyNamespace = uuid.MustParse("6f1e3c0a-4b52-4d1e-9a39-2f6a8f0d5c11")

// notifyPendingSecret notifies the owner of pending secret p, unless a
// notification for the same owner and writing agent was created within
// pendingSecretNotifyInterval. Every failure is logged and skipped: it
// never changes the write's result.
func (s *Server) notifyPendingSecret(ctx context.Context, p *store.PendingSecret) {
	if p == nil || p.Scope != store.ScopeUser || p.ScopeID == "" {
		return
	}
	now := time.Now()
	log := slog.With("pending_id", p.ID)

	// A replacement of an approved secret is announced whatever the
	// agent's hourly window says; its notification id is per key and hour.
	if p.ReplacesSecretID == "" {
		recent, err := s.store.GetNotificationsByAgent(ctx, p.AgentID, store.SubscriberTypeUser, p.ScopeID, false)
		if err != nil {
			log.WarnContext(ctx, "pending secret: notification skipped: listing notifications failed", "error", err)
			return
		}
		for _, n := range recent {
			if n.Kind == store.NotificationKindSecretPending && now.Sub(n.CreatedAt) < pendingSecretNotifyInterval {
				return
			}
		}
	}

	counts := pendingSecretCounts{waiting: 1}
	if p.ReplacesSecretID != "" {
		counts.replacing = 1
	}
	if rows, err := s.store.ListPendingSecretsByOwner(ctx, store.ScopeUser, p.ScopeID); err == nil {
		var c pendingSecretCounts
		for _, row := range rows {
			if row.AgentID == p.AgentID && !row.Expired(now) {
				c.waiting++
				if row.ReplacesSecretID != "" {
					c.replacing++
				}
			}
		}
		if c.waiting > 0 {
			counts = c
		}
	}

	notif := buildPendingSecretNotification(p, counts, now)
	if p.ReplacesSecretID != "" {
		if _, err := s.store.GetNotification(ctx, notif.ID); err == nil {
			return // this key's replacement was announced within the hour
		}
	}
	if err := s.store.CreateNotification(ctx, notif); err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			return // a concurrent write in the same hour stored it
		}
		log.ErrorContext(ctx, "pending secret: notification not stored", "error", err)
		return
	}
	if s.events != nil {
		s.events.PublishUserNotification(ctx, notif)
	}
}

// pendingSecretCounts are a writing agent's live pending secrets for one
// owner: how many await approval, and how many of those would replace an
// approved secret.
type pendingSecretCounts struct {
	waiting   int
	replacing int
}

// buildPendingSecretNotification is the notification for pending secret
// p, created at now, with the writing agent's counts. The agent-chosen
// names are cleaned. A replacement of an approved secret says so and gets
// an id per key and hour; any other write gets an id per agent and hour.
func buildPendingSecretNotification(p *store.PendingSecret, counts pendingSecretCounts, now time.Time) *store.Notification {
	slug := cleanNotificationText(p.AgentSlug, pendingSecretNotifyLabelMaxRunes)
	if slug == "" {
		slug = p.AgentID
	}
	project := cleanNotificationText(p.ProjectName, pendingSecretNotifyLabelMaxRunes)
	if project == "" {
		project = p.ProjectID
	}
	key := cleanNotificationText(p.Key, pendingSecretNotifyLabelMaxRunes)
	at := now.UTC().Format(time.RFC3339)
	bucket := now.UTC().Truncate(pendingSecretNotifyInterval).Unix()

	var message, idName string
	if p.ReplacesSecretID != "" {
		message = fmt.Sprintf("Agent %s in project %s saved a replacement for your existing secret %s (%s) at %s. "+
			"Your current value stays in effect until you approve it. ", slug, project, key, p.SecretType, at)
		idName = fmt.Sprintf("%s|%s|replace|%s|%d", p.ScopeID, p.AgentID, p.Key, bucket)
	} else {
		message = fmt.Sprintf("Agent %s in project %s saved secret %s (%s) for your account at %s. ",
			slug, project, key, p.SecretType, at)
		idName = fmt.Sprintf("%s|%s|%d", p.ScopeID, p.AgentID, bucket)
	}
	if counts.waiting > 1 {
		message += fmt.Sprintf("%d secrets from this agent await your approval", counts.waiting)
		switch {
		case counts.replacing == 1:
			message += ", 1 of them replaces an existing secret"
		case counts.replacing > 1:
			message += fmt.Sprintf(", %d of them replace existing secrets", counts.replacing)
		}
		message += ". "
	}
	message += "Approve or reject in Profile > Secrets."
	id := uuid.NewSHA1(pendingSecretNotifyNamespace, []byte(idName)).String()

	projectID := uuid.Nil.String()
	if pid, err := uuid.Parse(p.ProjectID); err == nil && pid.String() == p.ProjectID {
		projectID = p.ProjectID
	}
	agentID := uuid.Nil.String()
	if aid, err := uuid.Parse(p.AgentID); err == nil && aid.String() == p.AgentID {
		agentID = p.AgentID
	}
	return &store.Notification{
		ID:             id,
		SubscriptionID: store.CreatorFallbackSubscriptionID,
		AgentID:        agentID,
		ProjectID:      projectID,
		SubscriberType: store.SubscriberTypeUser,
		SubscriberID:   p.ScopeID,
		Status:         store.NotificationStatusSecretPending,
		Message:        message,
		CreatedAt:      now,
		Kind:           store.NotificationKindSecretPending,
		ResourceType:   store.NotificationResourcePendingSecret,
		ResourceID:     p.ID,
	}
}
