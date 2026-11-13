//go:build !no_sqlite

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

package hub

// Tests for opt-in wake on chat scheduled sends (ptone/scion#7755).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type scheduledWakeFixture struct {
	*scheduledSendFixture
	broker *store.RuntimeBroker
	disp   *wakeTrackingDispatcher
}

func newScheduledWakeFixture(t *testing.T, phase string) *scheduledWakeFixture {
	t.Helper()
	f := newScheduledSendFixture(t)
	return attachScheduledWakeBroker(t, f, phase)
}

func attachScheduledWakeBroker(t *testing.T, f *scheduledSendFixture, phase string) *scheduledWakeFixture {
	t.Helper()
	ctx := context.Background()

	disp := &wakeTrackingDispatcher{}
	f.srv.SetDispatcher(disp)

	broker := &store.RuntimeBroker{
		ID:     tid("sched-wake-broker"),
		Name:   "Sched Wake Broker",
		Slug:   "sched-wake-broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, f.store.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, f.store.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  f.project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}))

	f.agent.RuntimeBrokerID = broker.ID
	f.agent.Phase = phase
	f.agent.MessageMode = store.MessageModeProject
	require.NoError(t, f.store.UpdateAgent(ctx, f.agent))

	return &scheduledWakeFixture{
		scheduledSendFixture: f,
		broker:               broker,
		disp:                 disp,
	}
}

func (wf *scheduledWakeFixture) markReadySoon(agentID string) {
	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = wf.store.UpdateAgentStatus(context.Background(), agentID, store.AgentStatusUpdate{
			Phase:    string(state.PhaseRunning),
			Activity: "idle",
		})
	}()
}

func (wf *scheduledWakeFixture) scheduleWakeIn(t *testing.T, user *store.User, key, content string, wake bool) scheduledMessageResponse {
	t.Helper()
	rec := doRequestAsUser(t, wf.srv, user, http.MethodPost, scheduledConversationPath(key), map[string]any{
		"content":         content,
		"fire_at":         time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"idempotency_key": "idem-" + content,
		"wake":            wake,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var resp scheduledMessageResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	_, err := wf.db.ExecContext(context.Background(), `UPDATE webchat_scheduled_message SET fire_at = ? WHERE id = ?`,
		sqliteScheduledTime(time.Now().UTC().Truncate(time.Second)), resp.ID)
	require.NoError(t, err)
	return resp
}

// Create with wake=true on a topic and an agent DM stores and echoes wake in
// both the HTTP response and SSE event; dm:user:* returns 400.
func TestScheduledSendWake_CreateAndValidation(t *testing.T) {
	wf := newScheduledWakeFixture(t, string(state.PhaseSuspended))
	events, unsub := wf.srv.events.Subscribe("user." + wf.bob.ID + ".chat.scheduled")
	defer unsub()

	// Topic with wake=true.
	topicResp := wf.scheduleWakeIn(t, wf.bob, wf.topicID, "wake topic", true)
	assert.True(t, topicResp.Wake)
	assert.True(t, wf.row(t, wf.bob, topicResp.ID).Wake)

	// Agent DM with wake=true.
	agentDMKey, err := messages.DMConversationKey("agent", wf.agent.ID, "user", wf.bob.ID)
	require.NoError(t, err)
	dmResp := wf.scheduleWakeIn(t, wf.bob, agentDMKey, "wake agent dm", true)
	assert.True(t, dmResp.Wake)
	assert.True(t, wf.row(t, wf.bob, dmResp.ID).Wake)

	// Topic without wake defaults to false.
	plainResp := wf.schedule(t, wf.bob, "plain topic", time.Now().Add(time.Hour))
	assert.False(t, plainResp.Wake)
	assert.False(t, wf.row(t, wf.bob, plainResp.ID).Wake)

	// SSE payloads carry wake.
	evts := collectEvents(events)
	require.Len(t, evts, 3)
	var sse0, sse2 ChatScheduledEvent
	require.NoError(t, json.Unmarshal(evts[0].Data, &sse0))
	require.NoError(t, json.Unmarshal(evts[2].Data, &sse2))
	assert.True(t, sse0.ScheduledMessage.Wake)
	assert.False(t, sse2.ScheduledMessage.Wake)

	// User-to-user DM with wake=true is rejected with 400.
	userDMKey, err := messages.DMConversationKey("user", wf.alice.ID, "user", wf.bob.ID)
	require.NoError(t, err)
	rec := doRequestAsUser(t, wf.srv, wf.bob, http.MethodPost, scheduledConversationPath(userDMKey), map[string]any{
		"content":         "wake user dm",
		"fire_at":         time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"idempotency_key": "idem-user-dm-wake",
		"wake":            true,
	})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "wake requires an agent conversation")

	// User-to-user DM with wake=false remains accepted.
	rec = doRequestAsUser(t, wf.srv, wf.bob, http.MethodPost, scheduledConversationPath(userDMKey), map[string]any{
		"content":         "no-wake user dm",
		"fire_at":         time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"idempotency_key": "idem-user-dm-nowake",
		"wake":            false,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
}

// Fire with wake=true and a suspended primary that the sender may wake resumes
// the agent once, marks the scheduled row sent, persists a dispatched message,
// and emits wake=true in audit records.
func TestScheduledSendWake_SuspendedPrimary_WakesAndDelivers(t *testing.T) {
	wf := newScheduledWakeFixture(t, string(state.PhaseSuspended))
	ctx := context.Background()
	auditor := &recordingScheduledAuditor{}
	wf.srv.SetAuditLogger(auditor)
	logs := captureSlogDefault(t)

	sm := wf.scheduleWakeIn(t, wf.bob, wf.topicID, "good morning agent", true)
	wf.markReadySoon(wf.agent.ID)

	assert.Equal(t, 1, wf.srv.sweepScheduledMessages(ctx, time.Now().UTC()))

	row := wf.row(t, wf.bob, sm.ID)
	assert.Equal(t, ScheduledMessageSent, row.Status)
	assert.NotEmpty(t, row.MessageID)

	starts := wf.disp.getStartCalls()
	require.Len(t, starts, 1, "wakeAgentForDM should resume the primary once")
	assert.Equal(t, wf.agent.ID, starts[0].AgentID)
	assert.True(t, starts[0].Continue)

	msgs := wf.topicMessages(t)
	require.Len(t, msgs, 1)
	assert.Equal(t, store.MessageDispatchDispatched, msgs[0].DispatchState)
	assert.Nil(t, msgs[0].DispatchFailureReason)

	auditor.mu.Lock()
	require.Len(t, auditor.events, 2)
	assert.Equal(t, ScheduledAuditCreate, auditor.events[0].Action)
	assert.True(t, auditor.events[0].Wake)
	assert.Equal(t, ScheduledAuditFire, auditor.events[1].Action)
	assert.Equal(t, ScheduledMessageSent, auditor.events[1].Status)
	assert.True(t, auditor.events[1].Wake)
	ev1 := auditor.events[1]
	attrs := chatScheduledAuditAttrs(&ev1)
	auditor.mu.Unlock()
	_ = logs
	hasWakeAttr := false
	for _, a := range attrs {
		if a.Key == "wake" && a.Value.Bool() {
			hasWakeAttr = true
		}
	}
	assert.True(t, hasWakeAttr, "fire audit attrs must include wake=true")
}

// Fire with wake=true wakes only the primary agent recipient; a suspended
// secondary @-mentioned agent is never woken.
func TestScheduledSendWake_SecondaryMentionNeverWoken(t *testing.T) {
	wf := newScheduledWakeFixture(t, string(state.PhaseSuspended))
	ctx := context.Background()

	secondary := &store.Agent{
		ID:              tid("sched-secondary"),
		ProjectID:       wf.project.ID,
		Name:            "secondary-agent",
		Slug:            "secondary-agent",
		Phase:           string(state.PhaseSuspended),
		RuntimeBrokerID: wf.broker.ID,
		OwnerID:         wf.bob.ID,
		CreatedBy:       wf.bob.ID,
		MessageMode:     store.MessageModeProject,
	}
	require.NoError(t, wf.store.CreateAgent(ctx, secondary))

	sm := wf.scheduleWakeIn(t, wf.bob, wf.topicID, "hello and cc @secondary-agent", true)
	wf.markReadySoon(wf.agent.ID)

	assert.Equal(t, 1, wf.srv.sweepScheduledMessages(ctx, time.Now().UTC()))
	assert.Equal(t, ScheduledMessageSent, wf.row(t, wf.bob, sm.ID).Status)

	starts := wf.disp.getStartCalls()
	require.Len(t, starts, 1, "only the primary agent may be woken")
	assert.Equal(t, wf.agent.ID, starts[0].AgentID)

	secAfter, err := wf.store.GetAgent(ctx, secondary.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseSuspended), secAfter.Phase)
}

// Fire with wake=true when the sender lacks agent.lifecycle fails the row with
// wake_refused, posts no message, calls no start, and refuses send-now.
func TestScheduledSendWake_WithoutLifecyclePermission_Refused(t *testing.T) {
	wf := newScheduledWakeFixture(t, string(state.PhaseSuspended))
	ctx := context.Background()

	// Transfer agent ownership to alice so bob (ordinary project member) can
	// message the agent in the project topic but lacks agent.lifecycle.
	wf.agent.OwnerID = wf.alice.ID
	wf.agent.CreatedBy = wf.alice.ID
	require.NoError(t, wf.store.UpdateAgent(ctx, wf.agent))
	msgAuthzGrantAgentMessage(t, wf.store, wf.bob.ID, wf.project.ID)
	require.False(t, wf.srv.agentLifecycleAllowed(ctx, userIdentityFor(wf.bob), wf.agent))

	sm := wf.scheduleWakeIn(t, wf.bob, wf.topicID, "try wake without lifecycle", true)
	assert.Equal(t, 1, wf.srv.sweepScheduledMessages(ctx, time.Now().UTC()))

	row := wf.row(t, wf.bob, sm.ID)
	assert.Equal(t, ScheduledMessageFailed, row.Status)
	assert.Equal(t, ScheduledFailureWakeRefused, row.FailureReason)
	assert.Empty(t, wf.topicMessages(t), "refused wake must persist no chat message")
	assert.Empty(t, wf.disp.getStartCalls(), "refused wake must not start the agent")

	rec := doRequestAsUser(t, wf.srv, wf.bob, http.MethodPost, wf.rowPath(sm.ID, "/send-now"), nil)
	assert.Equal(t, http.StatusConflict, rec.Code, "wake_refused must not allow send-now")
}

// Fire with wake=true when the primary agent is held fails with wake_refused,
// posts nothing, and does not call start.
func TestScheduledSendWake_HeldPrimary_Refused(t *testing.T) {
	wf := newScheduledWakeFixture(t, string(state.PhaseSuspended))
	ctx := context.Background()

	_, err := wf.store.CreateAgentHolds(ctx, []*store.AgentHold{{
		AgentID:           wf.agent.ID,
		ProjectID:         wf.project.ID,
		Cause:             store.AgentHoldCauseOwnerAccessEnded,
		RootPrincipalType: store.AgentHoldRootUser,
		RootPrincipalID:   wf.bob.ID,
		Trigger:           store.MembershipLossTriggerMemberRemove,
		ActorKind:         "system",
		ActorID:           "hub",
		CorrelationID:     "test-7755",
	}})
	require.NoError(t, err)

	sm := wf.scheduleWakeIn(t, wf.bob, wf.topicID, "wake held primary", true)
	assert.Equal(t, 1, wf.srv.sweepScheduledMessages(ctx, time.Now().UTC()))

	row := wf.row(t, wf.bob, sm.ID)
	assert.Equal(t, ScheduledMessageFailed, row.Status)
	assert.Equal(t, ScheduledFailureWakeRefused, row.FailureReason)
	assert.Empty(t, wf.topicMessages(t))
	assert.Empty(t, wf.disp.getStartCalls())

	rec := doRequestAsUser(t, wf.srv, wf.bob, http.MethodPost, wf.rowPath(sm.ID, "/send-now"), nil)
	assert.Equal(t, http.StatusConflict, rec.Code)
}

// Wake dispatch error or readiness timeout fails with wake_failed, posts
// nothing, and allows send-now (preserving wake=true so retry wakes and sends).
func TestScheduledSendWake_DispatchErrorAndTimeout_FailedThenSendNow(t *testing.T) {
	t.Run("dispatchError", func(t *testing.T) {
		wf := newScheduledWakeFixture(t, string(state.PhaseSuspended))
		ctx := context.Background()
		wf.disp.startReturnErr = fmt.Errorf("broker refused start: %w", errStartRunNotBegun)

		sm := wf.scheduleWakeIn(t, wf.bob, wf.topicID, "wake broker down", true)
		assert.Equal(t, 1, wf.srv.sweepScheduledMessages(ctx, time.Now().UTC()))

		row := wf.row(t, wf.bob, sm.ID)
		assert.Equal(t, ScheduledMessageFailed, row.Status)
		assert.Equal(t, ScheduledFailureWakeFailed, row.FailureReason)
		assert.Empty(t, wf.topicMessages(t))

		// Send now resets to pending and keeps wake=true.
		wf.disp.startReturnErr = nil
		rec := doRequestAsUser(t, wf.srv, wf.bob, http.MethodPost, wf.rowPath(sm.ID, "/send-now"), nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var sendNowResp scheduledMessageResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &sendNowResp))
		assert.Equal(t, ScheduledMessagePending, sendNowResp.Status)
		assert.True(t, sendNowResp.Wake)

		time.Sleep(time.Until(sendNowResp.FireAt))
		wf.markReadySoon(wf.agent.ID)
		assert.Equal(t, 1, wf.srv.sweepScheduledMessages(ctx, time.Now().UTC()))
		assert.Equal(t, ScheduledMessageSent, wf.row(t, wf.bob, sm.ID).Status)
		require.Len(t, wf.topicMessages(t), 1)
	})

	t.Run("readinessTimeout", func(t *testing.T) {
		saved := wakeReadyTimeout
		wakeReadyTimeout = 150 * time.Millisecond
		t.Cleanup(func() { wakeReadyTimeout = saved })

		wf := newScheduledWakeFixture(t, string(state.PhaseSuspended))
		ctx := context.Background()

		sm := wf.scheduleWakeIn(t, wf.bob, wf.topicID, "wake slow harness", true)
		assert.Equal(t, 1, wf.srv.sweepScheduledMessages(ctx, time.Now().UTC()))

		row := wf.row(t, wf.bob, sm.ID)
		assert.Equal(t, ScheduledMessageFailed, row.Status)
		assert.Equal(t, ScheduledFailureWakeFailed, row.FailureReason)
		assert.Empty(t, wf.topicMessages(t))
	})

	t.Run("phaseChanged409", func(t *testing.T) {
		srv, s, alice, bob, project, wrapped, _ := setupDemoPolicyTestWithFault(t,
			func(inner store.Store, _ *storeFaultSwitch) *phaseChangedDuringStartGateStore {
				return &phaseChangedDuringStartGateStore{Store: inner}
			})
		f := newScheduledSendFixtureOn(t, srv, s, alice, bob, project)
		wf := attachScheduledWakeBroker(t, f, string(state.PhaseSuspended))
		ctx := context.Background()

		sm := wf.scheduleWakeIn(t, wf.bob, wf.topicID, "wake phase changed 409", true)
		assert.Equal(t, 1, wf.srv.sweepScheduledMessages(ctx, time.Now().UTC()))
		assert.True(t, wrapped.fired.Load(), "phase-change hook ran inside startGate")

		row := wf.row(t, wf.bob, sm.ID)
		assert.Equal(t, ScheduledMessageFailed, row.Status)
		assert.Equal(t, ScheduledFailureWakeFailed, row.FailureReason, "phase-changed 409 must map to wake_failed, not wake_refused")
		assert.Empty(t, wf.topicMessages(t))
		assert.Empty(t, wf.disp.getStartCalls())

		// Send now is allowed for wake_failed and delivers now that primary is running.
		rec := doRequestAsUser(t, wf.srv, wf.bob, http.MethodPost, wf.rowPath(sm.ID, "/send-now"), nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var sendNowResp scheduledMessageResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &sendNowResp))
		time.Sleep(time.Until(sendNowResp.FireAt))
		assert.Equal(t, 1, wf.srv.sweepScheduledMessages(ctx, time.Now().UTC()))
		assert.Equal(t, ScheduledMessageSent, wf.row(t, wf.bob, sm.ID).Status)
		require.Len(t, wf.topicMessages(t), 1)
	})
}

type phaseChangedDuringStartGateStore struct {
	store.Store
	fired atomic.Bool
}

func (w *phaseChangedDuringStartGateStore) HasOutstandingBrokerDispatch(ctx context.Context, agentID, op string) (bool, error) {
	exec, _ := ExecutorContextFromContext(ctx)
	if exec.Kind == scheduledSendClientType && w.fired.CompareAndSwap(false, true) {
		if err := w.UpdateAgentStatus(ctx, agentID, store.AgentStatusUpdate{
			Phase:   string(state.PhaseRunning),
			Restore: true,
		}); err != nil {
			return false, err
		}
	}
	return w.Store.HasOutstandingBrokerDispatch(ctx, agentID, op)
}

// Wake on a stopped primary, a running primary, or without wake on a suspended
// primary preserves existing behavior without calling start.
func TestScheduledSendWake_PhaseMatrixAndRegression(t *testing.T) {
	t.Run("stoppedPrimaryWithWake", func(t *testing.T) {
		wf := newScheduledWakeFixture(t, string(state.PhaseStopped))
		sm := wf.scheduleWakeIn(t, wf.bob, wf.topicID, "wake stopped", true)
		assert.Equal(t, 1, wf.srv.sweepScheduledMessages(context.Background(), time.Now().UTC()))
		assert.Equal(t, ScheduledMessageSent, wf.row(t, wf.bob, sm.ID).Status)
		assert.Empty(t, wf.disp.getStartCalls())
		msgs := wf.topicMessages(t)
		require.Len(t, msgs, 1)
		assert.Equal(t, store.MessageDispatchFailed, msgs[0].DispatchState)
		require.NotNil(t, msgs[0].DispatchFailureReason)
		assert.Contains(t, *msgs[0].DispatchFailureReason, "Agent unreachable (stopped)")
	})

	t.Run("runningPrimaryWithWake", func(t *testing.T) {
		wf := newScheduledWakeFixture(t, string(state.PhaseRunning))
		sm := wf.scheduleWakeIn(t, wf.bob, wf.topicID, "wake running", true)
		assert.Equal(t, 1, wf.srv.sweepScheduledMessages(context.Background(), time.Now().UTC()))
		assert.Equal(t, ScheduledMessageSent, wf.row(t, wf.bob, sm.ID).Status)
		assert.Empty(t, wf.disp.getStartCalls())
		msgs := wf.topicMessages(t)
		require.Len(t, msgs, 1)
		assert.Equal(t, store.MessageDispatchDispatched, msgs[0].DispatchState)
	})

	t.Run("suspendedPrimaryWithoutWake", func(t *testing.T) {
		wf := newScheduledWakeFixture(t, string(state.PhaseSuspended))
		sm := wf.scheduleWakeIn(t, wf.bob, wf.topicID, "no wake suspended", false)
		assert.Equal(t, 1, wf.srv.sweepScheduledMessages(context.Background(), time.Now().UTC()))
		assert.Equal(t, ScheduledMessageSent, wf.row(t, wf.bob, sm.ID).Status)
		assert.Empty(t, wf.disp.getStartCalls())
		msgs := wf.topicMessages(t)
		require.Len(t, msgs, 1)
		assert.Equal(t, store.MessageDispatchFailed, msgs[0].DispatchState)
		require.NotNil(t, msgs[0].DispatchFailureReason)
		assert.Contains(t, *msgs[0].DispatchFailureReason, "Agent unreachable (suspended)")
	})
}

type abortDuringStartGateStore struct {
	store.Store
	srv   atomic.Pointer[Server]
	fired atomic.Bool
}

func (w *abortDuringStartGateStore) HasOutstandingBrokerDispatch(ctx context.Context, agentID, op string) (bool, error) {
	exec, _ := ExecutorContextFromContext(ctx)
	if exec.Kind == scheduledSendClientType && w.fired.CompareAndSwap(false, true) {
		w.srv.Load().scheduledRuntime().abort()
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
			return false, errors.New("scheduled wake context was detached from sweeper abort")
		}
		return false, ctx.Err()
	}
	return w.Store.HasOutstandingBrokerDispatch(ctx, agentID, op)
}

// Shutdown during wake (sweeper aborted): the wake context is NOT detached from
// rt.onAbort, and the row is released back to pending instead of failing.
func TestScheduledSendWake_ShutdownDuringWake_ReleasedToPending(t *testing.T) {
	srv, s, alice, bob, project, wrapped, _ := setupDemoPolicyTestWithFault(t,
		func(inner store.Store, _ *storeFaultSwitch) *abortDuringStartGateStore {
			return &abortDuringStartGateStore{Store: inner}
		})
	wrapped.srv.Store(srv)

	f := newScheduledSendFixtureOn(t, srv, s, alice, bob, project)
	wf := attachScheduledWakeBroker(t, f, string(state.PhaseSuspended))

	sm := wf.scheduleWakeIn(t, wf.bob, wf.topicID, "wake interrupted by shutdown", true)
	events, unsub := wf.srv.events.Subscribe("user." + wf.bob.ID + ".chat.scheduled")
	defer unsub()
	assert.Equal(t, 1, wf.srv.sweepScheduledMessages(context.Background(), time.Now().UTC()))
	assert.True(t, wrapped.fired.Load(), "abort hook ran inside wakeAgentForDM startGate")

	row := wf.row(t, wf.bob, sm.ID)
	assert.Equal(t, ScheduledMessagePending, row.Status, "cut-short wake must release to pending, not fail")
	assert.Empty(t, row.FailureReason)
	assert.Empty(t, wf.topicMessages(t))
	assert.Empty(t, wf.disp.getStartCalls())
	assert.Equal(t, []string{"sending", "released"}, scheduledEventActions(t, collectEvents(events)))
}

type abortDuringResumeStore struct {
	store.Store
	aborted      atomic.Bool
	createFailed atomic.Bool
}

func (w *abortDuringResumeStore) CreateMessage(ctx context.Context, msg *store.Message) error {
	exec, _ := ExecutorContextFromContext(ctx)
	if exec.Kind == scheduledSendClientType && w.aborted.Load() {
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
			return errors.New("scheduled delivery context was not cancelled by sweeper abort")
		}
		err := w.Store.CreateMessage(ctx, msg)
		if err != nil {
			w.createFailed.Store(true)
		}
		return err
	}
	return w.Store.CreateMessage(ctx, msg)
}

type abortDuringResumeDispatcher struct {
	*wakeTrackingDispatcher
	srv       *Server
	store     *abortDuringResumeStore
	markReady func(agentID string)
}

func (d *abortDuringResumeDispatcher) DispatchAgentStart(ctx context.Context, agent *store.Agent, prompt string, cont bool) error {
	d.store.aborted.Store(true)
	d.srv.scheduledRuntime().abort()
	d.markReady(agent.ID)
	return d.wakeTrackingDispatcher.DispatchAgentStart(ctx, agent, prompt, cont)
}

// Shutdown abort during an in-flight resume: the detached resume completes and
// moves the agent to running, CreateMessage fails on the cancelled delivery
// context, and the row is released back to pending (not failed/delivery_error).
func TestScheduledSendWake_ShutdownDuringResume_ReleasedToPending(t *testing.T) {
	srv, s, alice, bob, project, wrapped, _ := setupDemoPolicyTestWithFault(t,
		func(inner store.Store, _ *storeFaultSwitch) *abortDuringResumeStore {
			return &abortDuringResumeStore{Store: inner}
		})

	f := newScheduledSendFixtureOn(t, srv, s, alice, bob, project)
	wf := attachScheduledWakeBroker(t, f, string(state.PhaseSuspended))
	wf.srv.SetDispatcher(&abortDuringResumeDispatcher{
		wakeTrackingDispatcher: wf.disp,
		srv:                    wf.srv,
		store:                  wrapped,
		markReady:              wf.markReadySoon,
	})

	sm := wf.scheduleWakeIn(t, wf.bob, wf.topicID, "wake succeeds after sweeper abort", true)
	events, unsub := wf.srv.events.Subscribe("user." + wf.bob.ID + ".chat.scheduled")
	defer unsub()
	assert.Equal(t, 1, wf.srv.sweepScheduledMessages(context.Background(), time.Now().UTC()))

	assert.True(t, wrapped.aborted.Load(), "sweeper aborted during resume dispatch")
	assert.True(t, wrapped.createFailed.Load(), "CreateMessage failed on cancelled delivery context")
	assert.Len(t, wf.disp.getStartCalls(), 1, "detached resume completed")
	gotAgent, err := wf.store.GetAgent(context.Background(), wf.agent.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseRunning), gotAgent.Phase)

	row := wf.row(t, wf.bob, sm.ID)
	assert.Equal(t, ScheduledMessagePending, row.Status, "pre-persistence failure after cut-short wake must release to pending")
	assert.Empty(t, row.FailureReason)
	assert.Empty(t, wf.topicMessages(t))
	assert.Equal(t, []string{"sending", "released"}, scheduledEventActions(t, collectEvents(events)))

	// Next sweep (fresh runtime) delivers to the now-running agent without a second start.
	wrapped.aborted.Store(false)
	wf.srv.mu.Lock()
	wf.srv.scheduledSend = nil
	wf.srv.mu.Unlock()
	assert.Equal(t, 1, wf.srv.sweepScheduledMessages(context.Background(), time.Now().UTC()))
	assert.Equal(t, ScheduledMessageSent, wf.row(t, wf.bob, sm.ID).Status)
	assert.Len(t, wf.disp.getStartCalls(), 1, "already-running agent needs no second resume")
	require.Len(t, wf.topicMessages(t), 1)
}

// scheduledStuckAfter() exceeds chatWakeResumeBudget + claim + delivery + finalize.
func TestScheduledSendWake_BudgetCoversWakeResume(t *testing.T) {
	delivery := time.Duration(1+messages.MaxMentionRecipients)*chatWakeDeliveryBudget + 30*time.Second
	assert.GreaterOrEqual(t, scheduledDeliveryBudget, chatWakeResumeBudget+delivery)
	assert.Greater(t, scheduledStuckAfter(),
		chatWakeResumeBudget+scheduledClaimTimeout+delivery+scheduledFinalizeTimeout)
}

// WakeDeferred (Q4a): when another start holds the agent's start claim, both
// live chat (retryable 503) and scheduled chat (wake_failed, nothing posted)
// fail cleanly without persisting a message row.
func TestScheduledSendWake_WakeDeferred_LiveAndScheduled(t *testing.T) {
	wf := newScheduledWakeFixture(t, string(state.PhaseSuspended))
	ctx := context.Background()

	_, err := wf.store.ClaimAgentStart(ctx, wf.agent.ID, "other-hub", store.StartClaimUser, "", time.Minute)
	require.NoError(t, err)

	// Live chat with wake=true gets a retryable 503 and persists nothing.
	liveRec := doRequestAsUser(t, wf.srv, wf.bob, http.MethodPost, "/api/v1/chat/conversations/"+wf.topicID+"/messages", map[string]any{
		"content": "live wake while starting",
		"wake":    true,
	})
	assert.Equal(t, http.StatusServiceUnavailable, liveRec.Code, liveRec.Body.String())
	assert.NotEmpty(t, liveRec.Header().Get("Retry-After"), "503 must carry Retry-After")
	assert.Empty(t, wf.topicMessages(t))

	// Scheduled chat with wake=true fails the row with wake_failed and persists nothing.
	sm := wf.scheduleWakeIn(t, wf.bob, wf.topicID, "scheduled wake while starting", true)
	assert.Equal(t, 1, wf.srv.sweepScheduledMessages(ctx, time.Now().UTC()))

	row := wf.row(t, wf.bob, sm.ID)
	assert.Equal(t, ScheduledMessageFailed, row.Status)
	assert.Equal(t, ScheduledFailureWakeFailed, row.FailureReason)
	assert.Empty(t, wf.topicMessages(t))
}

// Migration: an existing webchat_scheduled_message table without the wake
// column gains wake=false on Init() and records scheduled_message_wake_column.
func TestScheduledStore_SQLite_WakeColumnMigration(t *testing.T) {
	path := t.TempDir() + "/legacy_webchat.db"
	dsn := "file:" + path + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_timezone=UTC"
	db, err := sql.Open("sqlite", dsn)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	// Seed the pre-#7755 table schema without the wake column.
	_, err = db.Exec(`
CREATE TABLE webchat_migrations (
    name TEXT PRIMARY KEY,
    completed_at TEXT NOT NULL
);
INSERT INTO webchat_migrations (name, completed_at) VALUES ('scheduled_message_table', '2026-01-01T00:00:00Z');
CREATE TABLE webchat_scheduled_message (
    id               TEXT PRIMARY KEY,
    sender_user_id   TEXT NOT NULL,
    conversation_key TEXT NOT NULL,
    project_id       TEXT,
    content          TEXT NOT NULL,
    reply_to_id      TEXT,
    idempotency_key  TEXT NOT NULL,
    fire_at          TEXT NOT NULL,
    status           TEXT NOT NULL,
    failure_reason   TEXT,
    message_id       TEXT,
    created_at       TEXT NOT NULL,
    updated_at       TEXT NOT NULL,
    claimed_at       TEXT
);
INSERT INTO webchat_scheduled_message
    (id, sender_user_id, conversation_key, content, idempotency_key, fire_at, status, created_at, updated_at)
VALUES
    ('legacy-row', 'u-1', 'topic-1', 'hello legacy', 'idem-legacy', '2026-01-02T00:00:00Z', 'pending', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');
`)
	require.NoError(t, err)

	wcs := NewWebChatStore(db, "sqlite")
	require.NoError(t, wcs.Init())
	sms := scheduledMessageStoreFrom(wcs)
	require.NotNil(t, sms)

	var migCount int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM webchat_migrations WHERE name = ?`, scheduledMessageWakeMigration).Scan(&migCount))
	assert.Equal(t, 1, migCount, "migration row must be recorded")

	legacy, err := sms.GetScheduledMessage(context.Background(), "u-1", "legacy-row")
	require.NoError(t, err)
	require.NotNil(t, legacy)
	assert.False(t, legacy.Wake, "existing rows default to wake=false")

	freshIn := newTestScheduledRow("post-mig", "u-1", time.Now().Add(time.Hour))
	freshIn.Wake = true
	freshOut, _, err := sms.CreateScheduledMessage(context.Background(), freshIn)
	require.NoError(t, err)
	assert.True(t, freshOut.Wake)
}

// testScheduledStorePostgresWakeMigration verifies that upgrading an existing
// Postgres webchat_scheduled_message table created before #7755 adds the wake
// column with default false and records scheduled_message_wake_column.
// Called from TestScheduledStore_Postgres (WEBCHAT_POSTGRES_TESTS in CI).
func testScheduledStorePostgresWakeMigration(t *testing.T, dsn string) {
	t.Helper()
	ctx := context.Background()
	schema := fmt.Sprintf("sched_wake_mig_%d", time.Now().UnixNano())
	cfg, err := pgx.ParseConfig(dsn)
	require.NoError(t, err)
	cfg.RuntimeParams["search_path"] = schema

	db := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.ExecContext(ctx, "CREATE SCHEMA "+schema)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = db.Exec("DROP SCHEMA " + schema + " CASCADE") })

	_, err = db.ExecContext(ctx, `
CREATE TABLE messages (
    id uuid PRIMARY KEY, created timestamptz NOT NULL, channel text, thread_id text,
    sender text, sender_id text, recipient text, recipient_id text);
CREATE TABLE webchat_migrations (
    name TEXT PRIMARY KEY,
    completed_at TIMESTAMPTZ NOT NULL
);
INSERT INTO webchat_migrations (name, completed_at) VALUES ('scheduled_message_table', NOW());
CREATE TABLE webchat_scheduled_message (
    id               TEXT PRIMARY KEY,
    sender_user_id   TEXT NOT NULL,
    conversation_key TEXT NOT NULL,
    project_id       TEXT,
    content          TEXT NOT NULL,
    reply_to_id      TEXT,
    idempotency_key  TEXT NOT NULL,
    fire_at          TIMESTAMPTZ NOT NULL,
    status           TEXT NOT NULL,
    failure_reason   TEXT,
    message_id       TEXT,
    created_at       TIMESTAMPTZ NOT NULL,
    updated_at       TIMESTAMPTZ NOT NULL,
    claimed_at       TIMESTAMPTZ
);
INSERT INTO webchat_scheduled_message
    (id, sender_user_id, conversation_key, content, idempotency_key, fire_at, status, created_at, updated_at)
VALUES
    ('legacy-pg-row', 'u-1', 'topic-1', 'hello legacy pg', 'idem-legacy-pg', '2026-01-02T00:00:00Z', 'pending', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');
`)
	require.NoError(t, err)

	wcs := NewWebChatStore(db, "postgres")
	require.NoError(t, wcs.Init())
	sms := scheduledMessageStoreFrom(wcs)
	require.NotNil(t, sms)

	var migCount int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM webchat_migrations WHERE name = $1`, scheduledMessageWakeMigration).Scan(&migCount))
	assert.Equal(t, 1, migCount, "migration row must be recorded")

	legacy, err := sms.GetScheduledMessage(ctx, "u-1", "legacy-pg-row")
	require.NoError(t, err)
	require.NotNil(t, legacy)
	assert.False(t, legacy.Wake, "existing Postgres rows default to wake=false")

	freshIn := newTestScheduledRow("post-mig-pg", "u-1", time.Now().Add(time.Hour))
	freshIn.Wake = true
	freshOut, _, err := sms.CreateScheduledMessage(ctx, freshIn)
	require.NoError(t, err)
	assert.True(t, freshOut.Wake)
}
