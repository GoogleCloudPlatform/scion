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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agentcontrol"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/control"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Exec event fires through the real exec-start and exec-status routes on
// the agent's Conduit session (ptone/scion#7883): sciontool's control
// handler runs in-process and executes the commands on this host.

// execRouteFixture is the PTY conduit fixture with exec schedules on, the
// launched agent running and owned by the fixture owner, and fast tracker
// polls.
type execRouteFixture struct {
	*ptyConduitFixture
	capture *execCapture
	opts    control.Options
	ca      *controlAgent
	owner   Identity
}

func newExecRouteFixture(t *testing.T) *execRouteFixture {
	t.Helper()
	setExecTiming(t, &scheduledExecPollInitial, 20*time.Millisecond)
	setExecTiming(t, &scheduledExecPollMax, 50*time.Millisecond)
	f := newPTYConduitFixture(t)
	enableExecSchedules(t, f.srv)
	ctx := context.Background()
	a, err := f.store.GetAgent(ctx, f.launched.ID)
	require.NoError(t, err)
	a.Phase = "running"
	require.NoError(t, f.store.UpdateAgent(ctx, a))
	got, err := f.store.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, f.launched.RunID, got.RunID)
	f.launched = got
	owner, err := f.store.GetUser(ctx, got.OwnerID)
	require.NoError(t, err)
	// The scheduler is created by StartBackgroundServices, which test
	// servers do not run: build one with the exec handler.
	sched := NewScheduler(f.store, slog.Default())
	sched.RegisterEventHandler(scheduledEventTypeExec, f.srv.execEventHandler())
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

// setExecTiming sets a duration test seam for the test.
func setExecTiming(t *testing.T, v *time.Duration, d time.Duration) {
	t.Helper()
	prev := *v
	*v = d
	t.Cleanup(func() { *v = prev })
}

// startAgent runs the agent's session with the exec routes registered and
// advertised.
func (f *execRouteFixture) startAgent(t *testing.T) {
	t.Helper()
	f.ca = f.startControlAgentWithOptions(t, f.public.URL, true, f.opts, f.capture.wrap)
}

// event stores a pending one-shot exec event on the launched agent with
// the owner's session revision.
func (f *execRouteFixture) event(t *testing.T, timeout int, argv ...string) store.ScheduledEvent {
	t.Helper()
	p := scheduledExecPayload{AgentID: f.launched.ID, AgentName: f.launched.Slug, Command: argv, TimeoutSeconds: timeout}
	raw, err := p.encode()
	require.NoError(t, err)
	evt := withSessionRevision(store.ScheduledEvent{
		ID:        api.NewUUID(),
		ProjectID: f.launched.ProjectID,
		EventType: scheduledEventTypeExec,
		Payload:   raw,
		Status:    store.ScheduledEventPending,
		CreatedBy: f.launched.OwnerID,
		FireAt:    time.Now(),
	}, f.launched.OwnerID)
	require.NoError(t, f.store.CreateScheduledEvent(context.Background(), &evt))
	return evt
}

// fire runs the scheduler's one-shot fire of evt (claim, handler, record).
func (f *execRouteFixture) fire(evt store.ScheduledEvent) {
	f.srv.scheduler.fireEvent(context.Background(), evt, false)
}

func (f *execRouteFixture) getEvent(t *testing.T, id string) *store.ScheduledEvent {
	t.Helper()
	evt, err := f.store.GetScheduledEvent(context.Background(), id)
	require.NoError(t, err)
	return evt
}

func (f *execRouteFixture) getRun(t *testing.T, id string) *store.ScheduledExecRun {
	t.Helper()
	run, err := f.store.GetScheduledExecRun(context.Background(), id)
	require.NoError(t, err)
	return run
}

// waitFinished waits for the run of event id to record its outcome.
func (f *execRouteFixture) waitFinished(t *testing.T, id string) *store.ScheduledExecRun {
	t.Helper()
	var run *store.ScheduledExecRun
	require.Eventually(t, func() bool {
		r, err := f.store.GetScheduledExecRun(context.Background(), id)
		if err != nil || r.FinishedAt == nil {
			return false
		}
		run = r
		return true
	}, 20*time.Second, 10*time.Millisecond, "run %s never finished", id)
	return run
}

// waitUntracked waits until no tracker of this replica owns event id.
func (f *execRouteFixture) waitUntracked(t *testing.T, id string) {
	t.Helper()
	require.Eventually(t, func() bool { return !f.srv.execRuns.owns(id) }, 10*time.Second, 10*time.Millisecond)
}

// waitAgentFinished polls exec-status directly until the agent reports the
// run finished.
func (f *execRouteFixture) waitAgentFinished(t *testing.T, id string) {
	t.Helper()
	require.Eventually(t, func() bool {
		res, err := f.srv.AgentExecRunStatus(context.Background(), f.launched.ID, f.launched.RunID, id)
		return err == nil && res.Finished()
	}, 20*time.Second, 20*time.Millisecond)
}

func intp(v int) *int { return &v }

// TestScheduledExecFire_RealRoutes_Outcomes (AC5, D6): exit 0 fires; a
// non-zero exit, a timeout and a start failure fail the event with their
// generic text; the run records the numbers; GET shows execSummary.
func TestScheduledExecFire_RealRoutes_Outcomes(t *testing.T) {
	cases := []struct {
		name        string
		argv        []string
		timeout     int
		wantStatus  string
		wantError   string
		wantOutcome string
		wantExit    *int
	}{
		{"exit 0", []string{"sh", "-c", "exit 0"}, 10, store.ScheduledEventFired, "", store.ScheduledExecOutcomeOK, intp(0)},
		{"exit 3", []string{"sh", "-c", "exit 3"}, 10, store.ScheduledEventFailed, "exit status 3", store.ScheduledExecOutcomeNonzero, intp(3)},
		{"timeout", []string{"sleep", "30"}, 1, store.ScheduledEventFailed, "timed out after 1s", store.ScheduledExecOutcomeTimeout, nil},
		{"start failed", []string{"/nonexistent/exec-schedules-cmd"}, 10, store.ScheduledEventFailed, "command failed to start", store.ScheduledExecOutcomeError, nil},
	}
	f := newExecRouteFixture(t)
	f.startAgent(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			evt := f.event(t, tc.timeout, tc.argv...)
			f.fire(evt)
			run := f.waitFinished(t, evt.ID)
			assert.Equal(t, tc.wantOutcome, run.Outcome)
			if tc.wantExit != nil {
				require.NotNil(t, run.ExitCode)
				assert.Equal(t, *tc.wantExit, *run.ExitCode)
				assert.NotNil(t, run.DurationMS)
				assert.NotNil(t, run.OutputBytes)
			}
			got := f.getEvent(t, evt.ID)
			assert.Equal(t, tc.wantStatus, got.Status)
			assert.Equal(t, tc.wantError, got.Error)
			assert.Equal(t, agentcontrol.ExecArgvDigest(tc.argv), run.CommandSHA256)
			assert.Equal(t, len(tc.argv), run.Argc)
			f.waitUntracked(t, evt.ID)

			rec := requestAsIdentity(t, f.srv, f.owner, http.MethodGet,
				"/api/v1/projects/"+f.launched.ProjectID+"/scheduled-events/"+evt.ID, nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var resp ScheduledEventResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			require.NotNil(t, resp.ExecSummary)
			assert.Equal(t, tc.wantOutcome, resp.ExecSummary.Outcome)
			assert.NotNil(t, resp.ExecSummary.FinishedAt)
		})
	}
}

// TestScheduledExecFire_NoOutputAnywhere (AC6, SEC-P3): a sentinel the
// command prints, on stdout and stderr, exit 0 and non-zero, appears in no
// stored row, no API response and no hub or sciontool log line, and no
// audit record carries argv beyond argv0.
func TestScheduledExecFire_NoOutputAnywhere(t *testing.T) {
	logs := captureDefaultLog(t)
	f := newExecRouteFixture(t)
	f.startAgent(t)
	tag := strings.ReplaceAll(uuid.NewString(), "-", "")
	// The printed sentinel is the concatenation, which never appears in
	// argv itself.
	outSentinel := "OUTPUTSENTINEL" + tag
	argSentinel := "ARGSENTINEL" + tag
	for _, code := range []int{0, 7} {
		script := `printf '%s%s\n' OUTPUTSENTINEL "$1"; printf '%s%s\n' OUTPUTSENTINEL "$1" >&2; exit ` + map[int]string{0: "0", 7: "7"}[code]
		evt := f.event(t, 10, "sh", "-c", script, argSentinel, tag)
		f.fire(evt)
		run := f.waitFinished(t, evt.ID)
		require.NotNil(t, run.OutputBytes)
		assert.Greater(t, *run.OutputBytes, int64(0), "output was produced and counted")
		f.waitUntracked(t, evt.ID)
	}

	// Stored rows.
	ctx := context.Background()
	events, err := f.store.ListScheduledEvents(ctx, store.ScheduledEventFilter{ProjectID: f.launched.ProjectID}, store.ListOptions{Limit: 100})
	require.NoError(t, err)
	var ids []string
	for _, e := range events.Items {
		ids = append(ids, e.ID)
		assert.NotContains(t, e.Error, outSentinel)
		assert.NotContains(t, e.Payload, outSentinel)
	}
	runs, err := f.store.ListScheduledExecRunsByEvent(ctx, ids)
	require.NoError(t, err)
	require.Len(t, runs, 2)
	rowJSON, err := json.Marshal(runs)
	require.NoError(t, err)
	assert.NotContains(t, string(rowJSON), outSentinel)
	assert.NotContains(t, string(rowJSON), argSentinel)

	// API responses (the owner holds attach, so argv itself is revealed).
	for _, path := range []string{
		"/api/v1/projects/" + f.launched.ProjectID + "/scheduled-events",
		"/api/v1/projects/" + f.launched.ProjectID + "/scheduled-events/" + ids[0],
	} {
		rec := requestAsIdentity(t, f.srv, f.owner, http.MethodGet, path, nil)
		require.Equal(t, http.StatusOK, rec.Code)
		assert.NotContains(t, rec.Body.String(), outSentinel)
	}

	// Logs and audit records.
	out := logs.String()
	assert.NotContains(t, out, outSentinel, "no output in any log")
	assert.NotContains(t, out, argSentinel, "no argv beyond argv0 in any log")
	assert.Contains(t, out, "audit_action=schedule.exec.admit")
	assert.Contains(t, out, "audit_action=schedule.exec.outcome")
	assert.Contains(t, out, "argv0=sh")
	assert.Contains(t, out, "shell_form=true")
}

// TestScheduledExecFire_RealRoutes_Skips (D4, AC4): no session, a session
// without the routes, and an agent at its live-run cap skip the event; the
// admitted run records skipped.
func TestScheduledExecFire_RealRoutes_Skips(t *testing.T) {
	t.Run("no session", func(t *testing.T) {
		f := newExecRouteFixture(t)
		evt := f.event(t, 10, "true")
		f.fire(evt)
		got := f.getEvent(t, evt.ID)
		assert.Equal(t, store.ScheduledEventSkipped, got.Status)
		assert.Equal(t, execReasonUnavailable, got.Error)
		assert.Equal(t, store.ScheduledExecOutcomeSkipped, f.getRun(t, evt.ID).Outcome)
		assert.False(t, f.srv.execRuns.owns(evt.ID))
	})
	t.Run("routes not advertised", func(t *testing.T) {
		f := newExecRouteFixture(t)
		ca := f.startControlAgentWithOptions(t, f.public.URL, false, f.opts, nil)
		evt := f.event(t, 10, "true")
		f.fire(evt)
		got := f.getEvent(t, evt.ID)
		assert.Equal(t, store.ScheduledEventSkipped, got.Status)
		assert.Equal(t, execReasonUnavailable, got.Error)
		assert.Zero(t, ca.rpcs.Load(), "nothing sent")
	})
	t.Run("agent busy", func(t *testing.T) {
		f := newExecRouteFixture(t)
		f.startAgent(t)
		var long []store.ScheduledEvent
		for i := 0; i < 2; i++ {
			evt := f.event(t, 30, "sleep", "2")
			f.fire(evt)
			long = append(long, evt)
		}
		evt := f.event(t, 10, "true")
		f.fire(evt)
		got := f.getEvent(t, evt.ID)
		assert.Equal(t, store.ScheduledEventSkipped, got.Status)
		assert.Equal(t, execReasonCapacity, got.Error)
		assert.Equal(t, store.ScheduledExecOutcomeSkipped, f.getRun(t, evt.ID).Outcome)
		for _, e := range long {
			assert.Equal(t, store.ScheduledExecOutcomeOK, f.waitFinished(t, e.ID).Outcome)
		}
	})
}

// TestScheduledExecFire_DetachedFromHandler (AC9): the fire returns once
// exec-start answers; the command runs on, and its outcome is recorded
// later by the tracker.
func TestScheduledExecFire_DetachedFromHandler(t *testing.T) {
	f := newExecRouteFixture(t)
	f.startAgent(t)
	evt := f.event(t, 30, "sleep", "3")
	start := time.Now()
	f.fire(evt)
	assert.Less(t, time.Since(start), 2*time.Second, "the fire does not wait for the command")
	got := f.getEvent(t, evt.ID)
	assert.Equal(t, store.ScheduledEventFired, got.Status)
	run := f.getRun(t, evt.ID)
	assert.Nil(t, run.FinishedAt)
	assert.Equal(t, store.ScheduledExecOutcomeRunning, run.Outcome)
	assert.True(t, f.srv.execRuns.owns(evt.ID), "tracked")
	assert.Equal(t, store.ScheduledExecOutcomeOK, f.waitFinished(t, evt.ID).Outcome)
}

// TestScheduledExecFire_HubGate (AC18): turning either hub flag off stops
// new starts (skipped, nothing sent) but the tracker still recovers the
// outcome of a run admitted before.
func TestScheduledExecFire_HubGate(t *testing.T) {
	for _, off := range []string{conduitExperiment, experiments.ExecSchedules} {
		t.Run(off+" off", func(t *testing.T) {
			f := newExecRouteFixture(t)
			f.startAgent(t)
			admitted := f.event(t, 30, "sh", "-c", "sleep 1; exit 5")
			f.fire(admitted)
			require.Equal(t, store.ScheduledExecOutcomeRunning, f.getRun(t, admitted.ID).Outcome)

			setHubExperiments(t, f.srv, map[string]bool{conduitExperiment: off != conduitExperiment, experiments.ExecSchedules: off != experiments.ExecSchedules})
			starts := f.capture.count(execRoutePath(agentcontrol.RouteExecStart))

			fresh := f.event(t, 10, "true")
			f.fire(fresh)
			got := f.getEvent(t, fresh.ID)
			assert.Equal(t, store.ScheduledEventSkipped, got.Status)
			assert.Equal(t, execReasonDisabled, got.Error)
			assert.Equal(t, starts, f.capture.count(execRoutePath(agentcontrol.RouteExecStart)), "no new exec-start")
			_, err := f.srv.StartAgentExecRun(context.Background(), f.launched.ID, f.launched.RunID,
				agentcontrol.ExecStartRequest{RunID: uuid.NewString(), Argv: []string{"true"}, TimeoutS: 1})
			require.ErrorIs(t, err, ErrExecSchedulesDisabled)

			run := f.waitFinished(t, admitted.ID)
			assert.Equal(t, store.ScheduledExecOutcomeNonzero, run.Outcome, "recovery continues with the flag off")
			assert.Equal(t, "exit status 5", f.getEvent(t, admitted.ID).Error)

			// The status client itself is not gated.
			res, err := f.srv.AgentExecRunStatus(context.Background(), f.launched.ID, f.launched.RunID, admitted.ID)
			require.NoError(t, err)
			assert.True(t, res.Finished())
		})
	}
}

// TestScheduledExecFire_HubRestartSweeperRecordsOutcome (AC10): a tracker
// lost to a hub shutdown records nothing; after the restart the sweeper
// polls the run and records its real outcome, and exec-start is never
// resent.
func TestScheduledExecFire_HubRestartSweeperRecordsOutcome(t *testing.T) {
	// The shutdown cancels the tracker at once, well before the run ends.
	setExecTiming(t, &scheduledExecStopGrace, 10*time.Millisecond)
	f := newExecRouteFixture(t)
	f.startAgent(t)
	evt := f.event(t, 30, "sh", "-c", "sleep 2; exit 6")
	f.fire(evt)
	require.True(t, f.srv.execRuns.owns(evt.ID))

	// Shutdown: admission closes and the tracker is cancelled.
	f.srv.execRuns.work.closeAdmission()
	f.srv.stopScheduledExecRuns(context.Background())
	f.waitUntracked(t, evt.ID)
	run := f.getRun(t, evt.ID)
	require.Nil(t, run.FinishedAt, "shutdown records no outcome for an admitted run")
	assert.Equal(t, store.ScheduledEventFired, f.getEvent(t, evt.ID).Status)

	f.waitAgentFinished(t, evt.ID)
	f.srv.sweepScheduledExecRuns(context.Background())
	run = f.getRun(t, evt.ID)
	require.NotNil(t, run.FinishedAt)
	assert.Equal(t, store.ScheduledExecOutcomeNonzero, run.Outcome)
	assert.Equal(t, 6, *run.ExitCode)
	got := f.getEvent(t, evt.ID)
	assert.Equal(t, store.ScheduledEventFailed, got.Status)
	assert.Equal(t, "exit status 6", got.Error)
	assert.Equal(t, 1, f.capture.count(execRoutePath(agentcontrol.RouteExecStart)), "exec-start never resent")

	// After shutdown a new fire does not start.
	late := f.event(t, 10, "true")
	f.fire(late)
	assert.Equal(t, execReasonShuttingDown, f.getEvent(t, late.ID).Error)
	_, err := f.store.GetScheduledExecRun(context.Background(), late.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// TestScheduledExecFire_TrackerVsSweeper: the tracker and repeated sweeps
// poll the same run concurrently; exactly one outcome is recorded and
// audited.
func TestScheduledExecFire_TrackerVsSweeper(t *testing.T) {
	logs := captureDefaultLog(t)
	f := newExecRouteFixture(t)
	f.startAgent(t)
	evt := f.event(t, 30, "sh", "-c", "sleep 0.5; exit 2")
	f.fire(evt)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				// The sweeper skips runs a tracker of this replica owns;
				// poll the row directly as another replica would.
				run, err := f.store.GetScheduledExecRun(context.Background(), evt.ID)
				if err == nil && run.FinishedAt == nil {
					var unavailable bool
					f.srv.pollScheduledExecRunOnce(context.Background(), run, &unavailable)
				}
				f.srv.sweepScheduledExecRuns(context.Background())
			}
		}()
	}
	run := f.waitFinished(t, evt.ID)
	f.waitUntracked(t, evt.ID)
	close(stop)
	wg.Wait()
	assert.Equal(t, store.ScheduledExecOutcomeNonzero, run.Outcome)
	assert.Equal(t, "exit status 2", f.getEvent(t, evt.ID).Error)
	assert.Equal(t, 1, strings.Count(logs.String(), "audit_action=schedule.exec.outcome project_id="+f.launched.ProjectID+" scheduled_event_id="+evt.ID),
		"one outcome recorded")
}

// TestScheduledExecFire_AgentStoppedMidRun: the session ends while the run
// is live; the tracker keeps polling until the lease, then records
// abandoned with the stopped text.
func TestScheduledExecFire_AgentStoppedMidRun(t *testing.T) {
	setExecTiming(t, &scheduledExecLeaseGrace, time.Second)
	f := newExecRouteFixture(t)
	f.startAgent(t)
	evt := f.event(t, 1, "sleep", "30")
	f.fire(evt)
	f.ca.stop()
	run := f.waitFinished(t, evt.ID)
	assert.Equal(t, store.ScheduledExecOutcomeAbandoned, run.Outcome)
	got := f.getEvent(t, evt.ID)
	assert.Equal(t, store.ScheduledEventFailed, got.Status)
	assert.Equal(t, execReasonAgentStopped, got.Error)
}

// TestScheduledExecFire_AgentRestartedMidRun: a run is never queried on a
// replacement incarnation (D9-S3), and an agent that forgot a run it
// reported running restarted; both record abandoned.
func TestScheduledExecFire_AgentRestartedMidRun(t *testing.T) {
	t.Run("launch changed", func(t *testing.T) {
		f := newExecRouteFixture(t)
		f.startAgent(t)
		evt := f.event(t, 30, "sleep", "5")
		f.fire(evt)
		_, err := f.store.SetAgentRunID(context.Background(), f.launched.ID, uuid.NewString(), nil)
		require.NoError(t, err)
		run := f.waitFinished(t, evt.ID)
		assert.Equal(t, store.ScheduledExecOutcomeAbandoned, run.Outcome)
		assert.Equal(t, execReasonAgentRestarted, f.getEvent(t, evt.ID).Error)
	})
	t.Run("same launch, run forgotten", func(t *testing.T) {
		f := newExecRouteFixture(t)
		f.startAgent(t)
		evt := f.event(t, 30, "sleep", "5")
		f.fire(evt)
		require.Equal(t, store.ScheduledExecOutcomeRunning, f.getRun(t, evt.ID).Outcome)
		// A new sciontool (fresh run store) on the same launch.
		f.ca.stop()
		f.startControlAgentWithOptions(t, f.public.URL, true, execControlOptions(t), nil)
		run := f.waitFinished(t, evt.ID)
		assert.Equal(t, store.ScheduledExecOutcomeAbandoned, run.Outcome)
		assert.Equal(t, execReasonAgentRestarted, f.getEvent(t, evt.ID).Error)
	})
}

// TestScheduledExecFire_UnknownStart: a run never seen running that the
// agent does not know stays unresolved inside the admission window and is
// recorded as a dispatch failure once it has closed.
func TestScheduledExecFire_UnknownStart(t *testing.T) {
	f := newExecRouteFixture(t)
	f.startAgent(t)
	evt := f.event(t, 30, "true")
	claimed, err := f.store.(store.ScheduledEventClaimer).ClaimScheduledEvent(context.Background(), evt.ID, store.ScheduledEventFired)
	require.NoError(t, err)
	require.True(t, claimed)
	run := store.ScheduledExecRun{
		EventID: evt.ID, ProjectID: evt.ProjectID, TargetAgentID: f.launched.ID,
		CommandSHA256: agentcontrol.ExecArgvDigest([]string{"true"}), Argc: 1, TimeoutSeconds: 30,
		StartedAt: time.Now(), LeaseUntil: time.Now().Add(time.Hour), LaunchID: f.launched.RunID,
	}
	require.NoError(t, f.store.CreateScheduledExecRun(context.Background(), &run))
	var unavailable bool
	assert.False(t, f.srv.pollScheduledExecRunOnce(context.Background(), &run, &unavailable), "inside the window: keep polling")
	assert.Nil(t, f.getRun(t, evt.ID).FinishedAt)

	setExecTiming(t, &scheduledExecStartUnknownAfter, 0)
	assert.True(t, f.srv.pollScheduledExecRunOnce(context.Background(), &run, &unavailable))
	got := f.getRun(t, evt.ID)
	assert.Equal(t, store.ScheduledExecOutcomeError, got.Outcome)
	assert.Equal(t, execReasonDispatchFailed, f.getEvent(t, evt.ID).Error)
	assert.Zero(t, f.capture.count(execRoutePath(agentcontrol.RouteExecStart)), "recovery never starts a run")
}

// TestScheduledExecFire_AmbiguousStartNeverResent: the session drops while
// exec-start is in flight; the hub does not know whether the run started,
// tracks it with exec-status only, and records it at the lease.
func TestScheduledExecFire_AmbiguousStartNeverResent(t *testing.T) {
	setExecTiming(t, &scheduledExecLeaseGrace, time.Second)
	f := newExecRouteFixture(t)
	var stopOnce sync.Once
	f.capture.answer = func(req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
		if req.GetPath() != execRoutePath(agentcontrol.RouteExecStart) {
			return nil
		}
		stopOnce.Do(func() { go f.ca.stop() })
		time.Sleep(2 * time.Second)
		return &conduitv1.RpcResponse{Status: 202, Body: []byte(`{"state":"running"}`)}
	}
	f.startAgent(t)
	evt := f.event(t, 1, "true")
	f.fire(evt)
	run := f.waitFinished(t, evt.ID)
	assert.Contains(t, []string{store.ScheduledExecOutcomeAbandoned, store.ScheduledExecOutcomeSkipped}, run.Outcome)
	assert.Equal(t, 1, f.capture.count(execRoutePath(agentcontrol.RouteExecStart)), "exec-start sent once")
	got := f.getEvent(t, evt.ID)
	assert.NotEqual(t, store.ScheduledEventFired, got.Status, "no event stays fired with no outcome")
}

// TestScheduledExecFire_PurgeDuringRun: a purge that removes the event and
// run while the tracker polls leaves nothing behind and the tracker ends.
func TestScheduledExecFire_PurgeDuringRun(t *testing.T) {
	f := newExecRouteFixture(t)
	f.startAgent(t)
	evt := f.event(t, 30, "sleep", "1")
	f.fire(evt)
	require.True(t, f.srv.execRuns.owns(evt.ID))
	n, err := f.store.PurgeOldScheduledEvents(context.Background(), time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.GreaterOrEqual(t, n, 1)
	f.waitUntracked(t, evt.ID)
	_, err = f.store.GetScheduledEvent(context.Background(), evt.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = f.store.GetScheduledExecRun(context.Background(), evt.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// TestScheduledExecFire_TargetDeletedMidRun: deleting the target while its
// run is live still records an outcome; no event stays fired.
func TestScheduledExecFire_TargetDeletedMidRun(t *testing.T) {
	f := newExecRouteFixture(t)
	f.startAgent(t)
	evt := f.event(t, 30, "sleep", "1")
	f.fire(evt)
	require.NoError(t, f.store.DeleteAgent(context.Background(), f.launched.ID))
	run := f.waitFinished(t, evt.ID)
	assert.NotEmpty(t, run.Outcome)
	assert.NotEqual(t, store.ScheduledEventFired, f.getEvent(t, evt.ID).Status)
	f.waitUntracked(t, evt.ID)
}

// TestScheduledExecFire_SweeperLeaseAndFairness: the sweeper records runs
// past their lease as abandoned, and orders the rest round-robin across
// agents.
func TestScheduledExecFire_SweeperLeaseAndFairness(t *testing.T) {
	f := newExecRouteFixture(t)
	ctx := context.Background()
	evt := f.event(t, 30, "true")
	_, err := f.store.(store.ScheduledEventClaimer).ClaimScheduledEvent(ctx, evt.ID, store.ScheduledEventFired)
	require.NoError(t, err)
	past := time.Now().Add(-time.Hour)
	require.NoError(t, f.store.CreateScheduledExecRun(ctx, &store.ScheduledExecRun{
		EventID: evt.ID, ProjectID: evt.ProjectID, TargetAgentID: f.launched.ID,
		CommandSHA256: agentcontrol.ExecArgvDigest([]string{"true"}), Argc: 1, TimeoutSeconds: 30,
		StartedAt: past, LeaseUntil: past.Add(time.Minute), LaunchID: f.launched.RunID,
		Outcome: store.ScheduledExecOutcomeRunning,
	}))
	f.srv.sweepScheduledExecRuns(ctx)
	run := f.getRun(t, evt.ID)
	assert.Equal(t, store.ScheduledExecOutcomeAbandoned, run.Outcome)
	got := f.getEvent(t, evt.ID)
	assert.Equal(t, store.ScheduledEventFailed, got.Status)
	assert.Equal(t, execReasonOutcomeUnknown, got.Error)

	ordered := roundRobinByAgent([]store.ScheduledExecRun{
		{EventID: "1", TargetAgentID: "a"}, {EventID: "2", TargetAgentID: "a"}, {EventID: "3", TargetAgentID: "a"},
		{EventID: "4", TargetAgentID: "b"}, {EventID: "5", TargetAgentID: "c"},
	})
	var got2 []string
	for _, r := range ordered {
		got2 = append(got2, r.EventID)
	}
	assert.Equal(t, []string{"1", "4", "5", "2", "3"}, got2)
}
