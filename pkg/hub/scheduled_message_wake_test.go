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
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Scheduled message wake (ptone/scion#7734): a message event with wake set
// resumes a suspended target through the shared direct-message wake path,
// under the event's authorization revision, then delivers.

// schedWake is a scheduled wake fixture: a project with an online broker, a
// target agent in the given phase, a sender agent, and a dispatcher that
// records resumes and deliveries.
type schedWake struct {
	srv    *Server
	store  store.Store
	sender *store.Agent
	target *store.Agent
	disp   *wakeTrackingDispatcher
}

func newSchedWake(t *testing.T, phase state.Phase) *schedWake {
	t.Helper()
	srv, s, sender, target := createWakeDMFixtures(t, string(phase))
	disp := &wakeTrackingDispatcher{}
	srv.SetDispatcher(disp)
	return &schedWake{srv: srv, store: s, sender: sender, target: target, disp: disp}
}

// user creates a project member holding exactly permissions.
func (f *schedWake) user(t *testing.T, id string, permissions ...string) *store.User {
	t.Helper()
	return wakeLifecycleUser(t, f.store, id, f.target.ProjectID, permissions...)
}

// event returns a message event to the target with the given wake flag.
func (f *schedWake) event(wake bool) store.ScheduledEvent {
	payload, _ := json.Marshal(MessageEventPayload{AgentID: f.target.ID, Message: "scheduled poke", Wake: wake})
	return store.ScheduledEvent{
		ID:         api.NewUUID(),
		ProjectID:  f.target.ProjectID,
		EventType:  "message",
		Payload:    string(payload),
		ScheduleID: "sched-wake",
		FireAt:     time.Now(),
	}
}

func (f *schedWake) fire(evt store.ScheduledEvent) error {
	return f.srv.messageEventHandler()(context.Background(), evt)
}

// reportReadyOnStart marks the target running once the wake has written
// starting, as the resumed container's first status would.
func (f *schedWake) reportReadyOnStart(t *testing.T) {
	t.Helper()
	go func() {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			a, err := f.store.GetAgent(context.Background(), f.target.ID)
			if err == nil && a.Phase == string(state.PhaseStarting) {
				_ = f.store.UpdateAgentStatus(context.Background(), f.target.ID, store.AgentStatusUpdate{
					Phase: string(state.PhaseRunning), Activity: "idle",
				})
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
}

func (f *schedWake) phase(t *testing.T) string {
	t.Helper()
	a, err := f.store.GetAgent(context.Background(), f.target.ID)
	require.NoError(t, err)
	return a.Phase
}

// lifecyclePerms are the permissions of a user who may message and start
// the target.
var lifecyclePerms = []string{"agent.message", "agent.read", "agent.lifecycle"}

func TestScheduledMessageWake_ResumesAndDelivers(t *testing.T) {
	f := newSchedWake(t, state.PhaseSuspended)
	u := f.user(t, "sw-resume", lifecyclePerms...)
	f.reportReadyOnStart(t)

	require.NoError(t, f.fire(withSessionRevision(f.event(true), u.ID)))

	starts := f.disp.getStartCalls()
	require.Len(t, starts, 1, "exactly one resume")
	assert.True(t, starts[0].Continue, "the wake resumes the prior session")
	msgs := f.disp.getMessageCalls()
	require.Len(t, msgs, 1, "the message is delivered after the resume")
	assert.Equal(t, "scheduled poke", msgs[0].Message)
	assert.Equal(t, string(state.PhaseRunning), f.phase(t))
}

func TestScheduledMessageWake_WithoutWakeUnchanged(t *testing.T) {
	f := newSchedWake(t, state.PhaseSuspended)
	u := f.user(t, "sw-nowake", lifecyclePerms...)
	// Without wake a suspended target is not resumed. The dispatch is
	// attempted as before and its outcome is the dispatcher's.
	f.disp.msgReturnErr = assert.AnError

	err := f.fire(withSessionRevision(f.event(false), u.ID))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to dispatch message")
	assert.Empty(t, f.disp.getStartCalls(), "no resume without wake")
	assert.Equal(t, string(state.PhaseSuspended), f.phase(t))
}

func TestScheduledMessageWake_RunningTargetIsNotRestarted(t *testing.T) {
	f := newSchedWake(t, state.PhaseRunning)
	// No resume is needed, so message permission suffices.
	u := f.user(t, "sw-running", "agent.message", "agent.read")

	require.NoError(t, f.fire(withSessionRevision(f.event(true), u.ID)))
	assert.Empty(t, f.disp.getStartCalls())
	assert.Len(t, f.disp.getMessageCalls(), 1)
}

func TestScheduledMessageWake_RefusedWithoutLifecyclePermission(t *testing.T) {
	f := newSchedWake(t, state.PhaseSuspended)
	u := f.user(t, "sw-msgonly", "agent.message", "agent.read")

	err := f.fire(withSessionRevision(f.event(true), u.ID))
	require.Error(t, err)
	assert.Equal(t, errScheduledMessageRefused.Error(), err.Error())
	assert.Empty(t, f.disp.getStartCalls(), "a refused wake starts nothing")
	assert.Empty(t, f.disp.getMessageCalls(), "a refused wake delivers nothing")
	assert.Equal(t, string(state.PhaseSuspended), f.phase(t))
}

func TestScheduledMessageWake_AgentPrincipalNeedsLifecycleScope(t *testing.T) {
	t.Run("without the scope the wake is refused", func(t *testing.T) {
		f := newSchedWake(t, state.PhaseSuspended)
		// The fixture sender has no applied role, so without the test
		// server's dev-auth role override its scheduled identity carries no
		// lifecycle scope.
		f.srv.authzService.mintDevAuthOverride = false
		err := f.fire(withMockAgentRevision(f.event(true), f.sender.ID))
		require.Error(t, err)
		assert.Equal(t, errScheduledMessageRefused.Error(), err.Error())
		assert.Empty(t, f.disp.getStartCalls())
		assert.Empty(t, f.disp.getMessageCalls())
	})
	t.Run("with the full role the wake resumes and delivers", func(t *testing.T) {
		f := newSchedWake(t, state.PhaseSuspended)
		f.srv.authzService.mintDevAuthOverride = false
		sender, err := f.store.GetAgent(context.Background(), f.sender.ID)
		require.NoError(t, err)
		sender.AppliedConfig = &store.AgentAppliedConfig{AgentRole: string(AgentRoleFull)}
		require.NoError(t, f.store.UpdateAgent(context.Background(), sender))
		f.reportReadyOnStart(t)

		require.NoError(t, f.fire(withMockAgentRevision(f.event(true), f.sender.ID)))
		assert.Len(t, f.disp.getStartCalls(), 1)
		assert.Len(t, f.disp.getMessageCalls(), 1)
	})
	t.Run("a revision recorded without lifecycle is refused after the role widens", func(t *testing.T) {
		// Review R1 of ptone/scion#7769: the agent's current role grants
		// lifecycle, but the frozen revision ceiling does not. The revision
		// decides: the fire is refused and nothing starts.
		f := newSchedWake(t, state.PhaseSuspended)
		f.srv.authzService.mintDevAuthOverride = false
		sender, err := f.store.GetAgent(context.Background(), f.sender.ID)
		require.NoError(t, err)
		sender.AppliedConfig = &store.AgentAppliedConfig{AgentRole: string(AgentRoleFull)}
		require.NoError(t, f.store.UpdateAgent(context.Background(), sender))
		evt := withMockAgentRevision(f.event(true), f.sender.ID)
		evt.AuthorityCeiling.PermissionIDs = []string{"agent.read", "agent.message"}
		require.False(t, EffectCeilingAllows(evt.AuthorityCeiling, scheduledWakePermission, false))
		f.reportReadyOnStart(t)

		err = f.fire(evt)
		require.Error(t, err)
		assert.Equal(t, errScheduledMessageRefused.Error(), err.Error())
		assert.Empty(t, f.disp.getStartCalls(), "a refused revision never starts the agent")
		assert.Empty(t, f.disp.getMessageCalls(), "a refused revision delivers nothing")
		assert.Equal(t, string(state.PhaseSuspended), f.phase(t))

		auth := ScheduledAuthority{PrincipalKind: store.DelegationPrincipalAgent, PrincipalID: f.sender.ID, Ceiling: evt.AuthorityCeiling}
		err = f.srv.authorizeScheduledWake(context.Background(), auth, wakeDMSenderIdentity(f.sender, ScopeAgentLifecycle), f.target)
		require.ErrorIs(t, err, errScheduledAuthorityDenied)
	})
	t.Run("authorizeScheduledWake refuses an agent identity without the scope", func(t *testing.T) {
		f := newSchedWake(t, state.PhaseSuspended)
		auth := ScheduledAuthority{PrincipalKind: store.DelegationPrincipalAgent, PrincipalID: f.sender.ID,
			Ceiling: store.EffectCeiling{Kind: store.EffectCeilingBounded, Version: permissions.CeilingVersionV1,
				PermissionIDs: []string{"agent.lifecycle", "agent.message"}}}
		require.Error(t, f.srv.authorizeScheduledWake(context.Background(), auth, wakeDMSenderIdentity(f.sender), f.target))
		require.NoError(t, f.srv.authorizeScheduledWake(context.Background(), auth,
			wakeDMSenderIdentity(f.sender, ScopeAgentLifecycle), f.target))
	})
}

func TestScheduledMessageWake_RevisionCeilingMustAllowLifecycle(t *testing.T) {
	f := newSchedWake(t, state.PhaseSuspended)
	u := f.user(t, "sw-ceiling", lifecyclePerms...)
	identity := NewAuthenticatedUser(u.ID, u.Email, u.DisplayName, u.Role, "scheduler")
	bounded := func(ids ...string) store.EffectCeiling {
		return store.EffectCeiling{Kind: store.EffectCeilingBounded, Version: permissions.CeilingVersionV1, PermissionIDs: ids}
	}

	auth := ScheduledAuthority{PrincipalKind: store.DelegationPrincipalUser, PrincipalID: u.ID,
		Ceiling: bounded("agent.message", "agent.read")}
	err := f.srv.authorizeScheduledWake(context.Background(), auth, identity, f.target)
	require.ErrorIs(t, err, errScheduledAuthorityDenied)
	assert.Contains(t, err.Error(), scheduledWakePermission)

	auth.Ceiling = bounded("agent.lifecycle", "agent.message", "agent.read")
	require.NoError(t, f.srv.authorizeScheduledWake(context.Background(), auth, identity, f.target))

	auth.Ceiling = store.EffectCeiling{Kind: store.EffectCeilingPrincipal}
	require.NoError(t, f.srv.authorizeScheduledWake(context.Background(), auth, identity, f.target))
}

func TestScheduledMessageWake_HeldTargetIsRefused(t *testing.T) {
	f := newSchedWake(t, state.PhaseSuspended)
	u := f.user(t, "sw-held", lifecyclePerms...)
	holdAgentFor(t, f.store, f.target.ID, f.target.ProjectID, f.target.Ancestry[0])

	err := f.fire(withSessionRevision(f.event(true), u.ID))
	require.Error(t, err)
	assert.Equal(t, errScheduledMessageRefused.Error(), err.Error())
	assert.Empty(t, f.disp.getStartCalls(), "a held target is never woken")
	assert.Empty(t, f.disp.getMessageCalls())
	assert.Equal(t, string(state.PhaseSuspended), f.phase(t))
}

func TestScheduledMessageWake_StoppedTargetFails(t *testing.T) {
	f := newSchedWake(t, state.PhaseStopped)
	u := f.user(t, "sw-stopped", lifecyclePerms...)

	err := f.fire(withSessionRevision(f.event(true), u.ID))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to wake agent "+f.target.Slug)
	assert.Contains(t, err.Error(), "stopped, not suspended")
	assert.Empty(t, f.disp.getStartCalls())
	assert.Empty(t, f.disp.getMessageCalls())
}

func TestScheduledMessageWake_Deferred(t *testing.T) {
	t.Run("delivers once the start in progress brings the target up", func(t *testing.T) {
		f := newSchedWake(t, state.PhaseSuspended)
		u := f.user(t, "sw-deferred-ok", lifecyclePerms...)
		_, err := f.store.ClaimAgentStart(context.Background(), f.target.ID, "other-hub", store.StartClaimUser, "", time.Minute)
		require.NoError(t, err)
		// The other start writes starting only after the readiness wait's
		// first poll (500ms) would have run: the wake must wait for the
		// start to begin rather than fail on the suspended row.
		go func() {
			time.Sleep(600 * time.Millisecond)
			_ = f.store.UpdateAgentStatus(context.Background(), f.target.ID, store.AgentStatusUpdate{
				Phase: string(state.PhaseStarting),
			})
			time.Sleep(300 * time.Millisecond)
			_ = f.store.UpdateAgentStatus(context.Background(), f.target.ID, store.AgentStatusUpdate{
				Phase: string(state.PhaseRunning), Activity: "idle",
			})
		}()

		require.NoError(t, f.fire(withSessionRevision(f.event(true), u.ID)))
		assert.Empty(t, f.disp.getStartCalls(), "the other start holds the claim; no second resume")
		assert.Len(t, f.disp.getMessageCalls(), 1, "delivered after the other start")
	})
	t.Run("fails visibly when the target does not become ready", func(t *testing.T) {
		prev := wakeReadyTimeout
		wakeReadyTimeout = 300 * time.Millisecond
		t.Cleanup(func() { wakeReadyTimeout = prev })

		f := newSchedWake(t, state.PhaseSuspended)
		u := f.user(t, "sw-deferred-fail", lifecyclePerms...)
		_, err := f.store.ClaimAgentStart(context.Background(), f.target.ID, "other-hub", store.StartClaimUser, "", time.Minute)
		require.NoError(t, err)

		err = f.fire(withSessionRevision(f.event(true), u.ID))
		require.ErrorIs(t, err, errScheduledWakeNotReady)
		assert.Empty(t, f.disp.getStartCalls())
		assert.Empty(t, f.disp.getMessageCalls(), "nothing is delivered")
	})
}

func TestScheduledPayloadWakes(t *testing.T) {
	assert.True(t, scheduledPayloadWakes(`{"agentName":"a","message":"m","wake":true}`))
	assert.False(t, scheduledPayloadWakes(`{"agentName":"a","message":"m"}`))
	assert.False(t, scheduledPayloadWakes(""))
	assert.False(t, scheduledPayloadWakes("not json"))
}

// TestScheduledMessageWake_RecurringFireAdvancesNextRunFirst: a recurring
// wake fire can outlast the evaluator tick, so next_run_at is advanced
// before its handler runs (a later tick does not select the schedule again
// meanwhile), and its outcome is recorded even when the tick's context has
// ended. A fire without wake keeps the old order.
func TestScheduledMessageWake_RecurringFireAdvancesNextRunFirst(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	require.NotNil(t, srv.scheduler)

	for _, wake := range []bool{true, false} {
		name := "poke-nowake"
		if wake {
			name = "poke-wake"
		}
		rec := doRequest(t, srv, "POST", "/api/v1/projects/"+projectID+"/schedules", CreateScheduleRequest{
			Name: name, CronExpr: "*/5 * * * *", EventType: "message",
			AgentName: "some-agent", Message: "recheck", Wake: wake,
		})
		require.Equal(t, 201, rec.Code, rec.Body.String())
		var created store.Schedule
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&created))

		// Make the schedule due now.
		row, err := s.GetSchedule(context.Background(), created.ID)
		require.NoError(t, err)
		due := time.Now().UTC().Add(-time.Minute)
		row.NextRunAt = &due
		require.NoError(t, s.UpdateSchedule(context.Background(), row, store.ScheduleFieldMask{NextRunAt: true},
			row.AuthorizationRevision, row.AuthorizationRevision != 0, nil))
		row, err = s.GetSchedule(context.Background(), created.ID)
		require.NoError(t, err)

		tickCtx, endTick := context.WithCancel(context.Background())
		var advancedDuringFire bool
		srv.scheduler.RegisterEventHandler("message", func(ctx context.Context, evt store.ScheduledEvent) error {
			cur, err := s.GetSchedule(context.Background(), created.ID)
			require.NoError(t, err)
			advancedDuringFire = cur.NextRunAt != nil && cur.NextRunAt.After(time.Now())
			endTick() // the fire outlasted the tick
			return assert.AnError
		})

		now := time.Now().UTC()
		srv.executeSchedule(tickCtx, *row, now)
		srv.scheduledWakeFires.wg.Wait() // a wake fire runs off the evaluator
		endTick()

		assert.Equal(t, wake, advancedDuringFire, "wake=%v: next run advanced before the handler", wake)
		got, err := s.GetSchedule(context.Background(), created.ID)
		require.NoError(t, err)
		if wake {
			assert.Equal(t, 1, got.RunCount, "the wake fire's run is recorded after the tick ended")
			assert.Equal(t, assert.AnError.Error(), got.LastRunError)
			events, err := s.ListScheduledEvents(context.Background(),
				store.ScheduledEventFilter{ProjectID: projectID, ScheduleID: created.ID}, store.ListOptions{})
			require.NoError(t, err)
			require.Len(t, events.Items, 1)
			assert.Equal(t, store.ScheduledEventFailed, events.Items[0].Status)
		}
	}
}

// TestScheduledMessageWake_TargetAlreadyStarting: a target another start
// has already moved to starting (review 2, finding 1 of ptone/scion#7769)
// is not resumed again: the fire waits for it to become ready and delivers,
// or fails visibly when it does not.
func TestScheduledMessageWake_TargetAlreadyStarting(t *testing.T) {
	t.Run("delivers once ready, with no second start", func(t *testing.T) {
		f := newSchedWake(t, state.PhaseStarting)
		u := f.user(t, "sw-starting-ok", lifecyclePerms...)
		_, err := f.store.ClaimAgentStart(context.Background(), f.target.ID, "other-hub", store.StartClaimUser, "", time.Minute)
		require.NoError(t, err)
		go func() {
			time.Sleep(200 * time.Millisecond)
			_ = f.store.UpdateAgentStatus(context.Background(), f.target.ID, store.AgentStatusUpdate{
				Phase: string(state.PhaseRunning), Activity: "idle",
			})
		}()

		require.NoError(t, f.fire(withSessionRevision(f.event(true), u.ID)))
		assert.Empty(t, f.disp.getStartCalls(), "the start in progress is not repeated")
		assert.Len(t, f.disp.getMessageCalls(), 1, "delivered once the target is ready")
	})
	t.Run("fails visibly when it never becomes ready", func(t *testing.T) {
		prev := wakeReadyTimeout
		wakeReadyTimeout = 300 * time.Millisecond
		t.Cleanup(func() { wakeReadyTimeout = prev })

		f := newSchedWake(t, state.PhaseStarting)
		u := f.user(t, "sw-starting-fail", lifecyclePerms...)

		err := f.fire(withSessionRevision(f.event(true), u.ID))
		require.ErrorIs(t, err, errScheduledWakeNotReady)
		assert.Empty(t, f.disp.getStartCalls())
		assert.Empty(t, f.disp.getMessageCalls())
	})
}

// TestScheduledMessageWake_OverlappingRecurringRunsKeepNextRun: a wake fire
// that finishes after a newer recurrence of the same schedule ran (review
// 2, finding 2 of ptone/scion#7769; SQLite has no evaluator lock) must not
// rewind the newer recurrence's next-run reservation or its last-run
// outcome, and no off-cadence fire becomes due.
func TestScheduledMessageWake_OverlappingRecurringRunsKeepNextRun(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	ctx := context.Background()
	rec := doRequest(t, srv, "POST", "/api/v1/projects/"+projectID+"/schedules", CreateScheduleRequest{
		Name: "overlap-wake", CronExpr: "* * * * *", EventType: "message",
		AgentName: "worker", Message: "poke", Wake: true,
	})
	require.Equal(t, 201, rec.Code, rec.Body.String())
	var row store.Schedule
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&row))

	now := time.Now().UTC().Truncate(time.Minute)
	later := now.Add(time.Minute)
	var calls atomic.Int32
	srv.scheduler.RegisterEventHandler("message", func(context.Context, store.ScheduledEvent) error {
		if calls.Add(1) == 1 {
			// The older run is still waking when the next minute's
			// evaluator selects the schedule and runs it to completion.
			due, err := s.ListDueSchedules(ctx, later)
			require.NoError(t, err)
			require.Len(t, due, 1, "the next recurrence is due")
			srv.executeSchedule(ctx, due[0], later)
			return assert.AnError // the older run then fails
		}
		return nil
	})

	srv.executeSchedule(ctx, row, now)
	srv.scheduledWakeFires.wg.Wait() // both fires run off the evaluator
	require.Equal(t, int32(2), calls.Load())

	got, err := s.GetSchedule(ctx, row.ID)
	require.NoError(t, err)
	require.NotNil(t, got.NextRunAt)
	assert.True(t, got.NextRunAt.After(later),
		"the older completion must not rewind next_run_at: got %s, newer run at %s", got.NextRunAt, later)
	require.NotNil(t, got.LastRunAt)
	assert.True(t, got.LastRunAt.Equal(later), "the newer run's outcome is kept: last run %s", got.LastRunAt)
	assert.Equal(t, store.ScheduleRunSuccess, got.LastRunStatus)
	assert.Empty(t, got.LastRunError)
	assert.Equal(t, 2, got.RunCount, "both runs are counted")
	assert.Equal(t, 1, got.ErrorCount, "the older run's failure is counted")

	due, err := s.ListDueSchedules(ctx, later.Add(30*time.Second))
	require.NoError(t, err)
	for _, d := range due {
		assert.NotEqual(t, row.ID, d.ID, "no off-cadence fire becomes due")
	}
}

// TestScheduledMessageWake_RecurringWakeFireDoesNotBlockEvaluator: a slow
// recurring wake fire runs off the schedule evaluator (review 3 of
// ptone/scion#7769), so another schedule due in the same tick fires at once
// and the evaluator returns while the wake is still running. The wake's
// outcome is recorded when it ends.
func TestScheduledMessageWake_RecurringWakeFireDoesNotBlockEvaluator(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	ctx := context.Background()
	create := func(name string, wake bool, due time.Time) store.Schedule {
		rec := doRequest(t, srv, "POST", "/api/v1/projects/"+projectID+"/schedules", CreateScheduleRequest{
			Name: name, CronExpr: "*/5 * * * *", EventType: "message",
			AgentName: "worker", Message: "poke", Wake: wake,
		})
		require.Equal(t, 201, rec.Code, rec.Body.String())
		var created store.Schedule
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&created))
		row, err := s.GetSchedule(ctx, created.ID)
		require.NoError(t, err)
		row.NextRunAt = &due
		require.NoError(t, s.UpdateSchedule(ctx, row, store.ScheduleFieldMask{NextRunAt: true},
			row.AuthorizationRevision, row.AuthorizationRevision != 0, nil))
		return *row
	}
	// The wake schedule is due first, so the evaluator reaches it first.
	slow := create("slow-wake", true, time.Now().UTC().Add(-2*time.Minute))
	other := create("other", false, time.Now().UTC().Add(-time.Minute))

	release := make(chan struct{})
	otherFired := make(chan struct{})
	srv.scheduler.RegisterEventHandler("message", func(_ context.Context, evt store.ScheduledEvent) error {
		if evt.ScheduleID == slow.ID {
			<-release // a wake that takes a long time
			return nil
		}
		close(otherFired)
		return nil
	})

	evaluated := make(chan struct{})
	go func() {
		srv.evaluateSchedulesHandler()(ctx)
		close(evaluated)
	}()
	select {
	case <-evaluated:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("the evaluator waited for the slow wake fire")
	}
	select {
	case <-otherFired:
	default:
		t.Fatal("the other due schedule did not fire in the same tick")
	}
	got, err := s.GetSchedule(ctx, other.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, got.RunCount, "the other schedule's run is recorded")
	got, err = s.GetSchedule(ctx, slow.ID)
	require.NoError(t, err)
	assert.Equal(t, 0, got.RunCount, "the wake fire is still running")
	require.NotNil(t, got.NextRunAt)
	assert.True(t, got.NextRunAt.After(time.Now()), "its next run is already reserved")

	close(release)
	srv.scheduledWakeFires.wg.Wait()
	got, err = s.GetSchedule(ctx, slow.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, got.RunCount, "the wake fire's run is recorded when it ends")
	assert.Equal(t, store.ScheduleRunSuccess, got.LastRunStatus)
}

// TestScheduledMessageWake_RecurringWakeFireRefusedWhileShuttingDown: once
// the hub is shutting down a recurring wake fire is not started; its event
// and the schedule record the failure, and shutdown joins fires already
// running.
func TestScheduledMessageWake_RecurringWakeFireRefusedWhileShuttingDown(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	ctx := context.Background()
	rec := doRequest(t, srv, "POST", "/api/v1/projects/"+projectID+"/schedules", CreateScheduleRequest{
		Name: "late-wake", CronExpr: "*/5 * * * *", EventType: "message",
		AgentName: "worker", Message: "poke", Wake: true,
	})
	require.Equal(t, 201, rec.Code, rec.Body.String())
	var created store.Schedule
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&created))
	row, err := s.GetSchedule(ctx, created.ID)
	require.NoError(t, err)

	var calls atomic.Int32
	srv.scheduler.RegisterEventHandler("message", func(context.Context, store.ScheduledEvent) error {
		calls.Add(1)
		return nil
	})
	srv.scheduledWakeFires.closeAdmission()

	srv.executeSchedule(ctx, *row, time.Now().UTC())
	assert.Zero(t, calls.Load(), "no wake fire starts once shutdown began")
	got, err := s.GetSchedule(ctx, row.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, got.RunCount)
	assert.Equal(t, store.ScheduleRunError, got.LastRunStatus)
	assert.Contains(t, got.LastRunError, "shutting down")
	events, err := s.ListScheduledEvents(ctx, store.ScheduledEventFilter{ProjectID: projectID, ScheduleID: row.ID}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, events.Items, 1)
	assert.Equal(t, store.ScheduledEventFailed, events.Items[0].Status)
	assert.Zero(t, srv.scheduledWakeFires.stop(ctx, time.Second), "nothing left to join")
}

// createDueWakeTestSchedule creates a message schedule through the API and
// makes it due at due.
func createDueWakeTestSchedule(t *testing.T, srv *Server, s store.Store, projectID, name string, wake bool, due time.Time) store.Schedule {
	t.Helper()
	ctx := context.Background()
	rec := doRequest(t, srv, "POST", "/api/v1/projects/"+projectID+"/schedules", CreateScheduleRequest{
		Name: name, CronExpr: "*/5 * * * *", EventType: "message",
		AgentName: "worker", Message: "poke", Wake: wake,
	})
	require.Equal(t, 201, rec.Code, rec.Body.String())
	var created store.Schedule
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&created))
	row, err := s.GetSchedule(ctx, created.ID)
	require.NoError(t, err)
	row.NextRunAt = &due
	require.NoError(t, s.UpdateSchedule(ctx, row, store.ScheduleFieldMask{NextRunAt: true},
		row.AuthorizationRevision, row.AuthorizationRevision != 0, nil))
	row, err = s.GetSchedule(ctx, created.ID)
	require.NoError(t, err)
	return *row
}

// TestScheduledMessageWake_RecurringWakeFirePanicIsContained: a panic in a
// recurring wake fire, which runs on its own goroutine outside the
// scheduler's recovery, is contained (review 4 of ptone/scion#7769): the hub
// keeps running, the event and the schedule record the failure, and other
// schedules still execute.
func TestScheduledMessageWake_RecurringWakeFirePanicIsContained(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	ctx := context.Background()
	past := time.Now().UTC().Add(-time.Minute)
	panicky := createDueWakeTestSchedule(t, srv, s, projectID, "panicky-wake", true, past)
	other := createDueWakeTestSchedule(t, srv, s, projectID, "after-panic", false, past)

	var otherRan atomic.Int32
	srv.scheduler.RegisterEventHandler("message", func(_ context.Context, evt store.ScheduledEvent) error {
		if evt.ScheduleID == panicky.ID {
			panic("wake handler exploded")
		}
		otherRan.Add(1)
		return nil
	})

	srv.executeSchedule(ctx, panicky, time.Now().UTC())
	srv.scheduledWakeFires.wg.Wait()

	got, err := s.GetSchedule(ctx, panicky.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, got.RunCount)
	assert.Equal(t, store.ScheduleRunError, got.LastRunStatus)
	assert.Contains(t, got.LastRunError, "handler panicked: wake handler exploded")
	events, err := s.ListScheduledEvents(ctx, store.ScheduledEventFilter{ProjectID: projectID, ScheduleID: panicky.ID}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, events.Items, 1)
	assert.Equal(t, store.ScheduledEventFailed, events.Items[0].Status)
	assert.Contains(t, events.Items[0].Error, "handler panicked")

	// The hub is still up: another schedule executes.
	srv.executeSchedule(ctx, other, time.Now().UTC())
	assert.Equal(t, int32(1), otherRan.Load())
	got, err = s.GetSchedule(ctx, other.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, got.RunCount)
	assert.Equal(t, store.ScheduleRunSuccess, got.LastRunStatus)
}

// TestScheduledMessageWake_RecurringWakeFireEventNotReplayable: while a
// recurring wake fire runs, its materialized event is already claimed, so
// a hub restart or another replica starting meanwhile does not replay it
// from the pending events (loadPersistedTimers): no late duplicate wake
// and delivery (review 4 of ptone/scion#7769). The outcome then sets the
// final status.
func TestScheduledMessageWake_RecurringWakeFireEventNotReplayable(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	ctx := context.Background()
	sched := createDueWakeTestSchedule(t, srv, s, projectID, "slow-claimed-wake", true, time.Now().UTC().Add(-time.Minute))

	running := make(chan string, 1)
	release := make(chan struct{})
	srv.scheduler.RegisterEventHandler("message", func(_ context.Context, evt store.ScheduledEvent) error {
		running <- evt.ID
		<-release
		return nil
	})

	srv.executeSchedule(ctx, sched, time.Now().UTC())
	var eventID string
	select {
	case eventID = <-running:
	case <-time.After(5 * time.Second):
		t.Fatal("the wake fire did not start")
	}

	// What a restarting hub or a starting replica would replay.
	pending, err := s.ListPendingScheduledEvents(ctx)
	require.NoError(t, err)
	for _, p := range pending {
		assert.NotEqual(t, eventID, p.ID, "a running wake fire's event is not pending")
	}
	evt, err := s.GetScheduledEvent(ctx, eventID)
	require.NoError(t, err)
	assert.Equal(t, store.ScheduledEventFired, evt.Status, "claimed before the fire runs")
	if claimer, ok := s.(store.ScheduledEventClaimer); ok {
		claimed, err := claimer.ClaimScheduledEvent(ctx, eventID, store.ScheduledEventExpired)
		require.NoError(t, err)
		assert.False(t, claimed, "a replay cannot claim it again")
	}

	close(release)
	srv.scheduledWakeFires.wg.Wait()
	evt, err = s.GetScheduledEvent(ctx, eventID)
	require.NoError(t, err)
	assert.Equal(t, store.ScheduledEventFired, evt.Status)
	assert.Empty(t, evt.Error)
}
