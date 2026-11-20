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
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agentcontrol"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Regression tests for review round 1 of ptone/scion#7883 (R1-R5, F1),
// including the reviewer's reproductions.

// wrappedExecFireAction wraps execAction to run a hook immediately after Fire
// returns while preserving native Preauthorize, Authorize, Detaches, and
// OnAuthorityRefused behaviour.
type wrappedExecFireAction struct {
	execAction
	afterFire func(eventID string)
}

func (a *wrappedExecFireAction) Fire(ctx context.Context, req hubfire.FireRequest, auth FireAuthorized) (hubfire.FireOutcome, error) {
	out, err := a.execAction.Fire(ctx, req, auth)
	if a.afterFire != nil {
		a.afterFire(req.Authority.FireID)
	}
	return out, err
}

// setExecFireHook installs scheduledExecFireHook for the test.
func setExecFireHook(t *testing.T, hook func(stage, eventID string)) {
	t.Helper()
	prev := scheduledExecFireHook
	scheduledExecFireHook = hook
	t.Cleanup(func() { scheduledExecFireHook = prev })
}

// TestScheduledExecR1_SweepBeforeFireRecord is the reviewer's R1
// reproduction: another replica records the run's failure before the
// original fire returns; the fire must not overwrite it with fired.
func TestScheduledExecR1_SweepBeforeFireRecord(t *testing.T) {
	f := newExecRouteFixture(t)
	f.startAgent(t)
	evt := f.event(t, 30, "sh", "-c", "sleep 0.1; exit 7")
	f.srv.RegisterAction(&wrappedExecFireAction{
		execAction: execAction{srv: f.srv},
		afterFire: func(eID string) {
			f.waitAgentFinished(t, eID)
			run := f.getRun(t, eID)
			var unavailable bool
			require.True(t, f.srv.pollScheduledExecRunOnce(context.Background(), run, &unavailable))
			require.Equal(t, store.ScheduledEventFailed, f.getEvent(t, eID).Status)
		},
	})
	f.fire(evt)
	f.waitUntracked(t, evt.ID)
	assert.Equal(t, store.ScheduledExecOutcomeNonzero, f.getRun(t, evt.ID).Outcome)
	got := f.getEvent(t, evt.ID)
	assert.Equal(t, store.ScheduledEventFailed, got.Status, "the fire preserves the other replica's terminal failure")
	assert.Equal(t, "exit status 7", got.Error)
}

// TestScheduledExecR1R2R3_InterleavingMatrix runs another replica's poll,
// a purge, a flag flip and a target deletion at each stage of a fire
// (after the run row is written, and after exec-start answers, before
// anything is recorded), and checks the invariants every interleaving must
// keep: exec-start at most once; an event never shows fired while its run
// recorded a failure; the run ends recorded (or is gone with its event);
// no tracker is left behind.
func TestScheduledExecR1R2R3_InterleavingMatrix(t *testing.T) {
	setExecTiming(t, &scheduledExecLeaseGrace, 2*time.Second)
	actions := map[string]func(t *testing.T, f *execRouteFixture, eventID string){
		"other replica polls": func(t *testing.T, f *execRouteFixture, eventID string) {
			run, err := f.store.GetScheduledExecRun(context.Background(), eventID)
			require.NoError(t, err)
			var unavailable bool
			f.srv.pollScheduledExecRunOnce(context.Background(), run, &unavailable)
		},
		"other replica sweeps": func(t *testing.T, f *execRouteFixture, eventID string) {
			// Another replica owns none of this replica's runs.
			run, err := f.store.GetScheduledExecRun(context.Background(), eventID)
			require.NoError(t, err)
			run.LeaseUntil = time.Now().Add(-time.Second) // its view: the lease ended
			var unavailable bool
			f.srv.pollScheduledExecRunOnce(context.Background(), run, &unavailable)
		},
		"purge": func(t *testing.T, f *execRouteFixture, eventID string) {
			_, err := f.store.PurgeOldScheduledEvents(context.Background(), time.Now().Add(time.Hour))
			require.NoError(t, err)
		},
		"flag off": func(t *testing.T, f *execRouteFixture, eventID string) {
			setHubExperiments(t, f.srv, map[string]bool{conduitExperiment: true, experiments.ExecSchedules: false})
		},
		"target deleted": func(t *testing.T, f *execRouteFixture, eventID string) {
			require.NoError(t, f.store.DeleteAgent(context.Background(), f.launched.ID))
		},
	}
	for _, stage := range []string{scheduledExecStageAdmitted, scheduledExecStageStarted} {
		for name, action := range actions {
			for _, code := range []int{0, 7} {
				t.Run(stage+"/"+name+"/exit "+map[int]string{0: "0", 7: "7"}[code], func(t *testing.T) {
					f := newExecRouteFixture(t)
					f.startAgent(t)
					script := map[int]string{0: "exit 0", 7: "exit 7"}[code]
					evt := f.event(t, 1, "sh", "-c", script)
					var once sync.Once
					setExecFireHook(t, func(s, id string) {
						if s != stage || id != evt.ID {
							return
						}
						once.Do(func() {
							if stage == scheduledExecStageStarted {
								f.waitAgentFinished(t, id)
							}
							action(t, f, id)
						})
					})
					f.fire(evt)
					assert.LessOrEqual(t, f.capture.count(execRoutePath(agentcontrol.RouteExecStart)), 1, "exec-start at most once")
					if stage == scheduledExecStageAdmitted && (name == "other replica sweeps" || name == "purge") {
						// Recovery or a purge finished the run before
						// dispatch: nothing may start (P3-R1).
						assert.Zero(t, f.capture.count(execRoutePath(agentcontrol.RouteExecStart)), "no start after the run is terminal")
					}

					gotEvt, evtErr := f.store.GetScheduledEvent(context.Background(), evt.ID)
					if evtErr != nil {
						// Purged: the run went with its event.
						_, runErr := f.store.GetScheduledExecRun(context.Background(), evt.ID)
						assert.ErrorIs(t, runErr, store.ErrNotFound)
						f.waitUntracked(t, evt.ID)
						return
					}
					run, runErr := f.store.GetScheduledExecRun(context.Background(), evt.ID)
					if runErr == nil {
						run = f.waitFinished(t, evt.ID)
						gotEvt = f.getEvent(t, evt.ID)
						switch run.Outcome {
						case store.ScheduledExecOutcomeOK:
							assert.Contains(t, []string{store.ScheduledEventFired, store.ScheduledEventExpired}, gotEvt.Status)
						case store.ScheduledExecOutcomeSkipped:
							assert.Equal(t, store.ScheduledEventSkipped, gotEvt.Status)
						default:
							assert.Equal(t, store.ScheduledEventFailed, gotEvt.Status, "run %s", run.Outcome)
							assert.NotEmpty(t, gotEvt.Error)
						}
					} else {
						assert.NotEqual(t, store.ScheduledEventFired, gotEvt.Status, "no run: never fired")
					}
					f.waitUntracked(t, evt.ID)
				})
			}
		}
	}
}

// TestScheduledExecR2_SweepCursorProgress is the reviewer's R2
// reproduction: an expired run beyond one sweep's budget is still recorded.
func TestScheduledExecR2_SweepCursorProgress(t *testing.T) {
	f := newExecRouteFixture(t)
	setExecTiming2(t, &scheduledExecSweepBatch, 2)
	setExecTiming2(t, &scheduledExecSweepMaxPages, 1)
	ctx := context.Background()
	evts := []store.ScheduledEvent{f.event(t, 3600, "true"), f.event(t, 3600, "true"), f.event(t, 3600, "true")}
	high := evts[0].ID
	for _, evt := range evts {
		if evt.ID > high {
			high = evt.ID
		}
	}
	for _, evt := range evts {
		_, err := f.store.(store.ScheduledEventClaimer).ClaimScheduledEvent(ctx, evt.ID, store.ScheduledEventFired)
		require.NoError(t, err)
		lease := time.Now().Add(time.Hour)
		if evt.ID == high {
			lease = time.Now().Add(-time.Minute)
		}
		require.NoError(t, f.store.CreateScheduledExecRun(ctx, &store.ScheduledExecRun{
			EventID: evt.ID, ProjectID: evt.ProjectID, TargetAgentID: f.launched.ID,
			CommandSHA256: agentcontrol.ExecArgvDigest([]string{"true"}), Argc: 1, TimeoutSeconds: 3600,
			StartedAt: time.Now(), LeaseUntil: lease, LaunchID: f.launched.RunID,
			Outcome: store.ScheduledExecOutcomeRunning,
		}))
	}
	for i := 0; i < 3; i++ {
		f.srv.sweepScheduledExecRuns(ctx)
	}
	require.NotNil(t, f.getRun(t, high).FinishedAt, "later runs are reached while the prefix stays live")
}

// setExecTiming2 sets an int test seam for the test.
func setExecTiming2(t *testing.T, v *int, n int) {
	t.Helper()
	scheduledExecSeamMu.Lock()
	prev := *v
	*v = n
	scheduledExecSeamMu.Unlock()
	t.Cleanup(func() {
		scheduledExecSeamMu.Lock()
		*v = prev
		scheduledExecSeamMu.Unlock()
	})
}

// TestScheduledExecR2_UnavailablePrefixRotates: with more live runs on an
// unavailable agent than one sweep's budget, successive sweeps poll every
// one of them (the persisted rotation), never only the same prefix.
func TestScheduledExecR2_UnavailablePrefixRotates(t *testing.T) {
	f := newExecRouteFixture(t) // no session: every poll is unavailable
	setExecTiming2(t, &scheduledExecSweepBatch, 1)
	setExecTiming2(t, &scheduledExecSweepMaxPages, 2)
	setExecTiming2(t, &scheduledExecSweepInFlight, 2)
	ctx := context.Background()
	var ids []string
	for i := 0; i < 7; i++ {
		evt := f.event(t, 3600, "true")
		_, err := f.store.(store.ScheduledEventClaimer).ClaimScheduledEvent(ctx, evt.ID, store.ScheduledEventFired)
		require.NoError(t, err)
		require.NoError(t, f.store.CreateScheduledExecRun(ctx, &store.ScheduledExecRun{
			EventID: evt.ID, ProjectID: evt.ProjectID, TargetAgentID: f.launched.ID,
			CommandSHA256: agentcontrol.ExecArgvDigest([]string{"true"}), Argc: 1, TimeoutSeconds: 3600,
			StartedAt: time.Now().Add(-time.Duration(i) * time.Millisecond), LeaseUntil: time.Now().Add(time.Hour),
			LaunchID: f.launched.RunID, Outcome: store.ScheduledExecOutcomeRunning,
		}))
		ids = append(ids, evt.ID)
	}
	before := time.Now()
	for i := 0; i < 4; i++ { // budget 2 per sweep: 8 polls cover all 7
		f.srv.sweepScheduledExecRuns(ctx)
	}
	for _, id := range ids {
		run := f.getRun(t, id)
		assert.True(t, run.LastPolledAt.After(before), "run %s polled", id)
		assert.Nil(t, run.FinishedAt)
	}
}

// TestScheduledExecR3_SlowStatusOutcomeRecorded is the reviewer's R3
// reproduction: a finished answer that arrives after the old 10 s
// persistence budget, inside the 30 s call budget, is recorded.
func TestScheduledExecR3_SlowStatusOutcomeRecorded(t *testing.T) {
	if testing.Short() {
		t.Skip("waits past the 10 s record timeout")
	}
	f := newExecRouteFixture(t)
	setExecTiming(t, &scheduledExecPollInitial, time.Hour)
	f.startAgent(t)
	evt := f.event(t, 30, "sh", "-c", "sleep 0.1; exit 7")
	f.fire(evt)
	f.waitAgentFinished(t, evt.ID)
	f.capture.mu.Lock()
	f.capture.answer = func(req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
		if req.GetPath() != execRoutePath(agentcontrol.RouteExecStatus) {
			return nil
		}
		time.Sleep(scheduledEventRecordTimeout + time.Second)
		return &conduitv1.RpcResponse{Status: 200, Body: []byte(`{"state":"finished","outcome":"nonzero","exit_code":7,"duration_ms":100,"output_bytes":0}`)}
	}
	f.capture.mu.Unlock()
	run := f.getRun(t, evt.ID)
	var unavailable bool
	require.True(t, f.srv.pollScheduledExecRunOnce(context.Background(), run, &unavailable))
	got := f.getRun(t, evt.ID)
	require.NotNil(t, got.FinishedAt, "a status answered within the call budget is recorded")
	assert.Equal(t, store.ScheduledExecOutcomeNonzero, got.Outcome)
}

// finishFaultStore fails FinishScheduledExecRun while its switch is armed
// and fail is set.
type finishFaultStore struct {
	store.Store
	fault *storeFaultSwitch
	fail  atomic.Bool
}

func (s *finishFaultStore) FinishScheduledExecRun(ctx context.Context, fin store.ScheduledExecRunFinish) (bool, error) {
	if s.fault.Active() && s.fail.Load() {
		return false, context.DeadlineExceeded
	}
	return s.Store.FinishScheduledExecRun(ctx, fin)
}

// TestScheduledExecR3_FailedWriteKeepsTracking: a failed outcome write
// keeps the run tracked; the tracker records it once the store recovers.
// An already-finished run needs nothing more.
func TestScheduledExecR3_FailedWriteKeepsTracking(t *testing.T) {
	var faulty *finishFaultStore
	var fault *storeFaultSwitch
	f := newExecRouteFixtureWith(t, func(srv *Server) {
		faulty, fault = installStoreFault(t, srv, func(inner store.Store, sw *storeFaultSwitch) *finishFaultStore {
			return &finishFaultStore{Store: inner, fault: sw}
		})
	})
	faulty.fail.Store(true)
	fault.Arm()
	f.startAgent(t)
	evt := f.event(t, 30, "sh", "-c", "exit 4")
	f.fire(evt)
	f.waitAgentFinished(t, evt.ID)
	run := f.getRun(t, evt.ID)
	var unavailable bool
	assert.False(t, f.srv.pollScheduledExecRunOnce(context.Background(), run, &unavailable), "a failed write is not done")
	time.Sleep(200 * time.Millisecond)
	assert.True(t, f.srv.execRuns.owns(evt.ID), "still tracked")
	assert.Nil(t, f.getRun(t, evt.ID).FinishedAt)

	faulty.fail.Store(false)
	got := f.waitFinished(t, evt.ID)
	assert.Equal(t, store.ScheduledExecOutcomeNonzero, got.Outcome)
	f.waitUntracked(t, evt.ID)
	// Already finished: done, nothing written.
	assert.True(t, f.srv.pollScheduledExecRunOnce(context.Background(), got, &unavailable))
}

// TestScheduledExecR4_ConcurrentLastSlot is the reviewer's R4
// reproduction: two authors held after their early quota reads race for
// the last slot; one gets 201, the other 429 quota_exceeded.
func TestScheduledExecR4_ConcurrentLastSlot(t *testing.T) {
	f := execAuthoringFixture(t)
	for i := 0; i < scheduledExecMaxPerTarget-1; i++ {
		seedExecEvent(t, f, f.sibling.ID)
	}
	wrapped, fault := installStoreFault(t, f.srv, func(inner store.Store, sw *storeFaultSwitch) *quotaBarrierStore {
		return &quotaBarrierStore{Store: inner, fault: sw, arrivals: make(chan struct{}, 2), release: make(chan struct{})}
	})
	fault.Arm()
	codes := make(chan int, 2)
	for i := 0; i < 2; i++ {
		go func() {
			rec := requestAsIdentity(t, f.srv, authUser(f.owner), http.MethodPost, execEventsPath(f), execCreateBody(f.sibling.ID, "true"))
			codes <- rec.Code
		}()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-wrapped.arrivals:
		case <-time.After(10 * time.Second):
			t.Fatal("quota checks did not arrive")
		}
	}
	close(wrapped.release)
	a, b := <-codes, <-codes
	assert.ElementsMatch(t, []int{http.StatusCreated, http.StatusTooManyRequests}, []int{a, b})
	assert.Len(t, storedExecEvents(t, f), scheduledExecMaxPerTarget)
}

// quotaBarrierStore holds each early quota read until released.
type quotaBarrierStore struct {
	store.Store
	fault    *storeFaultSwitch
	arrivals chan struct{}
	release  chan struct{}
}

func (s *quotaBarrierStore) ListScheduledEvents(ctx context.Context, f store.ScheduledEventFilter, o store.ListOptions) (*store.ListResult[store.ScheduledEvent], error) {
	result, err := s.Store.ListScheduledEvents(ctx, f, o)
	if s.fault.Active() && f.EventType == scheduledEventTypeExec && f.Status == store.ScheduledEventPending {
		s.arrivals <- struct{}{}
		<-s.release
	}
	return result, err
}

// TestScheduledExecF1_TargetDeletionCancelsBeyondFirstPage (F1, P2-R2):
// the target's exec event is older than the first page and its UUID sorts
// after every newer event (UUID order opposed to creation order, the
// reviewer's fixture); deleting the target still cancels it.
func TestScheduledExecF1_TargetDeletionCancelsBeyondFirstPage(t *testing.T) {
	f := execAuthoringFixture(t)
	ctx := context.Background()
	old := time.Now().Add(-time.Hour)
	p := scheduledExecPayload{AgentID: f.sibling.ID, Command: []string{"true"}}
	raw, err := p.encode()
	require.NoError(t, err)
	target := withSessionRevision(store.ScheduledEvent{
		ID: "ffffffff-ffff-4fff-8fff-ffffffffffff", ProjectID: f.proj.ID, EventType: scheduledEventTypeExec,
		FireAt: time.Now().Add(time.Hour), Payload: raw, Status: store.ScheduledEventPending, CreatedAt: old,
	}, f.owner.ID)
	require.NoError(t, f.store.CreateScheduledEvent(ctx, &target))
	other, _ := json.Marshal(MessageEventPayload{AgentID: f.caller.ID, Message: "x"})
	for i := 0; i < 1100; i++ {
		low := uuid.New()
		low[0] = 1
		require.NoError(t, f.store.CreateScheduledEvent(ctx, &store.ScheduledEvent{
			ID: low.String(), ProjectID: f.proj.ID, EventType: "message", Payload: string(other),
			Status: store.ScheduledEventPending, FireAt: time.Now().Add(time.Hour),
			CreatedAt: old.Add(time.Minute + time.Duration(i)*time.Millisecond),
		}))
	}
	f.srv.cancelScheduledEventsForAgent(ctx, f.sibling)
	got, err := f.store.GetScheduledEvent(ctx, target.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ScheduledEventCancelled, got.Status)
}

// TestScheduledExecP2R1_ClaimBeforeRunCrash (P2-R1, AC10; the reviewer's
// reproduction): a hub that stops after claiming an exec event (fired, or
// expired on a restart replay) and before inserting its run leaves an event
// no replay or run sweep finds. The sweeper records it failed once the
// admission window has passed, never sends exec-start for it, and leaves a
// claim still inside the window alone.
func TestScheduledExecP2R1_ClaimBeforeRunCrash(t *testing.T) {
	for _, claim := range []string{store.ScheduledEventFired, store.ScheduledEventExpired} {
		t.Run(claim, func(t *testing.T) {
			f := newExecRouteFixture(t)
			f.startAgent(t)
			ctx := context.Background()
			evt := f.event(t, 1, "true")
			claimed, err := f.store.(store.ScheduledEventClaimer).ClaimScheduledEvent(ctx, evt.ID, claim)
			require.NoError(t, err)
			require.True(t, claimed)
			fresh := f.event(t, 1, "true")
			claimed, err = f.store.(store.ScheduledEventClaimer).ClaimScheduledEvent(ctx, fresh.ID, claim)
			require.NoError(t, err)
			require.True(t, claimed)

			// Restart: the claim is older than the admission window.
			old := time.Now().Add(-2 * time.Hour)
			require.NoError(t, f.store.UpdateScheduledEventStatus(ctx, evt.ID, claim, &old, ""))
			restarted := NewScheduler(f.store, slog.Default())
			restarted.srv = f.srv
			restarted.RegisterAction(&execAction{srv: f.srv})
			restarted.loadPersistedTimers(ctx)
			f.srv.sweepScheduledExecRuns(ctx)

			got := f.getEvent(t, evt.ID)
			assert.Equal(t, store.ScheduledEventFailed, got.Status, "AC10: no claimed exec stays successful without an outcome")
			assert.Equal(t, execReasonNotDispatched, got.Error)
			_, runErr := f.store.GetScheduledExecRun(ctx, evt.ID)
			assert.ErrorIs(t, runErr, store.ErrNotFound)
			assert.Zero(t, f.capture.count(execRoutePath(agentcontrol.RouteExecStart)), "recovery never sends exec-start")

			assert.Equal(t, claim, f.getEvent(t, fresh.ID).Status, "a claim inside the window may still be admitted")

			// A fire whose window has passed cannot add a run afterwards
			// either: the event is failed, and a late insert would only
			// leave a run whose outcome cannot touch the event.
			f.srv.sweepScheduledExecRuns(ctx)
			assert.Equal(t, store.ScheduledEventFailed, f.getEvent(t, evt.ID).Status)
		})
	}
}
