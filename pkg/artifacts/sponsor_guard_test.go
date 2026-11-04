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

package artifacts

import (
	"bufio"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// sponsorRefSites is every non-test, non-comment source line under pkg/ and
// cmd/ (recursively) that names the recorded delegating user, either
// the artifact.sponsor_ref column or the Artifact.SponsorRef field
// (ptone/scion#6028). Each is reviewed and has a role:
//   - write: the only two writers of a value, createArtifact (its INSERT
//     takes Artifact.SponsorRef, which only sponsorFor sets, in publish.go
//     and versions.go) and SetAgentSponsor (the backfill's UPDATE, NULL rows
//     only);
//   - read: scans that copy the column into the struct, and SELECT or WHERE
//     clauses;
//   - pass: handing the stored value to the host's clause decisions;
//   - ddl: the 0007_orphans column definition.
//
// A new line, a changed line or a removed one fails TestSponsorRefWriteSites.
// A change that adds a writer (for example clearing the value on an ownership
// transfer) adds it here, with its role, in the same change.
//
// The check is textual. It does not see a write that never names the field
// or the column, such as copying a whole Artifact value (*a = *other) into
// a create; code review covers that case.
var sponsorRefSites = []struct{ file, line, role string }{
	{"pkg/artifacts/grants.go", "clause = clauser.ReviewGrantClause(ctx, a.OwnerRef, a.ScopeRef, a.SponsorRef)", "pass"},
	{"pkg/artifacts/grants.go", "} else if authority, ok := s.host.(ReviewGrantAuthority); !ok || !authority.MayGrantReview(ctx, a.OwnerRef, a.ScopeRef, a.SponsorRef) {", "pass"},
	{"pkg/artifacts/links.go", "switch st.StewardClause(ctx, a.OwnerRef, a.ScopeRef, a.SponsorRef, a.OrphanedAt != nil) {", "pass"},
	{"pkg/artifacts/publish.go", "a.SponsorRef = s.sponsorFor(ctx, kind, ref, a.ScopeKind)", "write"},
	{"pkg/artifacts/schema.go", "ALTER TABLE artifact ADD COLUMN IF NOT EXISTS sponsor_ref TEXT;", "ddl"},
	{"pkg/artifacts/schema.go", "ALTER TABLE artifact ADD COLUMN sponsor_ref TEXT;", "ddl"},
	{"pkg/artifacts/sponsor_backfill.go", "WHERE owner_kind = ? AND owner_ref = ? AND scope_kind = ? AND sponsor_ref IS NULL AND deleted_at IS NULL`),", "write"},
	{"pkg/artifacts/sponsor_backfill.go", "WHERE owner_kind = ? AND scope_kind = ? AND sponsor_ref IS NULL AND deleted_at IS NULL AND owner_ref > ?", "read"},
	{"pkg/artifacts/sponsor_backfill.go", "r, err := s.db.ExecContext(ctx, s.rebind(`UPDATE artifact SET sponsor_ref = ?", "write"},
	{"pkg/artifacts/store.go", "SponsorRef string", "read"},
	{"pkg/artifacts/store_sql.go", "(id, scope_kind, scope_ref, owner_kind, owner_ref, \"key\", title, current_seq, expires_at, created_at, updated_at, sponsor_ref)", "write"},
	{"pkg/artifacts/store_sql.go", "a.SponsorRef = sponsor.String", "read"},
	{"pkg/artifacts/store_sql.go", "a.current_seq, a.expires_at, a.created_at, a.updated_at, a.orphaned_at, a.sponsor_ref, v.kind AS current_kind`", "read"},
	{"pkg/artifacts/store_sql.go", "c.SponsorRef = sponsor.String", "read"},
	{"pkg/artifacts/store_sql.go", "orphaned_at, message_project_id, sponsor_ref", "read"},
	{"pkg/artifacts/store_sql.go", "s.nullTimeArg(a.ExpiresAt), s.timeArg(a.CreatedAt), s.timeArg(a.UpdatedAt), nullString(a.SponsorRef)); err != nil {", "write"},
	{"pkg/artifacts/versions.go", "a.SponsorRef = s.sponsorFor(ctx, kind, ref, a.ScopeKind)", "write"},
}

// TestSponsorRefWriteSites pins the single write path of the recorded
// delegating user at the source level: the lines naming sponsor_ref or
// SponsorRef outside tests and comments, anywhere under pkg/ and cmd/
// (every subdirectory), are exactly sponsorRefSites. The boundary
// tests (TestSponsorRequestCannotSupply) pin the request channels; this
// pins that no other code writes the value.
func TestSponsorRefWriteSites(t *testing.T) {
	root := filepath.Join("..", "..")
	var got []string
	for _, dir := range []string{"pkg", "cmd"} {
		// pkgmove:scan-covers pkg/hub/...
		// recursive walk of pkg/ (pkg/hub included) and cmd/, no SkipDir except testdata
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer func() { _ = f.Close() }()
			sc := bufio.NewScanner(f)
			sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
			for sc.Scan() {
				line := strings.TrimSpace(sc.Text())
				if strings.HasPrefix(line, "//") {
					continue
				}
				if strings.Contains(line, "sponsor_ref") || strings.Contains(line, "SponsorRef") {
					got = append(got, filepath.ToSlash(rel)+"\t"+line)
				}
			}
			return sc.Err()
		})
		if err != nil {
			t.Fatalf("scan %s: %v", dir, err)
		}
	}
	var want []string
	for _, s := range sponsorRefSites {
		want = append(want, s.file+"\t"+s.line)
	}
	slices.Sort(got)
	slices.Sort(want)
	if len(got) == 0 {
		t.Fatal("no sponsor_ref or SponsorRef line found: the scan read nothing")
	}
	for _, g := range got {
		if !slices.Contains(want, g) {
			t.Errorf("unreviewed sponsor_ref/SponsorRef site (a new writer or reader must be added to sponsorRefSites with its role): %s", g)
		}
	}
	for _, w := range want {
		if !slices.Contains(got, w) {
			t.Errorf("pinned sponsor_ref/SponsorRef site no longer present (update sponsorRefSites): %s", w)
		}
	}
	if len(got) != len(want) {
		t.Errorf("found %d sites, pinned %d (a site appears more than once?)", len(got), len(want))
	}
}
