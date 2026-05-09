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

// Package sqlite provides a SQLite implementation of the Store interface.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ============================================================================
// Notification Subscription Operations
// ============================================================================

// CreateNotificationSubscription creates a new notification subscription.
func (s *SQLiteStore) CreateNotificationSubscription(ctx context.Context, sub *store.NotificationSubscription) error {
	if sub.ID == "" || sub.SubscriberID == "" || sub.ProjectID == "" {
		return store.ErrInvalidInput
	}

	// Default scope to agent for backward compatibility
	if sub.Scope == "" {
		sub.Scope = store.SubscriptionScopeAgent
	}

	// Validate scope-specific constraints
	switch sub.Scope {
	case store.SubscriptionScopeAgent:
		if sub.AgentID == "" {
			return store.ErrInvalidInput
		}
	case store.SubscriptionScopeProject:
		sub.AgentID = "" // Ensure no agent_id for project-scoped
	default:
		return fmt.Errorf("invalid scope %q: %w", sub.Scope, store.ErrInvalidInput)
	}

	now := time.Now()
	if sub.CreatedAt.IsZero() {
		sub.CreatedAt = now
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO notification_subscriptions (
			id, scope, agent_id, subscriber_type, subscriber_id, project_id,
			trigger_activities, created_at, created_by
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		sub.ID, sub.Scope, nullableString(sub.AgentID), sub.SubscriberType, sub.SubscriberID, sub.ProjectID,
		marshalJSON(sub.TriggerActivities), sub.CreatedAt, sub.CreatedBy,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return store.ErrAlreadyExists
		}
		if strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
			return fmt.Errorf("agent %s does not exist: %w", sub.AgentID, store.ErrInvalidInput)
		}
		return err
	}
	return nil
}

// GetNotificationSubscription returns a single subscription by ID.
func (s *SQLiteStore) GetNotificationSubscription(ctx context.Context, id string) (*store.NotificationSubscription, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, scope, agent_id, subscriber_type, subscriber_id, project_id,
			trigger_activities, created_at, created_by
		FROM notification_subscriptions
		WHERE id = ?
	`, id)

	var sub store.NotificationSubscription
	var agentID sql.NullString
	var triggerActivitiesJSON string

	if err := row.Scan(
		&sub.ID, &sub.Scope, &agentID, &sub.SubscriberType, &sub.SubscriberID, &sub.ProjectID,
		&triggerActivitiesJSON, &sub.CreatedAt, &sub.CreatedBy,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}

	if agentID.Valid {
		sub.AgentID = agentID.String
	}
	unmarshalJSON(triggerActivitiesJSON, &sub.TriggerActivities)
	return &sub, nil
}

// GetNotificationSubscriptions returns all agent-scoped subscriptions for a watched agent.
func (s *SQLiteStore) GetNotificationSubscriptions(ctx context.Context, agentID string) ([]store.NotificationSubscription, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, scope, agent_id, subscriber_type, subscriber_id, project_id,
			trigger_activities, created_at, created_by
		FROM notification_subscriptions
		WHERE agent_id = ?
		ORDER BY created_at ASC
	`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanSubscriptions(rows)
}

// GetNotificationSubscriptionsByProject returns all subscriptions within a project (any scope).
func (s *SQLiteStore) GetNotificationSubscriptionsByProject(ctx context.Context, projectID string) ([]store.NotificationSubscription, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, scope, agent_id, subscriber_type, subscriber_id, project_id,
			trigger_activities, created_at, created_by
		FROM notification_subscriptions
		WHERE project_id = ?
		ORDER BY created_at ASC
	`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanSubscriptions(rows)
}

// GetNotificationSubscriptionsByProjectScope returns project-scoped subscriptions
// (scope='project') for a given project.
func (s *SQLiteStore) GetNotificationSubscriptionsByProjectScope(ctx context.Context, projectID string) ([]store.NotificationSubscription, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, scope, agent_id, subscriber_type, subscriber_id, project_id,
			trigger_activities, created_at, created_by
		FROM notification_subscriptions
		WHERE project_id = ? AND scope = 'project'
		ORDER BY created_at ASC
	`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanSubscriptions(rows)
}

// GetSubscriptionsForSubscriber returns all subscriptions owned by a subscriber.
func (s *SQLiteStore) GetSubscriptionsForSubscriber(ctx context.Context, subscriberType, subscriberID string) ([]store.NotificationSubscription, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, scope, agent_id, subscriber_type, subscriber_id, project_id,
			trigger_activities, created_at, created_by
		FROM notification_subscriptions
		WHERE subscriber_type = ? AND subscriber_id = ?
		ORDER BY created_at ASC
	`, subscriberType, subscriberID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanSubscriptions(rows)
}

// UpdateNotificationSubscriptionTriggers updates the trigger activities of a subscription.
func (s *SQLiteStore) UpdateNotificationSubscriptionTriggers(ctx context.Context, id string, triggerActivities []string) error {
	if id == "" || len(triggerActivities) == 0 {
		return store.ErrInvalidInput
	}

	result, err := s.db.ExecContext(ctx, `
		UPDATE notification_subscriptions SET trigger_activities = ? WHERE id = ?
	`, marshalJSON(triggerActivities), id)
	if err != nil {
		return err
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return store.ErrNotFound
	}
	return nil
}

// DeleteNotificationSubscription deletes a subscription by ID.
func (s *SQLiteStore) DeleteNotificationSubscription(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `
		DELETE FROM notification_subscriptions WHERE id = ?
	`, id)
	if err != nil {
		return err
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return store.ErrNotFound
	}
	return nil
}

// DeleteNotificationSubscriptionsForAgent deletes all subscriptions for a watched agent.
// No error on zero rows affected.
func (s *SQLiteStore) DeleteNotificationSubscriptionsForAgent(ctx context.Context, agentID string) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM notification_subscriptions WHERE agent_id = ?
	`, agentID)
	return err
}

// ============================================================================
// Notification Operations
// ============================================================================

// CreateNotification creates a new notification record.
func (s *SQLiteStore) CreateNotification(ctx context.Context, notif *store.Notification) error {
	if notif.ID == "" || notif.SubscriptionID == "" || notif.AgentID == "" {
		return store.ErrInvalidInput
	}

	now := time.Now()
	if notif.CreatedAt.IsZero() {
		notif.CreatedAt = now
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO notifications (
			id, subscription_id, agent_id, project_id,
			subscriber_type, subscriber_id,
			status, message, dispatched, acknowledged, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		notif.ID, notif.SubscriptionID, notif.AgentID, notif.ProjectID,
		notif.SubscriberType, notif.SubscriberID,
		notif.Status, notif.Message,
		boolToInt(notif.Dispatched), boolToInt(notif.Acknowledged),
		notif.CreatedAt,
	)
	if err != nil {
		if strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
			return fmt.Errorf("subscription %s does not exist: %w", notif.SubscriptionID, store.ErrInvalidInput)
		}
		return err
	}
	return nil
}

// GetNotifications returns notifications for a subscriber.
// If onlyUnacknowledged is true, only unacknowledged notifications are returned.
// Results are ordered by created_at DESC.
func (s *SQLiteStore) GetNotifications(ctx context.Context, subscriberType, subscriberID string, onlyUnacknowledged bool) ([]store.Notification, error) {
	query := `
		SELECT id, subscription_id, agent_id, project_id,
			subscriber_type, subscriber_id,
			status, message, dispatched, acknowledged, created_at
		FROM notifications
		WHERE subscriber_type = ? AND subscriber_id = ?
	`
	args := []interface{}{subscriberType, subscriberID}

	if onlyUnacknowledged {
		query += ` AND acknowledged = 0`
	}

	query += ` ORDER BY created_at DESC`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanNotifications(rows)
}

// GetNotificationsByAgent returns notifications for a subscriber filtered by agent ID.
// If onlyUnacknowledged is true, only unacknowledged notifications are returned.
// Results are ordered by created_at DESC.
func (s *SQLiteStore) GetNotificationsByAgent(ctx context.Context, agentID, subscriberType, subscriberID string, onlyUnacknowledged bool) ([]store.Notification, error) {
	query := `
		SELECT id, subscription_id, agent_id, project_id,
			subscriber_type, subscriber_id,
			status, message, dispatched, acknowledged, created_at
		FROM notifications
		WHERE agent_id = ? AND subscriber_type = ? AND subscriber_id = ?
	`
	args := []interface{}{agentID, subscriberType, subscriberID}

	if onlyUnacknowledged {
		query += ` AND acknowledged = 0`
	}

	query += ` ORDER BY created_at DESC`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanNotifications(rows)
}

// AcknowledgeNotification marks a notification as acknowledged.
func (s *SQLiteStore) AcknowledgeNotification(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE notifications SET acknowledged = 1 WHERE id = ?
	`, id)
	if err != nil {
		return err
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return store.ErrNotFound
	}
	return nil
}

// AcknowledgeAllNotifications marks all notifications for a subscriber as acknowledged.
// No error on zero rows affected.
func (s *SQLiteStore) AcknowledgeAllNotifications(ctx context.Context, subscriberType, subscriberID string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE notifications SET acknowledged = 1
		WHERE subscriber_type = ? AND subscriber_id = ?
	`, subscriberType, subscriberID)
	return err
}

// MarkNotificationDispatched marks a notification as dispatched.
func (s *SQLiteStore) MarkNotificationDispatched(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE notifications SET dispatched = 1 WHERE id = ?
	`, id)
	if err != nil {
		return err
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return store.ErrNotFound
	}
	return nil
}

// GetLastNotificationStatus returns the status of the most recent notification
// for a given subscription. Returns ("", nil) if no notifications exist.
func (s *SQLiteStore) GetLastNotificationStatus(ctx context.Context, subscriptionID string) (string, error) {
	var status string
	err := s.db.QueryRowContext(ctx, `
		SELECT status FROM notifications
		WHERE subscription_id = ?
		ORDER BY created_at DESC
		LIMIT 1
	`, subscriptionID).Scan(&status)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return status, nil
}

// ============================================================================
// Subscription Template Operations
// ============================================================================

// CreateSubscriptionTemplate creates a new subscription template.
func (s *SQLiteStore) CreateSubscriptionTemplate(ctx context.Context, tmpl *store.SubscriptionTemplate) error {
	if tmpl.ID == "" || tmpl.Name == "" || len(tmpl.TriggerActivities) == 0 {
		return store.ErrInvalidInput
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO subscription_templates (id, name, scope, trigger_activities, project_id, created_by)
		VALUES (?, ?, ?, ?, ?, ?)
	`, tmpl.ID, tmpl.Name, tmpl.Scope, marshalJSON(tmpl.TriggerActivities), tmpl.ProjectID, tmpl.CreatedBy)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return store.ErrAlreadyExists
		}
		return err
	}
	return nil
}

// GetSubscriptionTemplate returns a template by ID.
func (s *SQLiteStore) GetSubscriptionTemplate(ctx context.Context, id string) (*store.SubscriptionTemplate, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, name, scope, trigger_activities, project_id, created_by
		FROM subscription_templates WHERE id = ?
	`, id)

	var tmpl store.SubscriptionTemplate
	var triggersJSON string
	if err := row.Scan(&tmpl.ID, &tmpl.Name, &tmpl.Scope, &triggersJSON, &tmpl.ProjectID, &tmpl.CreatedBy); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	unmarshalJSON(triggersJSON, &tmpl.TriggerActivities)
	return &tmpl, nil
}

// ListSubscriptionTemplates returns all templates. If projectID is non-empty,
// returns both global templates and project-specific templates.
func (s *SQLiteStore) ListSubscriptionTemplates(ctx context.Context, projectID string) ([]store.SubscriptionTemplate, error) {
	var rows *sql.Rows
	var err error

	if projectID != "" {
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, name, scope, trigger_activities, project_id, created_by
			FROM subscription_templates
			WHERE project_id = '' OR project_id = ?
			ORDER BY project_id ASC, name ASC
		`, projectID)
	} else {
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, name, scope, trigger_activities, project_id, created_by
			FROM subscription_templates
			WHERE project_id = ''
			ORDER BY name ASC
		`)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var templates []store.SubscriptionTemplate
	for rows.Next() {
		var tmpl store.SubscriptionTemplate
		var triggersJSON string
		if err := rows.Scan(&tmpl.ID, &tmpl.Name, &tmpl.Scope, &triggersJSON, &tmpl.ProjectID, &tmpl.CreatedBy); err != nil {
			return nil, err
		}
		unmarshalJSON(triggersJSON, &tmpl.TriggerActivities)
		templates = append(templates, tmpl)
	}
	return templates, rows.Err()
}

// DeleteSubscriptionTemplate deletes a template by ID.
func (s *SQLiteStore) DeleteSubscriptionTemplate(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `
		DELETE FROM subscription_templates WHERE id = ?
	`, id)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return store.ErrNotFound
	}
	return nil
}

// ============================================================================
// Helpers
// ============================================================================

// boolToInt converts a bool to an int for SQLite storage.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// scanSubscriptions scans rows into NotificationSubscription slices.
func scanSubscriptions(rows *sql.Rows) ([]store.NotificationSubscription, error) {
	var subs []store.NotificationSubscription
	for rows.Next() {
		var sub store.NotificationSubscription
		var agentID sql.NullString
		var triggerActivitiesJSON string

		if err := rows.Scan(
			&sub.ID, &sub.Scope, &agentID, &sub.SubscriberType, &sub.SubscriberID, &sub.ProjectID,
			&triggerActivitiesJSON, &sub.CreatedAt, &sub.CreatedBy,
		); err != nil {
			return nil, err
		}

		if agentID.Valid {
			sub.AgentID = agentID.String
		}
		unmarshalJSON(triggerActivitiesJSON, &sub.TriggerActivities)
		subs = append(subs, sub)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return subs, nil
}

// scanNotifications scans rows into Notification slices.
func scanNotifications(rows *sql.Rows) ([]store.Notification, error) {
	var notifs []store.Notification
	for rows.Next() {
		var notif store.Notification
		var dispatched, acknowledged int

		if err := rows.Scan(
			&notif.ID, &notif.SubscriptionID, &notif.AgentID, &notif.ProjectID,
			&notif.SubscriberType, &notif.SubscriberID,
			&notif.Status, &notif.Message, &dispatched, &acknowledged, &notif.CreatedAt,
		); err != nil {
			return nil, err
		}

		notif.Dispatched = dispatched != 0
		notif.Acknowledged = acknowledged != 0
		notifs = append(notifs, notif)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return notifs, nil
}
