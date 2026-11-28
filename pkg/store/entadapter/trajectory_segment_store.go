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

package entadapter

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	entsql "entgo.io/ent/dialect/sql"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/trajectorysegment"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

// TrajectorySegmentStore implements store.TrajectorySegmentStore using Ent.
type TrajectorySegmentStore struct {
	client *ent.Client
	inTx   bool
}

// NewTrajectorySegmentStore creates a new Ent-backed TrajectorySegmentStore.
func NewTrajectorySegmentStore(client *ent.Client) *TrajectorySegmentStore {
	return &TrajectorySegmentStore{client: client}
}

var _ store.TrajectorySegmentStore = (*TrajectorySegmentStore)(nil)
var _ store.TrajectorySegmentStore = (*CompositeStore)(nil)

func entTrajectorySegmentToStore(row *ent.TrajectorySegment) *store.TrajectorySegment {
	if row == nil {
		return nil
	}
	var committedAt *time.Time
	if row.CommittedAt != nil {
		t := row.CommittedAt.UTC()
		committedAt = &t
	}
	return &store.TrajectorySegment{
		ID:                 row.ID,
		HubID:              row.HubID,
		OriginHubID:        row.OriginHubID,
		ProjectID:          row.ProjectID,
		AgentID:            row.AgentID,
		RunID:              row.RunID,
		Harness:            row.Harness,
		NativeRelativePath: row.NativeRelativePath,
		ArtifactKind:       row.ArtifactKind,
		Generation:         row.Generation,
		ByteStart:          row.ByteStart,
		ByteEnd:            row.ByteEnd,
		RawSHA256:          row.RawSha256,
		CompressedSize:     row.CompressedSize,
		ChargedBytes:       row.ChargedBytes,
		CompressedSHA256:   row.CompressedSha256,
		IncompleteTail:     row.IncompleteTail,
		SessionID:          row.SessionID,
		ObjectPath:         row.ObjectPath,
		ManifestPath:       row.ManifestPath,
		Status:             row.Status,
		NormalizeStatus:    row.NormalizeStatus,
		CreatedAt:          row.CreatedAt.UTC(),
		CommittedAt:        committedAt,
		PrepareExpiresAt:   row.PrepareExpiresAt.UTC(),
	}
}

// PrepareTrajectorySegment implements store.TrajectorySegmentStore.
// Uses INSERT ... ON CONFLICT (agent_id, run_id, harness, native_relative_path,
// generation, byte_start) DO NOTHING so concurrent prepares are safe on both
// SQLite and Postgres.
func (s *TrajectorySegmentStore) PrepareTrajectorySegment(ctx context.Context, seg *store.TrajectorySegment) (*store.TrajectorySegment, error) {
	if seg == nil || seg.AgentID == "" || seg.RunID == "" || seg.Harness == "" || seg.NativeRelativePath == "" || seg.ObjectPath == "" {
		return nil, fmt.Errorf("%w: segment identity and object_path are required", store.ErrInvalidInput)
	}
	if seg.ByteStart < 0 || seg.ByteEnd <= seg.ByteStart {
		return nil, fmt.Errorf("%w: invalid byte range [%d, %d)", store.ErrInvalidInput, seg.ByteStart, seg.ByteEnd)
	}
	if seg.CompressedSize <= 0 || seg.RawSHA256 == "" || seg.CompressedSHA256 == "" {
		return nil, fmt.Errorf("%w: compressed_size and sha256 hashes are required", store.ErrInvalidInput)
	}

	id := seg.ID
	if id == "" {
		id = api.NewUUID()
	}
	now := seg.CreatedAt
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC().Truncate(time.Microsecond)
	expiresAt := seg.PrepareExpiresAt
	if expiresAt.IsZero() {
		expiresAt = now.Add(telemetrycontract.DefaultTrajectoryPrepareTTL)
	}
	expiresAt = expiresAt.UTC().Truncate(time.Microsecond)

	status := seg.Status
	if status == "" {
		status = telemetrycontract.TrajectorySegmentStatusPending
	}
	normStatus := seg.NormalizeStatus
	if normStatus == "" {
		normStatus = telemetrycontract.TrajectoryNormalizeStatusPending
	}

	reqCharged := max(seg.ByteEnd-seg.ByteStart, seg.CompressedSize)
	if seg.ChargedBytes > reqCharged {
		reqCharged = seg.ChargedBytes
	}

	drv := s.client.Driver()
	query, args := entsql.Dialect(drv.Dialect()).
		Insert(trajectorysegment.Table).
		Columns(
			trajectorysegment.FieldID,
			trajectorysegment.FieldHubID,
			trajectorysegment.FieldOriginHubID,
			trajectorysegment.FieldProjectID,
			trajectorysegment.FieldAgentID,
			trajectorysegment.FieldRunID,
			trajectorysegment.FieldHarness,
			trajectorysegment.FieldNativeRelativePath,
			trajectorysegment.FieldArtifactKind,
			trajectorysegment.FieldGeneration,
			trajectorysegment.FieldByteStart,
			trajectorysegment.FieldByteEnd,
			trajectorysegment.FieldRawSha256,
			trajectorysegment.FieldCompressedSize,
			trajectorysegment.FieldChargedBytes,
			trajectorysegment.FieldCompressedSha256,
			trajectorysegment.FieldIncompleteTail,
			trajectorysegment.FieldSessionID,
			trajectorysegment.FieldObjectPath,
			trajectorysegment.FieldManifestPath,
			trajectorysegment.FieldStatus,
			trajectorysegment.FieldNormalizeStatus,
			trajectorysegment.FieldCreatedAt,
			trajectorysegment.FieldPrepareExpiresAt,
		).
		Values(
			id,
			seg.HubID,
			seg.OriginHubID,
			seg.ProjectID,
			seg.AgentID,
			seg.RunID,
			seg.Harness,
			seg.NativeRelativePath,
			seg.ArtifactKind,
			seg.Generation,
			seg.ByteStart,
			seg.ByteEnd,
			seg.RawSHA256,
			seg.CompressedSize,
			reqCharged,
			seg.CompressedSHA256,
			seg.IncompleteTail,
			seg.SessionID,
			seg.ObjectPath,
			"",
			status,
			normStatus,
			now,
			expiresAt,
		).
		OnConflict(
			entsql.ConflictColumns(
				trajectorysegment.FieldAgentID,
				trajectorysegment.FieldRunID,
				trajectorysegment.FieldHarness,
				trajectorysegment.FieldNativeRelativePath,
				trajectorysegment.FieldGeneration,
				trajectorysegment.FieldByteStart,
			),
			entsql.DoNothing(),
		).
		Query()

	var res sql.Result
	if err := drv.Exec(ctx, query, args, &res); err != nil {
		return nil, mapError(err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return nil, mapError(err)
	}
	if affected > 0 {
		out := *seg
		out.ID = id
		out.ChargedBytes = reqCharged
		out.Status = status
		out.NormalizeStatus = normStatus
		out.CreatedAt = now
		out.PrepareExpiresAt = expiresAt
		out.ManifestPath = ""
		out.CommittedAt = nil
		return &out, nil
	}

	// Row already exists for this identity; inspect it.
	existing, err := s.GetTrajectorySegmentByIdentity(ctx, store.TrajectorySegmentIdentity{
		AgentID:            seg.AgentID,
		RunID:              seg.RunID,
		Harness:            seg.Harness,
		NativeRelativePath: seg.NativeRelativePath,
		Generation:         seg.Generation,
		ByteStart:          seg.ByteStart,
	})
	if err != nil {
		return nil, err
	}

	if existing.Status == telemetrycontract.TrajectorySegmentStatusCommitted {
		if existing.RawSHA256 == seg.RawSHA256 && existing.ByteEnd == seg.ByteEnd {
			return existing, nil
		}
		return nil, store.ErrTrajectorySegmentConflict
	}

	// Existing row is still pending: refresh pending metadata & TTL only while
	// status remains pending so we never overwrite a concurrent commit.
	// charged_bytes is strictly monotonic (high-water mark) so a pending
	// refresh can never shrink a row's contribution to the per-run cap.
	highWater := max(existing.ChargedBytes, max(existing.ByteEnd-existing.ByteStart, existing.CompressedSize), reqCharged)
	n, err := s.client.TrajectorySegment.Update().
		Where(
			trajectorysegment.IDEQ(existing.ID),
			trajectorysegment.StatusEQ(telemetrycontract.TrajectorySegmentStatusPending),
		).
		SetByteEnd(seg.ByteEnd).
		SetRawSha256(seg.RawSHA256).
		SetCompressedSize(seg.CompressedSize).
		SetChargedBytes(highWater).
		SetCompressedSha256(seg.CompressedSHA256).
		SetIncompleteTail(seg.IncompleteTail).
		SetSessionID(seg.SessionID).
		SetOriginHubID(seg.OriginHubID).
		SetPrepareExpiresAt(expiresAt).
		Save(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	refreshed, err := s.GetTrajectorySegment(ctx, existing.ID)
	if err != nil {
		return nil, err
	}
	if n == 0 && refreshed.Status == telemetrycontract.TrajectorySegmentStatusCommitted {
		// Committed concurrently between read and update.
		if refreshed.RawSHA256 == seg.RawSHA256 && refreshed.ByteEnd == seg.ByteEnd {
			return refreshed, nil
		}
		return nil, store.ErrTrajectorySegmentConflict
	}
	return refreshed, nil
}

// GetTrajectorySegment implements store.TrajectorySegmentStore.
func (s *TrajectorySegmentStore) GetTrajectorySegment(ctx context.Context, segmentID string) (*store.TrajectorySegment, error) {
	if segmentID == "" {
		return nil, store.ErrNotFound
	}
	row, err := s.client.TrajectorySegment.Query().
		Where(trajectorysegment.IDEQ(segmentID)).
		Only(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return entTrajectorySegmentToStore(row), nil
}

// GetTrajectorySegmentByIdentity implements store.TrajectorySegmentStore.
func (s *TrajectorySegmentStore) GetTrajectorySegmentByIdentity(ctx context.Context, id store.TrajectorySegmentIdentity) (*store.TrajectorySegment, error) {
	row, err := s.client.TrajectorySegment.Query().
		Where(
			trajectorysegment.AgentIDEQ(id.AgentID),
			trajectorysegment.RunIDEQ(id.RunID),
			trajectorysegment.HarnessEQ(id.Harness),
			trajectorysegment.NativeRelativePathEQ(id.NativeRelativePath),
			trajectorysegment.GenerationEQ(id.Generation),
			trajectorysegment.ByteStartEQ(id.ByteStart),
		).
		Only(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return entTrajectorySegmentToStore(row), nil
}

// CommitTrajectorySegment implements store.TrajectorySegmentStore.
func (s *TrajectorySegmentStore) CommitTrajectorySegment(ctx context.Context, segmentID, manifestPath string, committedAt time.Time, expect store.TrajectorySegmentCommitExpectation) (*store.TrajectorySegment, error) {
	if segmentID == "" {
		return nil, store.ErrNotFound
	}
	if committedAt.IsZero() {
		committedAt = time.Now()
	}
	committedAt = committedAt.UTC().Truncate(time.Microsecond)

	n, err := s.client.TrajectorySegment.Update().
		Where(
			trajectorysegment.IDEQ(segmentID),
			trajectorysegment.StatusEQ(telemetrycontract.TrajectorySegmentStatusPending),
			trajectorysegment.ByteEndEQ(expect.ByteEnd),
			trajectorysegment.RawSha256EQ(expect.RawSHA256),
			trajectorysegment.CompressedSizeEQ(expect.CompressedSize),
			trajectorysegment.CompressedSha256EQ(expect.CompressedSHA256),
			trajectorysegment.IncompleteTailEQ(expect.IncompleteTail),
			trajectorysegment.SessionIDEQ(expect.SessionID),
		).
		SetStatus(telemetrycontract.TrajectorySegmentStatusCommitted).
		SetManifestPath(manifestPath).
		SetCommittedAt(committedAt).
		Save(ctx)
	if err != nil {
		return nil, mapError(err)
	}

	row, err := s.GetTrajectorySegment(ctx, segmentID)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		if row.Status == telemetrycontract.TrajectorySegmentStatusCommitted {
			if row.ByteEnd == expect.ByteEnd &&
				row.RawSHA256 == expect.RawSHA256 &&
				row.CompressedSize == expect.CompressedSize &&
				row.CompressedSHA256 == expect.CompressedSHA256 &&
				row.IncompleteTail == expect.IncompleteTail &&
				row.SessionID == expect.SessionID {
				return row, nil
			}
			return nil, store.ErrTrajectorySegmentConflict
		}
		if row.Status == telemetrycontract.TrajectorySegmentStatusPending {
			return nil, store.ErrTrajectorySegmentModified
		}
		return nil, fmt.Errorf("unexpected trajectory segment status %q", row.Status)
	}
	return row, nil
}

// SumTrajectoryRunRawBytes implements store.TrajectorySegmentStore using a
// single SQL aggregate of MAX(charged_bytes, byte_end - byte_start,
// compressed_size) over all committed and pending segments (including expired
// pending rows) so orphaned uncommitted uploads cannot bypass the per-run
// storage cap by declaring a tiny raw range with a large compressed_size or by
// shrinking a pending row after upload. Uses ANSI CASE WHEN for SQLite and
// Postgres parity. Re-prepares of the same identity reuse the existing row and
// exclude it via excludeID.
func (s *TrajectorySegmentStore) SumTrajectoryRunRawBytes(ctx context.Context, agentID, runID, excludeID string) (int64, error) {
	if agentID == "" || runID == "" {
		return 0, nil
	}

	rawExpr := "(" + trajectorysegment.FieldByteEnd + " - " + trajectorysegment.FieldByteStart + ")"
	cb := trajectorysegment.FieldChargedBytes
	cs := trajectorysegment.FieldCompressedSize
	chargedExpr := "CASE WHEN " + cb + " >= " + rawExpr + " AND " + cb + " >= " + cs +
		" THEN " + cb +
		" WHEN " + rawExpr + " >= " + cs +
		" THEN " + rawExpr +
		" ELSE " + cs + " END"

	drv := s.client.Driver()
	sb := entsql.Dialect(drv.Dialect()).
		Select("COALESCE(SUM(" + chargedExpr + "), 0)").
		From(entsql.Table(trajectorysegment.Table)).
		Where(entsql.And(
			entsql.EQ(trajectorysegment.FieldAgentID, agentID),
			entsql.EQ(trajectorysegment.FieldRunID, runID),
			entsql.In(trajectorysegment.FieldStatus,
				telemetrycontract.TrajectorySegmentStatusCommitted,
				telemetrycontract.TrajectorySegmentStatusPending,
			),
		))
	if excludeID != "" {
		sb.Where(entsql.NEQ(trajectorysegment.FieldID, excludeID))
	}
	query, args := sb.Query()

	var rows entsql.Rows
	if err := drv.Query(ctx, query, args, &rows); err != nil {
		if errors.Is(mapError(err), store.ErrNotFound) {
			return 0, nil
		}
		return 0, mapError(err)
	}
	defer func() { _ = rows.Close() }()

	var total int64
	if rows.Next() {
		if err := rows.Scan(&total); err != nil {
			return 0, mapError(err)
		}
	}
	if err := rows.Err(); err != nil {
		return 0, mapError(err)
	}
	return total, nil
}

// CountTrajectoryRunSegments implements store.TrajectorySegmentStore using a
// single SQL COUNT(*) over all committed and pending segments (including
// expired pending rows) for (agentID, runID), excluding excludeID if non-empty.
func (s *TrajectorySegmentStore) CountTrajectoryRunSegments(ctx context.Context, agentID, runID, excludeID string) (int64, error) {
	if agentID == "" || runID == "" {
		return 0, nil
	}

	drv := s.client.Driver()
	sb := entsql.Dialect(drv.Dialect()).
		Select("COUNT(*)").
		From(entsql.Table(trajectorysegment.Table)).
		Where(entsql.And(
			entsql.EQ(trajectorysegment.FieldAgentID, agentID),
			entsql.EQ(trajectorysegment.FieldRunID, runID),
			entsql.In(trajectorysegment.FieldStatus,
				telemetrycontract.TrajectorySegmentStatusCommitted,
				telemetrycontract.TrajectorySegmentStatusPending,
			),
		))
	if excludeID != "" {
		sb.Where(entsql.NEQ(trajectorysegment.FieldID, excludeID))
	}
	query, args := sb.Query()

	var rows entsql.Rows
	if err := drv.Query(ctx, query, args, &rows); err != nil {
		if errors.Is(mapError(err), store.ErrNotFound) {
			return 0, nil
		}
		return 0, mapError(err)
	}
	defer func() { _ = rows.Close() }()

	var count int64
	if rows.Next() {
		if err := rows.Scan(&count); err != nil {
			return 0, mapError(err)
		}
	}
	if err := rows.Err(); err != nil {
		return 0, mapError(err)
	}
	return count, nil
}

// ListCommittedTrajectorySegments implements store.TrajectorySegmentStore.
func (s *TrajectorySegmentStore) ListCommittedTrajectorySegments(ctx context.Context, filter store.TrajectorySegmentListFilter) ([]*store.TrajectorySegment, bool, error) {
	if filter.AgentID == "" {
		return nil, false, fmt.Errorf("%w: agent_id is required", store.ErrInvalidInput)
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = 100
	}
	offset := filter.Offset
	if offset < 0 {
		offset = 0
	}

	q := s.client.TrajectorySegment.Query().
		Where(
			trajectorysegment.AgentIDEQ(filter.AgentID),
			trajectorysegment.StatusEQ(telemetrycontract.TrajectorySegmentStatusCommitted),
		)
	if filter.ProjectID != "" {
		q = q.Where(trajectorysegment.ProjectIDEQ(filter.ProjectID))
	}
	if filter.RunID != "" {
		q = q.Where(trajectorysegment.RunIDEQ(filter.RunID))
	}
	if filter.Generation != nil {
		q = q.Where(trajectorysegment.GenerationEQ(*filter.Generation))
	}
	if filter.NativeRelativePath != "" {
		q = q.Where(trajectorysegment.NativeRelativePathEQ(filter.NativeRelativePath))
	}

	rows, err := q.
		Order(
			ent.Asc(trajectorysegment.FieldRunID),
			ent.Asc(trajectorysegment.FieldNativeRelativePath),
			ent.Asc(trajectorysegment.FieldGeneration),
			ent.Asc(trajectorysegment.FieldByteStart),
			ent.Asc(trajectorysegment.FieldID),
		).
		Offset(offset).
		Limit(limit + 1).
		All(ctx)
	if err != nil {
		return nil, false, mapError(err)
	}

	hasMore := false
	if len(rows) > limit {
		hasMore = true
		rows = rows[:limit]
	}

	out := make([]*store.TrajectorySegment, 0, len(rows))
	for _, row := range rows {
		out = append(out, entTrajectorySegmentToStore(row))
	}
	return out, hasMore, nil
}
