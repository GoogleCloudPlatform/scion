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

package agydecode

import (
	"math"
	"time"
)

// DecodeStatus reports the verification state of a decoded file or record.
// Callers must never treat StatusUnverified or StatusMalformed records as
// verified metering truth.
type DecodeStatus string

const (
	// StatusVerified indicates a known, pinned agy version and a clean wire decode.
	StatusVerified DecodeStatus = "verified"
	// StatusUnverified indicates a clean wire decode from an unknown or unpinned agy version.
	StatusUnverified DecodeStatus = "unverified"
	// StatusMalformed indicates a wire/schema decode error or out-of-range token value.
	StatusMalformed DecodeStatus = "malformed"
)

// SourceTable identifies which SQLite table a decoded usage entry originated from.
type SourceTable string

const (
	SourceGenMetadata SourceTable = "gen_metadata"
	SourceSteps       SourceTable = "steps"
)

// TokenUsage holds canonical token counts aligned with pkg/telemetrycontract:
//   - Input is exclusive of CacheRead and CacheWrite.
//   - Output is inclusive of Reasoning (computed as max(f3, f9+f10)).
//   - Unknown / unreported fields are nil, never fabricated as 0.
type TokenUsage struct {
	Input                   *int64 `json:"input,omitempty"`
	Output                  *int64 `json:"output,omitempty"`
	CacheRead               *int64 `json:"cache_read,omitempty"`
	CacheWrite              *int64 `json:"cache_write,omitempty"`
	Reasoning               *int64 `json:"reasoning,omitempty"`
	ReasoningOverlapsOutput *bool  `json:"reasoning_overlaps_output,omitempty"`
	InputDerivation         string `json:"input_derivation,omitempty"`
}

// SummableTokens returns input + output + cache_read + cache_write, treating
// nil fields as 0. Reasoning is an informational subset of Output and is never
// added again. Returns (0, true) on int64 overflow.
func (t *TokenUsage) SummableTokens() (int64, bool) {
	if t == nil {
		return 0, false
	}
	var total int64
	for _, p := range []*int64{t.Input, t.Output, t.CacheRead, t.CacheWrite} {
		if p == nil || *p <= 0 {
			continue
		}
		if total > math.MaxInt64-*p {
			return 0, true
		}
		total += *p
	}
	return total, false
}

// Record represents one deduplicated LLM call decoded from an agy conversation DB.
type Record struct {
	// Status is StatusVerified, StatusUnverified, or StatusMalformed.
	Status DecodeStatus `json:"status"`
	// SessionID is the conversation identifier (database filename stem without .db).
	SessionID string `json:"session_id"`
	// Source identifies the originating table (gen_metadata or steps).
	Source SourceTable `json:"source"`
	// RowID is the SQLite rowid of the originating row.
	RowID int64 `json:"rowid"`
	// Idx is the table's idx column for the originating row.
	Idx int64 `json:"idx"`
	// UsageIndex is 0 for the primary usage entry and >= 1 for retry array entries.
	UsageIndex int `json:"usage_index"`
	// IsRetry is true when this entry came from a retry array (gen_metadata f17 / steps f28).
	IsRetry bool `json:"is_retry,omitempty"`
	// IsDelta is true when this record carries only a positive per-channel token
	// increment for a call identity already partially emitted in an earlier poll.
	// Live tailers should emit tokens (calls=0) for delta records so calls are
	// never double-counted.
	IsDelta bool `json:"is_delta,omitempty"`

	// Identity is the stable canonical dedupe key for this call.
	Identity string `json:"identity"`
	// SourceIdentity is telemetrycontract.SourceIdentityRequestID when derived
	// from protobuf IDs [11, 12, 7], or telemetrycontract.SourceIdentityComposite
	// when falling back to positional identity.
	SourceIdentity string `json:"source_identity"`
	// IdentityKeys lists all raw protobuf identity keys ("antigravity:<field>:<id>")
	// associated with this call after union-find merging.
	IdentityKeys []string `json:"identity_keys,omitempty"`

	// Timestamp is the call timestamp (UTC) when available in the database.
	Timestamp *time.Time `json:"timestamp,omitempty"`

	// Model is the resolved canonical model name (e.g. "gemini-4-neon", "claude-sonnet-4-5").
	Model string `json:"model"`
	// ModelID is the numeric protobuf model identifier when present.
	ModelID int64 `json:"model_id,omitempty"`
	// ModelSource is telemetrycontract.ModelSourceEvent or telemetrycontract.ModelSourceUnknown.
	ModelSource string `json:"model_source"`

	// Tokens holds the canonical token counts. Nil when Status == StatusMalformed.
	Tokens *TokenUsage `json:"tokens,omitempty"`
	// Error describes the decode failure when Status == StatusMalformed.
	Error string `json:"error,omitempty"`
}

// DecodeError captures a non-fatal row or blob decode error so callers can
// surface schema drift without crashing on a single corrupt row.
type DecodeError struct {
	Source SourceTable `json:"source,omitempty"`
	RowID  int64       `json:"rowid"`
	Idx    int64       `json:"idx"`
	Error  string      `json:"error"`
}

// Cursor tracks the highest SQLite rowid processed per table so callers can
// resume incrementally on subsequent polls. Nil pointers mean "start from the
// beginning" (important because agy's idx/rowid starts at 0).
type Cursor struct {
	GenMetadataRowID *int64 `json:"gen_metadata_rowid,omitempty"`
	StepsRowID       *int64 `json:"steps_rowid,omitempty"`
}

// Clone returns a deep copy of the cursor.
func (c Cursor) Clone() Cursor {
	var out Cursor
	if c.GenMetadataRowID != nil {
		v := *c.GenMetadataRowID
		out.GenMetadataRowID = &v
	}
	if c.StepsRowID != nil {
		v := *c.StepsRowID
		out.StepsRowID = &v
	}
	return out
}

// FileFingerprint captures the (db mtime, size, wal mtime, size) tuple so
// callers can skip decoding unchanged conversation databases.
type FileFingerprint struct {
	DBSize       int64 `json:"db_size"`
	DBModTimeNS  int64 `json:"db_mtime_ns"`
	WALPresent   bool  `json:"wal_present"`
	WALSize      int64 `json:"wal_size,omitempty"`
	WALModTimeNS int64 `json:"wal_mtime_ns,omitempty"`
}

// Equal reports whether two fingerprints represent the same on-disk state.
func (f FileFingerprint) Equal(other FileFingerprint) bool {
	return f == other
}

// Options configures a DecodeFile invocation.
type Options struct {
	// AgyVersion is the installed agy binary version (e.g. "1.5.18").
	// If empty or not in VerifiedVersions, decoded records are marked StatusUnverified.
	AgyVersion string
	// Cursor resumes reading strictly after the given rowids.
	Cursor Cursor
	// Dedupe is an optional persistent union-find + per-channel high-water-mark
	// state shared across polls/restarts. When non-nil AND AgyVersion is verified,
	// DecodeFile updates it in place, suppresses duplicates whose channels did not
	// increase, and emits only positive per-channel deltas (IsDelta=true) when a
	// later copy carries higher counts. When AgyVersion is unverified, Dedupe is
	// neither consulted nor mutated so unverified decodes cannot poison state.
	Dedupe *DedupeState
	// SkipSteps skips scanning the steps table when true (gen_metadata only).
	SkipSteps bool
	// BusyTimeoutMs overrides the SQLite busy_timeout pragma (default 2000ms).
	BusyTimeoutMs int
}

// Result is the structured output of DecodeFile and `sciontool internal decode-agy-db`.
type Result struct {
	// Status is the aggregate file status:
	//   - StatusMalformed if any wire/schema error occurred,
	//   - StatusUnverified if clean decode but AgyVersion is unknown/unpinned,
	//   - StatusVerified if clean decode and AgyVersion is in VerifiedVersions.
	Status DecodeStatus `json:"status"`
	// AgyVersion is the normalized agy version used for pinning (empty if unknown).
	AgyVersion string `json:"agy_version,omitempty"`
	// VersionVerified is true iff AgyVersion is in VerifiedVersions.
	VersionVerified bool `json:"version_verified"`
	// DBPath is the path that was decoded.
	DBPath string `json:"db_path"`
	// SessionID is the conversation identifier derived from the DB filename.
	SessionID string `json:"session_id"`
	// Fingerprint is the DB+WAL change fingerprint observed before opening.
	Fingerprint FileFingerprint `json:"fingerprint"`
	// Cursor holds the highest rowids read during this call.
	Cursor Cursor `json:"cursor"`
	// Records contains the deduplicated usage records in deterministic order.
	Records []Record `json:"records"`
	// Errors contains any row-level wire/schema decode errors encountered.
	Errors []DecodeError `json:"errors,omitempty"`
	// Warnings contains non-fatal diagnostics (such as a corrupt optional
	// trajectory_metadata_blob fallback timestamp) that do not invalidate
	// decoded token records or mark the file StatusMalformed.
	Warnings []string `json:"warnings,omitempty"`
}
