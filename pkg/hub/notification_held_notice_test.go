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
// subscriber, whatever its phase, gets no notification; the notification
// stays pending (parked) with no attempt counted, as for a suspended
// subscriber, and is delivered once the holds are cleared. A hold lookup
// fault parks it too.

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// notifHoldFaultStore fails the hold lookup for one agent while its fault
// switch is armed.
type notifHoldFaultStore struct {
	store.Store
	fault   *storeFaultSwitch
	agentID string
}

func (h *notifHoldFaultStore) HasActiveAgentHold(ctx context.Context, agentID string) (bool, error) {
	if h.fault.Active() && agentID == h.agentID {
		return false, errors.New("injected hold lookup fault")
	}
	return h.Store.HasActiveAgentHold(ctx, agentID)
}

// holdSubscriber places an active hold on env's (running) subscriber.
func holdSubscriber(t *testing.T, env *notificationTestEnv) {
	t.Helper()
	holdAgentFor(t, env.store, env.subscriber.ID, env.project.ID, tid("notif-held-root"))
}

// The primary dispatch makes no delivery attempt to a held, running
// subscriber and leaves the notification pending.
func TestHeldNotice_NotificationDispatchParked(t *testing.T) {
	env := setupNotificationTest(t)
	holdSubscriber(t, env)
	id := createUndispatchedNotification(t, env, env.sub, 0)

	env.nd.dispatchToAgent(context.Background(), env.sub, getNotification(t, env, id), env.watched.ID, env.watched.Slug)

	assert.Empty(t, env.dispatcher.getCalls(), "a held subscriber must not be sent anything")
	assertPendingUntouched(t, env, id)
}

// The sweep neither claims nor attempts a held subscriber's notification;
// after its holds are cleared, the next sweep delivers it.
func TestHeldNotice_NotificationSweepParkedUntilLift(t *testing.T) {
	env := setupNotificationTest(t)
	ctx := context.Background()
	holdSubscriber(t, env)
	id := createUndispatchedNotification(t, env, env.sub, 2*time.Minute)
	handler := sweepServer(env).notificationDispatchSweepHandler()

	handler(ctx)
	handler(ctx)
	assert.Empty(t, env.dispatcher.getCalls(), "a held subscriber must not be sent anything")
	assertPendingUntouched(t, env, id)

	_, err := env.store.ClearAgentHolds(ctx, env.subscriber.ID,
		store.ClearActor{Kind: store.ClearActorUser, ID: tid("notif-held-admin")}, "lifted")
	require.NoError(t, err)
	handler(ctx)

	require.Len(t, env.dispatcher.getCalls(), 1, "the first sweep after the lift must deliver")
	n := getNotification(t, env, id)
	assert.True(t, n.Dispatched)
	assert.Equal(t, store.NotificationOutcomeDelivered, n.DispatchOutcome)
	assert.Equal(t, 1, n.DispatchAttempts)
}

// A subscriber held after the sweep selected its notification is caught
// after the claim: the claim is released with no attempt counted and no
// backoff.
func TestHeldNotice_RetryDispatchHeldAfterSelection(t *testing.T) {
	env := setupRetryTest(t)
	ctx := context.Background()

	notifs, err := env.store.GetUndispatchedAgentNotifications(ctx, "")
	require.NoError(t, err)
	require.Len(t, notifs, 1)

	holdSubscriber(t, env)
	env.nd.RetryDispatch(ctx, &notifs[0])

	assert.Empty(t, env.dispatcher.getCalls(), "a held subscriber must not be sent anything")
	assertPendingUntouched(t, env, notifs[0].ID)
}

// A subscriber whose hold cannot be read is not sent anything: the primary
// dispatch and a retry both leave the notification pending with no attempt
// counted.
func TestHeldNotice_NotificationLookupFaultParks(t *testing.T) {
	env := setupRetryTest(t)
	ctx := context.Background()
	sw := &storeFaultSwitch{}
	nd := NewNotificationDispatcher(&notifHoldFaultStore{Store: env.store, fault: sw, agentID: env.subscriber.ID},
		env.pub, func() AgentDispatcher { return env.dispatcher }, slog.Default())

	notifs, err := env.store.GetUndispatchedAgentNotifications(ctx, "")
	require.NoError(t, err)
	require.Len(t, notifs, 1)
	primary := createUndispatchedNotification(t, env, env.sub, 0)

	sw.Arm()
	nd.RetryDispatch(ctx, &notifs[0])
	nd.dispatchToAgent(ctx, env.sub, getNotification(t, env, primary), env.watched.ID, env.watched.Slug)

	assert.Empty(t, env.dispatcher.getCalls(), "nothing is sent when the hold cannot be read")
	assertPendingUntouched(t, env, notifs[0].ID)
	assertPendingUntouched(t, env, primary)
}
