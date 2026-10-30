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
// launch_failed, and the phase rules decide by first match (P7a, P5, P5b,
// P1). The active pre-running failure, now written by the same helper via
// applyLaunchReportPreRunning, keeps its LaunchEnd hint and launch_failed
// (TestLifecycleStore2_LaunchReports). P7a and P5b need a before row
// without an open segment, which a running_observed start cannot give
// here; the rule table pins them (derive_table_test.go).

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
	f, a := newS2Fixture(t)
	ctx := context.Background()
	launchID := runningObservedLaunch(t, f, a.ID)
	before := f.events(a.ID)
	require.Equal(t, tc.TransitionStarted, before[len(before)-1].Transition, lcFull(before))

	answer, _, err := f.cs.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, failedAfterRunningObserved(launchID, "R1"))
	require.NoError(t, err)
	require.Equal(t, store.LaunchReportResultApplied, answer.Result)

	got, err := f.cs.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, "error", got.Phase)
	assert.Equal(t, store.LaunchEndReasonFailed, got.LaunchEndReason, "the stored end reason is the store's, not the derivation's")

	added := f.events(a.ID)[len(before):]
	require.Len(t, added, 1, lcFull(added))
	assert.Equal(t, tc.TransitionStopped, added[0].Transition, lcFull(added))
	assert.Equal(t, tc.StopKindError, added[0].StopKind, lcFull(added))
	requireNoTransition(t, added, tc.TransitionLaunchFailed)
}

// TestLifecycleStore4223_FailedAfterRunningObservedAlreadyError: a
// heartbeat relayed the broker's error first, so the failed report's write
// is error -> error (P1): it adds neither a stop nor a launch_failed.
func TestLifecycleStore4223_FailedAfterRunningObservedAlreadyError(t *testing.T) {
	f, a := newS2Fixture(t)
	ctx := context.Background()
	launchID := runningObservedLaunch(t, f, a.ID)
	f.status(ctx, a.ID, store.AgentStatusUpdate{Phase: "error"})
	before := f.events(a.ID)

	answer, _, err := f.cs.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, failedAfterRunningObserved(launchID, "R1"))
	require.NoError(t, err)
	require.Equal(t, store.LaunchReportResultApplied, answer.Result)

	got, err := f.cs.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, store.LaunchEndReasonFailed, got.LaunchEndReason)

	added := f.events(a.ID)[len(before):]
	requireNoTransition(t, added, tc.TransitionLaunchFailed)
	requireNoTransition(t, added, tc.TransitionStopped)
}
