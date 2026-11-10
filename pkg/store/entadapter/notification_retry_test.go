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
	"sort"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Retry backoff, reasons, suspended-subscriber parking and stale expiry of
// agent notifications (ptone/scion#5763).

// retryTestEnv is a notification store with one project for subscriber
// agents.
type retryTestEnv struct {
	client    *ent.Client
	s         *NotificationStore
	projectID uuid.UUID
}

func newRetryTestEnv(t *testing.T) *retryTestEnv {
	t.Helper()
	client := enttest.NewClient(t)
	projectID := uuid.New()
	_, err := client.Project.Create().
		SetID(projectID).
		SetName("Retry Project").
		SetSlug("retry-project").
		Save(context.Background())
	require.NoError(t, err)
	return &retryTestEnv{client: client, s: NewNotificationStore(client), projectID: projectID}
}

// addAgent creates a subscriber agent. An empty phase leaves the column
// NULL, as for agents written without one.
func (e *retryTestEnv) addAgent(t *testing.T, slug, brokerID, phase string) {
	t.Helper()
	c := e.client.Agent.Create().
		SetID(uuid.New()).
		SetSlug(slug).
		SetName(slug).
		SetProjectID(e.projectID).
		SetRuntimeBrokerID(brokerID)
	if phase != "" {
		c = c.SetPhase(phase)
	}
	_, err := c.Save(context.Background())
	require.NoError(t, err)
}

func (e *retryTestEnv) setPhase(t *testing.T, slug, phase string) {
	t.Helper()
	n, err := e.client.Agent.Update().
		Where(agent.SlugEQ(slug), agent.ProjectIDEQ(e.projectID)).
		SetPhase(phase).
		Save(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, n)
}

// addNotif stores a notification for subscriber, created age ago.
func (e *retryTestEnv) addNotif(t *testing.T, subscriberType, subscriber string, age time.Duration, dispatched bool) string {
	t.Helper()
	id := uuid.NewString()
	require.NoError(t, e.s.CreateNotification(context.Background(), &store.Notification{
		ID:             id,
		SubscriptionID: uuid.NewString(),
		AgentID:        uuid.NewString(),
		ProjectID:      e.projectID.String(),
		SubscriberType: subscriberType,
		SubscriberID:   subscriber,
		Status:         "COMPLETED",
		Message:        "done",
		Dispatched:     dispatched,
		CreatedAt:      time.Now().Add(-age),
	}))
	return id
}

func (e *retryTestEnv) get(t *testing.T, id string) *store.Notification {
	t.Helper()
	n, err := e.s.GetNotification(context.Background(), id)
	require.NoError(t, err)
	return n
}

func notifIDs(notifs []store.Notification) []string {
	ids := make([]string, 0, len(notifs))
	for _, n := range notifs {
		ids = append(ids, n.ID)
	}
	sort.Strings(ids)
	return ids
}

func sortedIDs(ids ...string) []string {
	out := append([]string(nil), ids...)
	sort.Strings(out)
	return out
}

// A revert with notBefore stores it as NextAttemptAt; a revert with a zero
// notBefore clears it.
func TestUnmarkNotificationDispatched_NotBefore(t *testing.T) {
	ctx := context.Background()
	s := newTestNotificationStore(t)
	id := createAgentNotification(t, s)

	claimed, err := s.ClaimNotificationForDispatch(ctx, id)
	require.NoError(t, err)
	require.True(t, claimed)

	notBefore := time.Now().Add(10 * time.Minute).Truncate(time.Second)
	reverted, err := s.UnmarkNotificationDispatched(ctx, id, notBefore)
	require.NoError(t, err)
	require.True(t, reverted)

	got, err := s.GetNotification(ctx, id)
	require.NoError(t, err)
	assert.False(t, got.Dispatched)
	require.NotNil(t, got.NextAttemptAt, "notBefore must be stored")
	assert.WithinDuration(t, notBefore, *got.NextAttemptAt, time.Second)

	claimed, err = s.ClaimNotificationForDispatch(ctx, id)
	require.NoError(t, err)
	require.True(t, claimed)
	reverted, err = s.UnmarkNotificationDispatched(ctx, id, time.Time{})
	require.NoError(t, err)
	require.True(t, reverted)

	got, err = s.GetNotification(ctx, id)
	require.NoError(t, err)
	assert.Nil(t, got.NextAttemptAt, "a zero notBefore must clear the backoff")
}

// A failed outcome stores its reason; delivered takes no reason and clears
// a recorded one; a refused write keeps the first reason.
func TestSetNotificationDispatchOutcome_Reason(t *testing.T) {
	ctx := context.Background()
	s := newTestNotificationStore(t)

	id := createAgentNotification(t, s)
	applied, err := s.SetNotificationDispatchOutcome(ctx, id, store.NotificationOutcomeFailed, store.NotificationReasonSubscriberGone)
	require.NoError(t, err)
	require.True(t, applied)
	got, err := s.GetNotification(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, store.NotificationOutcomeFailed, got.DispatchOutcome)
	assert.Equal(t, store.NotificationReasonSubscriberGone, got.DispatchReason)

	// A second failed write is refused and keeps the first reason.
	applied, err = s.SetNotificationDispatchOutcome(ctx, id, store.NotificationOutcomeFailed, store.NotificationReasonExpired)
	require.NoError(t, err)
	assert.False(t, applied)
	got, err = s.GetNotification(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, store.NotificationReasonSubscriberGone, got.DispatchReason)

	// Delivered replaces failed and clears the reason.
	applied, err = s.SetNotificationDispatchOutcome(ctx, id, store.NotificationOutcomeDelivered, "")
	require.NoError(t, err)
	require.True(t, applied)
	got, err = s.GetNotification(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, store.NotificationOutcomeDelivered, got.DispatchOutcome)
	assert.Empty(t, got.DispatchReason, "a delivered outcome has no reason")
}

func TestSetNotificationDispatchOutcome_RejectsBadReason(t *testing.T) {
	ctx := context.Background()
	s := newTestNotificationStore(t)
	id := createAgentNotification(t, s)

	_, err := s.SetNotificationDispatchOutcome(ctx, id, store.NotificationOutcomeDelivered, store.NotificationReasonExpired)
	assert.ErrorIs(t, err, store.ErrInvalidInput, "delivered takes no reason")
	_, err = s.SetNotificationDispatchOutcome(ctx, id, store.NotificationOutcomeFailed, "suspended")
	assert.ErrorIs(t, err, store.ErrInvalidInput, "an unknown reason is rejected")

	got, err := s.GetNotification(ctx, id)
	require.NoError(t, err)
	assert.False(t, got.Dispatched, "a rejected outcome must write nothing")
	assert.Empty(t, got.DispatchOutcome)
	assert.Empty(t, got.DispatchReason)
}

// A suspended subscriber's notifications are left out of both the sweep
// and the broker-connect query, a NULL phase is not suspended, and the
// notifications are selected again once the agent leaves the suspended
// phase.
func TestGetUndispatchedAgentNotifications_SuspendedSubscriberParked(t *testing.T) {
	ctx := context.Background()
	e := newRetryTestEnv(t)
	const broker = "broker-1"
	e.addAgent(t, "running", broker, string(state.PhaseRunning))
	e.addAgent(t, "suspended", broker, string(state.PhaseSuspended))
	e.addAgent(t, "no-phase", broker, "")

	running := e.addNotif(t, store.SubscriberTypeAgent, "running", 2*time.Minute, false)
	suspended := e.addNotif(t, store.SubscriberTypeAgent, "suspended", 2*time.Minute, false)
	noPhase := e.addNotif(t, store.SubscriberTypeAgent, "no-phase", 2*time.Minute, false)

	for _, mode := range []string{"", broker} {
		got, err := e.s.GetUndispatchedAgentNotifications(ctx, mode)
		require.NoError(t, err)
		assert.Equal(t, sortedIDs(running, noPhase), notifIDs(got),
			"mode %q: a suspended subscriber's notification must be parked", mode)
	}

	e.setPhase(t, "suspended", string(state.PhaseRunning))
	for _, mode := range []string{"", broker} {
		got, err := e.s.GetUndispatchedAgentNotifications(ctx, mode)
		require.NoError(t, err)
		assert.Equal(t, sortedIDs(running, suspended, noPhase), notifIDs(got),
			"mode %q: a resumed subscriber's notification must be selected again", mode)
	}
}

// A notification in retry backoff is left out of the sweep until its
// NextAttemptAt, and the broker-connect drain ignores the backoff.
func TestGetUndispatchedAgentNotifications_Backoff(t *testing.T) {
	ctx := context.Background()
	e := newRetryTestEnv(t)
	const broker = "broker-1"
	e.addAgent(t, "sub", broker, string(state.PhaseRunning))
	id := e.addNotif(t, store.SubscriberTypeAgent, "sub", 2*time.Minute, true)

	reverted, err := e.s.UnmarkNotificationDispatched(ctx, id, time.Now().Add(5*time.Minute))
	require.NoError(t, err)
	require.True(t, reverted)

	sweep, err := e.s.GetUndispatchedAgentNotifications(ctx, "")
	require.NoError(t, err)
	assert.Empty(t, sweep, "the sweep must wait for next_attempt_at")

	drain, err := e.s.GetUndispatchedAgentNotifications(ctx, broker)
	require.NoError(t, err)
	assert.Equal(t, []string{id}, notifIDs(drain), "the broker-connect drain ignores the backoff")

	claimed, err := e.s.ClaimNotificationForDispatch(ctx, id)
	require.NoError(t, err)
	require.True(t, claimed)
	reverted, err = e.s.UnmarkNotificationDispatched(ctx, id, time.Now().Add(-time.Second))
	require.NoError(t, err)
	require.True(t, reverted)

	sweep, err = e.s.GetUndispatchedAgentNotifications(ctx, "")
	require.NoError(t, err)
	assert.Equal(t, []string{id}, notifIDs(sweep), "a passed next_attempt_at is eligible")
}

// Expiry makes every pending agent notification older than the cutoff
// failed with reason expired, whatever the subscriber's state (suspended,
// no broker, or running), and leaves young, claimed, user-targeted and
// already-decided rows alone (ptone/scion#5030).
func TestExpireStaleAgentNotifications(t *testing.T) {
	ctx := context.Background()
	e := newRetryTestEnv(t)
	e.addAgent(t, "suspended", "broker-1", string(state.PhaseSuspended))
	e.addAgent(t, "no-broker", "", string(state.PhaseRunning))
	e.addAgent(t, "running", "broker-1", string(state.PhaseRunning))

	const old = 25 * time.Hour
	staleSuspended := e.addNotif(t, store.SubscriberTypeAgent, "suspended", old, false)
	staleNoBroker := e.addNotif(t, store.SubscriberTypeAgent, "no-broker", old, false)
	staleRunning := e.addNotif(t, store.SubscriberTypeAgent, "running", old, false)
	young := e.addNotif(t, store.SubscriberTypeAgent, "suspended", time.Hour, false)
	inFlight := e.addNotif(t, store.SubscriberTypeAgent, "running", old, true)
	user := e.addNotif(t, store.SubscriberTypeUser, "user-1", old, false)

	delivered := e.addNotif(t, store.SubscriberTypeAgent, "running", old, false)
	applied, err := e.s.SetNotificationDispatchOutcome(ctx, delivered, store.NotificationOutcomeDelivered, "")
	require.NoError(t, err)
	require.True(t, applied)
	gone := e.addNotif(t, store.SubscriberTypeAgent, "running", old, false)
	applied, err = e.s.SetNotificationDispatchOutcome(ctx, gone, store.NotificationOutcomeFailed, store.NotificationReasonSubscriptionGone)
	require.NoError(t, err)
	require.True(t, applied)

	n, err := e.s.ExpireStaleAgentNotifications(ctx, time.Now().Add(-24*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 3, n, "exactly the three stale pending agent notifications expire")

	for _, id := range []string{staleSuspended, staleNoBroker, staleRunning} {
		got := e.get(t, id)
		assert.True(t, got.Dispatched, "an expired notification is terminal")
		assert.Equal(t, store.NotificationOutcomeFailed, got.DispatchOutcome)
		assert.Equal(t, store.NotificationReasonExpired, got.DispatchReason)
		assert.Zero(t, got.DispatchAttempts, "expiry is not a delivery attempt")
	}

	got := e.get(t, young)
	assert.False(t, got.Dispatched, "a young notification stays pending")
	assert.Empty(t, got.DispatchOutcome)

	got = e.get(t, inFlight)
	assert.True(t, got.Dispatched)
	assert.Empty(t, got.DispatchOutcome, "a claimed notification in flight is left to its claimer")

	got = e.get(t, user)
	assert.False(t, got.Dispatched, "a user-targeted notification is not expired")
	assert.Empty(t, got.DispatchOutcome)

	got = e.get(t, delivered)
	assert.Equal(t, store.NotificationOutcomeDelivered, got.DispatchOutcome, "a delivered outcome is kept")
	assert.Empty(t, got.DispatchReason)

	got = e.get(t, gone)
	assert.Equal(t, store.NotificationOutcomeFailed, got.DispatchOutcome)
	assert.Equal(t, store.NotificationReasonSubscriptionGone, got.DispatchReason, "a recorded reason is kept")

	n, err = e.s.ExpireStaleAgentNotifications(ctx, time.Now().Add(-24*time.Hour))
	require.NoError(t, err)
	assert.Zero(t, n, "a second pass expires nothing")
}
