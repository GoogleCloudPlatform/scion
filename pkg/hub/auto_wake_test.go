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
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Auto-wake on new work (ptone/scion#7826 phase 2b): an agent the hub
// suspended (suspend_reason idle or stalled) is woken by new work with no
// --wake, under lifecycle.idle_suspend; a user-suspended agent, a held agent
// and the experiment off keep today's behaviour.

// waitAutoWakeIdle waits until every background auto-wake has ended.
func (s *Server) waitAutoWakeIdle() {
	s.autoWakeWork.wg.Wait()
}

// setIdleSuspendExperiment turns lifecycle.idle_suspend on or off for srv.
func setIdleSuspendExperiment(t *testing.T, srv *Server, on bool) {
	t.Helper()
	var active []experiments.Experiment
	for _, e := range experiments.Default().All() {
		if e.Name == experiments.IdleSuspend {
			e.Default = on
		}
		active = append(active, e)
	}
	reg, err := experiments.NewRegistry(active, nil)
	require.NoError(t, err)
	srv.experiments = reg
	require.Equal(t, on, srv.experimentEnabled(experiments.IdleSuspend))
}

// awSuspendFor stores agent as suspended with reason, as the suspend paths
// do, and refreshes the caller's copy.
func awSuspendFor(t *testing.T, s store.Store, agent *store.Agent, reason state.SuspendReason) {
	t.Helper()
	require.NoError(t, s.UpdateAgentStatus(context.Background(), agent.ID, store.AgentStatusUpdate{
		Phase: string(state.PhaseSuspended), SuspendReason: string(reason),
	}))
	got, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	*agent = *got
}

// awReadyOnStart reports agentID running, as the resumed container's first
// status would, each time a wake has written starting, until the test ends.
func awReadyOnStart(t *testing.T, s store.Store, agentID string) {
	t.Helper()
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	t.Cleanup(func() { close(done); wg.Wait() })
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			case <-time.After(20 * time.Millisecond):
			}
			a, err := s.GetAgent(context.Background(), agentID)
			if err == nil && a.Phase == string(state.PhaseStarting) {
				_ = s.UpdateAgentStatus(context.Background(), agentID, store.AgentStatusUpdate{
					Phase: string(state.PhaseRunning), Activity: "idle",
				})
			}
		}
	}()
}

// autoWakeFixture is createWakeDMFixtures with the experiment on, the
// target suspended for reason, and a recording dispatcher.
func autoWakeFixture(t *testing.T, reason state.SuspendReason) (*Server, store.Store, *store.Agent, *store.Agent, *wakeTrackingDispatcher) {
	t.Helper()
	srv, s, sender, target := createWakeDMFixtures(t, string(state.PhaseRunning))
	setIdleSuspendExperiment(t, srv, true)
	awSuspendFor(t, s, target, reason)
	disp := &wakeTrackingDispatcher{}
	srv.SetDispatcher(disp)
	t.Cleanup(srv.waitAutoWakeIdle)
	return srv, s, sender, target, disp
}

func awAgentPhase(t *testing.T, s store.Store, id string) string {
	t.Helper()
	a, err := s.GetAgent(context.Background(), id)
	require.NoError(t, err)
	return a.Phase
}

// --- shouldAutoWake and the recorded reason --------------------------------

func TestShouldAutoWake(t *testing.T) {
	srv, _ := testServer(t)
	cases := []struct {
		phase  state.Phase
		reason state.SuspendReason
		on     bool
		want   bool
	}{
		{state.PhaseSuspended, state.SuspendReasonIdle, true, true},
		{state.PhaseSuspended, state.SuspendReasonStalled, true, true},
		{state.PhaseSuspended, state.SuspendReasonUser, true, false},
		{state.PhaseSuspended, "", true, false},
		{state.PhaseRunning, state.SuspendReasonIdle, true, false},
		{state.PhaseStopped, state.SuspendReasonIdle, true, false},
		{state.PhaseSuspended, state.SuspendReasonIdle, false, false},
		{state.PhaseSuspended, state.SuspendReasonStalled, false, false},
	}
	for _, tc := range cases {
		setIdleSuspendExperiment(t, srv, tc.on)
		a := &store.Agent{ID: "a", Phase: string(tc.phase), SuspendReason: string(tc.reason)}
		assert.Equal(t, tc.want, srv.shouldAutoWake(a), "phase=%s reason=%q experiment=%v", tc.phase, tc.reason, tc.on)
	}
	assert.False(t, srv.shouldAutoWake(nil))
}

func TestSuspendReason_ManualSuspendRecordsUser(t *testing.T) {
	disp := &wakeTrackingDispatcher{}
	srv, s, project := setupCreateAgentServer(t, disp)
	agent := createSiteAgent(t, s, project, "sr-manual", state.PhaseRunning, store.RunIntentRunning)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/suspend", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	got, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseSuspended), got.Phase)
	assert.Equal(t, "user", got.SuspendReason)

	// The API shows it.
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/agents/"+agent.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "user", body["suspendReason"])
}

func TestSuspendReason_AutoSuspendStalledRecordsStalled(t *testing.T) {
	srv, s := testServer(t)
	disp := newGatedStopDispatcher()
	close(disp.release)
	srv.SetDispatcher(disp)
	a := newStalledBrokerAgent(t, s, "sr-stalled")

	srv.enqueueAutoSuspend([]store.Agent{*a})
	srv.waitAutoSuspendIdle()

	got, err := s.GetAgent(context.Background(), a.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseSuspended), got.Phase)
	assert.Equal(t, "stalled", got.SuspendReason)
}

// --- Agent DM (ExecuteAgentDM) ----------------------------------------------

func awAgentDMInput(sender, target *store.Agent, msg string) *AgentDMInput {
	return &AgentDMInput{
		SenderAgent: sender,
		// Delivery permission only: no agent.lifecycle scope.
		SenderIdentity: wakeDMSenderIdentity(sender, ScopeProjectRead),
		TargetAgent:    target,
		Msg:            msg,
		Type:           "instruction",
		ProjectID:      sender.ProjectID,
	}
}

func TestAutoWake_AgentDM(t *testing.T) {
	for _, reason := range []state.SuspendReason{state.SuspendReasonIdle, state.SuspendReasonStalled} {
		t.Run(string(reason)+" wakes and delivers without wake or lifecycle", func(t *testing.T) {
			srv, s, sender, target, disp := autoWakeFixture(t, reason)
			awReadyOnStart(t, s, target.ID)

			res, dmErr := srv.ExecuteAgentDM(context.Background(), awAgentDMInput(sender, target, "new work"))
			require.Nil(t, dmErr)
			assert.Equal(t, AgentDMAccepted, res.Outcome)
			require.Len(t, disp.getStartCalls(), 1, "one resume")
			assert.True(t, disp.getStartCalls()[0].Continue)
			msgs := disp.getMessageCalls()
			require.Len(t, msgs, 1)
			assert.Equal(t, "new work", msgs[0].Message)
			assert.Equal(t, string(state.PhaseRunning), awAgentPhase(t, s, target.ID))
			got, err := s.GetAgent(context.Background(), target.ID)
			require.NoError(t, err)
			assert.Empty(t, got.SuspendReason, "a resume clears the reason")
		})
	}

	t.Run("user-suspended is refused as today", func(t *testing.T) {
		srv, _, sender, target, disp := autoWakeFixture(t, state.SuspendReasonUser)
		_, dmErr := srv.ExecuteAgentDM(context.Background(), awAgentDMInput(sender, target, "x"))
		require.NotNil(t, dmErr)
		assert.Equal(t, ErrCodeAgentNotRunning, dmErr.Code)
		assert.Empty(t, disp.getStartCalls())
	})

	t.Run("an explicit wake to a user-suspended agent still needs lifecycle", func(t *testing.T) {
		srv, _, sender, target, disp := autoWakeFixture(t, state.SuspendReasonUser)
		in := awAgentDMInput(sender, target, "x")
		in.Wake = true
		_, dmErr := srv.ExecuteAgentDM(context.Background(), in)
		require.NotNil(t, dmErr)
		assert.Equal(t, http.StatusForbidden, dmErr.HTTPStatus)
		assert.Empty(t, disp.getStartCalls())
	})

	t.Run("experiment off is refused as today", func(t *testing.T) {
		srv, _, sender, target, disp := autoWakeFixture(t, state.SuspendReasonIdle)
		setIdleSuspendExperiment(t, srv, false)
		_, dmErr := srv.ExecuteAgentDM(context.Background(), awAgentDMInput(sender, target, "x"))
		require.NotNil(t, dmErr)
		assert.Equal(t, ErrCodeAgentNotRunning, dmErr.Code)
		assert.Empty(t, disp.getStartCalls())
	})

	t.Run("a held agent is never woken", func(t *testing.T) {
		srv, s, sender, target, disp := autoWakeFixture(t, state.SuspendReasonIdle)
		holdAgentFor(t, s, target.ID, target.ProjectID, target.Ancestry[0])
		_, dmErr := srv.ExecuteAgentDM(context.Background(), awAgentDMInput(sender, target, "x"))
		require.NotNil(t, dmErr)
		assert.Empty(t, disp.getStartCalls())
		assert.Empty(t, disp.getMessageCalls())
		assert.Equal(t, string(state.PhaseSuspended), awAgentPhase(t, s, target.ID))
	})

	t.Run("a failed resume returns the wake error and dispatches nothing", func(t *testing.T) {
		srv, _, sender, target, disp := autoWakeFixture(t, state.SuspendReasonIdle)
		disp.startReturnErr = assert.AnError
		_, dmErr := srv.ExecuteAgentDM(context.Background(), awAgentDMInput(sender, target, "x"))
		require.NotNil(t, dmErr)
		assert.Len(t, disp.getStartCalls(), 1)
		assert.Empty(t, disp.getMessageCalls())
	})
}

// Concurrent direct messages to one idle-suspended agent resume it once:
// the start claim lets one wake run and the others defer (WakeDeferred).
func TestAutoWake_AgentDM_ConcurrentResumesOnce(t *testing.T) {
	srv, s, sender, target, disp := autoWakeFixture(t, state.SuspendReasonIdle)
	awReadyOnStart(t, s, target.ID)

	const n = 4
	var wg sync.WaitGroup
	errs := make([]*AgentDMError, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tgt := *target
			_, errs[i] = srv.ExecuteAgentDM(context.Background(), awAgentDMInput(sender, &tgt, "concurrent"))
		}(i)
	}
	wg.Wait()
	for i, e := range errs {
		assert.Nil(t, e, "send %d", i)
	}
	assert.Len(t, disp.getStartCalls(), 1, "concurrent wakes resume the agent once")
	assert.Equal(t, n, storedMessagesFor(t, s, target.ID), "every message is kept")
}

// --- Mention fan-out (async) ------------------------------------------------

func awMentionInput(sender, target *store.Agent, msg string) *AgentDMInput {
	in := awAgentDMInput(sender, target, msg)
	in.Type = messages.TypeMention
	in.SkipPhaseGate = true
	return in
}

func TestAutoWake_Mention(t *testing.T) {
	t.Run("idle-suspended is woken in the background, then delivered", func(t *testing.T) {
		srv, s, sender, target, disp := autoWakeFixture(t, state.SuspendReasonIdle)
		awReadyOnStart(t, s, target.ID)

		res, dmErr := srv.ExecuteAgentDM(context.Background(), awMentionInput(sender, target, "@target look"))
		require.Nil(t, dmErr)
		assert.Equal(t, AgentDMWakeQueued, res.Outcome, "the send does not wait for the wake")
		srv.waitAutoWakeIdle()

		require.Len(t, disp.getStartCalls(), 1)
		require.Len(t, disp.getMessageCalls(), 1)
		m, err := s.GetMessage(context.Background(), res.MessageID)
		require.NoError(t, err)
		assert.Equal(t, store.MessageDispatchDispatched, m.DispatchState)
	})

	t.Run("concurrent mentions share one resume and are all delivered", func(t *testing.T) {
		srv, s, sender, target, disp := autoWakeFixture(t, state.SuspendReasonIdle)
		awReadyOnStart(t, s, target.ID)
		var ids []string
		for i := 0; i < 3; i++ {
			tgt := *target
			res, dmErr := srv.ExecuteAgentDM(context.Background(), awMentionInput(sender, &tgt, "@target again"))
			require.Nil(t, dmErr)
			ids = append(ids, res.MessageID)
		}
		srv.waitAutoWakeIdle()
		assert.Len(t, disp.getStartCalls(), 1, "one resume")
		assert.Len(t, disp.getMessageCalls(), 3, "every queued mention is delivered")
		for _, id := range ids {
			m, err := s.GetMessage(context.Background(), id)
			require.NoError(t, err)
			assert.Equal(t, store.MessageDispatchDispatched, m.DispatchState)
		}
	})

	t.Run("a failed wake marks the mention failed", func(t *testing.T) {
		srv, s, sender, target, disp := autoWakeFixture(t, state.SuspendReasonIdle)
		disp.startReturnErr = assert.AnError
		res, dmErr := srv.ExecuteAgentDM(context.Background(), awMentionInput(sender, target, "@target"))
		require.Nil(t, dmErr)
		require.Equal(t, AgentDMWakeQueued, res.Outcome)
		srv.waitAutoWakeIdle()
		assert.Empty(t, disp.getMessageCalls())
		m, err := s.GetMessage(context.Background(), res.MessageID)
		require.NoError(t, err)
		assert.Equal(t, store.MessageDispatchFailed, m.DispatchState)
		require.NotNil(t, m.DispatchFailureReason)
		assert.Contains(t, *m.DispatchFailureReason, "auto-wake failed")
	})

	t.Run("user-suspended takes today's buffered dispatch, no wake", func(t *testing.T) {
		srv, _, sender, target, disp := autoWakeFixture(t, state.SuspendReasonUser)
		res, dmErr := srv.ExecuteAgentDM(context.Background(), awMentionInput(sender, target, "@target"))
		require.Nil(t, dmErr)
		assert.Equal(t, AgentDMAccepted, res.Outcome)
		srv.waitAutoWakeIdle()
		assert.Empty(t, disp.getStartCalls())
		assert.Len(t, disp.getMessageCalls(), 1)
	})

	t.Run("a held agent keeps the mention deferred, no wake", func(t *testing.T) {
		srv, s, sender, target, disp := autoWakeFixture(t, state.SuspendReasonIdle)
		holdAgentFor(t, s, target.ID, target.ProjectID, target.Ancestry[0])
		res, dmErr := srv.ExecuteAgentDM(context.Background(), awMentionInput(sender, target, "@target"))
		require.Nil(t, dmErr)
		assert.Equal(t, AgentDMDeferred, res.Outcome)
		srv.waitAutoWakeIdle()
		assert.Empty(t, disp.getStartCalls())
	})
}

// --- User DM (handleAgentMessage) -------------------------------------------

func TestAutoWake_UserDM(t *testing.T) {
	send := func(t *testing.T, srv *Server, u *store.User, target *store.Agent) int {
		rec := requestAsIdentity(t, srv, authUser(u), http.MethodPost, "/api/v1/agents/"+target.ID+"/message",
			map[string]interface{}{"message": "hello"})
		t.Logf("response %d: %s", rec.Code, rec.Body.String())
		return rec.Code
	}

	t.Run("idle-suspended wakes for a message-only user without --wake", func(t *testing.T) {
		srv, s, _, target, disp := autoWakeFixture(t, state.SuspendReasonIdle)
		u := wakeLifecycleUser(t, s, "aw-user", target.ProjectID, "agent.message", "agent.read")
		awReadyOnStart(t, s, target.ID)
		require.Equal(t, http.StatusOK, send(t, srv, u, target))
		require.Len(t, disp.getStartCalls(), 1)
		require.Len(t, disp.getMessageCalls(), 1)
		assert.Equal(t, "hello", disp.getMessageCalls()[0].Message)
	})

	t.Run("user-suspended answers 409 as today", func(t *testing.T) {
		srv, s, _, target, disp := autoWakeFixture(t, state.SuspendReasonUser)
		u := wakeLifecycleUser(t, s, "aw-user-409", target.ProjectID, "agent.message", "agent.read")
		require.Equal(t, http.StatusConflict, send(t, srv, u, target))
		assert.Empty(t, disp.getStartCalls())
		assert.Zero(t, storedMessagesFor(t, s, target.ID))
	})

	t.Run("experiment off answers 409 as today", func(t *testing.T) {
		srv, s, _, target, disp := autoWakeFixture(t, state.SuspendReasonIdle)
		setIdleSuspendExperiment(t, srv, false)
		u := wakeLifecycleUser(t, s, "aw-user-off", target.ProjectID, "agent.message", "agent.read")
		require.Equal(t, http.StatusConflict, send(t, srv, u, target))
		assert.Empty(t, disp.getStartCalls())
	})

	t.Run("a held agent is refused, never woken", func(t *testing.T) {
		srv, s, _, target, disp := autoWakeFixture(t, state.SuspendReasonIdle)
		holdAgentFor(t, s, target.ID, target.ProjectID, target.Ancestry[0])
		u := wakeLifecycleUser(t, s, "aw-user-held", target.ProjectID, "agent.message", "agent.read")
		require.Equal(t, http.StatusConflict, send(t, srv, u, target))
		assert.Empty(t, disp.getStartCalls())
	})
}

// --- Scheduled message fire -------------------------------------------------

func TestAutoWake_ScheduledMessage(t *testing.T) {
	fixture := func(t *testing.T, reason state.SuspendReason) (*schedWake, *store.User) {
		f := newSchedWake(t, state.PhaseRunning)
		setIdleSuspendExperiment(t, f.srv, true)
		awSuspendFor(t, f.store, f.target, reason)
		// Message permission only: an explicit wake would be refused.
		return f, f.user(t, "aw-sched-"+string(reason), "agent.message", "agent.read")
	}

	t.Run("idle-suspended is woken without the payload's wake", func(t *testing.T) {
		f, u := fixture(t, state.SuspendReasonIdle)
		f.reportReadyOnStart(t)
		require.NoError(t, f.fire(withSessionRevision(f.event(false), u.ID)))
		require.Len(t, f.disp.getStartCalls(), 1)
		require.Len(t, f.disp.getMessageCalls(), 1)
		assert.Equal(t, string(state.PhaseRunning), f.phase(t))
	})

	t.Run("user-suspended fails as today", func(t *testing.T) {
		f, u := fixture(t, state.SuspendReasonUser)
		err := f.fire(withSessionRevision(f.event(false), u.ID))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "is suspended")
		assert.Empty(t, f.disp.getStartCalls())
	})

	t.Run("a held target is refused", func(t *testing.T) {
		f, u := fixture(t, state.SuspendReasonIdle)
		holdAgentFor(t, f.store, f.target.ID, f.target.ProjectID, f.target.Ancestry[0])
		err := f.fire(withSessionRevision(f.event(false), u.ID))
		require.Error(t, err)
		assert.Empty(t, f.disp.getStartCalls())
	})
}

// --- Notifications (async) --------------------------------------------------

// notificationAutoWakeFixture wires a notification dispatcher to srv, as
// StartNotificationDispatcher does, and stores one parked notification for
// target from sender (the creator fallback subscription, which needs no
// stored subscription).
func notificationAutoWakeFixture(t *testing.T, srv *Server, s store.Store, sender, target *store.Agent) (*store.NotificationSubscription, string) {
	t.Helper()
	nd := NewNotificationDispatcher(s, srv.events, srv.GetDispatcher, slog.Default())
	nd.autoWakeFn = srv.autoWakeNotificationSubscriber
	srv.notificationDispatcher = nd
	sub := &store.NotificationSubscription{
		ID:             store.CreatorFallbackSubscriptionID,
		SubscriberType: store.SubscriberTypeAgent,
		SubscriberID:   target.Slug,
		ProjectID:      target.ProjectID,
	}
	notif := &store.Notification{
		ID:             api.NewUUID(),
		SubscriptionID: sub.ID,
		AgentID:        sender.ID,
		ProjectID:      target.ProjectID,
		SubscriberType: store.SubscriberTypeAgent,
		SubscriberID:   target.Slug,
		Status:         "COMPLETED",
		Message:        "child has reached a state of COMPLETED",
		CreatedAt:      time.Now().Add(-2 * time.Minute),
	}
	require.NoError(t, s.CreateNotification(context.Background(), notif))
	return sub, notif.ID
}

func awNotification(t *testing.T, s store.Store, id string) *store.Notification {
	t.Helper()
	n, err := s.GetNotification(context.Background(), id)
	require.NoError(t, err)
	return n
}

func TestAutoWake_Notification(t *testing.T) {
	t.Run("idle-suspended subscriber is woken and the parked notification delivered", func(t *testing.T) {
		srv, s, sender, target, disp := autoWakeFixture(t, state.SuspendReasonIdle)
		sub, id := notificationAutoWakeFixture(t, srv, s, sender, target)
		awReadyOnStart(t, s, target.ID)

		srv.notificationDispatcher.dispatchToAgent(context.Background(), sub, awNotification(t, s, id), sender.ID, sender.Slug)
		srv.waitAutoWakeIdle()

		require.Len(t, disp.getStartCalls(), 1)
		require.Len(t, disp.getMessageCalls(), 1)
		n := awNotification(t, s, id)
		assert.True(t, n.Dispatched)
		assert.Equal(t, store.NotificationOutcomeDelivered, n.DispatchOutcome)
	})

	t.Run("several notifications share one resume and one drain", func(t *testing.T) {
		srv, s, sender, target, disp := autoWakeFixture(t, state.SuspendReasonStalled)
		sub, id1 := notificationAutoWakeFixture(t, srv, s, sender, target)
		_, id2 := notificationAutoWakeFixture(t, srv, s, sender, target)
		nd := srv.notificationDispatcher
		awReadyOnStart(t, s, target.ID)

		nd.dispatchToAgent(context.Background(), sub, awNotification(t, s, id1), sender.ID, sender.Slug)
		nd.dispatchToAgent(context.Background(), sub, awNotification(t, s, id2), sender.ID, sender.Slug)
		srv.waitAutoWakeIdle()

		assert.Len(t, disp.getStartCalls(), 1)
		assert.Len(t, disp.getMessageCalls(), 2, "both notifications are delivered")
	})

	t.Run("a failed wake leaves it parked and the next sweep retries", func(t *testing.T) {
		srv, s, sender, target, disp := autoWakeFixture(t, state.SuspendReasonIdle)
		sub, id := notificationAutoWakeFixture(t, srv, s, sender, target)
		// A start the broker never received: its claim is released at once
		// (an unconfirmed start would hold it until the start's hold ends).
		disp.mu2.Lock()
		disp.startReturnErr = errStartBrokerNotConnected
		disp.mu2.Unlock()

		srv.notificationDispatcher.dispatchToAgent(context.Background(), sub, awNotification(t, s, id), sender.ID, sender.Slug)
		srv.waitAutoWakeIdle()
		require.Len(t, disp.getStartCalls(), 1)
		assert.Empty(t, disp.getMessageCalls())
		n := awNotification(t, s, id)
		assert.False(t, n.Dispatched, "the notification stays parked")
		assert.Zero(t, n.DispatchAttempts)
		assert.Equal(t, string(state.PhaseSuspended), awAgentPhase(t, s, target.ID))
		after, err := s.GetAgent(context.Background(), target.ID)
		require.NoError(t, err)
		require.Equal(t, "idle", after.SuspendReason, "a failed resume keeps the reason, so the agent stays auto-wake eligible")

		disp.mu2.Lock()
		disp.startReturnErr = nil
		disp.mu2.Unlock()
		awReadyOnStart(t, s, target.ID)
		srv.notificationDispatchSweepHandler()(context.Background())
		srv.waitAutoWakeIdle()

		assert.Len(t, disp.getStartCalls(), 2, "the sweep wakes it again")
		require.Len(t, disp.getMessageCalls(), 1)
		assert.True(t, awNotification(t, s, id).Dispatched)
	})

	t.Run("user-suspended stays parked; the sweep wakes nothing", func(t *testing.T) {
		srv, s, sender, target, disp := autoWakeFixture(t, state.SuspendReasonUser)
		sub, id := notificationAutoWakeFixture(t, srv, s, sender, target)
		srv.notificationDispatcher.dispatchToAgent(context.Background(), sub, awNotification(t, s, id), sender.ID, sender.Slug)
		srv.notificationDispatchSweepHandler()(context.Background())
		srv.waitAutoWakeIdle()
		assert.Empty(t, disp.getStartCalls())
		assert.False(t, awNotification(t, s, id).Dispatched)
	})

	t.Run("experiment off: no wake from dispatch or sweep", func(t *testing.T) {
		srv, s, sender, target, disp := autoWakeFixture(t, state.SuspendReasonIdle)
		setIdleSuspendExperiment(t, srv, false)
		sub, id := notificationAutoWakeFixture(t, srv, s, sender, target)
		srv.notificationDispatcher.dispatchToAgent(context.Background(), sub, awNotification(t, s, id), sender.ID, sender.Slug)
		srv.notificationDispatchSweepHandler()(context.Background())
		srv.waitAutoWakeIdle()
		assert.Empty(t, disp.getStartCalls())
		assert.False(t, awNotification(t, s, id).Dispatched)
	})

	t.Run("a held subscriber is never woken", func(t *testing.T) {
		srv, s, sender, target, disp := autoWakeFixture(t, state.SuspendReasonIdle)
		holdAgentFor(t, s, target.ID, target.ProjectID, target.Ancestry[0])
		sub, id := notificationAutoWakeFixture(t, srv, s, sender, target)
		srv.notificationDispatcher.dispatchToAgent(context.Background(), sub, awNotification(t, s, id), sender.ID, sender.Slug)
		srv.notificationDispatchSweepHandler()(context.Background())
		srv.waitAutoWakeIdle()
		assert.Empty(t, disp.getStartCalls())
	})
}

// A hub that is shutting down starts no background wake: the item is left
// to the caller's non-wake handling.
func TestAutoWake_RefusedAfterShutdown(t *testing.T) {
	srv, _, _, target, disp := autoWakeFixture(t, state.SuspendReasonIdle)
	srv.stopAutoWakes(context.Background())
	ran := false
	ok := srv.queueAutoWake(target, autoWakeItem{
		deliver: func(context.Context, *store.Agent) { ran = true },
		fail:    func(context.Context, *AgentDMError) { ran = true },
	})
	assert.False(t, ok)
	assert.False(t, ran)
	assert.Empty(t, disp.getStartCalls())
}

// --- Web chat (chat v2): primary (sync) and secondaries (async) -------------

func TestAutoWake_ChatPrimary(t *testing.T) {
	setup := func(t *testing.T, reason state.SuspendReason) *chatWakeFixture {
		f := chatWakeSetup(t, string(state.PhaseRunning))
		setIdleSuspendExperiment(t, f.srv, true)
		awSuspendFor(t, f.s, f.agent, reason)
		t.Cleanup(f.srv.waitAutoWakeIdle)
		return f
	}

	t.Run("idle-suspended primary is woken without wake, for a member without lifecycle", func(t *testing.T) {
		f := setup(t, state.SuspendReasonIdle)
		member := f.memberWithoutLifecycle(t)
		awReadyOnStart(t, f.s, f.agent.ID)
		rec := doRequestAsUser(t, f.srv, member, http.MethodPost, f.path(), map[string]any{"content": "hello"})
		require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())
		assert.Equal(t, "dispatched", decodeWakeResp(t, rec)["dispatchState"])
		require.Len(t, f.disp.getStartCalls(), 1)
		assert.Len(t, f.disp.getMessageCalls(), 1)
	})

	t.Run("user-suspended primary keeps today's failed row", func(t *testing.T) {
		f := setup(t, state.SuspendReasonUser)
		rec := doRequest(t, f.srv, http.MethodPost, f.path(), map[string]any{"content": "hello"})
		require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())
		assert.Equal(t, "failed", decodeWakeResp(t, rec)["dispatchState"])
		assert.Empty(t, f.disp.getStartCalls())
	})

	t.Run("a held primary keeps today's failed row, never woken", func(t *testing.T) {
		f := setup(t, state.SuspendReasonIdle)
		holdAgentFor(t, f.s, f.agent.ID, f.proj.ID, DevUserID)
		rec := doRequest(t, f.srv, http.MethodPost, f.path(), map[string]any{"content": "hello"})
		require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())
		assert.Equal(t, "failed", decodeWakeResp(t, rec)["dispatchState"])
		assert.Empty(t, f.disp.getStartCalls())
	})
}

func TestAutoWake_ChatSecondary(t *testing.T) {
	setup := func(t *testing.T, reason state.SuspendReason) (*chatWakeFixture, *store.Agent) {
		f := chatWakeSetup(t, string(state.PhaseRunning))
		setIdleSuspendExperiment(t, f.srv, true)
		second := &store.Agent{
			ID: tid("aw-chat-second"), ProjectID: f.proj.ID, Name: "Second",
			Slug: "aw-chat-second", Phase: string(state.PhaseRunning),
			RuntimeBrokerID: f.agent.RuntimeBrokerID, OwnerID: DevUserID, CreatedBy: DevUserID,
			MessageMode: store.MessageModeProject,
		}
		require.NoError(t, f.s.CreateAgent(t.Context(), second))
		awSuspendFor(t, f.s, second, reason)
		t.Cleanup(f.srv.waitAutoWakeIdle)
		return f, second
	}
	mentionRow := func(t *testing.T, f *chatWakeFixture, agentID string) *store.Message {
		t.Helper()
		res, err := f.s.ListMessages(t.Context(), store.MessageFilter{ThreadID: f.topic}, store.ListOptions{Limit: 100})
		require.NoError(t, err)
		for i := range res.Items {
			if res.Items[i].RecipientID == agentID && res.Items[i].Type == messages.TypeMention {
				return &res.Items[i]
			}
		}
		t.Fatalf("no mention row for %s", agentID)
		return nil
	}

	t.Run("idle-suspended secondary is woken in the background and gets its copy", func(t *testing.T) {
		f, second := setup(t, state.SuspendReasonIdle)
		awReadyOnStart(t, f.s, second.ID)
		rec := doRequest(t, f.srv, http.MethodPost, f.path(), map[string]any{"content": "hi @aw-chat-second"})
		require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())
		f.srv.waitAutoWakeIdle()

		starts := f.disp.getStartCalls()
		require.Len(t, starts, 1)
		assert.Equal(t, second.ID, starts[0].AgentID)
		var toSecond int
		for _, m := range f.disp.getMessageCalls() {
			if m.AgentID == second.ID {
				toSecond++
			}
		}
		assert.Equal(t, 1, toSecond, "the secondary's copy is delivered after the wake")
		assert.Equal(t, store.MessageDispatchDispatched, mentionRow(t, f, second.ID).DispatchState)
	})

	t.Run("a failed wake marks the secondary's copy failed", func(t *testing.T) {
		f, second := setup(t, state.SuspendReasonIdle)
		f.disp.startReturnErr = assert.AnError
		rec := doRequest(t, f.srv, http.MethodPost, f.path(), map[string]any{"content": "hi @aw-chat-second"})
		require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())
		f.srv.waitAutoWakeIdle()
		row := mentionRow(t, f, second.ID)
		assert.Equal(t, store.MessageDispatchFailed, row.DispatchState)
		require.NotNil(t, row.DispatchFailureReason)
		assert.Contains(t, *row.DispatchFailureReason, "auto-wake failed")
	})

	t.Run("user-suspended secondary takes today's buffered dispatch, no wake", func(t *testing.T) {
		f, second := setup(t, state.SuspendReasonUser)
		rec := doRequest(t, f.srv, http.MethodPost, f.path(), map[string]any{"content": "hi @aw-chat-second"})
		require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())
		f.srv.waitAutoWakeIdle()
		assert.Empty(t, f.disp.getStartCalls())
		var toSecond int
		for _, m := range f.disp.getMessageCalls() {
			if m.AgentID == second.ID {
				toSecond++
			}
		}
		assert.Equal(t, 1, toSecond)
	})
}
