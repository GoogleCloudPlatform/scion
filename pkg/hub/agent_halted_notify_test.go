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
	"sort"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Halted notification and hook tests (ptone/scion#4853).

// haltNotifyEnv is a notification test env whose watched agent is halted.
type haltNotifyEnv struct {
	*notificationTestEnv
}

func setupHaltNotify(t *testing.T) haltNotifyEnv {
	t.Helper()
	env := setupNotificationTest(t)
	// Start from no subscriptions; each test adds its own.
	require.NoError(t, env.store.DeleteNotificationSubscription(context.Background(), env.sub.ID))
	env.watched.Phase = string(state.PhaseHalted)
	env.watched.ExitReason = string(state.ExitReasonPreempted)
	return haltNotifyEnv{env}
}

// subscribe adds an agent-scoped subscription of the watched agent.
func (e haltNotifyEnv) subscribe(t *testing.T, subscriberType, subscriberID string, triggers ...string) {
	t.Helper()
	require.NoError(t, e.store.CreateNotificationSubscription(context.Background(), &store.NotificationSubscription{
		ID:                api.NewUUID(),
		Scope:             store.SubscriptionScopeAgent,
		AgentID:           e.watched.ID,
		SubscriberType:    subscriberType,
		SubscriberID:      subscriberID,
		ProjectID:         e.project.ID,
		TriggerActivities: triggers,
		CreatedAt:         time.Now().Add(-time.Minute),
		CreatedBy:         "test",
	}))
}

// statuses returns the statuses of the notifications stored for a
// subscriber, sorted.
func (e haltNotifyEnv) statuses(t *testing.T, subscriberType, subscriberID string) []string {
	t.Helper()
	notifs, err := e.store.GetNotifications(context.Background(), subscriberType, subscriberID, false)
	require.NoError(t, err)
	out := make([]string, 0, len(notifs))
	for _, n := range notifs {
		out = append(out, n.Status)
	}
	sort.Strings(out)
	return out
}

func (e haltNotifyEnv) createUser(t *testing.T, id string) {
	t.Helper()
	require.NoError(t, e.store.CreateUser(context.Background(), &store.User{
		ID: id, Email: id + "@test.com", DisplayName: id, Role: "member", Status: store.UserStatusActive,
	}))
}

func TestDispatchHalted_Matching(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		haltedFrom string
		want       map[string][]string
	}{
		// The halt replaced an error write: the ERROR-only subscriber is
		// told as before (status ERROR, the halt's text).
		{"error", map[string][]string{"halted-only": {"HALTED"}, "error-only": {"ERROR"}, "both": {"HALTED"}, "completed-only": {}}},
		// The halt replaced a stopped write: ERROR does not match.
		{"stopped", map[string][]string{"halted-only": {"HALTED"}, "error-only": {}, "both": {"HALTED"}, "completed-only": {}}},
		// A same-phase upgrade: HALTED only, no ERROR alias.
		{"", map[string][]string{"halted-only": {"HALTED"}, "error-only": {}, "both": {"HALTED"}, "completed-only": {}}},
	} {
		t.Run("from="+tc.haltedFrom, func(t *testing.T) {
			e := setupHaltNotify(t)
			e.subscribe(t, store.SubscriberTypeUser, "halted-only", "HALTED")
			e.subscribe(t, store.SubscriberTypeUser, "error-only", "ERROR")
			e.subscribe(t, store.SubscriberTypeUser, "both", "ERROR", "HALTED")
			e.subscribe(t, store.SubscriberTypeUser, "completed-only", "COMPLETED")

			e.nd.dispatchHalted(ctx, e.watched, tc.haltedFrom)

			for sub, want := range tc.want {
				assert.Equal(t, want, e.statuses(t, store.SubscriberTypeUser, sub), "subscriber %s", sub)
			}
		})
	}
}

func TestDispatchHalted_MessageAndDedupAcrossScopes(t *testing.T) {
	ctx := context.Background()
	e := setupHaltNotify(t)
	e.subscribe(t, store.SubscriberTypeUser, "u1", "HALTED")
	require.NoError(t, e.store.CreateNotificationSubscription(ctx, &store.NotificationSubscription{
		ID:                api.NewUUID(),
		Scope:             store.SubscriptionScopeProject,
		SubscriberType:    store.SubscriberTypeUser,
		SubscriberID:      "u1",
		ProjectID:         e.project.ID,
		TriggerActivities: []string{"HALTED"},
		CreatedAt:         time.Now().Add(-time.Minute),
		CreatedBy:         "test",
	}))

	e.nd.dispatchHalted(ctx, e.watched, "stopped")

	notifs, err := e.store.GetNotifications(ctx, store.SubscriberTypeUser, "u1", false)
	require.NoError(t, err)
	require.Len(t, notifs, 1, "one notification per subscriber across scopes")
	assert.Equal(t, "watched-agent was halted by the platform (preempted). Workspace may be re-cloned. Resume: scion start watched-agent.", notifs[0].Message)
}

func TestDispatchHalted_AgentSubscriber(t *testing.T) {
	ctx := context.Background()
	e := setupHaltNotify(t)
	e.subscribe(t, store.SubscriberTypeAgent, e.subscriber.Slug, "HALTED")

	e.nd.dispatchHalted(ctx, e.watched, "stopped")

	calls := e.dispatcher.getCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, e.subscriber.ID, calls[0].Agent.ID)
	assert.Contains(t, calls[0].Message, "was halted by the platform (preempted)")
}

func TestDispatchHalted_CreatorFallback(t *testing.T) {
	ctx := context.Background()

	t.Run("user creator with no subscription", func(t *testing.T) {
		e := setupHaltNotify(t)
		e.createUser(t, "creator-user")
		e.watched.CreatedBy = "creator-user"
		e.nd.dispatchHalted(ctx, e.watched, "stopped")
		notifs, err := e.store.GetNotifications(ctx, store.SubscriberTypeUser, "creator-user", false)
		require.NoError(t, err)
		require.Len(t, notifs, 1)
		assert.Equal(t, "HALTED", notifs[0].Status)
		assert.Equal(t, store.CreatorFallbackSubscriptionID, notifs[0].SubscriptionID)
	})

	t.Run("agent creator", func(t *testing.T) {
		e := setupHaltNotify(t)
		e.watched.CreatedBy = e.subscriber.ID
		e.nd.dispatchHalted(ctx, e.watched, "stopped")
		calls := e.dispatcher.getCalls()
		require.Len(t, calls, 1, "the creating agent gets one message")
		assert.Equal(t, e.subscriber.ID, calls[0].Agent.ID)
		assert.Equal(t, []string{"HALTED"}, e.statuses(t, store.SubscriberTypeAgent, e.subscriber.Slug))
	})

	t.Run("creator with a HALTED subscription gets exactly one", func(t *testing.T) {
		e := setupHaltNotify(t)
		e.createUser(t, "creator-user")
		e.watched.CreatedBy = "creator-user"
		e.subscribe(t, store.SubscriberTypeUser, "creator-user", "HALTED")
		e.nd.dispatchHalted(ctx, e.watched, "stopped")
		assert.Equal(t, []string{"HALTED"}, e.statuses(t, store.SubscriberTypeUser, "creator-user"))
	})

	t.Run("creator with an ERROR subscription on an error halt gets exactly one", func(t *testing.T) {
		e := setupHaltNotify(t)
		e.createUser(t, "creator-user")
		e.watched.CreatedBy = "creator-user"
		e.subscribe(t, store.SubscriberTypeUser, "creator-user", "ERROR")
		e.nd.dispatchHalted(ctx, e.watched, "error")
		assert.Equal(t, []string{"ERROR"}, e.statuses(t, store.SubscriberTypeUser, "creator-user"))
	})

	t.Run("deleted creator agent gets none", func(t *testing.T) {
		e := setupHaltNotify(t)
		e.watched.CreatedBy = e.subscriber.ID
		creator, err := e.store.GetAgent(ctx, e.subscriber.ID)
		require.NoError(t, err)
		creator.DeletedAt = time.Now()
		require.NoError(t, e.store.UpdateAgent(ctx, creator))
		e.nd.dispatchHalted(ctx, e.watched, "stopped")
		assert.Empty(t, e.dispatcher.getCalls())
		assert.Empty(t, e.statuses(t, store.SubscriberTypeAgent, e.subscriber.Slug))
	})

	t.Run("unknown creator gets none", func(t *testing.T) {
		e := setupHaltNotify(t)
		e.watched.CreatedBy = api.NewUUID()
		e.nd.dispatchHalted(ctx, e.watched, "stopped")
		assert.Empty(t, e.dispatcher.getCalls())
	})

	t.Run("same-phase halt notifies the creator, no ERROR alias", func(t *testing.T) {
		e := setupHaltNotify(t)
		e.createUser(t, "creator-user")
		e.watched.CreatedBy = "creator-user"
		e.subscribe(t, store.SubscriberTypeUser, "error-watcher", "ERROR")
		e.nd.dispatchHalted(ctx, e.watched, "")
		assert.Equal(t, []string{"HALTED"}, e.statuses(t, store.SubscriberTypeUser, "creator-user"))
		assert.Empty(t, e.statuses(t, store.SubscriberTypeUser, "error-watcher"))
	})
}

func TestDispatchHalted_BusEventsNotifyNothing(t *testing.T) {
	e := setupHaltNotify(t)
	e.subscribe(t, store.SubscriberTypeAgent, e.subscriber.Slug, "HALTED", "ERROR", "COMPLETED")
	e.nd.Start()
	defer e.nd.Stop()

	// A heartbeat republish of a halted agent, with and without activity.
	e.publishStatusWithPhase(string(state.PhaseHalted), "")
	e.publishStatusWithPhase(string(state.PhaseHalted), "completed")
	time.Sleep(200 * time.Millisecond)
	assert.Empty(t, e.dispatcher.getCalls())
}

func TestRetryDispatch_CreatorFallbackDeliveredOnRetry(t *testing.T) {
	ctx := context.Background()
	e := setupHaltNotify(t)
	e.watched.CreatedBy = e.subscriber.ID
	e.dispatcher.setReturnErr(errors.New("broker unavailable"))

	e.nd.dispatchHalted(ctx, e.watched, "stopped")

	require.Len(t, e.dispatcher.getCalls(), 1, "the first delivery was attempted")
	notifs, err := e.store.GetNotifications(ctx, store.SubscriberTypeAgent, e.subscriber.Slug, false)
	require.NoError(t, err)
	require.Len(t, notifs, 1)
	require.Equal(t, store.CreatorFallbackSubscriptionID, notifs[0].SubscriptionID)
	require.False(t, notifs[0].Dispatched, "a failed delivery is left for the sweep")

	e.dispatcher.setReturnErr(nil)
	e.nd.RetryDispatch(ctx, &notifs[0])

	calls := e.dispatcher.getCalls()
	require.Len(t, calls, 2, "the sweep's retry delivers it")
	assert.Equal(t, e.subscriber.ID, calls[1].Agent.ID, "to the creator only")
	assert.True(t, notificationDispatched(t, e.notificationTestEnv, notifs[0].ID))
}

func TestRetryDispatch_OrdinaryRowStillLooksUpItsSubscription(t *testing.T) {
	// A row with a real (here: deleted) subscription is not rebuilt from
	// its own fields: the existing lookup decides, and a deleted
	// subscription is left claimed without a delivery.
	ctx := context.Background()
	e := setupHaltNotify(t)
	notif := &store.Notification{
		ID:             api.NewUUID(),
		SubscriptionID: api.NewUUID(),
		AgentID:        e.watched.ID,
		ProjectID:      e.project.ID,
		SubscriberType: store.SubscriberTypeAgent,
		SubscriberID:   e.subscriber.Slug,
		Status:         NotificationStatusHalted,
		Message:        "m",
		CreatedAt:      time.Now(),
	}
	require.NoError(t, e.store.CreateNotification(ctx, notif))

	e.nd.RetryDispatch(ctx, notif)

	assert.Empty(t, e.dispatcher.getCalls())
	assert.True(t, notificationDispatched(t, e.notificationTestEnv, notif.ID), "left claimed, as before")
}

func TestRetrySubscription_CreatorFallback(t *testing.T) {
	sub, err := retrySubscription(context.Background(), nil, &store.Notification{
		SubscriptionID: store.CreatorFallbackSubscriptionID,
		SubscriberType: store.SubscriberTypeAgent,
		SubscriberID:   "parent",
		ProjectID:      "p1",
	})
	require.NoError(t, err)
	assert.Equal(t, "parent", sub.SubscriberID)
	assert.Equal(t, store.SubscriberTypeAgent, sub.SubscriberType)
	assert.Equal(t, "p1", sub.ProjectID)
}

func TestHaltedWorkspaceWording(t *testing.T) {
	assert.Equal(t, "Workspace re-cloned", haltedWorkspaceWording(&store.Agent{Runtime: "kubernetes", WorkspacePlacement: api.WorkspacePlacementLocal}))
	assert.Equal(t, "Workspace kept", haltedWorkspaceWording(&store.Agent{Runtime: "kubernetes", WorkspacePlacement: api.WorkspacePlacementExport}))
	assert.Equal(t, "Workspace may be re-cloned", haltedWorkspaceWording(&store.Agent{Runtime: "kubernetes"}))
	assert.Equal(t, "Workspace may be re-cloned", haltedWorkspaceWording(&store.Agent{}))
	assert.Equal(t, "Workspace kept", haltedWorkspaceWording(&store.Agent{Runtime: "docker"}))
	assert.Equal(t, "a was halted by the platform (signal). Workspace kept. Resume: scion start a.",
		haltedNotificationMessage(&store.Agent{Slug: "a", Runtime: "docker"}))
}

func TestFireHalted(t *testing.T) {
	ctx := context.Background()
	newEnv := func(t *testing.T, opts ...EvaluatorOption) (store.Store, *store.Agent, *recordingExecutor, *LifecycleHookEvaluator, map[string]string) {
		s := testEvaluatorStore(t)
		projectID := seedHookProject(t, s, "halt-hooks")
		a := seedHookAgent(t, s, projectID, "claude", "running")
		hooks := map[string]string{}
		for _, trig := range []string{"halted", "stopped", "error", "running"} {
			hooks[seedLifecycleHook(t, s, "h-"+trig, trig, true, nil).ID] = trig
		}
		exec := &recordingExecutor{}
		ev := NewLifecycleHookEvaluator(s, nil, exec, slog.Default(), opts...)
		return s, a, exec, ev, hooks
	}
	firedTriggers := func(exec *recordingExecutor) []string {
		var out []string
		for _, c := range exec.getCalls() {
			out = append(out, c.Trigger)
		}
		sort.Strings(out)
		return out
	}

	t.Run("halted plus the replaced phase", func(t *testing.T) {
		_, a, exec, ev, _ := newEnv(t)
		ev.fireHalted(ctx, a, "stopped")
		assert.Equal(t, []string{"halted", "stopped"}, firedTriggers(exec))
	})
	t.Run("emptyDir upgrade fires error, not stopped again", func(t *testing.T) {
		_, a, exec, ev, _ := newEnv(t)
		ev.fireHalted(ctx, a, "error")
		assert.Equal(t, []string{"error", "halted"}, firedTriggers(exec))
	})
	t.Run("same-phase halt fires halted only", func(t *testing.T) {
		_, a, exec, ev, _ := newEnv(t)
		ev.fireHalted(ctx, a, "")
		assert.Equal(t, []string{"halted"}, firedTriggers(exec))
	})
	t.Run("a halt fires once", func(t *testing.T) {
		_, a, exec, ev, _ := newEnv(t)
		ev.fireHalted(ctx, a, "stopped")
		ev.fireHalted(ctx, a, "stopped")
		assert.Len(t, exec.getCalls(), 2, "halted + stopped, once")
	})
	t.Run("two evaluators on a shared durable deduper fire once", func(t *testing.T) {
		s, a, exec, ev, _ := newEnv(t, WithDBDriver(DBDriverPostgres))
		exec2 := &recordingExecutor{}
		ev2 := NewLifecycleHookEvaluator(s, nil, exec2, slog.Default(), WithDBDriver(DBDriverPostgres))
		ev.fireHalted(ctx, a, "")
		ev2.fireHalted(ctx, a, "")
		assert.Len(t, append(exec.getCalls(), exec2.getCalls()...), 1)
	})
	t.Run("bus events of a halted agent fire nothing", func(t *testing.T) {
		s, a, _, _, _ := newEnv(t)
		exec := &recordingExecutor{}
		events := NewChannelEventPublisher()
		defer events.Close()
		ev := NewLifecycleHookEvaluator(s, events, exec, slog.Default())
		ev.Start()
		defer ev.Stop()
		a.Phase = string(state.PhaseHalted)
		events.PublishAgentStatus(ctx, a)
		time.Sleep(200 * time.Millisecond)
		assert.Empty(t, exec.getCalls())
	})
}

func TestLifecycleHook_CreateHaltedTriggerAndFire(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	req := validCreateRequest()
	req.Trigger = store.LifecycleHookTriggerHalted
	hook := createHookViaAPI(t, srv, req)
	assert.Equal(t, store.LifecycleHookTriggerHalted, hook.Trigger)

	projectID := seedHookProject(t, s, "halt-hook-api")
	a := seedHookAgent(t, s, projectID, "claude", "running")
	exec := &recordingExecutor{}
	ev := NewLifecycleHookEvaluator(s, nil, exec, slog.Default())
	ev.fireHalted(ctx, a, "")
	ev.fireHalted(ctx, a, "")
	calls := exec.getCalls()
	require.Len(t, calls, 1, "the halted hook fires once")
	assert.Equal(t, hook.ID, calls[0].HookID)
}
