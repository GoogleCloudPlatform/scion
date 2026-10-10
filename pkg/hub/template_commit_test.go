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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Tests for the single template commit path (ptone/scion#4217).

// commitCfgBoth sets both harness-config keys. The broker's order (decision
// E5) resolves default_harness_config first, so the expected index is
// claude-web / claude. task and branch are per-agent fields the snapshot
// drops.
const (
	commitCfgOld  = "default_harness_config: gemini-web\n"
	commitCfgBoth = "harness_config: gemini-web\ndefault_harness_config: claude-web\nmodel: opus\ntask: per-agent task\nbranch: per-agent-branch\n"

	unusableHCConfig = "harness: claude\nprovisioner:\n  type: builtin\n"
)

// assertBothIndex checks the index derived from commitCfgBoth. The values
// are written out rather than computed with deriveTemplateIndex so a change
// to the derivation shows up here.
func assertBothIndex(t *testing.T, got *store.Template) {
	t.Helper()
	if got.DefaultHarnessConfig != "claude-web" {
		t.Errorf("DefaultHarnessConfig = %q, want %q (default_harness_config wins, E5)", got.DefaultHarnessConfig, "claude-web")
	}
	if got.Harness != "claude" {
		t.Errorf("Harness = %q, want %q", got.Harness, "claude")
	}
	if got.AgentConfig == nil {
		t.Fatal("AgentConfig = nil, want the parsed scion-agent.yaml")
	}
	if got.AgentConfig.Model != "opus" || got.AgentConfig.HarnessConfig != "gemini-web" || got.AgentConfig.DefaultHarnessConfig != "claude-web" {
		t.Errorf("AgentConfig = %+v, want model opus, harness_config gemini-web, default_harness_config claude-web", got.AgentConfig)
	}
	if got.AgentConfig.Task != "" || got.AgentConfig.Branch != "" {
		t.Errorf("AgentConfig kept per-agent fields: task %q, branch %q", got.AgentConfig.Task, got.AgentConfig.Branch)
	}
}

func newCommitTestStorage(t *testing.T) storage.Storage {
	t.Helper()
	stor, err := storage.NewLocal(storage.Config{Provider: storage.ProviderLocal, Bucket: "b", LocalPath: t.TempDir()})
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	return stor
}

func newCommitTestServer(t *testing.T, stor storage.Storage) (*Server, store.Store) {
	t.Helper()
	s, err := newTestStore(t, ":memory:")
	if err != nil {
		if strings.Contains(err.Error(), "sqlite driver not registered") {
			t.Skip("Skipping: sqlite driver not registered")
		}
		t.Fatalf("failed to create test store: %v", err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate: %v", err)
	}
	cfg := DefaultServerConfig()
	cfg.DevAuthToken = testDevToken
	srv, err := newTestHubServer(t, cfg, s)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	srv.SetStorage(stor)
	return srv, s
}

func commitFileHash(content string) string {
	h := sha256.Sum256([]byte(content))
	return "sha256:" + hex.EncodeToString(h[:])
}

// commitManifest builds a sorted manifest for files.
func commitManifest(files map[string]string) []store.TemplateFile {
	out := make([]store.TemplateFile, 0, len(files))
	for p, c := range files {
		out = append(out, store.TemplateFile{Path: p, Size: int64(len(c)), Hash: commitFileHash(c), Mode: "0644"})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func putObjects(t *testing.T, stor storage.Storage, storagePath string, files map[string]string) {
	t.Helper()
	for p, c := range files {
		if _, err := stor.Upload(context.Background(), storagePath+"/"+p, strings.NewReader(c), storage.UploadOptions{}); err != nil {
			t.Fatalf("upload %s: %v", p, err)
		}
	}
}

func objectExists(t *testing.T, stor storage.Storage, objectPath string) bool {
	t.Helper()
	ok, err := stor.Exists(context.Background(), objectPath)
	if err != nil {
		t.Fatalf("Exists(%s): %v", objectPath, err)
	}
	return ok
}

// seedCommittedTemplate creates a template with files through the commit path.
func seedCommittedTemplate(t *testing.T, srv *Server, name, scope, scopeID string, files map[string]string) *store.Template {
	t.Helper()
	slug := api.Slugify(name)
	tmpl := &store.Template{
		ID:          api.NewUUID(),
		Name:        name,
		Slug:        slug,
		Scope:       scope,
		ScopeID:     scopeID,
		ProjectID:   scopeID,
		Status:      store.TemplateStatusActive,
		StoragePath: storage.TemplateStoragePath(srv.HubID(), scope, scopeID, slug),
	}
	putObjects(t, srv.GetStorage(), tmpl.StoragePath, files)
	if err := srv.commitTemplateFiles(context.Background(), tmpl, commitManifest(files), commitOpts{create: true}); err != nil {
		t.Fatalf("seed commit: %v", err)
	}
	return tmpl
}

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
	if err := s.UpdateTemplate(ctx, tmpl); err != nil {
		t.Fatal(err)
	}
}

func writeTemplateDir(t *testing.T, parent, name string, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(parent, name)
	for p, c := range files {
		fp := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(fp), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fp, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func doTemplateRequest(t *testing.T, srv *Server, method, path, contentType string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Authorization", "Bearer "+testDevToken)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

func finalizeTemplate(t *testing.T, srv *Server, id string, files []store.TemplateFile) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(FinalizeRequest{Manifest: &TemplateManifest{Version: "1.0", Harness: "ignored", Files: files}})
	if err != nil {
		t.Fatal(err)
	}
	return doTemplateRequest(t, srv, http.MethodPost, "/api/v1/templates/"+id+"/finalize", "application/json", body)
}

func mustStatus(t *testing.T, w *httptest.ResponseRecorder, want int) {
	t.Helper()
	if w.Code != want {
		t.Fatalf("status = %d, want %d: %s", w.Code, want, w.Body.String())
	}
}

// TestTemplateCommit_RoutedPaths is the table test over every path routed
// through commitTemplateFiles. Each path commits a scion-agent.yaml that sets
// both harness-config keys and must produce the same index. Paths that drop
// a file also check that its storage object is deleted.
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
				putObjects(t, srv.GetStorage(), tmpl.StoragePath, next)
				mustStatus(t, finalizeTemplate(t, srv, tmpl.ID, commitManifest(next)), http.StatusOK)
				if objectExists(t, srv.GetStorage(), tmpl.StoragePath+"/old.md") {
					t.Error("finalize left the object of a file dropped from the manifest")
				}
				if !objectExists(t, srv.GetStorage(), tmpl.StoragePath+"/CLAUDE.md") {
					t.Error("finalize deleted a kept object")
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
				if objectExists(t, srv.GetStorage(), tmpl.StoragePath+"/old.md") {
					t.Error("delete left the removed object in storage")
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
				tmpl := seedCommittedTemplate(t, srv, "tpl-repair", store.TemplateScopeGlobal, "", map[string]string{"scion-agent.yaml": commitCfgOld})
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
			if got.ContentHash != computeContentHash(got.Files) {
				t.Errorf("ContentHash %q does not match the manifest", got.ContentHash)
			}
		})
	}
}

// TestTemplateCommit_DeleteAgentConfigClearsIndex covers acceptance 2:
// deleting scion-agent.yaml clears DefaultHarnessConfig and AgentConfig and
// takes Harness from the template name, and the object is deleted.
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
	if objectExists(t, srv.GetStorage(), tmpl.StoragePath+"/scion-agent.yaml") {
		t.Error("delete left scion-agent.yaml in storage")
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
	idx := deriveTemplateIndexFromDir(dir, "gemini-broken")
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
			putObjects(t, stor, tmpl.StoragePath, push)
		}
		mustStatus(t, finalizeTemplate(t, srv, tmpl.ID, commitManifest(push)), http.StatusOK)
		got, err := s.GetTemplate(ctx, tmpl.ID)
		if err != nil {
			t.Fatal(err)
		}
		if objectExists(t, stor, tmpl.StoragePath+"/old.md") {
			t.Error("dropped object not deleted")
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
		if objectExists(t, srv.GetStorage(), tmpl.StoragePath+"/"+hcPath) {
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
		srv.Handler().ServeHTTP(w, templateMultipartRequest(t, tmpl.ID, map[string][]byte{hcPath: []byte(unusableHCConfig)}))
		mustStatus(t, w, http.StatusUnprocessableEntity)
		assertUnchanged(t, s, tmpl)
	})

	t.Run("finalize", func(t *testing.T) {
		srv, s := newCommitTestServer(t, newCommitTestStorage(t))
		tmpl := seedCommittedTemplate(t, srv, "tpl-hc-finalize", store.TemplateScopeGlobal, "", map[string]string{"scion-agent.yaml": commitCfgOld})
		next := map[string]string{"scion-agent.yaml": commitCfgBoth, hcPath: unusableHCConfig}
		putObjects(t, srv.GetStorage(), tmpl.StoragePath, next)
		w := finalizeTemplate(t, srv, tmpl.ID, commitManifest(next))
		mustStatus(t, w, http.StatusUnprocessableEntity)
		if !strings.Contains(w.Body.String(), harnessConfigUnusableErrorCode) {
			t.Errorf("422 body lacks %s: %s", harnessConfigUnusableErrorCode, w.Body.String())
		}
		assertUnchanged(t, s, tmpl)
	})

	t.Run("bootstrap", func(t *testing.T) {
		srv, s := newCommitTestServer(t, newCommitTestStorage(t))
		// An existing template whose new on-disk content bundles a bad
		// harness-config: the sync is skipped and the row kept.
		tmpl := seedCommittedTemplate(t, srv, "tpl-hc-boot", store.TemplateScopeGlobal, "", map[string]string{"scion-agent.yaml": commitCfgOld})
		parent := t.TempDir()
		writeTemplateDir(t, parent, "tpl-hc-boot", map[string]string{"scion-agent.yaml": commitCfgBoth, hcPath: unusableHCConfig})
		// A new template with the same problem is not activated.
		writeTemplateDir(t, parent, "tpl-hc-new", map[string]string{"scion-agent.yaml": commitCfgBoth, hcPath: unusableHCConfig})
		if err := srv.BootstrapTemplatesFromDir(ctx, parent); err != nil {
			t.Fatalf("bootstrap must not fail on a refused template: %v", err)
		}
		assertUnchanged(t, s, tmpl)
		if got, err := s.GetTemplateBySlug(ctx, "tpl-hc-new", store.TemplateScopeGlobal, ""); err == nil && got.Status == store.TemplateStatusActive {
			t.Errorf("refused new template was activated: %+v", got)
		}
		if _, err := s.GetHarnessConfigBySlug(ctx, "bad", store.HarnessConfigScopeGlobal, ""); err == nil {
			t.Error("refused template's bundled harness-config was imported")
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

// seedUncommittedTemplate creates a template row and its objects directly,
// bypassing the commit path, so a source the commit would refuse (a legacy
// row) can be cloned.
func seedUncommittedTemplate(t *testing.T, srv *Server, s store.Store, name, scope, scopeID string, files map[string]string) *store.Template {
	t.Helper()
	slug := api.Slugify(name)
	tmpl := &store.Template{
		ID:          api.NewUUID(),
		Name:        name,
		Slug:        slug,
		Harness:     "claude",
		Scope:       scope,
		ScopeID:     scopeID,
		ProjectID:   scopeID,
		Status:      store.TemplateStatusActive,
		StoragePath: storage.TemplateStoragePath(srv.HubID(), scope, scopeID, slug),
	}
	putObjects(t, srv.GetStorage(), tmpl.StoragePath, files)
	tmpl.Files = commitManifest(files)
	tmpl.ContentHash = computeContentHash(tmpl.Files)
	if err := s.CreateTemplate(context.Background(), tmpl); err != nil {
		t.Fatal(err)
	}
	return tmpl
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
		src := seedUncommittedTemplate(t, srv, s, "tpl-bad-src", store.TemplateScopeGlobal, "", badFiles)
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
		// The list is newest first, so the usable template (created last) is
		// copied before the refused one; the rollback must remove it too.
		seedUncommittedTemplate(t, srv, s, "b-bad", store.TemplateScopeProject, project.ID, badFiles)
		seedCommittedTemplate(t, srv, "a-good", store.TemplateScopeProject, project.ID, map[string]string{"scion-agent.yaml": commitCfgBoth})

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
