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
//   - Pure Go (modernc.org/sqlite + google.golang.org/protobuf/encoding/protowire); no cgo.
//   - Opens databases strictly read-only with a busy timeout, tolerating an active WAL writer.
//   - Paginates gen_metadata one row per query by rowid so multi-MB blobs stay bounded in memory,
//     and supports incremental cursor resumption across polls.
//   - Maps token counts to Scion's canonical contract (pkg/telemetrycontract):
//     input is exclusive of cache_read/cache_write, output is inclusive of reasoning,
//     and unknown/unreported fields are omitted (nil) rather than fabricated as 0.
//   - Deduplicates per-call usage across gen_metadata, retry arrays, and steps using
//     union-find over protobuf identity fields [11, 12, 7], with serializable state so
//     live tailers can persist seen identities across restarts.
//   - Pins verified agy versions: unknown versions yield StatusUnverified and wire errors
//     yield StatusMalformed; guessed or corrupt tokens are never returned as StatusVerified.
//
// Reverse-engineered protobuf field layout and static model ID fallback mapping are
// informed by pingdotgg/t3code's MIT-licensed antigravityUsageReader.ts and verified
// directly against real agy conversation databases (see VerifiedVersions).
package agydecode
