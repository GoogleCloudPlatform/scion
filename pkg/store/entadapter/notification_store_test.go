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

package entadapter

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestNotificationStore(t *testing.T) *NotificationStore {
	t.Helper()
	client := enttest.NewClient(t)
	return NewNotificationStore(client)
}

func TestNotificationStore_SubscriptionCRUD(t *testing.T) {
	ctx := context.Background()
	s := newTestNotificationStore(t)

	projectID := uuid.NewString()
	agentID := uuid.NewString()
	sub := &store.NotificationSubscription{
		ID:                uuid.NewString(),
		Scope:             store.SubscriptionScopeAgent,
		AgentID:           agentID,
		SubscriberType:    "user",
		SubscriberID:      "user-1",
		ProjectID:         projectID,
		TriggerActivities: []string{"COMPLETED", "WAITING_FOR_INPUT"},
		CreatedBy:         "tester",
	}
	require.NoError(t, s.CreateNotificationSubscription(ctx, sub))

	got, err := s.GetNotificationSubscription(ctx, sub.ID)
	require.NoError(t, err)
	assert.Equal(t, sub.ID, got.ID)
	assert.Equal(t, agentID, got.AgentID)
	assert.Equal(t, []string{"COMPLETED", "WAITING_FOR_INPUT"}, got.TriggerActivities)
	assert.False(t, got.CreatedAt.IsZero())

	// Update triggers.
	require.NoError(t, s.UpdateNotificationSubscriptionTriggers(ctx, sub.ID, []string{"FAILED"}))
	got, err = s.GetNotificationSubscription(ctx, sub.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{"FAILED"}, got.TriggerActivities)

	// Query helpers.
	byAgent, err := s.GetNotificationSubscriptions(ctx, agentID)
	require.NoError(t, err)
	assert.Len(t, byAgent, 1)

	byProject, err := s.GetNotificationSubscriptionsByProject(ctx, projectID)
	require.NoError(t, err)
	assert.Len(t, byProject, 1)

	bySubscriber, err := s.GetSubscriptionsForSubscriber(ctx, "user", "user-1")
	require.NoError(t, err)
	assert.Len(t, bySubscriber, 1)

	// Delete.
	require.NoError(t, s.DeleteNotificationSubscription(ctx, sub.ID))
	_, err = s.GetNotificationSubscription(ctx, sub.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestNotificationStore_ProjectScopedSubscription(t *testing.T) {
	ctx := context.Background()
	s := newTestNotificationStore(t)

	projectID := uuid.NewString()
	sub := &store.NotificationSubscription{
		ID:                uuid.NewString(),
		Scope:             store.SubscriptionScopeProject,
		SubscriberType:    "user",
		SubscriberID:      "user-1",
		ProjectID:         projectID,
		TriggerActivities: []string{"COMPLETED"},
		CreatedBy:         "tester",
	}
	require.NoError(t, s.CreateNotificationSubscription(ctx, sub))
	assert.Empty(t, sub.AgentID, "project scope must clear agent id")

	scoped, err := s.GetNotificationSubscriptionsByProjectScope(ctx, projectID)
	require.NoError(t, err)
	require.Len(t, scoped, 1)
	assert.Empty(t, scoped[0].AgentID)
}

func TestNotificationStore_SubscriptionValidation(t *testing.T) {
	ctx := context.Background()
	s := newTestNotificationStore(t)

	// Missing required fields.
	assert.ErrorIs(t, s.CreateNotificationSubscription(ctx, &store.NotificationSubscription{}), store.ErrInvalidInput)

	// Agent scope without agent id.
	assert.ErrorIs(t, s.CreateNotificationSubscription(ctx, &store.NotificationSubscription{
		ID: uuid.NewString(), Scope: store.SubscriptionScopeAgent,
		SubscriberID: "u", ProjectID: uuid.NewString(),
	}), store.ErrInvalidInput)
}

func TestNotificationStore_NotificationLifecycle(t *testing.T) {
	ctx := context.Background()
	s := newTestNotificationStore(t)

	projectID := uuid.NewString()
	agentID := uuid.NewString()
	subID := uuid.NewString()

	notif := &store.Notification{
		ID:             uuid.NewString(),
		SubscriptionID: subID,
		AgentID:        agentID,
		ProjectID:      projectID,
		SubscriberType: "user",
		SubscriberID:   "user-1",
		Status:         "COMPLETED",
		Message:        "agent done",
	}
	require.NoError(t, s.CreateNotification(ctx, notif))

	list, err := s.GetNotifications(ctx, "user", "user-1", false)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.False(t, list[0].Acknowledged)
	assert.False(t, list[0].Dispatched)

	byAgent, err := s.GetNotificationsByAgent(ctx, agentID, "user", "user-1", true)
	require.NoError(t, err)
	require.Len(t, byAgent, 1)

	// Dispatch claim.
	require.NoError(t, s.MarkNotificationDispatched(ctx, notif.ID))

	// Acknowledge.
	require.NoError(t, s.AcknowledgeNotification(ctx, notif.ID))
	unack, err := s.GetNotifications(ctx, "user", "user-1", true)
	require.NoError(t, err)
	assert.Empty(t, unack)

	// Last status by subscription.
	status, err := s.GetLastNotificationStatus(ctx, subID)
	require.NoError(t, err)
	assert.Equal(t, "COMPLETED", status)

	// No notifications for an unknown subscription -> ("", nil).
	status, err = s.GetLastNotificationStatus(ctx, uuid.NewString())
	require.NoError(t, err)
	assert.Equal(t, "", status)
}

func TestNotificationStore_AcknowledgeAll(t *testing.T) {
	ctx := context.Background()
	s := newTestNotificationStore(t)

	for i := 0; i < 3; i++ {
		require.NoError(t, s.CreateNotification(ctx, &store.Notification{
			ID:             uuid.NewString(),
			SubscriptionID: uuid.NewString(),
			AgentID:        uuid.NewString(),
			ProjectID:      uuid.NewString(),
			SubscriberType: "user",
			SubscriberID:   "user-1",
			Status:         "COMPLETED",
			Message:        "m",
		}))
	}
	require.NoError(t, s.AcknowledgeAllNotifications(ctx, "user", "user-1"))
	unack, err := s.GetNotifications(ctx, "user", "user-1", true)
	require.NoError(t, err)
	assert.Empty(t, unack)
}

// TestNotificationStore_DispatchClaimIsExclusive verifies the multi-replica
// concurrency primitive: many concurrent MarkNotificationDispatched calls for
// the same notification must result in exactly one publisher "win".
func TestNotificationStore_DispatchClaimIsExclusive(t *testing.T) {
	ctx := context.Background()

	client := enttest.NewClient(t)

	pub := &countingPublisher{}
	s := NewNotificationStore(client).WithPublisher(pub)

	notifID := uuid.NewString()
	require.NoError(t, s.CreateNotification(ctx, &store.Notification{
		ID:             notifID,
		SubscriptionID: uuid.NewString(),
		AgentID:        uuid.NewString(),
		ProjectID:      uuid.NewString(),
		SubscriberType: "user",
		SubscriberID:   "user-1",
		Status:         "COMPLETED",
		Message:        "m",
	}))

	const racers = 8
	var wg sync.WaitGroup
	wg.Add(racers)
	for i := 0; i < racers; i++ {
		go func() {
			defer wg.Done()
			_ = s.MarkNotificationDispatched(ctx, notifID)
		}()
	}
	wg.Wait()

	// Exactly one dispatch event should have been published despite the race.
	assert.Equal(t, 1, pub.count(NotificationEventDispatched), "dispatch must be claimed exactly once")

	// Marking an unknown notification returns ErrNotFound.
	assert.ErrorIs(t, s.MarkNotificationDispatched(ctx, uuid.NewString()), store.ErrNotFound)
}

func TestNotificationStore_TemplateCRUD(t *testing.T) {
	ctx := context.Background()
	s := newTestNotificationStore(t)

	projectID := uuid.NewString()

	global := &store.SubscriptionTemplate{
		ID:                uuid.NewString(),
		Name:              "all-events",
		Scope:             "project",
		TriggerActivities: []string{"COMPLETED", "FAILED"},
		CreatedBy:         "tester",
	}
	require.NoError(t, s.CreateSubscriptionTemplate(ctx, global))

	scoped := &store.SubscriptionTemplate{
		ID:                uuid.NewString(),
		Name:              "critical",
		Scope:             "project",
		TriggerActivities: []string{"FAILED"},
		ProjectID:         projectID,
		CreatedBy:         "tester",
	}
	require.NoError(t, s.CreateSubscriptionTemplate(ctx, scoped))

	got, err := s.GetSubscriptionTemplate(ctx, scoped.ID)
	require.NoError(t, err)
	assert.Equal(t, projectID, got.ProjectID)
	assert.Equal(t, []string{"FAILED"}, got.TriggerActivities)

	// Global-only listing.
	globalOnly, err := s.ListSubscriptionTemplates(ctx, "")
	require.NoError(t, err)
	require.Len(t, globalOnly, 1)
	assert.Equal(t, "all-events", globalOnly[0].Name)
	assert.Empty(t, globalOnly[0].ProjectID)

	// Project listing includes global + project-specific.
	withProject, err := s.ListSubscriptionTemplates(ctx, projectID)
	require.NoError(t, err)
	assert.Len(t, withProject, 2)

	require.NoError(t, s.DeleteSubscriptionTemplate(ctx, scoped.ID))
	_, err = s.GetSubscriptionTemplate(ctx, scoped.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// countingPublisher is a thread-safe NotificationPublisher test double.
type countingPublisher struct {
	mu     sync.Mutex
	counts map[NotificationEventType]int
}

func (p *countingPublisher) PublishNotification(_ context.Context, evt NotificationEvent) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.counts == nil {
		p.counts = make(map[NotificationEventType]int)
	}
	p.counts[evt.Type]++
	return nil
}

func (p *countingPublisher) count(t NotificationEventType) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.counts[t]
}

// ---------------------------------------------------------------------------
// ClaimNotificationForDispatch tests
// ---------------------------------------------------------------------------

func TestClaimNotificationForDispatch_CAS(t *testing.T) {
	ctx := context.Background()
	s := newTestNotificationStore(t)

	notifID := uuid.NewString()
	require.NoError(t, s.CreateNotification(ctx, &store.Notification{
		ID:             notifID,
		SubscriptionID: uuid.NewString(),
		AgentID:        uuid.NewString(),
		ProjectID:      uuid.NewString(),
		SubscriberType: "agent",
		SubscriberID:   "some-agent",
		Status:         "COMPLETED",
		Message:        "done",
	}))

	// First claim should succeed.
	claimed, err := s.ClaimNotificationForDispatch(ctx, notifID)
	require.NoError(t, err)
	assert.True(t, claimed, "first claim should succeed")

	// Second claim should fail (already dispatched).
	claimed2, err := s.ClaimNotificationForDispatch(ctx, notifID)
	require.NoError(t, err)
	assert.False(t, claimed2, "second claim must fail")

	// Unknown notification returns false, nil.
	claimed3, err := s.ClaimNotificationForDispatch(ctx, uuid.NewString())
	require.NoError(t, err)
	assert.False(t, claimed3, "unknown notification must not be claimed")
}

func TestClaimNotificationForDispatch_Concurrent(t *testing.T) {
	ctx := context.Background()
	s := newTestNotificationStore(t)

	notifID := uuid.NewString()
	require.NoError(t, s.CreateNotification(ctx, &store.Notification{
		ID:             notifID,
		SubscriptionID: uuid.NewString(),
		AgentID:        uuid.NewString(),
		ProjectID:      uuid.NewString(),
		SubscriberType: "agent",
		SubscriberID:   "some-agent",
		Status:         "COMPLETED",
		Message:        "done",
	}))

	const racers = 8
	wins := make(chan bool, racers)
	var wg sync.WaitGroup
	wg.Add(racers)
	for i := 0; i < racers; i++ {
		go func() {
			defer wg.Done()
			claimed, err := s.ClaimNotificationForDispatch(ctx, notifID)
			if err == nil {
				wins <- claimed
			}
		}()
	}
	wg.Wait()
	close(wins)

	var winCount int
	for w := range wins {
		if w {
			winCount++
		}
	}
	assert.Equal(t, 1, winCount, "exactly one racer must win the claim")
}

// ---------------------------------------------------------------------------
// UnmarkNotificationDispatched tests
// ---------------------------------------------------------------------------

func TestUnmarkNotificationDispatched(t *testing.T) {
	ctx := context.Background()
	s := newTestNotificationStore(t)

	notifID := uuid.NewString()
	require.NoError(t, s.CreateNotification(ctx, &store.Notification{
		ID:             notifID,
		SubscriptionID: uuid.NewString(),
		AgentID:        uuid.NewString(),
		ProjectID:      uuid.NewString(),
		SubscriberType: "agent",
		SubscriberID:   "some-agent",
		Status:         "COMPLETED",
		Message:        "done",
	}))

	// Mark dispatched.
	require.NoError(t, s.MarkNotificationDispatched(ctx, notifID))

	// Unmark.
	reverted, err := s.UnmarkNotificationDispatched(ctx, notifID, time.Time{})
	require.NoError(t, err)
	assert.True(t, reverted, "a row with no outcome must be reverted")

	// Should be claimable again.
	claimed, err := s.ClaimNotificationForDispatch(ctx, notifID)
	require.NoError(t, err)
	assert.True(t, claimed, "notification should be claimable after unmark")
}

// ---------------------------------------------------------------------------
// Dispatch attempts and outcome tests (ptone/scion#4800)
// ---------------------------------------------------------------------------

// createAgentNotification stores an undispatched agent-targeted notification
// and returns its ID.
func createAgentNotification(t *testing.T, s *NotificationStore) string {
	t.Helper()
	id := uuid.NewString()
	require.NoError(t, s.CreateNotification(context.Background(), &store.Notification{
		ID:             id,
		SubscriptionID: uuid.NewString(),
		AgentID:        uuid.NewString(),
		ProjectID:      uuid.NewString(),
		SubscriberType: "agent",
		SubscriberID:   "some-agent",
		Status:         "COMPLETED",
		Message:        "done",
	}))
	return id
}

func TestNotificationDispatchFields_DefaultOnCreate(t *testing.T) {
	ctx := context.Background()
	s := newTestNotificationStore(t)
	id := createAgentNotification(t, s)

	got, err := s.GetNotification(ctx, id)
	require.NoError(t, err)
	assert.Zero(t, got.DispatchAttempts)
	assert.Empty(t, got.DispatchOutcome, "a new notification has no outcome")
	assert.False(t, got.Dispatched)
}

func TestRecordNotificationDispatchAttempt(t *testing.T) {
	ctx := context.Background()
	s := newTestNotificationStore(t)
	id := createAgentNotification(t, s)

	for i := 0; i < 3; i++ {
		require.NoError(t, s.RecordNotificationDispatchAttempt(ctx, id))
	}
	got, err := s.GetNotification(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, 3, got.DispatchAttempts)
	assert.False(t, got.Dispatched, "recording an attempt must not claim the notification")
	assert.Empty(t, got.DispatchOutcome, "recording an attempt must not set an outcome")

	assert.ErrorIs(t, s.RecordNotificationDispatchAttempt(ctx, uuid.NewString()), store.ErrNotFound)
}

func TestRecordNotificationDispatchAttempt_Concurrent(t *testing.T) {
	ctx := context.Background()
	s := newTestNotificationStore(t)
	id := createAgentNotification(t, s)

	const recorders = 8
	var wg sync.WaitGroup
	errs := make(chan error, recorders)
	wg.Add(recorders)
	for i := 0; i < recorders; i++ {
		go func() {
			defer wg.Done()
			errs <- s.RecordNotificationDispatchAttempt(ctx, id)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	got, err := s.GetNotification(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, recorders, got.DispatchAttempts, "the increment must be atomic")
}

func TestSetNotificationDispatchOutcome(t *testing.T) {
	ctx := context.Background()
	s := newTestNotificationStore(t)

	for _, outcome := range []string{store.NotificationOutcomeDelivered, store.NotificationOutcomeFailed} {
		t.Run(outcome, func(t *testing.T) {
			id := createAgentNotification(t, s)
			require.NoError(t, s.RecordNotificationDispatchAttempt(ctx, id))
			applied, err := s.SetNotificationDispatchOutcome(ctx, id, outcome, "")
			require.NoError(t, err)
			assert.True(t, applied, "the first outcome must apply")

			got, err := s.GetNotification(ctx, id)
			require.NoError(t, err)
			assert.Equal(t, outcome, got.DispatchOutcome)
			assert.True(t, got.Dispatched, "an outcome is terminal: the row must be dispatched")
			assert.Equal(t, 1, got.DispatchAttempts, "setting the outcome must keep the attempt count")

			claimed, err := s.ClaimNotificationForDispatch(ctx, id)
			require.NoError(t, err)
			assert.False(t, claimed, "a notification with an outcome must not be claimable for a retry")
		})
	}
}

func TestSetNotificationDispatchOutcome_Rejects(t *testing.T) {
	ctx := context.Background()
	s := newTestNotificationStore(t)
	id := createAgentNotification(t, s)

	for _, bad := range []string{"", "pending", "FAILED"} {
		_, err := s.SetNotificationDispatchOutcome(ctx, id, bad, "")
		assert.ErrorIs(t, err, store.ErrInvalidInput, "outcome %q", bad)
	}
	got, err := s.GetNotification(ctx, id)
	require.NoError(t, err)
	assert.False(t, got.Dispatched, "a rejected outcome must write nothing")
	assert.Empty(t, got.DispatchOutcome)

	for _, outcome := range []string{store.NotificationOutcomeDelivered, store.NotificationOutcomeFailed} {
		_, err = s.SetNotificationDispatchOutcome(ctx, uuid.NewString(), outcome, "")
		assert.ErrorIs(t, err, store.ErrNotFound, "outcome %q on a missing row", outcome)
	}
}

// Once an outcome is recorded, a revert is refused and the row stays
// dispatched with its outcome, so an empty outcome still means pending or in
// flight and the sweep does not deliver it again (ptone/scion#5030).
func TestUnmarkNotificationDispatched_RefusedAfterOutcome(t *testing.T) {
	ctx := context.Background()
	s := newTestNotificationStore(t)

	for _, outcome := range []string{store.NotificationOutcomeDelivered, store.NotificationOutcomeFailed} {
		t.Run(outcome, func(t *testing.T) {
			id := createAgentNotification(t, s)
			claimed, err := s.ClaimNotificationForDispatch(ctx, id)
			require.NoError(t, err)
			require.True(t, claimed)
			applied, err := s.SetNotificationDispatchOutcome(ctx, id, outcome, "")
			require.NoError(t, err)
			require.True(t, applied)

			reverted, err := s.UnmarkNotificationDispatched(ctx, id, time.Time{})
			require.NoError(t, err, "a refused revert is not an error")
			assert.False(t, reverted, "a row with an outcome must not be reverted")

			got, err := s.GetNotification(ctx, id)
			require.NoError(t, err)
			assert.True(t, got.Dispatched, "a row with an outcome must stay dispatched")
			assert.Equal(t, outcome, got.DispatchOutcome)

			claimed, err = s.ClaimNotificationForDispatch(ctx, id)
			require.NoError(t, err)
			assert.False(t, claimed, "a row with an outcome must not be claimable again")
		})
	}

	reverted, err := s.UnmarkNotificationDispatched(ctx, uuid.NewString(), time.Time{})
	require.NoError(t, err)
	assert.False(t, reverted, "a missing row is not reverted")
}

// Racing outcome writes keep the most informative outcome: delivered
// replaces failed (the message did reach the subscriber), and nothing
// replaces delivered or re-applies failed. A refused write returns no
// error and leaves the recorded outcome in place (ptone/scion#5030).
func TestSetNotificationDispatchOutcome_DeliveredBeatsFailed(t *testing.T) {
	ctx := context.Background()
	s := newTestNotificationStore(t)

	cases := []struct {
		first, second string
		wantApplied   bool
		want          string
	}{
		{store.NotificationOutcomeDelivered, store.NotificationOutcomeFailed, false, store.NotificationOutcomeDelivered},
		{store.NotificationOutcomeFailed, store.NotificationOutcomeDelivered, true, store.NotificationOutcomeDelivered},
		{store.NotificationOutcomeDelivered, store.NotificationOutcomeDelivered, false, store.NotificationOutcomeDelivered},
		{store.NotificationOutcomeFailed, store.NotificationOutcomeFailed, false, store.NotificationOutcomeFailed},
	}
	for _, tc := range cases {
		t.Run(tc.first+"_then_"+tc.second, func(t *testing.T) {
			id := createAgentNotification(t, s)
			applied, err := s.SetNotificationDispatchOutcome(ctx, id, tc.first, "")
			require.NoError(t, err)
			require.True(t, applied, "the first outcome must apply")

			applied, err = s.SetNotificationDispatchOutcome(ctx, id, tc.second, "")
			require.NoError(t, err, "a refused second outcome is not an error")
			assert.Equal(t, tc.wantApplied, applied, "whether the second outcome applies")

			got, err := s.GetNotification(ctx, id)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got.DispatchOutcome, "the recorded outcome")
			assert.True(t, got.Dispatched)
		})
	}
}

// ---------------------------------------------------------------------------
// GetUndispatchedAgentNotifications tests
// ---------------------------------------------------------------------------

func TestGetUndispatchedAgentNotifications_SweepMode(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	s := NewNotificationStore(client)

	projectID := uuid.New()
	oldTime := time.Now().Add(-2 * time.Minute) // beyond 60s grace period

	// Create project and agent records required for the sweep-mode EXISTS
	// subquery (which filters for agents with a non-empty RuntimeBrokerID).
	_, err := client.Project.Create().
		SetID(projectID).
		SetName("Sweep Project").
		SetSlug("sweep-project").
		Save(ctx)
	require.NoError(t, err)
	_, err = client.Agent.Create().
		SetID(uuid.New()).
		SetSlug("agent-a").
		SetName("Agent A").
		SetProjectID(projectID).
		SetRuntimeBrokerID("some-broker").
		Save(ctx)
	require.NoError(t, err)

	// Create an old undispatched agent notification (beyond 60s grace period).
	require.NoError(t, s.CreateNotification(ctx, &store.Notification{
		ID:             uuid.NewString(),
		SubscriptionID: uuid.NewString(),
		AgentID:        uuid.NewString(),
		ProjectID:      projectID.String(),
		SubscriberType: store.SubscriberTypeAgent,
		SubscriberID:   "agent-a",
		Status:         "COMPLETED",
		Message:        "done",
		CreatedAt:      oldTime, // set at creation (immutable field)
	}))

	// Create a recent undispatched agent notification (within grace period).
	require.NoError(t, s.CreateNotification(ctx, &store.Notification{
		ID:             uuid.NewString(),
		SubscriptionID: uuid.NewString(),
		AgentID:        uuid.NewString(),
		ProjectID:      projectID.String(),
		SubscriberType: store.SubscriberTypeAgent,
		SubscriberID:   "agent-b",
		Status:         "COMPLETED",
		Message:        "also done",
		// CreatedAt defaults to now (within grace period)
	}))

	// Create a dispatched agent notification (should be excluded).
	require.NoError(t, s.CreateNotification(ctx, &store.Notification{
		ID:             uuid.NewString(),
		SubscriptionID: uuid.NewString(),
		AgentID:        uuid.NewString(),
		ProjectID:      projectID.String(),
		SubscriberType: store.SubscriberTypeAgent,
		SubscriberID:   "agent-c",
		Status:         "COMPLETED",
		Message:        "dispatched",
		Dispatched:     true,
		CreatedAt:      oldTime,
	}))

	// Create an undispatched USER notification (should be excluded).
	require.NoError(t, s.CreateNotification(ctx, &store.Notification{
		ID:             uuid.NewString(),
		SubscriptionID: uuid.NewString(),
		AgentID:        uuid.NewString(),
		ProjectID:      projectID.String(),
		SubscriberType: "user",
		SubscriberID:   "user-1",
		Status:         "COMPLETED",
		Message:        "user notif",
		CreatedAt:      oldTime,
	}))

	// Sweep mode (empty brokerID)
	notifs, err := s.GetUndispatchedAgentNotifications(ctx, "")
	require.NoError(t, err)

	// Should only return the old agent notification
	require.Len(t, notifs, 1)
	assert.Equal(t, "agent-a", notifs[0].SubscriberID)
	assert.False(t, notifs[0].Dispatched)
}

func TestGetUndispatchedAgentNotifications_BrokerFilter(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	s := NewNotificationStore(client)

	projectID := uuid.New()
	brokerID := "broker-for-test"

	// Create a project (required as FK for agents).
	_, err := client.Project.Create().
		SetID(projectID).
		SetName("Test Project").
		SetSlug("test-project").
		Save(ctx)
	require.NoError(t, err)

	// Create an agent with the target broker.
	agentID := uuid.New()
	_, err = client.Agent.Create().
		SetID(agentID).
		SetSlug("agent-on-broker").
		SetName("Agent On Broker").
		SetProjectID(projectID).
		SetRuntimeBrokerID(brokerID).
		Save(ctx)
	require.NoError(t, err)

	// Create another agent with a different broker.
	otherAgentID := uuid.New()
	_, err = client.Agent.Create().
		SetID(otherAgentID).
		SetSlug("agent-other-broker").
		SetName("Agent Other Broker").
		SetProjectID(projectID).
		SetRuntimeBrokerID("different-broker").
		Save(ctx)
	require.NoError(t, err)

	// Create undispatched notifications for both agents, backdated past grace period.
	oldTime := time.Now().Add(-2 * time.Minute)

	require.NoError(t, s.CreateNotification(ctx, &store.Notification{
		ID:             uuid.NewString(),
		SubscriptionID: uuid.NewString(),
		AgentID:        uuid.NewString(),
		ProjectID:      projectID.String(),
		SubscriberType: store.SubscriberTypeAgent,
		SubscriberID:   "agent-on-broker",
		Status:         "COMPLETED",
		Message:        "msg1",
		CreatedAt:      oldTime,
	}))

	require.NoError(t, s.CreateNotification(ctx, &store.Notification{
		ID:             uuid.NewString(),
		SubscriptionID: uuid.NewString(),
		AgentID:        uuid.NewString(),
		ProjectID:      projectID.String(),
		SubscriberType: store.SubscriberTypeAgent,
		SubscriberID:   "agent-other-broker",
		Status:         "COMPLETED",
		Message:        "msg2",
		CreatedAt:      oldTime,
	}))

	// Filter by brokerID.
	notifs, err := s.GetUndispatchedAgentNotifications(ctx, brokerID)
	require.NoError(t, err)
	require.Len(t, notifs, 1, "should only return notifications for agents on the target broker")
	assert.Equal(t, "agent-on-broker", notifs[0].SubscriberID)

	// Filter by the other broker.
	notifs2, err := s.GetUndispatchedAgentNotifications(ctx, "different-broker")
	require.NoError(t, err)
	require.Len(t, notifs2, 1)
	assert.Equal(t, "agent-other-broker", notifs2[0].SubscriberID)

	// Sweep mode (all) returns both.
	all, err := s.GetUndispatchedAgentNotifications(ctx, "")
	require.NoError(t, err)
	assert.Len(t, all, 2)
}

func TestPurgeOrphanedNotifications(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	s := NewNotificationStore(client)

	projectID := uuid.New()
	_, err := client.Project.Create().SetID(projectID).SetName("GC Project").SetSlug("gc-project").Save(ctx)
	require.NoError(t, err)
	liveAgentID := uuid.New()
	_, err = client.Agent.Create().SetID(liveAgentID).SetSlug("live").SetName("Live").
		SetProjectID(projectID).Save(ctx)
	require.NoError(t, err)
	liveSub := &store.NotificationSubscription{
		ID: uuid.NewString(), Scope: store.SubscriptionScopeAgent, AgentID: liveAgentID.String(),
		SubscriberType: "user", SubscriberID: "user-1", ProjectID: projectID.String(),
		TriggerActivities: []string{"DELETED"}, CreatedBy: "tester",
	}
	require.NoError(t, s.CreateNotificationSubscription(ctx, liveSub))

	newNotif := func(agentID, subID string, acked bool) string {
		t.Helper()
		id := uuid.NewString()
		require.NoError(t, s.CreateNotification(ctx, &store.Notification{
			ID: id, SubscriptionID: subID, AgentID: agentID, ProjectID: projectID.String(),
			SubscriberType: "user", SubscriberID: "user-1", Status: "DELETED", Message: "deleted",
		}))
		if acked {
			require.NoError(t, s.AcknowledgeNotification(ctx, id))
		}
		return id
	}
	goneAgent, goneSub := uuid.NewString(), uuid.NewString()
	orphanAcked := newNotif(goneAgent, goneSub, true)
	orphanUnacked := newNotif(goneAgent, goneSub, false)
	agentLive := newNotif(liveAgentID.String(), goneSub, true)
	subLive := newNotif(goneAgent, liveSub.ID, true)

	purged, err := s.PurgeOrphanedNotifications(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, purged)

	_, err = s.GetNotification(ctx, orphanAcked)
	assert.ErrorIs(t, err, store.ErrNotFound, "acknowledged orphan is removed")
	for name, id := range map[string]string{
		"unacknowledged orphan": orphanUnacked,
		"agent still present":   agentLive,
		"subscription present":  subLive,
	} {
		_, err := s.GetNotification(ctx, id)
		assert.NoError(t, err, "%s must be kept", name)
	}

	purged, err = s.PurgeOrphanedNotifications(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, purged, "a second run removes nothing")
}

func TestPurgeOrphanedNotifications_DeletesInBatches(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	s := NewNotificationStore(client)

	prev := orphanPurgeBatchSize
	orphanPurgeBatchSize = 2
	t.Cleanup(func() { orphanPurgeBatchSize = prev })

	var deletes int
	client.Notification.Use(func(next ent.Mutator) ent.Mutator {
		return ent.MutateFunc(func(ctx context.Context, m ent.Mutation) (ent.Value, error) {
			if m.Op().Is(ent.OpDelete) {
				deletes++
			}
			return next.Mutate(ctx, m)
		})
	})

	projectID := uuid.New()
	_, err := client.Project.Create().SetID(projectID).SetName("Batch Project").SetSlug("batch-project").Save(ctx)
	require.NoError(t, err)
	const orphans = 5
	for i := 0; i < orphans; i++ {
		id := uuid.NewString()
		require.NoError(t, s.CreateNotification(ctx, &store.Notification{
			ID: id, SubscriptionID: uuid.NewString(), AgentID: uuid.NewString(),
			ProjectID: projectID.String(), SubscriberType: "user", SubscriberID: "user-1",
			Status: "DELETED", Message: "deleted",
		}))
		require.NoError(t, s.AcknowledgeNotification(ctx, id))
	}

	purged, err := s.PurgeOrphanedNotifications(ctx)
	require.NoError(t, err)
	assert.Equal(t, orphans, purged, "every orphan is removed across batches")
	assert.Equal(t, 3, deletes, "5 rows in batches of 2 take 3 delete statements")

	purged, err = s.PurgeOrphanedNotifications(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, purged)
}

// A full batch whose delete removes no rows ends the purge instead of
// selecting the same rows again.
func TestPurgeOrphanedNotifications_StopsWhenDeleteRemovesNothing(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	s := NewNotificationStore(client)

	prev := orphanPurgeBatchSize
	orphanPurgeBatchSize = 2
	t.Cleanup(func() { orphanPurgeBatchSize = prev })

	projectID := uuid.New()
	_, err := client.Project.Create().SetID(projectID).SetName("Stuck Project").SetSlug("stuck-project").Save(ctx)
	require.NoError(t, err)
	for i := 0; i < orphanPurgeBatchSize; i++ {
		id := uuid.NewString()
		require.NoError(t, s.CreateNotification(ctx, &store.Notification{
			ID: id, SubscriptionID: uuid.NewString(), AgentID: uuid.NewString(),
			ProjectID: projectID.String(), SubscriberType: "user", SubscriberID: "user-1",
			Status: "DELETED", Message: "deleted",
		}))
		require.NoError(t, s.AcknowledgeNotification(ctx, id))
	}

	// Every delete removes nothing. Past a few attempts, fail the delete
	// so a looping purge returns an error instead of hanging the test.
	var deletes int
	client.Notification.Use(func(next ent.Mutator) ent.Mutator {
		return ent.MutateFunc(func(ctx context.Context, m ent.Mutation) (ent.Value, error) {
			if !m.Op().Is(ent.OpDelete) {
				return next.Mutate(ctx, m)
			}
			deletes++
			if deletes > 5 {
				return nil, errors.New("purge kept deleting the same rows")
			}
			return 0, nil
		})
	})

	purged, err := s.PurgeOrphanedNotifications(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, purged)
	assert.Equal(t, 1, deletes, "the purge stops after one delete that removes nothing")
}
