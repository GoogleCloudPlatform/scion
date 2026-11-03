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

// Slice 2 lifecycle scenarios through the real store writers on SQLite
// (WP1 design section 8): each test drives the observed writers in the
// order a hub path makes them and asserts the exact event sequence, with
// revisions, run identity and generation.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/lifecycleevents"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	tc "github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetryemit"
)

// lcFull renders events with their distinguishing fields:
// transition@rev(kind,cause,reason,code)[launch_id/incarnation].
func lcFull(evs []*tc.LifecycleEvent) string {
	parts := make([]string, len(evs))
	for i, e := range evs {
		s := fmt.Sprintf("%s@%d", e.Transition, e.TransitionRevision)
		var attrs []string
		for _, a := range []string{e.StopKind, e.Cause, e.EndReason, e.ErrorCode, e.DeleteMode} {
			if a != "" {
				attrs = append(attrs, a)
			}
		}
		if len(attrs) > 0 {
			s += "(" + strings.Join(attrs, ",") + ")"
		}
		s += "[" + e.LaunchID + "/" + e.IncarnationID + "]"
		parts[i] = s
	}
	return strings.Join(parts, " ")
}

// newS2Fixture is a lifecycle fixture whose agent starts in phase created.
func newS2Fixture(t *testing.T) (*lifecycleFixture, *store.Agent) {
	t.Helper()
	f := newLifecycleFixture(t)
	a := f.create("s2-"+strings.ToLower(t.Name()[len(t.Name())-6:]), func(a *store.Agent) {
		a.Phase = "created"
		a.Generation = 1
	})
	return f, a
}

func (f *lifecycleFixture) mint(id, run string) {
	f.t.Helper()
	_, err := f.cs.SetAgentRunID(context.Background(), id, run, nil)
	require.NoError(f.t, err)
}

func (f *lifecycleFixture) status(ctx context.Context, id string, su store.AgentStatusUpdate) {
	f.t.Helper()
	require.NoError(f.t, f.cs.UpdateAgentStatus(ctx, id, su))
}

// startWrite is a start path's own running write (E(i)).
func (f *lifecycleFixture) startWrite(id string) {
	f.status(context.Background(), id, store.AgentStatusUpdate{Phase: "running", StartWrite: true})
}

// heartbeat is a broker heartbeat naming run (or none).
func (f *lifecycleFixture) heartbeat(id, phase, run string) {
	f.status(context.Background(), id, store.AgentStatusUpdate{Phase: phase, ReportSource: store.StatusReportSourceRuntime, ReportRunID: run})
}

// stopRecord is recordStopStatus's write.
func (f *lifecycleFixture) stopRecord(id, run, phase string) {
	f.status(context.Background(), id, store.AgentStatusUpdate{Phase: phase, IfRunID: run})
}

func (f *lifecycleFixture) events(id string) []*tc.LifecycleEvent {
	f.t.Helper()
	evs, _ := lifecycleEvents(f.t, f.cs, id)
	return evs
}

func (f *lifecycleFixture) update(ctx context.Context, id string, mutate func(a *store.Agent)) {
	f.t.Helper()
	a, err := f.cs.GetAgent(ctx, id)
	require.NoError(f.t, err)
	mutate(a)
	require.NoError(f.t, f.cs.UpdateAgent(ctx, a))
}

// TestLifecycleStore2_StartStopNoResync is r2 B2 through the slice 2
// writers: created -> provisioning -> starting -> running -> stopping ->
// stopped gives started and stopped only, no resynced, dense revisions.
func TestLifecycleStore2_StartStopNoResync(t *testing.T) {
	f, a := newS2Fixture(t)
	ctx := context.Background()
	f.status(ctx, a.ID, store.AgentStatusUpdate{Phase: "provisioning"})
	f.status(ctx, a.ID, store.AgentStatusUpdate{Phase: "starting"})
	f.mint(a.ID, "R1")
	f.startWrite(a.ID)
	f.heartbeat(a.ID, "running", "R1") // a duplicate running heartbeat: nothing
	f.status(ctx, a.ID, store.AgentStatusUpdate{Phase: "stopping"})
	f.stopRecord(a.ID, "R1", "stopped")
	assert.Equal(t, "created@1[/1] started@2[R1/1] stopped@3(stopped)[R1/1]", lcFull(f.events(a.ID)))
	assert.Zero(t, f.counters.Get(telemetryemit.MetricResyncs, tc.CauseUnobservedChange))
}

// TestLifecycleStore2_HTTPRestartOfRunningAgent: the stop leg's
// RecordRestartStopLeg closes R1 before R2 is minted, then the start
// write opens R2 (P1a, running -> running).
func TestLifecycleStore2_HTTPRestartOfRunningAgent(t *testing.T) {
	f, a := newS2Fixture(t)
	f.mint(a.ID, "R1")
	f.startWrite(a.ID)
	require.NoError(t, f.cs.RecordRestartStopLeg(context.Background(), a.ID, "R1"))
	require.NoError(t, f.cs.RecordRestartStopLeg(context.Background(), a.ID, "R1"), "a duplicate acknowledgment")
	f.mint(a.ID, "R2")
	f.startWrite(a.ID)
	evs := f.events(a.ID)
	require.Equal(t, "created@1[/1] started@2[R1/1] stopped@3(stopped,restart_stop_leg)[R1/1] started@4[R2/1]", lcFull(evs))
	require.NotNil(t, evs[2].SegmentMetadataRevision)
	assert.Equal(t, int64(1), *evs[2].SegmentMetadataRevision)
}

// TestLifecycleStore2_RestartStopLegRaces: an acknowledgment for a run
// that is not open (another restart, an adoption, a resting agent) gives
// nothing.
func TestLifecycleStore2_RestartStopLegRaces(t *testing.T) {
	f, a := newS2Fixture(t)
	require.NoError(t, f.cs.RecordRestartStopLeg(context.Background(), a.ID, "R1"), "resting agent, no open segment")
	f.mint(a.ID, "R1")
	f.startWrite(a.ID)
	require.NoError(t, f.cs.RecordRestartStopLeg(context.Background(), a.ID, "R0"), "a stale run")
	assert.Equal(t, "created@1[/1] started@2[R1/1]", lcFull(f.events(a.ID)))
}

// TestLifecycleStore2_QueuedRestartThenCorrelatedHeartbeat is r5 B2/N3:
// a queued restart writes only run IDs; the first heartbeat carrying R2
// closes R1 as superseded (with run_changed_at) and starts R2.
func TestLifecycleStore2_QueuedRestartThenCorrelatedHeartbeat(t *testing.T) {
	f, a := newS2Fixture(t)
	f.mint(a.ID, "R1")
	f.startWrite(a.ID)
	f.mint(a.ID, "R2")
	assert.Equal(t, "created@1[/1] started@2[R1/1]", lcFull(f.events(a.ID)), "run-ID writes emit nothing")
	f.heartbeat(a.ID, "running", "R2")
	evs := f.events(a.ID)
	require.Equal(t, "created@1[/1] started@2[R1/1] stopped@3(superseded)[R1/1] started@4[R2/1]", lcFull(evs))
	assert.NotEmpty(t, evs[2].RunChangedAt)
	require.NoError(t, tc.ValidateTS(evs[2].RunChangedAt))
}

// TestLifecycleStore2_QueuedRestartThenStop: an authoritative stop before
// any heartbeat gives superseded(R1) + stopped(start_unobserved, R2).
func TestLifecycleStore2_QueuedRestartThenStop(t *testing.T) {
	f, a := newS2Fixture(t)
	f.mint(a.ID, "R1")
	f.startWrite(a.ID)
	f.mint(a.ID, "R2")
	f.stopRecord(a.ID, "R2", "stopped")
	assert.Equal(t, "created@1[/1] started@2[R1/1] stopped@3(superseded)[R1/1] stopped@4(stopped,start_unobserved)[R2/1]",
		lcFull(f.events(a.ID)))
}

// TestLifecycleStore2_LabelPatchAfterQueuedRestart: a label write closes
// R1 and never synthesizes started(R2).
func TestLifecycleStore2_LabelPatchAfterQueuedRestart(t *testing.T) {
	f, a := newS2Fixture(t)
	ctx := context.Background()
	f.mint(a.ID, "R1")
	f.startWrite(a.ID)
	f.mint(a.ID, "R2")
	f.update(ctx, a.ID, func(a *store.Agent) { a.Labels = map[string]string{"stage": "review"} })
	evs := f.events(a.ID)
	require.Equal(t, "created@1[/1] started@2[R1/1] stopped@3(superseded)[R1/1] metadata_changed@4[R2/1]", lcFull(evs))
	assert.NotEmpty(t, evs[2].RunChangedAt)
	assert.Equal(t, int64(2), evs[3].MetadataRevision)
}

// TestLifecycleStore2_SupersededCarriesFrozenSegment: a label change
// inside R1's segment moves the current metadata revision to 2; the later
// closure of R1 carries segment_metadata_revision 1 (frozen at started)
// and metadata_revision 2 (current), and R1's frozen generation.
func TestLifecycleStore2_SupersededCarriesFrozenSegment(t *testing.T) {
	f, a := newS2Fixture(t)
	ctx := context.Background()
	f.mint(a.ID, "R1")
	f.startWrite(a.ID)
	f.update(ctx, a.ID, func(a *store.Agent) { a.Labels = map[string]string{"stage": "review"} })
	f.mint(a.ID, "R2")
	f.heartbeat(a.ID, "running", "R2")
	evs := f.events(a.ID)
	require.Equal(t, "created@1[/1] started@2[R1/1] metadata_changed@3[R1/1] stopped@4(superseded)[R1/1] started@5[R2/1]", lcFull(evs))
	require.NotNil(t, evs[3].SegmentMetadataRevision)
	assert.Equal(t, int64(1), *evs[3].SegmentMetadataRevision, "frozen at R1's started")
	assert.Equal(t, int64(2), evs[3].MetadataRevision, "metadata_revision stays current")
}

// TestLifecycleStore2_StaleReports: a stale R1 heartbeat after R2 is
// current never starts R2 (it can only trigger the supersede), and a late
// R1 stop with IfRunID is refused by the store.
func TestLifecycleStore2_StaleReports(t *testing.T) {
	f, a := newS2Fixture(t)
	f.mint(a.ID, "R1")
	f.startWrite(a.ID)
	f.mint(a.ID, "R2")
	f.heartbeat(a.ID, "running", "R1")
	err := f.cs.UpdateAgentStatus(context.Background(), a.ID, store.AgentStatusUpdate{Phase: "stopped", IfRunID: "R1"})
	require.ErrorIs(t, err, store.ErrRunChanged)
	assert.Equal(t, "created@1[/1] started@2[R1/1] stopped@3(superseded)[R1/1]", lcFull(f.events(a.ID)),
		"the stale heartbeat's timestamps-only write closes R1 (step 2) and starts nothing")
}

// TestLifecycleStore2_LegacyRunlessReport: a report with no run identity,
// stopped -> running, gives started(run_uncorrelated); a stale legacy
// report after R2 was minted gives at most that, never a correlated start.
func TestLifecycleStore2_LegacyRunlessReport(t *testing.T) {
	f, a := newS2Fixture(t)
	ctx := context.Background()
	f.mint(a.ID, "R1")
	f.status(ctx, a.ID, store.AgentStatusUpdate{Phase: "stopped"})
	f.heartbeat(a.ID, "running", "")
	evs := f.events(a.ID)
	require.Equal(t, "created@1[/1] started@2(run_uncorrelated)[R1/1]", lcFull(evs))

	g, b := newS2Fixture(t)
	g.mint(b.ID, "R1")
	g.status(ctx, b.ID, store.AgentStatusUpdate{Phase: "stopped"})
	g.mint(b.ID, "R2")
	g.heartbeat(b.ID, "running", "")
	evs = g.events(b.ID)
	require.Equal(t, "created@1[/1] started@2(run_uncorrelated)[R2/1]", lcFull(evs))
	assert.Equal(t, tc.CauseRunUncorrelated, evs[1].Cause, "no E path fired")
}

// TestLifecycleStore2_SameLaunchResume: two segments of one run, each
// closed once; stopped -> running with the same run starts it again.
func TestLifecycleStore2_SameLaunchResume(t *testing.T) {
	f, a := newS2Fixture(t)
	f.mint(a.ID, "R1")
	f.startWrite(a.ID)
	f.stopRecord(a.ID, "R1", "suspended")
	f.heartbeat(a.ID, "running", "R1")
	f.stopRecord(a.ID, "R1", "stopped")
	assert.Equal(t, "created@1[/1] started@2[R1/1] stopped@3(suspended)[R1/1] started@4[R1/1] stopped@5(stopped)[R1/1]",
		lcFull(f.events(a.ID)))
}

// TestLifecycleStore2_AdoptionProvenance is r2 N1: a start that finds the
// agent already running mints R2 and adopts the existing run R1 back: no
// superseded, no started, and the first later event carries the adoption.
func TestLifecycleStore2_AdoptionProvenance(t *testing.T) {
	f, a := newS2Fixture(t)
	ctx := context.Background()
	f.mint(a.ID, "R1")
	f.startWrite(a.ID)
	f.mint(a.ID, "R2")
	ok, err := f.cs.CompareAndSwapAgentRunID(ctx, a.ID, "R2", "R1")
	require.NoError(t, err)
	require.True(t, ok)
	f.heartbeat(a.ID, "running", "R1")
	f.update(ctx, a.ID, func(a *store.Agent) { a.Labels = map[string]string{"x": "y"} })
	evs := f.events(a.ID)
	require.Equal(t, "created@1[/1] started@2[R1/1] metadata_changed@3[R1/1]", lcFull(evs))
	assert.Equal(t, "R2", evs[2].AdoptedFromLaunchID)
	assert.Equal(t, store.AdoptionSourceCAS, evs[2].AdoptionSource)
	assert.NotEmpty(t, evs[2].AdoptedAt)
	assert.Empty(t, evs[2].RunChangedAt)
}

// TestLifecycleStore2_AdoptionRebind: an adoption of the open run rebinds
// the segment (same generation, no new segment); the next event, the
// stop, carries the provenance and the adopted launch_id.
func TestLifecycleStore2_AdoptionRebind(t *testing.T) {
	f, a := newS2Fixture(t)
	ctx := context.Background()
	f.mint(a.ID, "R1")
	f.startWrite(a.ID)
	ok, err := f.cs.AdoptAgentRunID(ctx, a.ID, "R1", "L1", time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.True(t, ok)
	f.heartbeat(a.ID, "running", "L1")
	f.stopRecord(a.ID, "L1", "stopped")
	evs := f.events(a.ID)
	require.Equal(t, "created@1[/1] started@2[R1/1] stopped@3(stopped)[L1/1]", lcFull(evs))
	assert.Equal(t, "R1", evs[2].AdoptedFromLaunchID)
	assert.Equal(t, store.AdoptionSourceHeartbeat, evs[2].AdoptionSource)
}

// TestLifecycleStore2_DeleteOfRunningAgent is r3 B1 / r3 N3: the claim
// writes running -> stopping (no event), the soft finalize emits deleted
// only, closing the segment; no resynced.
func TestLifecycleStore2_DeleteOfRunningAgent(t *testing.T) {
	f, a := newS2Fixture(t)
	ctx := context.Background()
	f.mint(a.ID, "R1")
	f.startWrite(a.ID)
	stopping := "stopping"
	_, err := f.cs.UpdateAgentDeletion(ctx, a.ID, store.DeletionPredicate{}, store.DeletionFields{Phase: &stopping})
	require.NoError(t, err)
	f.finalize(ctx, a.ID, store.DeletionFinalizeSoft)
	evs := f.events(a.ID)
	require.Equal(t, "created@1[/1] started@2[R1/1] deleted@3(soft)[R1/1]", lcFull(evs))
	require.NotNil(t, evs[2].SegmentMetadataRevision, "deleted closes the open segment")
}

// TestLifecycleStore2_DeleteRollback is r3 B1 and token-analyst-2 05:33:
// a failed delete restoring stopped gives stopped(delete_rollback); one
// restoring running gives nothing; no resynced on the next write.
func TestLifecycleStore2_DeleteRollback(t *testing.T) {
	ctx := context.Background()
	stopping, stopped, running := "stopping", "stopped", "running"
	f, a := newS2Fixture(t)
	f.mint(a.ID, "R1")
	f.startWrite(a.ID)
	_, err := f.cs.UpdateAgentDeletion(ctx, a.ID, store.DeletionPredicate{}, store.DeletionFields{Phase: &stopping})
	require.NoError(t, err)
	_, err = f.cs.UpdateAgentDeletion(ctx, a.ID, store.DeletionPredicate{}, store.DeletionFields{Phase: &stopped})
	require.NoError(t, err)
	f.update(ctx, a.ID, func(a *store.Agent) { a.Message = "after" })
	assert.Equal(t, "created@1[/1] started@2[R1/1] stopped@3(stopped,delete_rollback)[R1/1]", lcFull(f.events(a.ID)))

	g, b := newS2Fixture(t)
	g.mint(b.ID, "R1")
	g.startWrite(b.ID)
	_, err = g.cs.UpdateAgentDeletion(ctx, b.ID, store.DeletionPredicate{}, store.DeletionFields{Phase: &stopping})
	require.NoError(t, err)
	_, err = g.cs.UpdateAgentDeletion(ctx, b.ID, store.DeletionPredicate{}, store.DeletionFields{Phase: &running})
	require.NoError(t, err)
	assert.Equal(t, "created@1[/1] started@2[R1/1]", lcFull(g.events(b.ID)))
}

// TestLifecycleStore2_DeleteDuringStartRollback is r4 N1: the delete
// rollback over a start that never ran (starting -> stopped) is a
// cancellation without launch_id.
func TestLifecycleStore2_DeleteDuringStartRollback(t *testing.T) {
	f, a := newS2Fixture(t)
	ctx := context.Background()
	f.status(ctx, a.ID, store.AgentStatusUpdate{Phase: "starting"})
	f.mint(a.ID, "R1")
	stopped := "stopped"
	_, err := f.cs.UpdateAgentDeletion(ctx, a.ID, store.DeletionPredicate{}, store.DeletionFields{Phase: &stopped})
	require.NoError(t, err)
	assert.Equal(t, "created@1[/1] launch_failed@2(cancelled)[/1]", lcFull(f.events(a.ID)))
}

// TestLifecycleStore2_AmbiguousReportsCounted is r4 N1: a heartbeat or an
// agent self-report of stopped over starting gives no event, and the
// ambiguous counter.
func TestLifecycleStore2_AmbiguousReportsCounted(t *testing.T) {
	f, a := newS2Fixture(t)
	ctx := context.Background()
	f.status(ctx, a.ID, store.AgentStatusUpdate{Phase: "starting"})
	f.heartbeat(a.ID, "stopped", "")
	f.status(ctx, a.ID, store.AgentStatusUpdate{Phase: "starting"})
	f.status(ctx, a.ID, store.AgentStatusUpdate{Phase: "stopped", ReportSource: store.StatusReportSourceAgent})
	assert.Equal(t, "created@1[/1]", lcFull(f.events(a.ID)))
	assert.Equal(t, int64(2), f.counters.Get(telemetryemit.MetricAmbiguous, lifecycleevents.AmbiguousRestNoSegment))
}

// TestLifecycleStore2_StartRollbacks is r4 N3 and r3 B2: an unknown-outcome
// rollback gives no event (counted); a confirmed failed dispatch from
// stopped gives launch_failed(dispatch_failed) without launch_id, and two
// distinct failures get two events.
func TestLifecycleStore2_StartRollbacks(t *testing.T) {
	f, a := newS2Fixture(t)
	ctx := context.Background()
	f.status(ctx, a.ID, store.AgentStatusUpdate{Phase: "stopped"})
	for i := 0; i < 2; i++ {
		f.status(ctx, a.ID, store.AgentStatusUpdate{Phase: "starting"})
		f.status(ctx, a.ID, store.AgentStatusUpdate{Phase: "stopped", IfPhase: "starting", Restore: true})
	}
	f.status(ctx, a.ID, store.AgentStatusUpdate{Phase: "starting"})
	uctx := lifecycleevents.WithStartOutcome(ctx, lifecycleevents.StartOutcomeUnconfirmed)
	f.status(uctx, a.ID, store.AgentStatusUpdate{Phase: "stopped", IfPhase: "starting", Restore: true})
	f.mint(a.ID, "R1")
	f.startWrite(a.ID)
	assert.Equal(t, "created@1[/1] launch_failed@2(dispatch_failed)[/1] launch_failed@3(dispatch_failed)[/1] started@4[R1/1]",
		lcFull(f.events(a.ID)), "the later started is not preceded by a failure for the unconfirmed start")
	assert.Equal(t, int64(1), f.counters.Get(telemetryemit.MetricUnconfirmed, ""))
}

// TestLifecycleStore2_RestartFailedFromStopped: the restart_failed hint
// gives launch_failed(restart_failed) only.
func TestLifecycleStore2_RestartFailedFromStopped(t *testing.T) {
	f, a := newS2Fixture(t)
	ctx := context.Background()
	f.status(ctx, a.ID, store.AgentStatusUpdate{Phase: "stopped"})
	f.mint(a.ID, "R2")
	f.status(ctx, a.ID, store.AgentStatusUpdate{Phase: "starting"})
	rctx := lifecycleevents.WithStartOutcome(ctx, lifecycleevents.StartOutcomeRestartFailed)
	f.status(rctx, a.ID, store.AgentStatusUpdate{Phase: "stopped", IfRunID: "R2"})
	assert.Equal(t, "created@1[/1] launch_failed@2(restart_failed)[/1]", lcFull(f.events(a.ID)))
}

// TestLifecycleStore2_ErrorTransitions is r2 B1 and token-analyst-2
// 05:21: running -> error gives stopped(error); a start that errors before
// running gives launch_failed(launch_error) without launch_id (no
// correlation), and with launch_id when the write correlates it (Q10).
func TestLifecycleStore2_ErrorTransitions(t *testing.T) {
	ctx := context.Background()
	f, a := newS2Fixture(t)
	f.mint(a.ID, "R1")
	f.startWrite(a.ID)
	f.heartbeat(a.ID, "error", "R1")
	f.status(ctx, a.ID, store.AgentStatusUpdate{Phase: "starting"})
	f.mint(a.ID, "R2")
	f.status(ctx, a.ID, store.AgentStatusUpdate{Phase: "error"})
	f.status(ctx, a.ID, store.AgentStatusUpdate{Phase: "starting"})
	f.mint(a.ID, "R3")
	f.status(ctx, a.ID, store.AgentStatusUpdate{Phase: "error", ReportSource: store.StatusReportSourceAgent, ReportRunID: "R3"})
	assert.Equal(t, "created@1[/1] started@2[R1/1] stopped@3(error)[R1/1] launch_failed@4(launch_error)[/1] launch_failed@5(launch_error)[R3/1]",
		lcFull(f.events(a.ID)))
}

// TestLifecycleStore2_HaltedAfterStopped: halted after stopped is one
// segment (a reclassification), and a heartbeat over a halted row never
// halts again.
func TestLifecycleStore2_HaltedAfterStopped(t *testing.T) {
	f, a := newS2Fixture(t)
	ctx := context.Background()
	f.mint(a.ID, "R1")
	f.startWrite(a.ID)
	f.stopRecord(a.ID, "R1", "stopped")
	f.status(ctx, a.ID, store.AgentStatusUpdate{Phase: "halted", Restore: true})
	f.heartbeat(a.ID, "stopped", "R1")
	assert.Equal(t, "created@1[/1] started@2[R1/1] stopped@3(stopped)[R1/1] halted@4[R1/1]", lcFull(f.events(a.ID)))
}

// TestLifecycleStore2_ConfiguredOnlyFromResolution is slice 2 ruling Q1:
// a PATCH of configuration gives metadata_changed; the same change from a
// config-resolution write gives configured.
func TestLifecycleStore2_ConfiguredOnlyFromResolution(t *testing.T) {
	f, a := newS2Fixture(t)
	ctx := context.Background()
	f.update(ctx, a.ID, func(a *store.Agent) { a.AppliedConfig = &store.AgentAppliedConfig{Model: "m-1"} })
	rctx := lifecycleevents.WithConfigResolution(ctx)
	f.update(rctx, a.ID, func(a *store.Agent) {
		a.AppliedConfig = &store.AgentAppliedConfig{Model: "m-2"}
		a.Labels = map[string]string{"also": "labels"}
	})
	f.update(rctx, a.ID, func(a *store.Agent) { a.Labels = map[string]string{"only": "labels"} })
	evs := f.events(a.ID)
	require.Equal(t, "created@1[/1] metadata_changed@2[/1] configured@3[/1] metadata_changed@4[/1]", lcFull(evs))
	assert.Equal(t, "m-2", evs[2].Metadata.RequestedModel)
	assert.Equal(t, int64(3), evs[2].MetadataRevision, "one metadata revision for config and labels")
}

// TestLifecycleStore2_SyncDispatchResult is r6 B1 / r7 N1-N2: a dispatch
// result naming R and running gives started; a legacy answer gives
// started(run_uncorrelated); a starting answer gives nothing; a stale
// result after R2 was minted gives nothing; a duplicate gives one.
func TestLifecycleStore2_SyncDispatchResult(t *testing.T) {
	ctx := context.Background()
	dispatch := func(f *lifecycleFixture, id string, r lifecycleevents.DispatchResult, phase string) {
		f.update(lifecycleevents.WithDispatchResult(ctx, r), id, func(a *store.Agent) {
			a.Phase = phase
			a.AppliedConfig = &store.AgentAppliedConfig{Model: "resolved"}
		})
	}
	f, a := newS2Fixture(t)
	f.mint(a.ID, "R1")
	dispatch(f, a.ID, lifecycleevents.DispatchResult{RunID: "R1", BrokerEvidence: true}, "running")
	dispatch(f, a.ID, lifecycleevents.DispatchResult{RunID: "R1", BrokerEvidence: true}, "running")
	assert.Equal(t, "created@1[/1] configured@2[R1/1] started@3[R1/1]", lcFull(f.events(a.ID)),
		"sync create: created, configured, started at consecutive revisions; the duplicate adds nothing")

	g, b := newS2Fixture(t)
	g.mint(b.ID, "R1")
	dispatch(g, b.ID, lifecycleevents.DispatchResult{RunID: "R1", NoBrokerRun: true}, "running")
	assert.Equal(t, "created@1[/1] configured@2[R1/1] started@3(run_uncorrelated)[R1/1]", lcFull(g.events(b.ID)))

	h, c := newS2Fixture(t)
	h.mint(c.ID, "R1")
	dispatch(h, c.ID, lifecycleevents.NewDispatchResult("R1", "R1", false), "starting")
	h.heartbeat(c.ID, "running", "R1")
	assert.Equal(t, "created@1[/1] configured@2[R1/1] started@3[R1/1]", lcFull(h.events(c.ID)),
		"a starting answer gives no start; the correlated heartbeat does")

	// F2 (tele-wp1-arch 22:06): a legacy broker answered starting with no
	// run and the Hub forced running (env submit): no event at all; the
	// later run-less heartbeat is the legacy observation.
	m, e := newS2Fixture(t)
	m.mint(e.ID, "R1")
	m.status(ctx, e.ID, store.AgentStatusUpdate{Phase: "starting"})
	dispatch(m, e.ID, lifecycleevents.NewDispatchResult("R1", "", false), "running")
	assert.Equal(t, "created@1[/1] configured@2[R1/1]", lcFull(m.events(e.ID)), "a Hub-forced running is not a start")
	m.status(ctx, e.ID, store.AgentStatusUpdate{Phase: "stopped"})
	m.heartbeat(e.ID, "running", "")
	assert.Equal(t, "created@1[/1] configured@2[R1/1] started@3(run_uncorrelated)[R1/1]", lcFull(m.events(e.ID)))

	k, d := newS2Fixture(t)
	k.mint(d.ID, "R1")
	k.mint(d.ID, "R2")
	dispatch(k, d.ID, lifecycleevents.DispatchResult{RunID: "R1", BrokerEvidence: true}, "running")
	assert.Equal(t, "created@1[/1] metadata_changed@2[R2/1]", lcFull(k.events(d.ID)),
		"a stale result is neither a start nor a config resolution")
}

// TestLifecycleStore2_RunSwapStoreErrorNotCorrelated is 10b item 12 (r7 N4):
// the CAS to the broker's run L failed, so the row keeps the minted R
// while the broker runs L. The dispatch result names no matching run
// (brokerNamedRun false) and L's heartbeats are not E(ii) for R: the
// start is never correlated, and nothing flags it.
func TestLifecycleStore2_RunSwapStoreErrorNotCorrelated(t *testing.T) {
	f, a := newS2Fixture(t)
	ctx := context.Background()
	f.mint(a.ID, "R1")
	f.update(lifecycleevents.WithDispatchResult(ctx, lifecycleevents.DispatchResult{RunID: "R1"}), a.ID,
		func(a *store.Agent) { a.Phase = "running" })
	f.heartbeat(a.ID, "running", "L1")
	assert.Equal(t, "created@1[/1]", lcFull(f.events(a.ID)))
}

// TestLifecycleStore2_LaunchStoreFlow: BeginLaunch (fingerprint only),
// MarkLaunchAccepted (launch_accepted with owner, async_launch_id), a coded
// EndLaunch (launch_failed(not_launched)). A codeless end emits nothing.
func TestLifecycleStore2_LaunchStoreFlow(t *testing.T) {
	f, a := newS2Fixture(t)
	ctx := context.Background()
	launchID, err := f.cs.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, time.Minute)
	require.NoError(t, err)
	_, err = f.cs.MarkLaunchAccepted(ctx, a.ID, launchID, "bi-1")
	require.NoError(t, err)
	_, err = f.cs.MarkLaunchAccepted(ctx, a.ID, launchID, "bi-1")
	require.NoError(t, err)
	require.NoError(t, f.cs.EndLaunchWithCode(ctx, a.ID, launchID, store.LaunchEndReasonNotLaunched, "dispatch_refused"))
	evs := f.events(a.ID)
	require.Equal(t, "created@1[/1] launch_accepted@2[/1] launch_failed@3(not_launched,dispatch_refused)[/1]", lcFull(evs))
	assert.Equal(t, "bi-1", evs[1].LaunchOwner)
	assert.Equal(t, launchID, evs[1].AsyncLaunchID)
	assert.Equal(t, launchID, evs[2].AsyncLaunchID)

	g, b := newS2Fixture(t)
	launchID, err = g.cs.BeginLaunch(ctx, b.ID, store.LaunchKindCreate, time.Minute)
	require.NoError(t, err)
	require.NoError(t, g.cs.EndLaunch(ctx, b.ID, launchID, store.LaunchEndReasonNotLaunched))
	assert.Equal(t, "created@1[/1]", lcFull(g.events(b.ID)), "a codeless end derives nothing")
}

// TestLifecycleStore2_UnknownOutcomeAccept: the unknown-outcome branch
// accepts with no owner; launch_accepted carries no launch_owner.
func TestLifecycleStore2_UnknownOutcomeAccept(t *testing.T) {
	f, a := newS2Fixture(t)
	ctx := context.Background()
	launchID, err := f.cs.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, time.Minute)
	require.NoError(t, err)
	_, err = f.cs.MarkLaunchAccepted(ctx, a.ID, launchID, "")
	require.NoError(t, err)
	evs := f.events(a.ID)
	require.Equal(t, "created@1[/1] launch_accepted@2[/1]", lcFull(evs))
	assert.Empty(t, evs[1].LaunchOwner)
}

// TestLifecycleStore2_Reincarnation is r2 N2 / r4 B1c through the store:
// the claim records the base run; the reprovision configured is tagged
// N+1 without launch_id; the generation bump with R2 closes R1
// (superseded, generation N) before reincarnated(N+1); the new run's
// started carries N+1; no launch maps to two generations.
func TestLifecycleStore2_Reincarnation(t *testing.T) {
	f, a := newS2Fixture(t)
	ctx := context.Background()
	f.mint(a.ID, "R1")
	f.startWrite(a.ID)
	cur, err := f.cs.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	_, err = f.cs.ClaimAgentReincarnation(ctx, a.ID, cur.StateVersion, time.Now())
	require.NoError(t, err)
	f.update(ctx, a.ID, func(a *store.Agent) { a.Phase = "stopping"; a.ReincarnationState = store.ReincarnationStatePending })
	f.update(lifecycleevents.WithConfigResolution(ctx), a.ID, func(a *store.Agent) {
		a.Phase = "starting"
		a.AppliedConfig = &store.AgentAppliedConfig{Model: "next"}
	})
	f.mint(a.ID, "R2")
	f.update(ctx, a.ID, func(a *store.Agent) { a.Generation = 2 })
	f.update(ctx, a.ID, func(a *store.Agent) { a.ReincarnationState = store.ReincarnationStateNone })
	f.heartbeat(a.ID, "running", "R2")
	assert.Equal(t, "created@1[/1] started@2[R1/1] configured@3[/2] stopped@4(superseded)[R1/1] reincarnated@5[R2/2] started@6[R2/2]",
		lcFull(f.events(a.ID)))
}

// TestLifecycleStore2_ReincarnationHeartbeatRace: a heartbeat with no
// context committing running for R2 between the reprovision and the
// generation bump is tagged N+1; the old run keeps N.
func TestLifecycleStore2_ReincarnationHeartbeatRace(t *testing.T) {
	f, a := newS2Fixture(t)
	ctx := context.Background()
	f.mint(a.ID, "R1")
	f.startWrite(a.ID)
	cur, err := f.cs.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	_, err = f.cs.ClaimAgentReincarnation(ctx, a.ID, cur.StateVersion, time.Now())
	require.NoError(t, err)
	f.update(ctx, a.ID, func(a *store.Agent) { a.Phase = "starting"; a.ReincarnationState = store.ReincarnationStatePending })
	f.mint(a.ID, "R2")
	f.heartbeat(a.ID, "running", "R2")
	f.update(ctx, a.ID, func(a *store.Agent) { a.Generation = 2; a.ReincarnationState = store.ReincarnationStateNone })
	assert.Equal(t, "created@1[/1] started@2[R1/1] stopped@3(superseded)[R1/1] started@4[R2/2] reincarnated@5[R2/2]",
		lcFull(f.events(a.ID)))
}

// TestLifecycleStore2_UpdateAgentInTxObservedOnce is N11: UpdateAgent in
// WithTx (the restore path) observes exactly once and does not deadlock
// on SQLite's single connection.
func TestLifecycleStore2_UpdateAgentInTxObservedOnce(t *testing.T) {
	f, a := newS2Fixture(t)
	ctx := context.Background()
	f.finalize(ctx, a.ID, store.DeletionFinalizeSoft)
	require.NoError(t, f.cs.WithTx(ctx, func(tx store.Store) error {
		got, err := tx.GetAgent(ctx, a.ID)
		if err != nil {
			return err
		}
		got.DeletedAt = time.Time{}
		return tx.UpdateAgent(ctx, got)
	}))
	assert.Equal(t, "created@1[/1] deleted@2(soft)[/1] restored@3[/1]", lcFull(f.events(a.ID)))
}

// TestLifecycleStore2_FinalizeHookObservedWrite is section 22 item 10 N7
// (slice 2 ruling Q7): an observed agent write made by the finalize hook
// after the soft finalize's observe call finds a matching fingerprint, so
// it gives no resynced.
func TestLifecycleStore2_FinalizeHookObservedWrite(t *testing.T) {
	f, a := newS2Fixture(t)
	ctx := context.Background()
	now := time.Now()
	hook := func(ctx context.Context, tx store.Store, row *store.Agent, mode store.DeletionFinalizeMode) error {
		got, err := tx.GetAgent(ctx, row.ID)
		if err != nil {
			return err
		}
		got.Message = "finalized"
		return tx.UpdateAgent(ctx, got)
	}
	n, err := f.cs.FinalizeAgentDeletion(ctx, a.ID, store.DeletionPredicate{}, store.DeletionFinalizeSoft,
		store.DeletionFields{DeletedAt: &now}, hook)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	assert.Equal(t, "created@1[/1] deleted@2(soft)[/1]", lcFull(f.events(a.ID)))
	assert.Zero(t, f.counters.Get(telemetryemit.MetricResyncs, tc.CauseUnobservedChange))
}

// TestLifecycleStore2_ObserverErrorRollsBackSliceTwoWriters: every slice 2
// observed writer rolls its product write back with the observer's error,
// and no outbox row or cursor moves.
func TestLifecycleStore2_ObserverErrorRollsBackSliceTwoWriters(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		write func(f *lifecycleFixture, a *store.Agent) error
		check func(t *testing.T, before, after *store.Agent)
	}{
		{"UpdateAgent", func(f *lifecycleFixture, a *store.Agent) error {
			got, _ := f.cs.GetAgent(ctx, a.ID)
			got.Labels = map[string]string{"z": "z"}
			return f.cs.UpdateAgent(ctx, got)
		}, func(t *testing.T, b, a *store.Agent) { assert.Equal(t, b.Labels, a.Labels) }},
		{"UpdateAgent running", func(f *lifecycleFixture, a *store.Agent) error {
			got, _ := f.cs.GetAgent(ctx, a.ID)
			got.Phase = "running"
			return f.cs.UpdateAgent(ctx, got)
		}, func(t *testing.T, b, a *store.Agent) { assert.Equal(t, b.Phase, a.Phase) }},
		{"UpdateAgentStatus", func(f *lifecycleFixture, a *store.Agent) error {
			return f.cs.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{Phase: "running", StartWrite: true})
		}, func(t *testing.T, b, a *store.Agent) { assert.Equal(t, b.Phase, a.Phase) }},
		{"SetAgentRunID", func(f *lifecycleFixture, a *store.Agent) error {
			_, err := f.cs.SetAgentRunID(ctx, a.ID, "RX", nil)
			return err
		}, func(t *testing.T, b, a *store.Agent) { assert.Equal(t, b.RunID, a.RunID) }},
		{"CompareAndSwapAgentRunID", func(f *lifecycleFixture, a *store.Agent) error {
			_, err := f.cs.CompareAndSwapAgentRunID(ctx, a.ID, "R1", "L1")
			return err
		}, func(t *testing.T, b, a *store.Agent) { assert.Equal(t, b.RunID, a.RunID) }},
		{"ClaimAgentReincarnation", func(f *lifecycleFixture, a *store.Agent) error {
			got, _ := f.cs.GetAgent(ctx, a.ID)
			_, err := f.cs.ClaimAgentReincarnation(ctx, a.ID, got.StateVersion, time.Now())
			return err
		}, func(t *testing.T, b, a *store.Agent) { assert.Equal(t, b.ReincarnationState, a.ReincarnationState) }},
		{"UpdateAgentDeletion", func(f *lifecycleFixture, a *store.Agent) error {
			stopping := "stopping"
			_, err := f.cs.UpdateAgentDeletion(ctx, a.ID, store.DeletionPredicate{}, store.DeletionFields{Phase: &stopping})
			return err
		}, func(t *testing.T, b, a *store.Agent) { assert.Equal(t, b.Phase, a.Phase) }},
		{"BeginLaunch", func(f *lifecycleFixture, a *store.Agent) error {
			_, err := f.cs.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, time.Minute)
			return err
		}, func(t *testing.T, b, a *store.Agent) { assert.Equal(t, b.LaunchID, a.LaunchID) }},
		{"RecordRestartStopLeg", func(f *lifecycleFixture, a *store.Agent) error {
			return f.cs.RecordRestartStopLeg(ctx, a.ID, "R1")
		}, func(t *testing.T, b, a *store.Agent) {}},
		{"SetAgentEffectiveConfig", func(f *lifecycleFixture, a *store.Agent) error {
			// Give the row a broker with an unobserved direct write, so the
			// echo passes every fence and reaches the observer.
			uid, err := parseUUID(a.ID)
			require.NoError(f.t, err)
			require.NoError(f.t, f.cs.client.Agent.UpdateOneID(uid).SetRuntimeBrokerID("broker-1").Exec(ctx))
			_, err = f.cs.SetAgentEffectiveConfig(ctx, a.ID, "R1", effectiveEcho("R1", "broker-1"))
			return err
		}, func(t *testing.T, b, a *store.Agent) { assert.Equal(t, b.EffectiveConfig, a.EffectiveConfig) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, a := newS2Fixture(t)
			f.mint(a.ID, "R1")
			before, err := f.cs.GetAgent(ctx, a.ID)
			require.NoError(t, err)
			snap := lifecycleSnapshot(t, f.cs)
			f.cs.SetAgentWriteObserver(failingObserver{err: errInjectedLC})
			require.ErrorIs(t, c.write(f, a), errInjectedLC)
			after, err := f.cs.GetAgent(ctx, a.ID)
			require.NoError(t, err)
			c.check(t, before, after)
			assert.Equal(t, before.StateVersion, after.StateVersion, "the product write rolled back")
			assert.Equal(t, snap, lifecycleSnapshot(t, f.cs), "no outbox row or cursor may move")
		})
	}
}

// TestLifecycleStore2_LaunchReports: a succeeded launch report is E(iii)
// and a config resolution (configured, started with async_launch_id); a
// failed report ending the launch gives launch_failed(failed, code, cause)
// with launch_id and async_launch_id.
func TestLifecycleStore2_LaunchReports(t *testing.T) {
	ctx := context.Background()
	f := newLifecycleFixture(t)
	a := f.create("s2-report", func(a *store.Agent) {
		a.Phase = "created"
		a.Generation = 1
		a.AppliedConfig = &store.AgentAppliedConfig{HarnessConfig: "pre-dispatch"}
	})
	f.mint(a.ID, "R1")
	launchID, err := f.cs.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, time.Hour)
	require.NoError(t, err)
	_, _, err = f.cs.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateSucceeded,
		AgentConfig: store.BrokerAgentConfig{HarnessConfig: "claude", Profile: "p1"},
	})
	require.NoError(t, err)
	evs := f.events(a.ID)
	require.Equal(t, "created@1[/1] configured@2[R1/1] started@3[R1/1]", lcFull(evs))
	assert.Equal(t, launchID, evs[2].AsyncLaunchID)

	g, b := newS2Fixture(t)
	g.mint(b.ID, "R1")
	launchID, err = g.cs.BeginLaunch(ctx, b.ID, store.LaunchKindCreate, time.Hour)
	require.NoError(t, err)
	_, _, err = g.cs.ApplyLaunchReport(ctx, b.ID, b.RuntimeBrokerID, store.LaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateFailed,
		ErrorCode: "image_pull", Cause: "registry_unreachable", Message: "x",
	})
	require.NoError(t, err)
	evs = g.events(b.ID)
	require.Equal(t, "launch_failed@2(registry_unreachable,failed,image_pull)[R1/1]", lcFull(evs[1:]))
	assert.Equal(t, launchID, evs[1].AsyncLaunchID)
}

// TestLifecycleStore2_ReaperDeadline: a deadline reap gives
// launch_failed(timed_out, launch_timeout) with launch_id and
// async_launch_id (slice 2 ruling Q4).
func TestLifecycleStore2_ReaperDeadline(t *testing.T) {
	ctx := context.Background()
	f, a := newS2Fixture(t)
	f.mint(a.ID, "R1")
	launchID, err := f.cs.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, time.Hour)
	require.NoError(t, err)
	setAgentLaunchDeadline(t, ctx, f.cs.AgentStore, a.ID, readStoreNow(t, ctx, f.cs.AgentStore).Add(-time.Second))
	result, err := f.cs.RunLaunchReaperTick(ctx, testReaperParams)
	require.NoError(t, err)
	require.Len(t, result.Reaped, 1)
	evs := f.events(a.ID)
	require.Equal(t, "created@1[/1] launch_failed@2(timed_out,launch_timeout)[R1/1]", lcFull(evs))
	assert.Equal(t, launchID, evs[1].AsyncLaunchID)
}

// TestLifecycleStore2_ReaperWindDownCancelled: a wind-down reap of an
// attempt the stop cancelled gives launch_failed(cancelled,
// launch_stopped), no launch_id, with async_launch_id.
func TestLifecycleStore2_ReaperWindDownCancelled(t *testing.T) {
	ctx := context.Background()
	f, a := newS2Fixture(t)
	f.mint(a.ID, "R1")
	launchID, err := f.cs.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, time.Hour)
	require.NoError(t, err)
	f.stopRecord(a.ID, "R1", "stopped") // created -> stopped: cancelled (P8)
	_, err = f.cs.RunLaunchReaperTick(ctx, wideWindowReaperParams)
	require.NoError(t, err)
	setLaunchReaperArmedSince(t, ctx, f.cs.AgentStore, readStoreNow(t, ctx, f.cs.AgentStore).Add(-time.Hour))
	setAgentLastReportAt(t, ctx, f.cs.AgentStore, a.ID, readStoreNow(t, ctx, f.cs.AgentStore).Add(-time.Hour))
	result, err := f.cs.RunLaunchReaperTick(ctx, wideWindowReaperParams)
	require.NoError(t, err)
	require.Len(t, result.Reaped, 1)
	evs := f.events(a.ID)
	require.Equal(t, "created@1[/1] launch_failed@2(cancelled)[/1] launch_failed@3(cancelled,launch_stopped)[/1]", lcFull(evs))
	assert.Equal(t, launchID, evs[2].AsyncLaunchID)
}

// TestLifecycleStore2_ReaperRolledBackSavepoint: a reap whose observer
// fails rolls its savepoint back: no outbox row, no cursor movement, the
// launch stays active, and the tick itself completes.
func TestLifecycleStore2_ReaperRolledBackSavepoint(t *testing.T) {
	ctx := context.Background()
	f, a := newS2Fixture(t)
	f.mint(a.ID, "R1")
	_, err := f.cs.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, time.Hour)
	require.NoError(t, err)
	setAgentLaunchDeadline(t, ctx, f.cs.AgentStore, a.ID, readStoreNow(t, ctx, f.cs.AgentStore).Add(-time.Second))
	snap := lifecycleSnapshot(t, f.cs)
	f.cs.SetAgentWriteObserver(failingObserver{err: errInjectedLC})
	result, err := f.cs.RunLaunchReaperTick(ctx, testReaperParams)
	require.NoError(t, err)
	assert.Equal(t, store.ReaperTickCompleted, result.Outcome)
	assert.Empty(t, result.Reaped)
	assert.Equal(t, snap, lifecycleSnapshot(t, f.cs))
	got, err := f.cs.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, store.LaunchStateActive, got.LaunchState)
}

// TestLifecycleStore2_ContainerMissing: on a runtime that cannot halt the
// container-missing write moves running to error, which closes the open
// segment as stopped(error).
func TestLifecycleStore2_ContainerMissing(t *testing.T) {
	ctx := context.Background()
	f := newLifecycleFixture(t)
	a := f.create("s2-missing", func(a *store.Agent) {
		a.Phase = "created"
		a.Generation = 1
		a.RuntimeBrokerID = "broker-1"
	})
	f.mint(a.ID, "R1")
	f.startWrite(a.ID)
	got, err := f.cs.MarkAgentContainerMissing(ctx, a.ID, "broker-1", time.Now().Add(time.Minute), "", "gone")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "created@1[/1] started@2[R1/1] stopped@3(error)[R1/1]", lcFull(f.events(a.ID)))
}

// TestLifecycleStore2_CrossReplicaHeartbeatDuringRestart is r5 N5 and 10b
// item 2: R1 is closed at the stop leg, another replica's running heartbeat
// for R2 lands between beginRun(R2) and the start write, and the failed
// start leg then emits only launch_failed(restart_failed); R2 is never
// closed as restart_stop_leg.
func TestLifecycleStore2_CrossReplicaHeartbeatDuringRestart(t *testing.T) {
	f, a := newS2Fixture(t)
	ctx := context.Background()
	f.mint(a.ID, "R1")
	f.startWrite(a.ID)
	require.NoError(t, f.cs.RecordRestartStopLeg(ctx, a.ID, "R1"))
	f.mint(a.ID, "R2")
	f.heartbeat(a.ID, "running", "R2")
	rctx := lifecycleevents.WithStartOutcome(ctx, lifecycleevents.StartOutcomeRestartFailed)
	f.status(rctx, a.ID, store.AgentStatusUpdate{Phase: "stopped", IfRunID: "R2"})
	assert.Equal(t, "created@1[/1] started@2[R1/1] stopped@3(stopped,restart_stop_leg)[R1/1] started@4[R2/1] launch_failed@5(restart_failed)[/1]",
		lcFull(f.events(a.ID)))
}

// TestLifecycleStore2_AdoptionWhenOpenRunIsOlder is 10b item 5: the open
// run is R1, a start mints R2 and the broker reports L: no rebind (the
// open run is not R2); the first write for L supersedes R1, starts L, and
// carries the R2 -> L adoption.
func TestLifecycleStore2_AdoptionWhenOpenRunIsOlder(t *testing.T) {
	f, a := newS2Fixture(t)
	ctx := context.Background()
	f.mint(a.ID, "R1")
	f.startWrite(a.ID)
	f.mint(a.ID, "R2")
	ok, err := f.cs.CompareAndSwapAgentRunID(ctx, a.ID, "R2", "L1")
	require.NoError(t, err)
	require.True(t, ok)
	f.heartbeat(a.ID, "running", "L1")
	evs := f.events(a.ID)
	require.Equal(t, "created@1[/1] started@2[R1/1] stopped@3(superseded)[R1/1] started@4[L1/1]", lcFull(evs))
	assert.Empty(t, evs[2].AdoptedFromLaunchID, "a closure of the older segment carries no adoption")
	assert.Equal(t, "R2", evs[3].AdoptedFromLaunchID)
	assert.NotEmpty(t, evs[2].RunChangedAt, "the mint of R2 was the run change")
}

// TestLifecycleStore2_ReincarnationFailsAfterMint is 10b item 7: a
// reincarnation that minted R2 and then fails (writeFailedAgent: phase
// error, state failed, configuration restored) closes R1 as superseded,
// records the failed attempt without a launch_id, and the restored
// configuration is metadata_changed (not a resolution); no reincarnated,
// and R1 keeps generation 1.
func TestLifecycleStore2_ReincarnationFailsAfterMint(t *testing.T) {
	f, a := newS2Fixture(t)
	ctx := context.Background()
	f.mint(a.ID, "R1")
	f.startWrite(a.ID)
	cur, err := f.cs.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	_, err = f.cs.ClaimAgentReincarnation(ctx, a.ID, cur.StateVersion, time.Now())
	require.NoError(t, err)
	f.update(lifecycleevents.WithConfigResolution(ctx), a.ID, func(a *store.Agent) {
		a.Phase = "starting"
		a.ReincarnationState = store.ReincarnationStatePending
		a.AppliedConfig = &store.AgentAppliedConfig{Model: "next"}
	})
	f.mint(a.ID, "R2")
	f.update(ctx, a.ID, func(a *store.Agent) {
		a.Phase = "error"
		a.ReincarnationState = store.ReincarnationStateFailed
		a.AppliedConfig = nil
	})
	assert.Equal(t, "created@1[/1] started@2[R1/1] configured@3[/2] stopped@4(superseded)[R1/1] metadata_changed@5[R2/1] launch_failed@6(launch_error)[/1]",
		lcFull(f.events(a.ID)))
}

// TestLifecycleStore2_RestartWithoutStopLeg is 10b item 9: a restart with
// no broker stop leg records no RecordRestartStopLeg, so R1 is closed by
// step 2 as superseded when the start write for R2 lands.
func TestLifecycleStore2_RestartWithoutStopLeg(t *testing.T) {
	f, a := newS2Fixture(t)
	f.mint(a.ID, "R1")
	f.startWrite(a.ID)
	f.mint(a.ID, "R2")
	f.startWrite(a.ID)
	assert.Equal(t, "created@1[/1] started@2[R1/1] stopped@3(superseded)[R1/1] started@4[R2/1]", lcFull(f.events(a.ID)))
}

// TestLifecycleStore2_StartedBeforeConfigured is design N5: a correlated
// heartbeat commits started before the config echo commits configured;
// both are accepted and ordered by revision.
func TestLifecycleStore2_StartedBeforeConfigured(t *testing.T) {
	f, a := newS2Fixture(t)
	ctx := context.Background()
	f.mint(a.ID, "R1")
	f.heartbeat(a.ID, "running", "R1")
	f.update(lifecycleevents.WithConfigResolution(ctx), a.ID, func(a *store.Agent) {
		a.AppliedConfig = &store.AgentAppliedConfig{Model: "resolved"}
	})
	assert.Equal(t, "created@1[/1] started@2[R1/1] configured@3[R1/1]", lcFull(f.events(a.ID)))
}

// TestLifecycleStore2_DyingContainerReportBeforeR2: the old container's
// own stopped report for R1 lands first, so R1 is closed by stopped and
// the start of R2 needs no supersede: no double terminal.
func TestLifecycleStore2_DyingContainerReportBeforeR2(t *testing.T) {
	f, a := newS2Fixture(t)
	ctx := context.Background()
	f.mint(a.ID, "R1")
	f.startWrite(a.ID)
	f.status(ctx, a.ID, store.AgentStatusUpdate{Phase: "stopped", ReportSource: store.StatusReportSourceAgent, ReportRunID: "R1"})
	f.mint(a.ID, "R2")
	f.startWrite(a.ID)
	assert.Equal(t, "created@1[/1] started@2[R1/1] stopped@3(stopped)[R1/1] started@4[R2/1]", lcFull(f.events(a.ID)))
}

// TestLifecycleStore2_RestartFailedAfterRunChange: with no stop-leg
// closure (no broker stop leg), a failed start leg from running gives
// superseded(R1) + launch_failed(restart_failed), never a bare stopped.
func TestLifecycleStore2_RestartFailedAfterRunChange(t *testing.T) {
	f, a := newS2Fixture(t)
	ctx := context.Background()
	f.mint(a.ID, "R1")
	f.startWrite(a.ID)
	f.mint(a.ID, "R2")
	rctx := lifecycleevents.WithStartOutcome(ctx, lifecycleevents.StartOutcomeRestartFailed)
	f.status(rctx, a.ID, store.AgentStatusUpdate{Phase: "stopped", IfRunID: "R2"})
	assert.Equal(t, "created@1[/1] started@2[R1/1] stopped@3(superseded)[R1/1] launch_failed@4(restart_failed)[/1]",
		lcFull(f.events(a.ID)))
}

// TestLifecycleStore2_StopConfirmedStartUnconfirmed is the token-analyst-2
// 05:49 matrix case: the confirmed stop leg closes R1; the unconfirmed
// start rollback records nothing and is counted, so only the new attempt
// is unresolved.
func TestLifecycleStore2_StopConfirmedStartUnconfirmed(t *testing.T) {
	f, a := newS2Fixture(t)
	ctx := context.Background()
	f.mint(a.ID, "R1")
	f.startWrite(a.ID)
	require.NoError(t, f.cs.RecordRestartStopLeg(ctx, a.ID, "R1"))
	f.mint(a.ID, "R2")
	f.status(ctx, a.ID, store.AgentStatusUpdate{Phase: "starting"})
	uctx := lifecycleevents.WithStartOutcome(ctx, lifecycleevents.StartOutcomeUnconfirmed)
	f.status(uctx, a.ID, store.AgentStatusUpdate{Phase: "stopped", IfPhase: "starting", Restore: true})
	assert.Equal(t, "created@1[/1] started@2[R1/1] stopped@3(stopped,restart_stop_leg)[R1/1]", lcFull(f.events(a.ID)))
	assert.Equal(t, int64(1), f.counters.Get(telemetryemit.MetricUnconfirmed, ""))
}

// TestLifecycleStore2_AdoptionDoesNotLeakToALaterRun is wp1s2 review r1
// R1: after a rebind R1 -> L1, a queued restart closes L1 as superseded
// (carrying the R1 -> L1 provenance: its launch_id is L1) and started(R2)
// carries none; the HTTP-restart variant closes L1 at the stop leg.
func TestLifecycleStore2_AdoptionDoesNotLeakToALaterRun(t *testing.T) {
	ctx := context.Background()
	adopt := func(f *lifecycleFixture, id, from, to string) {
		ok, err := f.cs.AdoptAgentRunID(ctx, id, from, to, time.Now().Add(time.Hour))
		require.NoError(t, err)
		require.True(t, ok)
	}
	f, a := newS2Fixture(t)
	f.mint(a.ID, "R1")
	f.startWrite(a.ID)
	adopt(f, a.ID, "R1", "L1")
	f.heartbeat(a.ID, "running", "L1")
	f.mint(a.ID, "R2")
	f.heartbeat(a.ID, "running", "R2")
	evs := f.events(a.ID)
	require.Equal(t, "created@1[/1] started@2[R1/1] stopped@3(superseded)[L1/1] started@4[R2/1]", lcFull(evs))
	assert.Equal(t, "R1", evs[2].AdoptedFromLaunchID, "the closure of L1 carries the alias")
	assert.Empty(t, evs[3].AdoptedFromLaunchID, "started(R2) carries no adoption")

	g, b := newS2Fixture(t)
	g.mint(b.ID, "R1")
	g.startWrite(b.ID)
	adopt(g, b.ID, "R1", "L1")
	require.NoError(t, g.cs.RecordRestartStopLeg(ctx, b.ID, "L1"))
	g.mint(b.ID, "R2")
	g.startWrite(b.ID)
	evs = g.events(b.ID)
	require.Equal(t, "created@1[/1] started@2[R1/1] stopped@3(stopped,restart_stop_leg)[L1/1] started@4[R2/1]", lcFull(evs))
	assert.Equal(t, "R1", evs[2].AdoptedFromLaunchID)
	assert.Empty(t, evs[3].AdoptedFromLaunchID)
}

// TestLifecycleStore2_AdoptBackThenQueuedRestart is the start-of-running
// variant of review r1 R1: R2 is minted and the broker reports R1 back
// (pending R2 -> R1); a queued restart then closes R1 with that alias,
// and started(R3) carries none.
func TestLifecycleStore2_AdoptBackThenQueuedRestart(t *testing.T) {
	ctx := context.Background()
	f, a := newS2Fixture(t)
	f.mint(a.ID, "R1")
	f.startWrite(a.ID)
	f.mint(a.ID, "R2")
	ok, err := f.cs.CompareAndSwapAgentRunID(ctx, a.ID, "R2", "R1")
	require.NoError(t, err)
	require.True(t, ok)
	f.mint(a.ID, "R3")
	f.heartbeat(a.ID, "running", "R3")
	evs := f.events(a.ID)
	require.Equal(t, "created@1[/1] started@2[R1/1] stopped@3(superseded)[R1/1] started@4[R3/1]", lcFull(evs))
	assert.Equal(t, "R2", evs[2].AdoptedFromLaunchID)
	assert.Empty(t, evs[3].AdoptedFromLaunchID)
}

// TestLifecycleStore2_StopDuringReincarnation is wp1s2 review r1 R2: a stop
// record, or a heartbeat error, of the base run R1 while the reincarnation
// is in flight closes R1 with launch_id R1 and generation 1, never N+1.
func TestLifecycleStore2_StopDuringReincarnation(t *testing.T) {
	ctx := context.Background()
	claim := func(f *lifecycleFixture, id string) {
		cur, err := f.cs.GetAgent(ctx, id)
		require.NoError(t, err)
		_, err = f.cs.ClaimAgentReincarnation(ctx, id, cur.StateVersion, time.Now())
		require.NoError(t, err)
	}
	f, a := newS2Fixture(t)
	f.mint(a.ID, "R1")
	f.startWrite(a.ID)
	claim(f, a.ID)
	f.stopRecord(a.ID, "R1", "stopped")
	assert.Equal(t, "created@1[/1] started@2[R1/1] stopped@3(stopped)[R1/1]", lcFull(f.events(a.ID)))

	g := newLifecycleFixture(t)
	b := g.create("s2-reinc-missing", func(a *store.Agent) {
		a.Phase, a.Generation, a.RuntimeBrokerID = "created", 1, "broker-1"
	})
	g.mint(b.ID, "R1")
	g.startWrite(b.ID)
	claim(g, b.ID)
	// The container-missing CAS skips rows whose reincarnation is in
	// flight, so the in-flight terminal here is a broker heartbeat error
	// for R1 (this direct store call sets no GuardReincarnation).
	g.heartbeat(b.ID, "error", "R1")
	assert.Equal(t, "created@1[/1] started@2[R1/1] stopped@3(error)[R1/1]", lcFull(g.events(b.ID)))
}

// TestLifecycleStore2_OwnTransactionMarksTransient is design item 16
// (tele-wp1-arch 21:57, slice 1 CI-fix review r2 O1): when an observed
// writer opens its own lifecycle transaction, a serialization failure in
// its body is store.ErrTransient, as CompositeStore.WithTx marks its
// body's errors; a non-conflict error is not marked.
func TestLifecycleStore2_OwnTransactionMarksTransient(t *testing.T) {
	ctx := context.Background()
	conflict := fmt.Errorf("injected: %w", &pgconn.PgError{Code: "40001"})
	plain := errors.New("injected: not a conflict")
	writers := map[string]func(f *lifecycleFixture, a *store.Agent) error{
		"CreateAgent": func(f *lifecycleFixture, a *store.Agent) error {
			return f.cs.CreateAgent(ctx, makeAgent(f.projectID, "s2-transient-"+a.ID[:8]))
		},
		"DeleteAgent": func(f *lifecycleFixture, a *store.Agent) error { return f.cs.DeleteAgent(ctx, a.ID) },
		"UpdateAgent": func(f *lifecycleFixture, a *store.Agent) error {
			got, err := f.cs.GetAgent(ctx, a.ID)
			if err != nil {
				return err
			}
			got.Message = "x"
			return f.cs.UpdateAgent(ctx, got)
		},
		"SetAgentRunID": func(f *lifecycleFixture, a *store.Agent) error {
			_, err := f.cs.SetAgentRunID(ctx, a.ID, "RT", nil)
			return err
		},
		"RecordRestartStopLeg": func(f *lifecycleFixture, a *store.Agent) error {
			return f.cs.RecordRestartStopLeg(ctx, a.ID, "R1")
		},
	}
	for name, write := range writers {
		t.Run(name, func(t *testing.T) {
			f, a := newS2Fixture(t)
			f.cs.SetAgentWriteObserver(failingObserver{err: conflict})
			err := write(f, a)
			require.ErrorIs(t, err, conflict)
			assert.ErrorIs(t, err, store.ErrTransient, "a body serialization failure stays retryable")

			f.cs.SetAgentWriteObserver(failingObserver{err: plain})
			err = write(f, a)
			require.ErrorIs(t, err, plain)
			assert.NotErrorIs(t, err, store.ErrTransient)
		})
	}
}
