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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
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

	t.Run("suspended with retained harness_error fires SUSPENDED only", func(t *testing.T) {
		env := setupNotificationTest(t)
		ctx := context.Background()
		require.NoError(t, env.store.UpdateNotificationSubscriptionTriggers(
			ctx, env.sub.ID, []string{NotificationStatusHarnessError, "SUSPENDED"},
		))
		env.nd.Start()
		defer env.nd.Stop()

		reportAndPublishActivity(t, env, string(state.PhaseRunning), string(state.ActivityHarnessError))
		waitWatchedCalls(t, env, 1)
		assert.Contains(t, watchedAgentCalls(env)[0].Message, NotificationStatusHarnessError)

		// Move through working -> harness_error so last notification is HARNESS_ERROR,
		// then suspend (retaining harness_error): must match SUSPENDED only.
		require.NoError(t, env.store.UpdateAgentStatus(ctx, env.watched.ID, store.AgentStatusUpdate{
			Phase: string(state.PhaseSuspended),
		}))
		suspended := mustGetAgent(t, env.store, env.watched.ID)
		require.Equal(t, string(state.ActivityHarnessError), suspended.Activity)
		env.pub.PublishAgentStatus(ctx, suspended)
		waitWatchedCalls(t, env, 2)
		flushNotificationDispatcher(t, env)

		calls := watchedAgentCalls(env)
		require.Len(t, calls, 2)
		assert.Contains(t, calls[1].Message, "SUSPENDED")
		assert.NotContains(t, calls[1].Message, NotificationStatusHarnessError)
	})
}

func watchedAgentCalls(env *notificationTestEnv) []dispatchCall {
	var out []dispatchCall
	for _, c := range env.dispatcher.getCalls() {
		if strings.Contains(c.Message, env.watched.Slug) {
			out = append(out, c)
		}
	}
	return out
}

// flushNotificationDispatcher enqueues a status event for a fresh barrier
// agent on the same channel and waits until the dispatcher delivers it,
// guaranteeing that every earlier status event on the FIFO channel has finished
// handleEvent without relying on sleep.
func flushNotificationDispatcher(t *testing.T, env *notificationTestEnv) {
	t.Helper()
	ctx := context.Background()
	barrierID := api.NewUUID()
	barrierSlug := "barrier-" + barrierID[:8]
	barrier := &store.Agent{
		ID:              barrierID,
		Slug:            barrierSlug,
		Name:            barrierSlug,
		Template:        "claude",
		ProjectID:       env.project.ID,
		Phase:           string(state.PhaseError),
		RuntimeBrokerID: env.watched.RuntimeBrokerID,
	}
	require.NoError(t, env.store.CreateAgent(ctx, barrier))
	require.NoError(t, env.store.CreateNotificationSubscription(ctx, &store.NotificationSubscription{
		ID:                api.NewUUID(),
		Scope:             store.SubscriptionScopeAgent,
		AgentID:           barrier.ID,
		SubscriberType:    store.SubscriberTypeAgent,
		SubscriberID:      env.subscriber.Slug,
		ProjectID:         env.project.ID,
		TriggerActivities: []string{"ERROR"},
		CreatedAt:         time.Now().Add(-time.Minute),
		CreatedBy:         "test",
	}))
	env.pub.PublishAgentStatus(ctx, barrier)
	require.Eventually(t, func() bool {
		for _, c := range env.dispatcher.getCalls() {
			if strings.Contains(c.Message, barrierSlug) {
				return true
			}
		}
		return false
	}, 2*time.Second, 5*time.Millisecond)
}

func waitWatchedCalls(t *testing.T, env *notificationTestEnv, want int) {
	t.Helper()
	require.Eventually(t, func() bool {
		return len(watchedAgentCalls(env)) == want
	}, 2*time.Second, 5*time.Millisecond)
}

func restartNotificationDispatcher(env *notificationTestEnv, active **NotificationDispatcher) {
	(*active).Stop()
	next := NewNotificationDispatcher(env.store, env.pub, func() AgentDispatcher { return env.dispatcher }, slog.Default())
	next.Start()
	*active = next
}

func TestNotificationDispatcher_StaleReReportAfterRowMovedOnDoesNotReNotify(t *testing.T) {
	env := setupNotificationTest(t)
	ctx := context.Background()
	require.NoError(t, env.store.UpdateNotificationSubscriptionTriggers(ctx, env.sub.ID, []string{"IDLE"}))
	active := env.nd
	active.Start()
	defer func() { active.Stop() }()

	reportAndPublishActivity(t, env, string(state.PhaseRunning), string(state.ActivityIdle))
	waitWatchedCalls(t, env, 1)

	// Capture a same-period idle snapshot, then transition and publish working
	// so the dispatcher observes IDLE -> WORKING before the late stale idle arrives.
	staleIdle := mustGetAgent(t, env.store, env.watched.ID)
	reportAndPublishActivity(t, env, string(state.PhaseRunning), string(state.ActivityWorking))
	env.pub.PublishAgentStatus(ctx, staleIdle)
	flushNotificationDispatcher(t, env)
	assert.Equal(t, string(state.ActivityWorking), mustGetAgent(t, env.store, env.watched.ID).Activity)
	assert.Len(t, watchedAgentCalls(env), 1, "stale same-period idle delivered after working event must not re-notify")

	// Fresh dispatcher after a Hub restart must also reject the stale re-report.
	restartNotificationDispatcher(env, &active)
	env.pub.PublishAgentStatus(ctx, staleIdle)
	flushNotificationDispatcher(t, env)
	assert.Len(t, watchedAgentCalls(env), 1, "post-restart stale idle re-report after the row moved on must not re-notify")
}

func TestNotificationDispatcher_PerPeriodEventOrderings(t *testing.T) {
	type orderingCase struct {
		name      string
		wantCalls int
		run       func(t *testing.T, env *notificationTestEnv, active **NotificationDispatcher, act string, restart bool)
	}

	orderings := []orderingCase{
		{
			name:      "period_working_new_period",
			wantCalls: 2,
			run: func(t *testing.T, env *notificationTestEnv, active **NotificationDispatcher, act string, restart bool) {
				reportAndPublishActivity(t, env, string(state.PhaseRunning), act)
				waitWatchedCalls(t, env, 1)
				if restart {
					restartNotificationDispatcher(env, active)
				}
				reportAndPublishActivity(t, env, string(state.PhaseRunning), string(state.ActivityWorking))
				reportAndPublishActivity(t, env, string(state.PhaseRunning), act)
				waitWatchedCalls(t, env, 2)
				flushNotificationDispatcher(t, env)
			},
		},
		{
			name:      "period_working_published_then_stale_same_period",
			wantCalls: 1,
			run: func(t *testing.T, env *notificationTestEnv, active **NotificationDispatcher, act string, restart bool) {
				ctx := context.Background()
				reportAndPublishActivity(t, env, string(state.PhaseRunning), act)
				waitWatchedCalls(t, env, 1)
				stale := mustGetAgent(t, env.store, env.watched.ID)
				reportAndPublishActivity(t, env, string(state.PhaseRunning), string(state.ActivityWorking))
				if restart {
					flushNotificationDispatcher(t, env)
					restartNotificationDispatcher(env, active)
				}
				env.pub.PublishAgentStatus(ctx, stale)
				flushNotificationDispatcher(t, env)
				assert.Equal(t, string(state.ActivityWorking), mustGetAgent(t, env.store, env.watched.ID).Activity)
			},
		},
		{
			name:      "period_stale_then_working",
			wantCalls: 1,
			run: func(t *testing.T, env *notificationTestEnv, active **NotificationDispatcher, act string, restart bool) {
				ctx := context.Background()
				reportAndPublishActivity(t, env, string(state.PhaseRunning), act)
				waitWatchedCalls(t, env, 1)
				stale := mustGetAgent(t, env.store, env.watched.ID)
				if restart {
					restartNotificationDispatcher(env, active)
				}
				env.pub.PublishAgentStatus(ctx, stale)
				reportAndPublishActivity(t, env, string(state.PhaseRunning), string(state.ActivityWorking))
				flushNotificationDispatcher(t, env)
			},
		},
		{
			name:      "rereport_x3",
			wantCalls: 1,
			run: func(t *testing.T, env *notificationTestEnv, active **NotificationDispatcher, act string, restart bool) {
				reportAndPublishActivity(t, env, string(state.PhaseRunning), act)
				waitWatchedCalls(t, env, 1)
				if restart {
					restartNotificationDispatcher(env, active)
				}
				reportAndPublishActivity(t, env, string(state.PhaseRunning), act)
				reportAndPublishActivity(t, env, string(state.PhaseRunning), act)
				flushNotificationDispatcher(t, env)
			},
		},
		{
			name:      "two_periods_then_stale_first_period",
			wantCalls: 2,
			run: func(t *testing.T, env *notificationTestEnv, active **NotificationDispatcher, act string, restart bool) {
				ctx := context.Background()
				reportAndPublishActivity(t, env, string(state.PhaseRunning), act)
				waitWatchedCalls(t, env, 1)
				staleFirst := mustGetAgent(t, env.store, env.watched.ID)

				reportAndPublishActivity(t, env, string(state.PhaseRunning), string(state.ActivityWorking))
				reportAndPublishActivity(t, env, string(state.PhaseRunning), act)
				waitWatchedCalls(t, env, 2)
				if restart {
					restartNotificationDispatcher(env, active)
				}
				env.pub.PublishAgentStatus(ctx, staleFirst)
				flushNotificationDispatcher(t, env)
			},
		},
	}

	for _, act := range []state.Activity{state.ActivityIdle, state.ActivityCompleted} {
		trigger := strings.ToUpper(string(act))
		for _, tc := range orderings {
			for _, restart := range []bool{false, true} {
				mode := "live"
				if restart {
					mode = "after_restart"
				}
				t.Run(trigger+"/"+tc.name+"/"+mode, func(t *testing.T) {
					env := setupNotificationTest(t)
					ctx := context.Background()
					require.NoError(t, env.store.UpdateNotificationSubscriptionTriggers(ctx, env.sub.ID, []string{trigger}))
					active := env.nd
					active.Start()
					defer func() { active.Stop() }()

					tc.run(t, env, &active, string(act), restart)

					calls := watchedAgentCalls(env)
					require.Len(t, calls, tc.wantCalls)
					for _, c := range calls {
						assert.Contains(t, c.Message, trigger)
					}
				})
			}
		}
	}

	for _, nonPerPeriod := range []state.Activity{state.ActivityStalled, state.ActivityHarnessError} {
		trigger := strings.ToUpper(string(nonPerPeriod))
		for _, restart := range []bool{false, true} {
			mode := "live"
			if restart {
				mode = "after_restart"
			}
			t.Run(trigger+"/status_dedupe_across_working/"+mode, func(t *testing.T) {
				env := setupNotificationTest(t)
				ctx := context.Background()
				require.NoError(t, env.store.UpdateNotificationSubscriptionTriggers(ctx, env.sub.ID, []string{trigger}))
				active := env.nd
				active.Start()
				defer func() { active.Stop() }()

				reportAndPublishActivity(t, env, string(state.PhaseRunning), string(nonPerPeriod))
				waitWatchedCalls(t, env, 1)
				if restart {
					restartNotificationDispatcher(env, &active)
				}
				reportAndPublishActivity(t, env, string(state.PhaseRunning), string(state.ActivityWorking))
				reportAndPublishActivity(t, env, string(state.PhaseRunning), string(nonPerPeriod))
				flushNotificationDispatcher(t, env)

				calls := watchedAgentCalls(env)
				require.Len(t, calls, 1, "%s must keep status-based deduplication across %s -> working -> %s", trigger, nonPerPeriod, nonPerPeriod)
				assert.Contains(t, calls[0].Message, trigger)
			})
		}
	}
}

func TestNotificationDispatcher_NonPerPeriodTriggersKeepStatusDeduplication(t *testing.T) {
	env := setupNotificationTest(t)
	ctx := context.Background()
	require.NoError(t, env.store.UpdateNotificationSubscriptionTriggers(ctx, env.sub.ID, []string{"STALLED", "WAITING_FOR_INPUT", "IDLE"}))
	env.nd.Start()
	defer env.nd.Stop()

	// stalled -> working -> stalled must still deduplicate STALLED (not per-period).
	reportAndPublishActivity(t, env, string(state.PhaseRunning), string(state.ActivityStalled))
	waitWatchedCalls(t, env, 1)
	assert.Contains(t, watchedAgentCalls(env)[0].Message, "STALLED")

	reportAndPublishActivity(t, env, string(state.PhaseRunning), string(state.ActivityWorking))
	reportAndPublishActivity(t, env, string(state.PhaseRunning), string(state.ActivityStalled))
	flushNotificationDispatcher(t, env)
	assert.Len(t, watchedAgentCalls(env), 1, "STALLED must keep status-based deduplication across stalled -> working -> stalled")

	// Switching to WAITING_FOR_INPUT notifies once, and waiting_for_input -> offline -> waiting_for_input stays deduplicated.
	reportAndPublishActivity(t, env, string(state.PhaseRunning), string(state.ActivityWaitingForInput))
	waitWatchedCalls(t, env, 2)
	reportAndPublishActivity(t, env, string(state.PhaseRunning), string(state.ActivityOffline))
	reportAndPublishActivity(t, env, string(state.PhaseRunning), string(state.ActivityWaitingForInput))
	flushNotificationDispatcher(t, env)
	assert.Len(t, watchedAgentCalls(env), 2, "WAITING_FOR_INPUT must keep status-based deduplication across offline blips")

	// Non-status project events (e.g. exposed-port updates) must not trigger notifications on IDLE periods.
	reportAndPublishActivity(t, env, string(state.PhaseRunning), string(state.ActivityIdle))
	waitWatchedCalls(t, env, 3)
	env.pub.PublishAgentPorts(ctx, mustGetAgent(t, env.store, env.watched.ID))
	reportAndPublishActivity(t, env, string(state.PhaseRunning), string(state.ActivityIdle))
	flushNotificationDispatcher(t, env)
	assert.Len(t, watchedAgentCalls(env), 3)
}

func TestNotificationDispatcher_AutoSuspendStalledWithRetainedActivityFiresSuspendedOnly(t *testing.T) {
	ctx := context.Background()
	for _, preStall := range []state.Activity{state.ActivityCompleted, state.ActivityIdle, state.ActivityHarnessError} {
		t.Run("stalled_from_"+string(preStall), func(t *testing.T) {
			srv, s, _, _ := engineTestServer(t)
			disp := &recordingDispatcher{}
			nd := NewNotificationDispatcher(s, srv.events, func() AgentDispatcher { return disp }, slog.Default())
			nd.Start()
			defer nd.Stop()

			agent := createSuspendActivityTestAgent(
				t, s, "notif-autosusp-"+string(preStall), string(state.ActivityStalled), string(preStall),
			)

			subscriber := &store.Agent{
				ID:              tid("notif-sub-" + string(preStall)),
				Name:            "notif-sub-" + string(preStall),
				Slug:            "notif-sub-" + string(preStall),
				ProjectID:       agent.ProjectID,
				RuntimeBrokerID: agent.RuntimeBrokerID,
				Phase:           string(state.PhaseRunning),
			}
			require.NoError(t, s.CreateAgent(ctx, subscriber))

			sub := &store.NotificationSubscription{
				ID:                tid("notif-autosusp-sub-" + string(preStall)),
				Scope:             store.SubscriptionScopeAgent,
				AgentID:           agent.ID,
				ProjectID:         agent.ProjectID,
				SubscriberType:    store.SubscriberTypeAgent,
				SubscriberID:      subscriber.Slug,
				TriggerActivities: []string{"COMPLETED", "IDLE", "HARNESS_ERROR", "STALLED", "SUSPENDED"},
				CreatedAt:         time.Now(),
				CreatedBy:         "test",
			}
			require.NoError(t, s.CreateNotificationSubscription(ctx, sub))

			srv.autoSuspendStalledAgents(ctx, []store.Agent{*mustGetAgent(t, s, agent.ID)})

			persisted := mustGetAgent(t, s, agent.ID)
			assert.Equal(t, string(state.PhaseSuspended), persisted.Phase)
			assert.Equal(t, string(state.SuspendReasonStalled), persisted.SuspendReason)
			assert.Equal(t, string(preStall), persisted.Activity)

			require.Eventually(t, func() bool {
				return len(disp.getCalls()) >= 1
			}, 2*time.Second, 20*time.Millisecond)
			time.Sleep(100 * time.Millisecond)

			calls := disp.getCalls()
			require.Len(t, calls, 1, "auto-suspend with reason=stalled and retained %s must fire SUSPENDED only", preStall)
			assert.Contains(t, calls[0].Message, "SUSPENDED")
			assert.NotContains(t, calls[0].Message, strings.ToUpper(string(preStall)))

			notifs, err := s.GetNotifications(ctx, store.SubscriberTypeAgent, subscriber.Slug, false)
			require.NoError(t, err)
			require.Len(t, notifs, 1)
			assert.Equal(t, "SUSPENDED", notifs[0].Status)
		})
	}
}
