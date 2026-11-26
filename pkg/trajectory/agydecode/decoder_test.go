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

func TestDecodeFile_R1_NullMetadataUpdateNotSkipped(t *testing.T) {
	t.Run("steps_null_then_update", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "r1-steps.db")
		db := createAgySchemaDB(t, dbPath)
		defer func() { _ = db.Close() }()

		if _, err := db.Exec(`INSERT INTO steps(idx, step_type, metadata) VALUES (0, 15, NULL)`); err != nil {
			t.Fatalf("insert null step: %v", err)
		}

		dedupe := NewDedupeState()
		res1, err := DecodeFile(context.Background(), dbPath, Options{
			AgyVersion: "1.5.18",
			Dedupe:     dedupe,
		})
		if err != nil {
			t.Fatalf("poll 1: %v", err)
		}
		if len(res1.Records) != 0 {
			t.Fatalf("poll 1 records = %d, want 0", len(res1.Records))
		}
		if res1.Cursor.StepsRowID != nil {
			t.Fatalf("poll 1 Cursor.StepsRowID = %v, want nil (must not advance past NULL metadata)", *res1.Cursor.StepsRowID)
		}

		// Simulate agy updating the step row in place with usage metadata.
		step0 := encodeStepMetadataPB(pbStepMetaSpec{
			modelID:   1466,
			modelName: "gemini-4-neon",
			endSecs:   1795703000,
			primary: &pbUsageSpec{
				modelID:   1466,
				hasInput:  true,
				input:     250,
				hasOutput: true,
				output:    50,
				id11:      "r1-step-call",
			},
		})
		if _, err := db.Exec(`UPDATE steps SET metadata = ? WHERE idx = 0`, step0); err != nil {
			t.Fatalf("update step 0: %v", err)
		}

		res2, err := DecodeFile(context.Background(), dbPath, Options{
			AgyVersion: "1.5.18",
			Cursor:     res1.Cursor,
			Dedupe:     dedupe,
		})
		if err != nil {
			t.Fatalf("poll 2: %v", err)
		}
		if len(res2.Records) != 1 {
			t.Fatalf("poll 2 records = %d (%+v), want 1", len(res2.Records), res2.Records)
		}
		if res2.Records[0].Identity != "antigravity:11:r1-step-call" {
			t.Fatalf("identity = %q, want antigravity:11:r1-step-call", res2.Records[0].Identity)
		}
		if res2.Cursor.StepsRowID == nil || *res2.Cursor.StepsRowID != 0 {
			t.Fatalf("poll 2 Cursor.StepsRowID = %v, want 0", res2.Cursor.StepsRowID)
		}
	})

	t.Run("gen_metadata_null_then_update", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "r1-gen.db")
		db := createAgySchemaDB(t, dbPath)
		defer func() { _ = db.Close() }()

		if _, err := db.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (0, NULL, 0)`); err != nil {
			t.Fatalf("insert null gen_metadata: %v", err)
		}

		dedupe := NewDedupeState()
		res1, err := DecodeFile(context.Background(), dbPath, Options{
			AgyVersion: "1.5.18",
			Dedupe:     dedupe,
		})
		if err != nil {
			t.Fatalf("poll 1: %v", err)
		}
		if len(res1.Records) != 0 || res1.Status != StatusVerified {
			t.Fatalf("poll 1 got status=%q records=%d, want verified and 0 records", res1.Status, len(res1.Records))
		}
		if res1.Cursor.GenMetadataRowID != nil {
			t.Fatalf("poll 1 Cursor.GenMetadataRowID = %v, want nil", *res1.Cursor.GenMetadataRowID)
		}

		gen0 := encodeGenMetadataPB(pbGenMetaSpec{
			modelID:   1466,
			modelName: "gemini-4-neon",
			tsSecs:    1795703010,
			primary: &pbUsageSpec{
				modelID:   1466,
				hasInput:  true,
				input:     400,
				hasOutput: true,
				output:    80,
				id11:      "r1-gen-call",
			},
		})
		if _, err := db.Exec(`UPDATE gen_metadata SET data = ?, size = ? WHERE idx = 0`, gen0, len(gen0)); err != nil {
			t.Fatalf("update gen_metadata 0: %v", err)
		}

		res2, err := DecodeFile(context.Background(), dbPath, Options{
			AgyVersion: "1.5.18",
			Cursor:     res1.Cursor,
			Dedupe:     dedupe,
		})
		if err != nil {
			t.Fatalf("poll 2: %v", err)
		}
		if len(res2.Records) != 1 || res2.Records[0].Identity != "antigravity:11:r1-gen-call" {
			t.Fatalf("poll 2 got %+v, want 1 record for r1-gen-call", res2.Records)
		}
	})
}

func sumRecordsChannels(records []Record) (in, out, cr, cw, reas, summable int64) {
	for _, r := range records {
		if r.Tokens == nil {
			continue
		}
		if r.Tokens.Input != nil {
			in += *r.Tokens.Input
		}
		if r.Tokens.Output != nil {
			out += *r.Tokens.Output
		}
		if r.Tokens.CacheRead != nil {
			cr += *r.Tokens.CacheRead
		}
		if r.Tokens.CacheWrite != nil {
			cw += *r.Tokens.CacheWrite
		}
		if r.Tokens.Reasoning != nil {
			reas += *r.Tokens.Reasoning
		}
		s, _ := r.Tokens.SummableTokens()
		summable += s
	}
	return in, out, cr, cw, reas, summable
}

func TestDecodeFile_R2_CrossPollDeltaParity(t *testing.T) {
	t.Run("small_then_large_emits_delta_and_matches_large_total", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "r2-small-large.db")
		db := createAgySchemaDB(t, dbPath)
		defer func() { _ = db.Close() }()

		// Poll 1: partial step copy.
		step0 := encodeStepMetadataPB(pbStepMetaSpec{
			modelID:   1466,
			modelName: "gemini-4-neon",
			endSecs:   1795704000,
			primary: &pbUsageSpec{
				modelID:    1466,
				hasInput:   true,
				input:      10,
				hasOutput:  true,
				output:     5,
				hasReason:  true,
				reasoning:  2,
				hasNonReas: true,
				nonReason:  3,
				id11:       "r2-call-1",
				id12:       "r2-bridge-1",
			},
		})
		if _, err := db.Exec(`INSERT INTO steps(idx, step_type, metadata) VALUES (0, 15, ?)`, step0); err != nil {
			t.Fatalf("insert step0: %v", err)
		}

		dedupe := NewDedupeState()
		res1, err := DecodeFile(context.Background(), dbPath, Options{
			AgyVersion: "1.5.18",
			Dedupe:     dedupe,
		})
		if err != nil {
			t.Fatalf("poll 1: %v", err)
		}
		if len(res1.Records) != 1 || res1.Records[0].IsDelta {
			t.Fatalf("poll 1 got %+v, want 1 non-delta record", res1.Records)
		}

		// Round-trip DedupeState and Cursor through JSON.
		dBytes, err := json.Marshal(dedupe)
		if err != nil {
			t.Fatalf("marshal dedupe: %v", err)
		}
		var restored DedupeState
		if err := json.Unmarshal(dBytes, &restored); err != nil {
			t.Fatalf("unmarshal dedupe: %v", err)
		}

		// Poll 2: fuller gen_metadata copy sharing id12.
		gen0 := encodeGenMetadataPB(pbGenMetaSpec{
			modelID:   1466,
			modelName: "gemini-4-neon",
			tsSecs:    1795704005,
			primary: &pbUsageSpec{
				modelID:    1466,
				hasInput:   true,
				input:      1000,
				hasOutput:  true,
				output:     500,
				hasCacheRd: true,
				cacheRead:  50,
				hasReason:  true,
				reasoning:  100,
				hasNonReas: true,
				nonReason:  400,
				id12:       "r2-bridge-1",
				id7:        "r2-call-1-f7",
			},
		})
		if _, err := db.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (0, ?, ?)`, gen0, len(gen0)); err != nil {
			t.Fatalf("insert gen0: %v", err)
		}

		res2, err := DecodeFile(context.Background(), dbPath, Options{
			AgyVersion: "1.5.18",
			Cursor:     res1.Cursor,
			Dedupe:     &restored,
		})
		if err != nil {
			t.Fatalf("poll 2: %v", err)
		}
		if len(res2.Records) != 1 || !res2.Records[0].IsDelta {
			t.Fatalf("poll 2 got %+v, want 1 delta record", res2.Records)
		}
		if res2.Records[0].Identity != "antigravity:11:r2-call-1" {
			t.Fatalf("delta identity = %q, want antigravity:11:r2-call-1", res2.Records[0].Identity)
		}

		all := append(append([]Record{}, res1.Records...), res2.Records...)
		in, out, cr, cw, reas, summable := sumRecordsChannels(all)
		if in != 1000 || out != 500 || cr != 50 || cw != 0 || reas != 100 || summable != 1550 {
			t.Fatalf("cumulative = (in=%d out=%d cr=%d cw=%d reas=%d sum=%d), want (1000, 500, 50, 0, 100, 1550)",
				in, out, cr, cw, reas, summable)
		}
	})

	t.Run("large_then_small_adds_nothing", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "r2-large-small.db")
		db := createAgySchemaDB(t, dbPath)
		defer func() { _ = db.Close() }()

		gen0 := encodeGenMetadataPB(pbGenMetaSpec{
			modelID:   1466,
			modelName: "gemini-4-neon",
			tsSecs:    1795704100,
			primary: &pbUsageSpec{
				modelID:   1466,
				hasInput:  true,
				input:     1000,
				hasOutput: true,
				output:    500,
				id11:      "r2-rev-1",
			},
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
			t.Fatalf("poll 1: %v", err)
		}
		if len(res1.Records) != 1 {
			t.Fatalf("poll 1 records = %d, want 1", len(res1.Records))
		}

		// Poll 2: smaller duplicate arrives in steps.
		step0 := encodeStepMetadataPB(pbStepMetaSpec{
			modelID:   1466,
			modelName: "gemini-4-neon",
			endSecs:   1795704105,
			primary: &pbUsageSpec{
				modelID:   1466,
				hasInput:  true,
				input:     10,
				hasOutput: true,
				output:    5,
				id11:      "r2-rev-1",
			},
		})
		if _, err := db.Exec(`INSERT INTO steps(idx, step_type, metadata) VALUES (0, 15, ?)`, step0); err != nil {
			t.Fatalf("insert step0: %v", err)
		}

		res2, err := DecodeFile(context.Background(), dbPath, Options{
			AgyVersion: "1.5.18",
			Cursor:     res1.Cursor,
			Dedupe:     dedupe,
		})
		if err != nil {
			t.Fatalf("poll 2: %v", err)
		}
		if len(res2.Records) != 0 {
			t.Fatalf("poll 2 records = %d (%+v), want 0", len(res2.Records), res2.Records)
		}
	})

	t.Run("incremental_total_equals_single_pass_on_fixture", func(t *testing.T) {
		// Compare single-pass decode on the multi-call fixture DB against
		// incremental multi-poll decoding over the exact same DB as rows arrive.
		singleDB := buildMultiCallFixtureDB(t)
		singleRes, err := DecodeFile(context.Background(), singleDB, Options{AgyVersion: "1.5.18"})
		if err != nil {
			t.Fatalf("single pass: %v", err)
		}
		wantIn, wantOut, wantCR, wantCW, wantReas, wantSum := sumRecordsChannels(singleRes.Records)

		// Replay the same fixture rows in stages across 3 polls:
		// Poll 1: steps only (including partial step7 for call 3 and NULL step0)
		// Poll 2: gen_metadata idx=0,1 (including retry on claude-sonnet-4-5)
		// Poll 3: gen_metadata idx=2 (completing call 3 with larger output/reasoning)
		incDBPath := filepath.Join(t.TempDir(), "r2-incremental-parity.db")
		srcDB, err := sql.Open("sqlite", singleDB)
		if err != nil {
			t.Fatalf("open singleDB: %v", err)
		}
		defer func() { _ = srcDB.Close() }()
		dstDB := createAgySchemaDB(t, incDBPath)
		defer func() { _ = dstDB.Close() }()

		copyRows := func(table, where string) {
			t.Helper()
			var query string
			switch table {
			case "steps":
				query = "SELECT idx, step_type, metadata FROM steps " + where
			case "gen_metadata":
				query = "SELECT idx, data, size FROM gen_metadata " + where
			default:
				query = "SELECT id, data FROM trajectory_metadata_blob " + where
			}
			rows, err := srcDB.Query(query)
			if err != nil {
				t.Fatalf("query %s: %v", table, err)
			}
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				switch table {
				case "steps":
					var idx, st int64
					var blob []byte
					if err := rows.Scan(&idx, &st, &blob); err != nil {
						t.Fatalf("scan step: %v", err)
					}
					if _, err := dstDB.Exec(`INSERT INTO steps(idx, step_type, metadata) VALUES (?, ?, ?)`, idx, st, blob); err != nil {
						t.Fatalf("insert step: %v", err)
					}
				case "gen_metadata":
					var idx, sz int64
					var blob []byte
					if err := rows.Scan(&idx, &blob, &sz); err != nil {
						t.Fatalf("scan gen: %v", err)
					}
					if _, err := dstDB.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (?, ?, ?)`, idx, blob, sz); err != nil {
						t.Fatalf("insert gen: %v", err)
					}
				default:
					var id string
					var blob []byte
					if err := rows.Scan(&id, &blob); err != nil {
						t.Fatalf("scan tmb: %v", err)
					}
					if _, err := dstDB.Exec(`INSERT INTO trajectory_metadata_blob(id, data) VALUES (?, ?)`, id, blob); err != nil {
						t.Fatalf("insert tmb: %v", err)
					}
				}
			}
		}

		dedupe := NewDedupeState()
		var cur Cursor
		var collected []Record

		poll := func(stage string) {
			t.Helper()
			res, err := DecodeFile(context.Background(), incDBPath, Options{
				AgyVersion: "1.5.18",
				Cursor:     cur,
				Dedupe:     dedupe,
			})
			if err != nil {
				t.Fatalf("%s: %v", stage, err)
			}
			cur = res.Cursor
			collected = append(collected, res.Records...)
		}

		copyRows("trajectory_metadata_blob", "")
		copyRows("steps", "")
		poll("poll 1 (steps)")

		copyRows("gen_metadata", "WHERE idx IN (0, 1)")
		poll("poll 2 (gen 0,1)")

		copyRows("gen_metadata", "WHERE idx = 2")
		poll("poll 3 (gen 2)")

		gotIn, gotOut, gotCR, gotCW, gotReas, gotSum := sumRecordsChannels(collected)
		if gotIn != wantIn || gotOut != wantOut || gotCR != wantCR || gotCW != wantCW || gotReas != wantReas || gotSum != wantSum {
			t.Fatalf("incremental parity mismatch:\n  got  (in=%d out=%d cr=%d cw=%d reas=%d sum=%d)\n  want (in=%d out=%d cr=%d cw=%d reas=%d sum=%d)",
				gotIn, gotOut, gotCR, gotCW, gotReas, gotSum,
				wantIn, wantOut, wantCR, wantCW, wantReas, wantSum)
		}
	})
}

func TestDecodeFile_O2_CorruptTrajectoryMetadataBlobIsWarningNotMalformed(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "o2-bad-tmb.db")
	db := createAgySchemaDB(t, dbPath)
	defer func() { _ = db.Close() }()

	// Truncated protobuf in trajectory_metadata_blob.
	if _, err := db.Exec(`INSERT INTO trajectory_metadata_blob(id, data) VALUES ('main', ?)`, []byte{0x12, 0x50, 0x08}); err != nil {
		t.Fatalf("insert bad tmb: %v", err)
	}
	gen0 := encodeGenMetadataPB(pbGenMetaSpec{
		modelID:   1466,
		modelName: "gemini-4-neon",
		tsSecs:    1795705000,
		primary: &pbUsageSpec{
			modelID:   1466,
			hasInput:  true,
			input:     123,
			hasOutput: true,
			output:    45,
			id11:      "o2-call-1",
		},
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
		t.Fatalf("poll 1: %v", err)
	}
	if res1.Status != StatusVerified {
		t.Fatalf("poll 1 status = %q, want %q", res1.Status, StatusVerified)
	}
	if len(res1.Errors) != 0 {
		t.Fatalf("poll 1 errors = %+v, want none", res1.Errors)
	}
	if len(res1.Warnings) != 1 {
		t.Fatalf("poll 1 warnings = %+v, want 1 warning", res1.Warnings)
	}
	if len(res1.Records) != 1 || res1.Records[0].Status != StatusVerified {
		t.Fatalf("poll 1 records = %+v, want 1 verified record", res1.Records)
	}

	// Second poll must remain verified and emit zero records (no repeated malformed sentinel).
	res2, err := DecodeFile(context.Background(), dbPath, Options{
		AgyVersion: "1.5.18",
		Cursor:     res1.Cursor,
		Dedupe:     dedupe,
	})
	if err != nil {
		t.Fatalf("poll 2: %v", err)
	}
	if res2.Status != StatusVerified || len(res2.Records) != 0 || len(res2.Errors) != 0 {
		t.Fatalf("poll 2 got status=%q records=%d errors=%d, want verified/0/0", res2.Status, len(res2.Records), len(res2.Errors))
	}
}

func TestDecodeFile_O3_UnverifiedDecodeDoesNotPoisonDedupe(t *testing.T) {
	dbPath := buildMultiCallFixtureDB(t)
	dedupe := NewDedupeState()

	resUnverified, err := DecodeFile(context.Background(), dbPath, Options{
		AgyVersion: "9.9.9",
		Dedupe:     dedupe,
	})
	if err != nil {
		t.Fatalf("unverified DecodeFile: %v", err)
	}
	if resUnverified.Status != StatusUnverified {
		t.Fatalf("status = %q, want unverified", resUnverified.Status)
	}
	if dedupe.Size() != 0 {
		t.Fatalf("unverified decode mutated DedupeState: size = %d, want 0", dedupe.Size())
	}

	// Subsequent verified decode with the same DedupeState must emit all 4 records.
	resVerified, err := DecodeFile(context.Background(), dbPath, Options{
		AgyVersion: "1.5.18",
		Dedupe:     dedupe,
	})
	if err != nil {
		t.Fatalf("verified DecodeFile: %v", err)
	}
	if resVerified.Status != StatusVerified || len(resVerified.Records) != 4 {
		t.Fatalf("verified decode got status=%q records=%d, want verified and 4 records", resVerified.Status, len(resVerified.Records))
	}
	if dedupe.Size() == 0 {
		t.Fatal("expected DedupeState to be populated after verified decode")
	}
}

func TestDecodeError_ZeroRowIDAndIdxPreservedInJSON(t *testing.T) {
	de := DecodeError{
		Source: SourceGenMetadata,
		RowID:  0,
		Idx:    0,
		Error:  "bad blob",
	}
	b, err := json.Marshal(de)
	if err != nil {
		t.Fatalf("marshal DecodeError: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal DecodeError: %v", err)
	}
	if _, ok := m["rowid"]; !ok {
		t.Fatalf("expected rowid=0 in JSON, got %s", b)
	}
	if _, ok := m["idx"]; !ok {
		t.Fatalf("expected idx=0 in JSON, got %s", b)
	}
}
