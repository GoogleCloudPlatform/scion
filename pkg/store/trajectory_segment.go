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

package store

import (
	"context"
	"errors"
	"time"
)

// ErrTrajectorySegmentConflict is returned when a segment identity is already
// committed with a different raw_sha256 or byte_end.
var ErrTrajectorySegmentConflict = errors.New("trajectory segment conflict")

// ErrTrajectorySegmentModified is returned by CommitTrajectorySegment when a
// pending segment was concurrently modified by a newer prepare between
// verification and commit.
var ErrTrajectorySegmentModified = errors.New("trajectory segment modified before commit")

// TrajectorySegmentCommitExpectation binds CommitTrajectorySegment to the
// exact verified snapshot so a concurrent pending refresh cannot cause a row
// to be committed with unverified hashes, byte range, or manifest metadata.
type TrajectorySegmentCommitExpectation struct {
	ByteEnd          int64
	RawSHA256        string
	CompressedSize   int64
	CompressedSHA256 string
	IncompleteTail   bool
	SessionID        string
}

// TrajectorySegmentIdentity is the unique natural key of a raw trajectory
// segment.
type TrajectorySegmentIdentity struct {
	AgentID            string
	RunID              string
	Harness            string
	NativeRelativePath string
	Generation         int
	ByteStart          int64
}

// TrajectorySegment represents a row in the trajectory_segments table.
type TrajectorySegment struct {
	ID                 string     `json:"id"`
	HubID              string     `json:"hubId"`
	OriginHubID        string     `json:"originHubId,omitempty"`
	ProjectID          string     `json:"projectId"`
	AgentID            string     `json:"agentId"`
	RunID              string     `json:"runId"`
	Harness            string     `json:"harness"`
	NativeRelativePath string     `json:"nativeRelativePath"`
	ArtifactKind       string     `json:"artifactKind"`
	Generation         int        `json:"generation"`
	ByteStart          int64      `json:"byteStart"`
	ByteEnd            int64      `json:"byteEnd"`
	RawSHA256          string     `json:"rawSha256"`
	CompressedSize     int64      `json:"compressedSize"`
	ChargedBytes       int64      `json:"chargedBytes"`
	CompressedSHA256   string     `json:"compressedSha256"`
	IncompleteTail     bool       `json:"incompleteTail"`
	SessionID          string     `json:"sessionId,omitempty"`
	ObjectPath         string     `json:"objectPath"`
	ManifestPath       string     `json:"manifestPath,omitempty"`
	Status             string     `json:"status"`
	NormalizeStatus    string     `json:"normalizeStatus"`
	CreatedAt          time.Time  `json:"createdAt"`
	CommittedAt        *time.Time `json:"committedAt,omitempty"`
	PrepareExpiresAt   time.Time  `json:"prepareExpiresAt"`
}

// TrajectorySegmentStore defines persistence operations for Track 2 L0 raw
// trajectory segments (ptone/scion#9250).
type TrajectorySegmentStore interface {
	// PrepareTrajectorySegment atomically inserts a pending segment row using
	// ON CONFLICT DO NOTHING on the segment identity, or handles an existing
	// row for that identity:
	//   - If already committed with matching raw_sha256 and byte_end, returns
	//     the committed row (idempotent replay).
	//   - If already committed with a different raw_sha256 or byte_end, returns
	//     ErrTrajectorySegmentConflict.
	//   - If still pending, updates the pending metadata and prepare_expires_at
	//     and returns the refreshed pending row.
	PrepareTrajectorySegment(ctx context.Context, seg *TrajectorySegment) (*TrajectorySegment, error)

	// GetTrajectorySegment returns the trajectory_segments row by opaque ID,
	// or ErrNotFound if no row exists.
	GetTrajectorySegment(ctx context.Context, segmentID string) (*TrajectorySegment, error)

	// GetTrajectorySegmentByIdentity returns the trajectory_segments row for
	// the given natural identity, or ErrNotFound if no row exists.
	GetTrajectorySegmentByIdentity(ctx context.Context, id TrajectorySegmentIdentity) (*TrajectorySegment, error)

	// CommitTrajectorySegment marks a pending segment as committed with the
	// Hub-authored manifest path and commit timestamp, conditionally matching
	// the verified snapshot in expect. Idempotent when the segment is already
	// committed with matching expect values. Returns ErrNotFound if segmentID
	// does not exist, ErrTrajectorySegmentConflict if already committed with
	// different content, or ErrTrajectorySegmentModified if still pending but
	// modified concurrently after verification.
	CommitTrajectorySegment(ctx context.Context, segmentID, manifestPath string, committedAt time.Time, expect TrajectorySegmentCommitExpectation) (*TrajectorySegment, error)

	// SumTrajectoryRunRawBytes returns the total charged bytes —
	// MAX(charged_bytes, byte_end - byte_start, compressed_size) per row —
	// across all committed and pending segments (regardless of prepare_expires_at)
	// for (agentID, runID), excluding excludeID if non-empty. Each row's
	// charged_bytes is a monotonic high-water mark that never decreases on
	// pending refresh, bounding orphaned uncommitted object storage per run (no
	// retention sweep exists in P5, so pending charges persist for the lifetime
	// of the run). Re-preparing the same identity reuses the existing row and
	// passes its ID as excludeID so retries and probes do not double-count.
	SumTrajectoryRunRawBytes(ctx context.Context, agentID, runID, excludeID string) (int64, error)

	// CountTrajectoryRunSegments returns the number of trajectory_segments rows
	// (committed and pending, regardless of prepare_expires_at) for
	// (agentID, runID), excluding excludeID if non-empty.
	CountTrajectoryRunSegments(ctx context.Context, agentID, runID, excludeID string) (int64, error)

	// ListCommittedTrajectorySegments returns committed segments for an agent
	// matching filter in deterministic (run_id, native_relative_path, generation,
	// byte_start, id) order, along with hasMore indicating whether additional
	// rows exist beyond filter.Offset+len(result).
	ListCommittedTrajectorySegments(ctx context.Context, filter TrajectorySegmentListFilter) ([]*TrajectorySegment, bool, error)
}

// TrajectorySegmentListFilter scopes committed trajectory segment listing for an agent.
type TrajectorySegmentListFilter struct {
	AgentID            string
	RunID              string
	Generation         *int
	NativeRelativePath string
	Limit              int
	Offset             int
}
