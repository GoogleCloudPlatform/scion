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
)

// Tests for the content hash in the template commit compare-and-swap
// (ptone/scion#5658). With hub.content_hash_v2 on, a rename-only or
// chmod-only commit changes the content hash, so a finalize that diffed
// against the version before it is refused with 409 instead of overwriting
// it. While it is off, commits record the earlier (v1) hash and such a
// commit is not visible to the check, as before. The compare-and-swap is a
// strict equality on the stored value, so rows of either version are safe
// across a toggle in both directions.

const casScript = "#!/bin/sh\necho hi\n"

func finalizeTemplateCode(t *testing.T, srv *Server, tmpl *store.Template, files []store.TemplateFile, expected string) int {
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

func getTemplateRow(t *testing.T, s store.Store, id string) *store.Template {
	t.Helper()
	got, err := s.GetTemplate(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestTemplateFinalize_RenameAndChmodOnlyCommits(t *testing.T) {
	base := map[string]string{"scion-agent.yaml": commitCfgOld, "run.sh": casScript}

	tests := []struct {
		name   string
		staged map[string]string // uploads the commit introduces
		next   func() []store.TemplateFile
	}{
		// v1 concatenates file hashes in path order; this rename keeps the
		// order, so v1 cannot see it.
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
		for _, v2 := range []bool{true, false} {
			name := tt.name + " experiment off"
			if v2 {
				name = tt.name + " experiment on"
			}
			t.Run(name, func(t *testing.T) {
				stor := newCommitTestStorage(t)
				srv, s := newCommitTestServer(t, stor)
				setContentHashV2Experiment(t, srv, v2)

				tmpl := seedCommittedTemplate(t, srv, "cas", "global", "", base)
				h0 := tmpl.ContentHash
				if h0 != computeContentHashVersion(commitManifest(base), v2) {
					t.Fatalf("seeded hash %q is not the v2=%v hash of its files", h0, v2)
				}

				// Writer A commits the rename-only or chmod-only change.
				next := tt.next()
				stageObjects(t, srv, tmpl, tt.staged)
				if code := finalizeTemplateCode(t, srv, tmpl, next, h0); code != http.StatusOK {
					t.Fatalf("writer A: status %d, want 200", code)
				}
				afterA := getTemplateRow(t, s, tmpl.ID)
				if want := computeContentHashVersion(next, v2); afterA.ContentHash != want {
					t.Errorf("ContentHash = %q, want the v2=%v hash of the new manifest %q", afterA.ContentHash, v2, want)
				}
				if !reflect.DeepEqual(afterA.Files, next) {
					t.Fatalf("writer A's manifest not recorded: %+v, want %+v", afterA.Files, next)
				}

				// Writer B diffed against h0.
				stale := map[string]string{"scion-agent.yaml": commitCfgOld, "run.sh": casScript, "b.md": "from writer B"}
				stageObjects(t, srv, tmpl, stale)
				code := finalizeTemplateCode(t, srv, tmpl, commitManifest(stale), h0)
				if !v2 {
					// Off: the change is not visible to the check (the
					// behaviour before ptone/scion#5658), so B commits.
					if afterA.ContentHash != h0 || code != http.StatusOK {
						t.Fatalf("experiment off: hash changed (%q -> %q) or writer B refused (%d); want the earlier behaviour", h0, afterA.ContentHash, code)
					}
					return
				}
				if afterA.ContentHash == h0 {
					t.Fatalf("%s commit kept content hash %q", tt.name, h0)
				}
				if code != http.StatusConflict {
					t.Fatalf("writer B with the pre-%s hash: status %d, want 409", tt.name, code)
				}
				got := getTemplateRow(t, s, tmpl.ID)
				if got.ContentHash != afterA.ContentHash || !reflect.DeepEqual(got.Files, afterA.Files) {
					t.Errorf("refused finalize changed the row: hash %q files %+v, want %q %+v",
						got.ContentHash, got.Files, afterA.ContentHash, afterA.Files)
				}
			})
		}
	}
}

// TestTemplateFinalize_ExperimentToggle covers rows of both versions across
// a toggle of hub.content_hash_v2. Every commit records the version active
// when it runs, and the compare-and-swap is a strict equality on the stored
// value: accepting a v1 value that matches the v1 hash of the stored files
// would also accept it over a rename-only or chmod-only commit, which v1
// cannot see.
func TestTemplateFinalize_ExperimentToggle(t *testing.T) {
	files := map[string]string{"scion-agent.yaml": commitCfgOld, "run.sh": casScript}
	renamed := commitManifest(map[string]string{"scion-agent.yaml": commitCfgOld, "bin/run.sh": casScript})
	next := map[string]string{"scion-agent.yaml": commitCfgOld, "run.sh": casScript, "new.md": "new"}

	// seedV1 seeds a template with the experiment off, then turns it on.
	seedV1 := func(t *testing.T, name string) (*Server, store.Store, *store.Template) {
		t.Helper()
		srv, s := newCommitTestServer(t, newCommitTestStorage(t))
		setContentHashV2Experiment(t, srv, false)
		tmpl := seedCommittedTemplate(t, srv, name, "global", "", files)
		if tmpl.ContentHash != computeContentHashVersion(commitManifest(files), false) {
			t.Fatalf("seeded hash %q is not v1", tmpl.ContentHash)
		}
		setContentHashV2Experiment(t, srv, true)
		return srv, s, tmpl
	}

	t.Run("off to on: an unchanged v1 row accepts its v1 hash and is rewritten v2", func(t *testing.T) {
		srv, s, tmpl := seedV1(t, "toggle-v1-ok")
		stageObjects(t, srv, tmpl, next)
		if code := finalizeTemplateCode(t, srv, tmpl, commitManifest(next), tmpl.ContentHash); code != http.StatusOK {
			t.Fatalf("status %d, want 200", code)
		}
		if got, want := getTemplateRow(t, s, tmpl.ID).ContentHash, computeContentHashVersion(commitManifest(next), true); got != want {
			t.Errorf("ContentHash = %q, want the v2 hash %q", got, want)
		}
	})

	t.Run("off to on: a v1 hash is refused after a v1-invisible rename", func(t *testing.T) {
		srv, s, tmpl := seedV1(t, "toggle-v1-rename")
		h1 := tmpl.ContentHash
		if computeContentHashVersion(renamed, false) != h1 {
			t.Fatal("test premise: v1 must not see the rename")
		}
		stageObjects(t, srv, tmpl, map[string]string{"bin/run.sh": casScript})
		if code := finalizeTemplateCode(t, srv, tmpl, renamed, h1); code != http.StatusOK {
			t.Fatalf("rename: status %d, want 200", code)
		}
		stale := map[string]string{"scion-agent.yaml": commitCfgOld, "run.sh": casScript, "b.md": "stale"}
		stageObjects(t, srv, tmpl, stale)
		if code := finalizeTemplateCode(t, srv, tmpl, commitManifest(stale), h1); code != http.StatusConflict {
			t.Fatalf("stale v1 finalize over a rename: status %d, want 409", code)
		}
		if got := getTemplateRow(t, s, tmpl.ID); !reflect.DeepEqual(got.Files, renamed) {
			t.Errorf("rename lost: files = %+v, want %+v", got.Files, renamed)
		}
	})

	t.Run("off to on: one spurious 409 after a byte-identical recommit, then a re-read commits", func(t *testing.T) {
		srv, s, tmpl := seedV1(t, "toggle-spurious")
		h1 := tmpl.ContentHash // a client reads the row while it is v1
		// Another writer recommits the same files: the row becomes v2.
		if code := finalizeTemplateCode(t, srv, tmpl, commitManifest(files), h1); code != http.StatusOK {
			t.Fatalf("recommit: status %d, want 200", code)
		}
		reread := getTemplateRow(t, s, tmpl.ID)
		if reread.ContentHash == h1 || !reflect.DeepEqual(reread.Files, commitManifest(files)) {
			t.Fatalf("recommit: hash %q files %+v; want the same files under a v2 hash", reread.ContentHash, reread.Files)
		}
		stageObjects(t, srv, tmpl, next)
		if code := finalizeTemplateCode(t, srv, tmpl, commitManifest(next), h1); code != http.StatusConflict {
			t.Fatalf("finalize with the v1 hash read before the recommit: status %d, want the one spurious 409", code)
		}
		if code := finalizeTemplateCode(t, srv, tmpl, commitManifest(next), reread.ContentHash); code != http.StatusOK {
			t.Fatalf("retry after re-reading: status %d, want 200", code)
		}
	})

	t.Run("on to off: a v2 row accepts its v2 hash and is rewritten v1", func(t *testing.T) {
		srv, s := newCommitTestServer(t, newCommitTestStorage(t))
		setContentHashV2Experiment(t, srv, true)
		tmpl := seedCommittedTemplate(t, srv, "toggle-v2-off", "global", "", files)
		h2 := tmpl.ContentHash
		if h2 != computeContentHashVersion(commitManifest(files), true) {
			t.Fatalf("seeded hash %q is not v2", h2)
		}
		setContentHashV2Experiment(t, srv, false)

		stageObjects(t, srv, tmpl, next)
		if code := finalizeTemplateCode(t, srv, tmpl, commitManifest(next), h2); code != http.StatusOK {
			t.Fatalf("status %d, want 200", code)
		}
		if got, want := getTemplateRow(t, s, tmpl.ID).ContentHash, computeContentHashVersion(commitManifest(next), false); got != want {
			t.Errorf("ContentHash = %q, want the v1 hash %q", got, want)
		}
		if code := finalizeTemplateCode(t, srv, tmpl, commitManifest(files), h2); code != http.StatusConflict {
			t.Fatalf("finalize with the superseded v2 hash: status %d, want 409", code)
		}
	})
}

// A harness-config commit is a compare-and-swap against the row as read. A
// rename-only or chmod-only commit changes the v2 content hash (with
// hub.content_hash_v2 on), so a writer that read the row before it (here
// recommitting the files it read) is refused instead of reverting the
// change (ptone/scion#5658).
func TestHarnessConfigCommit_StaleReadOverRenameOrChmodConflicts(t *testing.T) {
	base := map[string]string{"config.yaml": deriveBareYAML, "run.sh": casScript}
	cases := []struct {
		name  string
		files map[string]string
		modes map[string]string // manifest modes, by path
	}{
		{"rename only", map[string]string{"config.yaml": deriveBareYAML, "bin/run.sh": casScript}, nil},
		{"chmod only", base, map[string]string{"run.sh": "0755"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, _ := newHCCommitTestServer(t)
			ctx := context.Background()
			setContentHashV2Experiment(t, srv, true)
			hc := ensureDeriveHC(t, srv, s)

			rec := finalizeHCManifest(t, srv, hc, base)
			if rec.Code != http.StatusOK {
				t.Fatalf("seed finalize: status %d: %s", rec.Code, rec.Body.String())
			}
			stale := *globalHC(t, s, deriveHCName)

			uploadID, manifest := stageHCUpload(t, srv, &stale, tc.files)
			for _, f := range manifest {
				if mode, ok := tc.modes[f["path"].(string)]; ok {
					f["mode"] = mode
				}
			}
			rec = doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs/"+stale.ID+"/finalize", map[string]interface{}{
				"manifest": map[string]interface{}{"version": "1.0", "files": manifest},
				"uploadId": uploadID,
			})
			if rec.Code != http.StatusOK {
				t.Fatalf("%s finalize: status %d: %s", tc.name, rec.Code, rec.Body.String())
			}
			changed := globalHC(t, s, deriveHCName)
			if changed.ContentHash == stale.ContentHash {
				t.Fatalf("%s commit kept content hash %q", tc.name, stale.ContentHash)
			}

			err := srv.commitHarnessConfigFiles(ctx, &stale, stale.Files, harnessConfigCommitOpts{})
			if !errors.Is(err, store.ErrHarnessConfigConflict) {
				t.Fatalf("stale commit over a %s change: err = %v, want ErrHarnessConfigConflict", tc.name, err)
			}
			got := globalHC(t, s, deriveHCName)
			if got.ContentHash != changed.ContentHash || !reflect.DeepEqual(got.Files, changed.Files) {
				t.Errorf("stale commit changed the row: hash %q files %+v, want %q %+v", got.ContentHash, got.Files, changed.ContentHash, changed.Files)
			}
		})
	}
}
