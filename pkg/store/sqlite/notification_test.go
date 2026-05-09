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

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// createTestProjectAndAgent is a helper that creates a project and agent for notification tests.
func createTestProjectAndAgent(t *testing.T, s *SQLiteStore) (projectID, agentID string) {
	t.Helper()
	ctx := context.Background()

	projectID = api.NewUUID()
	project := &store.Project{
		ID:         projectID,
		Name:       "Notification Test Project",
		Slug:       "notif-project-" + projectID[:8],
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateProject(ctx, project))

	agentID = api.NewUUID()
	agent := &store.Agent{
		ID:         agentID,
		Slug:       "notif-agent-" + agentID[:8],
		Name:       "Notification Test Agent",
		ProjectID:  projectID,
		Phase:      string(state.PhaseRunning),
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	return projectID, agentID
}

func TestNotificationSubscriptionCRUD(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	projectID, agentID := createTestProjectAndAgent(t, s)

	subID := uuid.New().String()
	sub := &store.NotificationSubscription{
		ID:                subID,
		Scope:             store.SubscriptionScopeAgent,
		AgentID:           agentID,
		SubscriberType:    store.SubscriberTypeAgent,
		SubscriberID:      "lead-agent",
		ProjectID:         projectID,
		TriggerActivities: []string{"COMPLETED", "WAITING_FOR_INPUT", "LIMITS_EXCEEDED"},
		CreatedBy:         "lead-agent",
	}

	// Create
	err := s.CreateNotificationSubscription(ctx, sub)
	require.NoError(t, err)
	assert.False(t, sub.CreatedAt.IsZero(), "CreatedAt should be set automatically")

	// Get by ID
	got, err := s.GetNotificationSubscription(ctx, subID)
	require.NoError(t, err)
	assert.Equal(t, subID, got.ID)
	assert.Equal(t, store.SubscriptionScopeAgent, got.Scope)
	assert.Equal(t, agentID, got.AgentID)
	assert.Equal(t, store.SubscriberTypeAgent, got.SubscriberType)
	assert.Equal(t, "lead-agent", got.SubscriberID)

	// Get by ID not found
	_, err = s.GetNotificationSubscription(ctx, "non-existent")
	assert.ErrorIs(t, err, store.ErrNotFound)

	// Get by agent
	subs, err := s.GetNotificationSubscriptions(ctx, agentID)
	require.NoError(t, err)
	require.Len(t, subs, 1)
	assert.Equal(t, subID, subs[0].ID)
	assert.Equal(t, store.SubscriptionScopeAgent, subs[0].Scope)
	assert.Equal(t, agentID, subs[0].AgentID)
	assert.Equal(t, store.SubscriberTypeAgent, subs[0].SubscriberType)
	assert.Equal(t, "lead-agent", subs[0].SubscriberID)
	assert.Equal(t, projectID, subs[0].ProjectID)
	assert.Equal(t, []string{"COMPLETED", "WAITING_FOR_INPUT", "LIMITS_EXCEEDED"}, subs[0].TriggerActivities)

	// Get by project
	subs, err = s.GetNotificationSubscriptionsByProject(ctx, projectID)
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

func TestNotificationSubscriptionScopeDefault(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	projectID, agentID := createTestProjectAndAgent(t, s)

	// Create subscription without explicit scope — should default to "agent"
	sub := &store.NotificationSubscription{
		ID:                uuid.New().String(),
		AgentID:           agentID,
		SubscriberType:    store.SubscriberTypeAgent,
		SubscriberID:      "default-scope-agent",
		ProjectID:         projectID,
		TriggerActivities: []string{"COMPLETED"},
		CreatedBy:         "test",
	}
	require.NoError(t, s.CreateNotificationSubscription(ctx, sub))
	assert.Equal(t, store.SubscriptionScopeAgent, sub.Scope)

	got, err := s.GetNotificationSubscription(ctx, sub.ID)
	require.NoError(t, err)
	assert.Equal(t, store.SubscriptionScopeAgent, got.Scope)
}

func TestProjectScopedSubscription(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	projectID, agentID := createTestProjectAndAgent(t, s)

	// Create a project-scoped subscription
	projectSub := &store.NotificationSubscription{
		ID:                uuid.New().String(),
		Scope:             store.SubscriptionScopeProject,
		SubscriberType:    store.SubscriberTypeUser,
		SubscriberID:      "user-project-watcher",
		ProjectID:         projectID,
		TriggerActivities: []string{"COMPLETED", "WAITING_FOR_INPUT"},
		CreatedBy:         "user-project-watcher",
	}
	require.NoError(t, s.CreateNotificationSubscription(ctx, projectSub))

	// Create an agent-scoped subscription in the same project
	agentSub := &store.NotificationSubscription{
		ID:                uuid.New().String(),
		Scope:             store.SubscriptionScopeAgent,
		AgentID:           agentID,
		SubscriberType:    store.SubscriberTypeUser,
		SubscriberID:      "user-agent-watcher",
		ProjectID:         projectID,
		TriggerActivities: []string{"COMPLETED"},
		CreatedBy:         "user-agent-watcher",
	}
	require.NoError(t, s.CreateNotificationSubscription(ctx, agentSub))

	// GetNotificationSubscriptionsByProjectScope should return only project-scoped
	projectSubs, err := s.GetNotificationSubscriptionsByProjectScope(ctx, projectID)
	require.NoError(t, err)
	require.Len(t, projectSubs, 1)
	assert.Equal(t, projectSub.ID, projectSubs[0].ID)
	assert.Equal(t, store.SubscriptionScopeProject, projectSubs[0].Scope)
	assert.Empty(t, projectSubs[0].AgentID)

	// GetNotificationSubscriptionsByProject should return both
	allSubs, err := s.GetNotificationSubscriptionsByProject(ctx, projectID)
	require.NoError(t, err)
	assert.Len(t, allSubs, 2)

	// GetNotificationSubscriptions (by agent) should return only agent-scoped
	agentSubs, err := s.GetNotificationSubscriptions(ctx, agentID)
	require.NoError(t, err)
	require.Len(t, agentSubs, 1)
	assert.Equal(t, agentSub.ID, agentSubs[0].ID)
}

func TestGetSubscriptionsForSubscriber(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	projectID, agentID := createTestProjectAndAgent(t, s)

	// Create project-scoped subscription for user
	sub1 := &store.NotificationSubscription{
		ID:                uuid.New().String(),
		Scope:             store.SubscriptionScopeProject,
		SubscriberType:    store.SubscriberTypeUser,
		SubscriberID:      "sub-user",
		ProjectID:         projectID,
		TriggerActivities: []string{"COMPLETED"},
		CreatedBy:         "sub-user",
	}
	require.NoError(t, s.CreateNotificationSubscription(ctx, sub1))

	// Create agent-scoped subscription for same user
	sub2 := &store.NotificationSubscription{
		ID:                uuid.New().String(),
		Scope:             store.SubscriptionScopeAgent,
		AgentID:           agentID,
		SubscriberType:    store.SubscriberTypeUser,
		SubscriberID:      "sub-user",
		ProjectID:         projectID,
		TriggerActivities: []string{"COMPLETED"},
		CreatedBy:         "sub-user",
	}
	require.NoError(t, s.CreateNotificationSubscription(ctx, sub2))

	// Create subscription for different user
	sub3 := &store.NotificationSubscription{
		ID:                uuid.New().String(),
		Scope:             store.SubscriptionScopeAgent,
		AgentID:           agentID,
		SubscriberType:    store.SubscriberTypeUser,
		SubscriberID:      "other-user",
		ProjectID:         projectID,
		TriggerActivities: []string{"COMPLETED"},
		CreatedBy:         "other-user",
	}
	require.NoError(t, s.CreateNotificationSubscription(ctx, sub3))

	// Get for sub-user
	subs, err := s.GetSubscriptionsForSubscriber(ctx, store.SubscriberTypeUser, "sub-user")
	require.NoError(t, err)
	assert.Len(t, subs, 2)

	// Get for other-user
	subs, err = s.GetSubscriptionsForSubscriber(ctx, store.SubscriberTypeUser, "other-user")
	require.NoError(t, err)
	assert.Len(t, subs, 1)

	// Get for non-existent
	subs, err = s.GetSubscriptionsForSubscriber(ctx, store.SubscriberTypeUser, "nobody")
	require.NoError(t, err)
	assert.Empty(t, subs)
}

func TestSubscriptionUniqueConstraint(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	projectID, agentID := createTestProjectAndAgent(t, s)

	// Create first subscription
	sub1 := &store.NotificationSubscription{
		ID:                uuid.New().String(),
		Scope:             store.SubscriptionScopeAgent,
		AgentID:           agentID,
		SubscriberType:    store.SubscriberTypeUser,
		SubscriberID:      "unique-user",
		ProjectID:         projectID,
		TriggerActivities: []string{"COMPLETED"},
		CreatedBy:         "unique-user",
	}
	require.NoError(t, s.CreateNotificationSubscription(ctx, sub1))

	// Duplicate should fail with ErrAlreadyExists
	sub2 := &store.NotificationSubscription{
		ID:                uuid.New().String(),
		Scope:             store.SubscriptionScopeAgent,
		AgentID:           agentID,
		SubscriberType:    store.SubscriberTypeUser,
		SubscriberID:      "unique-user",
		ProjectID:         projectID,
		TriggerActivities: []string{"COMPLETED", "WAITING_FOR_INPUT"},
		CreatedBy:         "unique-user",
	}
	err := s.CreateNotificationSubscription(ctx, sub2)
	assert.ErrorIs(t, err, store.ErrAlreadyExists)

	// Same subscriber with different scope should succeed
	sub3 := &store.NotificationSubscription{
		ID:                uuid.New().String(),
		Scope:             store.SubscriptionScopeProject,
		SubscriberType:    store.SubscriberTypeUser,
		SubscriberID:      "unique-user",
		ProjectID:         projectID,
		TriggerActivities: []string{"COMPLETED"},
		CreatedBy:         "unique-user",
	}
	require.NoError(t, s.CreateNotificationSubscription(ctx, sub3))
}

func TestProjectScopedValidation(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	projectID, _ := createTestProjectAndAgent(t, s)

	// project-scoped with agent_id should clear agent_id
	sub := &store.NotificationSubscription{
		ID:                uuid.New().String(),
		Scope:             store.SubscriptionScopeProject,
		AgentID:           "should-be-cleared",
		SubscriberType:    store.SubscriberTypeUser,
		SubscriberID:      "validation-user",
		ProjectID:         projectID,
		TriggerActivities: []string{"COMPLETED"},
		CreatedBy:         "validation-user",
	}
	require.NoError(t, s.CreateNotificationSubscription(ctx, sub))
	assert.Empty(t, sub.AgentID) // Should have been cleared

	// agent-scoped without agent_id should fail
	sub2 := &store.NotificationSubscription{
		ID:                uuid.New().String(),
		Scope:             store.SubscriptionScopeAgent,
		AgentID:           "",
		SubscriberType:    store.SubscriberTypeUser,
		SubscriberID:      "validation-user2",
		ProjectID:         projectID,
		TriggerActivities: []string{"COMPLETED"},
		CreatedBy:         "validation-user2",
	}
	err := s.CreateNotificationSubscription(ctx, sub2)
	assert.ErrorIs(t, err, store.ErrInvalidInput)
}

func TestNotificationSubscriptionFKConstraint(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	// Try to create subscription with non-existent agent
	sub := &store.NotificationSubscription{
		ID:                uuid.New().String(),
		Scope:             store.SubscriptionScopeAgent,
		AgentID:           "non-existent-agent",
		SubscriberType:    store.SubscriberTypeAgent,
		SubscriberID:      "lead-agent",
		ProjectID:         "some-project",
		TriggerActivities: []string{"COMPLETED"},
		CreatedBy:         "lead-agent",
	}

	err := s.CreateNotificationSubscription(ctx, sub)
	assert.Error(t, err)
	assert.ErrorIs(t, err, store.ErrInvalidInput)
}

func TestNotificationSubscriptionCascadeDelete(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	projectID, agentID := createTestProjectAndAgent(t, s)

	// Create subscription
	subID := uuid.New().String()
	sub := &store.NotificationSubscription{
		ID:                subID,
		Scope:             store.SubscriptionScopeAgent,
		AgentID:           agentID,
		SubscriberType:    store.SubscriberTypeAgent,
		SubscriberID:      "lead-agent",
		ProjectID:         projectID,
		TriggerActivities: []string{"COMPLETED"},
		CreatedBy:         "lead-agent",
	}
	require.NoError(t, s.CreateNotificationSubscription(ctx, sub))

	// Create notification for this subscription
	notifID := uuid.New().String()
	notif := &store.Notification{
		ID:             notifID,
		SubscriptionID: subID,
		AgentID:        agentID,
		ProjectID:      projectID,
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
	projectID, agentID := createTestProjectAndAgent(t, s)

	// Create multiple subscriptions
	for i := 0; i < 3; i++ {
		sub := &store.NotificationSubscription{
			ID:                uuid.New().String(),
			Scope:             store.SubscriptionScopeAgent,
			AgentID:           agentID,
			SubscriberType:    store.SubscriberTypeAgent,
			SubscriberID:      "subscriber-" + uuid.New().String()[:8],
			ProjectID:         projectID,
			TriggerActivities: []string{"COMPLETED"},
			CreatedBy:         "test",
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
	projectID, agentID := createTestProjectAndAgent(t, s)

	// Create subscription first
	subID := uuid.New().String()
	sub := &store.NotificationSubscription{
		ID:                subID,
		Scope:             store.SubscriptionScopeAgent,
		AgentID:           agentID,
		SubscriberType:    store.SubscriberTypeUser,
		SubscriberID:      "user-123",
		ProjectID:         projectID,
		TriggerActivities: []string{"COMPLETED", "WAITING_FOR_INPUT"},
		CreatedBy:         "user-123",
	}
	require.NoError(t, s.CreateNotificationSubscription(ctx, sub))

	// Create notification
	notifID := uuid.New().String()
	notif := &store.Notification{
		ID:             notifID,
		SubscriptionID: subID,
		AgentID:        agentID,
		ProjectID:      projectID,
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
	projectID, agentID := createTestProjectAndAgent(t, s)

	subID := uuid.New().String()
	sub := &store.NotificationSubscription{
		ID:                subID,
		Scope:             store.SubscriptionScopeAgent,
		AgentID:           agentID,
		SubscriberType:    store.SubscriberTypeUser,
		SubscriberID:      "filter-user",
		ProjectID:         projectID,
		TriggerActivities: []string{"COMPLETED"},
		CreatedBy:         "filter-user",
	}
	require.NoError(t, s.CreateNotificationSubscription(ctx, sub))

	// Create two notifications — one acknowledged, one not
	notif1 := &store.Notification{
		ID:             uuid.New().String(),
		SubscriptionID: subID,
		AgentID:        agentID,
		ProjectID:      projectID,
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
		ProjectID:      projectID,
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
	projectID, agentID := createTestProjectAndAgent(t, s)

	subID := uuid.New().String()
	sub := &store.NotificationSubscription{
		ID:                subID,
		Scope:             store.SubscriptionScopeAgent,
		AgentID:           agentID,
		SubscriberType:    store.SubscriberTypeAgent,
		SubscriberID:      "dispatch-target",
		ProjectID:         projectID,
		TriggerActivities: []string{"COMPLETED"},
		CreatedBy:         "test",
	}
	require.NoError(t, s.CreateNotificationSubscription(ctx, sub))

	notifID := uuid.New().String()
	notif := &store.Notification{
		ID:             notifID,
		SubscriptionID: subID,
		AgentID:        agentID,
		ProjectID:      projectID,
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
	projectID, agentID := createTestProjectAndAgent(t, s)

	subID := uuid.New().String()
	sub := &store.NotificationSubscription{
		ID:                subID,
		Scope:             store.SubscriptionScopeAgent,
		AgentID:           agentID,
		SubscriberType:    store.SubscriberTypeUser,
		SubscriberID:      "ack-all-user",
		ProjectID:         projectID,
		TriggerActivities: []string{"COMPLETED"},
		CreatedBy:         "ack-all-user",
	}
	require.NoError(t, s.CreateNotificationSubscription(ctx, sub))

	// Create multiple notifications
	for i := 0; i < 3; i++ {
		notif := &store.Notification{
			ID:             uuid.New().String(),
			SubscriptionID: subID,
			AgentID:        agentID,
			ProjectID:      projectID,
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
	projectID, agentID := createTestProjectAndAgent(t, s)

	subID := uuid.New().String()
	sub := &store.NotificationSubscription{
		ID:                subID,
		Scope:             store.SubscriptionScopeAgent,
		AgentID:           agentID,
		SubscriberType:    store.SubscriberTypeAgent,
		SubscriberID:      "last-status-agent",
		ProjectID:         projectID,
		TriggerActivities: []string{"COMPLETED", "WAITING_FOR_INPUT"},
		CreatedBy:         "test",
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
		ProjectID:      projectID,
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
		ProjectID:      projectID,
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

func TestMatchesActivity(t *testing.T) {
	sub := &store.NotificationSubscription{
		TriggerActivities: []string{"COMPLETED", "WAITING_FOR_INPUT"},
	}

	// Case-insensitive matching
	assert.True(t, sub.MatchesActivity("COMPLETED"))
	assert.True(t, sub.MatchesActivity("completed"))
	assert.True(t, sub.MatchesActivity("Completed"))
	assert.True(t, sub.MatchesActivity("waiting_for_input"))
	assert.True(t, sub.MatchesActivity("WAITING_FOR_INPUT"))

	// Non-matching
	assert.False(t, sub.MatchesActivity("RUNNING"))
	assert.False(t, sub.MatchesActivity("error"))
	assert.False(t, sub.MatchesActivity(""))

	// Empty trigger list
	emptySub := &store.NotificationSubscription{
		TriggerActivities: []string{},
	}
	assert.False(t, emptySub.MatchesActivity("COMPLETED"))

	// Nil trigger list
	nilSub := &store.NotificationSubscription{}
	assert.False(t, nilSub.MatchesActivity("COMPLETED"))
}

func TestGetNotificationsByAgent(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	// Create project and two agents
	projectID, agent1ID := createTestProjectAndAgent(t, s)
	agent2ID := api.NewUUID()
	agent2 := &store.Agent{
		ID:         agent2ID,
		Slug:       "notif-agent2-" + agent2ID[:8],
		Name:       "Second Agent",
		ProjectID:  projectID,
		Phase:      string(state.PhaseRunning),
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateAgent(ctx, agent2))

	// Create subscriptions for both agents
	sub1ID := uuid.New().String()
	sub1 := &store.NotificationSubscription{
		ID:                sub1ID,
		Scope:             store.SubscriptionScopeAgent,
		AgentID:           agent1ID,
		SubscriberType:    store.SubscriberTypeUser,
		SubscriberID:      "user-by-agent",
		ProjectID:         projectID,
		TriggerActivities: []string{"COMPLETED"},
		CreatedBy:         "user-by-agent",
	}
	require.NoError(t, s.CreateNotificationSubscription(ctx, sub1))

	sub2ID := uuid.New().String()
	sub2 := &store.NotificationSubscription{
		ID:                sub2ID,
		Scope:             store.SubscriptionScopeAgent,
		AgentID:           agent2ID,
		SubscriberType:    store.SubscriberTypeUser,
		SubscriberID:      "user-by-agent",
		ProjectID:         projectID,
		TriggerActivities: []string{"COMPLETED"},
		CreatedBy:         "user-by-agent",
	}
	require.NoError(t, s.CreateNotificationSubscription(ctx, sub2))

	// Create notifications for agent1 (2 notifications, one acked)
	n1 := &store.Notification{
		ID:             uuid.New().String(),
		SubscriptionID: sub1ID,
		AgentID:        agent1ID,
		ProjectID:      projectID,
		SubscriberType: store.SubscriberTypeUser,
		SubscriberID:   "user-by-agent",
		Status:         "COMPLETED",
		Message:        "agent1 completed first",
		CreatedAt:      time.Now().Add(-2 * time.Second),
	}
	n2 := &store.Notification{
		ID:             uuid.New().String(),
		SubscriptionID: sub1ID,
		AgentID:        agent1ID,
		ProjectID:      projectID,
		SubscriberType: store.SubscriberTypeUser,
		SubscriberID:   "user-by-agent",
		Status:         "COMPLETED",
		Message:        "agent1 completed second",
		CreatedAt:      time.Now(),
	}
	require.NoError(t, s.CreateNotification(ctx, n1))
	require.NoError(t, s.CreateNotification(ctx, n2))
	require.NoError(t, s.AcknowledgeNotification(ctx, n1.ID))

	// Create notification for agent2
	n3 := &store.Notification{
		ID:             uuid.New().String(),
		SubscriptionID: sub2ID,
		AgentID:        agent2ID,
		ProjectID:      projectID,
		SubscriberType: store.SubscriberTypeUser,
		SubscriberID:   "user-by-agent",
		Status:         "COMPLETED",
		Message:        "agent2 completed",
	}
	require.NoError(t, s.CreateNotification(ctx, n3))

	// GetNotificationsByAgent for agent1 — all
	notifs, err := s.GetNotificationsByAgent(ctx, agent1ID, store.SubscriberTypeUser, "user-by-agent", false)
	require.NoError(t, err)
	assert.Len(t, notifs, 2)
	assert.Equal(t, n2.ID, notifs[0].ID, "most recent first")
	assert.Equal(t, n1.ID, notifs[1].ID)

	// GetNotificationsByAgent for agent1 — only unacknowledged
	notifs, err = s.GetNotificationsByAgent(ctx, agent1ID, store.SubscriberTypeUser, "user-by-agent", true)
	require.NoError(t, err)
	assert.Len(t, notifs, 1)
	assert.Equal(t, n2.ID, notifs[0].ID)

	// GetNotificationsByAgent for agent2
	notifs, err = s.GetNotificationsByAgent(ctx, agent2ID, store.SubscriberTypeUser, "user-by-agent", false)
	require.NoError(t, err)
	assert.Len(t, notifs, 1)
	assert.Equal(t, n3.ID, notifs[0].ID)

	// GetNotificationsByAgent for non-existent agent
	notifs, err = s.GetNotificationsByAgent(ctx, "no-such-agent", store.SubscriberTypeUser, "user-by-agent", false)
	require.NoError(t, err)
	assert.Empty(t, notifs)
}
