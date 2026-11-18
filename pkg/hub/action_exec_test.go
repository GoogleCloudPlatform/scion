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
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agentcontrol"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestExecAction_NativeRegistryAndAfterRecord verifies:
//   - newExecRouteFixture / Server.registerFireActions registers *execAction
//     natively (not legacyAction);
//   - FireOutcome.AfterRecord defers starting trackScheduledExecRun until the
//     caller invokes AfterRecord (preserving the R1 outcome-persistence order);
//   - ResultRef and run.EventID equal the event's FireID, with run.ScheduleID
//     empty for one-shot fires.
func TestExecAction_NativeRegistryAndAfterRecord(t *testing.T) {
	f := newExecRouteFixture(t)
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
// property (a) UNIQUE scheduled_exec_runs.event_id idempotency and empty
// ResultRef on unadmitted Fire errors.
func TestExecAction_NativeOneShotAndRecurringEndToEnd(t *testing.T) {
	f := newExecRouteFixture(t)
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
	// CreateScheduledExecRun without sending a second exec-start and carries an
	// empty ResultRef.
	startsBefore := f.capture.count(execRoutePath(agentcontrol.RouteExecStart))
	dupOut := f.srv.fireAction(ctx, scheduledFireRequest(f.srv, oneShot, ExecutorContext{Kind: "scheduler", ID: "scheduled_event:" + oneShot.ID}, nil))
	assert.Equal(t, hubfire.FireStatusFailed, dupOut.Status)
	assert.Equal(t, execReasonDispatchFailed, dupOut.Error)
	assert.Empty(t, dupOut.ResultRef, "unadmitted Fire errors carry an empty ResultRef")
	assert.False(t, dupOut.Detached)
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

// TestExecAction_SynchronousTerminalDetachedPreservesRecordedOutcome guards
// property (b) on synchronous terminal answers from exec-start (409
// overlap/busy/mismatch, 408 expired_before_start, 400 invalid):
//   - FireOutcome.Detached must be true so the caller (Scheduler.fireEvent /
//     Server.executeSchedule) writes nothing after FinishScheduledExecRun;
//   - when another replica has already recorded a terminal outcome before
//     terminal() runs, neither FinishScheduledExecRun nor the scheduler caller
//     overwrites that recorded outcome. Setting Detached to false in terminal()
//     causes Scheduler.fireEvent to overwrite evt.Status / evt.Error and fails
//     every case below.
func TestExecAction_SynchronousTerminalDetachedPreservesRecordedOutcome(t *testing.T) {
	cases := []struct {
		name            string
		rpcStatus       int32
		rpcBody         string
		wantFireStatus  hubfire.FireStatus
		wantRunOutcome  string
		wantEventStatus string
		wantReason      string
	}{
		{
			name:            "409 overlap",
			rpcStatus:       http.StatusConflict,
			rpcBody:         `{"outcome":"overlap"}`,
			wantFireStatus:  hubfire.FireStatusSkipped,
			wantRunOutcome:  store.ScheduledExecOutcomeSkipped,
			wantEventStatus: store.ScheduledEventSkipped,
			wantReason:      execReasonOverlap,
		},
		{
			name:            "409 busy",
			rpcStatus:       http.StatusConflict,
			rpcBody:         `{"outcome":"busy"}`,
			wantFireStatus:  hubfire.FireStatusSkipped,
			wantRunOutcome:  store.ScheduledExecOutcomeSkipped,
			wantEventStatus: store.ScheduledEventSkipped,
			wantReason:      execReasonCapacity,
		},
		{
			name:            "409 mismatch",
			rpcStatus:       http.StatusConflict,
			rpcBody:         `{"outcome":"mismatch"}`,
			wantFireStatus:  hubfire.FireStatusFailed,
			wantRunOutcome:  store.ScheduledExecOutcomeError,
			wantEventStatus: store.ScheduledEventFailed,
			wantReason:      execReasonDispatchFailed,
		},
		{
			name:            "408 expired_before_start",
			rpcStatus:       http.StatusRequestTimeout,
			rpcBody:         `{"outcome":"expired_before_start"}`,
			wantFireStatus:  hubfire.FireStatusFailed,
			wantRunOutcome:  store.ScheduledExecOutcomeError,
			wantEventStatus: store.ScheduledEventFailed,
			wantReason:      execReasonStartExpired,
		},
		{
			name:            "400 invalid",
			rpcStatus:       http.StatusBadRequest,
			rpcBody:         `{"outcome":"invalid"}`,
			wantFireStatus:  hubfire.FireStatusFailed,
			wantRunOutcome:  store.ScheduledExecOutcomeError,
			wantEventStatus: store.ScheduledEventFailed,
			wantReason:      errScheduledExecPayloadInvalid.Error(),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newExecRouteFixture(t)
			f.capture.answer = func(req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
				if req.GetPath() != execRoutePath(agentcontrol.RouteExecStart) {
					return nil
				}
				return &conduitv1.RpcResponse{
					Status: tc.rpcStatus,
					Body:   []byte(tc.rpcBody),
				}
			}
			f.startAgent(t)
			ctx := context.Background()

			// 1. Direct fireAction check: Detached is true, ResultRef is evt.ID,
			// AfterRecord is nil, and FinishScheduledExecRun wrote run + event.
			directEvt := f.event(t, 10, "true")
			claimed, err := f.store.(store.ScheduledEventClaimer).ClaimScheduledEvent(ctx, directEvt.ID, store.ScheduledEventFired)
			require.NoError(t, err)
			require.True(t, claimed)

			out := f.srv.fireAction(withFireHistoryCreator(ctx, directEvt.CreatedBy),
				scheduledFireRequest(f.srv, directEvt, ExecutorContext{Kind: "scheduler", ID: "scheduled_event:" + directEvt.ID}, nil))
			assert.Equal(t, tc.wantFireStatus, out.Status)
			assert.Equal(t, tc.wantReason, out.Error)
			assert.Equal(t, directEvt.ID, out.ResultRef)
			assert.True(t, out.Detached, "synchronous terminal outcome must set Detached=true")
			assert.Nil(t, out.AfterRecord)
			assert.Equal(t, tc.wantRunOutcome, f.getRun(t, directEvt.ID).Outcome)
			gotDirectEvt := f.getEvent(t, directEvt.ID)
			assert.Equal(t, tc.wantEventStatus, gotDirectEvt.Status)
			assert.Equal(t, tc.wantReason, gotDirectEvt.Error)
			f.waitUntracked(t, directEvt.ID)

			// 2. End-to-end Scheduler.fireEvent non-overwrite check (property (b)):
			// another replica records a distinct terminal outcome ("exit status 9")
			// right after exec-start returns and before terminal() runs. Neither
			// FinishScheduledExecRun nor Scheduler.fireEvent may overwrite it.
			raceEvt := f.event(t, 10, "true")
			priorCode := 9
			priorReason := execReasonExitStatus(priorCode)
			setExecFireHook(t, func(stage, eventID string) {
				if stage != scheduledExecStageStarted || eventID != raceEvt.ID {
					return
				}
				run := f.getRun(t, eventID)
				changed, finErr := f.srv.finishScheduledExecRun(
					ctx,
					*run,
					store.ScheduledExecOutcomeNonzero,
					&agentcontrol.ExecResult{
						State:    agentcontrol.ExecStateFinished,
						Outcome:  agentcontrol.ExecOutcomeNonzero,
						ExitCode: priorCode,
					},
					store.ScheduledEventFailed,
					priorReason,
				)
				require.NoError(t, finErr)
				require.True(t, changed)
			})

			f.fire(raceEvt)
			f.waitUntracked(t, raceEvt.ID)

			gotRun := f.getRun(t, raceEvt.ID)
			assert.Equal(t, store.ScheduledExecOutcomeNonzero, gotRun.Outcome,
				"FinishScheduledExecRun CAS must preserve prior replica's run outcome")
			gotRaceEvt := f.getEvent(t, raceEvt.ID)
			assert.Equal(t, store.ScheduledEventFailed, gotRaceEvt.Status,
				"Scheduler.fireEvent must not overwrite prior replica's recorded event status")
			assert.Equal(t, priorReason, gotRaceEvt.Error,
				"Scheduler.fireEvent must not overwrite prior replica's recorded event error")
		})
	}
}
