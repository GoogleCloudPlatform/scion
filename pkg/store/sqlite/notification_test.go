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

//go:build !no_sqlite

package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/ptone/scion-agent/pkg/api"
	"github.com/ptone/scion-agent/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// createTestGroveAndAgent is a helper that creates a grove and agent for notification tests.
func createTestGroveAndAgent(t *testing.T, s *SQLiteStore) (groveID, agentID string) {
	t.Helper()
	ctx := context.Background()

	groveID = api.NewUUID()
	grove := &store.Grove{
		ID:         groveID,
		Name:       "Notification Test Grove",
		Slug:       "notif-grove-" + groveID[:8],
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateGrove(ctx, grove))

	agentID = api.NewUUID()
	agent := &store.Agent{
		ID:         agentID,
		Slug:       "notif-agent-" + agentID[:8],
		Name:       "Notification Test Agent",
		GroveID:    groveID,
		Status:     store.AgentStatusRunning,
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	return groveID, agentID
}

func TestNotificationSubscriptionCRUD(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	groveID, agentID := createTestGroveAndAgent(t, s)

	subID := uuid.New().String()
	sub := &store.NotificationSubscription{
		ID:              subID,
		AgentID:         agentID,
		SubscriberType:  store.SubscriberTypeAgent,
		SubscriberID:    "lead-agent",
		GroveID:         groveID,
		TriggerStatuses: []string{"COMPLETED", "WAITING_FOR_INPUT", "LIMITS_EXCEEDED"},
		CreatedBy:       "lead-agent",
	}

	// Create
	err := s.CreateNotificationSubscription(ctx, sub)
	require.NoError(t, err)
	assert.False(t, sub.CreatedAt.IsZero(), "CreatedAt should be set automatically")

	// Get by agent
	subs, err := s.GetNotificationSubscriptions(ctx, agentID)
	require.NoError(t, err)
	require.Len(t, subs, 1)
	assert.Equal(t, subID, subs[0].ID)
	assert.Equal(t, agentID, subs[0].AgentID)
	assert.Equal(t, store.SubscriberTypeAgent, subs[0].SubscriberType)
	assert.Equal(t, "lead-agent", subs[0].SubscriberID)
	assert.Equal(t, groveID, subs[0].GroveID)
	assert.Equal(t, []string{"COMPLETED", "WAITING_FOR_INPUT", "LIMITS_EXCEEDED"}, subs[0].TriggerStatuses)

	// Get by grove
	subs, err = s.GetNotificationSubscriptionsByGrove(ctx, groveID)
	require.NoError(t, err)
	require.Len(t, subs, 1)
	assert.Equal(t, subID, subs[0].ID)

	// Delete
	err = s.DeleteNotificationSubscription(ctx, subID)
	require.NoError(t, err)

	// Verify deleted
	subs, err = s.GetNotificationSubscriptions(ctx, agentID)
	require.NoError(t, err)
	assert.Empty(t, subs)

	// Delete not found
	err = s.DeleteNotificationSubscription(ctx, "non-existent")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestNotificationSubscriptionFKConstraint(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	// Try to create subscription with non-existent agent
	sub := &store.NotificationSubscription{
		ID:              uuid.New().String(),
		AgentID:         "non-existent-agent",
		SubscriberType:  store.SubscriberTypeAgent,
		SubscriberID:    "lead-agent",
		GroveID:         "some-grove",
		TriggerStatuses: []string{"COMPLETED"},
		CreatedBy:       "lead-agent",
	}

	err := s.CreateNotificationSubscription(ctx, sub)
	assert.Error(t, err)
	assert.ErrorIs(t, err, store.ErrInvalidInput)
}

func TestNotificationSubscriptionCascadeDelete(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	groveID, agentID := createTestGroveAndAgent(t, s)

	// Create subscription
	subID := uuid.New().String()
	sub := &store.NotificationSubscription{
		ID:              subID,
		AgentID:         agentID,
		SubscriberType:  store.SubscriberTypeAgent,
		SubscriberID:    "lead-agent",
		GroveID:         groveID,
		TriggerStatuses: []string{"COMPLETED"},
		CreatedBy:       "lead-agent",
	}
	require.NoError(t, s.CreateNotificationSubscription(ctx, sub))

	// Create notification for this subscription
	notifID := uuid.New().String()
	notif := &store.Notification{
		ID:             notifID,
		SubscriptionID: subID,
		AgentID:        agentID,
		GroveID:        groveID,
		SubscriberType: store.SubscriberTypeAgent,
		SubscriberID:   "lead-agent",
		Status:         "COMPLETED",
		Message:        "agent completed",
	}
	require.NoError(t, s.CreateNotification(ctx, notif))

	// Verify notification exists
	notifs, err := s.GetNotifications(ctx, store.SubscriberTypeAgent, "lead-agent", false)
	require.NoError(t, err)
	require.Len(t, notifs, 1)

	// Delete the agent — should cascade to subscriptions and their notifications
	err = s.DeleteAgent(ctx, agentID)
	require.NoError(t, err)

	// Verify subscription is gone
	subs, err := s.GetNotificationSubscriptions(ctx, agentID)
	require.NoError(t, err)
	assert.Empty(t, subs)

	// Verify notification is gone (cascaded from subscription)
	notifs, err = s.GetNotifications(ctx, store.SubscriberTypeAgent, "lead-agent", false)
	require.NoError(t, err)
	assert.Empty(t, notifs)
}

func TestBulkDeleteSubscriptions(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	groveID, agentID := createTestGroveAndAgent(t, s)

	// Create multiple subscriptions
	for i := 0; i < 3; i++ {
		sub := &store.NotificationSubscription{
			ID:              uuid.New().String(),
			AgentID:         agentID,
			SubscriberType:  store.SubscriberTypeAgent,
			SubscriberID:    "subscriber-" + uuid.New().String()[:8],
			GroveID:         groveID,
			TriggerStatuses: []string{"COMPLETED"},
			CreatedBy:       "test",
		}
		require.NoError(t, s.CreateNotificationSubscription(ctx, sub))
	}

	// Verify they exist
	subs, err := s.GetNotificationSubscriptions(ctx, agentID)
	require.NoError(t, err)
	assert.Len(t, subs, 3)

	// Bulk delete
	err = s.DeleteNotificationSubscriptionsForAgent(ctx, agentID)
	require.NoError(t, err)

	// Verify all gone
	subs, err = s.GetNotificationSubscriptions(ctx, agentID)
	require.NoError(t, err)
	assert.Empty(t, subs)

	// Repeat — no error on zero rows
	err = s.DeleteNotificationSubscriptionsForAgent(ctx, agentID)
	assert.NoError(t, err)
}

func TestNotificationCRUD(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	groveID, agentID := createTestGroveAndAgent(t, s)

	// Create subscription first
	subID := uuid.New().String()
	sub := &store.NotificationSubscription{
		ID:              subID,
		AgentID:         agentID,
		SubscriberType:  store.SubscriberTypeUser,
		SubscriberID:    "user-123",
		GroveID:         groveID,
		TriggerStatuses: []string{"COMPLETED", "WAITING_FOR_INPUT"},
		CreatedBy:       "user-123",
	}
	require.NoError(t, s.CreateNotificationSubscription(ctx, sub))

	// Create notification
	notifID := uuid.New().String()
	notif := &store.Notification{
		ID:             notifID,
		SubscriptionID: subID,
		AgentID:        agentID,
		GroveID:        groveID,
		SubscriberType: store.SubscriberTypeUser,
		SubscriberID:   "user-123",
		Status:         "COMPLETED",
		Message:        "agent has reached a state of COMPLETED",
	}
	err := s.CreateNotification(ctx, notif)
	require.NoError(t, err)
	assert.False(t, notif.CreatedAt.IsZero(), "CreatedAt should be set automatically")

	// Get notifications for subscriber
	notifs, err := s.GetNotifications(ctx, store.SubscriberTypeUser, "user-123", false)
	require.NoError(t, err)
	require.Len(t, notifs, 1)
	assert.Equal(t, notifID, notifs[0].ID)
	assert.Equal(t, subID, notifs[0].SubscriptionID)
	assert.Equal(t, agentID, notifs[0].AgentID)
	assert.Equal(t, "COMPLETED", notifs[0].Status)
	assert.Equal(t, "agent has reached a state of COMPLETED", notifs[0].Message)
	assert.False(t, notifs[0].Dispatched)
	assert.False(t, notifs[0].Acknowledged)

	// Acknowledge
	err = s.AcknowledgeNotification(ctx, notifID)
	require.NoError(t, err)

	// Verify acknowledged
	notifs, err = s.GetNotifications(ctx, store.SubscriberTypeUser, "user-123", false)
	require.NoError(t, err)
	require.Len(t, notifs, 1)
	assert.True(t, notifs[0].Acknowledged)

	// Acknowledge not found
	err = s.AcknowledgeNotification(ctx, "non-existent")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestNotificationFiltering(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	groveID, agentID := createTestGroveAndAgent(t, s)

	subID := uuid.New().String()
	sub := &store.NotificationSubscription{
		ID:              subID,
		AgentID:         agentID,
		SubscriberType:  store.SubscriberTypeUser,
		SubscriberID:    "filter-user",
		GroveID:         groveID,
		TriggerStatuses: []string{"COMPLETED"},
		CreatedBy:       "filter-user",
	}
	require.NoError(t, s.CreateNotificationSubscription(ctx, sub))

	// Create two notifications — one acknowledged, one not
	notif1 := &store.Notification{
		ID:             uuid.New().String(),
		SubscriptionID: subID,
		AgentID:        agentID,
		GroveID:        groveID,
		SubscriberType: store.SubscriberTypeUser,
		SubscriberID:   "filter-user",
		Status:         "COMPLETED",
		Message:        "first notification",
		CreatedAt:      time.Now().Add(-2 * time.Second),
	}
	notif2 := &store.Notification{
		ID:             uuid.New().String(),
		SubscriptionID: subID,
		AgentID:        agentID,
		GroveID:        groveID,
		SubscriberType: store.SubscriberTypeUser,
		SubscriberID:   "filter-user",
		Status:         "COMPLETED",
		Message:        "second notification",
		CreatedAt:      time.Now(),
	}
	require.NoError(t, s.CreateNotification(ctx, notif1))
	require.NoError(t, s.CreateNotification(ctx, notif2))

	// Acknowledge the first one
	require.NoError(t, s.AcknowledgeNotification(ctx, notif1.ID))

	// Get all — should return both, ordered by created_at DESC
	all, err := s.GetNotifications(ctx, store.SubscriberTypeUser, "filter-user", false)
	require.NoError(t, err)
	require.Len(t, all, 2)
	assert.Equal(t, notif2.ID, all[0].ID, "most recent should be first")
	assert.Equal(t, notif1.ID, all[1].ID)

	// Get only unacknowledged — should return only the second
	unacked, err := s.GetNotifications(ctx, store.SubscriberTypeUser, "filter-user", true)
	require.NoError(t, err)
	require.Len(t, unacked, 1)
	assert.Equal(t, notif2.ID, unacked[0].ID)
}

func TestMarkNotificationDispatched(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	groveID, agentID := createTestGroveAndAgent(t, s)

	subID := uuid.New().String()
	sub := &store.NotificationSubscription{
		ID:              subID,
		AgentID:         agentID,
		SubscriberType:  store.SubscriberTypeAgent,
		SubscriberID:    "dispatch-target",
		GroveID:         groveID,
		TriggerStatuses: []string{"COMPLETED"},
		CreatedBy:       "test",
	}
	require.NoError(t, s.CreateNotificationSubscription(ctx, sub))

	notifID := uuid.New().String()
	notif := &store.Notification{
		ID:             notifID,
		SubscriptionID: subID,
		AgentID:        agentID,
		GroveID:        groveID,
		SubscriberType: store.SubscriberTypeAgent,
		SubscriberID:   "dispatch-target",
		Status:         "COMPLETED",
		Message:        "dispatched test",
	}
	require.NoError(t, s.CreateNotification(ctx, notif))

	// Initially not dispatched
	notifs, err := s.GetNotifications(ctx, store.SubscriberTypeAgent, "dispatch-target", false)
	require.NoError(t, err)
	require.Len(t, notifs, 1)
	assert.False(t, notifs[0].Dispatched)

	// Mark dispatched
	err = s.MarkNotificationDispatched(ctx, notifID)
	require.NoError(t, err)

	// Verify dispatched
	notifs, err = s.GetNotifications(ctx, store.SubscriberTypeAgent, "dispatch-target", false)
	require.NoError(t, err)
	require.Len(t, notifs, 1)
	assert.True(t, notifs[0].Dispatched)

	// Not found
	err = s.MarkNotificationDispatched(ctx, "non-existent")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestAcknowledgeAllNotifications(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	groveID, agentID := createTestGroveAndAgent(t, s)

	subID := uuid.New().String()
	sub := &store.NotificationSubscription{
		ID:              subID,
		AgentID:         agentID,
		SubscriberType:  store.SubscriberTypeUser,
		SubscriberID:    "ack-all-user",
		GroveID:         groveID,
		TriggerStatuses: []string{"COMPLETED"},
		CreatedBy:       "ack-all-user",
	}
	require.NoError(t, s.CreateNotificationSubscription(ctx, sub))

	// Create multiple notifications
	for i := 0; i < 3; i++ {
		notif := &store.Notification{
			ID:             uuid.New().String(),
			SubscriptionID: subID,
			AgentID:        agentID,
			GroveID:        groveID,
			SubscriberType: store.SubscriberTypeUser,
			SubscriberID:   "ack-all-user",
			Status:         "COMPLETED",
			Message:        "notification",
		}
		require.NoError(t, s.CreateNotification(ctx, notif))
	}

	// All unacknowledged
	unacked, err := s.GetNotifications(ctx, store.SubscriberTypeUser, "ack-all-user", true)
	require.NoError(t, err)
	assert.Len(t, unacked, 3)

	// Acknowledge all
	err = s.AcknowledgeAllNotifications(ctx, store.SubscriberTypeUser, "ack-all-user")
	require.NoError(t, err)

	// Verify all acknowledged
	unacked, err = s.GetNotifications(ctx, store.SubscriberTypeUser, "ack-all-user", true)
	require.NoError(t, err)
	assert.Empty(t, unacked)

	// All should still be retrievable
	all, err := s.GetNotifications(ctx, store.SubscriberTypeUser, "ack-all-user", false)
	require.NoError(t, err)
	assert.Len(t, all, 3)

	// Repeat — no error on zero rows
	err = s.AcknowledgeAllNotifications(ctx, store.SubscriberTypeUser, "ack-all-user")
	assert.NoError(t, err)
}

func TestGetLastNotificationStatus(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	groveID, agentID := createTestGroveAndAgent(t, s)

	subID := uuid.New().String()
	sub := &store.NotificationSubscription{
		ID:              subID,
		AgentID:         agentID,
		SubscriberType:  store.SubscriberTypeAgent,
		SubscriberID:    "last-status-agent",
		GroveID:         groveID,
		TriggerStatuses: []string{"COMPLETED", "WAITING_FOR_INPUT"},
		CreatedBy:       "test",
	}
	require.NoError(t, s.CreateNotificationSubscription(ctx, sub))

	// No notifications yet — should return empty string, no error
	status, err := s.GetLastNotificationStatus(ctx, subID)
	require.NoError(t, err)
	assert.Equal(t, "", status)

	// Create first notification
	notif1 := &store.Notification{
		ID:             uuid.New().String(),
		SubscriptionID: subID,
		AgentID:        agentID,
		GroveID:        groveID,
		SubscriberType: store.SubscriberTypeAgent,
		SubscriberID:   "last-status-agent",
		Status:         "WAITING_FOR_INPUT",
		Message:        "waiting",
		CreatedAt:      time.Now().Add(-1 * time.Second),
	}
	require.NoError(t, s.CreateNotification(ctx, notif1))

	status, err = s.GetLastNotificationStatus(ctx, subID)
	require.NoError(t, err)
	assert.Equal(t, "WAITING_FOR_INPUT", status)

	// Create second notification (more recent)
	notif2 := &store.Notification{
		ID:             uuid.New().String(),
		SubscriptionID: subID,
		AgentID:        agentID,
		GroveID:        groveID,
		SubscriberType: store.SubscriberTypeAgent,
		SubscriberID:   "last-status-agent",
		Status:         "COMPLETED",
		Message:        "done",
		CreatedAt:      time.Now(),
	}
	require.NoError(t, s.CreateNotification(ctx, notif2))

	status, err = s.GetLastNotificationStatus(ctx, subID)
	require.NoError(t, err)
	assert.Equal(t, "COMPLETED", status)
}

func TestMatchesStatus(t *testing.T) {
	sub := &store.NotificationSubscription{
		TriggerStatuses: []string{"COMPLETED", "WAITING_FOR_INPUT"},
	}

	// Case-insensitive matching
	assert.True(t, sub.MatchesStatus("COMPLETED"))
	assert.True(t, sub.MatchesStatus("completed"))
	assert.True(t, sub.MatchesStatus("Completed"))
	assert.True(t, sub.MatchesStatus("waiting_for_input"))
	assert.True(t, sub.MatchesStatus("WAITING_FOR_INPUT"))

	// Non-matching
	assert.False(t, sub.MatchesStatus("RUNNING"))
	assert.False(t, sub.MatchesStatus("error"))
	assert.False(t, sub.MatchesStatus(""))

	// Empty trigger list
	emptySub := &store.NotificationSubscription{
		TriggerStatuses: []string{},
	}
	assert.False(t, emptySub.MatchesStatus("COMPLETED"))

	// Nil trigger list
	nilSub := &store.NotificationSubscription{}
	assert.False(t, nilSub.MatchesStatus("COMPLETED"))
}
