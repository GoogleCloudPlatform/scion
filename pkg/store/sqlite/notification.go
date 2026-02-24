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

	"github.com/ptone/scion-agent/pkg/store"
)

// ============================================================================
// Notification Subscription Operations
// ============================================================================

// CreateNotificationSubscription creates a new notification subscription.
func (s *SQLiteStore) CreateNotificationSubscription(ctx context.Context, sub *store.NotificationSubscription) error {
	if sub.ID == "" || sub.AgentID == "" || sub.SubscriberID == "" || sub.GroveID == "" {
		return store.ErrInvalidInput
	}

	now := time.Now()
	if sub.CreatedAt.IsZero() {
		sub.CreatedAt = now
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO notification_subscriptions (
			id, agent_id, subscriber_type, subscriber_id, grove_id,
			trigger_statuses, created_at, created_by
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`,
		sub.ID, sub.AgentID, sub.SubscriberType, sub.SubscriberID, sub.GroveID,
		marshalJSON(sub.TriggerStatuses), sub.CreatedAt, sub.CreatedBy,
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

// GetNotificationSubscriptions returns all subscriptions for a watched agent.
func (s *SQLiteStore) GetNotificationSubscriptions(ctx context.Context, agentID string) ([]store.NotificationSubscription, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, agent_id, subscriber_type, subscriber_id, grove_id,
			trigger_statuses, created_at, created_by
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

// GetNotificationSubscriptionsByGrove returns all subscriptions within a grove.
func (s *SQLiteStore) GetNotificationSubscriptionsByGrove(ctx context.Context, groveID string) ([]store.NotificationSubscription, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, agent_id, subscriber_type, subscriber_id, grove_id,
			trigger_statuses, created_at, created_by
		FROM notification_subscriptions
		WHERE grove_id = ?
		ORDER BY created_at ASC
	`, groveID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanSubscriptions(rows)
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
			id, subscription_id, agent_id, grove_id,
			subscriber_type, subscriber_id,
			status, message, dispatched, acknowledged, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		notif.ID, notif.SubscriptionID, notif.AgentID, notif.GroveID,
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
		SELECT id, subscription_id, agent_id, grove_id,
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
		var triggerStatusesJSON string

		if err := rows.Scan(
			&sub.ID, &sub.AgentID, &sub.SubscriberType, &sub.SubscriberID, &sub.GroveID,
			&triggerStatusesJSON, &sub.CreatedAt, &sub.CreatedBy,
		); err != nil {
			return nil, err
		}

		unmarshalJSON(triggerStatusesJSON, &sub.TriggerStatuses)
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
			&notif.ID, &notif.SubscriptionID, &notif.AgentID, &notif.GroveID,
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
