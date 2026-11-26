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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestApplyStartExtras_PriorStatus(t *testing.T) {
	payload := map[string]interface{}{}
	applyStartExtras(payload, StartExtras{})
	assert.NotContains(t, payload, "priorStatus", "nil PriorStatus sends nothing")

	ts := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	ps := &api.PriorStatus{
		Activity: string(state.ActivityCompleted),
		Summary:  "PR #123 opened",
		At:       ts,
	}
	payload = map[string]interface{}{}
	applyStartExtras(payload, StartExtras{PriorStatus: ps})
	got, ok := payload["priorStatus"]
	require.True(t, ok, "priorStatus must be written on the wire")
	assert.Equal(t, ps, got)
}

// TestDispatch_StartAndRestartCarryPriorStatus verifies that DispatchAgentStart
// and DispatchAgentRestart snapshot PriorStatus from the agent record, and that
// DispatchAgentStart skips falling back to AppliedConfig.Task when the prior
// run already completed (ptone/scion#8900).
func TestDispatch_StartAndRestartCarryPriorStatus(t *testing.T) {
	ctx := context.Background()
	ts := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)

	t.Run("stopped completed agent suppresses AppliedConfig.Task fallback", func(t *testing.T) {
		d, client, ag := autoExposeDispatchFixture(t)
		ag.Phase = string(state.PhaseStopped)
		ag.Activity = string(state.ActivityCompleted)
		ag.TaskSummary = "finished review"
		ag.LastActivityEvent = ts
		ag.AppliedConfig = &store.AgentAppliedConfig{
			Task:         "original brief task",
			CreateInputs: &store.AgentCreateInputs{},
		}

		require.NoError(t, d.DispatchAgentStart(ctx, ag, "", false))
		assert.Empty(t, client.lastTask, "completed agent must not replay AppliedConfig.Task")
		require.NotNil(t, client.lastStartExtras.PriorStatus)
		assert.Equal(t, string(state.ActivityCompleted), client.lastStartExtras.PriorStatus.Activity)
		assert.Equal(t, "finished review", client.lastStartExtras.PriorStatus.Summary)
		assert.Equal(t, ts, client.lastStartExtras.PriorStatus.At)
	})

	t.Run("stopped completed agent with explicit task passes explicit task", func(t *testing.T) {
		d, client, ag := autoExposeDispatchFixture(t)
		ag.Phase = string(state.PhaseStopped)
		ag.Activity = string(state.ActivityCompleted)
		ag.TaskSummary = "finished review"
		ag.LastActivityEvent = ts
		ag.AppliedConfig = &store.AgentAppliedConfig{
			Task:         "original brief task",
			CreateInputs: &store.AgentCreateInputs{},
		}

		require.NoError(t, d.DispatchAgentStart(ctx, ag, "follow-up task", false))
		assert.Equal(t, "follow-up task", client.lastTask)
		require.NotNil(t, client.lastStartExtras.PriorStatus)
	})

	t.Run("suspended completed agent carries PriorStatus with empty task", func(t *testing.T) {
		d, client, ag := autoExposeDispatchFixture(t)
		ag.Phase = string(state.PhaseSuspended)
		ag.Activity = string(state.ActivityCompleted)
		ag.TaskSummary = "all done"
		ag.LastActivityEvent = ts
		ag.AppliedConfig = &store.AgentAppliedConfig{
			Task:         "original brief task",
			CreateInputs: &store.AgentCreateInputs{},
		}

		require.NoError(t, d.DispatchAgentStart(ctx, ag, "", true))
		assert.Empty(t, client.lastTask)
		require.NotNil(t, client.lastStartExtras.PriorStatus)
		assert.Equal(t, string(state.ActivityCompleted), client.lastStartExtras.PriorStatus.Activity)
		assert.Equal(t, "all done", client.lastStartExtras.PriorStatus.Summary)
	})

	t.Run("stopped non-completed agent keeps AppliedConfig.Task fallback", func(t *testing.T) {
		d, client, ag := autoExposeDispatchFixture(t)
		ag.Phase = string(state.PhaseStopped)
		ag.Activity = ""
		ag.AppliedConfig = &store.AgentAppliedConfig{
			Task:         "original brief task",
			CreateInputs: &store.AgentCreateInputs{},
		}

		require.NoError(t, d.DispatchAgentStart(ctx, ag, "", false))
		assert.Equal(t, "original brief task", client.lastTask)
		assert.Nil(t, client.lastStartExtras.PriorStatus)
	})

	t.Run("halted completed agent suppresses AppliedConfig.Task fallback", func(t *testing.T) {
		d, client, ag := autoExposeDispatchFixture(t)
		ag.Phase = string(state.PhaseHalted)
		ag.Activity = string(state.ActivityCompleted)
		ag.ExitReason = string(state.ExitReasonPreempted)
		ag.TaskSummary = "finished before preemption"
		ag.LastActivityEvent = ts
		ag.AppliedConfig = &store.AgentAppliedConfig{
			Task:         "original brief task",
			CreateInputs: &store.AgentCreateInputs{},
		}

		require.NoError(t, d.DispatchAgentStart(ctx, ag, "", false))
		assert.Empty(t, client.lastTask, "halted completed agent must not replay AppliedConfig.Task")
		require.NotNil(t, client.lastStartExtras.PriorStatus)
		assert.Equal(t, string(state.ActivityCompleted), client.lastStartExtras.PriorStatus.Activity)
		assert.Equal(t, "finished before preemption", client.lastStartExtras.PriorStatus.Summary)
		assert.Equal(t, ts, client.lastStartExtras.PriorStatus.At)
	})

	t.Run("restart carries PriorStatus", func(t *testing.T) {
		d, client, ag := autoExposeDispatchFixture(t)
		ag.Phase = string(state.PhaseRunning)
		ag.Activity = string(state.ActivityCompleted)
		ag.TaskSummary = "shipped fix"
		ag.LastActivityEvent = ts

		require.NoError(t, d.DispatchAgentRestart(ctx, ag))
		require.NotNil(t, client.lastRestartExtras.PriorStatus)
		assert.Equal(t, string(state.ActivityCompleted), client.lastRestartExtras.PriorStatus.Activity)
		assert.Equal(t, "shipped fix", client.lastRestartExtras.PriorStatus.Summary)
		assert.Equal(t, ts, client.lastRestartExtras.PriorStatus.At)
	})
}

func TestNotificationEventStatus_StoppedAndHaltedRetainedCompleted(t *testing.T) {
	cases := []struct {
		name     string
		phase    string
		activity string
		want     string
	}{
		{"stopped with retained completed matches stopped", string(state.PhaseStopped), string(state.ActivityCompleted), string(state.PhaseStopped)},
		{"stopped with terminal crashed matches crashed", string(state.PhaseStopped), string(state.ActivityCrashed), string(state.ActivityCrashed)},
		{"stopped with terminal limits_exceeded matches limits_exceeded", string(state.PhaseStopped), string(state.ActivityLimitsExceeded), string(state.ActivityLimitsExceeded)},
		{"stopped with synthetic DELETED matches DELETED", string(state.PhaseStopped), "DELETED", "DELETED"},
		{"stopped with empty activity matches stopped", string(state.PhaseStopped), "", string(state.PhaseStopped)},
		{"halted with retained completed matches halted", string(state.PhaseHalted), string(state.ActivityCompleted), string(state.PhaseHalted)},
		{"halted with synthetic ERROR matches ERROR", string(state.PhaseHalted), "ERROR", "ERROR"},
		{"suspended with retained completed matches suspended", string(state.PhaseSuspended), string(state.ActivityCompleted), string(state.PhaseSuspended)},
		{"running with completed matches completed", string(state.PhaseRunning), string(state.ActivityCompleted), string(state.ActivityCompleted)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := notificationEventStatus(AgentStatusEvent{Phase: tc.phase, Activity: tc.activity})
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestNotificationDispatcher_StoppedAfterCompletedNotifiesStopped verifies R1:
// when a running agent reports completed and is then stopped (retaining
// activity=completed on the stopped row), a STOPPED subscriber still receives
// STOPPED, a COMPLETED+STOPPED subscriber receives both without deduplication,
// and a COMPLETED-only subscriber added after completion does not receive a
// duplicate COMPLETED on stop.
func TestNotificationDispatcher_StoppedAfterCompletedNotifiesStopped(t *testing.T) {
	ctx := context.Background()
	env := setupNotificationTest(t)
	require.NoError(t, env.store.DeleteNotificationSubscription(ctx, env.sub.ID))

	addSub := func(userID string, triggers ...string) {
		require.NoError(t, env.store.CreateNotificationSubscription(ctx, &store.NotificationSubscription{
			ID:                api.NewUUID(),
			Scope:             store.SubscriptionScopeAgent,
			AgentID:           env.watched.ID,
			SubscriberType:    store.SubscriberTypeUser,
			SubscriberID:      userID,
			ProjectID:         env.project.ID,
			TriggerActivities: triggers,
			CreatedAt:         time.Now().Add(-time.Minute),
			CreatedBy:         "test",
		}))
	}

	addSub("stopped-only", "STOPPED")
	addSub("both-sub", "COMPLETED", "STOPPED")

	env.nd.Start()
	defer env.nd.Stop()

	// 1. Agent completes while running.
	require.NoError(t, env.store.UpdateAgentStatus(ctx, env.watched.ID, store.AgentStatusUpdate{
		Phase:    string(state.PhaseRunning),
		Activity: string(state.ActivityCompleted),
	}))
	completedRow, err := env.store.GetAgent(ctx, env.watched.ID)
	require.NoError(t, err)
	env.pub.PublishAgentStatus(ctx, completedRow)

	require.Eventually(t, func() bool {
		notifs, err := env.store.GetNotifications(ctx, store.SubscriberTypeUser, "both-sub", false)
		return err == nil && len(notifs) == 1
	}, 2*time.Second, 20*time.Millisecond)

	// Subscribe a COMPLETED-only observer after completion.
	addSub("late-completed-only", "COMPLETED")

	// 2. Agent is stopped; store retains activity=completed on the stopped row.
	require.NoError(t, env.store.UpdateAgentStatus(ctx, env.watched.ID, store.AgentStatusUpdate{
		Phase: string(state.PhaseStopped),
	}))
	stoppedRow, err := env.store.GetAgent(ctx, env.watched.ID)
	require.NoError(t, err)
	require.Equal(t, string(state.PhaseStopped), stoppedRow.Phase)
	require.Equal(t, string(state.ActivityCompleted), stoppedRow.Activity)

	env.pub.PublishAgentStatus(ctx, stoppedRow)

	require.Eventually(t, func() bool {
		notifs, err := env.store.GetNotifications(ctx, store.SubscriberTypeUser, "stopped-only", false)
		return err == nil && len(notifs) == 1
	}, 2*time.Second, 20*time.Millisecond)

	require.Eventually(t, func() bool {
		notifs, err := env.store.GetNotifications(ctx, store.SubscriberTypeUser, "both-sub", false)
		return err == nil && len(notifs) == 2
	}, 2*time.Second, 20*time.Millisecond)

	stoppedOnly, err := env.store.GetNotifications(ctx, store.SubscriberTypeUser, "stopped-only", false)
	require.NoError(t, err)
	require.Len(t, stoppedOnly, 1)
	assert.Equal(t, "STOPPED", stoppedOnly[0].Status)

	bothNotifs, err := env.store.GetNotifications(ctx, store.SubscriberTypeUser, "both-sub", false)
	require.NoError(t, err)
	statuses := make([]string, 0, len(bothNotifs))
	for _, n := range bothNotifs {
		statuses = append(statuses, n.Status)
	}
	assert.ElementsMatch(t, []string{"COMPLETED", "STOPPED"}, statuses)

	lateCompleted, err := env.store.GetNotifications(ctx, store.SubscriberTypeUser, "late-completed-only", false)
	require.NoError(t, err)
	assert.Empty(t, lateCompleted, "stop after completed must not emit a duplicate COMPLETED notification")
}

func TestDispatchArgs_PriorStatusRoundTrip(t *testing.T) {
	ts := time.Date(2026, 11, 23, 12, 30, 0, 0, time.UTC)
	ps := &api.PriorStatus{
		Activity: string(state.ActivityCompleted),
		Summary:  "all tests green",
		At:       ts,
	}

	rawStart, err := MarshalDispatchArgs(&StartDispatchArgs{
		Task:        "",
		Resume:      false,
		PriorStatus: ps,
	})
	require.NoError(t, err)
	gotStart, err := UnmarshalStartArgs(rawStart)
	require.NoError(t, err)
	assert.Equal(t, ps, gotStart.PriorStatus)

	rawRestart, err := MarshalDispatchArgs(&RestartDispatchArgs{PriorStatus: ps})
	require.NoError(t, err)
	gotRestart, err := UnmarshalRestartArgs(rawRestart)
	require.NoError(t, err)
	assert.Equal(t, ps, gotRestart.PriorStatus)

	rawEmptyRestart, err := MarshalDispatchArgs(&RestartDispatchArgs{})
	require.NoError(t, err)
	assert.Equal(t, "{}", rawEmptyRestart)
}

// TestExecDispatchStartAndRestart_PreservesPriorStatusAcrossDeferredHandoff
// verifies R3: on a cross-node deferred start or restart, the requester has
// already written PhaseStarting (clearing any retained activity on the row).
// The owning node's execDispatchStart / execDispatchRestart must preserve the
// PriorStatus carried in StartDispatchArgs / RestartDispatchArgs rather than
// re-reading the cleared row and falling back to AppliedConfig.Task.
func TestExecDispatchStartAndRestart_PreservesPriorStatusAcrossDeferredHandoff(t *testing.T) {
	ctx := context.Background()
	ts := time.Date(2026, 11, 23, 12, 45, 0, 0, time.UTC)
	ps := &api.PriorStatus{
		Activity: string(state.ActivityCompleted),
		Summary:  "completed prior task",
		At:       ts,
	}

	d, client, ag := autoExposeDispatchFixture(t)
	srv, _ := testServerOnMigratedStore(t, d.store)
	srv.SetDispatcher(d)

	ownerID := tid("deferred-prior-status-owner")
	createTestUserWithProjectRole(t, d.store, ownerID, "deferred-prior-status-owner@example.com", ag.ProjectID, store.ProjectRoleMember)
	ag.ID = tid("deferred-prior-status-agent")
	ag.OwnerID = ownerID
	ag.Phase = string(state.PhaseStarting)
	ag.Activity = "" // cleared by requester's beginStartDispatch before deferring
	ag.AppliedConfig = &store.AgentAppliedConfig{
		Task:         "original brief task",
		CreateInputs: &store.AgentCreateInputs{},
	}
	require.NoError(t, d.store.CreateAgent(ctx, ag))

	startArgsRaw, err := MarshalDispatchArgs(&StartDispatchArgs{PriorStatus: ps})
	require.NoError(t, err)
	_, err = srv.execDispatchStart(ctx, store.BrokerDispatch{
		ID:        "disp-start-prior",
		BrokerID:  ag.RuntimeBrokerID,
		AgentID:   ag.ID,
		ProjectID: ag.ProjectID,
		Op:        "start",
		Args:      startArgsRaw,
	})
	require.NoError(t, err)
	assert.Empty(t, client.lastTask, "deferred start with completed PriorStatus must not replay AppliedConfig.Task")
	require.NotNil(t, client.lastStartExtras.PriorStatus)
	assert.Equal(t, ps, client.lastStartExtras.PriorStatus)

	// Unnormalized activity casing in JSON-decoded StartDispatchArgs must also
	// be recognized as completed (matching broker isPriorStatusCompleted).
	upperPS := &api.PriorStatus{
		Activity: " COMPLETED ",
		Summary:  "completed prior task",
		At:       ts,
	}
	upperStartArgsRaw, err := MarshalDispatchArgs(&StartDispatchArgs{PriorStatus: upperPS})
	require.NoError(t, err)
	client.lastTask = "sentinel"
	_, err = srv.execDispatchStart(ctx, store.BrokerDispatch{
		ID:        "disp-start-prior-upper",
		BrokerID:  ag.RuntimeBrokerID,
		AgentID:   ag.ID,
		ProjectID: ag.ProjectID,
		Op:        "start",
		Args:      upperStartArgsRaw,
	})
	require.NoError(t, err)
	assert.Empty(t, client.lastTask, "deferred start with unnormalized COMPLETED PriorStatus must not replay AppliedConfig.Task")

	restartArgsRaw, err := MarshalDispatchArgs(&RestartDispatchArgs{PriorStatus: ps})
	require.NoError(t, err)
	_, err = srv.execDispatchRestart(ctx, store.BrokerDispatch{
		ID:        "disp-restart-prior",
		BrokerID:  ag.RuntimeBrokerID,
		AgentID:   ag.ID,
		ProjectID: ag.ProjectID,
		Op:        "restart",
		Args:      restartArgsRaw,
	})
	require.NoError(t, err)
	require.NotNil(t, client.lastRestartExtras.PriorStatus)
	assert.Equal(t, ps, client.lastRestartExtras.PriorStatus)
}

// TestApplyStartExtras_WakeForMessage (T10a) verifies that WakeForMessage is
// serialized as wakeForMessage only when true.
func TestApplyStartExtras_WakeForMessage(t *testing.T) {
	payload := map[string]any{}
	applyStartExtras(payload, StartExtras{})
	assert.NotContains(t, payload, "wakeForMessage", "false WakeForMessage sends nothing")

	applyStartExtras(payload, StartExtras{WakeForMessage: true})
	assert.Equal(t, true, payload["wakeForMessage"])
}

// TestExecDispatchStart_PreservesWakeForMessageAcrossDeferredHandoff (T10b)
// verifies that StartDispatchArgs round-trips wakeForMessage and the owning
// node's execDispatchStart forwards it into StartExtras.WakeForMessage.
func TestExecDispatchStart_PreservesWakeForMessageAcrossDeferredHandoff(t *testing.T) {
	ctx := context.Background()

	raw, err := MarshalDispatchArgs(&StartDispatchArgs{
		Resume:         true,
		WakeForMessage: true,
	})
	require.NoError(t, err)
	got, err := UnmarshalStartArgs(raw)
	require.NoError(t, err)
	assert.True(t, got.WakeForMessage)
	assert.True(t, got.Resume)

	d, client, ag := autoExposeDispatchFixture(t)
	srv, _ := testServerOnMigratedStore(t, d.store)
	srv.SetDispatcher(d)

	ownerID := tid("deferred-wake-msg-owner")
	createTestUserWithProjectRole(t, d.store, ownerID, "deferred-wake-msg-owner@example.com", ag.ProjectID, store.ProjectRoleMember)
	ag.ID = tid("deferred-wake-msg-agent")
	ag.OwnerID = ownerID
	ag.Phase = string(state.PhaseStarting)
	ag.AppliedConfig = &store.AgentAppliedConfig{
		CreateInputs: &store.AgentCreateInputs{},
	}
	require.NoError(t, d.store.CreateAgent(ctx, ag))

	_, err = srv.execDispatchStart(ctx, store.BrokerDispatch{
		ID:        "disp-start-wake-msg",
		BrokerID:  ag.RuntimeBrokerID,
		AgentID:   ag.ID,
		ProjectID: ag.ProjectID,
		Op:        "start",
		Args:      raw,
	})
	require.NoError(t, err)
	assert.True(t, client.lastStartExtras.WakeForMessage, "owner execDispatchStart must forward WakeForMessage into StartExtras")
}
