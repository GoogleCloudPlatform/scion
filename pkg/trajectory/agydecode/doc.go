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

// Package agydecode implements a pure-Go, read-only SQLite + protobuf decoder
// for Antigravity (agy) conversation databases (~/.gemini/antigravity-cli/conversations/*.db).
//
// This package is the single shared decoder used by both:
//   - Track 1 live metering (in-process inside sciontool), and
//   - Track 2 offline trajectory normalization (via `sciontool internal decode-agy-db`).
//
// Design invariants:
//   - Pure Go (modernc.org/sqlite + google.golang.org/protobuf/encoding/protowire); no cgo,
//     no os/exec.
//   - Opens databases strictly read-only with a busy timeout inside a single read-only
//     transaction per DecodeFile call, tolerating an active WAL writer.
//   - Paginates gen_metadata one row per query by rowid so multi-MB blobs stay bounded in
//     memory, and supports incremental cursor resumption across polls. Cursor advances its
//     high-water rowid past NULL/empty-blob rows and tracks up to MaxPendingRowIDs (256,
//     newest-kept) pending gap rowids per table to re-check on subsequent polls.
//   - Cursor without Dedupe is supported only for single-table row-level resumption: each
//     table row is decoded once, but overlapping copies across steps, gen_metadata, and
//     retry arrays are not deduplicated across polls. Incremental callers that need
//     cross-poll deduplication, delta merging, and learned model persistence should pass
//     a persistent DedupeState alongside Cursor.
//   - Maps token counts to Scion's canonical contract (pkg/telemetrycontract):
//     input is exclusive of cache_read/cache_write, output is inclusive of reasoning,
//     and unknown/unreported fields are omitted (nil) rather than fabricated as 0.
//     Note: proto3 omits zero-valued scalars on the wire, so a measured 0 (for example
//     cache_read = 0) decodes as nil/omitted; this is safe under the three-state rule
//     (never fabricating values), and Track 1 measurement_status logic should not treat
//     omitted optional channels as partial coverage.
//   - Deduplicates per-call usage across gen_metadata, retry arrays, and steps using
//     union-find over protobuf identity fields [11, 12, 7], with serializable state so
//     live tailers can persist seen identities and learned model_id->model_name mappings
//     across restarts. Across incremental polls, DedupeState tracks the per-channel
//     high-water mark for each canonical identity and emits only positive per-channel
//     deltas (matching single-pass max-merge semantics so incremental Track 1 totals equal
//     single-pass Track 2 totals), with a deterministic replay-safe Record.EmissionKey
//     derived from the canonical identity and cumulative high-water mark.
//   - Pins verified agy versions: unknown versions yield StatusUnverified and wire errors
//     yield StatusMalformed; guessed or corrupt tokens are never returned as StatusVerified.
//     DedupeState is only consulted and updated when the agy version is verified.
//
// Observed agy 1.5.18 storage behavior:
//   - gen_metadata rows are appended as generations complete (idx 0, 1, 2, ...).
//   - steps contains permanently-NULL metadata rows (e.g. step_type=1 user inputs) and
//     LLM steps (step_type=15) whose metadata transitions NULL -> populated. In-place
//     updates to already-populated rows were not observed on agy 1.5.18; if a later
//     copy of the same call appears in the other table with higher counts, DedupeState
//     emits the positive per-channel delta.
//
// Reverse-engineered protobuf field layout and static model ID fallback mapping are
// informed by pingdotgg/t3code's MIT-licensed antigravityUsageReader.ts and verified
// directly against real agy conversation databases (see VerifiedVersions).
package agydecode
