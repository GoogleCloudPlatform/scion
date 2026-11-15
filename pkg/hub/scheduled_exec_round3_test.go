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
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agentcontrol"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Regression tests for review round 3 of ptone/scion#7883 (P3-R1, P3-R2).

// markerCommand returns argv that creates a marker file in the agent's
// workdir, and the marker's path: the command ran iff the file exists.
func (f *execRouteFixture) markerCommand(t *testing.T) ([]string, string) {
	t.Helper()
	name := "ran-" + uuid.NewString()
	return []string{"touch", name}, filepath.Join(f.opts.Exec.Workdir, name)
}

func assertNeverRan(t *testing.T, marker string) {
	t.Helper()
	time.Sleep(300 * time.Millisecond)
	_, err := os.Stat(marker)
	assert.True(t, os.IsNotExist(err), "the command must never start")
}

// TestScheduledExecP3R1_TerminalBeforeDispatchNeverStarts: recovery (another
// replica) finishes the run after it is admitted and before the original
// fire dispatches; the fire sends nothing and the command never starts.
// Covered for the lease abandon and the absent-start dispatch failure.
func TestScheduledExecP3R1_TerminalBeforeDispatchNeverStarts(t *testing.T) {
	for _, outcome := range []string{store.ScheduledExecOutcomeAbandoned, store.ScheduledExecOutcomeError} {
		t.Run(outcome, func(t *testing.T) {
			f := newExecRouteFixture(t)
			f.startAgent(t)
			argv, marker := f.markerCommand(t)
			evt := f.event(t, 10, argv...)
			setExecFireHook(t, func(stage, id string) {
				if stage != scheduledExecStageAdmitted || id != evt.ID {
					return
				}
				won, err := f.store.FinishScheduledExecRun(context.Background(), store.ScheduledExecRunFinish{
					EventID: id, Outcome: outcome, EventStatus: store.ScheduledEventFailed, EventError: execReasonOutcomeUnknown,
				})
				require.NoError(t, err)
				require.True(t, won)
			})
			f.fire(evt)
			assert.Zero(t, f.capture.count(execRoutePath(agentcontrol.RouteExecStart)), "no exec-start for a finished run")
			assertNeverRan(t, marker)
			got := f.getEvent(t, evt.ID)
			assert.Equal(t, store.ScheduledEventFailed, got.Status)
			assert.Equal(t, execReasonOutcomeUnknown, got.Error, "the recovery's outcome stands")
			f.waitUntracked(t, evt.ID)
		})
	}
}

// TestScheduledExecP3R1_DelayedFirePastDeadline: a fire delayed after the
// insert past its frozen start deadline sends nothing.
func TestScheduledExecP3R1_DelayedFirePastDeadline(t *testing.T) {
	setExecTiming(t, &scheduledExecNotAfter, 50*time.Millisecond)
	f := newExecRouteFixture(t)
	f.startAgent(t)
	argv, marker := f.markerCommand(t)
	evt := f.event(t, 10, argv...)
	setExecFireHook(t, func(stage, id string) {
		if stage == scheduledExecStageAdmitted && id == evt.ID {
			time.Sleep(200 * time.Millisecond)
		}
	})
	f.fire(evt)
	assert.Zero(t, f.capture.count(execRoutePath(agentcontrol.RouteExecStart)))
	assertNeverRan(t, marker)
	got := f.getEvent(t, evt.ID)
	assert.Equal(t, store.ScheduledEventFailed, got.Status)
	assert.Equal(t, execReasonStartExpired, got.Error)
}

// TestScheduledExecP3R1_PauseBetweenCheckAndSend: a fire that pauses after
// the dispatch guard and sends after its frozen deadline plus tolerance is
// refused by sciontool (expired_before_start), because the deadline is not
// refreshed for the late send; the command never starts.
func TestScheduledExecP3R1_PauseBetweenCheckAndSend(t *testing.T) {
	if testing.Short() {
		t.Skip("waits past the 10 s start tolerance")
	}
	setExecTiming(t, &scheduledExecNotAfter, time.Second)
	f := newExecRouteFixture(t)
	f.startAgent(t)
	argv, marker := f.markerCommand(t)
	evt := f.event(t, 10, argv...)
	setExecFireHook(t, func(stage, id string) {
		if stage == scheduledExecStageChecked && id == evt.ID {
			time.Sleep(time.Second + agentcontrol.ExecClockTolerance + time.Second)
		}
	})
	f.fire(evt)
	require.Equal(t, 1, f.capture.count(execRoutePath(agentcontrol.RouteExecStart)), "the late send reached the agent")
	var sent agentcontrol.ExecStartRequest
	f.capture.mu.Lock()
	require.NoError(t, json.Unmarshal(f.capture.bodies[execRoutePath(agentcontrol.RouteExecStart)][0], &sent))
	f.capture.mu.Unlock()
	run := f.getRun(t, evt.ID)
	assert.Equal(t, run.NotAfter.UnixMilli(), sent.NotAfterUnixMS, "the frozen deadline is sent, not a fresh one")
	assertNeverRan(t, marker)
	got := f.getEvent(t, evt.ID)
	assert.Equal(t, store.ScheduledEventFailed, got.Status)
	assert.Equal(t, execReasonStartExpired, got.Error)
}

// TestScheduledExecP3R2_SlowAgentPrefixDoesNotStarveHealthy (the
// reviewer's reproduction): unresolved runs of a connected target that never
// answers exec-status are older than a healthy target's finished run, and
// outnumber the in-flight slots; one sweep still records the healthy run.
func TestScheduledExecP3R2_SlowAgentPrefixDoesNotStarveHealthy(t *testing.T) {
	f := newExecRouteFixture(t)
	setExecTiming(t, &scheduledExecPollInitial, time.Hour)
	setExecTiming2(t, &scheduledExecSweepInFlight, 2)
	f.startAgent(t)
	ctx := context.Background()
	slow := *f.launched
	slow.ID = uuid.NewString()
	slow.Slug = "p3-slow-" + uuid.NewString()[:8]
	slow.Name = slow.Slug
	slow.RunID = uuid.NewString()
	require.NoError(t, f.store.CreateAgent(ctx, &slow))
	_, err := f.store.SetAgentRunID(ctx, slow.ID, slow.RunID, nil)
	require.NoError(t, err)
	rf := *f.relayFixture
	rf.launched = &slow
	sf := &ptyConduitFixture{relayFixture: &rf}
	sf.startControlAgentWithOptions(t, f.public.URL, true, f.opts, func(h conduit.RPCHandler) conduit.RPCHandler {
		return conduit.RPCHandlerFunc(func(ctx context.Context, req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
			if req.GetPath() == execRoutePath(agentcontrol.RouteExecStatus) {
				<-ctx.Done()
				return &conduitv1.RpcResponse{Status: 500}
			}
			return h.HandleRPC(ctx, req)
		})
	})
	for i := 0; i < 4; i++ {
		evt := f.event(t, 60, "true")
		_, err := f.store.(store.ScheduledEventClaimer).ClaimScheduledEvent(ctx, evt.ID, store.ScheduledEventFired)
		require.NoError(t, err)
		require.NoError(t, f.store.CreateScheduledExecRun(ctx, &store.ScheduledExecRun{
			EventID: evt.ID, ProjectID: evt.ProjectID, TargetAgentID: slow.ID,
			CommandSHA256: agentcontrol.ExecArgvDigest([]string{"true"}), Argc: 1, TimeoutSeconds: 60,
			StartedAt: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
			LeaseUntil: time.Now().Add(time.Hour), LaunchID: slow.RunID,
		}))
	}
	healthy := f.event(t, 1, "true")
	f.fire(healthy)
	f.waitAgentFinished(t, healthy.ID)
	f.srv.execRuns.work.closeAdmission()
	f.srv.stopScheduledExecRuns(ctx)
	f.waitUntracked(t, healthy.ID)

	sweepCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		f.srv.sweepScheduledExecRuns(sweepCtx)
	}()
	require.Eventually(t, func() bool { return f.getRun(t, healthy.ID).FinishedAt != nil }, 2*time.Second, 10*time.Millisecond,
		"round-robin across agents gives the healthy target a slot")
	cancel()
	wg.Wait()
	assert.Equal(t, store.ScheduledExecOutcomeOK, f.getRun(t, healthy.ID).Outcome)
}
