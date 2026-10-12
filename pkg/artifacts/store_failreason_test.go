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

//go:build !no_sqlite

package artifacts

import (
	"context"
	"testing"
	"time"
)

// TestStoreMigratesVersionFailReason: the fail_reason migration is the
// last ledger entry, numbered 0008; a database at 0007 is upgraded by Init
// without touching existing rows (a failed version keeps every value and
// has no reason, so it lists as discarded), and Init stays idempotent.
func TestStoreMigratesVersionFailReason(t *testing.T) {
	if last := migrations[len(migrations)-1]; last.name != migrationVersionFailReason || last.name != "0008_version_fail_reason" {
		t.Fatalf("last migration %q, want 0008_version_fail_reason", last.name)
	}
	for _, b := range testBackends() {
		t.Run(b.name, func(t *testing.T) {
			db, _ := b.open(t)
			st := NewStore(db, b.driver).(*sqlStore)
			ledger := ledgerSQLite
			if st.dialect == dialectPostgres {
				ledger = ledgerPostgres
			}
			if _, err := db.Exec(ledger); err != nil {
				t.Fatal(err)
			}
			for _, m := range migrations {
				if m.name == migrationVersionFailReason {
					break
				}
				ddl := m.sqlite
				if st.dialect == dialectPostgres {
					ddl = m.postgres
				}
				if _, err := db.Exec(ddl); err != nil {
					t.Fatalf("%s: %v", m.name, err)
				}
				if _, err := db.Exec(st.rebind("INSERT INTO artifact_migrations (name, applied_at) VALUES (?, ?)"), m.name, st.timeArg(time.Now())); err != nil {
					t.Fatal(err)
				}
			}
			now := time.Now()
			if _, err := db.Exec(st.rebind(`INSERT INTO artifact (id, scope_kind, scope_ref, owner_kind, owner_ref, title, current_seq,
				created_at, updated_at) VALUES ('a1', 'project', 'p', 'agent', 'ag', 't', 1, ?, ?)`),
				st.timeArg(now), st.timeArg(now)); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(st.rebind(`INSERT INTO artifact_version (id, artifact_id, seq, kind, entry_path, note, total_bytes,
				file_count, created_by_kind, created_by_ref, created_at, state) VALUES ('v2', 'a1', 2, 'review', 'f.md', 'n', 3, 1, 'user', 'u', ?, 'failed')`),
				st.timeArg(now)); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if err := st.Init(ctx); err != nil {
				t.Fatalf("Init over 0007: %v", err)
			}
			v, err := st.GetVersion(ctx, "a1", 2)
			if err != nil {
				t.Fatal(err)
			}
			if v.State != VersionStateFailed || v.FailReason != "" || v.Note != "n" || v.Kind != VersionKindReview ||
				v.CreatedByRef != "u" || v.TotalBytes != 3 {
				t.Errorf("migrated version = %+v", v)
			}
			if got := discardedVersionInfo(v); got.Reason != FailReasonDiscarded {
				t.Errorf("legacy failed version reason %q, want %q", got.Reason, FailReasonDiscarded)
			}
			var n int
			if err := db.QueryRow("SELECT COUNT(*) FROM artifact_migrations").Scan(&n); err != nil || n != len(migrations) {
				t.Errorf("ledger rows = %d, %v; want %d", n, err, len(migrations))
			}
			if err := st.Init(ctx); err != nil {
				t.Errorf("Init again: %v", err)
			}
		})
	}
}
