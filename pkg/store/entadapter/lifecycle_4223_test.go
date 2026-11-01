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

package entadapter

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	tc "github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

// The failed report after running_observed (upstream ptone/scion#5368, in
// failedReportEndUpdate via applyLaunchReportEnded) keeps LaunchReport
// evidence only: it gets no LaunchEnd hint, so it never derives
// launch_failed, and the phase rules decide by first match. A launch
// report is always run-correlated evidence (E(iii)), so for running ->
// error: with no open segment P7a fires (stopped(error, start_unobserved));
// with an open segment, P5 (stopped(error)); P5b, which needs no evidence,
// is unreachable here; an error -> error write is P1 (no transition). A
// start-kind launch ends in its stopped failure phase (P7 or P7a). The
// active pre-running failure, now written by the same helper via
// applyLaunchReportPreRunning, keeps its LaunchEnd hint and launch_failed
// (TestLifecycleStore2_LaunchReports).

// new4223Fixture is newS2Fixture with no live activity: makeAgent sets
// Activity "thinking", and #4223's path 2 applies only to a row with no
// live activity (failedAfterRunningObservedApplies), as upstream's
// endRunningObserved helper arranges.
func new4223Fixture(t *testing.T) (*lifecycleFixture, *store.Agent) {
	t.Helper()
	f := newLifecycleFixture(t)
	a := f.create("s4223-"+strings.ToLower(t.Name()[len(t.Name())-6:]), func(a *store.Agent) {
		a.Phase = "created"
		a.Generation = 1
		a.Activity = ""
	})
	return f, a
}

// requireNoLiveActivity checks path 2's activity precondition.
func requireNoLiveActivity(t *testing.T, f *lifecycleFixture, id string) {
	t.Helper()
	got, err := f.cs.GetAgent(context.Background(), id)
	require.NoError(t, err)
	require.Empty(t, got.Activity, "setup: #4223 path 2 needs no live activity")
}

// failedAfterRunningObserved returns a failed report for the row's
// current run, which applyLaunchReportEnded applies to a launch that
// ended running_observed.
func failedAfterRunningObserved(launchID, runID string) store.LaunchReport {
	return store.LaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateFailed,
		Step: "workspace_sync", Message: "launch timed out before the agent started",
		ErrorCode: "launch_timeout", RunID: runID,
	}
}

// runningObservedLaunch begins a create launch for run R1 and ends it as
// running_observed with a start write, which opens R1's segment.
func runningObservedLaunch(t *testing.T, f *lifecycleFixture, id string) string {
	t.Helper()
	ctx := context.Background()
	f.mint(id, "R1")
	launchID, err := f.cs.BeginLaunch(ctx, id, store.LaunchKindCreate, time.Hour)
	require.NoError(t, err)
	f.startWrite(id)
	got, err := f.cs.GetAgent(ctx, id)
	require.NoError(t, err)
	require.Equal(t, store.LaunchStateEnded, got.LaunchState)
	require.Equal(t, store.LaunchEndReasonRunningObserved, got.LaunchEndReason)
	return launchID
}

func requireNoTransition(t *testing.T, evs []*tc.LifecycleEvent, transition string) {
	t.Helper()
	for _, e := range evs {
		require.NotEqual(t, transition, e.Transition, "unexpected %s: %s", transition, lcFull(evs))
	}
}

// TestLifecycleStore4223_FailedAfterRunningObservedOpenSegment: with R1's
// segment open, the failed report's running -> error write derives P5,
// stopped(error), and no launch_failed. The stored end reason is the
// store's (failed), unchanged by the derivation.
func TestLifecycleStore4223_FailedAfterRunningObservedOpenSegment(t *testing.T) {
	f, a := new4223Fixture(t)
	ctx := context.Background()
	launchID := runningObservedLaunch(t, f, a.ID)
	before := f.events(a.ID)
	require.Equal(t, tc.TransitionStarted, before[len(before)-1].Transition, lcFull(before))

	requireNoLiveActivity(t, f, a.ID)
	answer, _, err := f.cs.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, failedAfterRunningObserved(launchID, "R1"))
	require.NoError(t, err)
	require.Equal(t, store.LaunchReportResultApplied, answer.Result)

	got, err := f.cs.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, "error", got.Phase)
	assert.Equal(t, store.LaunchEndReasonFailed, got.LaunchEndReason, "the stored end reason is the store's, not the derivation's")

	added := f.events(a.ID)[len(before):]
	require.Len(t, added, 1, lcFull(added))
	// P5 exactly: stopped(error) with NO cause. A P7a (cause
	// start_unobserved) would mean the open segment was lost.
	assert.Empty(t, added[0].Cause, lcFull(added))
	require.Equal(t, fmt.Sprintf("stopped@%d(error)[R1/1]", before[len(before)-1].TransitionRevision+1), lcFull(added))
	requireNoTransition(t, added, tc.TransitionLaunchFailed)
}

// TestLifecycleStore4223_FailedAfterRunningObservedNoSegment: a plain
// running write (no evidence, P2) ends the launch running_observed without
// opening a segment, so the failed report's running -> error write, which
// is launch-report evidence, derives P7a: stopped(error, start_unobserved),
// and no launch_failed.
func TestLifecycleStore4223_FailedAfterRunningObservedNoSegment(t *testing.T) {
	f, a := new4223Fixture(t)
	ctx := context.Background()
	f.mint(a.ID, "R1")
	launchID, err := f.cs.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, time.Hour)
	require.NoError(t, err)
	f.status(ctx, a.ID, store.AgentStatusUpdate{Phase: "running"})
	got, err := f.cs.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, store.LaunchEndReasonRunningObserved, got.LaunchEndReason)
	before := f.events(a.ID)
	requireNoTransition(t, before, tc.TransitionStarted)

	requireNoLiveActivity(t, f, a.ID)
	answer, _, err := f.cs.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, failedAfterRunningObserved(launchID, "R1"))
	require.NoError(t, err)
	require.Equal(t, store.LaunchReportResultApplied, answer.Result)

	added := f.events(a.ID)[len(before):]
	require.Len(t, added, 1, lcFull(added))
	assert.Equal(t, tc.TransitionStopped, added[0].Transition, lcFull(added))
	assert.Equal(t, tc.StopKindError, added[0].StopKind, lcFull(added))
	assert.Equal(t, tc.CauseStartUnobserved, added[0].Cause, lcFull(added))
	requireNoTransition(t, added, tc.TransitionLaunchFailed)
}

// TestLifecycleStore4223_FailedAfterRunningObservedAlreadyError: a
// heartbeat relayed the broker's error first, so the failed report's write
// is error -> error (P1): it adds neither a stop nor a launch_failed.
func TestLifecycleStore4223_FailedAfterRunningObservedAlreadyError(t *testing.T) {
	f, a := new4223Fixture(t)
	ctx := context.Background()
	launchID := runningObservedLaunch(t, f, a.ID)
	f.status(ctx, a.ID, store.AgentStatusUpdate{Phase: "error"})
	before := f.events(a.ID)

	requireNoLiveActivity(t, f, a.ID)
	answer, _, err := f.cs.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, failedAfterRunningObserved(launchID, "R1"))
	require.NoError(t, err)
	require.Equal(t, store.LaunchReportResultApplied, answer.Result)

	got, err := f.cs.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, store.LaunchEndReasonFailed, got.LaunchEndReason)

	// P1: the failed report's write (failedReportEndUpdate and its save)
	// changes no LifecycleMetadata snapshot or fingerprint field, so neither
	// digest moves and configured or metadata_changed cannot fire (it writes,
	// for example, the phase error -> error, the message, launch_* columns
	// and state_version). It changes no run state or generation (the segment
	// was already closed by the error write), and it carries no LaunchEnd
	// hint, since the launch had ended running_observed. So no event may be
	// added.
	added := f.events(a.ID)[len(before):]
	require.Empty(t, added, lcFull(added))
}

// TestLifecycleStore4223_FailedAfterRunningObservedStartKind: a start-kind
// launch's failed report after running_observed ends in its failure phase
// (stopped, via withStartFailureEnd), not error. With no open segment,
// running -> stopped with launch-report evidence derives P7a with the
// after phase as stop kind: stopped(stopped, start_unobserved), and no
// launch_failed.
func TestLifecycleStore4223_FailedAfterRunningObservedStartKind(t *testing.T) {
	f, a := new4223Fixture(t)
	ctx := context.Background()
	f.status(ctx, a.ID, store.AgentStatusUpdate{Phase: "starting"})
	f.mint(a.ID, "R1")
	launchID, err := f.cs.BeginStartLaunch(ctx, a.ID, store.LaunchKindStart, time.Hour, store.BeginLaunchOptions{PriorPhase: "stopped"})
	require.NoError(t, err)
	f.status(ctx, a.ID, store.AgentStatusUpdate{Phase: "running"})
	got, err := f.cs.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, store.LaunchEndReasonRunningObserved, got.LaunchEndReason)
	before := f.events(a.ID)
	requireNoTransition(t, before, tc.TransitionStarted)

	requireNoLiveActivity(t, f, a.ID)
	answer, _, err := f.cs.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, failedAfterRunningObserved(launchID, "R1"))
	require.NoError(t, err)
	require.Equal(t, store.LaunchReportResultApplied, answer.Result)
	got, err = f.cs.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, "stopped", got.Phase, "a start-kind launch ends in its failure phase")

	added := f.events(a.ID)[len(before):]
	require.Len(t, added, 1, lcFull(added))
	assert.Equal(t, tc.TransitionStopped, added[0].Transition, lcFull(added))
	assert.Equal(t, tc.StopKindStopped, added[0].StopKind, lcFull(added))
	assert.Equal(t, tc.CauseStartUnobserved, added[0].Cause, lcFull(added))
	requireNoTransition(t, added, tc.TransitionLaunchFailed)
}
