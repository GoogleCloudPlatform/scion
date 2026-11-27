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

package telemetrycontract

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Track 2 raw trajectory segment wire limits and defaults (Tokenomics design §5.1, §5.2, §5.5).
const (
	// DefaultTrajectoryMaxSegmentRawBytes is the default maximum uncompressed
	// raw byte length of a single trajectory segment (16 MiB).
	DefaultTrajectoryMaxSegmentRawBytes int64 = 16 * 1024 * 1024

	// DefaultTrajectoryMaxRunRawBytes is the default maximum cumulative
	// uncompressed raw bytes across all segments of a single run (2 GiB).
	DefaultTrajectoryMaxRunRawBytes int64 = 2 * 1024 * 1024 * 1024

	// DefaultTrajectoryUploadURLExpiry is the validity duration of signed or
	// Hub-proxied PUT upload URLs returned by segments:prepare.
	DefaultTrajectoryUploadURLExpiry = 10 * time.Minute

	// DefaultTrajectoryPrepareTTL is how long a pending prepared segment
	// remains commit-eligible before segments:commit returns 410 segment_expired.
	DefaultTrajectoryPrepareTTL = 15 * time.Minute

	// TrajectoryMaxRelativePathBytes is the maximum byte length of
	// native_relative_path.
	TrajectoryMaxRelativePathBytes = 1024

	// TrajectoryManifestSchemaVersion is the current schema_version stamped
	// into Hub-authored .manifest.json sidecars.
	TrajectoryManifestSchemaVersion = "1"
)

// Artifact kinds and encodings for raw trajectory segments.
const (
	TrajectoryArtifactKindJSONLAppend = "jsonl_append"
	TrajectoryContentEncodingGzip     = "gzip"
)

// Segment prepare/commit states and persisted status values.
const (
	TrajectorySegmentStateUploadRequired = "upload_required"
	TrajectorySegmentStateCommitted      = "committed"

	TrajectorySegmentStatusPending   = "pending"
	TrajectorySegmentStatusCommitted = "committed"

	TrajectoryNormalizeStatusPending = "pending"
)

// Effective capture policy values.
const (
	TrajectoryCapturePolicyFull         = "full"
	TrajectoryCapturePolicyMetadataOnly = "metadata_only"
	TrajectoryCapturePolicyOff          = "off"
)

// Machine-readable error codes returned by the trajectory segment endpoints.
const (
	TrajectoryErrInvalidSegment      = "invalid_segment"
	TrajectoryErrCaptureDisabled     = "capture_disabled"
	TrajectoryErrCaptureMetadataOnly = "capture_metadata_only"
	TrajectoryErrSegmentConflict     = "segment_conflict"
	TrajectoryErrSegmentExpired      = "segment_expired"
	TrajectoryErrSegmentTooLarge     = "segment_too_large"
	TrajectoryErrRunCapExceeded      = "run_cap_exceeded"
	TrajectoryErrVerificationFailed  = "segment_verification_failed"
)

// TrajectorySegmentsPrepareAgentPath returns the agent-authenticated prepare
// path: POST /api/v1/agents/{id}/trajectory/segments:prepare.
func TrajectorySegmentsPrepareAgentPath(agentID string) string {
	return fmt.Sprintf("/api/v1/agents/%s/trajectory/segments:prepare", url.PathEscape(agentID))
}

// TrajectorySegmentsCommitAgentPath returns the agent-authenticated commit
// path: POST /api/v1/agents/{id}/trajectory/segments:commit.
func TrajectorySegmentsCommitAgentPath(agentID string) string {
	return fmt.Sprintf("/api/v1/agents/%s/trajectory/segments:commit", url.PathEscape(agentID))
}

// TrajectorySegmentContentAgentPath returns the Hub-proxied upload path for
// local storage (and non-signBlob fallback):
// PUT /api/v1/agents/{id}/trajectory/segments/{segment_id}/content.
func TrajectorySegmentContentAgentPath(agentID, segmentID string) string {
	return fmt.Sprintf("/api/v1/agents/%s/trajectory/segments/%s/content",
		url.PathEscape(agentID), url.PathEscape(segmentID))
}

// TrajectoryRawObjectPath returns the Hub-chosen canonical storage object path
// for a raw trajectory segment:
//
//	{hub_id}/trajectories/v1/raw/project={project_id}/agent={agent_id}/run={run_id}/{harness}/{native_relative_path}/g{generation}-{byte_start:016d}.seg.gz
func TrajectoryRawObjectPath(hubID, projectID, agentID, runID, harness, nativeRelativePath string, generation int, byteStart int64) string {
	return fmt.Sprintf("%s/trajectories/v1/raw/project=%s/agent=%s/run=%s/%s/%s/g%d-%016d.seg.gz",
		hubID, projectID, agentID, runID, harness, nativeRelativePath, generation, byteStart)
}

// TrajectoryRawManifestPath returns the sidecar manifest path for an objectPath.
func TrajectoryRawManifestPath(objectPath string) string {
	return objectPath + ".manifest.json"
}

// ValidateTrajectoryRelativePath validates native_relative_path for a given
// harness per the P5 wire contract:
//   - non-empty, <= 1024 bytes
//   - no leading/trailing slash (relative path), no empty segments ("//"), no "." or ".." elements
//   - no backslashes, no NUL or ASCII control characters (< 0x20 or 0x7f)
//   - must match the harness allow-listed root and suffix (for "claude": prefix
//     ".claude/projects/" and suffix ".jsonl")
func ValidateTrajectoryRelativePath(harness, relPath string) error {
	if relPath == "" {
		return fmt.Errorf("native_relative_path is required")
	}
	if len(relPath) > TrajectoryMaxRelativePathBytes {
		return fmt.Errorf("native_relative_path exceeds maximum length of %d bytes", TrajectoryMaxRelativePathBytes)
	}
	if strings.HasPrefix(relPath, "/") || strings.HasSuffix(relPath, "/") {
		return fmt.Errorf("native_relative_path must be a relative path without leading or trailing slash")
	}
	if strings.ContainsRune(relPath, '\\') {
		return fmt.Errorf("native_relative_path must not contain backslashes")
	}
	for i := 0; i < len(relPath); i++ {
		b := relPath[i]
		if b < 0x20 || b == 0x7f {
			return fmt.Errorf("native_relative_path contains invalid control character")
		}
	}
	for _, elem := range strings.Split(relPath, "/") {
		if elem == "" || elem == "." || elem == ".." {
			return fmt.Errorf("native_relative_path contains invalid path element %q", elem)
		}
		if strings.HasSuffix(elem, ".seg.gz") || strings.HasSuffix(elem, ".manifest.json") {
			return fmt.Errorf("native_relative_path element %q must not end with .seg.gz or .manifest.json", elem)
		}
	}

	switch harness {
	case "claude":
		if !strings.HasPrefix(relPath, ".claude/projects/") {
			return fmt.Errorf("native_relative_path for claude must be under .claude/projects/")
		}
		if !strings.HasSuffix(relPath, ".jsonl") {
			return fmt.Errorf("native_relative_path for claude must end with .jsonl")
		}
		// Ensure there is a filename after ".claude/projects/" (not just ".claude/projects/.jsonl" is still a filename,
		// and there is at least one non-empty element after the root).
		rest := strings.TrimPrefix(relPath, ".claude/projects/")
		if rest == ".jsonl" {
			return fmt.Errorf("native_relative_path for claude must name a session file")
		}
	default:
		return fmt.Errorf("unsupported trajectory harness %q", harness)
	}
	return nil
}

// TrajectorySegmentPrepareRequest is the wire payload for
// POST /api/v1/agents/{id}/trajectory/segments:prepare.
type TrajectorySegmentPrepareRequest struct {
	Harness            string `json:"harness"`
	NativeRelativePath string `json:"native_relative_path"`
	ArtifactKind       string `json:"artifact_kind"`
	Generation         int    `json:"generation"`
	ByteStart          int64  `json:"byte_start"`
	ByteEnd            int64  `json:"byte_end"`
	RawSHA256          string `json:"raw_sha256"`
	CompressedSize     int64  `json:"compressed_size"`
	CompressedSHA256   string `json:"compressed_sha256"`
	ContentEncoding    string `json:"content_encoding"`
	IncompleteTail     bool   `json:"incomplete_tail"`
	SessionID          string `json:"session_id,omitempty"`
	OriginHubID        string `json:"origin_hub_id,omitempty"`
}

// TrajectorySegmentUploadInstruction describes how the client must PUT the
// compressed segment bytes.
type TrajectorySegmentUploadInstruction struct {
	Method    string            `json:"method"`
	URL       string            `json:"url"`
	Headers   map[string]string `json:"headers,omitempty"`
	ExpiresAt string            `json:"expires_at"`
}

// TrajectorySegmentPrepareResponse is returned by
// POST /api/v1/agents/{id}/trajectory/segments:prepare.
type TrajectorySegmentPrepareResponse struct {
	SegmentID  string                              `json:"segment_id"`
	State      string                              `json:"state"`
	ObjectPath string                              `json:"object_path"`
	Upload     *TrajectorySegmentUploadInstruction `json:"upload,omitempty"`
}

// TrajectorySegmentCommitRequest is the wire payload for
// POST /api/v1/agents/{id}/trajectory/segments:commit.
type TrajectorySegmentCommitRequest struct {
	SegmentID string `json:"segment_id"`
}

// TrajectorySegmentCommitResponse is returned by
// POST /api/v1/agents/{id}/trajectory/segments:commit.
type TrajectorySegmentCommitResponse struct {
	State        string `json:"state"`
	SegmentID    string `json:"segment_id"`
	ManifestPath string `json:"manifest_path"`
}

// TrajectorySegmentManifest is the Hub-authored immutable sidecar written at
// <object_path>.manifest.json during segments:commit.
type TrajectorySegmentManifest struct {
	SchemaVersion      string  `json:"schema_version"`
	HubID              string  `json:"hub_id"`
	OriginHubID        string  `json:"origin_hub_id,omitempty"`
	ProjectID          string  `json:"project_id"`
	AgentID            string  `json:"agent_id"`
	RunID              string  `json:"run_id"`
	Harness            string  `json:"harness"`
	HarnessConfig      string  `json:"harness_config,omitempty"`
	Template           string  `json:"template,omitempty"`
	BillingUserID      *string `json:"billing_user_id,omitempty"`
	NativeRelativePath string  `json:"native_relative_path"`
	ArtifactKind       string  `json:"artifact_kind"`
	Generation         int     `json:"generation"`
	ByteStart          int64   `json:"byte_start"`
	ByteEnd            int64   `json:"byte_end"`
	RawSHA256          string  `json:"raw_sha256"`
	CompressedSize     int64   `json:"compressed_size"`
	CompressedSHA256   string  `json:"compressed_sha256"`
	ContentEncoding    string  `json:"content_encoding"`
	IncompleteTail     bool    `json:"incomplete_tail"`
	SessionID          string  `json:"session_id,omitempty"`
	ObjectPath         string  `json:"object_path"`
	CommittedAt        string  `json:"committed_at"`
	// ManifestDigest is the lowercase hex SHA-256 of the JSON encoding of this
	// struct with ManifestDigest set to "" (so "manifest_digest":"" is present).
	// Verifiers must blank the field rather than omitting it.
	ManifestDigest string `json:"manifest_digest"`
}
