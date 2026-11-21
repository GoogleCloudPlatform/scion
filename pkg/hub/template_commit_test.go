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
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Tests for the single template commit path (ptone/scion#4217).

// corruptDerivedFields overwrites the stored derived fields, so a path that
// copies them instead of re-deriving is caught.
func corruptDerivedFields(t *testing.T, s store.Store, id string) {
	t.Helper()
	ctx := context.Background()
	tmpl, err := s.GetTemplate(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	tmpl.Harness = "stale"
	tmpl.DefaultHarnessConfig = ""
	tmpl.AgentConfig = nil
	if err := setTemplateContentForTest(ctx, s, tmpl); err != nil {
		t.Fatal(err)
	}
}

// TestTemplateCommit_RoutedPaths is the table test over every path routed
// through commitTemplateFiles. Each path commits a scion-agent.yaml that sets
// both harness-config keys and must produce the same index, in the blob
// layout. Paths that drop a file check that its blob is kept for the garbage
// collector rather than deleted by the commit (ptone/scion#4221).
func TestTemplateCommit_RoutedPaths(t *testing.T) {
	ctx := context.Background()
	oldFiles := map[string]string{"scion-agent.yaml": commitCfgOld, "old.md": "dropped"}

	cases := []struct {
		name string
		// run performs the commit and returns the resulting template ID.
		run func(t *testing.T, srv *Server, s store.Store) string
	}{
		{
			// Acceptance 1 and 3: a CLI-style finalize re-derives the index
			// and deletes the objects dropped from the manifest.
			name: "finalize",
			run: func(t *testing.T, srv *Server, s store.Store) string {
				tmpl := seedCommittedTemplate(t, srv, "tpl-finalize", store.TemplateScopeGlobal, "", oldFiles)
				if tmpl.DefaultHarnessConfig != "gemini-web" {
					t.Fatalf("seed DefaultHarnessConfig = %q", tmpl.DefaultHarnessConfig)
				}
				next := map[string]string{"scion-agent.yaml": commitCfgBoth, "CLAUDE.md": "# agent"}
				stageObjects(t, srv, tmpl, next)
				mustStatus(t, finalizeTemplate(t, srv, tmpl.ID, commitManifest(next)), http.StatusOK)
				if !objectExists(t, srv.GetStorage(), blobObjectPath(tmpl, "dropped")) {
					t.Error("finalize deleted the blob of a dropped file; only the garbage collector may")
				}
				if !objectExists(t, srv.GetStorage(), blobObjectPath(tmpl, "# agent")) {
					t.Error("finalize did not store the new file's blob")
				}
				return tmpl.ID
			},
		},
		{
			name: "file write (JSON)",
			run: func(t *testing.T, srv *Server, s store.Store) string {
				tmpl := seedCommittedTemplate(t, srv, "tpl-write", store.TemplateScopeGlobal, "", oldFiles)
				body, _ := json.Marshal(TemplateFileWriteRequest{Content: commitCfgBoth})
				mustStatus(t, doTemplateRequest(t, srv, http.MethodPut, "/api/v1/templates/"+tmpl.ID+"/files/scion-agent.yaml", "application/json", body), http.StatusOK)
				return tmpl.ID
			},
		},
		{
			name: "file write (raw)",
			run: func(t *testing.T, srv *Server, s store.Store) string {
				tmpl := seedCommittedTemplate(t, srv, "tpl-raw", store.TemplateScopeGlobal, "", oldFiles)
				mustStatus(t, doTemplateRequest(t, srv, http.MethodPut, "/api/v1/templates/"+tmpl.ID+"/files/scion-agent.yaml", "application/octet-stream", []byte(commitCfgBoth)), http.StatusOK)
				return tmpl.ID
			},
		},
		{
			name: "file upload (multipart)",
			run: func(t *testing.T, srv *Server, s store.Store) string {
				tmpl := seedCommittedTemplate(t, srv, "tpl-multipart", store.TemplateScopeGlobal, "", oldFiles)
				w := httptest.NewRecorder()
				srv.Handler().ServeHTTP(w, templateMultipartRequest(t, tmpl.ID, map[string][]byte{"scion-agent.yaml": []byte(commitCfgBoth)}))
				mustStatus(t, w, http.StatusOK)
				return tmpl.ID
			},
		},
		{
			name: "file delete (non-config file)",
			run: func(t *testing.T, srv *Server, s store.Store) string {
				tmpl := seedCommittedTemplate(t, srv, "tpl-delete", store.TemplateScopeGlobal, "",
					map[string]string{"scion-agent.yaml": commitCfgBoth, "old.md": "dropped"})
				corruptDerivedFields(t, s, tmpl.ID)
				mustStatus(t, doTemplateRequest(t, srv, http.MethodDelete, "/api/v1/templates/"+tmpl.ID+"/files/old.md", "", nil), http.StatusNoContent)
				if !objectExists(t, srv.GetStorage(), blobObjectPath(tmpl, "dropped")) {
					t.Error("delete removed the blob; only the garbage collector may")
				}
				return tmpl.ID
			},
		},
		{
			name: "import (forced resource import)",
			run: func(t *testing.T, srv *Server, s store.Store) string {
				dir := writeTemplateDir(t, t.TempDir(), "tpl-import", map[string]string{"scion-agent.yaml": commitCfgBoth})
				if _, err := srv.templateStore().Bootstrap(ctx, "tpl-import", dir, store.TemplateScopeGlobal, "", "https://example.com/src", true); err != nil {
					t.Fatal(err)
				}
				got, err := s.GetTemplateBySlug(ctx, "tpl-import", store.TemplateScopeGlobal, "")
				if err != nil {
					t.Fatal(err)
				}
				return got.ID
			},
		},
		{
			name: "bootstrap (new template from disk)",
			run: func(t *testing.T, srv *Server, s store.Store) string {
				parent := t.TempDir()
				writeTemplateDir(t, parent, "tpl-bootstrap", map[string]string{"scion-agent.yaml": commitCfgBoth})
				if err := srv.BootstrapTemplatesFromDir(ctx, parent); err != nil {
					t.Fatal(err)
				}
				got, err := s.GetTemplateBySlug(ctx, "tpl-bootstrap", store.TemplateScopeGlobal, "")
				if err != nil {
					t.Fatal(err)
				}
				return got.ID
			},
		},
		{
			name: "bootstrap (existing template, changed content)",
			run: func(t *testing.T, srv *Server, s store.Store) string {
				tmpl := seedCommittedTemplate(t, srv, "tpl-sync", store.TemplateScopeGlobal, "", oldFiles)
				dir := writeTemplateDir(t, t.TempDir(), "tpl-sync", map[string]string{"scion-agent.yaml": commitCfgBoth})
				if _, err := srv.syncExistingTemplate(ctx, tmpl, dir, false); err != nil {
					t.Fatal(err)
				}
				return tmpl.ID
			},
		},
		{
			name: "bootstrap (unchanged content re-derives stale row)",
			run: func(t *testing.T, srv *Server, s store.Store) string {
				dir := writeTemplateDir(t, t.TempDir(), "tpl-hashmatch", map[string]string{"scion-agent.yaml": commitCfgBoth})
				if _, err := srv.templateStore().Bootstrap(ctx, "tpl-hashmatch", dir, store.TemplateScopeGlobal, "", "", false); err != nil {
					t.Fatal(err)
				}
				got, err := s.GetTemplateBySlug(ctx, "tpl-hashmatch", store.TemplateScopeGlobal, "")
				if err != nil {
					t.Fatal(err)
				}
				corruptDerivedFields(t, s, got.ID)
				if _, err := srv.templateStore().Bootstrap(ctx, "tpl-hashmatch", dir, store.TemplateScopeGlobal, "", "", false); err != nil {
					t.Fatal(err)
				}
				return got.ID
			},
		},
		{
			// A legacy row without a storage path reaches the hash-match path
			// on every start and must still be re-derived.
			name: "bootstrap (legacy row without storage path, unchanged content)",
			run: func(t *testing.T, srv *Server, s store.Store) string {
				dir := writeTemplateDir(t, t.TempDir(), "tpl-legacy", map[string]string{"scion-agent.yaml": commitCfgBoth})
				if _, err := srv.templateStore().Bootstrap(ctx, "tpl-legacy", dir, store.TemplateScopeGlobal, "", "", false); err != nil {
					t.Fatal(err)
				}
				got, err := s.GetTemplateBySlug(ctx, "tpl-legacy", store.TemplateScopeGlobal, "")
				if err != nil {
					t.Fatal(err)
				}
				wantPath := got.StoragePath
				got.StoragePath = ""
				got.Layout = ""
				got.DefaultHarnessConfig = ""
				got.AgentConfig = nil
				if err := setTemplateContentForTest(ctx, s, got); err != nil {
					t.Fatal(err)
				}
				if _, err := srv.templateStore().Bootstrap(ctx, "tpl-legacy", dir, store.TemplateScopeGlobal, "", "", false); err != nil {
					t.Fatal(err)
				}
				after, err := s.GetTemplate(ctx, got.ID)
				if err != nil {
					t.Fatal(err)
				}
				if after.StoragePath != wantPath {
					t.Errorf("StoragePath = %q, want the computed %q", after.StoragePath, wantPath)
				}
				return got.ID
			},
		},
		{
			name: "bootstrap from source (BootstrapSource)",
			run: func(t *testing.T, srv *Server, s store.Store) string {
				br := testBundledResource(storage.ResourceKindTemplate, "tpl-source", map[string]string{"scion-agent.yaml": commitCfgBoth})
				if _, err := srv.templateStore().BootstrapSource(ctx, NewFSResourceSource(br), BootstrapOptions{}); err != nil {
					t.Fatal(err)
				}
				got, err := s.GetTemplateBySlug(ctx, "tpl-source", store.TemplateScopeGlobal, "")
				if err != nil {
					t.Fatal(err)
				}
				return got.ID
			},
		},
		{
			name: "reimport",
			run: func(t *testing.T, srv *Server, s store.Store) string {
				tmpl := seedCommittedTemplate(t, srv, "tpl-reimport", store.TemplateScopeGlobal, "", oldFiles)
				dir := writeTemplateDir(t, t.TempDir(), "tpl-reimport", map[string]string{"scion-agent.yaml": commitCfgBoth})
				if _, err := srv.targetTemplateStore(tmpl.ID).Bootstrap(ctx, tmpl.Name, dir, tmpl.Scope, tmpl.ScopeID, "", true); err != nil {
					t.Fatal(err)
				}
				return tmpl.ID
			},
		},
		{
			name: "repair from storage",
			run: func(t *testing.T, srv *Server, s store.Store) string {
				// Repair only changes legacy rows (blob content cannot
				// drift); its commit migrates the row to blobs.
				tmpl := seedUncommittedTemplate(t, srv, s, "", "tpl-repair", store.TemplateScopeGlobal, "", map[string]string{"scion-agent.yaml": commitCfgOld})
				// The stored object changes behind the row's back.
				putObjects(t, srv.GetStorage(), tmpl.StoragePath, map[string]string{"scion-agent.yaml": commitCfgBoth})
				if err := srv.syncTemplateFromStorage(ctx, TemplateRepairRef{ID: tmpl.ID}); err != nil {
					t.Fatal(err)
				}
				return tmpl.ID
			},
		},
		{
			// Acceptance 6: clone re-derives instead of copying.
			name: "template clone",
			run: func(t *testing.T, srv *Server, s store.Store) string {
				src := seedCommittedTemplate(t, srv, "tpl-clone-src", store.TemplateScopeGlobal, "", map[string]string{"scion-agent.yaml": commitCfgBoth})
				corruptDerivedFields(t, s, src.ID)
				body, _ := json.Marshal(CloneTemplateRequest{Name: "tpl-clone-dst", Scope: store.TemplateScopeGlobal})
				w := doTemplateRequest(t, srv, http.MethodPost, "/api/v1/templates/"+src.ID+"/clone", "application/json", body)
				mustStatus(t, w, http.StatusCreated)
				var clone store.Template
				if err := json.Unmarshal(w.Body.Bytes(), &clone); err != nil {
					t.Fatal(err)
				}
				return clone.ID
			},
		},
		{
			name: "project clone",
			run: func(t *testing.T, srv *Server, s store.Store) string {
				srcProject, dstProject := api.NewUUID(), api.NewUUID()
				src := seedCommittedTemplate(t, srv, "tpl-pclone", store.TemplateScopeProject, srcProject, map[string]string{"scion-agent.yaml": commitCfgBoth})
				corruptDerivedFields(t, s, src.ID)
				var rollback []func()
				if err := srv.cloneProjectTemplates(ctx, srcProject, &store.Project{ID: dstProject}, &rollback); err != nil {
					t.Fatal(err)
				}
				got, err := s.GetTemplateBySlug(ctx, src.Slug, store.TemplateScopeProject, dstProject)
				if err != nil {
					t.Fatal(err)
				}
				return got.ID
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s := newCommitTestServer(t, newCommitTestStorage(t))
			id := tc.run(t, srv, s)
			got, err := s.GetTemplate(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			assertBothIndex(t, got)
			if got.ContentHash != defaultContentHash(got.Files) {
				t.Errorf("ContentHash %q does not match the manifest", got.ContentHash)
			}
			if got.Layout != store.TemplateLayoutBlobs || got.StoragePath != srv.templateBlobStoragePath(got) {
				t.Errorf("row not in the blob layout: layout %q, path %q", got.Layout, got.StoragePath)
			}
			for _, f := range got.Files {
				if !objectExists(t, srv.GetStorage(), templateObjectPath(got, f)) {
					t.Errorf("blob of %s missing", f.Path)
				}
			}
		})
	}
}

// TestTemplateCommit_DeleteAgentConfigClearsIndex covers acceptance 2:
// deleting scion-agent.yaml clears DefaultHarnessConfig and AgentConfig and
// takes Harness from the template name. Its blob stays for the garbage
// collector (ptone/scion#4221).
func TestTemplateCommit_DeleteAgentConfigClearsIndex(t *testing.T) {
	ctx := context.Background()
	srv, s := newCommitTestServer(t, newCommitTestStorage(t))
	tmpl := seedCommittedTemplate(t, srv, "codex-delete", store.TemplateScopeGlobal, "",
		map[string]string{"scion-agent.yaml": commitCfgBoth, "AGENTS.md": "# agent"})
	if tmpl.DefaultHarnessConfig != "claude-web" || tmpl.AgentConfig == nil {
		t.Fatalf("seed index = %q / %v", tmpl.DefaultHarnessConfig, tmpl.AgentConfig)
	}

	mustStatus(t, doTemplateRequest(t, srv, http.MethodDelete, "/api/v1/templates/"+tmpl.ID+"/files/scion-agent.yaml", "", nil), http.StatusNoContent)

	got, err := s.GetTemplate(ctx, tmpl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DefaultHarnessConfig != "" || got.AgentConfig != nil || got.Harness != "codex" {
		t.Errorf("after delete: DefaultHarnessConfig %q, AgentConfig %v, Harness %q; want \"\", nil, \"codex\"",
			got.DefaultHarnessConfig, got.AgentConfig, got.Harness)
	}
	if len(got.Files) != 1 || got.Files[0].Path != "AGENTS.md" {
		t.Errorf("manifest after delete = %+v", got.Files)
	}
	if !objectExists(t, srv.GetStorage(), blobObjectPath(got, commitCfgBoth)) {
		t.Error("delete removed scion-agent.yaml's blob; only the garbage collector may")
	}
}

// TestTemplateCommit_UnparsableAgentConfig: an unparsable scion-agent.yaml is
// accepted and derived like a missing one on every path.
func TestTemplateCommit_UnparsableAgentConfig(t *testing.T) {
	ctx := context.Background()
	srv, s := newCommitTestServer(t, newCommitTestStorage(t))
	tmpl := seedCommittedTemplate(t, srv, "gemini-broken", store.TemplateScopeGlobal, "", map[string]string{"scion-agent.yaml": commitCfgBoth})

	body, _ := json.Marshal(TemplateFileWriteRequest{Content: ": invalid: yaml: ["})
	mustStatus(t, doTemplateRequest(t, srv, http.MethodPut, "/api/v1/templates/"+tmpl.ID+"/files/scion-agent.yaml", "application/json", body), http.StatusOK)

	got, err := s.GetTemplate(ctx, tmpl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AgentConfig != nil || got.DefaultHarnessConfig != "" || got.Harness != "gemini-cli" {
		t.Errorf("unparsable config: AgentConfig %v, DefaultHarnessConfig %q, Harness %q; want nil, \"\", \"gemini-cli\"",
			got.AgentConfig, got.DefaultHarnessConfig, got.Harness)
	}

	// The directory variant derives the same.
	dir := writeTemplateDir(t, t.TempDir(), "x", map[string]string{"scion-agent.yaml": ": invalid: yaml: ["})
	idx := deriveTemplateIndexForTest(t, dir, "gemini-broken")
	if idx.AgentConfig != nil || idx.DefaultHarnessConfig != "" || idx.Harness != "gemini-cli" {
		t.Errorf("dir variant: %+v", idx)
	}
}

// TestTemplateCommit_LocalAndGCSPushIdentical covers acceptance 4: the same
// CLI push produces identical rows on local storage (uploads go through the
// raw write handler) and on a GCS-style backend (uploads go straight to the
// bucket through signed URLs).
func TestTemplateCommit_LocalAndGCSPushIdentical(t *testing.T) {
	ctx := context.Background()
	push := map[string]string{"scion-agent.yaml": commitCfgBoth, "CLAUDE.md": "# agent", "home/.bashrc": "export A=1\n"}

	run := func(t *testing.T, stor storage.Storage, local bool) *store.Template {
		srv, s := newCommitTestServer(t, stor)
		tmpl := seedCommittedTemplate(t, srv, "tpl-push", store.TemplateScopeGlobal, "", map[string]string{"scion-agent.yaml": commitCfgOld, "old.md": "x"})
		if local {
			for p, c := range push {
				mustStatus(t, doTemplateRequest(t, srv, http.MethodPut, "/api/v1/templates/"+tmpl.ID+"/files/"+p, "application/octet-stream", []byte(c)), http.StatusOK)
			}
		} else {
			stageObjects(t, srv, tmpl, push)
		}
		mustStatus(t, finalizeTemplate(t, srv, tmpl.ID, commitManifest(push)), http.StatusOK)
		got, err := s.GetTemplate(ctx, tmpl.ID)
		if err != nil {
			t.Fatal(err)
		}
		for p, c := range push {
			if !objectExists(t, stor, blobObjectPath(got, c)) {
				t.Errorf("blob of %s missing", p)
			}
		}
		return got
	}

	gcs := newMockStorage("gcs-bucket")
	gcs.provider = storage.ProviderGCS
	localRow := run(t, newCommitTestStorage(t), true)
	gcsRow := run(t, gcs, false)

	type rowView struct {
		Files                []store.TemplateFile
		ContentHash          string
		Harness              string
		DefaultHarnessConfig string
		AgentConfig          *api.ScionConfig
		Status               string
	}
	view := func(r *store.Template) string {
		b, _ := json.Marshal(rowView{r.Files, r.ContentHash, r.Harness, r.DefaultHarnessConfig, r.AgentConfig, r.Status})
		return string(b)
	}
	if view(localRow) != view(gcsRow) {
		t.Errorf("local and GCS rows differ:\nlocal: %s\ngcs:   %s", view(localRow), view(gcsRow))
	}
	assertBothIndex(t, localRow)
}

// TestTemplateCommit_UnusableBundledHarnessConfig covers acceptance 8: a
// bundled harness-config whose provisioner can never provision an agent is
// refused with 422 on file write, multipart and finalize, before the row is
// updated, and skipped with a warning (no error) on bootstrap.
func TestTemplateCommit_UnusableBundledHarnessConfig(t *testing.T) {
	ctx := context.Background()
	const hcPath = "harness-configs/bad/config.yaml"

	assertUnchanged := func(t *testing.T, s store.Store, before *store.Template) {
		t.Helper()
		got, err := s.GetTemplate(ctx, before.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.ContentHash != before.ContentHash || len(got.Files) != len(before.Files) {
			t.Errorf("row changed after a refused commit: hash %q -> %q, files %d -> %d",
				before.ContentHash, got.ContentHash, len(before.Files), len(got.Files))
		}
	}

	t.Run("file write", func(t *testing.T) {
		srv, s := newCommitTestServer(t, newCommitTestStorage(t))
		tmpl := seedCommittedTemplate(t, srv, "tpl-hc-write", store.TemplateScopeGlobal, "", map[string]string{"scion-agent.yaml": commitCfgOld})
		body, _ := json.Marshal(TemplateFileWriteRequest{Content: unusableHCConfig})
		mustStatus(t, doTemplateRequest(t, srv, http.MethodPut, "/api/v1/templates/"+tmpl.ID+"/files/"+hcPath, "application/json", body), http.StatusUnprocessableEntity)
		assertUnchanged(t, s, tmpl)
		if objectExists(t, srv.GetStorage(), blobObjectPath(tmpl, unusableHCConfig)) {
			t.Error("refused content reached storage")
		}
	})

	t.Run("raw write", func(t *testing.T) {
		srv, s := newCommitTestServer(t, newCommitTestStorage(t))
		tmpl := seedCommittedTemplate(t, srv, "tpl-hc-raw", store.TemplateScopeGlobal, "", map[string]string{"scion-agent.yaml": commitCfgOld})
		mustStatus(t, doTemplateRequest(t, srv, http.MethodPut, "/api/v1/templates/"+tmpl.ID+"/files/"+hcPath, "application/octet-stream", []byte(unusableHCConfig)), http.StatusUnprocessableEntity)
		assertUnchanged(t, s, tmpl)
	})

	t.Run("multipart", func(t *testing.T) {
		srv, s := newCommitTestServer(t, newCommitTestStorage(t))
		tmpl := seedCommittedTemplate(t, srv, "tpl-hc-multipart", store.TemplateScopeGlobal, "", map[string]string{"scion-agent.yaml": commitCfgOld})
		w := httptest.NewRecorder()
		// A valid overwrite in the same request must not reach storage
		// either: every part is checked before any is uploaded.
		srv.Handler().ServeHTTP(w, templateMultipartRequest(t, tmpl.ID, map[string][]byte{
			"scion-agent.yaml": []byte(commitCfgBoth),
			hcPath:             []byte(unusableHCConfig),
		}))
		mustStatus(t, w, http.StatusUnprocessableEntity)
		assertUnchanged(t, s, tmpl)
		if objectExists(t, srv.GetStorage(), blobObjectPath(tmpl, commitCfgBoth)) {
			t.Error("multipart stored the valid part before refusing the request")
		}
	})

	t.Run("finalize", func(t *testing.T) {
		srv, s := newCommitTestServer(t, newCommitTestStorage(t))
		tmpl := seedCommittedTemplate(t, srv, "tpl-hc-finalize", store.TemplateScopeGlobal, "", map[string]string{"scion-agent.yaml": commitCfgOld})
		next := map[string]string{"scion-agent.yaml": commitCfgBoth, hcPath: unusableHCConfig}
		stageObjects(t, srv, tmpl, next)
		w := finalizeTemplate(t, srv, tmpl.ID, commitManifest(next))
		mustStatus(t, w, http.StatusUnprocessableEntity)
		if !strings.Contains(w.Body.String(), harnessConfigUnusableErrorCode) {
			t.Errorf("422 body lacks %s: %s", harnessConfigUnusableErrorCode, w.Body.String())
		}
		assertUnchanged(t, s, tmpl)
	})

	// readObject returns a stored object's content, or nil when it is gone.
	readObject := func(t *testing.T, stor storage.Storage, objectPath string) []byte {
		t.Helper()
		rc, _, err := stor.Download(ctx, objectPath)
		if err != nil {
			return nil
		}
		defer func() { _ = rc.Close() }()
		data, err := io.ReadAll(rc)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	// assertStorageUnchanged checks that a refused import wrote nothing to
	// the template's storage: the old objects keep their content and the
	// refused files never arrive.
	assertStorageUnchanged := func(t *testing.T, stor storage.Storage, tmpl *store.Template) {
		t.Helper()
		if got := readObject(t, stor, blobObjectPath(tmpl, commitCfgOld)); string(got) != commitCfgOld {
			t.Errorf("stored scion-agent.yaml = %q, want the old content", got)
		}
		if got := readObject(t, stor, blobObjectPath(tmpl, "kept")); string(got) != "kept" {
			t.Errorf("stored old.md = %q, want it kept", got)
		}
		if objectExists(t, stor, blobObjectPath(tmpl, unusableHCConfig)) {
			t.Error("refused bundled harness-config reached storage")
		}
	}
	seedFiles := map[string]string{"scion-agent.yaml": commitCfgOld, "old.md": "kept"}
	refusedFiles := map[string]string{"scion-agent.yaml": commitCfgBoth, hcPath: unusableHCConfig}

	t.Run("bootstrap", func(t *testing.T) {
		srv, s := newCommitTestServer(t, newCommitTestStorage(t))
		// An existing template whose new on-disk content bundles a bad
		// harness-config: the sync is skipped and neither the row nor
		// storage changes.
		tmpl := seedCommittedTemplate(t, srv, "tpl-hc-boot", store.TemplateScopeGlobal, "", seedFiles)
		parent := t.TempDir()
		writeTemplateDir(t, parent, "tpl-hc-boot", refusedFiles)
		// A new template with the same problem gets no row at all.
		writeTemplateDir(t, parent, "tpl-hc-new", refusedFiles)
		if err := srv.BootstrapTemplatesFromDir(ctx, parent); err != nil {
			t.Fatalf("bootstrap must not fail on a refused template: %v", err)
		}
		assertUnchanged(t, s, tmpl)
		assertStorageUnchanged(t, srv.GetStorage(), tmpl)
		if got, err := s.GetTemplateBySlug(ctx, "tpl-hc-new", store.TemplateScopeGlobal, ""); err == nil {
			t.Errorf("refused new template got a row: %+v", got)
		}
		newPath := storage.ResourceStoragePath(srv.HubID(), storage.ResourceKindTemplate, store.TemplateScopeGlobal, "", "tpl-hc-new")
		if objectExists(t, srv.GetStorage(), newPath+"/scion-agent.yaml") {
			t.Error("refused new template's files reached storage")
		}
		if listed, err := srv.GetStorage().List(ctx, storage.ListOptions{Prefix: newPath + "."}); err != nil || len(listed.Objects) != 0 {
			t.Errorf("refused new template's blobs reached storage: %v %v", listed, err)
		}
		if _, err := s.GetHarnessConfigBySlug(ctx, "bad", store.HarnessConfigScopeGlobal, ""); err == nil {
			t.Error("refused template's bundled harness-config was imported")
		}
	})

	t.Run("reimport", func(t *testing.T) {
		srv, s := newCommitTestServer(t, newCommitTestStorage(t))
		tmpl := seedCommittedTemplate(t, srv, "tpl-hc-reimport", store.TemplateScopeGlobal, "", seedFiles)
		dir := writeTemplateDir(t, t.TempDir(), "tpl-hc-reimport", refusedFiles)
		if _, err := srv.targetTemplateStore(tmpl.ID).Bootstrap(ctx, tmpl.Name, dir, tmpl.Scope, tmpl.ScopeID, "", true); err == nil {
			t.Fatal("reimport of a template bundling an unusable harness-config succeeded")
		}
		assertUnchanged(t, s, tmpl)
		assertStorageUnchanged(t, srv.GetStorage(), tmpl)
	})

	t.Run("bootstrap from source", func(t *testing.T) {
		srv, s := newCommitTestServer(t, newCommitTestStorage(t))
		br := testBundledResource(storage.ResourceKindTemplate, "tpl-hc-source", refusedFiles)
		if _, err := srv.templateStore().BootstrapSource(ctx, NewFSResourceSource(br), BootstrapOptions{}); err == nil {
			t.Fatal("BootstrapSource of a template bundling an unusable harness-config succeeded")
		}
		if got, err := s.GetTemplateBySlug(ctx, "tpl-hc-source", store.TemplateScopeGlobal, ""); err == nil {
			t.Errorf("refused source template got a row: %+v", got)
		}
	})
}

// TestTemplateCommit_AgentConfigNotWritableByAPI covers acceptance 9's API
// half: create and PUT bodies cannot set AgentConfig. (The store round trip
// is TestTemplateAgentConfigRoundTrip in pkg/store/entadapter.)
func TestTemplateCommit_AgentConfigNotWritableByAPI(t *testing.T) {
	ctx := context.Background()
	srv, s := newCommitTestServer(t, newCommitTestStorage(t))

	injected := `"agentConfig":{"model":"injected","default_harness_config":"evil"}`
	w := doTemplateRequest(t, srv, http.MethodPost, "/api/v1/templates", "application/json",
		[]byte(`{"name":"tpl-api","scope":"global",`+injected+`}`))
	mustStatus(t, w, http.StatusCreated)
	var created CreateTemplateResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetTemplate(ctx, created.Template.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AgentConfig != nil {
		t.Errorf("create accepted agentConfig: %+v", got.AgentConfig)
	}

	tmpl := seedCommittedTemplate(t, srv, "tpl-put", store.TemplateScopeGlobal, "", map[string]string{"scion-agent.yaml": commitCfgBoth})
	w = doTemplateRequest(t, srv, http.MethodPut, "/api/v1/templates/"+tmpl.ID, "application/json",
		[]byte(`{"name":"tpl-put",`+injected+`}`))
	mustStatus(t, w, http.StatusOK)
	got, err = s.GetTemplate(ctx, tmpl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AgentConfig == nil || got.AgentConfig.Model != "opus" || got.AgentConfig.DefaultHarnessConfig != "claude-web" {
		t.Errorf("PUT changed AgentConfig: %+v", got.AgentConfig)
	}
}

// TestDeriveTemplateIndex covers the derivation rules directly.
func TestDeriveTemplateIndex(t *testing.T) {
	tests := []struct {
		name, file, content, templateName string
		wantHarness, wantDHC              string
		wantSnapshot                      bool
	}{
		{"harness_config only", "scion-agent.yaml", "harness_config: claude-web\n", "my-template", "claude", "claude-web", true},
		{"default_harness_config only", "scion-agent.yaml", "default_harness_config: gemini-web\n", "my-template", "gemini-cli", "gemini-web", true},
		{"hyphenated keys normalized", "scion-agent.yaml", "default-harness-config: gemini-pro\n", "my-template", "gemini-cli", "gemini-pro", true},
		{"both keys: default_harness_config wins (E5)", "scion-agent.yaml", "harness_config: claude-web\ndefault_harness_config: gemini-web\n", "my-template", "gemini-cli", "gemini-web", true},
		{"explicit harness wins over inference", "scion-agent.yaml", "harness: codex\ndefault_harness_config: claude-web\n", "my-template", "codex", "claude-web", true},
		{"explicit harness only", "scion-agent.yaml", "harness: codex\n", "my-template", "codex", "", true},
		{"unknown config name falls back to template name", "scion-agent.yaml", "default_harness_config: adk\n", "claude-adk", "claude", "adk", true},
		{"no keys falls back to template name", "scion-agent.yaml", "env:\n  FOO: bar\n", "claude-default", "claude", "", true},
		{"no match returns empty harness", "scion-agent.yaml", "env:\n  FOO: bar\n", "custom", "", "", true},
		{"invalid yaml derives from name", "scion-agent.yaml", ": invalid: yaml: [", "gemini-template", "gemini-cli", "", false},
		{"json config", "scion-agent.json", `{"default_harness_config":"claude-web"}`, "x", "claude", "claude-web", true},
		{"no file", "", "", "opencode-x", "opencode", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := deriveTemplateIndex([]byte(tt.content), tt.file, tt.templateName)
			if got.Harness != tt.wantHarness || got.DefaultHarnessConfig != tt.wantDHC || (got.AgentConfig != nil) != tt.wantSnapshot {
				t.Errorf("deriveTemplateIndex = {%q, %q, snapshot %v}, want {%q, %q, snapshot %v}",
					got.Harness, got.DefaultHarnessConfig, got.AgentConfig != nil, tt.wantHarness, tt.wantDHC, tt.wantSnapshot)
			}
		})
	}
}

// TestTemplateCommit_CloneUnusableBundledHarnessConfig: cloning a template
// whose bundled harness-config is unusable is refused with 422
// harness_config_unusable on both template clone and project clone, and a
// refused project clone rolls back the templates it already created.
func TestTemplateCommit_CloneUnusableBundledHarnessConfig(t *testing.T) {
	ctx := context.Background()
	badFiles := map[string]string{"scion-agent.yaml": commitCfgBoth, "harness-configs/bad/config.yaml": unusableHCConfig}

	t.Run("template clone", func(t *testing.T) {
		srv, s := newCommitTestServer(t, newCommitTestStorage(t))
		src := seedUncommittedTemplate(t, srv, s, "", "tpl-bad-src", store.TemplateScopeGlobal, "", badFiles)
		body, _ := json.Marshal(CloneTemplateRequest{Name: "tpl-bad-dst", Scope: store.TemplateScopeGlobal})
		w := doTemplateRequest(t, srv, http.MethodPost, "/api/v1/templates/"+src.ID+"/clone", "application/json", body)
		mustStatus(t, w, http.StatusUnprocessableEntity)
		if !strings.Contains(w.Body.String(), harnessConfigUnusableErrorCode) {
			t.Errorf("422 body lacks %s: %s", harnessConfigUnusableErrorCode, w.Body.String())
		}
		if _, err := s.GetTemplateBySlug(ctx, "tpl-bad-dst", store.TemplateScopeGlobal, ""); err == nil {
			t.Error("refused clone created a template row")
		}
	})

	t.Run("project clone", func(t *testing.T) {
		srv, s := newCommitTestServer(t, newCommitTestStorage(t))
		project := &store.Project{
			ID: api.NewUUID(), Name: "Clone Source", Slug: "clone-source",
			OwnerID: DevUserID, CreatedBy: DevUserID,
		}
		if err := s.CreateProject(ctx, project); err != nil {
			t.Fatal(err)
		}
		// The list is newest first, ties broken by ID descending. The
		// refused template is created first with the lowest possible ID, so
		// the usable one is always copied before it and the rollback is
		// exercised. The precondition is asserted, not assumed.
		seedUncommittedTemplate(t, srv, s, "00000000-0000-4000-8000-000000000001", "b-bad", store.TemplateScopeProject, project.ID, badFiles)
		good := seedCommittedTemplate(t, srv, "a-good", store.TemplateScopeProject, project.ID, map[string]string{"scion-agent.yaml": commitCfgBoth})
		srcList, err := s.ListTemplates(ctx, store.TemplateFilter{Scope: store.TemplateScopeProject, ScopeID: project.ID}, store.ListOptions{Limit: 500})
		if err != nil {
			t.Fatal(err)
		}
		if len(srcList.Items) != 2 || srcList.Items[0].ID != good.ID {
			t.Fatalf("precondition: the usable template must be listed (and copied) first, got %+v", srcList.Items)
		}

		body, _ := json.Marshal(map[string]string{"name": "Clone Target"})
		w := doTemplateRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/clone", "application/json", body)
		mustStatus(t, w, http.StatusUnprocessableEntity)
		if !strings.Contains(w.Body.String(), harnessConfigUnusableErrorCode) {
			t.Errorf("422 body lacks %s: %s", harnessConfigUnusableErrorCode, w.Body.String())
		}

		list, err := s.ListTemplates(ctx, store.TemplateFilter{Scope: store.TemplateScopeProject}, store.ListOptions{Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		for _, tmpl := range list.Items {
			if tmpl.ScopeID != project.ID {
				t.Errorf("refused project clone left template %q in scope %q", tmpl.Name, tmpl.ScopeID)
			}
		}
	})
}

// setTemplateValidationExperiment turns hub.template_config_validation on or
// off on srv by replacing its experiment registry while preserving any other
// experiment defaults already configured on srv.
func setTemplateValidationExperiment(t *testing.T, srv *Server, on bool) {
	t.Helper()
	base := experiments.Default().All()
	if srv.experiments != nil {
		base = srv.experiments.All()
	}
	var active []experiments.Experiment
	for _, e := range base {
		if e.Name == experiments.TemplateConfigValidation {
			e.Default = on
		}
		active = append(active, e)
	}
	reg, err := experiments.NewRegistry(active, nil)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	srv.experiments = reg
	if got := srv.experimentEnabled(experiments.TemplateConfigValidation); got != on {
		t.Fatalf("experimentEnabled(%s) = %v, want %v", experiments.TemplateConfigValidation, got, on)
	}
}

// TestTemplateCommit_AgentConfigValidation covers every Hub commit path for
// template agent config schema validation (ptone/scion#7579) with
// hub.template_config_validation both on and off.
func TestTemplateCommit_AgentConfigValidation(t *testing.T) {
	ctx := context.Background()
	const (
		invalidDurationYAML = "default_harness_config: claude\nmax_duration: \"2 hours\"\n"
		invalidHyphenYAML   = "default_harness_config: claude\nauth-selectedType: none\n"
		invalidCaseJSON     = `{"default_harness_config":"claude","AUTH_SELECTEDTYPE":"none"}`
		unparsableYAML      = ": invalid: yaml: ["
	)

	assertUnchanged := func(t *testing.T, s store.Store, before *store.Template) {
		t.Helper()
		got, err := s.GetTemplate(ctx, before.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.ContentHash != before.ContentHash || len(got.Files) != len(before.Files) || got.SourceURL != before.SourceURL {
			t.Errorf("row changed after refused commit: hash %q -> %q, files %d -> %d, sourceURL %q -> %q",
				before.ContentHash, got.ContentHash, len(before.Files), len(got.Files), before.SourceURL, got.SourceURL)
		}
	}

	assert422InvalidConfig := func(t *testing.T, w *httptest.ResponseRecorder, wantField string) {
		t.Helper()
		mustStatus(t, w, http.StatusUnprocessableEntity)
		var resp struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
				Details struct {
					Findings []struct {
						Path    string `json:"path"`
						Message string `json:"message"`
					} `json:"findings"`
				} `json:"details"`
			} `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode 422 body: %v: %s", err, w.Body.String())
		}
		if resp.Error.Code != ErrCodeInvalidAgentConfig {
			t.Errorf("error.code = %q, want %q", resp.Error.Code, ErrCodeInvalidAgentConfig)
		}
		if !strings.Contains(resp.Error.Message, wantField) {
			t.Errorf("error.message %q does not name %q", resp.Error.Message, wantField)
		}
		if len(resp.Error.Details.Findings) == 0 {
			t.Errorf("422 response missing details.findings: %s", w.Body.String())
		}
	}

	t.Run("finalize_valid_and_invalid_variants", func(t *testing.T) {
		for _, tc := range []struct {
			name      string
			file      string
			content   string
			wantField string
		}{
			{"invalid max_duration", "scion-agent.yaml", invalidDurationYAML, "max_duration"},
			{"hyphenated auth-selectedType", "scion-agent.yaml", invalidHyphenYAML, "auth_selectedType"},
			{"case-variant json AUTH_SELECTEDTYPE", "scion-agent.json", invalidCaseJSON, "auth_selectedType"},
			{"unparsable yaml", "scion-agent.yaml", unparsableYAML, "failed to parse agent config scion-agent.yaml"},
		} {
			t.Run(tc.name+"/flag_on", func(t *testing.T) {
				srv, s := newCommitTestServer(t, newCommitTestStorage(t))
				setTemplateValidationExperiment(t, srv, true)
				tmpl := seedCommittedTemplate(t, srv, "tpl-val-fin", store.TemplateScopeGlobal, "", map[string]string{"scion-agent.yaml": commitCfgOld})
				next := map[string]string{tc.file: tc.content}
				stageObjects(t, srv, tmpl, next)
				w := finalizeTemplate(t, srv, tmpl.ID, commitManifest(next))
				assert422InvalidConfig(t, w, tc.wantField)
				assertUnchanged(t, s, tmpl)
			})
			t.Run(tc.name+"/flag_off", func(t *testing.T) {
				srv, _ := newCommitTestServer(t, newCommitTestStorage(t))
				setTemplateValidationExperiment(t, srv, false)
				logs := captureSlogDefault(t)
				tmpl := seedCommittedTemplate(t, srv, "tpl-val-fin-off", store.TemplateScopeGlobal, "", map[string]string{"scion-agent.yaml": commitCfgOld})
				next := map[string]string{tc.file: tc.content}
				stageObjects(t, srv, tmpl, next)
				mustStatus(t, finalizeTemplate(t, srv, tmpl.ID, commitManifest(next)), http.StatusOK)
				if !strings.Contains(logs.String(), "agent config failed schema validation") {
					t.Errorf("expected warning log when flag off, got:\n%s", logs.String())
				}
			})
		}
	})

	t.Run("file_write_raw_multipart_no_blob_on_refusal", func(t *testing.T) {
		for _, mode := range []string{"json", "raw", "multipart"} {
			t.Run(mode+"/flag_on", func(t *testing.T) {
				srv, s := newCommitTestServer(t, newCommitTestStorage(t))
				setTemplateValidationExperiment(t, srv, true)
				tmpl := seedCommittedTemplate(t, srv, "tpl-val-"+mode, store.TemplateScopeGlobal, "", map[string]string{"scion-agent.yaml": commitCfgOld})
				var w *httptest.ResponseRecorder
				switch mode {
				case "json":
					body, _ := json.Marshal(TemplateFileWriteRequest{Content: invalidDurationYAML})
					w = doTemplateRequest(t, srv, http.MethodPut, "/api/v1/templates/"+tmpl.ID+"/files/scion-agent.yaml", "application/json", body)
				case "raw":
					w = doTemplateRequest(t, srv, http.MethodPut, "/api/v1/templates/"+tmpl.ID+"/files/scion-agent.yaml", "application/octet-stream", []byte(invalidDurationYAML))
				case "multipart":
					w = httptest.NewRecorder()
					srv.Handler().ServeHTTP(w, templateMultipartRequest(t, tmpl.ID, map[string][]byte{
						"scion-agent.yaml": []byte(invalidDurationYAML),
						"other.md":         []byte("should not be stored"),
					}))
				}
				assert422InvalidConfig(t, w, "max_duration")
				assertUnchanged(t, s, tmpl)
				if objectExists(t, srv.GetStorage(), blobObjectPath(tmpl, invalidDurationYAML)) {
					t.Error("refused invalid agent config reached storage")
				}
				if mode == "multipart" && objectExists(t, srv.GetStorage(), blobObjectPath(tmpl, "should not be stored")) {
					t.Error("multipart stored sibling part before refusing invalid agent config")
				}
			})
			t.Run(mode+"/flag_off", func(t *testing.T) {
				srv, _ := newCommitTestServer(t, newCommitTestStorage(t))
				setTemplateValidationExperiment(t, srv, false)
				logs := captureSlogDefault(t)
				tmpl := seedCommittedTemplate(t, srv, "tpl-val-off-"+mode, store.TemplateScopeGlobal, "", map[string]string{"scion-agent.yaml": commitCfgOld})
				var w *httptest.ResponseRecorder
				switch mode {
				case "json":
					body, _ := json.Marshal(TemplateFileWriteRequest{Content: invalidDurationYAML})
					w = doTemplateRequest(t, srv, http.MethodPut, "/api/v1/templates/"+tmpl.ID+"/files/scion-agent.yaml", "application/json", body)
				case "raw":
					w = doTemplateRequest(t, srv, http.MethodPut, "/api/v1/templates/"+tmpl.ID+"/files/scion-agent.yaml", "application/octet-stream", []byte(invalidDurationYAML))
				case "multipart":
					w = httptest.NewRecorder()
					srv.Handler().ServeHTTP(w, templateMultipartRequest(t, tmpl.ID, map[string][]byte{
						"scion-agent.yaml": []byte(invalidDurationYAML),
					}))
				}
				mustStatus(t, w, http.StatusOK)
				if !strings.Contains(logs.String(), "agent config failed schema validation") {
					t.Errorf("expected warning log when flag off, got:\n%s", logs.String())
				}
			})
		}
	})

	t.Run("file_delete_exposing_invalid_json_refused", func(t *testing.T) {
		for _, on := range []bool{true, false} {
			name := "flag_off"
			if on {
				name = "flag_on"
			}
			t.Run(name, func(t *testing.T) {
				srv, s := newCommitTestServer(t, newCommitTestStorage(t))
				// Seed with valid yaml + invalid json fallback while flag is off.
				tmpl := seedCommittedTemplate(t, srv, "tpl-del-fallback", store.TemplateScopeGlobal, "", map[string]string{
					"scion-agent.yaml": commitCfgOld,
					"scion-agent.json": invalidCaseJSON,
				})
				setTemplateValidationExperiment(t, srv, on)
				logs := captureSlogDefault(t)
				w := doTemplateRequest(t, srv, http.MethodDelete, "/api/v1/templates/"+tmpl.ID+"/files/scion-agent.yaml", "", nil)
				if on {
					assert422InvalidConfig(t, w, "auth_selectedType")
					assertUnchanged(t, s, tmpl)
				} else {
					mustStatus(t, w, http.StatusNoContent)
					if !strings.Contains(logs.String(), "agent config failed schema validation") {
						t.Errorf("expected warning log when flag off, got:\n%s", logs.String())
					}
				}
			})
		}
	})

	t.Run("clone_and_unchanged_stored_invalid_not_refused", func(t *testing.T) {
		srv, s := newCommitTestServer(t, newCommitTestStorage(t))
		// Seed a stored invalid template while the flag is off.
		src := seedCommittedTemplate(t, srv, "tpl-stored-bad", store.TemplateScopeGlobal, "", map[string]string{
			"scion-agent.yaml": invalidDurationYAML,
			"extra.md":         "hello",
		})
		// Now turn the flag ON.
		setTemplateValidationExperiment(t, srv, true)

		// 1. Template clone must succeed (D3: clone counts as unchanged content).
		body, _ := json.Marshal(CloneTemplateRequest{Name: "tpl-stored-bad-clone", Scope: store.TemplateScopeGlobal})
		mustStatus(t, doTemplateRequest(t, srv, http.MethodPost, "/api/v1/templates/"+src.ID+"/clone", "application/json", body), http.StatusCreated)

		// 2. Non-config file delete must succeed (effective agent config unchanged).
		mustStatus(t, doTemplateRequest(t, srv, http.MethodDelete, "/api/v1/templates/"+src.ID+"/files/extra.md", "", nil), http.StatusNoContent)

		// 3. Metadata update must succeed.
		metaBody, _ := json.Marshal(map[string]string{"name": "tpl-stored-bad", "description": "updated"})
		mustStatus(t, doTemplateRequest(t, srv, http.MethodPut, "/api/v1/templates/"+src.ID, "application/json", metaBody), http.StatusOK)

		// 4. Project clone with a stored invalid template must succeed.
		project := &store.Project{
			ID: api.NewUUID(), Name: "Bad Tpl Project", Slug: "bad-tpl-project",
			OwnerID: DevUserID, CreatedBy: DevUserID,
		}
		if err := s.CreateProject(ctx, project); err != nil {
			t.Fatal(err)
		}
		setTemplateValidationExperiment(t, srv, false)
		seedCommittedTemplate(t, srv, "proj-bad", store.TemplateScopeProject, project.ID, map[string]string{"scion-agent.yaml": invalidDurationYAML})
		setTemplateValidationExperiment(t, srv, true)
		pBody, _ := json.Marshal(map[string]string{"name": "Cloned Project"})
		mustStatus(t, doTemplateRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/clone", "application/json", pBody), http.StatusCreated)
	})

	t.Run("reimport_preserves_prior_content_and_sourceURL", func(t *testing.T) {
		srv, s := newCommitTestServer(t, newCommitTestStorage(t))
		origURL := "https://github.com/acme/repo/tree/main/templates/tpl-reimp-val"
		seedDir := writeTemplateDir(t, t.TempDir(), "tpl-reimp-val", map[string]string{"scion-agent.yaml": commitCfgOld})
		if _, err := srv.templateStore().Bootstrap(ctx, "tpl-reimp-val", seedDir, store.TemplateScopeGlobal, "", origURL, true); err != nil {
			t.Fatal(err)
		}
		tmpl, err := s.GetTemplateBySlug(ctx, "tpl-reimp-val", store.TemplateScopeGlobal, "")
		if err != nil {
			t.Fatal(err)
		}
		if tmpl.SourceURL != origURL {
			t.Fatalf("seed SourceURL = %q, want %q", tmpl.SourceURL, origURL)
		}
		setTemplateValidationExperiment(t, srv, true)

		dir := writeTemplateDir(t, t.TempDir(), "tpl-reimp-val", map[string]string{"scion-agent.yaml": invalidDurationYAML})
		_, err = srv.targetTemplateStore(tmpl.ID).Bootstrap(ctx, tmpl.Name, dir, tmpl.Scope, tmpl.ScopeID, "https://github.com/acme/other/tree/main/t", true)
		if err == nil {
			t.Fatal("expected reimport of invalid agent config to fail when flag is on")
		}
		if !strings.Contains(err.Error(), "max_duration") {
			t.Errorf("reimport error %q does not name max_duration", err.Error())
		}
		assertUnchanged(t, s, tmpl)
	})

	t.Run("bootstrap_skips_invalid_and_continues", func(t *testing.T) {
		for _, on := range []bool{true, false} {
			name := "flag_off"
			if on {
				name = "flag_on"
			}
			t.Run(name, func(t *testing.T) {
				srv, s := newCommitTestServer(t, newCommitTestStorage(t))
				setTemplateValidationExperiment(t, srv, on)
				logs := captureSlogDefault(t)

				parent := t.TempDir()
				writeTemplateDir(t, parent, "good-boot", map[string]string{"scion-agent.yaml": commitCfgBoth})
				writeTemplateDir(t, parent, "bad-boot", map[string]string{"scion-agent.yaml": invalidDurationYAML})

				if err := srv.BootstrapTemplatesFromDir(ctx, parent); err != nil {
					t.Fatalf("BootstrapTemplatesFromDir must continue past invalid template: %v", err)
				}
				if _, err := s.GetTemplateBySlug(ctx, "good-boot", store.TemplateScopeGlobal, ""); err != nil {
					t.Fatalf("valid sibling template not imported: %v", err)
				}
				_, badErr := s.GetTemplateBySlug(ctx, "bad-boot", store.TemplateScopeGlobal, "")
				if on && badErr == nil {
					t.Error("invalid template was imported while flag is on")
				}
				if !on {
					if badErr != nil {
						t.Errorf("invalid template should be imported while flag is off: %v", badErr)
					}
					if !strings.Contains(logs.String(), "agent config failed schema validation") {
						t.Errorf("expected warning log when flag off, got:\n%s", logs.String())
					}
				}
			})
		}
	})

	t.Run("bootstrap_source", func(t *testing.T) {
		srv, s := newCommitTestServer(t, newCommitTestStorage(t))
		setTemplateValidationExperiment(t, srv, true)

		bad := testBundledResource(storage.ResourceKindTemplate, "src-bad", map[string]string{"scion-agent.yaml": invalidDurationYAML})
		if _, err := srv.templateStore().BootstrapSource(ctx, NewFSResourceSource(bad), BootstrapOptions{}); err == nil {
			t.Fatal("BootstrapSource accepted invalid agent config while flag is on")
		}
		if _, err := s.GetTemplateBySlug(ctx, "src-bad", store.TemplateScopeGlobal, ""); err == nil {
			t.Error("refused BootstrapSource template created a row")
		}

		good := testBundledResource(storage.ResourceKindTemplate, "src-good", map[string]string{"scion-agent.yaml": commitCfgBoth})
		if _, err := srv.templateStore().BootstrapSource(ctx, NewFSResourceSource(good), BootstrapOptions{}); err != nil {
			t.Fatalf("BootstrapSource refused valid template: %v", err)
		}
	})

	t.Run("checkdir_nil_server_does_not_panic", func(t *testing.T) {
		dir := writeTemplateDir(t, t.TempDir(), "nil-srv-tpl", map[string]string{"scion-agent.yaml": commitCfgBoth})
		rs := &ResourceStore{}
		cd, err := rs.collectFiles(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = cd.root.Close() }()
		if err := (&templatePersistence{}).CheckDir(ctx, "nil-srv-tpl", cd); err != nil {
			t.Fatalf("CheckDir with nil server returned unexpected error: %v", err)
		}
	})

	t.Run("unchanged_stored_invalid_allowed_on_bootstrap_and_reimport_flag_on", func(t *testing.T) {
		srv, s := newCommitTestServer(t, newCommitTestStorage(t))
		dir := writeTemplateDir(t, t.TempDir(), "unchanged-bad", map[string]string{
			"scion-agent.yaml": invalidDurationYAML,
			"README.md":        "v1",
		})
		// Seed while flag is off.
		setTemplateValidationExperiment(t, srv, false)
		if _, err := srv.templateStore().Bootstrap(ctx, "unchanged-bad", dir, store.TemplateScopeGlobal, "", "https://github.com/acme/repo/tree/main/templates/unchanged-bad", true); err != nil {
			t.Fatalf("seed bootstrap failed: %v", err)
		}
		tmpl, err := s.GetTemplateBySlug(ctx, "unchanged-bad", store.TemplateScopeGlobal, "")
		if err != nil {
			t.Fatal(err)
		}

		// Turn flag ON: unchanged effective agent config must NOT be refused.
		setTemplateValidationExperiment(t, srv, true)

		// 1. Workstation/hub-start bootstrap (force=false, hash match).
		if _, err := srv.templateStore().Bootstrap(ctx, "unchanged-bad", dir, store.TemplateScopeGlobal, "", "", false); err != nil {
			t.Fatalf("Bootstrap(force=false) on unchanged stored invalid template refused: %v", err)
		}

		// 2. Identical reimport via targetTemplateStore (force=true).
		if _, err := srv.targetTemplateStore(tmpl.ID).Bootstrap(ctx, tmpl.Name, dir, tmpl.Scope, tmpl.ScopeID, tmpl.SourceURL, true); err != nil {
			t.Fatalf("targetTemplateStore Bootstrap(force=true) on unchanged stored invalid template refused: %v", err)
		}

		// 3. Forced Bootstrap with another file edited (effective agent config still unchanged).
		dirEditedOther := writeTemplateDir(t, t.TempDir(), "unchanged-bad", map[string]string{
			"scion-agent.yaml": invalidDurationYAML,
			"README.md":        "v2",
		})
		if _, err := srv.templateStore().Bootstrap(ctx, "unchanged-bad", dirEditedOther, store.TemplateScopeGlobal, "", "", true); err != nil {
			t.Fatalf("Bootstrap(force=true) with unchanged agent config refused: %v", err)
		}

		// 4. BootstrapSource reseed with unchanged agent config.
		br := testBundledResource(storage.ResourceKindTemplate, "unchanged-bad", map[string]string{
			"scion-agent.yaml": invalidDurationYAML,
			"README.md":        "v3",
		})
		if _, err := srv.templateStore().BootstrapSource(ctx, NewFSResourceSource(br), BootstrapOptions{OverwritePolicy: OverwriteAlways}); err != nil {
			t.Fatalf("BootstrapSource on unchanged stored invalid template refused: %v", err)
		}
	})

	t.Run("staged_raw_upload_defers_agent_config_validation_to_finalize", func(t *testing.T) {
		srv, _ := newCommitTestServer(t, newCommitTestStorage(t))
		setTemplateValidationExperiment(t, srv, true)

		// Create a new pending template (no files yet).
		createBody, _ := json.Marshal(CreateTemplateRequest{Name: "tpl-staged-pair", Scope: store.TemplateScopeGlobal})
		w := doTemplateRequest(t, srv, http.MethodPost, "/api/v1/templates", "application/json", createBody)
		mustStatus(t, w, http.StatusCreated)
		var created CreateTemplateResponse
		if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
			t.Fatal(err)
		}
		const uploadID = "0123456789abcdef"

		// Stage invalid scion-agent.json first (alphabetically before .yaml),
		// then valid scion-agent.yaml. Neither staged PUT is refused, and
		// finalize accepts because scion-agent.yaml is the effective config.
		mustStatus(t, doTemplateRequest(t, srv, http.MethodPut, "/api/v1/templates/"+created.Template.ID+"/files/scion-agent.json?uploadId="+uploadID, "application/octet-stream", []byte(invalidCaseJSON)), http.StatusOK)
		mustStatus(t, doTemplateRequest(t, srv, http.MethodPut, "/api/v1/templates/"+created.Template.ID+"/files/scion-agent.yaml?uploadId="+uploadID, "application/octet-stream", []byte(commitCfgBoth)), http.StatusOK)

		finBody, _ := json.Marshal(FinalizeRequest{
			Manifest: &TemplateManifest{
				Version: "1.0",
				Harness: "claude",
				Files: commitManifest(map[string]string{
					"scion-agent.json": invalidCaseJSON,
					"scion-agent.yaml": commitCfgBoth,
				}),
			},
			UploadID: uploadID,
		})
		mustStatus(t, doTemplateRequest(t, srv, http.MethodPost, "/api/v1/templates/"+created.Template.ID+"/finalize", "application/json", finBody), http.StatusOK)

		// Conversely, staging only an invalid scion-agent.json succeeds on PUT
		// and is refused at finalize with 422 invalid_agent_config.
		createBody2, _ := json.Marshal(CreateTemplateRequest{Name: "tpl-staged-only-bad", Scope: store.TemplateScopeGlobal})
		w2 := doTemplateRequest(t, srv, http.MethodPost, "/api/v1/templates", "application/json", createBody2)
		mustStatus(t, w2, http.StatusCreated)
		var created2 CreateTemplateResponse
		if err := json.Unmarshal(w2.Body.Bytes(), &created2); err != nil {
			t.Fatal(err)
		}
		mustStatus(t, doTemplateRequest(t, srv, http.MethodPut, "/api/v1/templates/"+created2.Template.ID+"/files/scion-agent.json?uploadId="+uploadID, "application/octet-stream", []byte(invalidCaseJSON)), http.StatusOK)
		finBody2, _ := json.Marshal(FinalizeRequest{
			Manifest: &TemplateManifest{
				Version: "1.0",
				Harness: "claude",
				Files:   commitManifest(map[string]string{"scion-agent.json": invalidCaseJSON}),
			},
			UploadID: uploadID,
		})
		assert422InvalidConfig(t, doTemplateRequest(t, srv, http.MethodPost, "/api/v1/templates/"+created2.Template.ID+"/finalize", "application/json", finBody2), "auth_selectedType")
	})
}
