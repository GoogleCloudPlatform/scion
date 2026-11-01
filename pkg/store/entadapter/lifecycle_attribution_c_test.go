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

// Persistent run attribution fixtures, part 3 (attribution design v2.8,
// section 10.1): corrupt state, rollback, quarantine and undo, rebinding,
// non-fresh mints, the UNKNOWN coverage limit and the effective echo.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	tc "github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetryemit"
)

// stoppedAgent is a fixture agent that ran R1 and stopped (clear).
func stoppedAgent(t *testing.T) (*lifecycleFixture, *store.Agent) {
	t.Helper()
	f, a := newS2Fixture(t)
	f.mint(a.ID, "R1")
	f.startWrite(a.ID)
	f.stopRecord(a.ID, "R1", "stopped")
	return f, a
}

func tail(evs []*tc.LifecycleEvent, n int) string {
	if n > len(evs) {
		n = len(evs)
	}
	return lcFull(evs[len(evs)-n:])
}

// TestLifecycleAttribution_T13Corrupt: every CORRUPT pattern of 4.1 fails
// closed to held/UNATTRIBUTED with an UNKNOWN history.
func TestLifecycleAttribution_T13Corrupt(t *testing.T) {
	for _, tt := range []struct {
		name string
		edit func(u *ent.AgentLifecycleCursorUpdateOne)
	}{
		{"unknown state", func(u *ent.AgentLifecycleCursorUpdateOne) { u.SetAttributionState("bogus") }},
		{"held attribution without generation", func(u *ent.AgentLifecycleCursorUpdateOne) {
			u.SetAttributionState(store.AttributionHeld).SetAttributedRun("R0").SetAttributedGeneration("")
		}},
		{"non-decimal generation", func(u *ent.AgentLifecycleCursorUpdateOne) { u.SetLastGeneration("x") }},
		{"run without generation", func(u *ent.AgentLifecycleCursorUpdateOne) { u.SetLastGeneration("") }},
		{"clear attribution without quarantine", func(u *ent.AgentLifecycleCursorUpdateOne) {
			u.SetAttributedRun("R0").SetAttributedGeneration("1")
		}},
		{"quarantine empty element", func(u *ent.AgentLifecycleCursorUpdateOne) { u.SetQuarantinedRuns("A,,B") }},
		{"quarantine duplicate", func(u *ent.AgentLifecycleCursorUpdateOne) { u.SetQuarantinedRuns("A,A") }},
		{"quarantine too long", func(u *ent.AgentLifecycleCursorUpdateOne) { u.SetQuarantinedRuns("a,b,c,d,e,f,g,h,i") }},
		{"unknown quarantine mode", func(u *ent.AgentLifecycleCursorUpdateOne) { u.SetQuarantineMode("weird") }},
		{"partial marker", func(u *ent.AgentLifecycleCursorUpdateOne) { u.SetUntrustedOpenRun("R1") }},
		{"witness without a base run", func(u *ent.AgentLifecycleCursorUpdateOne) { u.SetReincarnationMintRun("R1") }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f, a := stoppedAgent(t)
			editCursor(t, f, a.ID, false, tt.edit)
			f.metaEdit(a.ID, "m-13")
			evs := f.events(a.ID)
			assert.Equal(t, "metadata_changed@4[/1]", tail(evs, 1))
			att := attrCursor(t, f, a.ID)
			assert.Equal(t, store.AttributionHeld, att.State)
			assert.Equal(t, store.QuarantineUnknown, att.QuarantineMode)
			assert.Equal(t, int64(1), f.counters.Get(telemetryemit.MetricAttributionStale, telemetryemit.AttributionStaleCorrupt))
		})
	}
	t.Run("LEGACY state with a stamp", func(t *testing.T) {
		f, a := stoppedAgent(t)
		editCursor(t, f, a.ID, false, func(u *ent.AgentLifecycleCursorUpdateOne) { u.SetAttributionState("") })
		f.metaEdit(a.ID, "m-13")
		assert.Equal(t, int64(1), f.counters.Get(telemetryemit.MetricAttributionStale, telemetryemit.AttributionStaleCorrupt))
	})
}

// TestLifecycleAttribution_T14Rollback: an observer failure rolls the write
// back with the attribution columns, the stamp, the marker, the list, the
// revision and the outbox unchanged, for every new write.
func TestLifecycleAttribution_T14Rollback(t *testing.T) {
	type state struct {
		att  store.LifecycleAttribution
		rows int
		rev  int64
	}
	snap := func(t *testing.T, f *lifecycleFixture, id string) state {
		rows, rev := softFinishState(t, f, id)
		return state{att: attrCursor(t, f, id), rows: rows, rev: rev}
	}
	for _, tt := range []struct {
		name  string
		setup func(t *testing.T) (*lifecycleFixture, *store.Agent)
		write func(f *lifecycleFixture, id string) error
	}{
		{"soft finish", stoppedAgent, func(f *lifecycleFixture, id string) error {
			_, err := softFinishRun(f.t, f, id, "B1", []string{"R1"})
			return err
		}},
		{"restore", func(t *testing.T) (*lifecycleFixture, *store.Agent) {
			f, a := stoppedAgent(t)
			softFinish(t, f, a.ID, "B1", "R1")
			return f, a
		}, func(f *lifecycleFixture, id string) error {
			a, err := f.cs.GetAgent(context.Background(), id)
			if err != nil {
				return err
			}
			a.DeletedAt = time.Time{}
			return f.cs.UpdateAgent(context.Background(), a)
		}},
		{"establishing mint", heldAfterSoftFinish, func(f *lifecycleFixture, id string) error {
			_, err := f.cs.SetAgentRunID(context.Background(), id, "R2", nil)
			return err
		}},
		{"T7 revert while held", heldAfterSoftFinish, func(f *lifecycleFixture, id string) error {
			_, err := f.cs.RevertAgentRunID(context.Background(), id, "B1", "Y1")
			return err
		}},
		{"T19 revert to the quarantined run", func(t *testing.T) (*lifecycleFixture, *store.Agent) {
			f, a := heldAfterSoftFinish(t)
			f.mint(a.ID, "R2")
			return f, a
		}, func(f *lifecycleFixture, id string) error {
			_, err := f.cs.RevertAgentRunID(context.Background(), id, "R2", "B1")
			return err
		}},
		{"T32 rebinding", func(t *testing.T) (*lifecycleFixture, *store.Agent) {
			f, a := heldAfterSoftFinish(t)
			f.mint(a.ID, "R2")
			f.startWrite(a.ID)
			return f, a
		}, func(f *lifecycleFixture, id string) error {
			_, err := f.cs.CompareAndSwapAgentRunID(context.Background(), id, "R2", "B1")
			return err
		}},
		{"T35 non-fresh mint", heldAfterSoftFinish, func(f *lifecycleFixture, id string) error {
			_, err := f.cs.SetAgentRunID(context.Background(), id, "R1", nil)
			return err
		}},
		{"T9o-seg bump", func(t *testing.T) (*lifecycleFixture, *store.Agent) {
			f, a := newS2Fixture(t)
			f.mint(a.ID, "R1")
			f.startWrite(a.ID)
			return f, a
		}, func(f *lifecycleFixture, id string) error {
			a, err := f.cs.GetAgent(context.Background(), id)
			if err != nil {
				return err
			}
			a.Generation++
			return f.cs.UpdateAgent(context.Background(), a)
		}},
		{"T5c soft finish", func(t *testing.T) (*lifecycleFixture, *store.Agent) {
			f, a := newS2Fixture(t)
			f.mint(a.ID, "B1")
			f.startWrite(a.ID)
			f.mint(a.ID, "R1")
			return f, a
		}, func(f *lifecycleFixture, id string) error {
			_, err := softFinishRun(f.t, f, id, "B1", []string{"R1"})
			return err
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f, a := tt.setup(t)
			before := snap(t, f, a.ID)
			f.cs.SetAgentWriteObserver(failingObserver{err: errInjectedLC})
			err := tt.write(f, a.ID)
			f.cs.SetAgentWriteObserver(f.obs)
			require.ErrorIs(t, err, errInjectedLC)
			assert.Equal(t, before, snap(t, f, a.ID))
		})
	}
}

// TestLifecycleAttribution_T15Unmatched: a soft finish whose predicate no
// longer matches writes nothing: the attribution is unchanged.
func TestLifecycleAttribution_T15Unmatched(t *testing.T) {
	f, a := stoppedAgent(t)
	before := attrCursor(t, f, a.ID)
	now := time.Now()
	b, prev := "B1", []string{"R1"}
	n, err := f.cs.FinalizeAgentDeletion(context.Background(), a.ID,
		store.DeletionPredicate{States: []string{store.DeletionStateFinalizing}}, store.DeletionFinalizeSoft,
		store.DeletionFields{DeletedAt: &now, SoftFinishRunID: &b, SoftFinishPreviousRunIDs: &prev}, nil)
	require.NoError(t, err)
	require.Zero(t, n)
	assert.Equal(t, before, attrCursor(t, f, a.ID))
}

// TestLifecycleAttribution_T16CoherentCursorQualification: with a coherent
// cursor the soft finish emits no resynced; with a fingerprint-mismatched
// cursor the resynced(unobserved_change, gap) diagnostic comes first.
func TestLifecycleAttribution_T16CoherentCursorQualification(t *testing.T) {
	f, a := stoppedAgent(t)
	softFinish(t, f, a.ID, "B1", "R1")
	assert.Zero(t, countTransitions(f.events(a.ID), tc.TransitionResynced))

	f2, a2 := stoppedAgent(t)
	_, err := f2.cs.client.Agent.UpdateOneID(uuid.MustParse(a2.ID)).SetPhase("error").Save(context.Background())
	require.NoError(t, err)
	softFinish(t, f2, a2.ID, "B1", "R1")
	evs := f2.events(a2.ID)
	assert.Equal(t, "resynced@5(unobserved_change)[R1/1] deleted@6(soft)[R1/1]", tail(evs, 2))
	require.NotNil(t, evs[len(evs)-2].Gap)
}

// TestLifecycleAttribution_T19RevertAfterEstablishment (r1 R1): restore, a
// fresh mint R2, a failed start, the dispatcher's revert R2 -> B1, then a
// delete: the undo restores the held attribution; nothing names B1.
func TestLifecycleAttribution_T19RevertAfterEstablishment(t *testing.T) {
	f, a := heldAfterSoftFinish(t)
	f.mint(a.ID, "R2")
	f.status(context.Background(), a.ID, store.AgentStatusUpdate{Phase: "starting"})
	f.revert(a.ID, "R2", "B1")
	att := attrCursor(t, f, a.ID)
	assert.Equal(t, store.AttributionHeld, att.State)
	assert.Equal(t, "R1", att.AttributedRun)
	f.hardDelete(a.ID)
	evs := f.events(a.ID)
	assert.Equal(t, "deleted@6(hard)[R1/1]", tail(evs, 1))
	checkLaunchMap(t, evs, "B1")
}

// TestLifecycleAttribution_T19uUndoUnderUnknown (r2 N2, r3 L1): LEGACY set
// before T1's mint R1, so R1 establishes (clear, UNKNOWN); the undo still
// applies under UNKNOWN.
func TestLifecycleAttribution_T19uUndoUnderUnknown(t *testing.T) {
	f, a := newS2Fixture(t)
	makeLegacy(t, f, a.ID)
	f.mint(a.ID, "R1")
	f.startWrite(a.ID)
	f.stopRecord(a.ID, "R1", "stopped")
	softFinish(t, f, a.ID, "B1", "R1")
	f.restore(a.ID)
	f.mint(a.ID, "R2")
	f.revert(a.ID, "R2", "B1")
	f.hardDelete(a.ID)
	evs := f.events(a.ID)
	assert.Equal(t, "deleted@4(soft)[R1/1] restored@5[R1/1] deleted@6(hard)[R1/1]", tail(evs, 3))
	checkLaunchMap(t, evs, "B1")
}

// TestLifecycleAttribution_T19uPrimeLegacyAtSoftFinish: LEGACY set just
// before the soft finish: PRE is held/UNATTRIBUTED, so deleted and
// restored omit (D3).
func TestLifecycleAttribution_T19uPrimeLegacyAtSoftFinish(t *testing.T) {
	f, a := stoppedAgent(t)
	makeLegacy(t, f, a.ID)
	softFinish(t, f, a.ID, "B1", "R1")
	f.restore(a.ID)
	evs := f.events(a.ID)
	assert.Equal(t, "deleted@4(soft)[/1] restored@5[/1]", tail(evs, 2))
	checkLaunchMap(t, evs, "B1")
}

// TestLifecycleAttribution_T20 (r1 R1 paths, r2 N9a): the move rollback's
// revert after one mint takes the undo; after a CAS or a second mint the
// undo is gone (UNATTRIBUTED); an adoption back to B1, now or after
// further mints, is held/UNATTRIBUTED.
func TestLifecycleAttribution_T20(t *testing.T) {
	expect := func(t *testing.T, f *lifecycleFixture, id, want string) {
		t.Helper()
		f.hardDelete(id)
		evs := f.events(id)
		assert.Equal(t, want, tail(evs, 1))
		checkLaunchMap(t, evs, "B1")
	}
	t.Run("a1 move rollback after one mint", func(t *testing.T) {
		f, a := heldAfterSoftFinish(t)
		f.mint(a.ID, "R2")
		f.revert(a.ID, "R2", "B1")
		expect(t, f, a.ID, "deleted@6(hard)[R1/1]")
	})
	t.Run("a2 move rollback after a CAS", func(t *testing.T) {
		f, a := heldAfterSoftFinish(t)
		f.mint(a.ID, "R2")
		f.cas(a.ID, "R2", "Z1")
		f.revert(a.ID, "Z1", "B1")
		expect(t, f, a.ID, "deleted@6(hard)[/1]")
	})
	t.Run("a2 move rollback after a second mint", func(t *testing.T) {
		f, a := heldAfterSoftFinish(t)
		f.mint(a.ID, "R2")
		f.mint(a.ID, "R3")
		f.revert(a.ID, "R3", "B1")
		expect(t, f, a.ID, "deleted@6(hard)[/1]")
	})
	t.Run("b CAS to B1", func(t *testing.T) {
		f, a := heldAfterSoftFinish(t)
		f.mint(a.ID, "R2")
		f.cas(a.ID, "R2", "B1")
		expect(t, f, a.ID, "deleted@6(hard)[/1]")
	})
	t.Run("b heartbeat adoption of B1 (direct store call)", func(t *testing.T) {
		f, a := heldAfterSoftFinish(t)
		f.mint(a.ID, "R2")
		f.heartbeatAdopt(a.ID, "R2", "B1")
		expect(t, f, a.ID, "deleted@6(hard)[/1]")
	})
	t.Run("c later return after a second mint", func(t *testing.T) {
		f, a := heldAfterSoftFinish(t)
		f.mint(a.ID, "R2")
		f.mint(a.ID, "R3")
		f.heartbeatAdopt(a.ID, "R3", "B1")
		assert.Contains(t, strings.Split(attrCursor(t, f, a.ID).QuarantinedRuns, ","), "B1", "the quarantine survives mints")
		expect(t, f, a.ID, "deleted@6(hard)[/1]")
	})
}

// TestLifecycleAttribution_T21ReincarnationInFlight (r1 R3): a soft finish
// between a reincarnation mint and its bump stores UNATTRIBUTED; no launch
// maps to two generations.
func TestLifecycleAttribution_T21ReincarnationInFlight(t *testing.T) {
	f, a := newS2Fixture(t)
	ctx := context.Background()
	f.mint(a.ID, "R1")
	f.startWrite(a.ID)
	f.stopRecord(a.ID, "R1", "stopped")
	cur, err := f.cs.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	_, err = f.cs.ClaimAgentReincarnation(ctx, a.ID, cur.StateVersion, time.Now())
	require.NoError(t, err)
	f.update(ctx, a.ID, func(a *store.Agent) { a.ReincarnationState = store.ReincarnationStatePending })
	f.mint(a.ID, "R2")
	softFinish(t, f, a.ID, "R1", "R2")
	f.restore(a.ID)
	assert.Equal(t, "", attrCursor(t, f, a.ID).AttributedRun)
	evs := f.events(a.ID)
	assert.Empty(t, lastOf(evs, tc.TransitionDeleted).LaunchID)
	assert.Empty(t, lastOf(evs, tc.TransitionRestored).LaunchID)
	checkLaunchMap(t, evs)
}

// TestLifecycleAttribution_T22QuietMint (r1 R2): a mint that emits nothing
// still persists clear and last_run (the unchanged-skip compares the
// attribution); the next write names the run.
func TestLifecycleAttribution_T22QuietMint(t *testing.T) {
	f, a := stoppedAgent(t)
	n0 := len(f.events(a.ID))
	f.mint(a.ID, "R2")
	require.Len(t, f.events(a.ID), n0, "the mint emits nothing")
	att := attrCursor(t, f, a.ID)
	assert.Equal(t, store.AttributionClear, att.State)
	assert.Equal(t, "R2", att.LastRun)
	f.startWrite(a.ID)
	assert.Equal(t, "started@4[R2/1]", tail(f.events(a.ID), 1))
}

// TestLifecycleAttribution_T23BuildFailureCommits: the soft finish commits
// without a bump (withoutObservation: a no-op observer for that write,
// simulating the commit-without-bump state of the build-failure path, not
// that executed path); the restore normalises to
// held/UNATTRIBUTED; nothing names B1. T23b: then a fresh mint, a failed
// start and a revert to B1: B1 was pushed at the normalisation, so the
// revert is held (rule 4).
func TestLifecycleAttribution_T23BuildFailureCommits(t *testing.T) {
	f, a := stoppedAgent(t)
	f.withoutObservation(func() { softFinish(t, f, a.ID, "B1", "R1") })
	f.restore(a.ID)
	evs := f.events(a.ID)
	assert.Equal(t, "resynced@5(unobserved_change)[/1] restored@6[/1]", tail(evs, 2))
	checkLaunchMap(t, evs, "B1")
	assert.Contains(t, strings.Split(attrCursor(t, f, a.ID).QuarantinedRuns, ","), "B1")

	// T23b.
	f.mint(a.ID, "R2")
	f.revert(a.ID, "R2", "B1")
	assert.Equal(t, store.AttributionHeld, attrCursor(t, f, a.ID).State)
	f.metaEdit(a.ID, "m-23b")
	evs = f.events(a.ID)
	assert.Equal(t, "metadata_changed@7[/1]", tail(evs, 1))
	checkLaunchMap(t, evs, "B1")
}

// TestLifecycleAttribution_T23cHeldRowNormalisation (r5 R5-1 path ii, r6
// R6-5): a pending clear-rebinding carrier is never first named after its
// run was held. The intervening writes are labelled direct same-value
// settles (CompareAndSwapAgentRunID(X, X)), which return before Derive
// step 2.
func TestLifecycleAttribution_T23cHeldRowNormalisation(t *testing.T) {
	run := func(t *testing.T, rebound bool) {
		f, a := stoppedAgent(t)
		if rebound {
			f.mint(a.ID, "R2")
			f.startWrite(a.ID)
			f.cas(a.ID, "R2", "Y1") // a clear rebinding; the carrier is pending
		} else {
			f.mint(a.ID, "Y1")
			f.startWrite(a.ID)
		}
		f.withoutObservation(func() { f.cas(a.ID, "Y1", "W1") })
		f.cas(a.ID, "W1", "W1") // direct same-value settle: no step 2
		att := attrCursor(t, f, a.ID)
		c, err := f.cs.client.AgentLifecycleCursor.Get(context.Background(), uuid.MustParse(a.ID))
		require.NoError(t, err)
		assert.Equal(t, "Y1", c.OpenRun, "the segment is still Y1")
		assert.Equal(t, store.AttributionHeld, att.State)
		assert.Equal(t, "W1", att.LastRun)
		assert.Equal(t, "", att.UntrustedOpenRun, "no marker yet")
		f.withoutObservation(func() { f.cas(a.ID, "W1", "Y1") })
		f.cas(a.ID, "Y1", "Y1")
		assert.Equal(t, "Y1", attrCursor(t, f, a.ID).UntrustedOpenRun, "held-row normalisation fires the marker")
		f.stopRecord(a.ID, "Y1", "stopped")
		evs := f.events(a.ID)
		last := evs[len(evs)-1]
		assert.Equal(t, tc.TransitionStopped, last.Transition)
		assert.Empty(t, last.LaunchID)
		assert.Empty(t, last.AdoptedFromLaunchID)
		checkLaunchMap(t, evs)
	}
	t.Run("rebound segment", func(t *testing.T) { run(t, true) })
	t.Run("named segment", func(t *testing.T) { run(t, false) })
}

// TestLifecycleAttribution_T24ObserverOffOn: an unobserved soft finish and
// restore with a valid stamp; the next observed write holds from the run
// difference.
func TestLifecycleAttribution_T24ObserverOffOn(t *testing.T) {
	f, a := stoppedAgent(t)
	f.withoutObservation(func() {
		softFinish(t, f, a.ID, "B1", "R1")
		f.restore(a.ID)
	})
	f.metaEdit(a.ID, "m-24")
	att := attrCursor(t, f, a.ID)
	assert.Equal(t, store.AttributionHeld, att.State)
	assert.Equal(t, "B1", att.LastRun)
	evs := f.events(a.ID)
	assert.Equal(t, "metadata_changed@4[/1]", tail(evs, 1))
	checkLaunchMap(t, evs, "B1")
}

// TestLifecycleAttribution_T26QuarantineOverflow: a ninth held run makes
// the history UNKNOWN for good; a return to the evicted B1 after a fresh
// mint is held (6U).
func TestLifecycleAttribution_T26QuarantineOverflow(t *testing.T) {
	f, a := newS2Fixture(t)
	runs := []string{"1", "2", "3", "4", "5", "6", "7", "8", "9"}
	for _, i := range runs {
		f.mint(a.ID, "R"+i)
		softFinish(t, f, a.ID, "B"+i, "R"+i)
		f.restore(a.ID)
	}
	att := attrCursor(t, f, a.ID)
	assert.Equal(t, store.QuarantineUnknown, att.QuarantineMode)
	assert.NotContains(t, strings.Split(att.QuarantinedRuns, ","), "B1", "evicted")
	f.mint(a.ID, "R10")
	assert.Equal(t, store.QuarantineUnknown, attrCursor(t, f, a.ID).QuarantineMode, "sticky after a mint")
	f.revert(a.ID, "R10", "B1")
	assert.Equal(t, store.AttributionHeld, attrCursor(t, f, a.ID).State)
	f.metaEdit(a.ID, "m-26")
	checkLaunchMap(t, f.events(a.ID), "B1", "B2", "B3", "B4", "B5", "B6", "B7", "B8", "B9")
}

// TestLifecycleAttribution_T27HardDelete: a held live row's hard delete
// names R1/1. R6-2: a write with no POST still fires the trigger for
// closure selection (a revert committed without a bump via
// withoutObservation, simulating the commit-without-bump state, then
// DeleteAgent).
func TestLifecycleAttribution_T27HardDelete(t *testing.T) {
	f, a := heldAfterSoftFinish(t)
	f.hardDelete(a.ID)
	assert.Equal(t, "deleted@6(hard)[R1/1]", tail(f.events(a.ID), 1))

	f2, a2 := newS2Fixture(t)
	f2.mint(a2.ID, "R1")
	f2.startWrite(a2.ID)
	f2.mint(a2.ID, "M1")
	f2.withoutObservation(func() { f2.revert(a2.ID, "M1", "R1") })
	f2.hardDelete(a2.ID)
	evs := f2.events(a2.ID)
	last := evs[len(evs)-1]
	assert.Equal(t, "deleted@3(hard)[/1]", tail(evs, 1))
	require.NotNil(t, last.SegmentMetadataRevision)
	checkLaunchMap(t, evs)
}

// TestLifecycleAttribution_T28ProjectDelete: a held live row's project
// delete names R1/1.
func TestLifecycleAttribution_T28ProjectDelete(t *testing.T) {
	f, a := heldAfterSoftFinish(t)
	require.NoError(t, f.cs.DeleteProject(context.Background(), f.projectID))
	last := lastOf(f.events(a.ID), tc.TransitionDeleted)
	require.NotNil(t, last)
	assert.Equal(t, "R1/1", last.LaunchID+"/"+last.IncarnationID)
}

// TestLifecycleAttribution_T31UnknownPriorHistory: LEGACY, missing,
// CORRUPT and stale cursors: a fresh mint establishes only itself; a
// revert or adoption to an earlier run unknown to the cursor is held.
func TestLifecycleAttribution_T31UnknownPriorHistory(t *testing.T) {
	for _, tt := range []struct {
		name   string
		inject func(t *testing.T, f *lifecycleFixture, id string)
	}{
		{"a legacy", makeLegacy},
		{"b missing", func(t *testing.T, f *lifecycleFixture, id string) {
			_, err := f.cs.client.AgentLifecycleCursor.Delete().Exec(context.Background())
			require.NoError(t, err)
		}},
		{"c corrupt", func(t *testing.T, f *lifecycleFixture, id string) {
			editCursor(t, f, id, false, func(u *ent.AgentLifecycleCursorUpdateOne) { u.SetQuarantineMode("weird") })
		}},
		{"d stale stamp", olderBinaryUpdate},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f, a := newS2Fixture(t)
			f.mint(a.ID, "B0")
			f.startWrite(a.ID)
			f.stopRecord(a.ID, "B0", "stopped")
			tt.inject(t, f, a.ID)
			f.mint(a.ID, "R2")
			att := attrCursor(t, f, a.ID)
			assert.Equal(t, store.AttributionClear, att.State)
			assert.Equal(t, store.QuarantineUnknown, att.QuarantineMode)
			f.revert(a.ID, "R2", "B0")
			assert.Equal(t, store.AttributionHeld, attrCursor(t, f, a.ID).State)
			f.metaEdit(a.ID, "m-31")
			evs := f.events(a.ID)
			assert.Empty(t, evs[len(evs)-1].LaunchID, lcFull(evs))
		})
	}
}

// TestLifecycleAttribution_T32Rebinding (r2 N1): a rebinding while held,
// to a quarantined run, or under UNKNOWN marks the rebound segment; its
// closures omit. (d) is the control: a clear rebinding names Y1 and
// carries the adoption trio.
func TestLifecycleAttribution_T32Rebinding(t *testing.T) {
	stopAndDelete := func(t *testing.T, f *lifecycleFixture, id, run string, banned ...string) {
		f.stopRecord(id, run, "stopped")
		evs := f.events(id)
		last := evs[len(evs)-1]
		assert.Equal(t, tc.TransitionStopped, last.Transition)
		assert.Empty(t, last.LaunchID, lcFull(evs))
		f.finalize(context.Background(), id, store.DeletionFinalizeSoft)
		evs = f.events(id)
		assert.Empty(t, evs[len(evs)-1].LaunchID, lcFull(evs))
		checkLaunchMap(t, evs, banned...)
	}
	t.Run("a CAS to a quarantined run", func(t *testing.T) {
		f, a := heldAfterSoftFinish(t)
		f.mint(a.ID, "R2")
		f.startWrite(a.ID)
		f.cas(a.ID, "R2", "B1")
		assert.Equal(t, "B1", attrCursor(t, f, a.ID).UntrustedOpenRun)
		stopAndDelete(t, f, a.ID, "B1", "B1")
	})
	t.Run("a heartbeat adoption of a quarantined run (direct store call)", func(t *testing.T) {
		f, a := heldAfterSoftFinish(t)
		f.mint(a.ID, "R2")
		f.startWrite(a.ID)
		f.heartbeatAdopt(a.ID, "R2", "B1")
		stopAndDelete(t, f, a.ID, "B1", "B1")
	})
	t.Run("b UNKNOWN 6U", func(t *testing.T) {
		f, a := newS2Fixture(t)
		makeLegacy(t, f, a.ID)
		f.mint(a.ID, "R2")
		f.startWrite(a.ID)
		f.cas(a.ID, "R2", "Y1")
		assert.Equal(t, "Y1", attrCursor(t, f, a.ID).UntrustedOpenRun)
		stopAndDelete(t, f, a.ID, "Y1", "Y1")
	})
	t.Run("c held rule 5", func(t *testing.T) {
		f, a := newS2Fixture(t)
		f.mint(a.ID, "R1")
		f.startWrite(a.ID)
		f.stopRecord(a.ID, "R1", "stopped")
		softFinish(t, f, a.ID, "X1")
		f.restore(a.ID)
		f.heartbeat(a.ID, "running", "X1")
		f.cas(a.ID, "X1", "Y1")
		assert.Equal(t, "Y1", attrCursor(t, f, a.ID).UntrustedOpenRun, "the marker moves with the rebinding")
		stopAndDelete(t, f, a.ID, "Y1", "X1", "Y1")
	})
	t.Run("d control: a clear rebinding", func(t *testing.T) {
		f, a := newS2Fixture(t)
		f.mint(a.ID, "R2")
		f.startWrite(a.ID)
		f.cas(a.ID, "R2", "Y1")
		assert.Equal(t, "", attrCursor(t, f, a.ID).UntrustedOpenRun)
		f.stopRecord(a.ID, "Y1", "stopped")
		evs := f.events(a.ID)
		last := evs[len(evs)-1]
		assert.Equal(t, "Y1", last.LaunchID, lcFull(evs))
		assert.Equal(t, "R2", last.AdoptedFromLaunchID)
	})
}

// TestLifecycleAttribution_T33UnknownLimit (N5 documenting fixture): under
// UNKNOWN a failed start's revert to the proven P is held/UNATTRIBUTED
// until the next qualifying fresh mint, which restores attribution for
// its own run. T33b: N's start also fails; the revert to P (now
// quarantined) is held through rule 4.
func TestLifecycleAttribution_T33UnknownLimit(t *testing.T) {
	f, a := newS2Fixture(t)
	makeLegacy(t, f, a.ID)
	f.mint(a.ID, "P1")
	f.startWrite(a.ID)
	f.stopRecord(a.ID, "P1", "stopped")
	f.mint(a.ID, "M1")
	f.status(context.Background(), a.ID, store.AgentStatusUpdate{Phase: "starting"})
	f.revert(a.ID, "M1", "P1")
	f.status(context.Background(), a.ID, store.AgentStatusUpdate{Phase: "stopped"})
	f.metaEdit(a.ID, "m-33")
	evs := f.events(a.ID)
	assert.Equal(t, "metadata_changed@4[/1]", tail(evs, 1), "the documented limit")
	f.mint(a.ID, "N1")
	f.startWrite(a.ID)
	evs = f.events(a.ID)
	assert.Equal(t, "started@5[N1/1]", tail(evs, 1), "a fresh mint restores attribution for its run")
	assert.Equal(t, store.QuarantineUnknown, attrCursor(t, f, a.ID).QuarantineMode)
	assert.Equal(t, "created@1[/1] started@2[P1/1] stopped@3(stopped)[P1/1]", lcFull(evs[:3]), "P1's history unchanged")

	// T33b.
	f.stopRecord(a.ID, "N1", "stopped")
	f.mint(a.ID, "N2")
	f.revert(a.ID, "N2", "P1")
	f.metaEdit(a.ID, "m-33b")
	evs = f.events(a.ID)
	assert.Equal(t, store.AttributionHeld, attrCursor(t, f, a.ID).State)
	assert.Empty(t, evs[len(evs)-1].LaunchID)
	checkLaunchMap(t, evs)
}

// TestLifecycleAttribution_T33cUnknownCost (r4 R4-2): under UNKNOWN an
// adoption back to the open run, or a failed restart's revert to it,
// marks that segment; its closure omits.
func TestLifecycleAttribution_T33cUnknownCost(t *testing.T) {
	for _, tt := range []struct {
		name string
		back func(f *lifecycleFixture, id string)
	}{
		{"a adoption back (direct store call)", func(f *lifecycleFixture, id string) { f.heartbeatAdopt(id, "R2", "R1") }},
		{"b failed restart revert", func(f *lifecycleFixture, id string) { f.revert(id, "R2", "R1") }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f, a := newS2Fixture(t)
			makeLegacy(t, f, a.ID)
			f.mint(a.ID, "R1")
			f.startWrite(a.ID)
			f.mint(a.ID, "R2")
			tt.back(f, a.ID)
			f.stopRecord(a.ID, "R1", "stopped")
			evs := f.events(a.ID)
			assert.Equal(t, "created@1[/1] started@2[R1/1] stopped@3(stopped)[/1]", lcFull(evs))
			checkLaunchMap(t, evs)
		})
	}
}

// TestLifecycleAttribution_T34FirstRunRevertToEmpty (r2 N3a): a first-ever
// start reverted to "" under UNKNOWN pushes no empty element.
func TestLifecycleAttribution_T34FirstRunRevertToEmpty(t *testing.T) {
	f, a := newS2Fixture(t)
	makeLegacy(t, f, a.ID)
	f.mint(a.ID, "M1")
	f.revert(a.ID, "M1", "")
	f.metaEdit(a.ID, "m-34")
	att := attrCursor(t, f, a.ID)
	assert.NotContains(t, att.QuarantinedRuns, ",,")
	assert.False(t, strings.HasPrefix(att.QuarantinedRuns, ",") || strings.HasSuffix(att.QuarantinedRuns, ","))
	assert.Zero(t, f.counters.Get(telemetryemit.MetricAttributionStale, telemetryemit.AttributionStaleCorrupt))
}

// TestLifecycleAttribution_T35NonFreshMint (rule 2F): a mint that fails a
// freshness condition is held/UNATTRIBUTED in either mode and from either
// state; a same-value mint is a no-op.
func TestLifecycleAttribution_T35NonFreshMint(t *testing.T) {
	assertHeld := func(t *testing.T, f *lifecycleFixture, id, target string, n0 int) {
		t.Helper()
		att := attrCursor(t, f, id)
		assert.Equal(t, store.AttributionHeld, att.State)
		assert.Equal(t, "", att.AttributedRun, "attribution and undo cleared")
		assert.Equal(t, "", att.ReincarnationMintRun, "witness cleared")
		assert.Contains(t, strings.Split(att.QuarantinedRuns, ","), target)
		f.metaEdit(id, "m-35")
		for _, e := range f.events(id)[n0:] {
			assert.NotEqual(t, target, e.LaunchID, "no new pairing for %s", target)
		}
	}
	t.Run("a complete, target only in previous_run_ids", func(t *testing.T) {
		f, a := stoppedAgent(t)
		f.mint(a.ID, "R2")
		require.Equal(t, store.QuarantineComplete, attrCursor(t, f, a.ID).QuarantineMode)
		n0 := len(f.events(a.ID))
		f.mint(a.ID, "R1")
		assertHeld(t, f, a.ID, "R1", n0)
	})
	t.Run("b UNKNOWN", func(t *testing.T) {
		f, a := stoppedAgent(t)
		makeLegacy(t, f, a.ID)
		f.mint(a.ID, "R2")
		n0 := len(f.events(a.ID))
		f.mint(a.ID, "R1")
		assertHeld(t, f, a.ID, "R1", n0)
	})
	t.Run("c quarantined target", func(t *testing.T) {
		f, a := heldAfterSoftFinish(t)
		f.mint(a.ID, "R2")
		n0 := len(f.events(a.ID))
		f.mint(a.ID, "B1")
		assertHeld(t, f, a.ID, "B1", n0)
	})
	t.Run("d same-value mint is a no-op", func(t *testing.T) {
		f, a := stoppedAgent(t)
		before := attrCursor(t, f, a.ID)
		f.mint(a.ID, "R1")
		assert.Equal(t, before, attrCursor(t, f, a.ID))
	})
	t.Run("e PRE held, non-fresh target", func(t *testing.T) {
		f, a := heldAfterSoftFinish(t)
		n0 := len(f.events(a.ID))
		f.mint(a.ID, "R1")
		assertHeld(t, f, a.ID, "R1", n0)
	})
}

// TestLifecycleAttribution_T35bSettledRunReminted documents the residual
// covered only by the single-caller guard: after a settle cleared
// previous_run_ids, re-minting a settled run of the same generation is
// not detectable and establishes.
func TestLifecycleAttribution_T35bSettledRunReminted(t *testing.T) {
	f, a := newS2Fixture(t)
	f.mint(a.ID, "P1")
	f.startWrite(a.ID)
	f.mint(a.ID, "Q1")
	f.cas(a.ID, "Q1", "Q1") // a same-value settle clears previous_run_ids
	f.mint(a.ID, "P1")
	att := attrCursor(t, f, a.ID)
	assert.Equal(t, store.AttributionClear, att.State)
	assert.Equal(t, "P1", att.LastRun)
	assert.Equal(t, "1", att.LastGeneration)
	checkLaunchMap(t, f.events(a.ID))
}

// TestLifecycleAttribution_EchoShapes (review R2): the effective echo is
// filtered against the FINAL launch_id. (i) No segment, R1's echo: the
// deleted and restored name R1 and keep all six fields. (ii) A
// non-matching echo (R1's, while the deleted names R2): all six omitted.
func TestLifecycleAttribution_EchoShapes(t *testing.T) {
	const r1 = "dev-resolved|sha256:cd|m-echo-1|claude|rev-1|v9"
	const none = "|||||"
	setup := func(t *testing.T) (*lifecycleFixture, *store.Agent) {
		f := newLifecycleFixture(t)
		a := f.create("attr-echo", func(a *store.Agent) { a.Phase, a.Generation, a.RuntimeBrokerID = "created", 1, "broker-1" })
		f.mint(a.ID, "R1")
		f.startWrite(a.ID)
		res, err := f.cs.SetAgentEffectiveConfig(context.Background(), a.ID, "R1", softFinishEcho("R1"))
		require.NoError(t, err)
		require.Empty(t, res.StaleReason)
		f.stopRecord(a.ID, "R1", "stopped")
		return f, a
	}
	t.Run("i matching", func(t *testing.T) {
		f, a := setup(t)
		softFinish(t, f, a.ID, "B1", "R1")
		f.restore(a.ID)
		evs := f.events(a.ID)
		del, rst := lastOf(evs, tc.TransitionDeleted), lastOf(evs, tc.TransitionRestored)
		assert.Equal(t, "R1", del.LaunchID)
		assert.Equal(t, r1, effectiveOf(del))
		assert.Equal(t, r1, effectiveOf(rst))
	})
	t.Run("ii non-matching", func(t *testing.T) {
		f, a := setup(t)
		f.mint(a.ID, "R2")
		softFinish(t, f, a.ID, "R1", "R2")
		f.restore(a.ID)
		evs := f.events(a.ID)
		del, rst := lastOf(evs, tc.TransitionDeleted), lastOf(evs, tc.TransitionRestored)
		assert.Equal(t, "R2", del.LaunchID)
		assert.Equal(t, none, effectiveOf(del))
		assert.Equal(t, none, effectiveOf(rst))
	})
}

// TestLifecycleAttribution_T18cSameStoredTimestamp documents the stamp's
// non-detection (attribution design 7.1): an older binary's update that
// persists exactly the stamp's instant is not detected as stale. The event
// output is then governed only by the run and generation checks of 4.1: an
// unobserved run change still normalises to held/UNATTRIBUTED.
func TestLifecycleAttribution_T18cSameStoredTimestamp(t *testing.T) {
	f, a := stoppedAgent(t)
	c, err := f.cs.client.AgentLifecycleCursor.Get(context.Background(), uuid.MustParse(a.ID))
	require.NoError(t, err)
	_, err = f.cs.client.AgentLifecycleCursor.UpdateOneID(c.ID).SetMetadataDigest(c.MetadataDigest).
		SetUpdatedAt(*c.AttributionStamp).Save(context.Background())
	require.NoError(t, err)
	f.metaEdit(a.ID, "m-18c")
	assert.Zero(t, f.counters.Get(telemetryemit.MetricAttributionStale, telemetryemit.AttributionStaleStamp), "not detected")
	assert.Equal(t, store.AttributionClear, attrCursor(t, f, a.ID).State)

	_, err = f.cs.client.Agent.UpdateOneID(uuid.MustParse(a.ID)).SetRunID("Z9").Save(context.Background())
	require.NoError(t, err)
	f.metaEdit(a.ID, "m-18c-2")
	att := attrCursor(t, f, a.ID)
	assert.Equal(t, store.AttributionHeld, att.State, "the run check still catches the unobserved change")
	assert.Equal(t, "Z9", att.LastRun)
	assert.Empty(t, f.events(a.ID)[len(f.events(a.ID))-1].LaunchID)
}

// TestLifecycleAttribution_T31eUnknownTargetNotListed: under UNKNOWN a
// revert to a run that is NOT in the stored list (here B0, replaced by P1
// before the legacy period) is held by rule 6U, not trusted by rule 6
// (M29).
func TestLifecycleAttribution_T31eUnknownTargetNotListed(t *testing.T) {
	f, a := newS2Fixture(t)
	f.mint(a.ID, "B0")
	f.startWrite(a.ID)
	f.stopRecord(a.ID, "B0", "stopped")
	f.mint(a.ID, "P1")
	makeLegacy(t, f, a.ID)
	f.metaEdit(a.ID, "m-31e") // normalises (pushes P1, not B0)
	f.mint(a.ID, "R2")
	att := attrCursor(t, f, a.ID)
	require.Equal(t, store.AttributionClear, att.State)
	require.NotContains(t, strings.Split(att.QuarantinedRuns, ","), "B0")
	f.revert(a.ID, "R2", "B0")
	assert.Equal(t, store.AttributionHeld, attrCursor(t, f, a.ID).State)
	f.metaEdit(a.ID, "m-31e-2")
	evs := f.events(a.ID)
	assert.Empty(t, evs[len(evs)-1].LaunchID, lcFull(evs))
}

// TestLifecycleAttribution_T34bNoEmptyElementInAList: a held B1, then a
// revert to "" (rule 5): the list keeps only B1, with no empty element and
// no CORRUPT normalisation on the next write (M34).
func TestLifecycleAttribution_T34bNoEmptyElementInAList(t *testing.T) {
	f, a := heldAfterSoftFinish(t)
	f.revert(a.ID, "B1", "")
	f.metaEdit(a.ID, "m-34b")
	assert.Equal(t, "B1", attrCursor(t, f, a.ID).QuarantinedRuns)
	assert.Zero(t, f.counters.Get(telemetryemit.MetricAttributionStale, telemetryemit.AttributionStaleCorrupt))
}

// TestLifecycleAttribution_T23dNormalisationPushOnEstablishingMint (r4
// R4-1(a)): the soft finish and the restore both commit without a bump;
// the next observed write is a fresh mint, which establishes (POST clear,
// no POST push), so only the normalisation push quarantines B1. A later
// revert to B1 is held (rule 4) and nothing names B1 (M38).
func TestLifecycleAttribution_T23dNormalisationPushOnEstablishingMint(t *testing.T) {
	f, a := stoppedAgent(t)
	f.withoutObservation(func() {
		softFinish(t, f, a.ID, "B1", "R1")
		f.restore(a.ID)
	})
	f.mint(a.ID, "R2")
	att := attrCursor(t, f, a.ID)
	require.Equal(t, store.AttributionClear, att.State)
	assert.Contains(t, strings.Split(att.QuarantinedRuns, ","), "B1", "pushed by the normalisation")
	f.revert(a.ID, "R2", "B1")
	assert.Equal(t, store.AttributionHeld, attrCursor(t, f, a.ID).State)
	f.metaEdit(a.ID, "m-23d")
	evs := f.events(a.ID)
	checkLaunchMap(t, evs, "B1")
	assert.Empty(t, evs[len(evs)-1].LaunchID)
}

// corruptPatterns are the 4.1 CORRUPT patterns that apply to every state,
// LEGACY included (implementation review r1 RQ-1, contract review TA-1).
var corruptPatterns = []struct {
	name string
	edit func(u *ent.AgentLifecycleCursorUpdateOne)
}{
	{"partial marker: run only", func(u *ent.AgentLifecycleCursorUpdateOne) { u.SetUntrustedOpenRun("R1") }},
	{"marker revision 0", func(u *ent.AgentLifecycleCursorUpdateOne) {
		u.SetUntrustedOpenRun("R1").SetUntrustedOpenRunGeneration("1").SetUntrustedOpenRunRev(0)
	}},
	{"non-decimal marker generation", func(u *ent.AgentLifecycleCursorUpdateOne) {
		u.SetUntrustedOpenRun("R1").SetUntrustedOpenRunGeneration("x").SetUntrustedOpenRunRev(2)
	}},
	{"invalid quarantine mode", func(u *ent.AgentLifecycleCursorUpdateOne) { u.SetQuarantineMode("weird") }},
	{"unparsable quarantine list", func(u *ent.AgentLifecycleCursorUpdateOne) { u.SetQuarantinedRuns("A,,B") }},
	{"non-decimal last generation", func(u *ent.AgentLifecycleCursorUpdateOne) { u.SetLastGeneration("x") }},
	{"non-decimal attributed generation", func(u *ent.AgentLifecycleCursorUpdateOne) { u.SetAttributedGeneration("x") }},
	{"witness without a base run", func(u *ent.AgentLifecycleCursorUpdateOne) { u.SetReincarnationMintRun("R1") }},
}

// TestLifecycleAttribution_T13CorruptOpenSegment (RQ-1, OP-2): every
// CORRUPT pattern, on a LEGACY cursor (attribution_state "", NULL stamp)
// and on a coherent clear cursor, with a VALID open segment R1/1: the
// cursor is CORRUPT (counter reason corrupt), its valid segment is
// untrusted, and the closure (a hard delete) omits launch_id while keeping
// incarnation_id and segment_metadata_revision. The valid-LEGACY and
// exact-marker controls are in TestLifecycleAttribution_T11mLegacyPartialMarker.
func TestLifecycleAttribution_T13CorruptOpenSegment(t *testing.T) {
	for _, legacy := range []bool{true, false} {
		for _, p := range corruptPatterns {
			name := "clear/" + p.name
			if legacy {
				name = "LEGACY/" + p.name
			}
			t.Run(name, func(t *testing.T) {
				f, a := newS2Fixture(t)
				f.mint(a.ID, "R1")
				f.startWrite(a.ID)
				if legacy {
					makeLegacy(t, f, a.ID)
				}
				editCursor(t, f, a.ID, legacy, p.edit)
				f.hardDelete(a.ID)
				evs := f.events(a.ID)
				del := evs[len(evs)-1]
				assert.Equal(t, "deleted@3(hard)[/1]", lcFull(evs[len(evs)-1:]))
				require.NotNil(t, del.SegmentMetadataRevision, "the closure keeps segment_metadata_revision")
				assert.Equal(t, int64(1), f.counters.Get(telemetryemit.MetricAttributionStale, telemetryemit.AttributionStaleCorrupt))
				assert.Zero(t, f.counters.Get(telemetryemit.MetricAttributionStale, telemetryemit.AttributionStaleLegacy))
			})
		}
	}
}
