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

// Agent notifications to a held subscriber (ptone/scion#7145): a held
// subscriber, whatever its phase and whether or not it has a runtime broker,
// gets no notification. The notification is dropped at once, recorded as
// failed with reason subscriber_held and no delivery attempt, and it is not
// delivered after the holds are cleared. A hold lookup fault delivers
// nothing and leaves the notification pending, so a later pass checks it
// again.

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

// holdSubscriber places an active hold on env's subscriber.
func holdSubscriber(t *testing.T, env *notificationTestEnv) {
	t.Helper()
	holdAgentFor(t, env.store, env.subscriber.ID, env.project.ID, tid("notif-held-root"))
}

// liftSubscriberHold clears env's subscriber's holds.
func liftSubscriberHold(t *testing.T, env *notificationTestEnv) {
	t.Helper()
	_, err := env.store.ClearAgentHolds(context.Background(), env.subscriber.ID,
		store.ClearActor{Kind: store.ClearActorUser, ID: tid("notif-held-admin")}, "lifted")
	require.NoError(t, err)
}

// assertDroppedHeld checks a notification was dropped for a held subscriber:
// terminal, failed with reason subscriber_held, and never attempted.
func assertDroppedHeld(t *testing.T, env *notificationTestEnv, id string) {
	t.Helper()
	n := getNotification(t, env, id)
	assert.True(t, n.Dispatched, "a dropped notification is terminal")
	assert.Equal(t, store.NotificationOutcomeFailed, n.DispatchOutcome)
	assert.Equal(t, store.NotificationReasonSubscriberHeld, n.DispatchReason)
	assert.Zero(t, n.DispatchAttempts, "no delivery may be attempted")
}

// The primary dispatch to a held, running subscriber delivers nothing and
// drops the notification.
func TestHeldNotice_NotificationDispatchDropped(t *testing.T) {
	env := setupNotificationTest(t)
	holdSubscriber(t, env)
	id := createUndispatchedNotification(t, env, env.sub, 0)

	env.nd.dispatchToAgent(context.Background(), env.sub, getNotification(t, env, id), env.watched.ID, env.watched.Slug)

	assert.Empty(t, env.dispatcher.getCalls(), "a held subscriber must not be sent anything")
	assertDroppedHeld(t, env, id)
}

// A held subscriber in the stopped phase (neither running nor suspended)
// is dropped the same way.
func TestHeldNotice_NotificationStoppedSubscriberDropped(t *testing.T) {
	env := setupNotificationTest(t)
	setAgentPhase(t, env, env.subscriber, state.PhaseStopped)
	holdSubscriber(t, env)
	id := createUndispatchedNotification(t, env, env.sub, 0)

	env.nd.dispatchToAgent(context.Background(), env.sub, getNotification(t, env, id), env.watched.ID, env.watched.Slug)

	assert.Empty(t, env.dispatcher.getCalls())
	assertDroppedHeld(t, env, id)
}

// A held subscriber that is also suspended is dropped, not parked by the
// suspended branch: on the primary dispatch, and by the sweep for a
// notification that was already pending.
func TestHeldNotice_NotificationSuspendedSubscriberDropped(t *testing.T) {
	env := setupNotificationTest(t)
	ctx := context.Background()
	pending := createUndispatchedNotification(t, env, env.sub, 2*time.Minute)
	setAgentPhase(t, env, env.subscriber, state.PhaseSuspended)
	holdSubscriber(t, env)

	primary := createUndispatchedNotification(t, env, env.sub, 0)
	env.nd.dispatchToAgent(ctx, env.sub, getNotification(t, env, primary), env.watched.ID, env.watched.Slug)
	sweepServer(env).notificationDispatchSweepHandler()(ctx)

	assert.Empty(t, env.dispatcher.getCalls(), "a held, suspended subscriber must not be sent anything")
	assertDroppedHeld(t, env, primary)
	assertDroppedHeld(t, env, pending)
}

// The sweep drops a held subscriber's pending notification in one pass, and
// nothing is delivered after the holds are cleared.
func TestHeldNotice_NotificationSweepDropsHeld(t *testing.T) {
	env := setupNotificationTest(t)
	ctx := context.Background()
	holdSubscriber(t, env)
	id := createUndispatchedNotification(t, env, env.sub, 2*time.Minute)
	handler := sweepServer(env).notificationDispatchSweepHandler()

	handler(ctx)
	assert.Empty(t, env.dispatcher.getCalls(), "a held subscriber must not be sent anything")
	assertDroppedHeld(t, env, id)

	liftSubscriberHold(t, env)
	handler(ctx)

	assert.Empty(t, env.dispatcher.getCalls(), "nothing is delivered after the lift")
	assertDroppedHeld(t, env, id)
}

// A held subscriber with no runtime broker is still selected by the sweep and
// dropped; the hold is checked before any use of the broker, so there is no
// broker call.
func TestHeldNotice_NotificationNoBrokerDropped(t *testing.T) {
	env := setupNotificationTest(t)
	ctx := context.Background()
	env.subscriber.RuntimeBrokerID = ""
	require.NoError(t, env.store.UpdateAgent(ctx, env.subscriber))
	holdSubscriber(t, env)
	id := createUndispatchedNotification(t, env, env.sub, 2*time.Minute)

	sweepServer(env).notificationDispatchSweepHandler()(ctx)

	assert.Empty(t, env.dispatcher.getCalls(), "no broker call for a held subscriber")
	assertDroppedHeld(t, env, id)
}

// A subscriber held after the sweep selected its notification is caught
// after the claim: the claim is kept and the notification is dropped.
func TestHeldNotice_RetryDispatchHeldAfterSelection(t *testing.T) {
	env := setupRetryTest(t)
	ctx := context.Background()

	notifs, err := env.store.GetUndispatchedAgentNotifications(ctx, "")
	require.NoError(t, err)
	require.Len(t, notifs, 1)

	holdSubscriber(t, env)
	env.nd.RetryDispatch(ctx, &notifs[0])

	assert.Empty(t, env.dispatcher.getCalls(), "a held subscriber must not be sent anything")
	assertDroppedHeld(t, env, notifs[0].ID)
}

// A hold lookup fault is not a known hold: the primary dispatch and a retry
// deliver nothing and leave the notification pending (not failed). Once the
// fault clears, the next sweep delivers to a subscriber that is not held.
func TestHeldNotice_NotificationLookupFaultWithholds(t *testing.T) {
	env := setupRetryTest(t)
	ctx := context.Background()
	sw := &storeFaultSwitch{}
	faulty := NewNotificationDispatcher(&targetHoldFaultStore{Store: env.store, fault: sw, agentID: env.subscriber.ID},
		env.pub, func() AgentDispatcher { return env.dispatcher }, slog.Default())

	notifs, err := env.store.GetUndispatchedAgentNotifications(ctx, "")
	require.NoError(t, err)
	require.Len(t, notifs, 1)
	primary := createUndispatchedNotification(t, env, env.sub, 2*time.Minute)

	sw.Arm()
	faulty.RetryDispatch(ctx, &notifs[0])
	faulty.dispatchToAgent(ctx, env.sub, getNotification(t, env, primary), env.watched.ID, env.watched.Slug)

	assert.Empty(t, env.dispatcher.getCalls(), "nothing is delivered when the hold cannot be read")
	assertPendingUntouched(t, env, notifs[0].ID)
	assertPendingUntouched(t, env, primary)

	// The fault clears (env.nd reads the store directly): the subscriber is
	// not held, so the next sweep delivers both.
	sweepServer(env).notificationDispatchSweepHandler()(ctx)
	require.Len(t, env.dispatcher.getCalls(), 2, "an unheld subscriber's notifications are delivered after the fault")
	for _, id := range []string{notifs[0].ID, primary} {
		n := getNotification(t, env, id)
		assert.Equal(t, store.NotificationOutcomeDelivered, n.DispatchOutcome)
	}
}

// After a hold lookup fault clears, a subscriber that IS held has its
// pending notification dropped by the next sweep.
func TestHeldNotice_NotificationLookupFaultThenHeldDropped(t *testing.T) {
	env := setupRetryTest(t)
	ctx := context.Background()
	sw := &storeFaultSwitch{}
	faulty := NewNotificationDispatcher(&targetHoldFaultStore{Store: env.store, fault: sw, agentID: env.subscriber.ID},
		env.pub, func() AgentDispatcher { return env.dispatcher }, slog.Default())

	notifs, err := env.store.GetUndispatchedAgentNotifications(ctx, "")
	require.NoError(t, err)
	require.Len(t, notifs, 1)

	sw.Arm()
	faulty.RetryDispatch(ctx, &notifs[0])
	assertPendingUntouched(t, env, notifs[0].ID)

	holdSubscriber(t, env)
	sweepServer(env).notificationDispatchSweepHandler()(ctx)

	assert.Empty(t, env.dispatcher.getCalls())
	assertDroppedHeld(t, env, notifs[0].ID)
}

// claimFaultStore fails every notification claim.
type claimFaultStore struct{ store.Store }

func (claimFaultStore) ClaimNotificationForDispatch(context.Context, string) (bool, error) {
	return false, errors.New("injected claim fault")
}

// dropHeldNotification records an outcome only for a notification it claims:
// a notification another caller already holds is left to that caller (no
// outcome is recorded), and a claim error leaves the notification pending.
func TestHeldNotice_DropHeldNotificationClaim(t *testing.T) {
	env := setupNotificationTest(t)
	ctx := context.Background()

	taken := createUndispatchedNotification(t, env, env.sub, 0)
	claimed, err := env.store.ClaimNotificationForDispatch(ctx, taken)
	require.NoError(t, err)
	require.True(t, claimed)
	env.nd.dropHeldNotification(ctx, taken)
	n := getNotification(t, env, taken)
	assert.True(t, n.Dispatched, "the other caller still holds the claim")
	assert.Empty(t, n.DispatchOutcome, "a lost claim records no outcome")
	assert.Empty(t, n.DispatchReason)

	pending := createUndispatchedNotification(t, env, env.sub, 0)
	nd := NewNotificationDispatcher(claimFaultStore{Store: env.store}, env.pub, func() AgentDispatcher { return env.dispatcher }, slog.Default())
	nd.dropHeldNotification(ctx, pending)
	assertPendingUntouched(t, env, pending)
	assert.Empty(t, env.dispatcher.getCalls())
}

// The broker-connect drain drops a held subscriber's notification, even a
// suspended one: it is selected and recorded as failed / subscriber_held with
// no delivery attempt.
func TestHeldNotice_NotificationDrainDropsHeld(t *testing.T) {
	env := setupNotificationTest(t)
	ctx := context.Background()
	setAgentPhase(t, env, env.subscriber, state.PhaseSuspended)
	holdSubscriber(t, env)
	id := createUndispatchedNotification(t, env, env.sub, 0)

	sweepServer(env).drainUndispatchedNotifications(ctx, env.subscriber.RuntimeBrokerID)

	assert.Empty(t, env.dispatcher.getCalls(), "a held subscriber must not be sent anything")
	assertDroppedHeld(t, env, id)
}
