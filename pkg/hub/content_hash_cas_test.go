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

package hub

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

// Tests for the v2 content hash in the template commit compare-and-swap
// (ptone/scion#5658): a rename-only or chmod-only commit changes the
// content hash, so a finalize that diffed against the version before it is
// refused with 409 instead of overwriting it.

const casScript = "#!/bin/sh\necho hi\n"

func finalizeTemplate(t *testing.T, srv *Server, tmpl *store.Template, files []store.TemplateFile, expected string) int {
	t.Helper()
	rec := doTemplateRequest(t, srv, http.MethodPost, "/api/v1/templates/"+tmpl.ID+"/finalize", "application/json",
		finalizeBody(t, files, expected))
	if rec.Code == http.StatusConflict {
		if code := templateErrorCode(t, rec); code != ErrCodeTemplateConflict {
			t.Errorf("409 code = %q, want %q", code, ErrCodeTemplateConflict)
		}
	} else if rec.Code != http.StatusOK {
		t.Fatalf("finalize: status %d: %s", rec.Code, rec.Body.String())
	}
	return rec.Code
}

func TestTemplateFinalize_RenameAndChmodOnlyCommitsConflict(t *testing.T) {
	base := map[string]string{"scion-agent.yaml": commitCfgOld, "run.sh": casScript}

	tests := []struct {
		name   string
		staged map[string]string // uploads the commit introduces
		next   func() []store.TemplateFile
	}{
		{"rename only", map[string]string{"bin/run.sh": casScript}, func() []store.TemplateFile {
			return commitManifest(map[string]string{"scion-agent.yaml": commitCfgOld, "bin/run.sh": casScript})
		}},
		{"chmod only", nil, func() []store.TemplateFile {
			files := commitManifest(base)
			for i := range files {
				if files[i].Path == "run.sh" {
					files[i].Mode = "0755"
				}
			}
			return files
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stor := newCommitTestStorage(t)
			srv, s := newCommitTestServer(t, stor)
			ctx := context.Background()

			tmpl := seedCommittedTemplate(t, srv, "cas-v2", "global", "", base)
			h0 := tmpl.ContentHash
			if h0 != computeContentHash(commitManifest(base)) {
				t.Fatalf("seeded hash %q is not the v2 hash of its files", h0)
			}

			// Writer A commits the rename-only or chmod-only change.
			next := tt.next()
			stageObjects(t, srv, tmpl, tt.staged)
			if code := finalizeTemplate(t, srv, tmpl, next, h0); code != http.StatusOK {
				t.Fatalf("writer A: status %d, want 200", code)
			}
			afterA, err := s.GetTemplate(ctx, tmpl.ID)
			if err != nil {
				t.Fatal(err)
			}
			if afterA.ContentHash == h0 {
				t.Fatalf("%s commit kept content hash %q", tt.name, h0)
			}
			if afterA.ContentHash != computeContentHash(next) {
				t.Errorf("ContentHash = %q, want the v2 hash of the new manifest %q", afterA.ContentHash, computeContentHash(next))
			}

			// Writer B diffed against h0: refused, and the row keeps A's commit.
			stale := map[string]string{"scion-agent.yaml": commitCfgOld, "run.sh": casScript, "b.md": "from writer B"}
			stageObjects(t, srv, tmpl, stale)
			if code := finalizeTemplate(t, srv, tmpl, commitManifest(stale), h0); code != http.StatusConflict {
				t.Fatalf("writer B with the pre-%s hash: status %d, want 409", tt.name, code)
			}
			got, err := s.GetTemplate(ctx, tmpl.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.ContentHash != afterA.ContentHash || !reflect.DeepEqual(got.Files, afterA.Files) {
				t.Errorf("refused finalize changed the row: hash %q files %+v, want %q %+v",
					got.ContentHash, got.Files, afterA.ContentHash, afterA.Files)
			}
		})
	}
}

// A row written before ptone/scion#5658 keeps its v1 content hash until its
// next commit. A client that read that v1 value commits while the row is
// unchanged; once any commit (even a byte-identical one) has rewritten the
// row with v2, the same v1 value is refused with 409. The compare-and-swap
// is a strict equality: accepting a v1 value that matches the v1 hash of
// the stored files would also accept it over a rename-only or chmod-only
// commit, which v1 cannot see.
func TestTemplateFinalize_V1RowStrictCompare(t *testing.T) {
	files := map[string]string{"scion-agent.yaml": commitCfgOld, "run.sh": casScript}

	plantV1 := func(t *testing.T, s store.Store, tmpl *store.Template) string {
		t.Helper()
		ctx := context.Background()
		row, err := s.GetTemplate(ctx, tmpl.ID)
		if err != nil {
			t.Fatal(err)
		}
		row.ContentHash = transfer.ComputeContentHashV1(templateFileInfos(row.Files))
		if err := setTemplateContentForTest(ctx, s, row); err != nil {
			t.Fatal(err)
		}
		return row.ContentHash
	}

	t.Run("v1 row unchanged: v1 expected hash commits and rewrites v2", func(t *testing.T) {
		stor := newCommitTestStorage(t)
		srv, s := newCommitTestServer(t, stor)
		tmpl := seedCommittedTemplate(t, srv, "cas-v1", "global", "", files)
		h1 := plantV1(t, s, tmpl)

		next := map[string]string{"scion-agent.yaml": commitCfgOld, "run.sh": casScript, "new.md": "new"}
		stageObjects(t, srv, tmpl, next)
		if code := finalizeTemplate(t, srv, tmpl, commitManifest(next), h1); code != http.StatusOK {
			t.Fatalf("status %d, want 200", code)
		}
		got, err := s.GetTemplate(context.Background(), tmpl.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.ContentHash != computeContentHash(commitManifest(next)) {
			t.Errorf("ContentHash = %q, want the v2 hash %q", got.ContentHash, computeContentHash(commitManifest(next)))
		}
	})

	t.Run("v1 row renamed since the read: v1 expected hash is refused", func(t *testing.T) {
		stor := newCommitTestStorage(t)
		srv, s := newCommitTestServer(t, stor)
		tmpl := seedCommittedTemplate(t, srv, "cas-v1-rename", "global", "", files)
		h1 := plantV1(t, s, tmpl)

		// Another writer renames run.sh; the v1 hash of the new files is
		// still h1, the v2 hash the row now records is not.
		renamed := commitManifest(map[string]string{"scion-agent.yaml": commitCfgOld, "bin/run.sh": casScript})
		if transfer.ComputeContentHashV1(templateFileInfos(renamed)) != h1 {
			t.Fatal("test premise: v1 must not see the rename")
		}
		stageObjects(t, srv, tmpl, map[string]string{"bin/run.sh": casScript})
		if code := finalizeTemplate(t, srv, tmpl, renamed, h1); code != http.StatusOK {
			t.Fatalf("rename: status %d, want 200", code)
		}

		stale := map[string]string{"scion-agent.yaml": commitCfgOld, "run.sh": casScript, "b.md": "stale"}
		stageObjects(t, srv, tmpl, stale)
		if code := finalizeTemplate(t, srv, tmpl, commitManifest(stale), h1); code != http.StatusConflict {
			t.Fatalf("stale v1 finalize over a rename: status %d, want 409", code)
		}
		got, err := s.GetTemplate(context.Background(), tmpl.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got.Files, renamed) {
			t.Errorf("rename lost: files = %+v, want %+v", got.Files, renamed)
		}
	})
}

// A harness-config commit is a compare-and-swap against the row as read. A
// rename-only commit changes the v2 content hash, so a writer that read the
// row before it (here recommitting the files it read) is refused instead of
// reverting the rename (ptone/scion#5658).
func TestHarnessConfigCommit_StaleReadOverRenameConflicts(t *testing.T) {
	srv, s, _ := newHCCommitTestServer(t)
	ctx := context.Background()
	hc := ensureDeriveHC(t, srv, s)

	rec := finalizeHCManifest(t, srv, hc, map[string]string{"config.yaml": deriveBareYAML, "run.sh": casScript})
	if rec.Code != http.StatusOK {
		t.Fatalf("seed finalize: status %d: %s", rec.Code, rec.Body.String())
	}
	stale := *globalHC(t, s, deriveHCName)

	rec = finalizeHCManifest(t, srv, &stale, map[string]string{"config.yaml": deriveBareYAML, "bin/run.sh": casScript})
	if rec.Code != http.StatusOK {
		t.Fatalf("rename finalize: status %d: %s", rec.Code, rec.Body.String())
	}
	renamed := globalHC(t, s, deriveHCName)
	if renamed.ContentHash == stale.ContentHash {
		t.Fatalf("rename-only commit kept content hash %q", stale.ContentHash)
	}

	err := srv.commitHarnessConfigFiles(ctx, &stale, stale.Files, harnessConfigCommitOpts{})
	if !errors.Is(err, store.ErrHarnessConfigConflict) {
		t.Fatalf("stale commit over a rename: err = %v, want ErrHarnessConfigConflict", err)
	}
	got := globalHC(t, s, deriveHCName)
	if got.ContentHash != renamed.ContentHash || !reflect.DeepEqual(got.Files, renamed.Files) {
		t.Errorf("stale commit changed the row: hash %q files %+v, want %q %+v", got.ContentHash, got.Files, renamed.ContentHash, renamed.Files)
	}
}
