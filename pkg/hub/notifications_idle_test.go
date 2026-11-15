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
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// The subscription a create with notify=true records includes IDLE, so a
// parent hears about a child that goes idle without task_completed.
func TestNewNotifySubscription_DefaultTriggersIncludeIdle(t *testing.T) {
	sub := newNotifySubscription("project-1", store.SubscriberTypeAgent, "parent", "parent")
	require.NotNil(t, sub)
	assert.True(t, sub.MatchesActivity("idle"), "default triggers %v must match idle", sub.TriggerActivities)
}

// A subscribed parent with the create-default triggers receives an IDLE
// notification, with the child's message, when the child goes idle.
func TestNotificationDispatcher_IdleNotifiesSubscribedParent(t *testing.T) {
	env := setupNotificationTest(t)
	ctx := context.Background()
	require.NoError(t, env.store.UpdateNotificationSubscriptionTriggers(ctx, env.sub.ID,
		newNotifySubscription(env.project.ID, store.SubscriberTypeAgent, env.subscriber.Slug, "test").TriggerActivities))
	require.NoError(t, env.store.UpdateAgentStatus(ctx, env.watched.ID, store.AgentStatusUpdate{
		Activity: "idle", Message: "Between assignments",
	}))
	env.nd.Start()
	defer env.nd.Stop()

	env.publishStatus("idle")

	require.Eventually(t, func() bool {
		return len(env.dispatcher.getCalls()) == 1
	}, 2*time.Second, 50*time.Millisecond)
	assert.Equal(t, env.subscriber.ID, env.dispatcher.getCalls()[0].Agent.ID)
	assert.Contains(t, env.dispatcher.getCalls()[0].Message, "watched-agent is IDLE: Between assignments")

	notifs, err := env.store.GetNotifications(ctx, store.SubscriberTypeAgent, env.subscriber.Slug, false)
	require.NoError(t, err)
	require.Len(t, notifs, 1)
	assert.Equal(t, "IDLE", notifs[0].Status)
}

func reportAndPublishActivity(t *testing.T, env *notificationTestEnv, phase, activity string) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, env.store.UpdateAgentStatus(ctx, env.watched.ID, store.AgentStatusUpdate{
		Phase:    phase,
		Activity: activity,
	}))
	fresh, err := env.store.GetAgent(ctx, env.watched.ID)
	require.NoError(t, err)
	env.pub.PublishAgentStatus(ctx, fresh)
}

func TestNotificationDispatcher_RepeatedIdlePeriodsNotifyEachTime(t *testing.T) {
	env := setupNotificationTest(t)
	require.NoError(t, env.store.UpdateNotificationSubscriptionTriggers(
		context.Background(), env.sub.ID, []string{"COMPLETED", "IDLE", "WAITING_FOR_INPUT"},
	))
	env.nd.Start()
	defer env.nd.Stop()

	// Period 1: agent enters idle -> notifies once.
	reportAndPublishActivity(t, env, string(state.PhaseRunning), string(state.ActivityIdle))
	require.Eventually(t, func() bool {
		return len(env.dispatcher.getCalls()) == 1
	}, 2*time.Second, 20*time.Millisecond)
	assert.Contains(t, env.dispatcher.getCalls()[0].Message, "IDLE")

	// Re-report / heartbeat within the SAME idle period -> must NOT re-notify.
	env.pub.PublishAgentStatus(context.Background(), mustGetAgent(t, env.store, env.watched.ID))
	time.Sleep(150 * time.Millisecond)
	assert.Len(t, env.dispatcher.getCalls(), 1, "re-reporting idle in the same period must be deduped")

	// Agent leaves idle to working (non-notifying), then re-enters idle -> notifies a second time.
	reportAndPublishActivity(t, env, string(state.PhaseRunning), string(state.ActivityWorking))
	reportAndPublishActivity(t, env, string(state.PhaseRunning), string(state.ActivityIdle))

	require.Eventually(t, func() bool {
		return len(env.dispatcher.getCalls()) == 2
	}, 2*time.Second, 20*time.Millisecond)
	assert.Contains(t, env.dispatcher.getCalls()[1].Message, "IDLE")

	notifs, err := env.store.GetNotifications(context.Background(), store.SubscriberTypeAgent, env.subscriber.Slug, false)
	require.NoError(t, err)
	assert.Len(t, notifs, 2)
}

func TestNotificationDispatcher_RepeatedCompletedPeriodsNotifyEachTime(t *testing.T) {
	env := setupNotificationTest(t)
	env.nd.Start()
	defer env.nd.Stop()

	// Period 1: completed -> notifies once.
	reportAndPublishActivity(t, env, string(state.PhaseRunning), string(state.ActivityCompleted))
	require.Eventually(t, func() bool {
		return len(env.dispatcher.getCalls()) == 1
	}, 2*time.Second, 20*time.Millisecond)

	// Re-report within the same completed period -> deduped.
	env.pub.PublishAgentStatus(context.Background(), mustGetAgent(t, env.store, env.watched.ID))
	time.Sleep(150 * time.Millisecond)
	assert.Len(t, env.dispatcher.getCalls(), 1)

	// Transition completed -> working -> completed -> notifies a second time.
	reportAndPublishActivity(t, env, string(state.PhaseRunning), string(state.ActivityWorking))
	reportAndPublishActivity(t, env, string(state.PhaseRunning), string(state.ActivityCompleted))

	require.Eventually(t, func() bool {
		return len(env.dispatcher.getCalls()) == 2
	}, 2*time.Second, 20*time.Millisecond)
	assert.Contains(t, env.dispatcher.getCalls()[1].Message, "COMPLETED")
}

func TestNotificationDispatcher_CrossRestartActivitySinceDedupeAndReNotify(t *testing.T) {
	env := setupNotificationTest(t)
	require.NoError(t, env.store.UpdateNotificationSubscriptionTriggers(
		context.Background(), env.sub.ID, []string{"COMPLETED", "IDLE"},
	))
	env.nd.Start()

	// Period 1: idle -> notifies once.
	reportAndPublishActivity(t, env, string(state.PhaseRunning), string(state.ActivityIdle))
	require.Eventually(t, func() bool {
		return len(env.dispatcher.getCalls()) == 1
	}, 2*time.Second, 20*time.Millisecond)
	env.nd.Stop()

	// Simulate Hub restart: fresh NotificationDispatcher with empty in-memory state.
	nd2 := NewNotificationDispatcher(env.store, env.pub, func() AgentDispatcher { return env.dispatcher }, slog.Default())
	nd2.Start()
	defer nd2.Stop()

	// Case 1: Re-publish without leaving idle (ActivitySince predates the stored notification) -> deduped.
	env.pub.PublishAgentStatus(context.Background(), mustGetAgent(t, env.store, env.watched.ID))
	time.Sleep(150 * time.Millisecond)
	assert.Len(t, env.dispatcher.getCalls(), 1, "cross-restart re-report of unchanged idle period must still dedupe")

	// Case 2: Agent left idle while Hub was restarting (working -> idle advances ActivitySince past notif.CreatedAt) -> notifies.
	time.Sleep(15 * time.Millisecond)
	require.NoError(t, env.store.UpdateAgentStatus(context.Background(), env.watched.ID, store.AgentStatusUpdate{
		Phase:    string(state.PhaseRunning),
		Activity: string(state.ActivityWorking),
	}))
	reportAndPublishActivity(t, env, string(state.PhaseRunning), string(state.ActivityIdle))

	require.Eventually(t, func() bool {
		return len(env.dispatcher.getCalls()) == 2
	}, 2*time.Second, 20*time.Millisecond)
}

func TestNotificationDispatcher_SuspendedWithRetainedActivityFiresSuspendedOnly(t *testing.T) {
	t.Run("default subscription without SUSPENDED does not re-fire COMPLETED or IDLE on suspend", func(t *testing.T) {
		env := setupNotificationTest(t)
		require.NoError(t, env.store.UpdateNotificationSubscriptionTriggers(
			context.Background(), env.sub.ID, []string{"COMPLETED", "IDLE"},
		))
		env.nd.Start()
		defer env.nd.Stop()

		// Running agent completes -> 1st COMPLETED notification.
		reportAndPublishActivity(t, env, string(state.PhaseRunning), string(state.ActivityCompleted))
		require.Eventually(t, func() bool {
			return len(env.dispatcher.getCalls()) == 1
		}, 2*time.Second, 20*time.Millisecond)

		// Suspend retains activity="completed": must NOT fire a second COMPLETED notification.
		require.NoError(t, env.store.UpdateAgentStatus(context.Background(), env.watched.ID, store.AgentStatusUpdate{
			Phase: string(state.PhaseSuspended),
		}))
		suspended := mustGetAgent(t, env.store, env.watched.ID)
		require.Equal(t, string(state.ActivityCompleted), suspended.Activity)
		env.pub.PublishAgentStatus(context.Background(), suspended)

		time.Sleep(150 * time.Millisecond)
		assert.Len(t, env.dispatcher.getCalls(), 1, "suspended+completed must not match COMPLETED")

		// Resume and complete again -> notifies COMPLETED for the new run.
		require.NoError(t, env.store.UpdateAgentStatus(context.Background(), env.watched.ID, store.AgentStatusUpdate{
			Phase:                 string(state.PhaseRunning),
			ClearTerminalRemnants: true,
		}))
		env.pub.PublishAgentStatus(context.Background(), mustGetAgent(t, env.store, env.watched.ID))
		reportAndPublishActivity(t, env, string(state.PhaseRunning), string(state.ActivityCompleted))

		require.Eventually(t, func() bool {
			return len(env.dispatcher.getCalls()) == 2
		}, 2*time.Second, 20*time.Millisecond)
	})

	t.Run("subscription watching SUSPENDED and COMPLETED fires SUSPENDED only", func(t *testing.T) {
		env := setupNotificationTest(t)
		require.NoError(t, env.store.UpdateNotificationSubscriptionTriggers(
			context.Background(), env.sub.ID, []string{"COMPLETED", "IDLE", "SUSPENDED"},
		))
		env.nd.Start()
		defer env.nd.Stop()

		// Running agent becomes idle -> IDLE notification (#1).
		reportAndPublishActivity(t, env, string(state.PhaseRunning), string(state.ActivityIdle))
		require.Eventually(t, func() bool {
			return len(env.dispatcher.getCalls()) == 1
		}, 2*time.Second, 20*time.Millisecond)
		assert.Contains(t, env.dispatcher.getCalls()[0].Message, "IDLE")

		// Agent is suspended with retained activity="idle" -> fires SUSPENDED (#2), never IDLE.
		require.NoError(t, env.store.UpdateAgentStatus(context.Background(), env.watched.ID, store.AgentStatusUpdate{
			Phase: string(state.PhaseSuspended),
		}))
		suspended := mustGetAgent(t, env.store, env.watched.ID)
		require.Equal(t, string(state.ActivityIdle), suspended.Activity)
		env.pub.PublishAgentStatus(context.Background(), suspended)

		require.Eventually(t, func() bool {
			return len(env.dispatcher.getCalls()) == 2
		}, 2*time.Second, 20*time.Millisecond)
		assert.Contains(t, env.dispatcher.getCalls()[1].Message, "SUSPENDED")
		assert.NotContains(t, env.dispatcher.getCalls()[1].Message, "IDLE")

		notifs, err := env.store.GetNotifications(context.Background(), store.SubscriberTypeAgent, env.subscriber.Slug, false)
		require.NoError(t, err)
		require.Len(t, notifs, 2)
		assert.Equal(t, "SUSPENDED", notifs[0].Status)
	})
}

func TestNotificationDispatcher_StaleReReportAfterRowMovedOnDoesNotReNotify(t *testing.T) {
	env := setupNotificationTest(t)
	ctx := context.Background()
	require.NoError(t, env.store.UpdateNotificationSubscriptionTriggers(ctx, env.sub.ID, []string{"IDLE"}))
	env.nd.Start()

	reportAndPublishActivity(t, env, string(state.PhaseRunning), string(state.ActivityIdle))
	require.Eventually(t, func() bool {
		return len(env.dispatcher.getCalls()) == 1
	}, 2*time.Second, 20*time.Millisecond)

	// Capture a same-period idle snapshot before the row moves to working.
	staleIdle := mustGetAgent(t, env.store, env.watched.ID)
	time.Sleep(15 * time.Millisecond)
	require.NoError(t, env.store.UpdateAgentStatus(ctx, env.watched.ID, store.AgentStatusUpdate{
		Phase:    string(state.PhaseRunning),
		Activity: string(state.ActivityWorking),
	}))

	// 1. In-process dispatcher handles the stale same-period idle re-report after the row moved on.
	env.pub.PublishAgentStatus(ctx, staleIdle)
	time.Sleep(200 * time.Millisecond)
	assert.Len(t, env.dispatcher.getCalls(), 1, "a same-period idle re-report handled after the row moved on must not re-notify")
	env.nd.Stop()

	// 2. Fresh dispatcher after a Hub restart (`hadObserved == false`) must also reject the stale re-report.
	nd2 := NewNotificationDispatcher(env.store, env.pub, func() AgentDispatcher { return env.dispatcher }, slog.Default())
	nd2.Start()
	defer nd2.Stop()
	env.pub.PublishAgentStatus(ctx, staleIdle)
	time.Sleep(200 * time.Millisecond)
	assert.Len(t, env.dispatcher.getCalls(), 1, "post-restart stale idle re-report after the row moved on must not re-notify")
}

func TestNotificationDispatcher_NonPerPeriodTriggersKeepStatusDeduplication(t *testing.T) {
	env := setupNotificationTest(t)
	ctx := context.Background()
	require.NoError(t, env.store.UpdateNotificationSubscriptionTriggers(ctx, env.sub.ID, []string{"STALLED", "WAITING_FOR_INPUT"}))
	env.nd.Start()
	defer env.nd.Stop()

	// stalled -> working -> stalled must still deduplicate STALLED (not per-period).
	reportAndPublishActivity(t, env, string(state.PhaseRunning), string(state.ActivityStalled))
	require.Eventually(t, func() bool {
		return len(env.dispatcher.getCalls()) == 1
	}, 2*time.Second, 20*time.Millisecond)
	assert.Contains(t, env.dispatcher.getCalls()[0].Message, "STALLED")

	reportAndPublishActivity(t, env, string(state.PhaseRunning), string(state.ActivityWorking))
	reportAndPublishActivity(t, env, string(state.PhaseRunning), string(state.ActivityStalled))
	time.Sleep(200 * time.Millisecond)
	assert.Len(t, env.dispatcher.getCalls(), 1, "STALLED must keep status-based deduplication across stalled -> working -> stalled")

	// Switching to WAITING_FOR_INPUT notifies once, and waiting_for_input -> offline -> waiting_for_input stays deduplicated.
	reportAndPublishActivity(t, env, string(state.PhaseRunning), string(state.ActivityWaitingForInput))
	require.Eventually(t, func() bool {
		return len(env.dispatcher.getCalls()) == 2
	}, 2*time.Second, 20*time.Millisecond)
	reportAndPublishActivity(t, env, string(state.PhaseRunning), string(state.ActivityOffline))
	reportAndPublishActivity(t, env, string(state.PhaseRunning), string(state.ActivityWaitingForInput))
	time.Sleep(200 * time.Millisecond)
	assert.Len(t, env.dispatcher.getCalls(), 2, "WAITING_FOR_INPUT must keep status-based deduplication across offline blips")
}

func (nd *NotificationDispatcher) hasObservedStatus(agentID string) bool {
	nd.observedMu.Lock()
	defer nd.observedMu.Unlock()
	_, ok := nd.lastObservedStatus[agentID]
	return ok
}

func TestNotificationDispatcher_DeletePrunesObservedStatusWithoutDeletedSubscription(t *testing.T) {
	env := setupNotificationTest(t)
	ctx := context.Background()
	require.NoError(t, env.store.UpdateNotificationSubscriptionTriggers(ctx, env.sub.ID, []string{"IDLE"}))
	env.nd.Start()
	defer env.nd.Stop()

	reportAndPublishActivity(t, env, string(state.PhaseRunning), string(state.ActivityIdle))
	require.Eventually(t, func() bool {
		return env.nd.hasObservedStatus(env.watched.ID) && len(env.dispatcher.getCalls()) == 1
	}, 2*time.Second, 20*time.Millisecond)

	// Non-status project events (e.g. exposed-port updates) must not clobber
	// lastObservedStatus or cause a subsequent same-period idle re-report to re-notify.
	env.pub.PublishAgentPorts(ctx, mustGetAgent(t, env.store, env.watched.ID))
	reportAndPublishActivity(t, env, string(state.PhaseRunning), string(state.ActivityIdle))
	time.Sleep(150 * time.Millisecond)
	assert.Len(t, env.dispatcher.getCalls(), 1)
	assert.True(t, env.nd.hasObservedStatus(env.watched.ID))

	// Deleting the agent when no DELETED subscription matched still prunes lastObservedStatus.
	env.pub.PublishAgentDeleted(ctx, env.watched.ID, env.project.ID)
	require.Eventually(t, func() bool {
		return !env.nd.hasObservedStatus(env.watched.ID)
	}, 2*time.Second, 20*time.Millisecond)
	assert.Len(t, env.dispatcher.getCalls(), 1)
}
