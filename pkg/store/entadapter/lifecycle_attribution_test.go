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

// Persistent run attribution through the real store writers on SQLite
// (WP1 attribution design v2.8, section 10.1). Each fixture asserts the
// full event sequence with [launch_id/incarnation], the cursor's
// attribution after the decisive steps, and the launch-to-generation map
// invariants (10.1): no launch maps to two generations, no banned
// (unproven) run is named, no extra terminal for a closed segment.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	tc "github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

// attrCursor reads a's cursor attribution.
func attrCursor(t *testing.T, f *lifecycleFixture, id string) store.LifecycleAttribution {
	t.Helper()
	c, err := f.cs.client.AgentLifecycleCursor.Get(context.Background(), uuid.MustParse(id))
	require.NoError(t, err)
	return entCursorToStore(c).Attribution
}

// checkLaunchMap asserts the 10.1 invariants over all events: one
// generation per launch, no banned run named (launch_id or
// adopted_from_launch_id), and at most one terminal per started launch.
func checkLaunchMap(t *testing.T, evs []*tc.LifecycleEvent, banned ...string) {
	t.Helper()
	gens := map[string]map[string]bool{}
	started, terminals := map[string]int{}, map[string]int{}
	for _, e := range evs {
		for _, b := range banned {
			assert.NotEqual(t, b, e.LaunchID, "%s@%d names banned run %s", e.Transition, e.TransitionRevision, b)
			assert.NotEqual(t, b, e.AdoptedFromLaunchID, "%s@%d carries banned run %s", e.Transition, e.TransitionRevision, b)
		}
		if e.LaunchID == "" {
			continue
		}
		if gens[e.LaunchID] == nil {
			gens[e.LaunchID] = map[string]bool{}
		}
		gens[e.LaunchID][e.IncarnationID] = true
		switch {
		case e.Transition == tc.TransitionStarted:
			started[e.LaunchID]++
		case e.Transition == tc.TransitionStopped || e.Transition == tc.TransitionHalted ||
			(e.Transition == tc.TransitionDeleted && e.SegmentMetadataRevision != nil):
			terminals[e.LaunchID]++
		}
	}
	for l, g := range gens {
		assert.Len(t, g, 1, "launch %s maps to generations %v", l, g)
	}
	for l, n := range terminals {
		assert.LessOrEqual(t, n, started[l], "launch %s has %d terminals for %d starts", l, n, started[l])
	}
}

// restore clears deleted_at as restoreAgentTx does (UpdateAgent; run_id is
// not written).
func (f *lifecycleFixture) restore(id string) {
	f.t.Helper()
	f.update(context.Background(), id, func(a *store.Agent) {
		a.DeletedAt = time.Time{}
		a.SoftDeleteOpID = ""
	})
}

// metaEdit is a metadata-only edit of the row (metadata_changed).
func (f *lifecycleFixture) metaEdit(id, model string) {
	f.t.Helper()
	f.update(context.Background(), id, func(a *store.Agent) {
		a.AppliedConfig = &store.AgentAppliedConfig{Model: model}
	})
}

func (f *lifecycleFixture) revert(id, minted, previous string) {
	f.t.Helper()
	ok, err := f.cs.RevertAgentRunID(context.Background(), id, minted, previous)
	require.NoError(f.t, err)
	require.True(f.t, ok, "revert %s -> %s", minted, previous)
}

func (f *lifecycleFixture) cas(id, expected, next string) {
	f.t.Helper()
	ok, err := f.cs.CompareAndSwapAgentRunID(context.Background(), id, expected, next)
	require.NoError(f.t, err)
	require.True(f.t, ok, "cas %s -> %s", expected, next)
}

// heartbeatAdopt is a DIRECT store-level AdoptAgentRunID with the
// heartbeat adoption source (labelled: the hub's heartbeat path gates it
// further, e.g. refusing it while a reincarnation is in flight).
func (f *lifecycleFixture) heartbeatAdopt(id, expected, next string) {
	f.t.Helper()
	ctx := store.WithRunAdoptionSource(context.Background(), store.AdoptionSourceHeartbeat)
	ok, err := f.cs.AdoptAgentRunID(ctx, id, expected, next, time.Now().Add(time.Minute))
	require.NoError(f.t, err)
	require.True(f.t, ok, "heartbeat adopt %s -> %s", expected, next)
}

func (f *lifecycleFixture) bumpGeneration(id string) {
	f.t.Helper()
	f.update(context.Background(), id, func(a *store.Agent) { a.Generation++ })
}

func (f *lifecycleFixture) hardDelete(id string) {
	f.t.Helper()
	require.NoError(f.t, f.cs.DeleteAgent(context.Background(), id))
}

func softFinish(t *testing.T, f *lifecycleFixture, id, b string, prev ...string) {
	t.Helper()
	if prev == nil {
		prev = []string{}
	}
	n, err := softFinishRun(t, f, id, b, prev)
	require.NoError(t, err)
	require.Equal(t, 1, n)
}

// TestLifecycleAttribution_T1Plain: the plain soft finish to an unrecorded
// B1 after a stop, restore, a metadata edit, then a fresh mint and start.
// The deleted, restored and metadata_changed name R1/1 exactly as
// cec382dd did; B1 never appears; the R2 mint clears the state.
func TestLifecycleAttribution_T1Plain(t *testing.T) {
	f, a := newS2Fixture(t)
	f.mint(a.ID, "R1")
	f.startWrite(a.ID)
	f.stopRecord(a.ID, "R1", "stopped")
	softFinish(t, f, a.ID, "B1", "R1")
	att := attrCursor(t, f, a.ID)
	assert.Equal(t, store.AttributionHeld, att.State)
	assert.Equal(t, "B1", att.LastRun)
	assert.Equal(t, "R1", att.AttributedRun)
	assert.Equal(t, "1", att.AttributedGeneration)
	assert.Equal(t, "B1", att.QuarantinedRuns)
	assert.Equal(t, store.QuarantineComplete, att.QuarantineMode)
	f.restore(a.ID)
	f.metaEdit(a.ID, "m-edit")
	f.mint(a.ID, "R2")
	att = attrCursor(t, f, a.ID)
	assert.Equal(t, store.AttributionClear, att.State)
	assert.Equal(t, "R2", att.LastRun)
	assert.Equal(t, "R1", att.AttributedRun, "the undo identity of the newest quarantined run")
	f.startWrite(a.ID)
	evs := f.events(a.ID)
	assert.Equal(t, "created@1[/1] started@2[R1/1] stopped@3(stopped)[R1/1] deleted@4(soft)[R1/1] "+
		"restored@5[R1/1] metadata_changed@6[R1/1] started@7[R2/1]", lcFull(evs))
	checkLaunchMap(t, evs, "B1")
}

// TestLifecycleAttribution_T2GenerationNMinus1: B is the run R1 of the
// previous generation, replaced by a reincarnation mint R2 that never
// settled. deleted and restored name R2/2; R1 maps only to generation 1.
func TestLifecycleAttribution_T2GenerationNMinus1(t *testing.T) {
	f, a := newS2Fixture(t)
	ctx := context.Background()
	f.mint(a.ID, "R1")
	f.startWrite(a.ID)
	cur, err := f.cs.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	_, err = f.cs.ClaimAgentReincarnation(ctx, a.ID, cur.StateVersion, time.Now())
	require.NoError(t, err)
	f.update(ctx, a.ID, func(a *store.Agent) { a.Phase = "stopping"; a.ReincarnationState = store.ReincarnationStatePending })
	f.mint(a.ID, "R2")
	f.update(ctx, a.ID, func(a *store.Agent) { a.Generation = 2 })
	f.update(ctx, a.ID, func(a *store.Agent) { a.Phase = "stopped"; a.ReincarnationState = store.ReincarnationStateNone })
	softFinish(t, f, a.ID, "R1", "R2")
	f.restore(a.ID)
	evs := f.events(a.ID)
	del, rst := lastOf(evs, tc.TransitionDeleted), lastOf(evs, tc.TransitionRestored)
	require.NotNil(t, del)
	require.NotNil(t, rst)
	assert.Equal(t, "R2/2", del.LaunchID+"/"+del.IncarnationID, lcFull(evs))
	assert.Equal(t, "R2/2", rst.LaunchID+"/"+rst.IncarnationID, lcFull(evs))
	checkLaunchMap(t, evs)
	for _, e := range evs[3:] {
		assert.NotEqual(t, "R1", e.LaunchID, "R1 is never named after the reincarnation: %s", lcFull(evs))
	}
}

// TestLifecycleAttribution_T2bSameGenerationOlderRun: B is an older run of
// the same generation (a replaced, unsettled mint).
func TestLifecycleAttribution_T2bSameGenerationOlderRun(t *testing.T) {
	f, a := newS2Fixture(t)
	f.mint(a.ID, "R1")
	f.startWrite(a.ID)
	f.stopRecord(a.ID, "R1", "stopped")
	f.mint(a.ID, "R2")
	softFinish(t, f, a.ID, "R1", "R2")
	f.restore(a.ID)
	evs := f.events(a.ID)
	assert.Equal(t, "created@1[/1] started@2[R1/1] stopped@3(stopped)[R1/1] deleted@4(soft)[R2/1] restored@5[R2/1]", lcFull(evs))
	checkLaunchMap(t, evs)
}

// TestLifecycleAttribution_T3Force: a forced soft finish to an unrecorded
// X, restore, a heartbeat reporting X running, a stop, then a fresh mint.
// The started and stopped of X omit launch_id (D1/D4); the segment opened
// on X is untrusted and its closure omits; X never appears.
func TestLifecycleAttribution_T3Force(t *testing.T) {
	f, a := newS2Fixture(t)
	f.mint(a.ID, "R1")
	f.startWrite(a.ID)
	f.stopRecord(a.ID, "R1", "stopped")
	softFinish(t, f, a.ID, "X1")
	f.restore(a.ID)
	f.heartbeat(a.ID, "running", "X1")
	att := attrCursor(t, f, a.ID)
	assert.Equal(t, "X1", att.UntrustedOpenRun, "the segment opened while held is marked")
	f.stopRecord(a.ID, "X1", "stopped")
	assert.Equal(t, "", attrCursor(t, f, a.ID).UntrustedOpenRun, "the closure clears the segment and its marker together")
	f.mint(a.ID, "R2")
	f.startWrite(a.ID)
	evs := f.events(a.ID)
	assert.Equal(t, "created@1[/1] started@2[R1/1] stopped@3(stopped)[R1/1] deleted@4(soft)[R1/1] "+
		"restored@5[R1/1] started@6[/1] stopped@7(stopped)[/1] started@8[R2/1]", lcFull(evs))
	checkLaunchMap(t, evs, "X1")
}

// TestLifecycleAttribution_T4EmptyBefore: a row with no run soft-finished
// to B: explicit UNATTRIBUTED, deleted and restored omit.
func TestLifecycleAttribution_T4EmptyBefore(t *testing.T) {
	f, a := newS2Fixture(t)
	softFinish(t, f, a.ID, "B1")
	f.restore(a.ID)
	evs := f.events(a.ID)
	assert.Equal(t, "created@1[/1] deleted@2(soft)[/1] restored@3[/1]", lcFull(evs))
	att := attrCursor(t, f, a.ID)
	assert.Equal(t, store.AttributionHeld, att.State)
	assert.Equal(t, "", att.AttributedRun)
	checkLaunchMap(t, evs, "B1")
}

// TestLifecycleAttribution_T5TrustedSegment: R1 running; the soft finish
// closes R1's segment with its frozen identity (closure precedence) and
// keeps R1's echo; restored names R1/1.
func TestLifecycleAttribution_T5TrustedSegment(t *testing.T) {
	f := newLifecycleFixture(t)
	ctx := context.Background()
	a := f.create("attr-t5", func(a *store.Agent) { a.Phase, a.Generation, a.RuntimeBrokerID = "created", 1, "broker-1" })
	f.mint(a.ID, "R1")
	f.startWrite(a.ID)
	res, err := f.cs.SetAgentEffectiveConfig(ctx, a.ID, "R1", softFinishEcho("R1"))
	require.NoError(t, err)
	require.Empty(t, res.StaleReason)
	softFinish(t, f, a.ID, "B1", "R1")
	f.restore(a.ID)
	evs := f.events(a.ID)
	assert.Equal(t, "created@1[/1] started@2[R1/1] configured@3[R1/1] deleted@4(soft)[R1/1] restored@5[R1/1]", lcFull(evs))
	del := lastOf(evs, tc.TransitionDeleted)
	require.NotNil(t, del.SegmentMetadataRevision)
	assert.Equal(t, "dev-resolved|sha256:cd|m-echo-1|claude|rev-1|v9", effectiveOf(del), "R1's echo kept on the R1 closure")
	checkLaunchMap(t, evs, "B1")
}

// TestLifecycleAttribution_T5bSegmentGenerationMismatch: the open segment's
// frozen generation differs from the row's at the soft finish: the closure
// keeps the segment identity, the stored attribution is UNATTRIBUTED and
// restored omits.
func TestLifecycleAttribution_T5bSegmentGenerationMismatch(t *testing.T) {
	f, a := newS2Fixture(t)
	f.mint(a.ID, "R1")
	f.startWrite(a.ID)
	// An observed bump with the same run while the segment is open
	// (not a witnessed reincarnation) is itself held (rule 7); seed the
	// mismatch directly instead: the cursor's segment generation is 0.
	_, err := f.cs.client.AgentLifecycleCursor.UpdateOneID(uuid.MustParse(a.ID)).SetOpenRunGeneration("0").Save(context.Background())
	require.NoError(t, err)
	// Keep the stamp coherent with the raw edit (the edit bumped
	// updated_at): copy updated_at into the stamp.
	coherentStamp(t, f, a.ID)
	softFinish(t, f, a.ID, "B1", "R1")
	f.restore(a.ID)
	evs := f.events(a.ID)
	assert.Equal(t, "created@1[/1] started@2[R1/1] deleted@3(soft)[R1/0] restored@4[/1]", lcFull(evs))
	assert.Equal(t, "", attrCursor(t, f, a.ID).AttributedRun)
}

// TestLifecycleAttribution_T5cSoftFinishClosesSegmentOnB (r5 R5-1 path i,
// rule 3 OMIT ruling): B runs with a trusted segment; a restart mints R
// (the segment stays on B); the mismatch delete soft-finishes to B. The
// deleted closure omits launch_id (B is quarantined by that write) and
// keeps its incarnation_id and segment_metadata_revision; B's earlier
// started stands; the restore names R/1 (a segment on another run).
func TestLifecycleAttribution_T5cSoftFinishClosesSegmentOnB(t *testing.T) {
	f := newLifecycleFixture(t)
	ctx := context.Background()
	a := f.create("attr-t5c", func(a *store.Agent) { a.Phase, a.Generation, a.RuntimeBrokerID = "created", 1, "broker-1" })
	f.mint(a.ID, "B1")
	f.startWrite(a.ID)
	res, err := f.cs.SetAgentEffectiveConfig(ctx, a.ID, "B1", softFinishEcho("B1"))
	require.NoError(t, err)
	require.Empty(t, res.StaleReason)
	f.mint(a.ID, "R1")
	softFinish(t, f, a.ID, "B1", "R1")
	f.restore(a.ID)
	evs := f.events(a.ID)
	assert.Equal(t, "created@1[/1] started@2[B1/1] configured@3[B1/1] deleted@4(soft)[/1] restored@5[R1/1]", lcFull(evs))
	del := lastOf(evs, tc.TransitionDeleted)
	require.NotNil(t, del.SegmentMetadataRevision, "the closure keeps segment_metadata_revision")
	assert.Equal(t, "|||||", effectiveOf(del), "no echo without launch_id")
	assert.Empty(t, del.AdoptedFromLaunchID)
	att := attrCursor(t, f, a.ID)
	assert.Equal(t, "R1", att.AttributedRun)
	assert.Contains(t, strings.Split(att.QuarantinedRuns, ","), "B1")
}

// TestLifecycleAttribution_T6Composition: a second run-mismatch soft finish
// keeps the first attribution (R1/1); never B1 or B2.
func TestLifecycleAttribution_T6Composition(t *testing.T) {
	f, a := newS2Fixture(t)
	f.mint(a.ID, "R1")
	f.startWrite(a.ID)
	f.stopRecord(a.ID, "R1", "stopped")
	softFinish(t, f, a.ID, "B1", "R1")
	f.restore(a.ID)
	softFinish(t, f, a.ID, "B2", "B1")
	f.restore(a.ID)
	evs := f.events(a.ID)
	assert.Equal(t, "created@1[/1] started@2[R1/1] stopped@3(stopped)[R1/1] deleted@4(soft)[R1/1] restored@5[R1/1] "+
		"deleted@6(soft)[R1/1] restored@7[R1/1]", lcFull(evs))
	att := attrCursor(t, f, a.ID)
	assert.Equal(t, "R1", att.AttributedRun)
	assert.Equal(t, "B2,B1", att.QuarantinedRuns)
	checkLaunchMap(t, evs, "B1", "B2")
}

// TestLifecycleAttribution_T18StampCoherent (second clause): the new
// binary's own bumps keep attribution_stamp equal to updated_at, on the
// create branch and the update branch.
func TestLifecycleAttribution_T18StampCoherent(t *testing.T) {
	f, a := newS2Fixture(t)
	check := func(step string) {
		c, err := f.cs.client.AgentLifecycleCursor.Get(context.Background(), uuid.MustParse(a.ID))
		require.NoError(t, err)
		require.NotNil(t, c.AttributionStamp, step)
		assert.True(t, c.AttributionStamp.Equal(c.UpdatedAt), "%s: stamp %v updated_at %v", step, c.AttributionStamp, c.UpdatedAt)
	}
	check("create branch")
	f.mint(a.ID, "R1")
	f.startWrite(a.ID)
	check("update branch")
}

// TestLifecycleAttribution_T18StaleStampFailsClosed (first clause): an
// older binary's update (updated_at moves, the attribution columns do
// not) makes the next write fail closed until a fresh mint.
func TestLifecycleAttribution_T18StaleStampFailsClosed(t *testing.T) {
	f, a := newS2Fixture(t)
	f.mint(a.ID, "R1")
	f.startWrite(a.ID)
	olderBinaryUpdate(t, f, a.ID)
	f.stopRecord(a.ID, "R1", "stopped")
	f.metaEdit(a.ID, "m-after-stale")
	evs := f.events(a.ID)
	// The closure of R1's valid named segment keeps its frozen pair (R4
	// closure continuity; the history-unknown normalisation push does not
	// fire the trigger); every later event omits until a fresh mint.
	assert.Equal(t, "created@1[/1] started@2[R1/1] stopped@3(stopped)[R1/1] metadata_changed@4[/1]", lcFull(evs))
	assert.Equal(t, int64(1), f.counters.Get("lifecycle_attribution_stale", "stale_stamp"))
	assert.Equal(t, store.QuarantineUnknown, attrCursor(t, f, a.ID).QuarantineMode)
}

// olderBinaryUpdate simulates a cursor update by a binary that does not
// maintain the attribution: only the pre-change columns are written, so
// updated_at moves (UpdateDefault) and the stamp does not.
func olderBinaryUpdate(t *testing.T, f *lifecycleFixture, id string) {
	t.Helper()
	c, err := f.cs.client.AgentLifecycleCursor.Get(context.Background(), uuid.MustParse(id))
	require.NoError(t, err)
	time.Sleep(2 * time.Millisecond)
	_, err = f.cs.client.AgentLifecycleCursor.UpdateOneID(c.ID).SetMetadataDigest(c.MetadataDigest).
		SetUpdatedAt(time.Now().UTC()).Save(context.Background())
	require.NoError(t, err)
}

// coherentStamp copies updated_at into attribution_stamp after a raw
// fixture edit (so the edit is not read as an older binary's).
func coherentStamp(t *testing.T, f *lifecycleFixture, id string) {
	t.Helper()
	c, err := f.cs.client.AgentLifecycleCursor.Get(context.Background(), uuid.MustParse(id))
	require.NoError(t, err)
	now := time.Now().UTC()
	_, err = f.cs.client.AgentLifecycleCursor.UpdateOneID(c.ID).SetUpdatedAt(now).SetAttributionStamp(now).Save(context.Background())
	require.NoError(t, err)
}

func lastOf(evs []*tc.LifecycleEvent, transition string) *tc.LifecycleEvent {
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Transition == transition {
			return evs[i]
		}
	}
	return nil
}

// softFinishEcho is a broker effective-configuration echo for run, from
// broker-1.
func softFinishEcho(run string) store.AgentEffectiveConfig {
	return store.AgentEffectiveConfig{
		RunID: run, Template: "dev-resolved", TemplateHash: "sha256:cd", Harness: "claude",
		Model: "m-echo-1", HarnessConfigRevision: "rev-1", BrokerID: "broker-1", BrokerBuild: "v9",
		Source: store.EffectiveConfigSourceSync,
	}
}

// effectiveOf renders the six effective_* fields of e's metadata.
func effectiveOf(e *tc.LifecycleEvent) string {
	if e.Metadata == nil {
		return "nometa"
	}
	m := e.Metadata
	return m.EffectiveTemplate + "|" + m.EffectiveTemplateHash + "|" + m.EffectiveModel + "|" +
		m.EffectiveHarness + "|" + m.HarnessConfigRevision + "|" + m.EffectiveBrokerBuild
}
