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

package commands

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/trajectory/agydecode"
	"google.golang.org/protobuf/encoding/protowire"
	_ "modernc.org/sqlite"
)

func writeTinyAgyFixtureDB(t *testing.T) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "cli-fixture.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(`CREATE TABLE gen_metadata (idx INTEGER PRIMARY KEY, data BLOB, size INTEGER NOT NULL DEFAULT 0)`); err != nil {
		t.Fatalf("create table: %v", err)
	}

	var u []byte
	u = protowire.AppendTag(u, 1, protowire.VarintType)
	u = protowire.AppendVarint(u, 1466)
	u = protowire.AppendTag(u, 2, protowire.VarintType)
	u = protowire.AppendVarint(u, 120)
	u = protowire.AppendTag(u, 3, protowire.VarintType)
	u = protowire.AppendVarint(u, 45)
	u = protowire.AppendTag(u, 9, protowire.VarintType)
	u = protowire.AppendVarint(u, 15)
	u = protowire.AppendTag(u, 10, protowire.VarintType)
	u = protowire.AppendVarint(u, 30)
	u = protowire.AppendTag(u, 11, protowire.BytesType)
	u = protowire.AppendString(u, "cli-req-1")

	var d []byte
	d = protowire.AppendTag(d, 3, protowire.VarintType)
	d = protowire.AppendVarint(d, 1466)
	d = protowire.AppendTag(d, 4, protowire.BytesType)
	d = protowire.AppendBytes(d, u)
	d = protowire.AppendTag(d, 19, protowire.BytesType)
	d = protowire.AppendString(d, "gemini-4-neon")

	var root []byte
	root = protowire.AppendTag(root, 1, protowire.BytesType)
	root = protowire.AppendBytes(root, d)

	if _, err := db.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES (0, ?, ?)`, root, len(root)); err != nil {
		t.Fatalf("insert row: %v", err)
	}
	return dbPath
}

func TestInternalDecodeAgyDB_JSONOutput(t *testing.T) {
	dbPath := writeTinyAgyFixtureDB(t)

	cmd := newInternalDecodeAgyDBCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"--db", dbPath, "--format", "json", "--agy-version", "1.5.18"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var res agydecode.Result
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal stdout JSON: %v\noutput:\n%s", err, stdout.String())
	}
	if res.Status != agydecode.StatusVerified {
		t.Fatalf("status = %q, want %q", res.Status, agydecode.StatusVerified)
	}
	if len(res.Records) != 1 {
		t.Fatalf("len(records) = %d, want 1", len(res.Records))
	}
	rec := res.Records[0]
	if rec.Model != "gemini-4-neon" {
		t.Fatalf("model = %q, want gemini-4-neon", rec.Model)
	}
	if rec.Tokens == nil || rec.Tokens.Input == nil || *rec.Tokens.Input != 120 || rec.Tokens.Output == nil || *rec.Tokens.Output != 45 {
		t.Fatalf("unexpected tokens: %+v", rec.Tokens)
	}
	if rec.Tokens.CacheRead != nil || rec.Tokens.CacheWrite != nil {
		t.Fatalf("unreported cache fields must be nil, got %+v", rec.Tokens)
	}
}

func TestInternalDecodeAgyDB_UnsupportedFormat(t *testing.T) {
	dbPath := writeTinyAgyFixtureDB(t)

	cmd := newInternalDecodeAgyDBCmd()
	cmd.SetArgs([]string{"--db", dbPath, "--format", "csv"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error for unsupported --format")
	}
}

func TestInternalSubcommandIsQuiet(t *testing.T) {
	if !isHookInvocation([]string{"internal", "decode-agy-db", "--db", "/tmp/x.db"}) {
		t.Fatal("expected internal decode-agy-db to suppress startup logging")
	}
}
