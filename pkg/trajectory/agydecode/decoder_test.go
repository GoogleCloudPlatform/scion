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
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

// normalizedGoldenView strips environment-specific fields (TempDir db_path and
// filesystem mtime/size fingerprint) so golden comparisons are deterministic.
type normalizedGoldenView struct {
	Status          DecodeStatus  `json:"status"`
	AgyVersion      string        `json:"agy_version,omitempty"`
	VersionVerified bool          `json:"version_verified"`
	SessionID       string        `json:"session_id"`
	Cursor          Cursor        `json:"cursor"`
	Records         []Record      `json:"records"`
	Errors          []DecodeError `json:"errors,omitempty"`
}

func assertMatchesGolden(t *testing.T, goldenName string, res *Result) {
	t.Helper()
	view := normalizedGoldenView{
		Status:          res.Status,
		AgyVersion:      res.AgyVersion,
		VersionVerified: res.VersionVerified,
		SessionID:       res.SessionID,
		Cursor:          res.Cursor,
		Records:         res.Records,
		Errors:          res.Errors,
	}
	got, err := json.MarshalIndent(view, "", "  ")
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	got = append(got, '\n')

	goldenPath := filepath.Join("testdata", goldenName)
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatalf("write golden %s: %v", goldenPath, err)
		}
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden %s: %v", goldenPath, err)
	}
	if string(got) != string(want) {
		t.Fatalf("golden mismatch for %s:\n--- got ---\n%s\n--- want ---\n%s", goldenName, got, want)
	}
}

func TestDecodeFile_GoldenMultiCallVerified(t *testing.T) {
	dbPath := buildMultiCallFixtureDB(t)
	res, err := DecodeFile(context.Background(), dbPath, Options{AgyVersion: "1.5.18"})
	if err != nil {
		t.Fatalf("DecodeFile: %v", err)
	}
	if res.Status != StatusVerified {
		t.Fatalf("status = %q, want %q", res.Status, StatusVerified)
	}
	if len(res.Records) != 4 {
		t.Fatalf("len(records) = %d, want 4 (3 primary + 1 retry on different model)", len(res.Records))
	}
	assertMatchesGolden(t, "multi_call_verified.golden.json", res)
}

func TestDecodeFile_GoldenUnknownVersion(t *testing.T) {
	dbPath := buildMultiCallFixtureDB(t)
	res, err := DecodeFile(context.Background(), dbPath, Options{AgyVersion: "9.9.9"})
	if err != nil {
		t.Fatalf("DecodeFile: %v", err)
	}
	if res.Status != StatusUnverified {
		t.Fatalf("status = %q, want %q", res.Status, StatusUnverified)
	}
	if res.VersionVerified {
		t.Fatal("VersionVerified = true, want false")
	}
	for i, r := range res.Records {
		if r.Status != StatusUnverified {
			t.Fatalf("record[%d].Status = %q, want %q", i, r.Status, StatusUnverified)
		}
	}
	assertMatchesGolden(t, "unknown_version.golden.json", res)
}

func TestDecodeFile_GoldenMalformedBlob(t *testing.T) {
	dbPath := buildMalformedFixtureDB(t)
	res, err := DecodeFile(context.Background(), dbPath, Options{AgyVersion: "1.5.18"})
	if err != nil {
		t.Fatalf("DecodeFile: %v", err)
	}
	if res.Status != StatusMalformed {
		t.Fatalf("status = %q, want %q", res.Status, StatusMalformed)
	}
	if len(res.Errors) != 1 {
		t.Fatalf("len(errors) = %d, want 1", len(res.Errors))
	}
	foundMalformed := false
	for _, r := range res.Records {
		if r.Status == StatusMalformed {
			foundMalformed = true
			if r.Tokens != nil {
				t.Fatalf("malformed record must not carry tokens, got %+v", r.Tokens)
			}
		}
	}
	if !foundMalformed {
		t.Fatal("expected a malformed record entry")
	}
	assertMatchesGolden(t, "malformed_blob.golden.json", res)
}

// TestCanonicalTokenMapping_HandChecked verifies the exact canonical token
// contract (pkg/telemetrycontract):
//   - input is exclusive of cache_read and cache_write
//   - output is max(f3, f9+f10) and inclusive of reasoning
//   - reasoning is not double-counted in SummableTokens
//   - unreported fields remain nil (absent), never fabricated as 0
func TestCanonicalTokenMapping_HandChecked(t *testing.T) {
	t.Run("disjoint_and_reasoning_not_double_counted", func(t *testing.T) {
		// Hand-checked example from design §3.1:
		// input=8, output=5 (f3=5, f9=2, f10=3), cache_read=3, cache_write=2, reasoning=2
		// SummableTokens = 8 + 5 + 3 + 2 = 18 (NOT 20).
		raw := encodeUsagePB(pbUsageSpec{
			modelID:    333,
			hasInput:   true,
			input:      8,
			hasOutput:  true,
			output:     5,
			hasCacheRd: true,
			cacheRead:  3,
			hasCacheWr: true,
			cacheWrite: 2,
			hasReason:  true,
			reasoning:  2,
			hasNonReas: true,
			nonReason:  3,
			id11:       "hand-checked-1",
		})
		entry, err := decodeUsageMessage(raw, false)
		if err != nil {
			t.Fatalf("decodeUsageMessage: %v", err)
		}
		tok := entry.tokens
		if tok.Input == nil || *tok.Input != 8 {
			t.Fatalf("Input = %v, want 8", tok.Input)
		}
		if tok.Output == nil || *tok.Output != 5 {
			t.Fatalf("Output = %v, want 5", tok.Output)
		}
		if tok.CacheRead == nil || *tok.CacheRead != 3 {
			t.Fatalf("CacheRead = %v, want 3", tok.CacheRead)
		}
		if tok.CacheWrite == nil || *tok.CacheWrite != 2 {
			t.Fatalf("CacheWrite = %v, want 2", tok.CacheWrite)
		}
		if tok.Reasoning == nil || *tok.Reasoning != 2 {
			t.Fatalf("Reasoning = %v, want 2", tok.Reasoning)
		}
		if tok.ReasoningOverlapsOutput == nil || !*tok.ReasoningOverlapsOutput {
			t.Fatalf("ReasoningOverlapsOutput = %v, want true", tok.ReasoningOverlapsOutput)
		}
		if tok.InputDerivation != telemetrycontract.InputDerivationReported {
			t.Fatalf("InputDerivation = %q, want %q", tok.InputDerivation, telemetrycontract.InputDerivationReported)
		}
		sum, overflow := tok.SummableTokens()
		if overflow || sum != 18 {
			t.Fatalf("SummableTokens = (%d, %v), want (18, false)", sum, overflow)
		}
	})

	t.Run("output_uses_max_of_f3_and_f9_plus_f10", func(t *testing.T) {
		raw := encodeUsagePB(pbUsageSpec{
			hasInput:   true,
			input:      100,
			hasOutput:  true,
			output:     10, // smaller than f9 + f10
			hasReason:  true,
			reasoning:  12,
			hasNonReas: true,
			nonReason:  8,
		})
		entry, err := decodeUsageMessage(raw, false)
		if err != nil {
			t.Fatalf("decodeUsageMessage: %v", err)
		}
		if entry.tokens.Output == nil || *entry.tokens.Output != 20 {
			t.Fatalf("Output = %v, want 20", entry.tokens.Output)
		}
		if entry.tokens.Reasoning == nil || *entry.tokens.Reasoning != 12 {
			t.Fatalf("Reasoning = %v, want 12", entry.tokens.Reasoning)
		}
		sum, _ := entry.tokens.SummableTokens()
		if sum != 120 {
			t.Fatalf("SummableTokens = %d, want 120", sum)
		}
	})

	t.Run("unknown_fields_omitted_not_zero", func(t *testing.T) {
		raw := encodeUsagePB(pbUsageSpec{
			hasInput:  true,
			input:     42,
			hasOutput: true,
			output:    10,
		})
		entry, err := decodeUsageMessage(raw, false)
		if err != nil {
			t.Fatalf("decodeUsageMessage: %v", err)
		}
		if entry.tokens.CacheRead != nil || entry.tokens.CacheWrite != nil || entry.tokens.Reasoning != nil {
			t.Fatalf("unreported fields must be nil, got %+v", entry.tokens)
		}
		if entry.tokens.ReasoningOverlapsOutput != nil {
			t.Fatalf("ReasoningOverlapsOutput must be nil when Reasoning is nil")
		}
		b, err := json.Marshal(entry.tokens)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		for _, k := range []string{"cache_read", "cache_write", "reasoning", "reasoning_overlaps_output"} {
			if _, exists := m[k]; exists {
				t.Fatalf("JSON must omit unreported key %q, got %s", k, b)
			}
		}
	})
}

func TestDecodeFile_CursorResumeAndPersistentDedupe(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "resume.db")
	db := createAgySchemaDB(t, dbPath)
	defer func() { _ = db.Close() }()

	// Insert rowid=0 (verifies rowid=0 is not skipped when Cursor is empty).
	u0 := pbUsageSpec{
		modelID:   1466,
		hasInput:  true,
		input:     500,
		hasOutput: true,
		output:    100,
		id11:      "call-0-id",
	}
	gen0 := encodeGenMetadataPB(pbGenMetaSpec{
		modelID:   1466,
		modelName: "gemini-4-neon",
		tsSecs:    1795701000,
		primary:   &u0,
	})
	if _, err := db.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (0, ?, ?)`, gen0, len(gen0)); err != nil {
		t.Fatalf("insert gen0: %v", err)
	}

	dedupe := NewDedupeState()
	res1, err := DecodeFile(context.Background(), dbPath, Options{
		AgyVersion: "1.5.18",
		Dedupe:     dedupe,
	})
	if err != nil {
		t.Fatalf("first DecodeFile: %v", err)
	}
	if len(res1.Records) != 1 || res1.Records[0].RowID != 0 {
		t.Fatalf("first decode got %d records (%+v), want 1 record at rowid=0", len(res1.Records), res1.Records)
	}
	if res1.Cursor.GenMetadataRowID == nil || *res1.Cursor.GenMetadataRowID != 0 {
		t.Fatalf("cursor.GenMetadataRowID = %v, want 0", res1.Cursor.GenMetadataRowID)
	}

	// Persist DedupeState and Cursor through JSON to simulate process restart.
	savedDedupeJSON, err := json.Marshal(dedupe)
	if err != nil {
		t.Fatalf("marshal dedupe: %v", err)
	}
	savedCursorJSON, err := json.Marshal(res1.Cursor)
	if err != nil {
		t.Fatalf("marshal cursor: %v", err)
	}

	// Now append:
	//  - a step duplicating call-0-id (must be suppressed by restored DedupeState!)
	//  - a new gen_metadata rowid=1 with call-1-id (must be emitted!)
	stepDup := encodeStepMetadataPB(pbStepMetaSpec{
		endSecs: 1795701002,
		primary: &u0,
	})
	if _, err := db.Exec(`INSERT INTO steps(idx, step_type, metadata) VALUES (1, 15, ?)`, stepDup); err != nil {
		t.Fatalf("insert dup step: %v", err)
	}
	u1 := pbUsageSpec{
		modelID:   1466,
		hasInput:  true,
		input:     700,
		hasOutput: true,
		output:    150,
		id11:      "call-1-id",
	}
	gen1 := encodeGenMetadataPB(pbGenMetaSpec{
		modelID:   1466,
		modelName: "gemini-4-neon",
		tsSecs:    1795701010,
		primary:   &u1,
	})
	if _, err := db.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (1, ?, ?)`, gen1, len(gen1)); err != nil {
		t.Fatalf("insert gen1: %v", err)
	}

	var restoredDedupe DedupeState
	if err := json.Unmarshal(savedDedupeJSON, &restoredDedupe); err != nil {
		t.Fatalf("unmarshal dedupe: %v", err)
	}
	var restoredCursor Cursor
	if err := json.Unmarshal(savedCursorJSON, &restoredCursor); err != nil {
		t.Fatalf("unmarshal cursor: %v", err)
	}

	res2, err := DecodeFile(context.Background(), dbPath, Options{
		AgyVersion: "1.5.18",
		Cursor:     restoredCursor,
		Dedupe:     &restoredDedupe,
	})
	if err != nil {
		t.Fatalf("second DecodeFile: %v", err)
	}
	if len(res2.Records) != 1 {
		t.Fatalf("second decode got %d records (%+v), want 1 (only call-1-id)", len(res2.Records), res2.Records)
	}
	if res2.Records[0].Identity != "antigravity:11:call-1-id" {
		t.Fatalf("got identity %q, want antigravity:11:call-1-id", res2.Records[0].Identity)
	}
	if res2.Cursor.GenMetadataRowID == nil || *res2.Cursor.GenMetadataRowID != 1 {
		t.Fatalf("cursor.GenMetadataRowID = %v, want 1", res2.Cursor.GenMetadataRowID)
	}
	if res2.Cursor.StepsRowID == nil || *res2.Cursor.StepsRowID != 1 {
		t.Fatalf("cursor.StepsRowID = %v, want 1", res2.Cursor.StepsRowID)
	}
}

func TestDecodeFile_WALConcurrentWriterAndFingerprint(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "wal-live.db")
	writer, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	defer func() { _ = writer.Close() }()
	writer.SetMaxOpenConns(1)

	if _, err := writer.Exec(`PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0;`); err != nil {
		t.Fatalf("enable WAL: %v", err)
	}
	if _, err := writer.Exec(`CREATE TABLE gen_metadata (idx INTEGER PRIMARY KEY, data BLOB, size INTEGER NOT NULL DEFAULT 0)`); err != nil {
		t.Fatalf("create table: %v", err)
	}

	fpBefore, err := Fingerprint(dbPath)
	if err != nil {
		t.Fatalf("Fingerprint before: %v", err)
	}
	if !fpBefore.Equal(fpBefore) {
		t.Fatal("fingerprint must equal itself")
	}

	// Write a committed row into the WAL while keeping writer open and uncheckpointed.
	gen0 := encodeGenMetadataPB(pbGenMetaSpec{
		modelID:   1466,
		modelName: "gemini-4-neon",
		tsSecs:    1795702000,
		primary: &pbUsageSpec{
			modelID:   1466,
			hasInput:  true,
			input:     321,
			hasOutput: true,
			output:    65,
			id11:      "wal-call-1",
		},
	})
	if _, err := writer.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (0, ?, ?)`, gen0, len(gen0)); err != nil {
		t.Fatalf("insert into WAL: %v", err)
	}

	fpAfter, err := Fingerprint(dbPath)
	if err != nil {
		t.Fatalf("Fingerprint after: %v", err)
	}
	if !fpAfter.WALPresent || fpAfter.WALSize == 0 {
		t.Fatalf("expected non-empty WAL in fingerprint, got %+v", fpAfter)
	}
	if fpBefore.Equal(fpAfter) {
		t.Fatalf("expected fingerprint to change after WAL write: before=%+v after=%+v", fpBefore, fpAfter)
	}

	res, err := DecodeFile(context.Background(), dbPath, Options{AgyVersion: "1.5.18"})
	if err != nil {
		t.Fatalf("DecodeFile on live WAL DB: %v", err)
	}
	if res.Status != StatusVerified {
		t.Fatalf("status = %q, want verified", res.Status)
	}
	if len(res.Records) != 1 || res.Records[0].Identity != "antigravity:11:wal-call-1" {
		t.Fatalf("unexpected records from WAL DB: %+v", res.Records)
	}
}

func TestVersionPinningAndNormalization(t *testing.T) {
	if !IsVerifiedVersion("1.5.18") {
		t.Fatal("1.5.18 must be verified")
	}
	if !IsVerifiedVersion("v1.5.18\n") {
		t.Fatal("v1.5.18 must normalize and be verified")
	}
	if !IsVerifiedVersion("agy 1.5.18 (linux/amd64)") {
		t.Fatal("prefixed version must normalize and be verified")
	}
	if IsVerifiedVersion("") {
		t.Fatal("empty version must not be verified")
	}
	if IsVerifiedVersion("9.9.9") {
		t.Fatal("unknown version must not be verified")
	}
}

func TestNormalizeModelName(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"Gemini 4 Neon (Medium)", "gemini-4-neon-medium"},
		{"gemini-4-neon", "gemini-4-neon"},
		{"Claude 4.5 Sonnet", "claude-sonnet-4-5"},
		{"Claude 4.6 Opus", "claude-opus-4-6"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := NormalizeModelName(tc.in); got != tc.want {
			t.Errorf("NormalizeModelName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
