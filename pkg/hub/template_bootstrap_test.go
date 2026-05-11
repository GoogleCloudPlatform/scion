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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/sqlite"
)

// testTemplateBootstrapServer creates a hub Server backed by an in-memory
// SQLite store and a mock storage, suitable for template bootstrap tests.
func testTemplateBootstrapServer(t *testing.T) (*Server, store.Store, *mockStorage) {
	t.Helper()
	s, err := sqlite.New(":memory:")
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
	srv, err := New(cfg, s)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	t.Cleanup(func() { srv.Shutdown(context.Background()) })

	stor := newMockStorage("test-bucket")
	srv.SetStorage(stor)

	return srv, s, stor
}

// makeTemplateDir creates a temp directory with template files and returns
// the parent templates directory. The template is created as a subdirectory
// named templateName.
func makeTemplateDir(t *testing.T, templateName string, files map[string]string) string {
	t.Helper()
	templatesDir := t.TempDir()
	templateDir := filepath.Join(templatesDir, templateName)
	for relPath, content := range files {
		full := filepath.Join(templateDir, relPath)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return templatesDir
}

func TestBootstrapTemplatesFromDir_ImportsTemplates(t *testing.T) {
	srv, s, stor := testTemplateBootstrapServer(t)
	ctx := context.Background()

	templatesDir := makeTemplateDir(t, "my-template", map[string]string{
		"home/.bashrc": "# bashrc content",
		"README.md":    "hello",
	})

	if err := srv.BootstrapTemplatesFromDir(ctx, templatesDir); err != nil {
		t.Fatalf("bootstrap failed: %v", err)
	}

	// Verify a template was created in the store
	result, err := s.ListTemplates(ctx, store.TemplateFilter{}, store.ListOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if result.TotalCount != 1 {
		t.Fatalf("expected 1 template, got %d", result.TotalCount)
	}

	tmpl := result.Items[0]
	if tmpl.Name != "my-template" {
		t.Errorf("expected name 'my-template', got %q", tmpl.Name)
	}
	if tmpl.Status != store.TemplateStatusActive {
		t.Errorf("expected status active, got %q", tmpl.Status)
	}
	if tmpl.Scope != store.TemplateScopeGlobal {
		t.Errorf("expected scope global, got %q", tmpl.Scope)
	}
	if len(tmpl.Files) != 2 {
		t.Errorf("expected 2 files in manifest, got %d", len(tmpl.Files))
	}
	if tmpl.ContentHash == "" {
		t.Error("expected non-empty content hash")
	}

	// Verify files were uploaded to storage
	if len(stor.objects) != 2 {
		t.Errorf("expected 2 objects in storage, got %d", len(stor.objects))
	}
}

func TestBootstrapTemplatesFromDir_ImportsNewAlongsideExisting(t *testing.T) {
	srv, s, stor := testTemplateBootstrapServer(t)
	ctx := context.Background()

	// Pre-create a template in the store
	existing := &store.Template{
		ID:     "existing-id",
		Name:   "existing",
		Slug:   "existing",
		Scope:  store.TemplateScopeGlobal,
		Status: store.TemplateStatusActive,
	}
	if err := s.CreateTemplate(ctx, existing); err != nil {
		t.Fatal(err)
	}

	templatesDir := makeTemplateDir(t, "new-template", map[string]string{
		"file.txt": "content",
	})

	if err := srv.BootstrapTemplatesFromDir(ctx, templatesDir); err != nil {
		t.Fatalf("bootstrap failed: %v", err)
	}

	// Verify the new template was imported alongside the existing one
	result, err := s.ListTemplates(ctx, store.TemplateFilter{}, store.ListOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if result.TotalCount != 2 {
		t.Fatalf("expected 2 templates (existing + new), got %d", result.TotalCount)
	}

	// Verify the new template files were uploaded
	if len(stor.objects) != 1 {
		t.Errorf("expected 1 object in storage (new template file), got %d", len(stor.objects))
	}
}

func TestBootstrapTemplatesFromDir_SyncsChangedTemplate(t *testing.T) {
	srv, s, stor := testTemplateBootstrapServer(t)
	ctx := context.Background()

	// First bootstrap
	templatesDir := makeTemplateDir(t, "my-template", map[string]string{
		"file.txt": "original content",
	})

	if err := srv.BootstrapTemplatesFromDir(ctx, templatesDir); err != nil {
		t.Fatalf("first bootstrap failed: %v", err)
	}

	// Verify initial state
	result, err := s.ListTemplates(ctx, store.TemplateFilter{}, store.ListOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if result.TotalCount != 1 {
		t.Fatalf("expected 1 template, got %d", result.TotalCount)
	}
	originalHash := result.Items[0].ContentHash
	_ = stor // storage is used during upload

	// Modify the template file on disk
	if err := os.WriteFile(filepath.Join(templatesDir, "my-template", "file.txt"), []byte("updated content"), 0644); err != nil {
		t.Fatal(err)
	}

	// Second bootstrap should detect the change and update
	if err := srv.BootstrapTemplatesFromDir(ctx, templatesDir); err != nil {
		t.Fatalf("second bootstrap failed: %v", err)
	}

	// Verify the template was updated with a new content hash
	result, err = s.ListTemplates(ctx, store.TemplateFilter{}, store.ListOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if result.TotalCount != 1 {
		t.Fatalf("expected 1 template, got %d", result.TotalCount)
	}
	if result.Items[0].ContentHash == originalHash {
		t.Error("expected content hash to change after file update")
	}
}

func TestBootstrapTemplatesFromDir_SkipsUnchangedTemplate(t *testing.T) {
	srv, s, stor := testTemplateBootstrapServer(t)
	ctx := context.Background()

	templatesDir := makeTemplateDir(t, "my-template", map[string]string{
		"file.txt": "stable content",
	})

	if err := srv.BootstrapTemplatesFromDir(ctx, templatesDir); err != nil {
		t.Fatalf("first bootstrap failed: %v", err)
	}

	result, _ := s.ListTemplates(ctx, store.TemplateFilter{}, store.ListOptions{Limit: 10})
	originalHash := result.Items[0].ContentHash
	uploadCountAfterFirst := len(stor.objects)

	// Second bootstrap with no changes
	if err := srv.BootstrapTemplatesFromDir(ctx, templatesDir); err != nil {
		t.Fatalf("second bootstrap failed: %v", err)
	}

	result, _ = s.ListTemplates(ctx, store.TemplateFilter{}, store.ListOptions{Limit: 10})
	if result.Items[0].ContentHash != originalHash {
		t.Error("content hash should not change when files are unchanged")
	}
	if len(stor.objects) != uploadCountAfterFirst {
		t.Errorf("expected no new uploads, storage objects went from %d to %d",
			uploadCountAfterFirst, len(stor.objects))
	}
}

func TestBootstrapTemplatesFromDir_NoopWhenNoStorage(t *testing.T) {
	// Create server without storage
	s, err := sqlite.New(":memory:")
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
	srv, err := New(cfg, s)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	t.Cleanup(func() { srv.Shutdown(context.Background()) })
	// Deliberately not calling srv.SetStorage()

	ctx := context.Background()
	templatesDir := makeTemplateDir(t, "some-template", map[string]string{
		"file.txt": "content",
	})

	// Should not error, just skip
	if err := srv.BootstrapTemplatesFromDir(ctx, templatesDir); err != nil {
		t.Fatalf("bootstrap should not fail without storage: %v", err)
	}

	// Verify no templates were created
	result, err := s.ListTemplates(ctx, store.TemplateFilter{}, store.ListOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if result.TotalCount != 0 {
		t.Fatalf("expected 0 templates, got %d", result.TotalCount)
	}
}

func TestBootstrapTemplatesFromDir_EmptyDirectory(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()

	// Create an empty templates directory
	templatesDir := t.TempDir()

	if err := srv.BootstrapTemplatesFromDir(ctx, templatesDir); err != nil {
		t.Fatalf("bootstrap failed on empty dir: %v", err)
	}

	result, err := s.ListTemplates(ctx, store.TemplateFilter{}, store.ListOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if result.TotalCount != 0 {
		t.Fatalf("expected 0 templates, got %d", result.TotalCount)
	}
}

func TestBootstrapTemplatesFromDir_NonexistentDirectory(t *testing.T) {
	srv, _, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()

	if err := srv.BootstrapTemplatesFromDir(ctx, "/nonexistent/path"); err != nil {
		t.Fatalf("bootstrap should silently skip nonexistent directory: %v", err)
	}
}

func TestBootstrapTemplatesFromDir_MultipleTemplates(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()

	templatesDir := t.TempDir()

	// Create two template subdirectories
	for _, name := range []string{"alpha", "beta"} {
		dir := filepath.Join(templatesDir, name)
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "config.txt"), []byte(name), 0644); err != nil {
			t.Fatal(err)
		}
	}

	if err := srv.BootstrapTemplatesFromDir(ctx, templatesDir); err != nil {
		t.Fatalf("bootstrap failed: %v", err)
	}

	result, err := s.ListTemplates(ctx, store.TemplateFilter{}, store.ListOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if result.TotalCount != 2 {
		t.Fatalf("expected 2 templates, got %d", result.TotalCount)
	}
}

func TestBootstrapTemplatesFromDir_SkipsNonDirectories(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()

	templatesDir := t.TempDir()

	// Create a regular file (not a directory) at the top level
	if err := os.WriteFile(filepath.Join(templatesDir, "not-a-template.txt"), []byte("hi"), 0644); err != nil {
		t.Fatal(err)
	}
	// Create one valid template
	dir := filepath.Join(templatesDir, "valid")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("ok"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := srv.BootstrapTemplatesFromDir(ctx, templatesDir); err != nil {
		t.Fatalf("bootstrap failed: %v", err)
	}

	result, err := s.ListTemplates(ctx, store.TemplateFilter{}, store.ListOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if result.TotalCount != 1 {
		t.Fatalf("expected 1 template (skipping file), got %d", result.TotalCount)
	}
}

// TestSyncExistingTemplate_ForceReconcilesStorage verifies that a forced
// re-sync re-uploads modified files, uploads added files, and deletes files
// that are no longer present on disk. This mirrors the import-from-URL path
// where the user expects re-import to fully reflect their source changes.
func TestSyncExistingTemplate_ForceReconcilesStorage(t *testing.T) {
	srv, s, stor := testTemplateBootstrapServer(t)
	ctx := context.Background()

	// Initial bootstrap of a template with three files.
	templatesDir := makeTemplateDir(t, "my-template", map[string]string{
		"file-keep.txt":   "keep original",
		"file-update.txt": "before",
		"file-remove.txt": "stale",
	})
	templateDir := filepath.Join(templatesDir, "my-template")

	if err := srv.bootstrapSingleTemplate(ctx, "my-template", templateDir, store.TemplateScopeGlobal, ""); err != nil {
		t.Fatalf("initial bootstrap failed: %v", err)
	}

	existing, err := s.GetTemplateBySlug(ctx, "my-template", store.TemplateScopeGlobal, "")
	if err != nil {
		t.Fatalf("get template: %v", err)
	}
	originalHash := existing.ContentHash
	if len(stor.objects) != 3 {
		t.Fatalf("expected 3 storage objects after bootstrap, got %d", len(stor.objects))
	}

	// Modify the source: update one file, delete one, add a new one.
	if err := os.WriteFile(filepath.Join(templateDir, "file-update.txt"), []byte("after"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(templateDir, "file-remove.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(templateDir, "file-new.txt"), []byte("new"), 0644); err != nil {
		t.Fatal(err)
	}

	changed, err := srv.syncExistingTemplate(ctx, existing, templateDir, true)
	if err != nil {
		t.Fatalf("syncExistingTemplate failed: %v", err)
	}
	if !changed {
		t.Error("expected changed=true when content differs")
	}

	// DB manifest reflects the new file set.
	got, err := s.GetTemplateBySlug(ctx, "my-template", store.TemplateScopeGlobal, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.ContentHash == originalHash {
		t.Error("expected ContentHash to change after reconcile")
	}
	wantPaths := map[string]bool{"file-keep.txt": true, "file-update.txt": true, "file-new.txt": true}
	if len(got.Files) != len(wantPaths) {
		t.Errorf("expected %d files in manifest, got %d", len(wantPaths), len(got.Files))
	}
	for _, f := range got.Files {
		if !wantPaths[f.Path] {
			t.Errorf("unexpected file in manifest: %q", f.Path)
		}
	}

	// Storage reflects the new set: removed file is gone, new file is present.
	storagePath := got.StoragePath
	if _, exists := stor.objects[storagePath+"/file-remove.txt"]; exists {
		t.Error("expected file-remove.txt to be deleted from storage")
	}
	if _, exists := stor.objects[storagePath+"/file-new.txt"]; !exists {
		t.Error("expected file-new.txt to be uploaded to storage")
	}
	if _, exists := stor.objects[storagePath+"/file-update.txt"]; !exists {
		t.Error("expected file-update.txt to remain in storage after re-upload")
	}
	if len(stor.objects) != 3 {
		t.Errorf("expected 3 storage objects after reconcile, got %d", len(stor.objects))
	}
}

// TestSyncExistingTemplate_ForceWithoutChangesStillReuploads verifies that
// force=true re-uploads even when the source files are unchanged, so that
// the storage state is brought back into sync with the manifest if a user
// has reason to suspect drift.
func TestSyncExistingTemplate_ForceWithoutChangesStillReuploads(t *testing.T) {
	srv, s, stor := testTemplateBootstrapServer(t)
	ctx := context.Background()

	templatesDir := makeTemplateDir(t, "stable", map[string]string{
		"only.txt": "same content",
	})
	templateDir := filepath.Join(templatesDir, "stable")

	if err := srv.bootstrapSingleTemplate(ctx, "stable", templateDir, store.TemplateScopeGlobal, ""); err != nil {
		t.Fatalf("bootstrap failed: %v", err)
	}
	existing, err := s.GetTemplateBySlug(ctx, "stable", store.TemplateScopeGlobal, "")
	if err != nil {
		t.Fatal(err)
	}

	// Manually drop the storage object to simulate drift.
	storagePath := existing.StoragePath
	delete(stor.objects, storagePath+"/only.txt")

	if _, err := srv.syncExistingTemplate(ctx, existing, templateDir, true); err != nil {
		t.Fatalf("syncExistingTemplate failed: %v", err)
	}

	if _, exists := stor.objects[storagePath+"/only.txt"]; !exists {
		t.Error("expected only.txt to be re-uploaded by forced sync")
	}
}

// TestSyncExistingTemplate_PopulatesNewHashForLaterAgents verifies that after
// a forced re-sync, a freshly resolved template (the path used when creating a
// new agent) carries the updated ContentHash. This is the chain that ensures
// new agents created after a re-import use the new template version.
func TestSyncExistingTemplate_PopulatesNewHashForLaterAgents(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()

	templatesDir := makeTemplateDir(t, "claude-template", map[string]string{
		"home/.bashrc": "# v1",
	})
	templateDir := filepath.Join(templatesDir, "claude-template")

	if err := srv.bootstrapSingleTemplate(ctx, "claude-template", templateDir, store.TemplateScopeGlobal, ""); err != nil {
		t.Fatalf("bootstrap failed: %v", err)
	}
	v1, err := srv.resolveTemplate(ctx, "claude-template", "")
	if err != nil || v1 == nil {
		t.Fatalf("resolveTemplate v1: %v", err)
	}
	v1Hash := v1.ContentHash
	if v1Hash == "" {
		t.Fatal("expected non-empty hash after bootstrap")
	}

	// Edit the source as the user would after editing the git repo.
	if err := os.WriteFile(filepath.Join(templateDir, "home/.bashrc"), []byte("# v2"), 0644); err != nil {
		t.Fatal(err)
	}

	existing, err := s.GetTemplateBySlug(ctx, "claude-template", store.TemplateScopeGlobal, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.syncExistingTemplate(ctx, existing, templateDir, true); err != nil {
		t.Fatalf("sync (force) failed: %v", err)
	}

	v2, err := srv.resolveTemplate(ctx, "claude-template", "")
	if err != nil || v2 == nil {
		t.Fatalf("resolveTemplate v2: %v", err)
	}
	if v2.ContentHash == v1Hash {
		t.Errorf("expected ContentHash to change after re-sync; v1=%s v2=%s", v1Hash, v2.ContentHash)
	}
}

func TestDetectHarnessFromConfig_NameBased(t *testing.T) {
	tests := []struct {
		name     string
		expected string
	}{
		{"claude", "claude"},
		{"my-claude-template", "claude"},
		{"gemini", "gemini"},
		{"custom-gemini-pro", "gemini"},
		{"opencode", "opencode"},
		{"codex", "codex"},
		{"default", ""},
		{"my-custom", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			got := detectHarnessFromConfig(dir, tt.name)
			if got.Harness != tt.expected {
				t.Errorf("detectHarnessFromConfig(%q, %q).Harness = %q, want %q", dir, tt.name, got.Harness, tt.expected)
			}
			if got.DefaultHarnessConfig != "" {
				t.Errorf("expected empty DefaultHarnessConfig for name-based, got %q", got.DefaultHarnessConfig)
			}
		})
	}
}

func TestDetectHarnessFromConfig_FromConfigFile(t *testing.T) {
	dir := t.TempDir()

	configContent := `harness_config: claude
`
	if err := os.WriteFile(filepath.Join(dir, "scion-agent.yaml"), []byte(configContent), 0644); err != nil {
		t.Fatal(err)
	}

	got := detectHarnessFromConfig(dir, "my-template")
	if got.Harness != "claude" {
		t.Errorf("expected Harness 'claude' from config, got %q", got.Harness)
	}
	if got.DefaultHarnessConfig != "claude" {
		t.Errorf("expected DefaultHarnessConfig 'claude', got %q", got.DefaultHarnessConfig)
	}
}

func TestDetectHarnessFromConfig_DefaultHarnessConfig(t *testing.T) {
	dir := t.TempDir()

	configContent := `default_harness_config: gemini-web
`
	if err := os.WriteFile(filepath.Join(dir, "scion-agent.yaml"), []byte(configContent), 0644); err != nil {
		t.Fatal(err)
	}

	got := detectHarnessFromConfig(dir, "my-template")
	if got.Harness != "gemini" {
		t.Errorf("expected Harness 'gemini', got %q", got.Harness)
	}
	if got.DefaultHarnessConfig != "gemini-web" {
		t.Errorf("expected DefaultHarnessConfig 'gemini-web', got %q", got.DefaultHarnessConfig)
	}
}

func TestDetectHarnessFromConfig_HarnessField(t *testing.T) {
	dir := t.TempDir()

	configContent := `harness: codex
`
	if err := os.WriteFile(filepath.Join(dir, "scion-agent.yaml"), []byte(configContent), 0644); err != nil {
		t.Fatal(err)
	}

	got := detectHarnessFromConfig(dir, "my-template")
	if got.Harness != "codex" {
		t.Errorf("expected Harness 'codex' from config, got %q", got.Harness)
	}
	if got.DefaultHarnessConfig != "" {
		t.Errorf("expected empty DefaultHarnessConfig for explicit harness field, got %q", got.DefaultHarnessConfig)
	}
}

func TestDetectHarnessFromConfig_CustomDefaultHarnessConfig(t *testing.T) {
	dir := t.TempDir()

	configContent := `default_harness_config: adk
`
	if err := os.WriteFile(filepath.Join(dir, "scion-agent.yaml"), []byte(configContent), 0644); err != nil {
		t.Fatal(err)
	}

	got := detectHarnessFromConfig(dir, "my-template")
	if got.Harness != "" {
		t.Errorf("expected empty Harness for unknown config name 'adk', got %q", got.Harness)
	}
	if got.DefaultHarnessConfig != "adk" {
		t.Errorf("expected DefaultHarnessConfig 'adk', got %q", got.DefaultHarnessConfig)
	}
}

// setupWorkspaceProject creates a server, store, project, and workspace temp dir
// linked via an embedded broker provider. Returns the server, store, project,
// and the workspace root path. Templates should be placed under the returned
// workspace root.
func setupWorkspaceProject(t *testing.T, projectName string) (*Server, store.Store, *store.Project, string) {
	t.Helper()
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()

	workspaceRoot := t.TempDir()

	project := &store.Project{
		ID:        "project-ws-" + projectName,
		Name:      projectName,
		Slug:      projectName,
		GitRemote: "https://github.com/test/" + projectName,
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	brokerID := "broker-ws-" + projectName
	broker := &store.RuntimeBroker{
		ID:       brokerID,
		Name:     "ws-broker",
		Endpoint: "http://localhost:9090",
		Status:   store.BrokerStatusOnline,
	}
	if err := s.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create broker: %v", err)
	}

	if err := s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:    project.ID,
		BrokerID:   brokerID,
		BrokerName: broker.Name,
		LocalPath:  workspaceRoot,
		Status:     "online",
		LastSeen:   time.Now(),
	}); err != nil {
		t.Fatalf("failed to add project provider: %v", err)
	}

	srv.SetEmbeddedBrokerID(brokerID)

	return srv, s, project, workspaceRoot
}

func TestImportTemplatesFromWorkspace_ImportsTemplates(t *testing.T) {
	srv, s, project, wsRoot := setupWorkspaceProject(t, "ws-import")
	ctx := context.Background()

	// Create a templates directory with one valid scion template
	templateDir := filepath.Join(wsRoot, ".scion", "templates", "my-template")
	if err := os.MkdirAll(templateDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(templateDir, "scion-agent.yaml"), []byte("harness: claude\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(templateDir, "README.md"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}

	imported, err := srv.importTemplatesFromWorkspace(ctx, project, "/.scion/templates")
	if err != nil {
		t.Fatalf("import failed: %v", err)
	}
	if len(imported) != 1 || imported[0] != "my-template" {
		t.Fatalf("expected [my-template], got %v", imported)
	}

	result, err := s.ListTemplates(ctx, store.TemplateFilter{
		Scope:   string(store.TemplateScopeProject),
		ProjectID: project.ID,
	}, store.ListOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if result.TotalCount != 1 {
		t.Fatalf("expected 1 project-scoped template, got %d", result.TotalCount)
	}
	if result.Items[0].Scope != store.TemplateScopeProject {
		t.Errorf("expected project scope, got %q", result.Items[0].Scope)
	}
}

func TestImportTemplatesFromWorkspace_DefaultPath(t *testing.T) {
	srv, s, project, wsRoot := setupWorkspaceProject(t, "ws-default")
	ctx := context.Background()

	// Create a template at the default /.scion/templates path
	templateDir := filepath.Join(wsRoot, ".scion", "templates", "default-tmpl")
	if err := os.MkdirAll(templateDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(templateDir, "scion-agent.yaml"), []byte("harness: gemini\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Pass default path
	imported, err := srv.importTemplatesFromWorkspace(ctx, project, "/.scion/templates")
	if err != nil {
		t.Fatalf("import failed: %v", err)
	}
	if len(imported) != 1 {
		t.Fatalf("expected 1 template, got %d", len(imported))
	}

	result, err := s.ListTemplates(ctx, store.TemplateFilter{ProjectID: project.ID}, store.ListOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if result.TotalCount != 1 {
		t.Fatalf("expected 1 template, got %d", result.TotalCount)
	}
}

func TestImportTemplatesFromWorkspace_NonexistentPath(t *testing.T) {
	srv, _, project, _ := setupWorkspaceProject(t, "ws-nopath")
	ctx := context.Background()

	_, err := srv.importTemplatesFromWorkspace(ctx, project, "/does/not/exist")
	if err == nil {
		t.Fatal("expected error for nonexistent path")
	}
}

func TestImportTemplatesFromWorkspace_NoTemplatesFound(t *testing.T) {
	srv, _, project, wsRoot := setupWorkspaceProject(t, "ws-empty")
	ctx := context.Background()

	// Create the directory but with no valid templates
	emptyDir := filepath.Join(wsRoot, "empty-templates")
	if err := os.MkdirAll(emptyDir, 0755); err != nil {
		t.Fatal(err)
	}

	_, err := srv.importTemplatesFromWorkspace(ctx, project, "/empty-templates")
	if err == nil {
		t.Fatal("expected error for directory with no templates")
	}
	if !strings.Contains(err.Error(), "no scion templates found") {
		t.Fatalf("expected 'no scion templates found' error, got: %v", err)
	}
}

func TestImportTemplatesFromWorkspace_PathTraversal(t *testing.T) {
	srv, _, project, _ := setupWorkspaceProject(t, "ws-traversal")
	ctx := context.Background()

	// A relative path with .. escapes the project root
	_, err := srv.importTemplatesFromWorkspace(ctx, project, "../../../etc")
	if err == nil {
		t.Fatal("expected error for path traversal attempt")
	}
	if !strings.Contains(err.Error(), "must be within") {
		t.Fatalf("expected 'must be within' error, got: %v", err)
	}
}

func TestImportTemplatesFromWorkspace_MultipleTemplates(t *testing.T) {
	srv, s, project, wsRoot := setupWorkspaceProject(t, "ws-multi")
	ctx := context.Background()

	// Create two valid templates
	for _, name := range []string{"tmpl-a", "tmpl-b"} {
		dir := filepath.Join(wsRoot, "templates", name)
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "scion-agent.yaml"), []byte("harness: claude\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	imported, err := srv.importTemplatesFromWorkspace(ctx, project, "/templates")
	if err != nil {
		t.Fatalf("import failed: %v", err)
	}
	if len(imported) != 2 {
		t.Fatalf("expected 2 templates, got %d: %v", len(imported), imported)
	}

	result, err := s.ListTemplates(ctx, store.TemplateFilter{ProjectID: project.ID}, store.ListOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if result.TotalCount != 2 {
		t.Fatalf("expected 2 templates, got %d", result.TotalCount)
	}
}

func TestBootstrapTemplatesFromDir_ImportsDefaultHarnessConfig(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()

	templatesDir := makeTemplateDir(t, "web-dev", map[string]string{
		"scion-agent.yaml":                       "default_harness_config: claude-web\n",
		"harness-configs/claude-web/config.yaml": "harness: claude\n",
		"harness-configs/gemini-web/config.yaml": "harness: gemini\n",
	})

	if err := srv.BootstrapTemplatesFromDir(ctx, templatesDir); err != nil {
		t.Fatalf("bootstrap failed: %v", err)
	}

	result, err := s.ListTemplates(ctx, store.TemplateFilter{}, store.ListOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if result.TotalCount != 1 {
		t.Fatalf("expected 1 template, got %d", result.TotalCount)
	}

	tmpl := result.Items[0]
	if tmpl.DefaultHarnessConfig != "claude-web" {
		t.Errorf("expected DefaultHarnessConfig 'claude-web', got %q", tmpl.DefaultHarnessConfig)
	}
	if tmpl.Harness != "claude" {
		t.Errorf("expected Harness 'claude', got %q", tmpl.Harness)
	}

	// Verify bundled harness-configs were imported
	hcResult, err := s.ListHarnessConfigs(ctx, store.HarnessConfigFilter{}, store.ListOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if hcResult.TotalCount != 2 {
		t.Fatalf("expected 2 harness configs imported from template, got %d", hcResult.TotalCount)
	}

	names := map[string]bool{}
	for _, hc := range hcResult.Items {
		names[hc.Name] = true
	}
	if !names["claude-web"] {
		t.Error("expected harness config 'claude-web' to be imported")
	}
	if !names["gemini-web"] {
		t.Error("expected harness config 'gemini-web' to be imported")
	}
}

func TestBootstrapTemplatesFromDir_BackfillsDefaultHarnessConfig(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()

	templatesDir := makeTemplateDir(t, "backfill-tmpl", map[string]string{
		"scion-agent.yaml":                       "default_harness_config: claude-web\n",
		"harness-configs/claude-web/config.yaml": "harness: claude\n",
	})

	// First bootstrap populates DefaultHarnessConfig
	if err := srv.BootstrapTemplatesFromDir(ctx, templatesDir); err != nil {
		t.Fatalf("first bootstrap failed: %v", err)
	}

	// Simulate a pre-migration template by clearing the field in the DB
	tmpl, err := s.GetTemplateBySlug(ctx, "backfill-tmpl", store.TemplateScopeGlobal, "")
	if err != nil {
		t.Fatal(err)
	}
	tmpl.DefaultHarnessConfig = ""
	if err := s.UpdateTemplate(ctx, tmpl); err != nil {
		t.Fatal(err)
	}

	// Second bootstrap should backfill despite content hash matching
	if err := srv.BootstrapTemplatesFromDir(ctx, templatesDir); err != nil {
		t.Fatalf("second bootstrap failed: %v", err)
	}

	tmpl, err = s.GetTemplateBySlug(ctx, "backfill-tmpl", store.TemplateScopeGlobal, "")
	if err != nil {
		t.Fatal(err)
	}
	if tmpl.DefaultHarnessConfig != "claude-web" {
		t.Errorf("expected DefaultHarnessConfig 'claude-web' after backfill, got %q", tmpl.DefaultHarnessConfig)
	}
}
