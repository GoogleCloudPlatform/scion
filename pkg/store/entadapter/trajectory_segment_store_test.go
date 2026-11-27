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

	// Per-run raw byte sum includes non-expired pending segment, excluding self when excludeID is passed.
	sumAll, err := cs.SumTrajectoryRunRawBytes(ctx, "agent-1", "run-1", "", now)
	require.NoError(t, err)
	assert.Equal(t, int64(1024), sumAll)

	sumExcludingSelf, err := cs.SumTrajectoryRunRawBytes(ctx, "agent-1", "run-1", prepared.ID, now)
	require.NoError(t, err)
	assert.Equal(t, int64(0), sumExcludingSelf)

	// Expired pending segment is not counted in run raw bytes.
	sumAfterExpiry, err := cs.SumTrajectoryRunRawBytes(ctx, "agent-1", "run-1", "", laterExp.Add(time.Minute))
	require.NoError(t, err)
	assert.Equal(t, int64(0), sumAfterExpiry)

	// Commit transitions pending -> committed with manifest_path and committed_at.
	manifestPath := base.ObjectPath + ".manifest.json"
	commitTime := now.Add(2 * time.Minute)
	expectA := store.TrajectorySegmentCommitExpectation{
		ByteEnd:          base.ByteEnd,
		RawSHA256:        base.RawSHA256,
		CompressedSize:   base.CompressedSize,
		CompressedSHA256: base.CompressedSHA256,
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

	// Committed segment is always counted in run raw bytes even past prepare_expires_at.
	sumCommitted, err := cs.SumTrajectoryRunRawBytes(ctx, "agent-1", "run-1", "", laterExp.Add(time.Hour))
	require.NoError(t, err)
	assert.Equal(t, int64(1024), sumCommitted)

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
	})
	require.ErrorIs(t, err, store.ErrTrajectorySegmentModified)

	stillPending, err := cs.GetTrajectorySegment(ctx, preparedGen1.ID)
	require.NoError(t, err)
	assert.Equal(t, telemetrycontract.TrajectorySegmentStatusPending, stillPending.Status)
	assert.Equal(t, gen1Refresh.RawSHA256, stillPending.RawSHA256)
}
