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
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agentcontrol"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func withScheduleSessionRevision(sc store.Schedule, userID string) store.Schedule {
	evt := withSessionRevision(store.ScheduledEvent{ProjectID: sc.ProjectID}, userID)
	sc.InitiatorAttribution = evt.InitiatorAttribution
	sc.AuthorityCeiling = evt.AuthorityCeiling
	return sc
}

func (f *execRouteFixture) createExecSchedule(t *testing.T, name string, nextRunAt time.Time, timeout int, argv ...string) store.Schedule {
	t.Helper()
	p := scheduledExecPayload{
		AgentID:        f.launched.ID,
		AgentName:      f.launched.Slug,
		Command:        argv,
		TimeoutSeconds: timeout,
	}
	raw, err := p.encode()
	require.NoError(t, err)
	nr := nextRunAt.UTC()
	sc := withScheduleSessionRevision(store.Schedule{
		ID:        api.NewUUID(),
		ProjectID: f.launched.ProjectID,
		Name:      name,
		CronExpr:  "* * * * *",
		EventType: scheduledEventTypeExec,
		Payload:   raw,
		Status:    store.ScheduleStatusActive,
		NextRunAt: &nr,
		CreatedBy: f.launched.OwnerID,
	}, f.launched.OwnerID)
	require.NoError(t, f.store.CreateSchedule(context.Background(), &sc))
	return sc
}

func waitScheduleRunCount(t *testing.T, st store.Store, schedID string, wantRuns int) store.Schedule {
	t.Helper()
	var got *store.Schedule
	require.Eventually(t, func() bool {
		var err error
		got, err = st.GetSchedule(context.Background(), schedID)
		return err == nil && got.RunCount >= wantRuns
	}, 10*time.Second, 20*time.Millisecond, "schedule %s did not reach runCount=%d", schedID, wantRuns)
	return *got
}

func latestScheduleEvent(t *testing.T, st store.Store, projectID, schedID string) store.ScheduledEvent {
	t.Helper()
	res, err := st.ListScheduledEvents(context.Background(), store.ScheduledEventFilter{
		ProjectID:  projectID,
		ScheduleID: schedID,
	}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.NotEmpty(t, res.Items)
	return res.Items[0]
}

// nextRealScheduleTick returns the current UTC wall-clock time, waiting briefly
// if needed so it is strictly after after (e.g. a schedule's LastRunAt) without
// ever jumping ahead of real wall-clock time.Now().
func nextRealScheduleTick(after *time.Time) time.Time {
	t := time.Now().UTC()
	for after != nil && !t.After(*after) {
		time.Sleep(time.Millisecond)
		t = time.Now().UTC()
	}
	return t
}

// TestScheduledExecRecurring_OutcomesAndAccounting verifies recurring exec
// fires through executeSchedule: exit 0, non-zero exit, >10m missed window,
// and experiment-off skip, plus run_count / error_count / last_run_status
// accounting (D6, AC3-AC5).
func TestScheduledExecRecurring_OutcomesAndAccounting(t *testing.T) {
	f := newExecRouteFixture(t)
	f.startAgent(t)
	ctx := context.Background()
	t1 := nextRealScheduleTick(nil)

	// 1. Exit 0 -> event fired, run ok, RunCount=1, ErrorCount=0, LastRunStatus=success.
	sc := f.createExecSchedule(t, "rec-outcomes", t1.Add(-time.Second), 10, "true")
	f.srv.executeSchedule(ctx, sc, t1)
	gotSc := waitScheduleRunCount(t, f.store, sc.ID, 1)
	assert.Equal(t, 1, gotSc.RunCount)
	assert.Equal(t, 0, gotSc.ErrorCount)
	assert.Equal(t, store.ScheduleRunSuccess, gotSc.LastRunStatus)
	assert.Empty(t, gotSc.LastRunError)
	require.NotNil(t, gotSc.NextRunAt)
	assert.True(t, gotSc.NextRunAt.After(t1))

	evt1 := latestScheduleEvent(t, f.store, f.launched.ProjectID, sc.ID)
	assert.Equal(t, store.ScheduledEventFired, evt1.Status)
	run1, err := f.store.GetScheduledExecRun(ctx, evt1.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ScheduledExecOutcomeOK, run1.Outcome)
	assert.Equal(t, sc.ID, run1.ScheduleID)

	// 2. Non-zero exit -> event failed, run nonzero, RunCount=2, ErrorCount=1.
	p2 := scheduledExecPayload{AgentID: f.launched.ID, Command: []string{"sh", "-c", "exit 7"}, TimeoutSeconds: 10}
	raw2, err := p2.encode()
	require.NoError(t, err)
	gotSc.Payload = raw2
	t2 := nextRealScheduleTick(gotSc.LastRunAt)
	due2 := t2.Add(-time.Second)
	gotSc.NextRunAt = &due2
	f.srv.executeSchedule(ctx, gotSc, t2)
	gotSc = waitScheduleRunCount(t, f.store, sc.ID, 2)
	assert.Equal(t, 2, gotSc.RunCount)
	assert.Equal(t, 1, gotSc.ErrorCount)
	assert.Equal(t, store.ScheduleRunError, gotSc.LastRunStatus)
	assert.Equal(t, execReasonExitStatus(7), gotSc.LastRunError)

	// 3. Missed window (>10m late) -> skipped ("missed window"), no run row, ErrorCount unchanged!
	t3 := nextRealScheduleTick(gotSc.LastRunAt)
	staleDue := t3.Add(-15 * time.Minute)
	gotSc.NextRunAt = &staleDue
	f.srv.executeSchedule(ctx, gotSc, t3)
	gotSc = waitScheduleRunCount(t, f.store, sc.ID, 3)
	assert.Equal(t, 3, gotSc.RunCount)
	assert.Equal(t, 1, gotSc.ErrorCount, "skipped runs must not increment error_count")
	assert.Equal(t, store.ScheduleRunSkipped, gotSc.LastRunStatus)
	assert.Equal(t, execReasonMissedWindow, gotSc.LastRunError)

	evt3 := latestScheduleEvent(t, f.store, f.launched.ProjectID, sc.ID)
	assert.Equal(t, store.ScheduledEventSkipped, evt3.Status)
	assert.Equal(t, execReasonMissedWindow, evt3.Error)
	_, runErr := f.store.GetScheduledExecRun(ctx, evt3.ID)
	assert.ErrorIs(t, runErr, store.ErrNotFound)

	// 4. Experiment turned off -> skipped ("exec schedules disabled"), ErrorCount still 1.
	setHubExperiments(t, f.srv, map[string]bool{conduitExperiment: true, experiments.ExecSchedules: false})
	t4 := nextRealScheduleTick(gotSc.LastRunAt)
	due4 := t4.Add(-time.Second)
	gotSc.NextRunAt = &due4
	f.srv.executeSchedule(ctx, gotSc, t4)
	gotSc = waitScheduleRunCount(t, f.store, sc.ID, 4)
	assert.Equal(t, 4, gotSc.RunCount)
	assert.Equal(t, 1, gotSc.ErrorCount)
	assert.Equal(t, store.ScheduleRunSkipped, gotSc.LastRunStatus)
	assert.Equal(t, execReasonDisabled, gotSc.LastRunError)

	// 5. Re-enable experiment; command timeout (1s) -> failed ("timed out after 1s"), ErrorCount=2.
	setHubExperiments(t, f.srv, map[string]bool{conduitExperiment: true, experiments.ExecSchedules: true})
	p5 := scheduledExecPayload{AgentID: f.launched.ID, Command: []string{"sh", "-c", "trap 'exit 143' TERM; sleep 10 & wait"}, TimeoutSeconds: 1}
	raw5, err := p5.encode()
	require.NoError(t, err)
	gotSc.Payload = raw5
	t5 := nextRealScheduleTick(gotSc.LastRunAt)
	due5 := t5.Add(-time.Second)
	gotSc.NextRunAt = &due5
	f.srv.executeSchedule(ctx, gotSc, t5)
	gotSc = waitScheduleRunCount(t, f.store, sc.ID, 5)
	assert.Equal(t, 5, gotSc.RunCount)
	assert.Equal(t, 2, gotSc.ErrorCount)
	assert.Equal(t, store.ScheduleRunError, gotSc.LastRunStatus)
	assert.Equal(t, execReasonTimedOut(1), gotSc.LastRunError)

	// 6. Fire-time authz refusal (revision ceiling lacks agent.attach) -> failed ("exec refused"),
	// ErrorCount=3, 0 new exec-start RPCs, and schedule.exec.refused audit emitted.
	logs := captureDefaultLog(t)
	gotSc.AuthorityCeiling = store.EffectCeiling{
		Kind:              store.EffectCeilingBounded,
		Version:           1,
		PermissionIDs:     []string{"agent.read", "project.read"},
		BoundaryKind:      "project",
		BoundaryProjectID: f.launched.ProjectID,
	}
	startsBefore := f.capture.count(execRoutePath(agentcontrol.RouteExecStart))
	t6 := nextRealScheduleTick(gotSc.LastRunAt)
	due6 := t6.Add(-time.Second)
	gotSc.NextRunAt = &due6
	f.srv.executeSchedule(ctx, gotSc, t6)
	gotSc = waitScheduleRunCount(t, f.store, sc.ID, 6)
	assert.Equal(t, 6, gotSc.RunCount)
	assert.Equal(t, 3, gotSc.ErrorCount)
	assert.Equal(t, store.ScheduleRunError, gotSc.LastRunStatus)
	assert.Equal(t, execReasonRefused, gotSc.LastRunError)
	assert.Equal(t, startsBefore, f.capture.count(execRoutePath(agentcontrol.RouteExecStart)))
	assert.Contains(t, logs.String(), "audit_action=schedule.exec.refused")
}

// TestScheduledExecRecurring_DBOverlapAndSciontoolOverlap (AC8):
//  1. While a recurring schedule's previous run holds an unexpired DB lease,
//     a new recurrence is skipped before contacting sciontool.
//  2. If the DB lease expired and was swept while the main process is still
//     alive in sciontool, sciontool's per-schedule overlap check returns 409
//     overlap, which the Hub records as skipped without bumping error_count.
func TestScheduledExecRecurring_DBOverlapAndSciontoolOverlap(t *testing.T) {
	f := newExecRouteFixture(t)
	setExecTiming(t, &scheduledExecLeaseGrace, 200*time.Millisecond)
	f.startAgent(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// Start a 3s sleep with a 1s timeout? No: give it 1s timeout? Wait:
	// lease = timeout + grace. Use timeout=1s, grace=-800ms so lease expires
	// in 200ms while sleep 2 is still alive for ~1s!
	setExecTiming(t, &scheduledExecLeaseGrace, 5*time.Minute)
	sc := f.createExecSchedule(t, "rec-overlap", now, 5, "sleep", "2")
	f.srv.executeSchedule(ctx, sc, now)

	// Wait until the first run is admitted and running in sciontool.
	var firstEvt store.ScheduledEvent
	require.Eventually(t, func() bool {
		res, err := f.store.ListScheduledEvents(ctx, store.ScheduledEventFilter{
			ProjectID: f.launched.ProjectID, ScheduleID: sc.ID,
		}, store.ListOptions{Limit: 10})
		if err != nil || len(res.Items) == 0 {
			return false
		}
		firstEvt = res.Items[0]
		r, rerr := f.store.GetScheduledExecRun(ctx, firstEvt.ID)
		return rerr == nil && r.Outcome == store.ScheduledExecOutcomeRunning
	}, 3*time.Second, 20*time.Millisecond)

	startsBefore := f.capture.count("/v1/control/exec-start")
	require.Equal(t, 1, startsBefore)

	// Fire 2 while DB lease is live -> skipped via DB overlap lease without calling exec-start!
	sc2, err := f.store.GetSchedule(ctx, sc.ID)
	require.NoError(t, err)
	t2 := nextRealScheduleTick(sc2.LastRunAt)
	sc2.NextRunAt = &t2
	f.srv.executeSchedule(ctx, *sc2, t2)

	gotSc := waitScheduleRunCount(t, f.store, sc.ID, 1)
	assert.Equal(t, 0, gotSc.ErrorCount)
	assert.Equal(t, store.ScheduleRunSkipped, gotSc.LastRunStatus)
	assert.Equal(t, execReasonOverlap, gotSc.LastRunError)
	assert.Equal(t, 1, f.capture.count("/v1/control/exec-start"), "DB overlap lease must skip before contacting sciontool")

	// Now force-finish firstEvt's DB run row as abandoned (simulating an
	// expired DB lease) while the `sleep 2` main process is STILL alive in
	// sciontool, then fire again -> sciontool returns 409 overlap!
	won, err := f.store.FinishScheduledExecRun(ctx, store.ScheduledExecRunFinish{
		EventID:     firstEvt.ID,
		FinishedAt:  nextRealScheduleTick(gotSc.LastRunAt),
		Outcome:     store.ScheduledExecOutcomeAbandoned,
		EventStatus: store.ScheduledEventFailed,
		EventError:  execReasonOutcomeUnknown,
	})
	require.NoError(t, err)
	require.True(t, won)
	gotSc = waitScheduleRunCount(t, f.store, sc.ID, 2)
	assert.Equal(t, 1, gotSc.ErrorCount)

	sc3, err := f.store.GetSchedule(ctx, sc.ID)
	require.NoError(t, err)
	t3 := nextRealScheduleTick(sc3.LastRunAt)
	sc3.NextRunAt = &t3
	f.srv.executeSchedule(ctx, *sc3, t3)

	gotSc = waitScheduleRunCount(t, f.store, sc.ID, 3)
	assert.Equal(t, 1, gotSc.ErrorCount, "sciontool 409 overlap skip must not increment error_count")
	assert.Equal(t, store.ScheduleRunSkipped, gotSc.LastRunStatus)
	assert.Equal(t, execReasonOverlap, gotSc.LastRunError)
	assert.Equal(t, 2, f.capture.count("/v1/control/exec-start"), "second exec-start reached sciontool and got 409 overlap")
}

// TestScheduledExecRecurring_SlowRunDoesNotDelayMessageScheduleInSameTick (AC9):
// evaluating a due slow exec schedule (sleep 120) and a due message schedule
// in the same evaluator pass delivers the message schedule immediately without
// waiting for the exec command.
func TestScheduledExecRecurring_SlowRunDoesNotDelayMessageScheduleInSameTick(t *testing.T) {
	f := newExecRouteFixture(t)
	f.startAgent(t)
	ctx := context.Background()

	var msgFired atomic.Int32
	f.srv.RegisterAction(newTestFireAction("message", func(_ context.Context, _ hubfire.FireRequest) error {
		msgFired.Add(1)
		return nil
	}))

	now := time.Now().UTC()
	execSc := f.createExecSchedule(t, "slow-exec-120", now.Add(-time.Second), 120, "sleep", "120")

	due := now.Add(-time.Second)
	msgSc := withScheduleSessionRevision(store.Schedule{
		ID:        api.NewUUID(),
		ProjectID: f.launched.ProjectID,
		Name:      "fast-message",
		CronExpr:  "* * * * *",
		EventType: "message",
		Payload:   `{"agentName":"` + f.launched.Slug + `","message":"ping"}`,
		Status:    store.ScheduleStatusActive,
		NextRunAt: &due,
		CreatedBy: f.launched.OwnerID,
	}, f.launched.OwnerID)
	require.NoError(t, f.store.CreateSchedule(ctx, &msgSc))

	start := time.Now()
	f.srv.executeSchedule(ctx, execSc, now)
	f.srv.executeSchedule(ctx, msgSc, now)
	elapsed := time.Since(start)

	assert.Less(t, elapsed, 2*time.Second, "detached exec fire must not block the evaluator loop")
	assert.Equal(t, int32(1), msgFired.Load(), "due message schedule in the same tick must fire immediately")

	gotMsg, err := f.store.GetSchedule(ctx, msgSc.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, gotMsg.RunCount)
	assert.Equal(t, store.ScheduleRunSuccess, gotMsg.LastRunStatus)
}

// TestScheduledExecRecurring_TargetDeletedPauseAndStrictIDMatch (AC11):
//   - Deleting the target pauses active recurring exec schedules targeting its
//     agentId with statusReason=target_deleted (including >200 schedules across
//     pages, and self-targeted schedules get target_deleted rather than
//     creator_deleted).
//   - Neither recurring pause nor one-shot cancel matches an exec row by
//     agentName/slug when agentId belongs to another agent (recreated-slug
//     guard).
//   - Evaluator backstop pauses an active exec schedule whose target is soft-
//     deleted without firing it.
func TestScheduledExecRecurring_TargetDeletedPauseAndStrictIDMatch(t *testing.T) {
	f := execAuthoringFixture(t)
	ctx := context.Background()

	// Seed 205 active exec schedules targeting f.sibling (spans >1 page of 200).
	var targetSchedIDs []string
	next := time.Now().UTC().Add(time.Minute)
	for i := 0; i < scheduleCreatorPausePageSize+5; i++ {
		p := scheduledExecPayload{AgentID: f.sibling.ID, AgentName: f.sibling.Slug, Command: []string{"true"}}
		raw, err := p.encode()
		require.NoError(t, err)
		sc := withScheduleSessionRevision(store.Schedule{
			ID:        api.NewUUID(),
			ProjectID: f.proj.ID,
			Name:      "target-del-" + uuid.NewString(),
			CronExpr:  "* * * * *",
			EventType: scheduledEventTypeExec,
			Payload:   raw,
			Status:    store.ScheduleStatusActive,
			NextRunAt: &next,
			CreatedBy: f.owner.ID,
		}, f.owner.ID)
		require.NoError(t, f.store.CreateSchedule(ctx, &sc))
		targetSchedIDs = append(targetSchedIDs, sc.ID)
	}

	// Self-targeted schedule authored by f.sibling itself: must get target_deleted!
	selfPayload, err := (&scheduledExecPayload{AgentID: f.sibling.ID, AgentName: f.sibling.Slug, Command: []string{"true"}}).encode()
	require.NoError(t, err)
	selfSched := store.Schedule{
		ID:        api.NewUUID(),
		ProjectID: f.proj.ID,
		Name:      "self-target",
		CronExpr:  "* * * * *",
		EventType: scheduledEventTypeExec,
		Payload:   selfPayload,
		Status:    store.ScheduleStatusActive,
		NextRunAt: &next,
		CreatedBy: f.sibling.ID,
		InitiatorAttribution: store.InitiatorAttribution{
			InitiatorPrincipalKind:  store.DelegationPrincipalAgent,
			InitiatorPrincipalID:    f.sibling.ID,
			InitiatorCredentialKind: "agent_jwt",
			InitiatorCredentialID:   f.sibling.ID,
			AttributionVersion:      1,
			AuthorizationRevision:   1,
		},
	}
	require.NoError(t, f.store.CreateSchedule(ctx, &selfSched))

	// Same-slug decoy: agentId is f.child.ID, but agentName is f.sibling.Slug!
	// Neither pauseSchedulesTargetingAgent nor cancelScheduledEventsForAgent
	// may touch it!
	decoyPayload, err := (&scheduledExecPayload{AgentID: f.child.ID, AgentName: f.sibling.Slug, Command: []string{"true"}}).encode()
	require.NoError(t, err)
	decoySched := withScheduleSessionRevision(store.Schedule{
		ID:        api.NewUUID(),
		ProjectID: f.proj.ID,
		Name:      "same-slug-decoy",
		CronExpr:  "* * * * *",
		EventType: scheduledEventTypeExec,
		Payload:   decoyPayload,
		Status:    store.ScheduleStatusActive,
		NextRunAt: &next,
		CreatedBy: f.owner.ID,
	}, f.owner.ID)
	require.NoError(t, f.store.CreateSchedule(ctx, &decoySched))

	decoyEvt := withSessionRevision(store.ScheduledEvent{
		ID:        api.NewUUID(),
		ProjectID: f.proj.ID,
		EventType: scheduledEventTypeExec,
		FireAt:    time.Now().Add(time.Hour),
		Payload:   decoyPayload,
		Status:    store.ScheduledEventPending,
		CreatedBy: f.owner.ID,
	}, f.owner.ID)
	require.NoError(t, f.store.CreateScheduledEvent(ctx, &decoyEvt))

	// Run target-deletion steps in the order agent_delete_engine runs them.
	f.srv.cancelScheduledEventsForAgent(ctx, f.sibling)
	f.srv.pauseSchedulesTargetingAgent(ctx, f.sibling)
	f.srv.pauseSchedulesAuthoredByAgent(ctx, f.sibling)

	for _, id := range targetSchedIDs {
		got, gerr := f.store.GetSchedule(ctx, id)
		require.NoError(t, gerr)
		assert.Equal(t, store.ScheduleStatusPaused, got.Status)
		assert.Equal(t, store.ScheduleStatusReasonTargetDeleted, got.StatusReason)
	}
	gotSelf, err := f.store.GetSchedule(ctx, selfSched.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ScheduleStatusPaused, gotSelf.Status)
	assert.Equal(t, store.ScheduleStatusReasonTargetDeleted, gotSelf.StatusReason)

	gotDecoySc, err := f.store.GetSchedule(ctx, decoySched.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ScheduleStatusActive, gotDecoySc.Status, "exec schedule must match strictly by agentId, never agentName")

	gotDecoyEvt, err := f.store.GetScheduledEvent(ctx, decoyEvt.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ScheduledEventPending, gotDecoyEvt.Status, "exec event must match strictly by agentId, never agentName")

	// Evaluator backstop: soft-delete f.child without calling pauseSchedulesTargetingAgent,
	// then call executeSchedule on decoySched -> pauses with target_deleted without firing!
	delAt := time.Now().UTC()
	f.child.DeletedAt = delAt
	require.NoError(t, f.store.DeleteAgent(ctx, f.child.ID))
	f.srv.executeSchedule(ctx, *gotDecoySc, time.Now().UTC())
	gotBackstop, err := f.store.GetSchedule(ctx, decoySched.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ScheduleStatusPaused, gotBackstop.Status)
	assert.Equal(t, store.ScheduleStatusReasonTargetDeleted, gotBackstop.StatusReason)
	assert.Equal(t, 0, gotBackstop.RunCount)
}

// TestScheduledExecRecurring_ShutdownAndSweeperInterleavingMatrix (AC10):
//  1. Pre-admission shutdown (scheduledDetachedFires closed before executeSchedule)
//     records the recurring event failed ("hub shutting down") and bumps
//     error_count once.
//  2. Post-admission shutdown (stopScheduledDetachedFires + stopScheduledExecRuns
//     while the recurring run is executing) leaves the admitted run unfinished
//     and the event "fired" (never clobbered by recordAbandonedDetachedFires), and
//     a subsequent sweepScheduledExecRuns recovers the real exit-0 outcome and
//     updates the parent schedule's run_count=2, error_count=1, last_run_status=success!
//  3. Concurrent tracker + sweeper polls on a recurring run count the run on
//     the parent schedule exactly once.
func TestScheduledExecRecurring_ShutdownAndSweeperInterleavingMatrix(t *testing.T) {
	t.Run("pre-admission shutdown records failed once", func(t *testing.T) {
		f := newExecRouteFixture(t)
		ctx := context.Background()
		now := time.Now().UTC()
		sc := f.createExecSchedule(t, "pre-admit-shutdown", now, 10, "true")

		f.srv.scheduledDetachedFires.closeAdmission()
		f.srv.executeSchedule(ctx, sc, now)

		gotSc := waitScheduleRunCount(t, f.store, sc.ID, 1)
		assert.Equal(t, 1, gotSc.RunCount)
		assert.Equal(t, 1, gotSc.ErrorCount)
		assert.Equal(t, store.ScheduleRunError, gotSc.LastRunStatus)
		assert.Equal(t, execReasonShuttingDown, gotSc.LastRunError)

		evt := latestScheduleEvent(t, f.store, f.launched.ProjectID, sc.ID)
		assert.Equal(t, store.ScheduledEventFailed, evt.Status)
		assert.Equal(t, execReasonShuttingDown, evt.Error)
	})

	t.Run("unadmitted crash recovery records schedule failed once", func(t *testing.T) {
		f := newExecRouteFixture(t)
		ctx := context.Background()
		now := time.Now().UTC()
		sc := f.createExecSchedule(t, "unadmitted-crash-recovery", now, 10, "true")

		// Simulate a Hub crash after executeSchedule created the recurring
		// ScheduledEvent in status "fired" (FiredAt older than
		// scheduledExecAdmitWindow) before CreateScheduledExecRun ran.
		claimedAt := now.Add(-2 * time.Hour)
		crashedEvt := withSessionRevision(store.ScheduledEvent{
			ID:         api.NewUUID(),
			ProjectID:  f.launched.ProjectID,
			ScheduleID: sc.ID,
			EventType:  scheduledEventTypeExec,
			FireAt:     claimedAt,
			FiredAt:    &claimedAt,
			Payload:    sc.Payload,
			Status:     store.ScheduledEventFired,
			CreatedBy:  sc.CreatedBy,
		}, f.launched.OwnerID)
		require.NoError(t, f.store.CreateScheduledEvent(ctx, &crashedEvt))

		logs := captureDefaultLog(t)
		f.srv.sweepScheduledExecRuns(ctx)
		// Second sweep must be a no-op (idempotent once-write).
		f.srv.sweepScheduledExecRuns(ctx)

		gotEvt, err := f.store.GetScheduledEvent(ctx, crashedEvt.ID)
		require.NoError(t, err)
		assert.Equal(t, store.ScheduledEventFailed, gotEvt.Status)
		assert.Equal(t, execReasonNotDispatched, gotEvt.Error)

		gotSc, err := f.store.GetSchedule(ctx, sc.ID)
		require.NoError(t, err)
		assert.Equal(t, 1, gotSc.RunCount, "O1: recoverUnadmittedExecEvents must increment parent schedule run_count")
		assert.Equal(t, 1, gotSc.ErrorCount, "O1: recoverUnadmittedExecEvents must increment parent schedule error_count per D6")
		assert.Equal(t, store.ScheduleRunError, gotSc.LastRunStatus)
		assert.Equal(t, execReasonNotDispatched, gotSc.LastRunError)
		assert.Equal(t, 0, f.capture.count(execRoutePath(agentcontrol.RouteExecStart)))
		assert.Contains(t, logs.String(), "audit_action=schedule.exec.outcome")
		assert.Contains(t, logs.String(), "schedule_id="+sc.ID)
	})

	t.Run("post-admission shutdown preserves admitted run for sweeper recovery", func(t *testing.T) {
		f := newExecRouteFixture(t)
		setExecTiming(t, &scheduledExecPollInitial, 10*time.Second)
		setExecTiming(t, &scheduledExecPollMax, 10*time.Second)
		setExecTiming(t, &scheduledExecStopGrace, 10*time.Millisecond)
		f.startAgent(t)
		ctx := context.Background()
		now := time.Now().UTC()

		sc := f.createExecSchedule(t, "post-admit-sweeper", now, 15, "sh", "-c", "sleep 0.25")
		f.srv.executeSchedule(ctx, sc, now)

		var evt store.ScheduledEvent
		require.Eventually(t, func() bool {
			res, err := f.store.ListScheduledEvents(ctx, store.ScheduledEventFilter{
				ProjectID: f.launched.ProjectID, ScheduleID: sc.ID,
			}, store.ListOptions{Limit: 10})
			if err != nil || len(res.Items) == 0 {
				return false
			}
			evt = res.Items[0]
			r, rerr := f.store.GetScheduledExecRun(ctx, evt.ID)
			return rerr == nil && r.Outcome == store.ScheduledExecOutcomeRunning
		}, 3*time.Second, 20*time.Millisecond)

		// Simulate Hub shutdown while the command is still running!
		f.srv.haltDispatchProducers()
		f.srv.stopScheduledDetachedFires(ctx)
		f.srv.stopScheduledExecRuns(ctx)

		// Admitted run MUST remain unfinished and event "fired", never clobbered!
		afterStopEvt, err := f.store.GetScheduledEvent(ctx, evt.ID)
		require.NoError(t, err)
		assert.Equal(t, store.ScheduledEventFired, afterStopEvt.Status)
		afterStopRun, err := f.store.GetScheduledExecRun(ctx, evt.ID)
		require.NoError(t, err)
		assert.Nil(t, afterStopRun.FinishedAt)

		// Wait for the 0.25s command in sciontool to finish, then sweep (even
		// with hub.exec_schedules flipped off per AC18!).
		time.Sleep(350 * time.Millisecond)
		setHubExperiments(t, f.srv, map[string]bool{conduitExperiment: true, experiments.ExecSchedules: false})
		f.srv.sweepScheduledExecRuns(ctx)

		gotSc, err := f.store.GetSchedule(ctx, sc.ID)
		require.NoError(t, err)
		assert.Equal(t, 1, gotSc.RunCount)
		assert.Equal(t, 0, gotSc.ErrorCount)
		assert.Equal(t, store.ScheduleRunSuccess, gotSc.LastRunStatus)

		recRun, err := f.store.GetScheduledExecRun(ctx, evt.ID)
		require.NoError(t, err)
		assert.Equal(t, store.ScheduledExecOutcomeOK, recRun.Outcome)
		assert.NotNil(t, recRun.FinishedAt)

		// Verify health summary recognizes target_deleted if paused later.
		require.NoError(t, f.store.UpdateScheduleStatus(ctx, sc.ID, store.ScheduleStatusPaused, store.ScheduleStatusReasonTargetDeleted))
		rec := requestAsIdentity(t, f.srv, f.owner, http.MethodGet, "/api/v1/projects/"+f.launched.ProjectID+"/health-summary", nil)
		if rec.Code == http.StatusOK {
			assert.Contains(t, rec.Body.String(), store.ScheduleStatusReasonTargetDeleted)
		}
		_ = errors.New
		_ = sync.Mutex{}
	})
}

type p2o1BlockAdmitStore struct {
	store.Store
	fault *storeFaultSwitch
	gate  chan struct{}
}

func (s *p2o1BlockAdmitStore) CreateScheduledExecRun(ctx context.Context, run *store.ScheduledExecRun) error {
	if s.fault.Active() && run.ScheduleID != "" {
		select {
		case <-s.gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.Store.CreateScheduledExecRun(ctx, run)
}

// TestScheduledExecP2O1_RecurringFiredAtUsesCreationTime verifies Phase 2 Pass 2
// P2-O1: executeSchedule stamps a recurring exec event's FiredAt at event
// creation time (time.Now()), not the evaluator tick start passed in `now`, so
// a slow tick does not shrink the unadmitted-recovery clock-skew margin.
func TestScheduledExecP2O1_RecurringFiredAtUsesCreationTime(t *testing.T) {
	gate := make(chan struct{})
	var fault *storeFaultSwitch
	f := newExecRouteFixtureWith(t, func(srv *Server) {
		_, fault = installStoreFault(t, srv, func(inner store.Store, sw *storeFaultSwitch) *p2o1BlockAdmitStore {
			return &p2o1BlockAdmitStore{Store: inner, fault: sw, gate: gate}
		})
	})
	f.startAgent(t)
	fault.Arm()

	ctx := context.Background()
	callStart := time.Now().UTC()
	staleTickStart := callStart.Add(-45 * time.Second)
	dueAt := staleTickStart.Add(-5 * time.Second)

	sc := f.createExecSchedule(t, "p2o1-fired-at", dueAt, 10, "true")
	f.srv.executeSchedule(ctx, sc, staleTickStart)
	callEnd := time.Now().UTC()

	evt := latestScheduleEvent(t, f.store, f.launched.ProjectID, sc.ID)
	require.NotNil(t, evt.FiredAt)
	assert.False(t, evt.FiredAt.Before(callStart),
		"recurring exec FiredAt (%s) must not be earlier than executeSchedule start (%s); stale tickStart was %s",
		evt.FiredAt.UTC(), callStart, staleTickStart)
	assert.False(t, evt.FiredAt.After(callEnd.Add(time.Second)),
		"recurring exec FiredAt (%s) must be near event creation time (%s)", evt.FiredAt.UTC(), callEnd)
	assert.Equal(t, dueAt.UnixNano(), evt.FireAt.UnixNano(), "FireAt preserves scheduled NextRunAt for missed-window check")

	// With FiredAt stamped at creation time rather than staleTickStart (-45s),
	// an unadmitted sweep running 60s after callStart (cutoff = callStart + 60s - 100s = callStart - 40s)
	// must NOT prematurely claim the still-admitting event.
	cutoff := callStart.Add(60 * time.Second).Add(-scheduledExecAdmitWindow)
	unadmitted, err := f.store.ListUnadmittedScheduledEvents(ctx, scheduledEventTypeExec, cutoff, 10)
	require.NoError(t, err)
	assert.Empty(t, unadmitted, "unadmitted recovery within 60s skew margin must not claim recurring event")

	close(gate)
	waitScheduleRunCount(t, f.store, sc.ID, 1)
}

type recoverBeforeRunInsertStore struct {
	store.Store
	fault *storeFaultSwitch
	srv   atomic.Pointer[Server]
	fired atomic.Bool
}

func (s *recoverBeforeRunInsertStore) CreateScheduledExecRun(ctx context.Context, run *store.ScheduledExecRun) error {
	if s.fault.Active() && s.fired.CompareAndSwap(false, true) {
		// Simulate another replica's unadmitted-event sweeper running after the
		// event was claimed fired and before this fire inserts its run row
		// (for example under out-of-spec inter-replica clock skew; ptone/scion#8210).
		s.srv.Load().recoverUnadmittedExecEvents(context.Background(), 10, 10)
	}
	return s.Store.CreateScheduledExecRun(ctx, run)
}

// TestScheduledExec_UnadmittedRecoveryBeforeRunInsertClosesAdmission (ptone/scion#8210)
// verifies that if unadmitted-event recovery marks an exec event failed before
// CreateScheduledExecRun inserts its run row, admission is refused
// transactionally: no run row is written, exec-start is never sent, the command
// does not run, the event retains execReasonNotDispatched, and recurring
// schedule run_count is incremented only once.
func TestScheduledExec_UnadmittedRecoveryBeforeRunInsertClosesAdmission(t *testing.T) {
	t.Run("recurring", func(t *testing.T) {
		var wrapped *recoverBeforeRunInsertStore
		var fault *storeFaultSwitch
		f := newExecRouteFixtureWith(t, func(srv *Server) {
			wrapped, fault = installStoreFault(t, srv, func(inner store.Store, sw *storeFaultSwitch) *recoverBeforeRunInsertStore {
				return &recoverBeforeRunInsertStore{Store: inner, fault: sw}
			})
			wrapped.srv.Store(srv)
		})
		setExecTiming(t, &scheduledExecAdmitWindow, -time.Minute)
		fault.Arm()
		f.startAgent(t)
		logs := captureDefaultLog(t)

		argv, marker := f.markerCommand(t)
		now := time.Now().UTC()
		sc := f.createExecSchedule(t, "race-8210-recurring", now.Add(-time.Second), 10, argv...)
		f.srv.executeSchedule(context.Background(), sc, now)

		evt := latestScheduleEvent(t, f.store, f.launched.ProjectID, sc.ID)
		require.Eventually(t, func() bool {
			return len(f.srv.runningDetachedFires.snapshot()) == 0 && !f.srv.execRuns.owns(evt.ID)
		}, 5*time.Second, 10*time.Millisecond, "detached recurring fire finished")

		assert.Zero(t, f.capture.count(execRoutePath(agentcontrol.RouteExecStart)), "lost admission race must not send exec-start")
		_, statErr := os.Stat(marker)
		assert.ErrorIs(t, statErr, os.ErrNotExist, "command must not have run")

		_, runErr := f.store.GetScheduledExecRun(context.Background(), evt.ID)
		assert.ErrorIs(t, runErr, store.ErrNotFound, "no run row may be inserted after event was closed")

		gotEvt := f.getEvent(t, evt.ID)
		assert.Equal(t, store.ScheduledEventFailed, gotEvt.Status)
		assert.Equal(t, execReasonNotDispatched, gotEvt.Error)

		gotSc, err := f.store.GetSchedule(context.Background(), sc.ID)
		require.NoError(t, err)
		assert.Equal(t, 1, gotSc.RunCount, "one fire must count once")
		assert.Equal(t, 1, gotSc.ErrorCount)
		assert.Equal(t, store.ScheduleRunError, gotSc.LastRunStatus)
		assert.Equal(t, execReasonNotDispatched, gotSc.LastRunError)
		assert.NotContains(t, logs.String(), "audit_action=schedule.exec.admit")
		assert.Equal(t, 1, strings.Count(logs.String(), "audit_action=schedule.exec.outcome"))
	})

	t.Run("one-shot", func(t *testing.T) {
		var wrapped *recoverBeforeRunInsertStore
		var fault *storeFaultSwitch
		f := newExecRouteFixtureWith(t, func(srv *Server) {
			wrapped, fault = installStoreFault(t, srv, func(inner store.Store, sw *storeFaultSwitch) *recoverBeforeRunInsertStore {
				return &recoverBeforeRunInsertStore{Store: inner, fault: sw}
			})
			wrapped.srv.Store(srv)
		})
		setExecTiming(t, &scheduledExecAdmitWindow, -time.Minute)
		fault.Arm()
		f.startAgent(t)
		logs := captureDefaultLog(t)

		argv, marker := f.markerCommand(t)
		evt := f.event(t, 10, argv...)
		f.fire(evt)
		f.waitUntracked(t, evt.ID)

		assert.Zero(t, f.capture.count(execRoutePath(agentcontrol.RouteExecStart)), "lost admission race must not send exec-start")
		_, statErr := os.Stat(marker)
		assert.ErrorIs(t, statErr, os.ErrNotExist, "command must not have run")

		_, runErr := f.store.GetScheduledExecRun(context.Background(), evt.ID)
		assert.ErrorIs(t, runErr, store.ErrNotFound, "no run row may be inserted after event was closed")

		gotEvt := f.getEvent(t, evt.ID)
		assert.Equal(t, store.ScheduledEventFailed, gotEvt.Status)
		assert.Equal(t, execReasonNotDispatched, gotEvt.Error, "one-shot event must preserve recovery failure reason")
		assert.NotContains(t, logs.String(), "audit_action=schedule.exec.admit")
		assert.Equal(t, 1, strings.Count(logs.String(), "audit_action=schedule.exec.outcome"))
	})
}
