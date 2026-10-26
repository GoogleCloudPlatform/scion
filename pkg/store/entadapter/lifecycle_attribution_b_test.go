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

// Persistent run attribution fixtures, part 2 (attribution design v2.8,
// section 10.1): held transitions, generations, legacy, stale and missing
// cursors, the R4 closure continuity and the P3 discard.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/lifecycleevents"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	tc "github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetryemit"
)

// noBumpObserver simulates the observer's build-failure path
// (observer.go: buildFailed then return nil): the agent write commits,
// with no cursor bump and no outbox row.
type noBumpObserver struct{}

func (noBumpObserver) ObserveAgentWrite(context.Context, store.LifecycleTx, store.AgentWrite) error {
	return nil
}

// withoutObservation runs fn with the observer replaced by noBumpObserver.
func (f *lifecycleFixture) withoutObservation(fn func()) {
	f.t.Helper()
	f.cs.SetAgentWriteObserver(noBumpObserver{})
	defer f.cs.SetAgentWriteObserver(f.obs)
	fn()
}

// editCursor applies a raw cursor edit (a fixture seed), then keeps the
// stamp coherent unless stale is set.
func editCursor(t *testing.T, f *lifecycleFixture, id string, stale bool, edit func(u *ent.AgentLifecycleCursorUpdateOne)) {
	t.Helper()
	u := f.cs.client.AgentLifecycleCursor.UpdateOneID(uuid.MustParse(id))
	edit(u)
	_, err := u.Save(context.Background())
	require.NoError(t, err)
	if !stale {
		coherentStamp(t, f, id)
	}
}

// makeLegacy turns a's cursor into a LEGACY one (written before this
// design): attribution columns at their defaults, NULL stamp.
func makeLegacy(t *testing.T, f *lifecycleFixture, id string) {
	t.Helper()
	editCursor(t, f, id, true, func(u *ent.AgentLifecycleCursorUpdateOne) {
		u.SetAttributionState("").ClearAttributionStamp().SetLastRun("").SetLastGeneration("").
			SetAttributedRun("").SetAttributedGeneration("").SetQuarantinedRuns("").SetQuarantineMode("").
			SetReincarnationMintRun("").SetUntrustedOpenRun("").SetUntrustedOpenRunGeneration("").SetUntrustedOpenRunRev(0)
	})
}

// heldAfterSoftFinish is T1 to the restore: held B1, attributed R1/1.
func heldAfterSoftFinish(t *testing.T) (*lifecycleFixture, *store.Agent) {
	t.Helper()
	f, a := newS2Fixture(t)
	f.mint(a.ID, "R1")
	f.startWrite(a.ID)
	f.stopRecord(a.ID, "R1", "stopped")
	softFinish(t, f, a.ID, "B1", "R1")
	f.restore(a.ID)
	return f, a
}

// TestLifecycleAttribution_T7HeldToYSameWrite: held B1, then an observed
// revert to Y: POST held/UNATTRIBUTED with last Y, so the write's events
// and later agent-state events omit; Y is never named.
func TestLifecycleAttribution_T7HeldToYSameWrite(t *testing.T) {
	f, a := heldAfterSoftFinish(t)
	f.revert(a.ID, "B1", "Y1")
	att := attrCursor(t, f, a.ID)
	assert.Equal(t, "Y1", att.LastRun)
	assert.Equal(t, "", att.AttributedRun)
	f.metaEdit(a.ID, "m-y")
	evs := f.events(a.ID)
	assert.Equal(t, "metadata_changed@6[/1]", lcFull(evs[len(evs)-1:]))
	checkLaunchMap(t, evs, "B1", "Y1")
}

// TestLifecycleAttribution_T8UnknownSegment: held; a heartbeat opens an
// untrusted segment on X1; its closure omits launch_id and keeps the
// generation and segment_metadata_revision, whatever closes it.
func TestLifecycleAttribution_T8UnknownSegment(t *testing.T) {
	open := func(t *testing.T) (*lifecycleFixture, *store.Agent) {
		f, a := newS2Fixture(t)
		f.mint(a.ID, "R1")
		f.startWrite(a.ID)
		f.stopRecord(a.ID, "R1", "stopped")
		softFinish(t, f, a.ID, "X1")
		f.restore(a.ID)
		f.heartbeat(a.ID, "running", "X1")
		return f, a
	}
	t.Run("soft delete", func(t *testing.T) {
		f, a := open(t)
		f.finalize(context.Background(), a.ID, store.DeletionFinalizeSoft)
		evs := f.events(a.ID)
		del := evs[len(evs)-1]
		assert.Equal(t, "deleted@7(soft)[/1]", lcFull(evs[len(evs)-1:]))
		require.NotNil(t, del.SegmentMetadataRevision)
		checkLaunchMap(t, evs, "X1")
	})
	t.Run("mint then start", func(t *testing.T) {
		f, a := open(t)
		f.mint(a.ID, "R2")
		f.startWrite(a.ID)
		evs := f.events(a.ID)
		assert.Equal(t, "stopped@7(superseded)[/1] started@8[R2/1]", lcFull(evs[len(evs)-2:]))
		require.NotNil(t, evs[len(evs)-2].SegmentMetadataRevision)
		checkLaunchMap(t, evs, "X1")
	})
	t.Run("hard delete", func(t *testing.T) {
		f, a := open(t)
		f.hardDelete(a.ID)
		evs := f.events(a.ID)
		assert.Equal(t, "deleted@7(hard)[/1]", lcFull(evs[len(evs)-1:]))
		require.NotNil(t, evs[len(evs)-1].SegmentMetadataRevision)
		checkLaunchMap(t, evs, "X1")
	})
	t.Run("project delete", func(t *testing.T) {
		f, a := open(t)
		require.NoError(t, f.cs.DeleteProject(context.Background(), f.projectID))
		evs := f.events(a.ID)
		last := evs[len(evs)-1]
		assert.Equal(t, tc.TransitionDeleted, last.Transition)
		assert.Empty(t, last.LaunchID)
		require.NotNil(t, last.SegmentMetadataRevision)
		checkLaunchMap(t, evs, "X1")
	})
}

// TestLifecycleAttribution_T9UnobservedGenerationChange: a clear cursor; the
// generation changes with the observer bypassed; the next observed write
// normalises to held/UNATTRIBUTED and omits until a mint.
func TestLifecycleAttribution_T9UnobservedGenerationChange(t *testing.T) {
	f, a := newS2Fixture(t)
	f.mint(a.ID, "R1")
	f.startWrite(a.ID)
	f.stopRecord(a.ID, "R1", "stopped")
	f.withoutObservation(func() { f.bumpGeneration(a.ID) })
	f.metaEdit(a.ID, "m-9")
	evs := f.events(a.ID)
	assert.Equal(t, "resynced@5(unobserved_change)[/2] metadata_changed@6[/2]", lcFull(evs[len(evs)-2:]))
	assert.Equal(t, store.AttributionHeld, attrCursor(t, f, a.ID).State)
	checkLaunchMap(t, evs)
}

// TestLifecycleAttribution_T9oObservedSameRunGeneration: an observed bump of
// the generation with the same run and a metadata edit in the SAME write,
// not a witnessed reincarnation: POST held/UNATTRIBUTED, the same-write
// metadata_changed and reincarnated omit.
func TestLifecycleAttribution_T9oObservedSameRunGeneration(t *testing.T) {
	f, a := newS2Fixture(t)
	f.mint(a.ID, "R1")
	f.startWrite(a.ID)
	f.stopRecord(a.ID, "R1", "stopped")
	f.update(context.Background(), a.ID, func(a *store.Agent) {
		a.Generation++
		a.AppliedConfig = &store.AgentAppliedConfig{Model: "m-9o"}
	})
	evs := f.events(a.ID)
	// reincarnated is the write's RowAfter snapshot (it carries the edited
	// metadata, so no separate metadata_changed): it omits launch_id.
	assert.Equal(t, "stopped@3(stopped)[R1/1] reincarnated@4[/2]", lcFull(evs[len(evs)-2:]))
	require.NotNil(t, evs[len(evs)-1].Metadata)
	assert.Equal(t, "m-9o", evs[len(evs)-1].Metadata.RequestedModel)
	assert.Equal(t, store.AttributionHeld, attrCursor(t, f, a.ID).State)
	checkLaunchMap(t, evs)
}

// TestLifecycleAttribution_T9oSegLateQuarantine (r3 P1): R1 runs with a
// trusted segment; an unwitnessed same-run bump pushes R1 and the
// push-time marker fires; the later closure omits launch_id and keeps the
// segment generation 1.
func TestLifecycleAttribution_T9oSegLateQuarantine(t *testing.T) {
	f, a := newS2Fixture(t)
	f.mint(a.ID, "R1")
	f.startWrite(a.ID)
	f.bumpGeneration(a.ID)
	assert.Equal(t, "R1", attrCursor(t, f, a.ID).UntrustedOpenRun, "push-time marker")
	f.stopRecord(a.ID, "R1", "stopped")
	evs := f.events(a.ID)
	assert.Equal(t, "created@1[/1] started@2[R1/1] reincarnated@3[/2] stopped@4(stopped)[/1]", lcFull(evs))
	checkLaunchMap(t, evs)
}

// TestLifecycleAttribution_T9oSame: the bump and the stop in ONE write:
// closure selection uses the post-trigger marker.
func TestLifecycleAttribution_T9oSame(t *testing.T) {
	f, a := newS2Fixture(t)
	f.mint(a.ID, "R1")
	f.startWrite(a.ID)
	f.update(context.Background(), a.ID, func(a *store.Agent) { a.Generation++; a.Phase = "stopped" })
	evs := f.events(a.ID)
	assert.Equal(t, "created@1[/1] started@2[R1/1] reincarnated@3[/2] stopped@4(stopped)[/1]", lcFull(evs))
	checkLaunchMap(t, evs)
}

// TestLifecycleAttribution_T9rWitnessedReincarnation: the supported
// reincarnation sequence keeps clear; the output equals
// TestLifecycleStore2_Reincarnation's.
func TestLifecycleAttribution_T9rWitnessedReincarnation(t *testing.T) {
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
	assert.Equal(t, "R2", attrCursor(t, f, a.ID).ReincarnationMintRun, "the witness")
	f.update(ctx, a.ID, func(a *store.Agent) { a.Generation = 2 })
	att := attrCursor(t, f, a.ID)
	assert.Equal(t, store.AttributionClear, att.State)
	assert.Equal(t, "", att.ReincarnationMintRun, "cleared with the base run at the bump")
	f.update(ctx, a.ID, func(a *store.Agent) { a.ReincarnationState = store.ReincarnationStateNone })
	f.heartbeat(a.ID, "running", "R2")
	assert.Equal(t, "created@1[/1] started@2[R1/1] configured@3[/2] stopped@4(superseded)[R1/1] reincarnated@5[R2/2] started@6[R2/2]",
		lcFull(f.events(a.ID)))
}

// TestLifecycleAttribution_T9aAdoptionAfterClaim: an adoption after the
// claim does not witness the run; the bump is held/UNATTRIBUTED.
func TestLifecycleAttribution_T9aAdoptionAfterClaim(t *testing.T) {
	for _, tt := range []struct {
		name  string
		adopt func(f *lifecycleFixture, id string)
	}{
		{"cas", func(f *lifecycleFixture, id string) { f.cas(id, "R1", "Y1") }},
		// A DIRECT store-level heartbeat adoption: the hub refuses it while
		// a reincarnation is in flight.
		{"heartbeat (direct store call)", func(f *lifecycleFixture, id string) { f.heartbeatAdopt(id, "R1", "Y1") }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f, a := newS2Fixture(t)
			ctx := context.Background()
			f.mint(a.ID, "R1")
			f.startWrite(a.ID)
			cur, err := f.cs.GetAgent(ctx, a.ID)
			require.NoError(t, err)
			_, err = f.cs.ClaimAgentReincarnation(ctx, a.ID, cur.StateVersion, time.Now())
			require.NoError(t, err)
			tt.adopt(f, a.ID)
			f.update(ctx, a.ID, func(a *store.Agent) { a.Generation = 2 })
			assert.Equal(t, store.AttributionHeld, attrCursor(t, f, a.ID).State)
			evs := f.events(a.ID)
			last := evs[len(evs)-1]
			assert.Equal(t, tc.TransitionReincarnated, last.Transition)
			assert.Empty(t, last.LaunchID)
			checkLaunchMap(t, evs)
		})
	}
}

// TestLifecycleAttribution_T9cBrokerAnswersOwnRun (r2 N6): the witness is
// R2, a CAS moves the row to Y1 (clear), the bump is not witnessed: held;
// Y1 never maps to two generations.
func TestLifecycleAttribution_T9cBrokerAnswersOwnRun(t *testing.T) {
	f, a := newS2Fixture(t)
	ctx := context.Background()
	f.mint(a.ID, "R1")
	f.startWrite(a.ID)
	cur, err := f.cs.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	_, err = f.cs.ClaimAgentReincarnation(ctx, a.ID, cur.StateVersion, time.Now())
	require.NoError(t, err)
	f.mint(a.ID, "R2")
	f.cas(a.ID, "R2", "Y1")
	f.update(ctx, a.ID, func(a *store.Agent) { a.Generation = 2 })
	assert.Equal(t, store.AttributionHeld, attrCursor(t, f, a.ID).State)
	checkLaunchMap(t, f.events(a.ID))
}

// TestLifecycleAttribution_T9cSeg (r3 P1, complete mode): a started R2 in
// flight, a clear rebinding to Y1 (no marker), then the unwitnessed bump
// pushes Y1: the push-time marker fires and the closure omits and drops
// the adoption trio.
func TestLifecycleAttribution_T9cSeg(t *testing.T) {
	f, a := newS2Fixture(t)
	ctx := context.Background()
	f.mint(a.ID, "R1")
	f.startWrite(a.ID)
	f.stopRecord(a.ID, "R1", "stopped")
	cur, err := f.cs.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	_, err = f.cs.ClaimAgentReincarnation(ctx, a.ID, cur.StateVersion, time.Now())
	require.NoError(t, err)
	f.mint(a.ID, "R2")
	f.startWrite(a.ID)
	require.Equal(t, store.QuarantineComplete, attrCursor(t, f, a.ID).QuarantineMode)
	f.cas(a.ID, "R2", "Y1")
	assert.Equal(t, "", attrCursor(t, f, a.ID).UntrustedOpenRun, "a clear rebinding keeps no marker")
	f.update(ctx, a.ID, func(a *store.Agent) { a.Generation = 2 })
	assert.Equal(t, "Y1", attrCursor(t, f, a.ID).UntrustedOpenRun, "push-time marker on the rebound segment")
	f.stopRecord(a.ID, "Y1", "stopped")
	evs := f.events(a.ID)
	last := evs[len(evs)-1]
	assert.Equal(t, tc.TransitionStopped, last.Transition)
	assert.Empty(t, last.LaunchID)
	assert.Empty(t, last.AdoptedFromLaunchID, "the trio is dropped with the launch_id")
	checkLaunchMap(t, evs)
}

// TestLifecycleAttribution_T10GenerationChangeWhileHeld: the attribution is
// kept, but agent-state events after the bump omit (generation mismatch).
func TestLifecycleAttribution_T10GenerationChangeWhileHeld(t *testing.T) {
	f, a := heldAfterSoftFinish(t)
	f.bumpGeneration(a.ID)
	f.metaEdit(a.ID, "m-10")
	evs := f.events(a.ID)
	assert.Equal(t, "reincarnated@6[/2] metadata_changed@7[/2]", lcFull(evs[len(evs)-2:]))
	assert.Equal(t, "R1", attrCursor(t, f, a.ID).AttributedRun)
	checkLaunchMap(t, evs, "B1")
}

// TestLifecycleAttribution_T11LegacyCursor: a LEGACY cursor normalises to
// held/UNATTRIBUTED; restored omits; a fresh mint clears it.
func TestLifecycleAttribution_T11LegacyCursor(t *testing.T) {
	f, a := newS2Fixture(t)
	f.mint(a.ID, "R1")
	f.startWrite(a.ID)
	f.stopRecord(a.ID, "R1", "stopped")
	softFinish(t, f, a.ID, "B1", "R1")
	makeLegacy(t, f, a.ID)
	f.restore(a.ID)
	f.mint(a.ID, "R2")
	f.startWrite(a.ID)
	evs := f.events(a.ID)
	assert.Equal(t, "restored@5[/1] started@6[R2/1]", lcFull(evs[len(evs)-2:]))
	assert.Equal(t, int64(1), f.counters.Get(telemetryemit.MetricAttributionStale, telemetryemit.AttributionStaleLegacy))
	assert.Equal(t, store.QuarantineUnknown, attrCursor(t, f, a.ID).QuarantineMode, "UNKNOWN is sticky")
	checkLaunchMap(t, evs, "B1")
}

// TestLifecycleAttribution_T11sLegacyNamedSegment (R4): a LEGACY cursor's
// valid named segment closes with its original frozen pair; segment and
// marker are cleared together; non-closure events omit.
func TestLifecycleAttribution_T11sLegacyNamedSegment(t *testing.T) {
	for _, tt := range []struct {
		name         string
		intervening  bool
		explicitPush bool
		want         string
	}{
		{"T11s closure names the frozen pair", false, false, "stopped@3(stopped)[R1/1] metadata_changed@4[/1]"},
		// T11s': an intervening heartbeat (the otherwise row: no source).
		{"T11s' across writes", true, false, "stopped@3(stopped)[R1/1] metadata_changed@4[/1]"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f, a := newS2Fixture(t)
			f.mint(a.ID, "R1")
			f.startWrite(a.ID)
			makeLegacy(t, f, a.ID)
			if tt.intervening {
				f.heartbeat(a.ID, "running", "R1")
			}
			f.stopRecord(a.ID, "R1", "stopped")
			att := attrCursor(t, f, a.ID)
			assert.Equal(t, "", att.UntrustedOpenRun, "segment and marker cleared together")
			f.metaEdit(a.ID, "m-11s")
			evs := f.events(a.ID)
			assert.Equal(t, tt.want, lcFull(evs[len(evs)-2:]))
			checkLaunchMap(t, evs)
		})
	}
	// T11s'': the first observed write on the LEGACY cursor is an
	// unwitnessed same-run bump with a stop: the rule 7 push fires the
	// marker (the exclusion covers only the normalisation push).
	t.Run("T11s'' explicit push in the first write", func(t *testing.T) {
		f, a := newS2Fixture(t)
		f.mint(a.ID, "R1")
		f.startWrite(a.ID)
		makeLegacy(t, f, a.ID)
		f.update(context.Background(), a.ID, func(a *store.Agent) { a.Generation++; a.Phase = "stopped" })
		evs := f.events(a.ID)
		assert.Equal(t, "reincarnated@3[/2] stopped@4(stopped)[/1]", lcFull(evs[len(evs)-2:]))
		checkLaunchMap(t, evs)
	})
}

// TestLifecycleAttribution_T11uMarkerSurvivesOlderWriter: the new binary
// opens an untrusted segment; an older binary's update (stale stamp)
// leaves the segment untouched; the exact-match marker still omits.
func TestLifecycleAttribution_T11uMarkerSurvivesOlderWriter(t *testing.T) {
	f, a := newS2Fixture(t)
	f.mint(a.ID, "R1")
	f.startWrite(a.ID)
	f.stopRecord(a.ID, "R1", "stopped")
	softFinish(t, f, a.ID, "X1")
	f.restore(a.ID)
	f.heartbeat(a.ID, "running", "X1")
	olderBinaryUpdate(t, f, a.ID)
	f.stopRecord(a.ID, "X1", "stopped")
	evs := f.events(a.ID)
	assert.Equal(t, "stopped@7(stopped)[/1]", lcFull(evs[len(evs)-1:]))
	checkLaunchMap(t, evs, "X1")
}

// TestLifecycleAttribution_T11dOlderWriterReopens: the marker is for
// segment X1; an older binary closes it and opens a different named
// segment S2; S2 is trusted for its own closure and names its frozen pair.
func TestLifecycleAttribution_T11dOlderWriterReopens(t *testing.T) {
	f, a := newS2Fixture(t)
	ctx := context.Background()
	f.mint(a.ID, "R1")
	f.startWrite(a.ID)
	f.stopRecord(a.ID, "R1", "stopped")
	softFinish(t, f, a.ID, "X1")
	f.restore(a.ID)
	f.heartbeat(a.ID, "running", "X1")
	// The older binary: run S2 written and a named segment S2/1 opened (it
	// emitted started[S2/1]), stamp left stale.
	_, err := f.cs.client.Agent.UpdateOneID(uuid.MustParse(a.ID)).SetRunID("S2").Save(ctx)
	require.NoError(t, err)
	editCursor(t, f, a.ID, true, func(u *ent.AgentLifecycleCursorUpdateOne) {
		u.SetOpenRun("S2").SetOpenRunGeneration("1").SetOpenRunMetadataRevision(9).SetUpdatedAt(time.Now().UTC().Add(time.Second))
	})
	f.stopRecord(a.ID, "S2", "stopped")
	evs := f.events(a.ID)
	assert.Equal(t, "S2/1", evs[len(evs)-1].LaunchID+"/"+evs[len(evs)-1].IncarnationID, lcFull(evs))
	assert.Equal(t, tc.TransitionStopped, evs[len(evs)-1].Transition)
}

// TestLifecycleAttribution_T11dSameRunNewSegment: the older binary closes
// the untrusted X1 segment and reopens X1 by name at another revision
// (it emitted started[X1/1]): the marker is keyed to the exact
// (run, generation, revision), so the new segment is trusted for its own
// closure (R4), whereas a run-only comparison would omit it (M25).
func TestLifecycleAttribution_T11dSameRunNewSegment(t *testing.T) {
	f, a := newS2Fixture(t)
	f.mint(a.ID, "R1")
	f.startWrite(a.ID)
	f.stopRecord(a.ID, "R1", "stopped")
	softFinish(t, f, a.ID, "X1")
	f.restore(a.ID)
	f.heartbeat(a.ID, "running", "X1")
	editCursor(t, f, a.ID, true, func(u *ent.AgentLifecycleCursorUpdateOne) {
		u.SetOpenRunMetadataRevision(9).SetUpdatedAt(time.Now().UTC().Add(time.Second))
	})
	f.stopRecord(a.ID, "X1", "stopped")
	evs := f.events(a.ID)
	assert.Equal(t, "X1/1", evs[len(evs)-1].LaunchID+"/"+evs[len(evs)-1].IncarnationID, lcFull(evs))
}

// TestLifecycleAttribution_T11mMalformedSegment (P3, R4-6): a malformed
// segment identity is discarded at normalisation: no closure for it, one
// resync gap without launch_id, the cursor is bumped and later writes emit
// normally; the diagnostic counts it.
func TestLifecycleAttribution_T11mMalformedSegment(t *testing.T) {
	for _, tt := range []struct {
		name string
		edit func(u *ent.AgentLifecycleCursorUpdateOne)
	}{
		{"empty generation", func(u *ent.AgentLifecycleCursorUpdateOne) { u.SetOpenRunGeneration("") }},
		{"revision 0", func(u *ent.AgentLifecycleCursorUpdateOne) { u.SetOpenRunMetadataRevision(0) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f, a := newS2Fixture(t)
			f.mint(a.ID, "R1")
			f.startWrite(a.ID)
			editCursor(t, f, a.ID, false, tt.edit)
			f.stopRecord(a.ID, "R1", "stopped")
			f.metaEdit(a.ID, "m-11m")
			evs := f.events(a.ID)
			// No stopped closing R1's segment; the stop record on a running
			// row with no segment gives P7a stopped(start_unobserved).
			assert.Equal(t, "created@1[/1] started@2[R1/1] resynced@4(unobserved_change)[/1] stopped@5(stopped,start_unobserved)[/1] metadata_changed@6[/1]", lcFull(evs))
			gap := evs[2]
			require.NotNil(t, gap.Gap)
			assert.True(t, *gap.Gap)
			assert.Nil(t, gap.SegmentMetadataRevision)
			assert.Equal(t, int64(1), f.counters.Get(telemetryemit.MetricAttributionStale, telemetryemit.AttributionStaleDiscarded))
			assert.Zero(t, f.counters.Get(telemetryemit.MetricBuildFailures, "segment_state"))
			checkLaunchMap(t, evs)
		})
	}
	t.Run("valid segment with a partial marker", func(t *testing.T) {
		f, a := newS2Fixture(t)
		f.mint(a.ID, "R1")
		f.startWrite(a.ID)
		editCursor(t, f, a.ID, false, func(u *ent.AgentLifecycleCursorUpdateOne) { u.SetUntrustedOpenRun("R1") })
		f.stopRecord(a.ID, "R1", "stopped")
		evs := f.events(a.ID)
		assert.Equal(t, "stopped@3(stopped)[/1]", lcFull(evs[len(evs)-1:]), "a CORRUPT cursor's valid segment is untrusted")
		assert.Equal(t, int64(1), f.counters.Get(telemetryemit.MetricAttributionStale, telemetryemit.AttributionStaleCorrupt))
	})
	t.Run("purge of a soft-deleted row", func(t *testing.T) {
		f, a := newS2Fixture(t)
		ctx := context.Background()
		f.mint(a.ID, "R1")
		f.startWrite(a.ID)
		f.stopRecord(a.ID, "R1", "stopped")
		f.finalize(ctx, a.ID, store.DeletionFinalizeSoft)
		editCursor(t, f, a.ID, false, func(u *ent.AgentLifecycleCursorUpdateOne) {
			u.SetOpenRun("R1").SetOpenRunGeneration("").SetOpenRunMetadataRevision(1)
		})
		n0 := len(f.events(a.ID))
		f.finalize(ctx, a.ID, store.DeletionFinalizeHard)
		evs := f.events(a.ID)
		require.Len(t, evs, n0+1, "exactly one gap and no deleted: %s", lcFull(evs))
		assert.Equal(t, tc.TransitionResynced, evs[n0].Transition)
		assert.Empty(t, evs[n0].LaunchID)
	})
	t.Run("no second gap when the write already has one", func(t *testing.T) {
		f, a := newS2Fixture(t)
		f.mint(a.ID, "R1")
		f.startWrite(a.ID)
		editCursor(t, f, a.ID, false, func(u *ent.AgentLifecycleCursorUpdateOne) { u.SetOpenRunMetadataRevision(0) })
		// An unobserved phase change makes the fingerprint mismatch: the
		// write has its own resync gap.
		_, err := f.cs.client.Agent.UpdateOneID(uuid.MustParse(a.ID)).SetPhase("stopping").Save(context.Background())
		require.NoError(t, err)
		f.metaEdit(a.ID, "m-gap")
		evs := f.events(a.ID)
		assert.Equal(t, 1, countTransitions(evs, tc.TransitionResynced), lcFull(evs))
	})
	t.Run("rollback commits neither", func(t *testing.T) {
		f, a := newS2Fixture(t)
		f.mint(a.ID, "R1")
		f.startWrite(a.ID)
		editCursor(t, f, a.ID, false, func(u *ent.AgentLifecycleCursorUpdateOne) { u.SetOpenRunMetadataRevision(0) })
		before := attrCursor(t, f, a.ID)
		rows0, rev0 := softFinishState(t, f, a.ID)
		f.cs.SetAgentWriteObserver(failingObserver{err: errInjectedLC})
		err := f.cs.UpdateAgentStatus(context.Background(), a.ID, store.AgentStatusUpdate{Phase: "stopped", IfRunID: "R1"})
		f.cs.SetAgentWriteObserver(f.obs)
		require.ErrorIs(t, err, errInjectedLC)
		rows1, rev1 := softFinishState(t, f, a.ID)
		assert.Equal(t, before, attrCursor(t, f, a.ID))
		assert.Equal(t, rows0, rows1)
		assert.Equal(t, rev0, rev1)
		c, err := f.cs.client.AgentLifecycleCursor.Get(context.Background(), uuid.MustParse(a.ID))
		require.NoError(t, err)
		assert.Equal(t, int64(0), c.OpenRunMetadataRevision, "the malformed segment is still stored")
	})
}

// TestLifecycleAttribution_T12MissingCursor: the cursor row is gone; the
// next write resyncs(no_cursor) without launch_id, held until a mint.
func TestLifecycleAttribution_T12MissingCursor(t *testing.T) {
	f, a := newS2Fixture(t)
	f.mint(a.ID, "R1")
	f.startWrite(a.ID)
	f.stopRecord(a.ID, "R1", "stopped")
	_, err := f.cs.client.AgentLifecycleCursor.Delete().Exec(context.Background())
	require.NoError(t, err)
	f.metaEdit(a.ID, "m-12")
	f.mint(a.ID, "R2")
	f.startWrite(a.ID)
	evs := f.events(a.ID)
	assert.Equal(t, "resynced@1(no_cursor)[/1] metadata_changed@2[/1] started@3[R2/1]", lcFull(evs[len(evs)-3:]))
	assert.Equal(t, int64(1), f.counters.Get(telemetryemit.MetricAttributionStale, telemetryemit.AttributionStaleMissing))
	assert.Equal(t, store.QuarantineUnknown, attrCursor(t, f, a.ID).QuarantineMode)
}

// TestLifecycleAttribution_T11mLegacyPartialMarker (contract review TA-1):
// a LEGACY cursor (attribution_state "", NULL stamp) with a valid named
// segment but a partial or malformed marker is CORRUPT: the valid segment
// is untrusted and its closure omits launch_id. The controls: a valid
// LEGACY cursor without a marker closes with its frozen pair (R4); an
// exact-match marker on that segment omits.
func TestLifecycleAttribution_T11mLegacyPartialMarker(t *testing.T) {
	legacyWith := func(t *testing.T, marker func(u *ent.AgentLifecycleCursorUpdateOne)) (*lifecycleFixture, *store.Agent) {
		f, a := newS2Fixture(t)
		f.mint(a.ID, "R1")
		f.startWrite(a.ID)
		makeLegacy(t, f, a.ID)
		if marker != nil {
			editCursor(t, f, a.ID, true, marker)
		}
		return f, a
	}
	stop := func(t *testing.T, f *lifecycleFixture, id string) string {
		f.stopRecord(id, "R1", "stopped")
		evs := f.events(id)
		return lcFull(evs[len(evs)-1:])
	}
	for _, tt := range []struct {
		name   string
		marker func(u *ent.AgentLifecycleCursorUpdateOne)
	}{
		{"run only", func(u *ent.AgentLifecycleCursorUpdateOne) { u.SetUntrustedOpenRun("R1") }},
		{"run and generation, no revision", func(u *ent.AgentLifecycleCursorUpdateOne) {
			u.SetUntrustedOpenRun("R1").SetUntrustedOpenRunGeneration("1")
		}},
		{"non-decimal generation", func(u *ent.AgentLifecycleCursorUpdateOne) {
			u.SetUntrustedOpenRun("R1").SetUntrustedOpenRunGeneration("x").SetUntrustedOpenRunRev(2)
		}},
		{"revision without a run", func(u *ent.AgentLifecycleCursorUpdateOne) { u.SetUntrustedOpenRunRev(2) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f, a := legacyWith(t, tt.marker)
			assert.Equal(t, "stopped@3(stopped)[/1]", stop(t, f, a.ID))
			assert.Equal(t, int64(1), f.counters.Get(telemetryemit.MetricAttributionStale, telemetryemit.AttributionStaleCorrupt))
			assert.Zero(t, f.counters.Get(telemetryemit.MetricAttributionStale, telemetryemit.AttributionStaleLegacy))
		})
	}
	t.Run("control: valid LEGACY without a marker names the frozen pair", func(t *testing.T) {
		f, a := legacyWith(t, nil)
		assert.Equal(t, "stopped@3(stopped)[R1/1]", stop(t, f, a.ID))
		assert.Equal(t, int64(1), f.counters.Get(telemetryemit.MetricAttributionStale, telemetryemit.AttributionStaleLegacy))
	})
	t.Run("control: an exact-match marker on a LEGACY-state cursor omits", func(t *testing.T) {
		f, a := legacyWith(t, nil)
		c, err := f.cs.client.AgentLifecycleCursor.Get(context.Background(), uuid.MustParse(a.ID))
		require.NoError(t, err)
		editCursor(t, f, a.ID, true, func(u *ent.AgentLifecycleCursorUpdateOne) {
			u.SetUntrustedOpenRun(c.OpenRun).SetUntrustedOpenRunGeneration(c.OpenRunGeneration).SetUntrustedOpenRunRev(c.OpenRunMetadataRevision)
		})
		assert.Equal(t, "stopped@3(stopped)[/1]", stop(t, f, a.ID))
	})
}
