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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

func TestTrajectorySegmentStore_PrepareCommitAndIdentityUniqueness(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)

	now := time.Now().UTC().Truncate(time.Microsecond)
	rawHashA := strings.Repeat("a", 64)
	rawHashB := strings.Repeat("b", 64)
	compHashA := strings.Repeat("c", 64)

	base := &store.TrajectorySegment{
		HubID:              "hub-1",
		OriginHubID:        "hub-1",
		ProjectID:          "proj-1",
		AgentID:            "agent-1",
		RunID:              "run-1",
		Harness:            "claude",
		NativeRelativePath: ".claude/projects/-workspace/sess-1.jsonl",
		ArtifactKind:       telemetrycontract.TrajectoryArtifactKindJSONLAppend,
		Generation:         0,
		ByteStart:          0,
		ByteEnd:            1024,
		RawSHA256:          rawHashA,
		CompressedSize:     256,
		CompressedSHA256:   compHashA,
		IncompleteTail:     false,
		SessionID:          "sess-1",
		ObjectPath:         "hub-1/trajectories/v1/raw/project=proj-1/agent=agent-1/run=run-1/claude/.claude/projects/-workspace/sess-1.jsonl/g0-0000000000000000.seg.gz",
		CreatedAt:          now,
		PrepareExpiresAt:   now.Add(15 * time.Minute),
	}

	// Initial prepare inserts a pending row.
	prepared, err := cs.PrepareTrajectorySegment(ctx, base)
	require.NoError(t, err)
	require.NotEmpty(t, prepared.ID)
	assert.Equal(t, telemetrycontract.TrajectorySegmentStatusPending, prepared.Status)
	assert.Equal(t, telemetrycontract.TrajectoryNormalizeStatusPending, prepared.NormalizeStatus)

	// Re-prepare of the same pending identity refreshes TTL and keeps the same ID.
	laterExp := now.Add(30 * time.Minute)
	retry := *base
	retry.ID = ""
	retry.PrepareExpiresAt = laterExp
	reprepared, err := cs.PrepareTrajectorySegment(ctx, &retry)
	require.NoError(t, err)
	assert.Equal(t, prepared.ID, reprepared.ID)
	assert.Equal(t, telemetrycontract.TrajectorySegmentStatusPending, reprepared.Status)
	assert.Equal(t, laterExp, reprepared.PrepareExpiresAt)

	// Per-run raw byte sum includes pending segment (regardless of expiry),
	// excluding self when excludeID is passed so re-prepares/probes reuse the
	// row without double-counting.
	sumAll, err := cs.SumTrajectoryRunRawBytes(ctx, "agent-1", "run-1", "")
	require.NoError(t, err)
	assert.Equal(t, int64(1024), sumAll)

	sumExcludingSelf, err := cs.SumTrajectoryRunRawBytes(ctx, "agent-1", "run-1", prepared.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(0), sumExcludingSelf)

	// Expired pending segment STILL counts toward run raw bytes (charging
	// MAX(raw, compressed) so compressed_size > raw_size is charged in full)
	// and toward the per-run segment row count.
	expiredPending := *base
	expiredPending.ID = ""
	expiredPending.ByteStart = 1024
	expiredPending.ByteEnd = 1025 // 1 raw byte
	expiredPending.CompressedSize = 4096
	expiredPending.ObjectPath = "hub-1/trajectories/v1/raw/project=proj-1/agent=agent-1/run=run-1/claude/.claude/projects/-workspace/sess-1.jsonl/g0-0000000000001024.seg.gz"
	expiredPending.PrepareExpiresAt = now.Add(-time.Hour)
	expiredRow, err := cs.PrepareTrajectorySegment(ctx, &expiredPending)
	require.NoError(t, err)

	sumWithExpired, err := cs.SumTrajectoryRunRawBytes(ctx, "agent-1", "run-1", "")
	require.NoError(t, err)
	assert.Equal(t, int64(1024+4096), sumWithExpired)

	countWithExpired, err := cs.CountTrajectoryRunSegments(ctx, "agent-1", "run-1", "")
	require.NoError(t, err)
	assert.Equal(t, int64(2), countWithExpired)

	// Excluding the expired row itself during a re-prepare of that identity
	// does not double-count its bytes or row count.
	sumExcludingExpired, err := cs.SumTrajectoryRunRawBytes(ctx, "agent-1", "run-1", expiredRow.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(1024), sumExcludingExpired)

	countExcludingExpired, err := cs.CountTrajectoryRunSegments(ctx, "agent-1", "run-1", expiredRow.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), countExcludingExpired)

	// Commit transitions pending -> committed with manifest_path and committed_at.
	manifestPath := base.ObjectPath + ".manifest.json"
	commitTime := now.Add(2 * time.Minute)
	expectA := store.TrajectorySegmentCommitExpectation{
		ByteEnd:          base.ByteEnd,
		RawSHA256:        base.RawSHA256,
		CompressedSize:   base.CompressedSize,
		CompressedSHA256: base.CompressedSHA256,
		IncompleteTail:   base.IncompleteTail,
		SessionID:        base.SessionID,
	}
	committed, err := cs.CommitTrajectorySegment(ctx, prepared.ID, manifestPath, commitTime, expectA)
	require.NoError(t, err)
	assert.Equal(t, telemetrycontract.TrajectorySegmentStatusCommitted, committed.Status)
	assert.Equal(t, manifestPath, committed.ManifestPath)
	require.NotNil(t, committed.CommittedAt)
	assert.Equal(t, commitTime, *committed.CommittedAt)

	// Repeating commit with matching expectation is idempotent.
	committedAgain, err := cs.CommitTrajectorySegment(ctx, prepared.ID, manifestPath, commitTime.Add(time.Minute), expectA)
	require.NoError(t, err)
	assert.Equal(t, telemetrycontract.TrajectorySegmentStatusCommitted, committedAgain.Status)
	assert.Equal(t, prepared.ID, committedAgain.ID)

	// Repeating commit with different expectation returns ErrTrajectorySegmentConflict.
	expectWrong := expectA
	expectWrong.RawSHA256 = rawHashB
	_, err = cs.CommitTrajectorySegment(ctx, prepared.ID, manifestPath, commitTime.Add(time.Minute), expectWrong)
	require.ErrorIs(t, err, store.ErrTrajectorySegmentConflict)

	// Re-prepare of committed identity with the same raw_sha256 returns committed (idempotent replay).
	replay, err := cs.PrepareTrajectorySegment(ctx, base)
	require.NoError(t, err)
	assert.Equal(t, prepared.ID, replay.ID)
	assert.Equal(t, telemetrycontract.TrajectorySegmentStatusCommitted, replay.Status)

	// Re-prepare of committed identity with a different raw_sha256 fails with ErrTrajectorySegmentConflict.
	conflict := *base
	conflict.RawSHA256 = rawHashB
	_, err = cs.PrepareTrajectorySegment(ctx, &conflict)
	require.ErrorIs(t, err, store.ErrTrajectorySegmentConflict)

	// Committed segment + expired pending segment are both counted in run charged bytes.
	sumCommitted, err := cs.SumTrajectoryRunRawBytes(ctx, "agent-1", "run-1", "")
	require.NoError(t, err)
	assert.Equal(t, int64(1024+4096), sumCommitted)

	// Bumping generation creates a distinct segment identity.
	gen1 := *base
	gen1.Generation = 1
	gen1.RawSHA256 = rawHashB
	gen1.ObjectPath = "hub-1/trajectories/v1/raw/project=proj-1/agent=agent-1/run=run-1/claude/.claude/projects/-workspace/sess-1.jsonl/g1-0000000000000000.seg.gz"
	preparedGen1, err := cs.PrepareTrajectorySegment(ctx, &gen1)
	require.NoError(t, err)
	assert.NotEqual(t, prepared.ID, preparedGen1.ID)

	// R2 TOCTOU guard: Prepare A -> Prepare B (refresh same pending identity) ->
	// Commit with A's expectation must fail with ErrTrajectorySegmentModified
	// and leave the row pending (never committed with B's unverified hashes).
	gen1Refresh := gen1
	gen1Refresh.ID = ""
	gen1Refresh.ByteEnd = 2048
	gen1Refresh.RawSHA256 = strings.Repeat("d", 64)
	gen1Refresh.CompressedSize = 512
	gen1Refresh.CompressedSHA256 = strings.Repeat("e", 64)
	refreshedGen1, err := cs.PrepareTrajectorySegment(ctx, &gen1Refresh)
	require.NoError(t, err)
	assert.Equal(t, preparedGen1.ID, refreshedGen1.ID)

	_, err = cs.CommitTrajectorySegment(ctx, preparedGen1.ID, gen1.ObjectPath+".manifest.json", now.Add(3*time.Minute), store.TrajectorySegmentCommitExpectation{
		ByteEnd:          gen1.ByteEnd,
		RawSHA256:        gen1.RawSHA256,
		CompressedSize:   gen1.CompressedSize,
		CompressedSHA256: gen1.CompressedSHA256,
		IncompleteTail:   gen1.IncompleteTail,
		SessionID:        gen1.SessionID,
	})
	require.ErrorIs(t, err, store.ErrTrajectorySegmentModified)

	stillPending, err := cs.GetTrajectorySegment(ctx, preparedGen1.ID)
	require.NoError(t, err)
	assert.Equal(t, telemetrycontract.TrajectorySegmentStatusPending, stillPending.Status)
	assert.Equal(t, gen1Refresh.RawSHA256, stillPending.RawSHA256)

	// N1 TOCTOU guard: refreshing only incomplete_tail or session_id on a pending
	// row must also cause Commit with the prior expectation to fail with
	// ErrTrajectorySegmentModified so the DB row and manifest never disagree.
	gen2 := *base
	gen2.Generation = 2
	gen2.IncompleteTail = false
	gen2.SessionID = "sess-before"
	gen2.ObjectPath = "hub-1/trajectories/v1/raw/project=proj-1/agent=agent-1/run=run-1/claude/.claude/projects/-workspace/sess-1.jsonl/g2-0000000000000000.seg.gz"
	preparedGen2, err := cs.PrepareTrajectorySegment(ctx, &gen2)
	require.NoError(t, err)

	// Flip incomplete_tail only.
	gen2TailFlip := gen2
	gen2TailFlip.ID = ""
	gen2TailFlip.IncompleteTail = true
	_, err = cs.PrepareTrajectorySegment(ctx, &gen2TailFlip)
	require.NoError(t, err)

	_, err = cs.CommitTrajectorySegment(ctx, preparedGen2.ID, gen2.ObjectPath+".manifest.json", now.Add(4*time.Minute), store.TrajectorySegmentCommitExpectation{
		ByteEnd:          gen2.ByteEnd,
		RawSHA256:        gen2.RawSHA256,
		CompressedSize:   gen2.CompressedSize,
		CompressedSHA256: gen2.CompressedSHA256,
		IncompleteTail:   false,
		SessionID:        "sess-before",
	})
	require.ErrorIs(t, err, store.ErrTrajectorySegmentModified)

	// Change session_id only.
	gen2SessFlip := gen2TailFlip
	gen2SessFlip.SessionID = "sess-after"
	_, err = cs.PrepareTrajectorySegment(ctx, &gen2SessFlip)
	require.NoError(t, err)

	_, err = cs.CommitTrajectorySegment(ctx, preparedGen2.ID, gen2.ObjectPath+".manifest.json", now.Add(4*time.Minute), store.TrajectorySegmentCommitExpectation{
		ByteEnd:          gen2.ByteEnd,
		RawSHA256:        gen2.RawSHA256,
		CompressedSize:   gen2.CompressedSize,
		CompressedSHA256: gen2.CompressedSHA256,
		IncompleteTail:   true,
		SessionID:        "sess-before",
	})
	require.ErrorIs(t, err, store.ErrTrajectorySegmentModified)

	stillPendingGen2, err := cs.GetTrajectorySegment(ctx, preparedGen2.ID)
	require.NoError(t, err)
	assert.Equal(t, telemetrycontract.TrajectorySegmentStatusPending, stillPendingGen2.Status)
	assert.True(t, stillPendingGen2.IncompleteTail)
	assert.Equal(t, "sess-after", stillPendingGen2.SessionID)

	// R1 monotonic charged_bytes high-water mark: refreshing a pending row to
	// smaller byte_end / compressed_size never decreases charged_bytes or the
	// sum reported by SumTrajectoryRunRawBytes.
	gen3 := *base
	gen3.Generation = 3
	gen3.ByteStart = 0
	gen3.ByteEnd = 8192
	gen3.CompressedSize = 4096
	gen3.ObjectPath = "hub-1/trajectories/v1/raw/project=proj-1/agent=agent-1/run=run-1/claude/.claude/projects/-workspace/sess-1.jsonl/g3-0000000000000000.seg.gz"
	preparedGen3, err := cs.PrepareTrajectorySegment(ctx, &gen3)
	require.NoError(t, err)
	assert.Equal(t, int64(8192), preparedGen3.ChargedBytes)

	sumBeforeShrink, err := cs.SumTrajectoryRunRawBytes(ctx, "agent-1", "run-1", "")
	require.NoError(t, err)

	gen3Shrink := gen3
	gen3Shrink.ID = ""
	gen3Shrink.ByteEnd = 1
	gen3Shrink.CompressedSize = 25
	gen3Shrink.ChargedBytes = 0
	refreshedGen3, err := cs.PrepareTrajectorySegment(ctx, &gen3Shrink)
	require.NoError(t, err)
	assert.Equal(t, preparedGen3.ID, refreshedGen3.ID)
	assert.Equal(t, int64(8192), refreshedGen3.ChargedBytes, "charged_bytes must be monotonic and never decrease")

	sumAfterShrink, err := cs.SumTrajectoryRunRawBytes(ctx, "agent-1", "run-1", "")
	require.NoError(t, err)
	assert.Equal(t, sumBeforeShrink, sumAfterShrink, "SumTrajectoryRunRawBytes must not decrease after shrinking a pending row")
}

func TestTrajectorySegmentStore_ListCommittedFiltersAndPaging(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	makeCommitted := func(runID, relPath string, gen int, byteStart, byteEnd int64, hashChar string) *store.TrajectorySegment {
		t.Helper()
		rawHash := strings.Repeat(hashChar, 64)
		compHash := strings.Repeat("f", 64)
		objPath := telemetrycontract.TrajectoryRawObjectPath("hub-1", "proj-1", "agent-1", runID, "claude", relPath, gen, byteStart)
		prep, err := cs.PrepareTrajectorySegment(ctx, &store.TrajectorySegment{
			HubID:              "hub-1",
			OriginHubID:        "hub-1",
			ProjectID:          "proj-1",
			AgentID:            "agent-1",
			RunID:              runID,
			Harness:            "claude",
			NativeRelativePath: relPath,
			ArtifactKind:       telemetrycontract.TrajectoryArtifactKindJSONLAppend,
			Generation:         gen,
			ByteStart:          byteStart,
			ByteEnd:            byteEnd,
			RawSHA256:          rawHash,
			CompressedSize:     128,
			CompressedSHA256:   compHash,
			ObjectPath:         objPath,
			CreatedAt:          now,
			PrepareExpiresAt:   now.Add(15 * time.Minute),
		})
		require.NoError(t, err)
		committed, err := cs.CommitTrajectorySegment(ctx, prep.ID, telemetrycontract.TrajectoryRawManifestPath(objPath), now.Add(time.Second), store.TrajectorySegmentCommitExpectation{
			ByteEnd:          byteEnd,
			RawSHA256:        rawHash,
			CompressedSize:   128,
			CompressedSHA256: compHash,
		})
		require.NoError(t, err)
		return committed
	}

	s1 := makeCommitted("run-1", ".claude/projects/-workspace/a.jsonl", 0, 0, 100, "1")
	s2 := makeCommitted("run-1", ".claude/projects/-workspace/a.jsonl", 0, 100, 250, "2")
	s3 := makeCommitted("run-1", ".claude/projects/-workspace/a.jsonl", 1, 0, 300, "3")
	s4 := makeCommitted("run-2", ".claude/projects/-workspace/b.jsonl", 0, 0, 400, "4")

	// Pending segment must never appear in ListCommittedTrajectorySegments.
	_, err := cs.PrepareTrajectorySegment(ctx, &store.TrajectorySegment{
		HubID:              "hub-1",
		ProjectID:          "proj-1",
		AgentID:            "agent-1",
		RunID:              "run-2",
		Harness:            "claude",
		NativeRelativePath: ".claude/projects/-workspace/b.jsonl",
		ArtifactKind:       telemetrycontract.TrajectoryArtifactKindJSONLAppend,
		Generation:         0,
		ByteStart:          400,
		ByteEnd:            500,
		RawSHA256:          strings.Repeat("5", 64),
		CompressedSize:     64,
		CompressedSHA256:   strings.Repeat("6", 64),
		ObjectPath:         "hub-1/pending.seg.gz",
		CreatedAt:          now,
		PrepareExpiresAt:   now.Add(15 * time.Minute),
	})
	require.NoError(t, err)

	// Unfiltered list returns all 4 committed segments in deterministic order.
	all, hasMore, err := cs.ListCommittedTrajectorySegments(ctx, store.TrajectorySegmentListFilter{
		AgentID: "agent-1",
		Limit:   10,
	})
	require.NoError(t, err)
	assert.False(t, hasMore)
	require.Len(t, all, 4)
	assert.Equal(t, []string{s1.ID, s2.ID, s3.ID, s4.ID}, []string{all[0].ID, all[1].ID, all[2].ID, all[3].ID})

	// Pagination with limit=2.
	page1, more1, err := cs.ListCommittedTrajectorySegments(ctx, store.TrajectorySegmentListFilter{
		AgentID: "agent-1",
		Limit:   2,
		Offset:  0,
	})
	require.NoError(t, err)
	assert.True(t, more1)
	require.Len(t, page1, 2)
	assert.Equal(t, s1.ID, page1[0].ID)
	assert.Equal(t, s2.ID, page1[1].ID)

	page2, more2, err := cs.ListCommittedTrajectorySegments(ctx, store.TrajectorySegmentListFilter{
		AgentID: "agent-1",
		Limit:   2,
		Offset:  2,
	})
	require.NoError(t, err)
	assert.False(t, more2)
	require.Len(t, page2, 2)
	assert.Equal(t, s3.ID, page2[0].ID)
	assert.Equal(t, s4.ID, page2[1].ID)

	// Filters by project_id, run_id, generation, native_relative_path.
	gen1 := 1
	filtered, moreF, err := cs.ListCommittedTrajectorySegments(ctx, store.TrajectorySegmentListFilter{
		ProjectID:          "proj-1",
		AgentID:            "agent-1",
		RunID:              "run-1",
		Generation:         &gen1,
		NativeRelativePath: ".claude/projects/-workspace/a.jsonl",
		Limit:              10,
	})
	require.NoError(t, err)
	assert.False(t, moreF)
	require.Len(t, filtered, 1)
	assert.Equal(t, s3.ID, filtered[0].ID)

	// Mismatched ProjectID returns empty.
	wrongProj, moreWP, err := cs.ListCommittedTrajectorySegments(ctx, store.TrajectorySegmentListFilter{
		ProjectID: "proj-other",
		AgentID:   "agent-1",
		Limit:     10,
	})
	require.NoError(t, err)
	assert.False(t, moreWP)
	assert.Empty(t, wrongProj)
}
