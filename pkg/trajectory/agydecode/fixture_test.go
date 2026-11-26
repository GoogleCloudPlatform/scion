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
	"database/sql"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
)

// Synthesized protobuf builders matching the real agy 1.5.18 SQLite schema.
// All values are synthetic (no real conversation text, paths, or secrets).

type pbUsageSpec struct {
	modelID    uint64
	hasInput   bool
	input      uint64
	hasOutput  bool
	output     uint64
	hasCacheWr bool
	cacheWrite uint64
	hasCacheRd bool
	cacheRead  uint64
	hasReason  bool
	reasoning  uint64
	hasNonReas bool
	nonReason  uint64
	id7        string
	id11       string
	id12       string
}

func encodeTimestampPB(secs, nanos uint64) []byte {
	var b []byte
	if secs > 0 {
		b = protowire.AppendTag(b, 1, protowire.VarintType)
		b = protowire.AppendVarint(b, secs)
	}
	if nanos > 0 {
		b = protowire.AppendTag(b, 2, protowire.VarintType)
		b = protowire.AppendVarint(b, nanos)
	}
	return b
}

func encodeUsagePB(u pbUsageSpec) []byte {
	var b []byte
	if u.modelID > 0 {
		b = protowire.AppendTag(b, 1, protowire.VarintType)
		b = protowire.AppendVarint(b, u.modelID)
	}
	if u.hasInput {
		b = protowire.AppendTag(b, 2, protowire.VarintType)
		b = protowire.AppendVarint(b, u.input)
	}
	if u.hasOutput {
		b = protowire.AppendTag(b, 3, protowire.VarintType)
		b = protowire.AppendVarint(b, u.output)
	}
	if u.hasCacheWr {
		b = protowire.AppendTag(b, 4, protowire.VarintType)
		b = protowire.AppendVarint(b, u.cacheWrite)
	}
	if u.hasCacheRd {
		b = protowire.AppendTag(b, 5, protowire.VarintType)
		b = protowire.AppendVarint(b, u.cacheRead)
	}
	if u.id7 != "" {
		b = protowire.AppendTag(b, 7, protowire.BytesType)
		b = protowire.AppendString(b, u.id7)
	}
	if u.hasReason {
		b = protowire.AppendTag(b, 9, protowire.VarintType)
		b = protowire.AppendVarint(b, u.reasoning)
	}
	if u.hasNonReas {
		b = protowire.AppendTag(b, 10, protowire.VarintType)
		b = protowire.AppendVarint(b, u.nonReason)
	}
	if u.id11 != "" {
		b = protowire.AppendTag(b, 11, protowire.BytesType)
		b = protowire.AppendString(b, u.id11)
	}
	if u.id12 != "" {
		b = protowire.AppendTag(b, 12, protowire.BytesType)
		b = protowire.AppendString(b, u.id12)
	}
	return b
}

func encodeRetryEntryPB(u pbUsageSpec) []byte {
	var b []byte
	ub := encodeUsagePB(u)
	b = protowire.AppendTag(b, 2, protowire.BytesType)
	b = protowire.AppendBytes(b, ub)
	return b
}

type pbGenMetaSpec struct {
	modelID     uint64
	modelName   string
	displayName string
	tsSecs      uint64
	primary     *pbUsageSpec
	retries     []pbUsageSpec
}

func encodeGenMetadataPB(g pbGenMetaSpec) []byte {
	var d []byte
	if g.modelID > 0 {
		d = protowire.AppendTag(d, 3, protowire.VarintType)
		d = protowire.AppendVarint(d, g.modelID)
	}
	if g.primary != nil {
		d = protowire.AppendTag(d, 4, protowire.BytesType)
		d = protowire.AppendBytes(d, encodeUsagePB(*g.primary))
	}
	if g.tsSecs > 0 {
		var f9 []byte
		f9 = protowire.AppendTag(f9, 4, protowire.BytesType)
		f9 = protowire.AppendBytes(f9, encodeTimestampPB(g.tsSecs, 0))
		d = protowire.AppendTag(d, 9, protowire.BytesType)
		d = protowire.AppendBytes(d, f9)
	}
	for _, r := range g.retries {
		d = protowire.AppendTag(d, 17, protowire.BytesType)
		d = protowire.AppendBytes(d, encodeRetryEntryPB(r))
	}
	if g.modelName != "" {
		d = protowire.AppendTag(d, 19, protowire.BytesType)
		d = protowire.AppendString(d, g.modelName)
	}
	if g.displayName != "" {
		d = protowire.AppendTag(d, 21, protowire.BytesType)
		d = protowire.AppendString(d, g.displayName)
	}
	var root []byte
	root = protowire.AppendTag(root, 1, protowire.BytesType)
	root = protowire.AppendBytes(root, d)
	return root
}

type pbStepMetaSpec struct {
	modelID     uint64
	modelName   string
	displayName string
	startSecs   uint64
	endSecs     uint64
	primary     *pbUsageSpec
	retries     []pbUsageSpec
}

func encodeStepMetadataPB(s pbStepMetaSpec) []byte {
	var d []byte
	if s.startSecs > 0 {
		d = protowire.AppendTag(d, 1, protowire.BytesType)
		d = protowire.AppendBytes(d, encodeTimestampPB(s.startSecs, 0))
	}
	if s.endSecs > 0 {
		d = protowire.AppendTag(d, 8, protowire.BytesType)
		d = protowire.AppendBytes(d, encodeTimestampPB(s.endSecs, 0))
	}
	if s.primary != nil {
		d = protowire.AppendTag(d, 9, protowire.BytesType)
		d = protowire.AppendBytes(d, encodeUsagePB(*s.primary))
	}
	if s.modelID > 0 || s.modelName != "" || s.displayName != "" {
		var m []byte
		if s.modelID > 0 {
			m = protowire.AppendTag(m, 1, protowire.VarintType)
			m = protowire.AppendVarint(m, s.modelID)
		}
		if s.displayName != "" {
			m = protowire.AppendTag(m, 8, protowire.BytesType)
			m = protowire.AppendString(m, s.displayName)
		}
		if s.modelName != "" {
			m = protowire.AppendTag(m, 12, protowire.BytesType)
			m = protowire.AppendString(m, s.modelName)
		}
		d = protowire.AppendTag(d, 24, protowire.BytesType)
		d = protowire.AppendBytes(d, m)
	}
	for _, r := range s.retries {
		d = protowire.AppendTag(d, 28, protowire.BytesType)
		d = protowire.AppendBytes(d, encodeRetryEntryPB(r))
	}
	return d
}

func encodeTrajectoryMetaBlobPB(secs uint64) []byte {
	var b []byte
	b = protowire.AppendTag(b, 2, protowire.BytesType)
	b = protowire.AppendBytes(b, encodeTimestampPB(secs, 0))
	return b
}

func createAgySchemaDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open fixture db: %v", err)
	}
	db.SetMaxOpenConns(1)
	schema := `
CREATE TABLE trajectory_meta (
	trajectory_id TEXT PRIMARY KEY,
	cascade_id TEXT,
	trajectory_type INTEGER,
	source INTEGER
);
CREATE TABLE steps (
	idx INTEGER PRIMARY KEY,
	step_type INTEGER NOT NULL DEFAULT 0,
	status INTEGER NOT NULL DEFAULT 0,
	has_subtrajectory NUMERIC NOT NULL DEFAULT false,
	metadata BLOB
);
CREATE TABLE gen_metadata (
	idx INTEGER PRIMARY KEY,
	data BLOB,
	size INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE trajectory_metadata_blob (
	id TEXT PRIMARY KEY DEFAULT "main",
	data BLOB
);
`
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		t.Fatalf("create schema: %v", err)
	}
	return db
}

// buildMultiCallFixtureDB creates a synthesized, scrubbed DB matching real
// agy 1.5.18 schema and blob shapes:
//   - Call 1 (gen_metadata idx=0 + steps idx=1 + d.17 duplicate): gemini-4-neon, no cache fields
//   - Call 2 (gen_metadata idx=1 + steps idx=4): gemini-4-neon with cache_read + retry on claude-sonnet-4-5 with cache_write
//   - Call 3 (steps idx=7 + gen_metadata idx=2): transitive union-find dedupe across IDs [11, 12] and [12, 7]
func buildMultiCallFixtureDB(t *testing.T) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "fixture-multi-call.db")
	db := createAgySchemaDB(t, dbPath)
	defer func() { _ = db.Close() }()

	_, err := db.Exec(`INSERT INTO trajectory_metadata_blob(id, data) VALUES ('main', ?)`,
		encodeTrajectoryMetaBlobPB(1795700000))
	if err != nil {
		t.Fatalf("insert trajectory_metadata_blob: %v", err)
	}

	call1Usage := pbUsageSpec{
		modelID:    1466,
		hasInput:   true,
		input:      17943,
		hasOutput:  true,
		output:     283,
		hasReason:  true,
		reasoning:  121,
		hasNonReas: true,
		nonReason:  162,
		id7:        "req-call-1",
		id11:       "req-call-1",
		id12:       "req-call-1",
	}
	gen0 := encodeGenMetadataPB(pbGenMetaSpec{
		modelID:     1466,
		modelName:   "gemini-4-neon",
		displayName: "Gemini 4 Neon (Medium)",
		primary:     &call1Usage,
		retries:     []pbUsageSpec{call1Usage}, // mirrors real agy 1.5.18 d.17 repeating final attempt
	})
	step1 := encodeStepMetadataPB(pbStepMetaSpec{
		startSecs: 1795700001,
		endSecs:   1795700005,
		primary:   &call1Usage,
	})

	call2Usage := pbUsageSpec{
		modelID:    1466,
		hasInput:   true,
		input:      2091,
		hasOutput:  true,
		output:     295,
		hasCacheRd: true,
		cacheRead:  17772,
		hasReason:  true,
		reasoning:  120,
		hasNonReas: true,
		nonReason:  175,
		id7:        "req-call-2",
		id11:       "req-call-2",
		id12:       "req-call-2",
	}
	call2RetryUsage := pbUsageSpec{
		modelID:    333, // claude-sonnet-4-5 (different model on retry!)
		hasInput:   true,
		input:      500,
		hasOutput:  true,
		output:     80,
		hasCacheWr: true,
		cacheWrite: 1200,
		hasCacheRd: true,
		cacheRead:  300,
		hasReason:  true,
		reasoning:  20,
		hasNonReas: true,
		nonReason:  60,
		id11:       "req-call-2-retry",
	}
	gen1 := encodeGenMetadataPB(pbGenMetaSpec{
		modelID:     1466,
		modelName:   "gemini-4-neon",
		displayName: "Gemini 4 Neon (Medium)",
		primary:     &call2Usage,
		retries:     []pbUsageSpec{call2RetryUsage, call2Usage},
	})
	step4 := encodeStepMetadataPB(pbStepMetaSpec{
		startSecs: 1795700010,
		endSecs:   1795700015,
		primary:   &call2Usage,
	})

	// Transitive duplicate IDs: step7 has [11, 12] with partial output; gen2 has [12, 7] with final output.
	call3StepUsage := pbUsageSpec{
		modelID:    1466,
		hasInput:   true,
		input:      1000,
		hasOutput:  true,
		output:     50,
		hasReason:  true,
		reasoning:  10,
		hasNonReas: true,
		nonReason:  40,
		id11:       "req-call-3-a",
		id12:       "req-call-3-bridge",
	}
	call3GenUsage := pbUsageSpec{
		modelID:    1466,
		hasInput:   true,
		input:      1000,
		hasOutput:  true,
		output:     90,
		hasReason:  true,
		reasoning:  30,
		hasNonReas: true,
		nonReason:  60,
		id12:       "req-call-3-bridge",
		id7:        "req-call-3-c",
	}
	step7 := encodeStepMetadataPB(pbStepMetaSpec{
		startSecs: 1795700020,
		endSecs:   1795700025,
		primary:   &call3StepUsage,
	})
	gen2 := encodeGenMetadataPB(pbGenMetaSpec{
		modelID:     1466,
		modelName:   "gemini-4-neon",
		displayName: "Gemini 4 Neon (Medium)",
		primary:     &call3GenUsage,
	})

	for _, r := range []struct {
		idx  int64
		data []byte
	}{
		{0, gen0},
		{1, gen1},
		{2, gen2},
	} {
		if _, err := db.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (?, ?, ?)`, r.idx, r.data, len(r.data)); err != nil {
			t.Fatalf("insert gen_metadata %d: %v", r.idx, err)
		}
	}
	// Include a step with NULL metadata to verify NULL skipping.
	if _, err := db.Exec(`INSERT INTO steps(idx, step_type, metadata) VALUES (0, 1, NULL)`); err != nil {
		t.Fatalf("insert null step: %v", err)
	}
	for _, r := range []struct {
		idx  int64
		data []byte
	}{
		{1, step1},
		{4, step4},
		{7, step7},
	} {
		if _, err := db.Exec(`INSERT INTO steps(idx, step_type, metadata) VALUES (?, 15, ?)`, r.idx, r.data); err != nil {
			t.Fatalf("insert steps %d: %v", r.idx, err)
		}
	}

	return dbPath
}

// buildMalformedFixtureDB creates a DB containing one valid row and one
// truncated/corrupt protobuf blob in gen_metadata.
func buildMalformedFixtureDB(t *testing.T) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "fixture-malformed.db")
	db := createAgySchemaDB(t, dbPath)
	defer func() { _ = db.Close() }()

	valid := encodeGenMetadataPB(pbGenMetaSpec{
		modelID:   1466,
		modelName: "gemini-4-neon",
		tsSecs:    1795700100,
		primary: &pbUsageSpec{
			modelID:   1466,
			hasInput:  true,
			input:     100,
			hasOutput: true,
			output:    25,
			id11:      "req-valid-1",
		},
	})
	// Truncated length-delimited field 1 claiming 100 bytes with only 2 bytes present.
	corrupt := []byte{0x0a, 0x64, 0x08, 0x01}

	if _, err := db.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (0, ?, ?)`, valid, len(valid)); err != nil {
		t.Fatalf("insert valid row: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (1, ?, ?)`, corrupt, len(corrupt)); err != nil {
		t.Fatalf("insert corrupt row: %v", err)
	}
	return dbPath
}
