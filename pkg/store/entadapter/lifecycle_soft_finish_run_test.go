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
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	tc "github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

// The delete engine's soft finish after a run-mismatch delete (upstream
// ptone/scion#4058) writes the run the broker held (B) as run_id and the
// recorded run (R) among the previous runs, in the same soft
// FinalizeAgentDeletion write that sets deleted_at. WP1 observes that
// finalize (one transaction, before and after rows read under lock). These
// tests pin the lifecycle outcome of that write (WP1 upmerge-3):
// exactly one deleted(soft); a closure of an open segment names the
// segment's own run R (taken from the cursor); with no open segment the
// event names the row's committed run B; no run-change, configured or
// resynced event; a rolled-back or unmatched write emits nothing.

// softFinishRun soft-finalizes a's deletion recording broker run b with
// the previous runs prev, as the delete engine's soft finish does.
func softFinishRun(t *testing.T, f *lifecycleFixture, id, b string, prev []string) (int, error) {
	t.Helper()
	now := time.Now()
	stopped := "stopped"
	none := store.DeletionStateNone
	set := store.DeletionFields{
		State: &none, Phase: &stopped, DeletedAt: &now,
		SoftFinishRunID: &b, SoftFinishPreviousRunIDs: &prev,
	}
	return f.cs.FinalizeAgentDeletion(context.Background(), id, store.DeletionPredicate{}, store.DeletionFinalizeSoft, set, nil)
}

func softFinishState(t *testing.T, f *lifecycleFixture, id string) (rows int, rev int64) {
	t.Helper()
	_, out := lifecycleEvents(t, f.cs, id)
	c, err := f.cs.client.AgentLifecycleCursor.Get(context.Background(), uuid.MustParse(id))
	require.NoError(t, err)
	return len(out), c.TransitionRevision
}

// TestLifecycleSoftFinishRun_OpenSegmentClosesWithItsOwnRun: the agent ran
// on R (segment open); the run-mismatch soft finish records B. One
// deleted(soft) closes the segment and names R, the segment's run, not the
// committed B. OLD (before #4058): the same, since run_id stayed R.
func TestLifecycleSoftFinishRun_OpenSegmentClosesWithItsOwnRun(t *testing.T) {
	f, a := newS2Fixture(t)
	f.mint(a.ID, "R1")
	f.startWrite(a.ID)
	n, err := softFinishRun(t, f, a.ID, "B1", []string{"R1"})
	require.NoError(t, err)
	require.Equal(t, 1, n)
	row, err := f.cs.client.Agent.Get(context.Background(), uuid.MustParse(a.ID))
	require.NoError(t, err)
	require.Equal(t, "B1", row.RunID, "fixture: the soft finish committed the broker's run")
	require.NotNil(t, row.DeletedAt, "fixture: soft-deleted")
	evs := f.events(a.ID)
	require.Equal(t, "created@1[/1] started@2[R1/1] deleted@3(soft)[R1/1]", lcFull(evs))
	require.NotNil(t, evs[2].SegmentMetadataRevision, "deleted closes the open segment")
	assert.Zero(t, countTransitions(evs, tc.TransitionResynced))
	assert.Zero(t, countTransitions(evs, tc.TransitionConfigured))
}

// TestLifecycleSoftFinishRun_NoOpenSegmentNamesBeforeRun (attribution
// design v2.8, T1 deletion step): the agent had stopped (segment closed);
// the soft finish records B. B is unproven for this generation, so the one
// deleted(soft) names the BEFORE run R1 with its generation, exactly as
// before #4058 (no caller-visible change against cec382dd).
func TestLifecycleSoftFinishRun_NoOpenSegmentNamesBeforeRun(t *testing.T) {
	f, a := newS2Fixture(t)
	f.mint(a.ID, "R1")
	f.startWrite(a.ID)
	f.stopRecord(a.ID, "R1", "stopped")
	n, err := softFinishRun(t, f, a.ID, "B1", []string{"R1"})
	require.NoError(t, err)
	require.Equal(t, 1, n)
	evs := f.events(a.ID)
	require.Equal(t, "created@1[/1] started@2[R1/1] stopped@3(stopped)[R1/1] deleted@4(soft)[R1/1]", lcFull(evs))
	assert.Nil(t, evs[3].SegmentMetadataRevision, "no segment to close")
	assert.Zero(t, countTransitions(evs, tc.TransitionResynced))
	assert.Zero(t, countTransitions(evs, tc.TransitionConfigured))
}

// TestLifecycleSoftFinishRun_SameRun: a soft finish that records the
// row's own run (only the previous runs change) is the plain soft delete:
// deleted(soft) naming R.
func TestLifecycleSoftFinishRun_SameRun(t *testing.T) {
	f, a := newS2Fixture(t)
	f.mint(a.ID, "R1")
	f.startWrite(a.ID)
	f.stopRecord(a.ID, "R1", "stopped")
	_, err := softFinishRun(t, f, a.ID, "R1", []string{})
	require.NoError(t, err)
	require.Equal(t, "created@1[/1] started@2[R1/1] stopped@3(stopped)[R1/1] deleted@4(soft)[R1/1]", lcFull(f.events(a.ID)))
}

// TestLifecycleSoftFinishRun_RollbackSilent: an observer failure rolls the
// soft finish back with its run write: run_id, deleted_at and the cursor
// unchanged, nothing emitted.
func TestLifecycleSoftFinishRun_RollbackSilent(t *testing.T) {
	f, a := newS2Fixture(t)
	f.mint(a.ID, "R1")
	f.startWrite(a.ID)
	rows, rev := softFinishState(t, f, a.ID)
	errObs := errors.New("injected observer failure")
	f.cs.SetAgentWriteObserver(failingObserver{err: errObs})
	_, err := softFinishRun(t, f, a.ID, "B1", []string{"R1"})
	f.cs.SetAgentWriteObserver(f.obs)
	require.ErrorIs(t, err, errObs)
	got, err := f.cs.GetAgent(context.Background(), a.ID)
	require.NoError(t, err)
	assert.Equal(t, "R1", got.RunID)
	assert.True(t, got.DeletedAt.IsZero(), "not deleted")
	r2, rev2 := softFinishState(t, f, a.ID)
	assert.Equal(t, rows, r2)
	assert.Equal(t, rev, rev2)
}

// TestLifecycleSoftFinishRun_UnmatchedSilent: a predicate that no longer
// matches writes nothing and emits nothing.
func TestLifecycleSoftFinishRun_UnmatchedSilent(t *testing.T) {
	f, a := newS2Fixture(t)
	f.mint(a.ID, "R1")
	f.startWrite(a.ID)
	rows, rev := softFinishState(t, f, a.ID)
	now := time.Now()
	b, prev := "B1", []string{"R1"}
	n, err := f.cs.FinalizeAgentDeletion(context.Background(), a.ID,
		store.DeletionPredicate{States: []string{store.DeletionStateFinalizing}}, store.DeletionFinalizeSoft,
		store.DeletionFields{DeletedAt: &now, SoftFinishRunID: &b, SoftFinishPreviousRunIDs: &prev}, nil)
	require.NoError(t, err)
	require.Zero(t, n, "fixture: the predicate does not match")
	r2, rev2 := softFinishState(t, f, a.ID)
	assert.Equal(t, rows, r2)
	assert.Equal(t, rev, rev2)
}
