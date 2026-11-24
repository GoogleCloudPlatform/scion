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
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
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
	assert.Len(t, disp.getMessageCalls(), n, "every message is delivered after the single resume, none left deferred")
	assertEveryRowDispatched(t, s, target.ID, n)
}

// assertEveryRowDispatched checks that recipientID has exactly n stored
// messages and that every one is dispatched.
func assertEveryRowDispatched(t *testing.T, s store.Store, recipientID string, n int) {
	t.Helper()
	res, err := s.ListMessages(context.Background(), store.MessageFilter{RecipientID: recipientID}, store.ListOptions{Limit: 100})
	require.NoError(t, err)
	require.Len(t, res.Items, n)
	for _, m := range res.Items {
		assert.Equal(t, store.MessageDispatchDispatched, m.DispatchState, "message %s", m.ID)
	}
}

// A user DM racing other user DMs to the same idle-suspended agent waits
// for the one resume and is delivered, instead of being kept deferred.
func TestAutoWake_UserDM_ConcurrentAllDelivered(t *testing.T) {
	srv, s, _, target, disp := autoWakeFixture(t, state.SuspendReasonIdle)
	u := wakeLifecycleUser(t, s, "aw-user-conc", target.ProjectID, "agent.message", "agent.read")
	awReadyOnStart(t, s, target.ID)

	const n = 3
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := requestAsIdentity(t, srv, authUser(u), http.MethodPost, "/api/v1/agents/"+target.ID+"/message",
				map[string]interface{}{"message": "concurrent user"})
			codes[i] = rec.Code
		}(i)
	}
	wg.Wait()
	for i, c := range codes {
		assert.Equal(t, http.StatusOK, c, "send %d", i)
	}
	assert.Len(t, disp.getStartCalls(), 1, "one resume")
	assert.Len(t, disp.getMessageCalls(), n, "every message is delivered")
	assertEveryRowDispatched(t, s, target.ID, n)
}

// awSignalHandler is a slog.Handler that closes fired the first time a
// record with message msg is logged, so a test can act at a known point of
// the code under test instead of relying on goroutine timing.
type awSignalHandler struct {
	msg   string
	once  sync.Once
	fired chan struct{}
}

func newAWSignal(msg string) *awSignalHandler {
	return &awSignalHandler{msg: msg, fired: make(chan struct{})}
}

func (h *awSignalHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *awSignalHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Message == h.msg {
		h.once.Do(func() { close(h.fired) })
	}
	return nil
}
func (h *awSignalHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *awSignalHandler) WithGroup(string) slog.Handler      { return h }

// awStartingFresh moves target to starting, as another wake of the agent
// the hub suspended would (the reason is kept through starting), and
// returns the row as a sender reading it now sees it.
func awStartingFresh(t *testing.T, s store.Store, target *store.Agent) *store.Agent {
	t.Helper()
	require.NoError(t, s.UpdateAgentStatus(context.Background(), target.ID, store.AgentStatusUpdate{Phase: string(state.PhaseStarting)}))
	fresh, err := s.GetAgent(context.Background(), target.ID)
	require.NoError(t, err)
	require.Equal(t, string(state.PhaseStarting), fresh.Phase)
	return fresh
}

func TestAutoWakeModeOf(t *testing.T) {
	srv, _ := testServer(t)
	cases := []struct {
		phase  state.Phase
		reason state.SuspendReason
		on     bool
		want   autoWakeMode
	}{
		{state.PhaseSuspended, state.SuspendReasonIdle, true, autoWakeResume},
		{state.PhaseSuspended, state.SuspendReasonStalled, true, autoWakeResume},
		{state.PhaseStarting, state.SuspendReasonIdle, true, autoWakeAwait},
		{state.PhaseStarting, state.SuspendReasonStalled, true, autoWakeAwait},
		// A resume of a user-suspended agent, or of one with no reason,
		// is none of the hub's business.
		{state.PhaseStarting, state.SuspendReasonUser, true, autoWakeNone},
		{state.PhaseStarting, "", true, autoWakeNone},
		{state.PhaseSuspended, state.SuspendReasonUser, true, autoWakeNone},
		{state.PhaseRunning, state.SuspendReasonIdle, true, autoWakeNone},
		{state.PhaseError, state.SuspendReasonIdle, true, autoWakeNone},
		{state.PhaseSuspended, state.SuspendReasonIdle, false, autoWakeNone},
		{state.PhaseStarting, state.SuspendReasonIdle, false, autoWakeNone},
	}
	for _, tc := range cases {
		setIdleSuspendExperiment(t, srv, tc.on)
		a := &store.Agent{ID: "a", Phase: string(tc.phase), SuspendReason: string(tc.reason)}
		assert.Equal(t, tc.want, srv.autoWakeModeOf(a), "phase=%s reason=%q experiment=%v", tc.phase, tc.reason, tc.on)
	}
	assert.Equal(t, autoWakeNone, srv.autoWakeModeOf(nil))
}

// A failed asynchronous resume ends in error (with resume memory
// suspended); the error write clears the reason, so the agent is not
// auto-woken again. This is the deliberate, conservative choice.
func TestAutoWake_ErrorAfterFailedResumeIsNotAutoWoken(t *testing.T) {
	srv, s, _, target, _ := autoWakeFixture(t, state.SuspendReasonIdle)
	require.NoError(t, s.UpdateAgentStatus(context.Background(), target.ID, store.AgentStatusUpdate{Phase: string(state.PhaseStarting)}))
	require.NoError(t, s.UpdateAgentStatus(context.Background(), target.ID, store.AgentStatusUpdate{Phase: string(state.PhaseError)}))
	got, err := s.GetAgent(context.Background(), target.ID)
	require.NoError(t, err)
	assert.Empty(t, got.SuspendReason, "the error write clears the reason")
	assert.Equal(t, autoWakeNone, srv.autoWakeModeOf(got))
}

// A background run whose re-read finds an error row that remembers a
// suspended resume (a failed asynchronous resume: no reason left) does not
// resume it, although wakeAgentForDM would act on the effective resting
// phase; the queued item fails instead.
func TestAutoWake_AsyncRunSkipsErrorMemoryRow(t *testing.T) {
	srv, s, _, target, disp := autoWakeFixture(t, state.SuspendReasonIdle)
	ctx := context.Background()
	require.NoError(t, s.UpdateAgentStatus(ctx, target.ID, store.AgentStatusUpdate{Phase: string(state.PhaseStarting)}))
	launchID, err := s.BeginStartLaunch(ctx, target.ID, store.LaunchKindStart, 5*time.Minute, store.BeginLaunchOptions{PriorPhase: "suspended"})
	require.NoError(t, err)
	_, _, err = s.ApplyLaunchReport(ctx, target.ID, target.RuntimeBrokerID, store.LaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateFailed, ErrorCode: "boom"})
	require.NoError(t, err)
	row, err := s.GetAgent(ctx, target.ID)
	require.NoError(t, err)
	require.Equal(t, string(state.PhaseError), row.Phase, "fixture check: the failed resume ends in error")
	require.Equal(t, string(state.PhaseSuspended), store.EffectiveRestingPhase(row), "fixture check: it remembers the suspended resume")
	require.Equal(t, autoWakeNone, srv.autoWakeModeOf(row))
	awReadyOnStart(t, s, target.ID)

	_, dmErr := srv.autoWakeAgent(ctx, target) // the enqueuer's copy is idle-suspended
	require.NotNil(t, dmErr, "the run refuses the error row")
	assert.Empty(t, disp.getStartCalls(), "it is not resumed again")
}

// A DM whose fresh read sees the target starting, because a hub wake of
// it is in flight, waits for that start and is delivered: no second
// resume, no 409. The row is starting before the send, so the outcome
// does not depend on scheduling.
func TestAutoWake_FreshStarting(t *testing.T) {
	t.Run("agent DM", func(t *testing.T) {
		srv, s, sender, target, disp := autoWakeFixture(t, state.SuspendReasonIdle)
		fresh := awStartingFresh(t, s, target)
		awReadyOnStart(t, s, target.ID)

		res, dmErr := srv.ExecuteAgentDM(context.Background(), awAgentDMInput(sender, fresh, "during an auto-wake"))
		require.Nil(t, dmErr)
		assert.Equal(t, AgentDMAccepted, res.Outcome)
		assert.Empty(t, disp.getStartCalls(), "nothing is started")
		assert.Len(t, disp.getMessageCalls(), 1)
		assertEveryRowDispatched(t, s, target.ID, 1)
	})

	t.Run("user DM", func(t *testing.T) {
		srv, s, _, target, disp := autoWakeFixture(t, state.SuspendReasonIdle)
		u := wakeLifecycleUser(t, s, "aw-fresh-user", target.ProjectID, "agent.message", "agent.read")
		awStartingFresh(t, s, target)
		awReadyOnStart(t, s, target.ID)

		rec := requestAsIdentity(t, srv, authUser(u), http.MethodPost, "/api/v1/agents/"+target.ID+"/message",
			map[string]interface{}{"message": "during an auto-wake"})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Empty(t, disp.getStartCalls(), "nothing is started")
		assert.Len(t, disp.getMessageCalls(), 1)
		assertEveryRowDispatched(t, s, target.ID, 1)
	})

	t.Run("scheduled message", func(t *testing.T) {
		f := newSchedWake(t, state.PhaseRunning)
		setIdleSuspendExperiment(t, f.srv, true)
		awSuspendFor(t, f.store, f.target, state.SuspendReasonIdle)
		u := f.user(t, "aw-fresh-sched", "agent.message", "agent.read")
		awStartingFresh(t, f.store, f.target)
		awReadyOnStart(t, f.store, f.target.ID)

		require.NoError(t, f.fire(withSessionRevision(f.event(false), u.ID)))
		assert.Empty(t, f.disp.getStartCalls(), "nothing is started")
		assert.Len(t, f.disp.getMessageCalls(), 1)
	})

	t.Run("a user's resume in flight keeps today's 409", func(t *testing.T) {
		srv, s, sender, target, disp := autoWakeFixture(t, state.SuspendReasonUser)
		fresh := awStartingFresh(t, s, target)
		_, dmErr := srv.ExecuteAgentDM(context.Background(), awAgentDMInput(sender, fresh, "x"))
		require.NotNil(t, dmErr)
		assert.Equal(t, ErrCodeAgentNotRunning, dmErr.Code)
		assert.Empty(t, disp.getMessageCalls())
	})

	t.Run("experiment off keeps today's 409", func(t *testing.T) {
		srv, s, _, target, disp := autoWakeFixture(t, state.SuspendReasonIdle)
		setIdleSuspendExperiment(t, srv, false)
		u := wakeLifecycleUser(t, s, "aw-fresh-off", target.ProjectID, "agent.message", "agent.read")
		awStartingFresh(t, s, target)
		rec := requestAsIdentity(t, srv, authUser(u), http.MethodPost, "/api/v1/agents/"+target.ID+"/message",
			map[string]interface{}{"message": "x"})
		require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
		assert.Empty(t, disp.getMessageCalls())
	})

	t.Run("a held target is refused, never waited on", func(t *testing.T) {
		srv, s, sender, target, disp := autoWakeFixture(t, state.SuspendReasonIdle)
		fresh := awStartingFresh(t, s, target)
		holdAgentFor(t, s, target.ID, target.ProjectID, target.Ancestry[0])
		_, dmErr := srv.ExecuteAgentDM(context.Background(), awAgentDMInput(sender, fresh, "x"))
		require.NotNil(t, dmErr)
		assert.Empty(t, disp.getMessageCalls())
	})
}

// A DM whose copy reads suspended while another start of the agent holds
// its start claim gets WakeDeferred; it waits for that start and is
// delivered, never kept deferred. The other start is driven from the
// point the DM logs that it is waiting, not from timing.
func TestAutoWake_WakeDeferred(t *testing.T) {
	for _, path := range []string{"agent DM", "user DM"} {
		t.Run(path, func(t *testing.T) {
			srv, s, sender, target, disp := autoWakeFixture(t, state.SuspendReasonIdle)
			signal := newAWSignal("wake: another start of the target is in progress; waiting for it")
			srv.messageLog = slog.New(signal)
			_, err := s.ClaimAgentStart(context.Background(), target.ID, "other-wake", store.StartClaimWake, "", time.Minute)
			require.NoError(t, err)
			awReadyOnStart(t, s, target.ID)
			go func() {
				select {
				case <-signal.fired:
				case <-time.After(20 * time.Second):
					return
				}
				// The other start writes starting; awReadyOnStart then
				// reports it running.
				_ = s.UpdateAgentStatus(context.Background(), target.ID, store.AgentStatusUpdate{Phase: string(state.PhaseStarting)})
			}()

			if path == "agent DM" {
				res, dmErr := srv.ExecuteAgentDM(context.Background(), awAgentDMInput(sender, target, "deferred wake"))
				require.Nil(t, dmErr)
				assert.Equal(t, AgentDMAccepted, res.Outcome)
			} else {
				u := wakeLifecycleUser(t, s, "aw-deferred-user", target.ProjectID, "agent.message", "agent.read")
				rec := requestAsIdentity(t, srv, authUser(u), http.MethodPost, "/api/v1/agents/"+target.ID+"/message",
					map[string]interface{}{"message": "deferred wake"})
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			}
			select {
			case <-signal.fired:
			default:
				t.Fatal("fixture check: the send did not take the WakeDeferred wait")
			}
			assert.Empty(t, disp.getStartCalls(), "the start in progress is reused")
			assert.Len(t, disp.getMessageCalls(), 1)
			assertEveryRowDispatched(t, s, target.ID, 1)
		})
	}
}

// A DM whose copy is stale (read suspended) while the row already reads
// starting and another wake holds the claim is waited on and delivered.
func TestAutoWake_AgentDM_StaleSuspendedCopy(t *testing.T) {
	srv, s, sender, target, disp := autoWakeFixture(t, state.SuspendReasonIdle)
	stale := *target
	_, err := s.ClaimAgentStart(context.Background(), target.ID, "other-wake", store.StartClaimWake, "", time.Minute)
	require.NoError(t, err)
	awStartingFresh(t, s, target)
	awReadyOnStart(t, s, target.ID)

	res, dmErr := srv.ExecuteAgentDM(context.Background(), awAgentDMInput(sender, &stale, "during a wake"))
	require.Nil(t, dmErr)
	assert.Equal(t, AgentDMAccepted, res.Outcome)
	assert.Empty(t, disp.getStartCalls(), "the start in progress is reused")
	assert.Len(t, disp.getMessageCalls(), 1)
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

	t.Run("a target another wake already moved to starting is waited for, then delivered", func(t *testing.T) {
		srv, s, sender, target, disp := autoWakeFixture(t, state.SuspendReasonIdle)
		stale := *target // the fan-out read it while suspended
		require.NoError(t, s.UpdateAgentStatus(context.Background(), target.ID, store.AgentStatusUpdate{Phase: string(state.PhaseStarting)}))
		awReadyOnStart(t, s, target.ID)

		res, dmErr := srv.ExecuteAgentDM(context.Background(), awMentionInput(sender, &stale, "@target look"))
		require.Nil(t, dmErr)
		require.Equal(t, AgentDMWakeQueued, res.Outcome)
		srv.waitAutoWakeIdle()

		assert.Empty(t, disp.getStartCalls(), "no second resume")
		require.Len(t, disp.getMessageCalls(), 1)
		m, err := s.GetMessage(context.Background(), res.MessageID)
		require.NoError(t, err)
		assert.Equal(t, store.MessageDispatchDispatched, m.DispatchState)
	})

	t.Run("the attachment addressee is settled by the wake's outcome", func(t *testing.T) {
		for _, fail := range []bool{false, true} {
			srv, s, sender, target, disp := autoWakeFixture(t, state.SuspendReasonIdle)
			if fail {
				disp.startReturnErr = assert.AnError
			} else {
				awReadyOnStart(t, s, target.ID)
			}
			parent := &store.Message{
				ID: api.NewUUID(), ProjectID: sender.ProjectID, Sender: "agent:" + sender.Slug, SenderID: sender.ID,
				Recipient: "agent:" + sender.Slug, RecipientID: sender.ID, Msg: "parent", Type: "instruction",
				AgentID: sender.ID, CreatedAt: time.Now(), DispatchState: store.MessageDispatchDispatched,
			}
			require.NoError(t, s.CreateMessage(context.Background(), parent))
			row := srv.recordAttachmentAddressee(context.Background(), parent.ID, target.ID, "body-mention")
			require.NotEmpty(t, row)

			in := awMentionInput(sender, target, "@target with a file")
			in.AttachmentAddressee = row
			res, dmErr := srv.ExecuteAgentDM(context.Background(), in)
			require.Nil(t, dmErr)
			require.Equal(t, AgentDMWakeQueued, res.Outcome)
			srv.waitAutoWakeIdle()

			rows, err := s.ListAddressees(context.Background(), parent.ID)
			require.NoError(t, err)
			require.Len(t, rows, 1)
			want := addresseeDelivered
			if fail {
				want = addresseeFailed
			}
			assert.Equal(t, want, rows[0].DeliveryState, "wake failed=%v", fail)
		}
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

	t.Run("expired pending row is not delivered late after auto-wake finishes", func(t *testing.T) {
		ctx := context.Background()
		srv, s, faultStore, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *backdatePendingCreateStore {
			return &backdatePendingCreateStore{Store: inner, fault: f, age: 11 * time.Minute}
		})
		setIdleSuspendExperiment(t, srv, true)

		owner := &store.User{
			ID: tid("aw-o1-owner"), Email: "aw-o1@test.example",
			Role: store.UserRoleMember, Status: "active", Created: time.Now(),
		}
		require.NoError(t, s.CreateUser(ctx, owner))
		ensureHubMembership(ctx, s, owner.ID)
		proj := &store.Project{
			ID: tid("aw-o1-proj"), Name: "aw-o1-proj", Slug: "aw-o1-proj",
			OwnerID: owner.ID, CreatedBy: owner.ID,
		}
		require.NoError(t, s.CreateProject(ctx, proj))
		ensureStandingRoot(t, s, proj.ID, owner.ID)
		brokerID := tid("aw-o1-broker")
		require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
			ID: brokerID, Name: "aw-o1-broker", Slug: "aw-o1-broker", Status: store.BrokerStatusOnline,
		}))
		sender := &store.Agent{
			ID: tid("aw-o1-sender"), Name: "aw-o1-sender", Slug: "aw-o1-sender",
			ProjectID: proj.ID, Phase: string(state.PhaseRunning), RuntimeBrokerID: brokerID,
			MessageMode: store.MessageModeProject, Ancestry: []string{owner.ID},
		}
		require.NoError(t, s.CreateAgent(ctx, sender))
		target := &store.Agent{
			ID: tid("aw-o1-target"), Name: "aw-o1-target", Slug: "aw-o1-target",
			ProjectID: proj.ID, Phase: string(state.PhaseRunning), RuntimeBrokerID: brokerID,
			MessageMode: store.MessageModeProject, Ancestry: []string{owner.ID},
		}
		require.NoError(t, s.CreateAgent(ctx, target))
		awSuspendFor(t, s, target, state.SuspendReasonIdle)

		disp := &wakeTrackingDispatcher{}
		srv.SetDispatcher(disp)

		parent := &store.Message{
			ID: api.NewUUID(), ProjectID: sender.ProjectID, Sender: "agent:" + sender.Slug, SenderID: sender.ID,
			Recipient: "agent:" + sender.Slug, RecipientID: sender.ID, Msg: "parent", Type: "instruction",
			AgentID: sender.ID, CreatedAt: time.Now(), DispatchState: store.MessageDispatchDispatched,
		}
		require.NoError(t, s.CreateMessage(ctx, parent))
		addrRow := srv.recordAttachmentAddressee(ctx, parent.ID, target.ID, "body-mention")
		require.NotEmpty(t, addrRow)

		// Backdate CreatedAt on the queued mention row so the sweep sees it as
		// past the 10m stuckMessageExpireTTL while the background wake is still
		// waiting on target readiness (ptone/scion#7405, review O1).
		fault.Arm()
		faultStore.backdate.Store(true)
		in := awMentionInput(sender, target, "@target queued-past-ttl")
		in.AttachmentAddressee = addrRow
		res, dmErr := srv.ExecuteAgentDM(ctx, in)
		faultStore.backdate.Store(false)
		require.Nil(t, dmErr)
		require.Equal(t, AgentDMWakeQueued, res.Outcome)

		// Wait until runAutoWake has started resuming target and is waiting for readiness.
		require.Eventually(t, func() bool {
			return len(disp.getStartCalls()) == 1
		}, 5*time.Second, 10*time.Millisecond, "background auto-wake must begin resume")

		// Sweep expires the pending row while it is still queued behind the wake.
		srv.brokerMessageSweepHandler()(ctx)
		expiredRow, err := s.GetMessage(ctx, res.MessageID)
		require.NoError(t, err)
		require.Equal(t, store.MessageDispatchFailed, expiredRow.DispatchState)

		// Mark target ready so runAutoWake proceeds to deliver.
		require.NoError(t, s.UpdateAgentStatus(ctx, target.ID, store.AgentStatusUpdate{
			Phase: string(state.PhaseRunning), Activity: "idle",
		}))
		srv.waitAutoWakeIdle()

		assert.Empty(t, disp.getMessageCalls(), "expired pending row must never be delivered late after auto-wake")
		afterRow, err := s.GetMessage(ctx, res.MessageID)
		require.NoError(t, err)
		assert.Equal(t, store.MessageDispatchFailed, afterRow.DispatchState)
		require.NotNil(t, afterRow.DispatchFailureReason)
		assert.Equal(t, store.MessageExpiredStuckPendingReason, *afterRow.DispatchFailureReason)

		addrs, err := s.ListAddressees(ctx, parent.ID)
		require.NoError(t, err)
		require.Len(t, addrs, 1)
		assert.Equal(t, addresseeFailed, addrs[0].DeliveryState, "attachment addressee must be settled as failed when row is no longer pending")
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

	t.Run("a woken lineage-mode secondary becomes a group participant", func(t *testing.T) {
		f, second := setup(t, state.SuspendReasonIdle)
		second.MessageMode = store.MessageModeOwner
		second.Ancestry = []string{DevUserID}
		require.NoError(t, f.s.UpdateAgent(t.Context(), second))
		awReadyOnStart(t, f.s, second.ID)
		rec := doRequest(t, f.srv, http.MethodPost, f.path(), map[string]any{"content": "hi @aw-chat-second"})
		require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())
		f.srv.waitAutoWakeIdle()

		row := mentionRow(t, f, second.ID)
		require.NotEmpty(t, row.ConversationID)
		parts, err := f.s.ListParticipants(t.Context(), row.ConversationID)
		require.NoError(t, err)
		var found bool
		for _, p := range parts {
			if p.PrincipalKind == "agent" && p.PrincipalID == second.ID {
				found = true
			}
		}
		assert.True(t, found, "the woken secondary is a participant, so it can reply into the group")
		decision, err := f.srv.agentGroupPostDecision(t.Context(), second, row.ConversationID)
		require.NoError(t, err)
		assert.True(t, decision.Allowed, "a lineage-mode participant may post into the group: %s", decision.Reason)
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

// suspendShutdownRaceStore orders shutdown status/heartbeat writes right after
// the hub's suspend write while letting the handlers read the agent row before
// suspend commits (while phase is still running), reproducing ptone/scion#8256.
type suspendShutdownRaceStore struct {
	store.Store
	fault            *storeFaultSwitch
	targetID         string
	wantReports      int
	mu               sync.Mutex
	reportsAtStore   int
	heartbeatAtStore chan struct{}
	allReportsReady  chan struct{}
	suspendWritten   chan struct{}
	heartbeatOnce    sync.Once
	suspendOnce      sync.Once
}

func (w *suspendShutdownRaceStore) TryAdvisoryLock(ctx context.Context, key store.AdvisoryLockKey) (bool, func() error, error) {
	return w.Store.(store.AdvisoryLocker).TryAdvisoryLock(ctx, key)
}

func (w *suspendShutdownRaceStore) TryAdvisoryLockObject(ctx context.Context, classID store.AdvisoryLockKey, objID int32) (bool, func() error, error) {
	return w.Store.(store.AdvisoryLocker).TryAdvisoryLockObject(ctx, classID, objID)
}

func (w *suspendShutdownRaceStore) UpdateAgentStatus(ctx context.Context, id string, su store.AgentStatusUpdate) error {
	if !w.fault.Active() || id != w.targetID {
		return w.Store.UpdateAgentStatus(ctx, id, su)
	}
	if su.ReportSource != "" {
		if su.ReportSource == store.StatusReportSourceRuntime {
			w.heartbeatOnce.Do(func() { close(w.heartbeatAtStore) })
		}
		w.mu.Lock()
		w.reportsAtStore++
		if w.reportsAtStore == w.wantReports {
			close(w.allReportsReady)
		}
		w.mu.Unlock()
		<-w.suspendWritten
		return w.Store.UpdateAgentStatus(ctx, id, su)
	}
	err := w.Store.UpdateAgentStatus(ctx, id, su)
	if su.Phase == string(state.PhaseSuspended) {
		w.suspendOnce.Do(func() { close(w.suspendWritten) })
	}
	return err
}

type stopCallbackDispatcher struct {
	*wakeTrackingDispatcher
	onStop func(ctx context.Context, agent *store.Agent) error
}

func (d *stopCallbackDispatcher) DispatchAgentStop(ctx context.Context, agent *store.Agent) error {
	if d.onStop != nil {
		return d.onStop(ctx, agent)
	}
	return nil
}

// TestSuspend_ShutdownReportsKeepSuspendedAndAutoWakeable is the hub-level
// end-to-end regression test for ptone/scion#8256: when DispatchAgentStop
// triggers the dying container's shutdown reports (sciontool stopping +
// stopped "Session ended", and a broker heartbeat reporting stopped) whose
// handler reads happen while the row is still running and whose store writes
// land after phase=suspended commits, the row ends suspended with its
// suspend_reason and retained activity intact, and a stalled-suspended agent
// remains auto-wakeable (2b).
func TestSuspend_ShutdownReportsKeepSuspendedAndAutoWakeable(t *testing.T) {
	srv, s, raceStore, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *suspendShutdownRaceStore {
		return &suspendShutdownRaceStore{
			Store:            inner,
			fault:            f,
			wantReports:      3,
			heartbeatAtStore: make(chan struct{}),
			allReportsReady:  make(chan struct{}),
			suspendWritten:   make(chan struct{}),
		}
	})
	setIdleSuspendExperiment(t, srv, true)
	t.Cleanup(srv.waitAutoWakeIdle)

	ctx := context.Background()
	target := newStalledBrokerAgent(t, s, "susp-race-stalled")
	const runID = "run-susp-race-1"
	_, err := s.SetAgentRunID(ctx, target.ID, runID, nil)
	require.NoError(t, err)
	// Stalled from completed so suspend retains activity=completed.
	target, err = s.GetAgent(ctx, target.ID)
	require.NoError(t, err)
	target.StalledFromActivity = string(state.ActivityCompleted)
	target.MessageMode = store.MessageModeProject
	require.NoError(t, s.UpdateAgent(ctx, target))

	sender := &store.Agent{
		ID:              tid("sender-susp-race"),
		Slug:            "sender-susp-race",
		Name:            "Sender",
		ProjectID:       target.ProjectID,
		RuntimeBrokerID: target.RuntimeBrokerID,
		Phase:           string(state.PhaseRunning),
		MessageMode:     store.MessageModeProject,
		OwnerID:         DevUserID,
		CreatedBy:       DevUserID,
	}
	require.NoError(t, s.CreateAgent(ctx, sender))
	ensureStandingRoot(t, s, target.ProjectID, DevUserID)
	target, err = s.GetAgent(ctx, target.ID)
	require.NoError(t, err)
	target.Ancestry = []string{DevUserID}
	require.NoError(t, s.UpdateAgent(ctx, target))
	sender, err = s.GetAgent(ctx, sender.ID)
	require.NoError(t, err)
	sender.Ancestry = []string{DevUserID}
	require.NoError(t, s.UpdateAgent(ctx, sender))

	raceStore.targetID = target.ID
	baseDisp := &wakeTrackingDispatcher{}
	var reportWG sync.WaitGroup
	disp := &stopCallbackDispatcher{
		wakeTrackingDispatcher: baseDisp,
		onStop: func(_ context.Context, a *store.Agent) error {
			// Trigger sciontool's shutdown reports for runID while the DB row
			// is still running: updateAgentStatus's GetAgent sees phase=running
			// (bypassing Guard 0) and blocks at UpdateAgentStatus until
			// recordStopStatus writes phase=suspended.
			postAgentStatus := func(upd store.AgentStatusUpdate) {
				reportWG.Add(1)
				go func() {
					defer reportWG.Done()
					body, merr := json.Marshal(upd)
					require.NoError(t, merr)
					claims := &AgentTokenClaims{
						Claims:    jwt.Claims{Subject: a.ID},
						ProjectID: a.ProjectID,
						Scopes:    []AgentTokenScope{ScopeAgentStatusUpdate},
						RunID:     runID,
					}
					req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+a.ID+"/status", bytes.NewReader(body))
					rctx := context.WithValue(req.Context(), agentContextKey{}, claims)
					rctx = contextWithIdentity(rctx, &agentIdentityWrapper{claims})
					req = req.WithContext(rctx)
					rec := httptest.NewRecorder()
					srv.updateAgentStatus(rec, req, a.ID)
					assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				}()
			}
			postAgentStatus(store.AgentStatusUpdate{
				Phase:   string(state.PhaseStopping),
				Message: "Agent shutting down",
			})
			postAgentStatus(store.AgentStatusUpdate{
				Phase:      string(state.PhaseStopped),
				Message:    "Session ended",
				ExitReason: string(state.ExitReasonSignaled),
			})

			select {
			case <-raceStore.allReportsReady:
			case <-time.After(5 * time.Second):
				t.Error("timed out waiting for shutdown reports to reach UpdateAgentStatus")
			}
			return nil
		},
	}
	srv.SetDispatcher(disp)

	target, err = s.GetAgent(ctx, target.ID)
	require.NoError(t, err)
	fault.Arm()

	// Start a broker heartbeat reporting stopped while no local lifecycleOp is
	// active (as on another replica or just before the op starts), so its
	// pre-write read sees phase=running (agentSuspended=false) and carries
	// Phase=stopped into UpdateAgentStatus after suspend commits.
	reportWG.Add(1)
	go func() {
		defer reportWG.Done()
		exitZero := 0
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/runtime-brokers/"+target.RuntimeBrokerID+"/heartbeat",
			brokerHeartbeatRequest{
				Status: store.BrokerStatusOnline,
				Projects: []brokerProjectHeartbeat{{
					ProjectID:  target.ProjectID,
					AgentCount: 1,
					Agents: []brokerAgentHeartbeat{{
						Slug:            target.Slug,
						RunID:           runID,
						Phase:           string(state.PhaseStopped),
						ContainerStatus: "exited (0)",
						ExitCode:        &exitZero,
					}},
				}},
			})
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}()
	select {
	case <-raceStore.heartbeatAtStore:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for heartbeat to reach UpdateAgentStatus")
	}

	srv.enqueueAutoSuspend([]store.Agent{*target})
	srv.waitAutoSuspendIdle()
	reportWG.Wait()

	// Disarm the race wrapper before testing auto-wake.
	raceStore.targetID = ""

	got, err := s.GetAgent(ctx, target.ID)
	require.NoError(t, err)
	require.Equal(t, string(state.PhaseSuspended), got.Phase, "shutdown reports must not move suspended row to stopped")
	require.Equal(t, string(state.SuspendReasonStalled), got.SuspendReason, "suspend_reason must remain stalled")
	assert.Equal(t, string(state.ActivityCompleted), got.Activity, "retained suspend activity must be preserved")

	// A stalled-suspended agent remains auto-wakeable (phase 2b) after the reports.
	awReadyOnStart(t, s, target.ID)
	res, dmErr := srv.ExecuteAgentDM(ctx, awAgentDMInput(sender, got, "wake after shutdown reports"))
	require.Nil(t, dmErr)
	assert.Equal(t, AgentDMAccepted, res.Outcome)
	require.Len(t, baseDisp.getStartCalls(), 1, "stalled-suspended agent auto-wakes once")
	assert.True(t, baseDisp.getStartCalls()[0].Continue, "resume continues the session")
	msgs := baseDisp.getMessageCalls()
	require.Len(t, msgs, 1)
	assert.Equal(t, "wake after shutdown reports", msgs[0].Message)

	woken, err := s.GetAgent(ctx, target.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseRunning), woken.Phase)
	assert.Empty(t, woken.SuspendReason, "resume clears suspend_reason")
}

// TestAutoWake_MentionStalledSuspendedWithExperimentOffMatchesBase verifies R3
// (AC 8): with lifecycle.idle_suspend off, a mention to a stalled-suspended
// agent preserves base behaviour (accepted, persisted as dispatched, handed to
// DispatchAgentMessage with zero auto-wake starts and zero MarkAgentDelivery
// queries).
func TestAutoWake_MentionStalledSuspendedWithExperimentOffMatchesBase(t *testing.T) {
	ctx := context.Background()
	srv, s, sender, target, disp := autoWakeFixture(t, state.SuspendReasonStalled)
	setIdleSuspendExperiment(t, srv, false)

	res, dmErr := srv.ExecuteAgentDM(ctx, awMentionInput(sender, target, "@target"))
	require.Nil(t, dmErr)
	require.NotNil(t, res)
	assert.Equal(t, AgentDMAccepted, res.Outcome)
	assert.Empty(t, disp.getStartCalls())
	require.Len(t, disp.getMessageCalls(), 1)

	msgs, err := s.ListMessages(ctx, store.MessageFilter{RecipientID: target.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, msgs.Items, 1)
	assert.Equal(t, store.MessageDispatchDispatched, msgs.Items[0].DispatchState)
}
