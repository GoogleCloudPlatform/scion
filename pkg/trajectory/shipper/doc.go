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
// # State File Contract (Shared with Runtime Broker Harvest and Normalizers)
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
// Each artifact entry is logically keyed by (native_relative_path, inode,
// generation) and records:
//   - inode / device (and human-readable file_id "dev:ino"): stored separately.
//     Rotation detection compares inode only, because a Runtime Broker reading
//     the same agent home over virtiofs or NFS may see a different st_dev than
//     the container. The 64-byte FNV-1a guard hash catches content rewrites
//     even when an inode number is reused.
//   - raw_committed_offset: byte offset durably committed to the Hub; advanced
//     only after Uploader.Ship returns OutcomeCommitted.
//   - parsed_offset: byte offset consumed by the Track 2 normalizer. This
//     package maintains the invariant parsed_offset <= raw_committed_offset
//     within a generation: at baseline parsed_offset is initialized to
//     baseline_offset (so pre-enablement history is never normalized), on
//     normal commits parsed_offset is preserved untouched (advanced only by
//     the normalizer), and on any generation bump parsed_offset resets to 0
//     alongside raw_committed_offset.
//   - guard_hash / guard_len: hex-encoded 64-bit FNV-1a hash of the up to 64
//     bytes immediately preceding raw_committed_offset, computed from the
//     committed byte buffer (plus up to 64 preceding bytes).
//   - pending: optional in-flight segment record (byte_end, raw_sha256,
//     incomplete_tail) persisted under the lock before calling Uploader.Ship.
//     If a commit ACK is lost or the process crashes before committing state,
//     and the file continues to grow, the next round replays the exact pending
//     range [raw_committed_offset, pending.byte_end) so the Hub resolves it
//     via idempotent replay rather than 409 segment_conflict.
//   - baseline / baseline_offset / needs_baseline: whether the artifact existed
//     at initial baseline, its recorded starting offset, and whether an initial
//     open/stat failure deferred baselining to the first successful open.
//   - origin_hub_id: Hub identifier recorded for relocation continuity.
//   - consecutive_failures / last_error / last_generation_reason: fault and
//     generation-rotation diagnostics.
//   - rejected / rejected_reason: terminal parking state set on OutcomeRejected
//     (e.g. 400 invalid_segment or 413 segment_too_large) so a permanently
//     invalid artifact is not retried forever.
//   - truncated / truncated_run_id: run-scoped cap state set on
//     OutcomeRunCapExceeded. Truncation stops shipping only for the matching
//     run_id; when a new run starts (different Config.RunID or ResetForRun),
//     truncation clears automatically.
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
// # Safe Artifact Opening and Containment
//
// Discovery requires every artifact and any symlink target to remain inside the
// matched harness rule root (for example .claude/projects/), not merely inside
// the agent home. Opening walks components relative to the resolved rule root
// with openat(2) and O_NOFOLLOW (O_DIRECTORY on intermediate directories,
// O_NONBLOCK on the leaf) and verifies the opened descriptor is a regular file,
// preventing TOCTOU symlink swaps.
//
// # Generation Bumps
//
// Before planning a segment for an artifact with raw_committed_offset > 0, the
// shipper verifies:
//  1. Current file size >= raw_committed_offset (else reason "truncated").
//  2. Current inode matches stored inode (else reason "inode_changed").
//  3. The 64-byte FNV-1a guard hash immediately before raw_committed_offset
//     matches stored guard_hash (else reason "guard_mismatch").
//
// Any mismatch—or an OutcomeConflict response from Uploader.Ship—increments
// generation, resets raw_committed_offset and parsed_offset to 0, clears any
// pending segment and guard hash, and records the reason. Existing committed
// segments in prior generations are never overwritten.
//
// Note on pre-baseline bytes: the pending-segment record prevents ordinary
// lost-ACK + file-growth retries from causing a generation bump. However, a
// genuine truncation, inode replacement, guard-hash mismatch (in-place rewrite
// before the committed offset), or Hub 409 conflict on a baselined file
// intentionally restarts the new generation at offset 0 so the generation is
// self-contained; in those fault cases, bytes before the original baseline
// offset can be included in the new generation.
package shipper
