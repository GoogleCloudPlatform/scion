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

	"github.com/GoogleCloudPlatform/scion/pkg/agentcontrol"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newNativeExecRouteFixture builds an execRouteFixture that leaves Server's
// native registerFireActions() registration (&execAction{srv: srv}) in place
// rather than calling Scheduler.RegisterEventHandler.
func newNativeExecRouteFixture(t *testing.T) *execRouteFixture {
	t.Helper()
	setExecTiming(t, &scheduledExecPollInitial, 20*time.Millisecond)
	setExecTiming(t, &scheduledExecPollMax, 50*time.Millisecond)
	f := newPTYConduitFixtureServer(t, nil)
	enableExecSchedules(t, f.srv)
	ctx := context.Background()
	a, err := f.store.GetAgent(ctx, f.launched.ID)
	require.NoError(t, err)
	a.Phase = "running"
	require.NoError(t, f.store.UpdateAgent(ctx, a))
	got, err := f.store.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	f.launched = got
	owner, err := f.store.GetUser(ctx, got.OwnerID)
	require.NoError(t, err)

	sched := NewScheduler(f.store, slog.Default())
	sched.srv = f.srv
	f.srv.mu.Lock()
	f.srv.scheduler = sched
	f.srv.mu.Unlock()

	return &execRouteFixture{
		ptyConduitFixture: f,
		capture:           &execCapture{},
		opts:              execControlOptions(t),
		owner:             authUser(owner),
	}
}

// TestExecAction_NativeRegistryAndAfterRecord verifies:
//   - Server.registerFireActions registers *execAction natively (not legacyAction);
//   - FireOutcome.AfterRecord defers starting trackScheduledExecRun until the
//     caller invokes AfterRecord (preserving the R1 outcome-persistence order);
//   - ResultRef and run.EventID equal the event's FireID, with run.ScheduleID
//     empty for one-shot fires.
func TestExecAction_NativeRegistryAndAfterRecord(t *testing.T) {
	f := newNativeExecRouteFixture(t)
	f.startAgent(t)

	registered, ok := f.srv.LookupAction(scheduledEventTypeExec)
	require.True(t, ok)
	_, isNative := registered.(*execAction)
	require.True(t, isNative, "expected *execAction in Server.actions, got %T", registered)

	ctx := context.Background()
	evt := f.event(t, 30, "sh", "-c", "sleep 0.05; exit 0")
	claimed, err := f.store.(store.ScheduledEventClaimer).ClaimScheduledEvent(ctx, evt.ID, store.ScheduledEventFired)
	require.NoError(t, err)
	require.True(t, claimed)

	req := scheduledFireRequest(f.srv, evt, ExecutorContext{Kind: "scheduler", ID: "scheduled_event:" + evt.ID}, nil)
	out := f.srv.fireAction(withFireHistoryCreator(ctx, evt.CreatedBy), req)
	require.Equal(t, hubfire.FireStatusFired, out.Status)
	assert.True(t, out.Detached)
	assert.Equal(t, evt.ID, out.ResultRef)
	require.NotNil(t, out.AfterRecord, "admitted running exec must return AfterRecord hook")

	// Before AfterRecord runs, the replica slot is held (handed off) and no
	// background tracker has polled or finalized the run yet — even past
	// several scheduledExecPollInitial (20ms) intervals.
	assert.True(t, f.srv.execRuns.owns(evt.ID), "slot remains owned across AfterRecord handoff")
	time.Sleep(150 * time.Millisecond)
	assert.Zero(t, f.capture.count(execRoutePath(agentcontrol.RouteExecStatus)),
		"tracker must not poll exec-status before caller invokes AfterRecord")
	runBefore := f.getRun(t, evt.ID)
	assert.Nil(t, runBefore.FinishedAt)
	assert.Empty(t, runBefore.ScheduleID, "one-shot exec must leave schedule_id empty")

	// Invoking AfterRecord starts the tracker, which polls exec-status, records
	// the final outcome, and releases replica ownership.
	out.AfterRecord(ctx)
	runAfter := f.waitFinished(t, evt.ID)
	assert.Equal(t, store.ScheduledExecOutcomeOK, runAfter.Outcome)
	assert.Equal(t, store.ScheduledEventFired, f.getEvent(t, evt.ID).Status)
	f.waitUntracked(t, evt.ID)
}

// TestExecAction_NativeOneShotAndRecurringEndToEnd exercises native *execAction
// registration through Scheduler.fireEvent and Server.executeSchedule, plus
// property (a) UNIQUE scheduled_exec_runs.event_id idempotency.
func TestExecAction_NativeOneShotAndRecurringEndToEnd(t *testing.T) {
	f := newNativeExecRouteFixture(t)
	f.startAgent(t)
	ctx := context.Background()

	// 1. One-shot exit 0 and UNIQUE scheduled_exec_runs.event_id idempotency.
	oneShot := f.event(t, 10, "true")
	f.fire(oneShot)
	gotRun := f.waitFinished(t, oneShot.ID)
	assert.Equal(t, store.ScheduledExecOutcomeOK, gotRun.Outcome)
	assert.Equal(t, store.ScheduledEventFired, f.getEvent(t, oneShot.ID).Status)
	assert.Empty(t, gotRun.ScheduleID)
	f.waitUntracked(t, oneShot.ID)

	// Re-firing the already-admitted event_id via fireAction fails closed at
	// CreateScheduledExecRun without sending a second exec-start.
	startsBefore := f.capture.count(execRoutePath(agentcontrol.RouteExecStart))
	dupOut := f.srv.fireAction(ctx, scheduledFireRequest(f.srv, oneShot, ExecutorContext{Kind: "scheduler", ID: "scheduled_event:" + oneShot.ID}, nil))
	assert.Equal(t, hubfire.FireStatusFailed, dupOut.Status)
	assert.Equal(t, execReasonDispatchFailed, dupOut.Error)
	assert.Equal(t, startsBefore, f.capture.count(execRoutePath(agentcontrol.RouteExecStart)),
		"property (a): duplicate event_id is never started twice")
	assert.Equal(t, store.ScheduledEventFired, f.getEvent(t, oneShot.ID).Status,
		"property (b): existing terminal event status is not overwritten")

	// 2. Recurring exec schedule: schedule_id populated, run/error accounting preserved.
	t1 := nextRealScheduleTick(nil)
	sc := f.createExecSchedule(t, "native-rec-exec", t1.Add(-time.Second), 10, "sh", "-c", "exit 4")
	f.srv.executeSchedule(ctx, sc, t1)
	gotSc := waitScheduleRunCount(t, f.store, sc.ID, 1)
	assert.Equal(t, 1, gotSc.RunCount)
	assert.Equal(t, 1, gotSc.ErrorCount)
	assert.Equal(t, store.ScheduleRunError, gotSc.LastRunStatus)
	assert.Equal(t, execReasonExitStatus(4), gotSc.LastRunError)

	recEvt := latestScheduleEvent(t, f.store, f.launched.ProjectID, sc.ID)
	assert.Equal(t, store.ScheduledEventFailed, recEvt.Status)
	recRun := f.getRun(t, recEvt.ID)
	assert.Equal(t, sc.ID, recRun.ScheduleID)
	assert.Equal(t, store.ScheduledExecOutcomeNonzero, recRun.Outcome)
	f.waitUntracked(t, recEvt.ID)
}
