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
	// Hub-authored manifest path and commit timestamp. Idempotent when the
	// segment is already committed with the same raw_sha256. Returns
	// ErrNotFound if segmentID does not exist, or ErrTrajectorySegmentConflict
	// if committed with a different hash.
	CommitTrajectorySegment(ctx context.Context, segmentID, manifestPath string, committedAt time.Time) (*TrajectorySegment, error)

	// SumTrajectoryRunRawBytes returns the total raw bytes (byte_end - byte_start)
	// across committed segments (and non-expired pending segments at now) for
	// (agentID, runID), excluding excludeID if non-empty.
	SumTrajectoryRunRawBytes(ctx context.Context, agentID, runID, excludeID string, now time.Time) (int64, error)
}
