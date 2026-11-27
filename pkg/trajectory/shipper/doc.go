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

// Package shipper implements byte-verbatim incremental capture and durable
// watermark tracking for native harness trajectory artifacts (Track 2 L0 raw).
//
// # Scope and Dependencies
//
// This package is a pure library with no imports of sciontool or pkg/hub.
// Callers inject an [Uploader] implementation (for example, a Hub client
// adapter in sciontool or in the Runtime Broker during harvest).
//
// # State File Contract (Shared with Runtime Broker Harvest)
//
// Durable shipper state lives at:
//
//	<home>/.scion/trajectory-shipper/state.json
//
// Both sciontool (inside the container) and the Runtime Broker (post-exit and
// pre-delete harvest) read and update this file. The file is created and
// replaced atomically (temp file + fsync + rename) through pkg/util/privatefs
// with owner-only permissions (0600 on Unix, owner-only DACL on Windows) inside
// a private directory (0700 on Unix).
//
// Each artifact entry is logically keyed by (native_relative_path, file_id,
// generation) and records:
//   - raw_committed_offset: byte offset durably committed to the Hub; advanced
//     only after Uploader.Ship returns OutcomeCommitted.
//   - parsed_offset: byte offset consumed by the Track 2 normalizer. Preserved
//     verbatim by this package and never advanced here.
//   - guard_hash / guard_len: hex-encoded 64-bit FNV-1a hash of the up to 64
//     bytes immediately preceding raw_committed_offset.
//   - baseline / baseline_offset: whether the artifact existed at initial
//     baseline and its recorded starting offset.
//   - origin_hub_id: Hub identifier recorded for relocation continuity.
//   - consecutive_failures / last_error / last_generation_reason: fault and
//     generation-rotation diagnostics.
//   - truncated: whether shipping stopped due to a run cap.
//
// Both the top-level State object and each ArtifactState entry preserve unknown
// JSON fields across load/save cycles so newer writers (such as normalizers or
// future harvest phases) can add fields without older readers dropping them.
//
// # Exclusive Lock Contract
//
// All state reads, uploads that advance watermarks, and state writes are
// serialized by an exclusive inter-process advisory lock at:
//
//	<home>/.scion/trajectory-shipper/state.lock
//
// Locking uses pkg/util/filelock (flock(2) on Unix, LockFileEx on Windows) with
// a bounded wait timeout and context cancellation. The lock file is created on
// first use with owner-only permissions (0600) and is never unlinked, ensuring
// that sciontool and broker harvest always synchronize on the same inode.
//
// # Generation Bumps
//
// Before planning a segment for an artifact with raw_committed_offset > 0, the
// shipper verifies:
//  1. Current file size >= raw_committed_offset (else reason "truncated").
//  2. Current file identity matches stored file_id (else reason "inode_changed").
//  3. The 64-byte FNV-1a guard hash immediately before raw_committed_offset
//     matches stored guard_hash (else reason "guard_mismatch").
//
// Any mismatch—or an OutcomeConflict response from Uploader.Ship—increments
// generation, resets raw_committed_offset to 0, clears the guard hash, and
// records the reason. Existing committed segments in prior generations are
// never overwritten.
package shipper
