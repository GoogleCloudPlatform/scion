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

package hub

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Notification delivery to a suspended subscriber, retry backoff and
// expiry of undeliverable notifications (ptone/scion#5763).

// clearRetryBackoff clears a pending notification's retry backoff, standing
// in for the backoff period passing.
func clearRetryBackoff(t *testing.T, env *notificationTestEnv, id string) {
	t.Helper()
	reverted, err := env.store.UnmarkNotificationDispatched(context.Background(), id, time.Time{})
	require.NoError(t, err)
	require.True(t, reverted, "clearRetryBackoff needs a notification with no outcome")
}

// setAgentPhase stores a new phase for agent.
func setAgentPhase(t *testing.T, env *notificationTestEnv, agent *store.Agent, phase state.Phase) {
	t.Helper()
	agent.Phase = string(phase)
	require.NoError(t, env.store.UpdateAgent(context.Background(), agent))
}

// sweepServer returns a Server that runs env's sweep.
func sweepServer(env *notificationTestEnv) *Server {
	return &Server{
		store:                  env.store,
		agentLifecycleLog:      slog.Default(),
		notificationDispatcher: env.nd,
	}
}

// assertPendingUntouched checks a notification is still pending exactly as
// stored: not claimed, no outcome, no attempt, no backoff.
func assertPendingUntouched(t *testing.T, env *notificationTestEnv, id string) {
	t.Helper()
	n := getNotification(t, env, id)
	assert.False(t, n.Dispatched, "the notification must stay pending")
	assert.Empty(t, n.DispatchOutcome)
	assert.Empty(t, n.DispatchReason)
	assert.Zero(t, n.DispatchAttempts, "no delivery may be attempted")
	assert.Nil(t, n.NextAttemptAt, "no retry backoff is set")
}

// The primary dispatch makes no delivery attempt to a suspended subscriber
// and leaves the notification pending.
func TestDispatchToAgent_SuspendedSubscriber_Parked(t *testing.T) {
	env := setupNotificationTest(t)
	setAgentPhase(t, env, env.subscriber, state.PhaseSuspended)
	id := createUndispatchedNotification(t, env, env.sub, 0)

	env.nd.dispatchToAgent(context.Background(), env.sub, getNotification(t, env, id), env.watched.ID, env.watched.Slug)

	assert.Empty(t, env.dispatcher.getCalls(), "a suspended subscriber must not be sent anything")
	assertPendingUntouched(t, env, id)
}

// The sweep neither claims nor attempts a suspended subscriber's
// notification; after the agent resumes, the next sweep delivers it.
func TestSweepHandler_SuspendedSubscriber_DeliveredAfterResume(t *testing.T) {
	env := setupNotificationTest(t)
	setAgentPhase(t, env, env.subscriber, state.PhaseSuspended)
	id := createUndispatchedNotification(t, env, env.sub, 2*time.Minute)
	handler := sweepServer(env).notificationDispatchSweepHandler()

	handler(context.Background())
	handler(context.Background())
	assert.Empty(t, env.dispatcher.getCalls(), "a suspended subscriber must not be sent anything")
	assertPendingUntouched(t, env, id)

	setAgentPhase(t, env, env.subscriber, state.PhaseRunning)
	handler(context.Background())

	require.Len(t, env.dispatcher.getCalls(), 1, "the first sweep after the resume must deliver")
	n := getNotification(t, env, id)
	assert.True(t, n.Dispatched)
	assert.Equal(t, store.NotificationOutcomeDelivered, n.DispatchOutcome)
	assert.Equal(t, 1, n.DispatchAttempts)
}

// A subscriber suspended after the sweep selected its notification is
// caught after the claim: the claim is released with no attempt counted
// and no backoff.
func TestRetryDispatch_SubscriberSuspendedAfterSelection_NoAttempt(t *testing.T) {
	env := setupRetryTest(t)
	ctx := context.Background()

	notifs, err := env.store.GetUndispatchedAgentNotifications(ctx, "")
	require.NoError(t, err)
	require.Len(t, notifs, 1)

	setAgentPhase(t, env, env.subscriber, state.PhaseSuspended)
	env.nd.RetryDispatch(ctx, &notifs[0])

	assert.Empty(t, env.dispatcher.getCalls(), "a suspended subscriber must not be sent anything")
	assertPendingUntouched(t, env, notifs[0].ID)
}

// A pending notification past notificationRetryMaxAge ends as failed with
// reason expired on the next sweep, with no delivery attempt, whether its
// subscriber stays suspended or has no runtime broker; a notification that
// already has an outcome keeps it (ptone/scion#5030).
func TestSweepHandler_ExpiresStalePendingNotifications(t *testing.T) {
	env := setupNotificationTest(t)
	ctx := context.Background()
	stale := notificationRetryMaxAge + time.Hour

	setAgentPhase(t, env, env.subscriber, state.PhaseSuspended)
	suspended := createUndispatchedNotification(t, env, env.sub, stale)

	noBroker, noBrokerSub := addSubscriberOnBroker(t, env, "no-broker")
	noBroker.RuntimeBrokerID = ""
	require.NoError(t, env.store.UpdateAgent(ctx, noBroker))
	brokerless := createUndispatchedNotification(t, env, noBrokerSub, stale)

	_, runningSub := addSubscriberOnBroker(t, env, "running")
	delivered := createUndispatchedNotification(t, env, runningSub, stale)
	applied, err := env.store.SetNotificationDispatchOutcome(ctx, delivered, store.NotificationOutcomeDelivered, "")
	require.NoError(t, err)
	require.True(t, applied)

	young := createUndispatchedNotification(t, env, env.sub, time.Hour)

	sweepServer(env).notificationDispatchSweepHandler()(ctx)

	assert.Empty(t, env.dispatcher.getCalls(), "expiry must not attempt delivery")
	for name, id := range map[string]string{"suspended": suspended, "no broker": brokerless} {
		n := getNotification(t, env, id)
		assert.True(t, n.Dispatched, "%s: an expired notification is terminal", name)
		assert.Equal(t, store.NotificationOutcomeFailed, n.DispatchOutcome, name)
		assert.Equal(t, store.NotificationReasonExpired, n.DispatchReason, name)
		assert.Zero(t, n.DispatchAttempts, "%s: no delivery was attempted", name)
	}

	n := getNotification(t, env, delivered)
	assert.Equal(t, store.NotificationOutcomeDelivered, n.DispatchOutcome, "a recorded outcome must be kept")
	assert.Empty(t, n.DispatchReason)

	assertPendingUntouched(t, env, young)
}

// Each failed delivery releases the claim with a longer retry backoff:
// 5 minutes after the first attempt, 10 after the second.
func TestRetryDispatch_DeliveryFailure_BacksOff(t *testing.T) {
	env := setupRetryTest(t)
	env.dispatcher.setReturnErr(errors.New("broker refused"))
	ctx := context.Background()

	notifs, err := env.store.GetUndispatchedAgentNotifications(ctx, "")
	require.NoError(t, err)
	require.Len(t, notifs, 1)
	id := notifs[0].ID

	for attempt, want := range []time.Duration{5 * time.Minute, 10 * time.Minute} {
		before := time.Now()
		env.nd.RetryDispatch(ctx, getNotification(t, env, id))
		after := time.Now()

		n := getNotification(t, env, id)
		require.False(t, n.Dispatched, "attempt %d: the claim must be released", attempt+1)
		require.Equal(t, attempt+1, n.DispatchAttempts)
		require.NotNil(t, n.NextAttemptAt, "attempt %d: a retry backoff must be set", attempt+1)
		assert.False(t, n.NextAttemptAt.Before(before.Add(want).Add(-time.Second)),
			"attempt %d: backoff %v too short: next attempt at %v", attempt+1, want, n.NextAttemptAt)
		assert.False(t, n.NextAttemptAt.After(after.Add(want).Add(time.Second)),
			"attempt %d: backoff %v too long: next attempt at %v", attempt+1, want, n.NextAttemptAt)
	}
	assert.Len(t, env.dispatcher.getCalls(), 2)
}
