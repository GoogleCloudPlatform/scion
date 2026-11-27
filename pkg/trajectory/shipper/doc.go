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
// Both sciontool (inside the container, including root init and agent-UID
// hooks) and the Runtime Broker (post-exit and pre-delete harvest) read and
// update this file. On Unix, the state directory, lock file, temp files, and
// state.json are all opened, created, verified, and atomically replaced
// (temp file + fchmod 0600 + fsync + renameat) through pinned openat(2)
// O_NOFOLLOW directory descriptors rooted at homeDir, eliminating path-based
// TOCTOU symlink redirection. On non-Unix platforms, state, lock, file-identity,
// and artifact-open operations fail closed with errors.ErrUnsupported.
//
// Single-owner cross-UID contract: the home directory's owner UID is the single
// canonical owner of the shipper state tree. When called as root (euid == 0),
// every directory and file created by the shipper (trajectory-shipper/,
// state.lock, temp files, and state.json) is chowned on its pinned descriptor
// (fchown) to the home directory's owner. Root readers accept directories, lock
// files, and state.json owned by either root (0) or the home owner; non-root
// readers accept only their own euid. This allows root sciontool init,
// agent-UID hook processes, and P6 broker harvest to share the same state and
// lock without locking each other out.
//
// Root-squashed filesystems (for example NFS exports with root_squash): when
// euid == 0 and fchown, mkdirat, or openat is refused with EPERM/EACCES, the
// shipper best-effort removes any directory it just created and fails closed
// with [ErrRootSquashChownRefused]. On root-squashed homes, callers must run
// the shipper as the home directory's owner instead of root.
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
//     the normalizer via [WithLockedState]), and on any generation bump
//     parsed_offset resets to the new generation's starting offset
//     (baseline_offset when pre-baseline bytes are preserved, or 0 otherwise).
//   - guard_hash / guard_len: hex-encoded 64-bit FNV-1a hash of the up to 64
//     bytes immediately preceding raw_committed_offset, computed from the
//     committed byte buffer (plus up to 64 preceding bytes).
//   - pending: optional in-flight segment record (byte_end, raw_sha256,
//     incomplete_tail) persisted under the lock before calling Uploader.Ship.
//     If a commit ACK is lost or the process crashes before committing state,
//     and the file continues to grow, the next round replays the exact pending
//     range [raw_committed_offset, pending.byte_end) so the Hub resolves it
//     via idempotent replay rather than 409 segment_conflict. Pending replays
//     strictly respect the round's MaxBytes budget: if a pending segment
//     exceeds the remaining budget, it is skipped with BudgetExhausted=true.
//   - baseline / baseline_offset / baseline_guard_hash / baseline_guard_len /
//     needs_baseline: whether the artifact existed at initial baseline, its
//     recorded starting offset and 64-byte FNV-1a guard at that offset, and
//     whether an initial open/stat failure deferred baselining to the first
//     successful open.
//   - origin_hub_id: Hub identifier recorded for relocation continuity.
//   - consecutive_failures / last_error / last_generation_reason: fault and
//     generation-rotation diagnostics.
//   - rejected / rejected_reason / rejected_run_id: terminal parking state set
//     on OutcomeRejected (e.g. 400 invalid_segment or 413 segment_too_large) so
//     a permanently invalid artifact is not retried forever. Cleared
//     automatically on a generation bump or when a new run_id starts.
//   - deferred_baseline_prefixes (top-level): home-relative prefixes that
//     discovery could not read or refused at baseline (other than ENOENT). Any
//     untracked artifact later discovered under one of these prefixes is
//     lazily baselined at EOF on first open rather than shipped from offset 0.
//   - truncated / truncated_run_id: run-scoped cap state set on
//     OutcomeRunCapExceeded. Config.RunID is required for RunRound and
//     ResumeCatchup. Truncation stops shipping only for the matching run_id;
//     when a new run starts (different Config.RunID or ResetForRun),
//     truncation clears automatically.
//
// External consumers (such as P7 normalizers advancing parsed_offset or P6
// broker harvest) should use [WithLockedState] to perform locked
// read-modify-write updates against state.json. Because P7 runs normalization
// outside the lock, its [WithLockedState] callback MUST compare both
// generation and inode against the snapshot it normalized—and verify
// parsed_offset <= raw_committed_offset—before writing the updated
// parsed_offset, so a concurrent generation bump cannot be overwritten by a
// stale parse result.
//
// # Segment Framing and incomplete_tail
//
// Non-final rounds plan newline-aligned segments [raw_committed_offset,
// last_newline+1) capped at min(MaxSegmentBytes, roundMaxBytes). The
// Segment.IncompleteTail flag is set in two cases:
//   - On a Final round (RoundOptions.Final=true), when the trailing bytes at
//     EOF do not end with '\n'.
//   - On a non-final round, when a single line exceeds the effective segment
//     cap and is split mid-line at the cap boundary.
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
// The exclusive lock is held across [Uploader.Ship] (network I/O) for the full
// duration of a [Shipper.RunRound] call (bounded by MaxRoundBytes and the
// caller's context), so other lock users (hook processes, P6 broker harvest, P7
// normalizers using [WithLockedState]) must size LockTimeout and context
// deadlines accordingly.
//
// # Structural Invariants (Append-Only Generations & Write-Once Baseline)
//
// Two structural rules enforce byte-verbatim contiguity and pre-baseline privacy:
//  1. Generations are append-only: RawCommittedOffset may advance within a
//     generation only through commitSegmentLocked for a committed segment whose
//     byte_start equals the current RawCommittedOffset. Every other position
//     change (baseline, Rebaseline, lazy baseline, skip-to-next-newline,
//     truncation/rotation/conflict bumps, and probe-accept) goes through
//     startGenerationLocked, which increments Generation unless the current
//     generation is provably empty (no commits, no pending, and
//     GenerationStartOffset == RawCommittedOffset), sets GenerationStartOffset,
//     RawCommittedOffset, and ParsedOffset together, and clears Pending.
//  2. The run baseline is write-once: BaselineSet, BaselineOffset, and
//     BaselineGuard* are recorded at most once per artifact via
//     setBaselineOnceLocked (initial baseline, first untracked creation, or
//     first lazy baseline when none exists) and are never overwritten
//     regardless of NeedsBaseline. Refusal-window positions live separately in
//     RebaselineOffset and RebaselineGuard*.
//
// # Policy Refusal Rebaselining
//
// When the Hub refuses capture (404 or 403 capture_disabled /
// capture_metadata_only), callers should invoke [Shipper.Rebaseline] to move
// every discovered artifact's raw_committed_offset and parsed_offset to EOF
// (preserving the original baseline_offset / baseline_guard_hash for
// pre-baseline privacy, clearing any pending segments, bumping generation if
// the current generation already had committed or pending bytes past its start,
// and persisting RefusedPendingRebaseline=true) under the lock, then back off.
// If EOF falls mid-line (the preceding byte is not '\n'), skip_to_next_newline
// is set on the artifact (and likewise on lazy baseline completion) so the next
// shipping round skips forward past the remainder of that partial line without
// shipping it (bounded by MaxSegmentBytes) before emitting new complete lines.
//
// When RefusedPendingRebaseline is true and the Uploader implements [Prober],
// the next round probes segments:prepare without uploading payload bytes or
// committing; once the Hub accepts capture again, the shipper rebaselines past
// everything written during the refusal/backoff window and clears the flag.
// Note that a probe inserts a pending row on the Hub reserving up to one
// segment (<= 16 MiB) of run-cap headroom until the prepare TTL expires
// (15 min); any subsequent identity collision (for example when the
// post-rebaseline offset equals the probe's byte_start) safely hits the Hub's
// pending-row refresh path.
//
// # Safe Artifact Opening and Containment
//
// Discovery refuses every symlink at or below a matched harness rule root
// (for example .claude/projects/), regardless of its target. Opening opens the
// trusted resolved home directory once with O_DIRECTORY and walks every
// component below it (rule-root ancestors, intermediate directories, and leaf)
// with openat(2) and O_NOFOLLOW (O_DIRECTORY on directories, O_NONBLOCK on the
// leaf), then verifies the opened descriptor is a regular file via fstat. When
// running as root (euid == 0), regular files with Nlink > 1 (hard links) are
// also refused with [ErrHardLinkRefused]. This prevents TOCTOU symlink swaps
// and hard-link aliasing at any component between the home directory and leaf.
//
// # Generation Bumps and Pre-Baseline Privacy
//
// Before planning a segment for an artifact with raw_committed_offset > 0, the
// shipper verifies:
//  1. Current file size >= raw_committed_offset (else reason "truncated").
//  2. Current inode matches stored inode (else reason "inode_changed").
//  3. The 64-byte FNV-1a guard hash immediately before raw_committed_offset
//     matches stored guard_hash (else reason "guard_mismatch").
//
// Any mismatch—or an OutcomeConflict response from Uploader.Ship—increments
// generation, clears any pending segment, and records the reason. Existing
// committed segments in prior generations are never overwritten.
//
// For a baselined artifact (baseline_offset > 0), pre-baseline privacy takes
// precedence over restarting at offset 0: if the current file is at least
// baseline_offset bytes long and its 64-byte FNV-1a guard at baseline_offset
// still matches baseline_guard_hash (e.g. a Hub 409 conflict or a
// write-temp+rename rotation that kept pre-baseline history), the new
// generation starts at baseline_offset (both raw_committed_offset and
// parsed_offset set to baseline_offset, with guard_hash recomputed there).
// Only when the file is shorter than baseline_offset or its pre-baseline bytes
// genuinely changed (baseline guard mismatch) does the new generation restart
// at offset 0.
//
// # P6 Broker Harvest Prerequisites & Threat Model Notes
//
// The shipper's privacy invariants (baseline exclusion and refusal-window
// exclusion) protect against accidental capture on the honest path; they are
// not a security boundary against a malicious agent holding its own agent token
// (the Hub's policy check in segments:prepare is the enforcement boundary).
// Before reusing this package from a host Runtime Broker in P6, note:
//   - flock coherence across the host/container boundary: virtiofs (depending
//     on virtiofsd -o posix_lock/flock) and some NFS mounts do not guarantee
//     coherent flock(2) between guest and host. P6 must verify lock coherence
//     or add a lease file + generation counter.
//   - Prefer non-root broker harvest: running broker harvest as the home
//     owner's UID avoids all root-owned-file risks on host filesystems.
//   - UID mapping: idmapped or user-namespaced mounts can cause the host view
//     of homeUID to differ from the container UID, causing ownership checks to
//     fail closed on one side; P6 must account for mapped UIDs.
//   - Treat state.json as hostile agent-controlled input: reads are bounded by
//     MaxStateFileBytes (16 MiB), filesystem paths must always come from live
//     Discover (never from state.json keys), and foreign origin_hub_id values
//     must be validated or ignored on the Hub.
//   - Inode-only rotation identity: rotation detection compares st_ino only
//     (ignoring st_dev because virtiofs/NFS may report a different device ID
//     to a host broker). On NFS, a reused inode after delete-and-recreate can
//     pass as the same file if the 64-byte trailing guard hash also matches.
package shipper
