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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// makeHarnessConfigDir creates a temp harness-configs directory with a single
// config subdirectory containing config.yaml and optional extra files.
// Returns the parent harness-configs directory.
func makeHarnessConfigDir(t *testing.T, configName string, files map[string]string) string {
	t.Helper()
	parentDir := t.TempDir()
	configDir := filepath.Join(parentDir, configName)
	for relPath, content := range files {
		full := filepath.Join(configDir, relPath)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return parentDir
}

func TestBootstrapHarnessConfigsFromDir_ImportsConfigs(t *testing.T) {
	srv, s, stor := testTemplateBootstrapServer(t)
	ctx := context.Background()

	dir := makeHarnessConfigDir(t, "claude", map[string]string{
		"config.yaml":       "harness: claude\nimage: scion-claude:latest\nuser: scion\n",
		"home/.claude.json": "{}",
		"home/.bashrc":      "# bashrc",
	})

	if err := srv.BootstrapHarnessConfigsFromDir(ctx, dir); err != nil {
		t.Fatalf("bootstrap failed: %v", err)
	}

	result, err := s.ListHarnessConfigs(ctx, store.HarnessConfigFilter{}, store.ListOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if result.TotalCount != 1 {
		t.Fatalf("expected 1 harness config, got %d", result.TotalCount)
	}

	hc := result.Items[0]
	if hc.Name != "claude" {
		t.Errorf("expected name 'claude', got %q", hc.Name)
	}
	if hc.Harness != "claude" {
		t.Errorf("expected harness 'claude', got %q", hc.Harness)
	}
	if hc.Status != store.HarnessConfigStatusActive {
		t.Errorf("expected status active, got %q", hc.Status)
	}
	if hc.Scope != store.HarnessConfigScopeGlobal {
		t.Errorf("expected scope global, got %q", hc.Scope)
	}
	if len(hc.Files) != 3 {
		t.Errorf("expected 3 files in manifest, got %d", len(hc.Files))
	}
	if hc.ContentHash == "" {
		t.Error("expected non-empty content hash")
	}
	if len(stor.objects) != 3 {
		t.Errorf("expected 3 objects in storage, got %d", len(stor.objects))
	}
}

func TestBootstrapHarnessConfigsFromDir_MultipleConfigs(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()

	dir := t.TempDir()

	// Create two harness config directories
	for _, name := range []string{"gemini", "adk"} {
		harness := name
		if name == "adk" {
			harness = "generic"
		}
		configDir := filepath.Join(dir, name)
		if err := os.MkdirAll(configDir, 0755); err != nil {
			t.Fatal(err)
		}
		content := "harness: " + harness + "\nimage: scion-" + name + ":latest\nuser: scion\n"
		if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	if err := srv.BootstrapHarnessConfigsFromDir(ctx, dir); err != nil {
		t.Fatalf("bootstrap failed: %v", err)
	}

	result, err := s.ListHarnessConfigs(ctx, store.HarnessConfigFilter{}, store.ListOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if result.TotalCount != 2 {
		t.Fatalf("expected 2 harness configs, got %d", result.TotalCount)
	}
}

func TestBootstrapHarnessConfigsFromDir_SyncsChangedConfig(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()

	dir := makeHarnessConfigDir(t, "gemini", map[string]string{
		"config.yaml": "harness: gemini\nimage: scion-gemini:latest\nuser: scion\n",
	})

	if err := srv.BootstrapHarnessConfigsFromDir(ctx, dir); err != nil {
		t.Fatalf("first bootstrap failed: %v", err)
	}

	result, err := s.ListHarnessConfigs(ctx, store.HarnessConfigFilter{}, store.ListOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	originalHash := result.Items[0].ContentHash

	// Modify config.yaml
	if err := os.WriteFile(filepath.Join(dir, "gemini", "config.yaml"),
		[]byte("harness: gemini\nimage: scion-gemini:v2\nuser: scion\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := srv.BootstrapHarnessConfigsFromDir(ctx, dir); err != nil {
		t.Fatalf("second bootstrap failed: %v", err)
	}

	result, err = s.ListHarnessConfigs(ctx, store.HarnessConfigFilter{}, store.ListOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if result.TotalCount != 1 {
		t.Fatalf("expected 1 harness config, got %d", result.TotalCount)
	}
	if result.Items[0].ContentHash == originalHash {
		t.Error("expected content hash to change after file update")
	}
}

func TestBootstrapHarnessConfigsFromDir_SkipsUnchanged(t *testing.T) {
	srv, s, stor := testTemplateBootstrapServer(t)
	ctx := context.Background()

	dir := makeHarnessConfigDir(t, "claude", map[string]string{
		"config.yaml": "harness: claude\nimage: scion-claude:latest\nuser: scion\n",
	})

	if err := srv.BootstrapHarnessConfigsFromDir(ctx, dir); err != nil {
		t.Fatalf("first bootstrap failed: %v", err)
	}

	result, _ := s.ListHarnessConfigs(ctx, store.HarnessConfigFilter{}, store.ListOptions{Limit: 10})
	originalHash := result.Items[0].ContentHash
	uploadCountAfterFirst := len(stor.objects)

	if err := srv.BootstrapHarnessConfigsFromDir(ctx, dir); err != nil {
		t.Fatalf("second bootstrap failed: %v", err)
	}

	result, _ = s.ListHarnessConfigs(ctx, store.HarnessConfigFilter{}, store.ListOptions{Limit: 10})
	if result.Items[0].ContentHash != originalHash {
		t.Error("expected content hash to remain unchanged")
	}
	if len(stor.objects) != uploadCountAfterFirst {
		t.Errorf("expected no new uploads, got %d objects (was %d)", len(stor.objects), uploadCountAfterFirst)
	}
}

// TestSyncHarnessConfig_PreservesTypedConfig guards the ResourceStore
// record↔model round-trip for harness-configs: a content-changing sync must
// leave the typed HarnessConfigData payload intact.
func TestSyncHarnessConfig_PreservesTypedConfig(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()

	dir := makeHarnessConfigDir(t, "claude", map[string]string{
		"config.yaml": "harness: claude\nimage: scion-claude:latest\nuser: scion\n",
	})

	if err := srv.BootstrapHarnessConfigsFromDir(ctx, dir); err != nil {
		t.Fatalf("first bootstrap failed: %v", err)
	}

	hc, err := s.GetHarnessConfigBySlug(ctx, "claude", store.HarnessConfigScopeGlobal, "")
	if err != nil {
		t.Fatal(err)
	}
	hc.Config = &store.HarnessConfigData{Image: "scion-claude:latest", Model: "opus"}
	hc.DisplayName = "Claude Config"
	if err := s.UpdateHarnessConfig(ctx, hc); err != nil {
		t.Fatal(err)
	}

	// Change content and re-sync.
	if err := os.WriteFile(filepath.Join(dir, "claude", "config.yaml"),
		[]byte("harness: claude\nimage: scion-claude:v2\nuser: scion\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := srv.BootstrapHarnessConfigsFromDir(ctx, dir); err != nil {
		t.Fatalf("second bootstrap failed: %v", err)
	}

	got, err := s.GetHarnessConfigBySlug(ctx, "claude", store.HarnessConfigScopeGlobal, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Config == nil {
		t.Fatal("expected typed Config to survive sync, got nil")
	}
	if got.Config.Model != "opus" {
		t.Errorf("typed Config not preserved: got %+v", got.Config)
	}
	if got.DisplayName != "Claude Config" {
		t.Errorf("expected DisplayName preserved, got %q", got.DisplayName)
	}
}

// TestSyncHarnessConfig_ReconcilesRemovedFiles verifies harness-configs gained
// stale-object reconcile on sync by routing through the shared ResourceStore
// (previously harness-config sync left removed files lingering in storage).
func TestSyncHarnessConfig_ReconcilesRemovedFiles(t *testing.T) {
	srv, s, stor := testTemplateBootstrapServer(t)
	ctx := context.Background()

	dir := makeHarnessConfigDir(t, "claude", map[string]string{
		"config.yaml":  "harness: claude\nimage: scion-claude:latest\nuser: scion\n",
		"home/.bashrc": "# keep",
		"home/.stale":  "remove me",
	})

	if err := srv.BootstrapHarnessConfigsFromDir(ctx, dir); err != nil {
		t.Fatalf("first bootstrap failed: %v", err)
	}
	hc, err := s.GetHarnessConfigBySlug(ctx, "claude", store.HarnessConfigScopeGlobal, "")
	if err != nil {
		t.Fatal(err)
	}
	stalePath := hc.StoragePath + "/home/.stale"
	if _, ok := stor.objects[stalePath]; !ok {
		t.Fatalf("expected %q in storage after bootstrap", stalePath)
	}

	// Remove a file and change another so the content hash differs.
	if err := os.Remove(filepath.Join(dir, "claude", "home/.stale")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "claude", "home/.bashrc"), []byte("# changed"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := srv.BootstrapHarnessConfigsFromDir(ctx, dir); err != nil {
		t.Fatalf("second bootstrap failed: %v", err)
	}

	if _, ok := stor.objects[stalePath]; ok {
		t.Errorf("expected stale object %q to be reconciled (deleted) from storage", stalePath)
	}
}

func TestBootstrapHarnessConfigsFromDir_NonexistentDir(t *testing.T) {
	srv, _, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()

	if err := srv.BootstrapHarnessConfigsFromDir(ctx, "/nonexistent/path"); err != nil {
		t.Errorf("expected nil error for nonexistent dir, got: %v", err)
	}
}

func TestBootstrapHarnessConfigsFromDir_SkipsNonDirectories(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()

	dir := t.TempDir()
	// Create a regular file (not a directory) — should be skipped
	if err := os.WriteFile(filepath.Join(dir, "not-a-dir.txt"), []byte("ignored"), 0644); err != nil {
		t.Fatal(err)
	}
	// Create a valid harness config directory
	configDir := filepath.Join(dir, "gemini")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"),
		[]byte("harness: gemini\nimage: scion-gemini:latest\nuser: scion\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := srv.BootstrapHarnessConfigsFromDir(ctx, dir); err != nil {
		t.Fatalf("bootstrap failed: %v", err)
	}

	result, err := s.ListHarnessConfigs(ctx, store.HarnessConfigFilter{}, store.ListOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if result.TotalCount != 1 {
		t.Fatalf("expected 1 harness config (skipping non-dir), got %d", result.TotalCount)
	}
}

// ============================================================================
// Project harness-config import (workspace path) — mirrors the template
// import tests in template_bootstrap_test.go.
// ============================================================================

// writeHarnessConfigDir creates a harness-config directory at
// <root>/<rel>/<name> containing a minimal valid config.yaml.
func writeHarnessConfigDir(t *testing.T, root, rel, name, harness string) {
	t.Helper()
	dir := filepath.Join(root, filepath.FromSlash(rel), name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("harness: "+harness+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestImportHarnessConfigsFromWorkspace_ImportsConfig(t *testing.T) {
	srv, s, project, wsRoot := setupWorkspaceProject(t, "hc-import")
	ctx := context.Background()

	writeHarnessConfigDir(t, wsRoot, ".scion/harness-configs", "my-config", "claude")
	// A bonus non-yaml file to make sure extra files are walked fine.
	if err := os.WriteFile(filepath.Join(wsRoot, ".scion", "harness-configs", "my-config", "README.md"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}

	imported, err := srv.importHarnessConfigsFromWorkspace(ctx, project, "/.scion/harness-configs")
	if err != nil {
		t.Fatalf("import failed: %v", err)
	}
	if len(imported) != 1 || imported[0] != "my-config" {
		t.Fatalf("expected [my-config], got %v", imported)
	}

	result, err := s.ListHarnessConfigs(ctx, store.HarnessConfigFilter{
		Scope:     store.HarnessConfigScopeProject,
		ProjectID: project.ID,
	}, store.ListOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if result.TotalCount != 1 {
		t.Fatalf("expected 1 project-scoped harness-config, got %d", result.TotalCount)
	}
	if result.Items[0].Scope != store.HarnessConfigScopeProject {
		t.Errorf("expected project scope, got %q", result.Items[0].Scope)
	}
	if result.Items[0].Harness != "claude" {
		t.Errorf("expected harness 'claude', got %q", result.Items[0].Harness)
	}
}

func TestImportHarnessConfigsFromWorkspace_DefaultPath(t *testing.T) {
	srv, s, project, wsRoot := setupWorkspaceProject(t, "hc-default")
	ctx := context.Background()

	writeHarnessConfigDir(t, wsRoot, ".scion/harness-configs", "default-hc", "gemini")

	imported, err := srv.importHarnessConfigsFromWorkspace(ctx, project, "/.scion/harness-configs")
	if err != nil {
		t.Fatalf("import failed: %v", err)
	}
	if len(imported) != 1 {
		t.Fatalf("expected 1 harness-config, got %d", len(imported))
	}

	result, err := s.ListHarnessConfigs(ctx, store.HarnessConfigFilter{ProjectID: project.ID}, store.ListOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if result.TotalCount != 1 {
		t.Fatalf("expected 1 harness-config, got %d", result.TotalCount)
	}
}

func TestImportHarnessConfigsFromWorkspace_NonexistentPath(t *testing.T) {
	srv, _, project, _ := setupWorkspaceProject(t, "hc-nopath")
	ctx := context.Background()

	_, err := srv.importHarnessConfigsFromWorkspace(ctx, project, "/does/not/exist")
	if err == nil {
		t.Fatal("expected error for nonexistent path")
	}
}

func TestImportHarnessConfigsFromWorkspace_NoConfigsFound(t *testing.T) {
	srv, _, project, wsRoot := setupWorkspaceProject(t, "hc-empty")
	ctx := context.Background()

	emptyDir := filepath.Join(wsRoot, "empty-configs")
	if err := os.MkdirAll(emptyDir, 0755); err != nil {
		t.Fatal(err)
	}

	_, err := srv.importHarnessConfigsFromWorkspace(ctx, project, "/empty-configs")
	if err == nil {
		t.Fatal("expected error for directory with no harness-configs")
	}
	if !strings.Contains(err.Error(), "no scion harness-configs found") {
		t.Fatalf("expected 'no scion harness-configs found' error, got: %v", err)
	}
}

func TestImportHarnessConfigsFromWorkspace_PathTraversal(t *testing.T) {
	srv, _, project, _ := setupWorkspaceProject(t, "hc-traversal")
	ctx := context.Background()

	_, err := srv.importHarnessConfigsFromWorkspace(ctx, project, "../../../etc")
	if err == nil {
		t.Fatal("expected error for path traversal attempt")
	}
	if !strings.Contains(err.Error(), "must be within") {
		t.Fatalf("expected 'must be within' error, got: %v", err)
	}
}

func TestImportHarnessConfigsFromWorkspace_MultipleConfigs(t *testing.T) {
	srv, s, project, wsRoot := setupWorkspaceProject(t, "hc-multi")
	ctx := context.Background()

	writeHarnessConfigDir(t, wsRoot, "configs", "hc-a", "claude")
	writeHarnessConfigDir(t, wsRoot, "configs", "hc-b", "gemini")

	imported, err := srv.importHarnessConfigsFromWorkspace(ctx, project, "/configs")
	if err != nil {
		t.Fatalf("import failed: %v", err)
	}
	if len(imported) != 2 {
		t.Fatalf("expected 2 harness-configs, got %v", imported)
	}

	result, err := s.ListHarnessConfigs(ctx, store.HarnessConfigFilter{
		Scope:     store.HarnessConfigScopeProject,
		ProjectID: project.ID,
	}, store.ListOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if result.TotalCount != 2 {
		t.Fatalf("expected 2 project-scoped harness-configs, got %d", result.TotalCount)
	}
}

// TestImportHarnessConfigsFromWorkspace_SingleConfigAtRoot verifies that
// pointing directly at one harness-config directory (rather than a parent of
// configs) imports it.
func TestImportHarnessConfigsFromWorkspace_SingleConfigAtRoot(t *testing.T) {
	srv, s, project, wsRoot := setupWorkspaceProject(t, "hc-single")
	ctx := context.Background()

	writeHarnessConfigDir(t, wsRoot, "", "solo-config", "claude")

	imported, err := srv.importHarnessConfigsFromWorkspace(ctx, project, "/solo-config")
	if err != nil {
		t.Fatalf("import failed: %v", err)
	}
	if len(imported) != 1 || imported[0] != "solo-config" {
		t.Fatalf("expected [solo-config], got %v", imported)
	}

	result, err := s.ListHarnessConfigs(ctx, store.HarnessConfigFilter{ProjectID: project.ID}, store.ListOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if result.TotalCount != 1 {
		t.Fatalf("expected 1 harness-config, got %d", result.TotalCount)
	}
}

// TestImportHarnessConfigsFromWorkspace_Reimport verifies a second import of the
// same config force-syncs without error and does not create duplicates.
func TestImportHarnessConfigsFromWorkspace_Reimport(t *testing.T) {
	srv, s, project, wsRoot := setupWorkspaceProject(t, "hc-reimport")
	ctx := context.Background()

	writeHarnessConfigDir(t, wsRoot, ".scion/harness-configs", "repeat", "claude")

	if _, err := srv.importHarnessConfigsFromWorkspace(ctx, project, "/.scion/harness-configs"); err != nil {
		t.Fatalf("first import failed: %v", err)
	}
	imported, err := srv.importHarnessConfigsFromWorkspace(ctx, project, "/.scion/harness-configs")
	if err != nil {
		t.Fatalf("second import failed: %v", err)
	}
	if len(imported) != 1 {
		t.Fatalf("expected 1 harness-config re-imported, got %v", imported)
	}

	result, err := s.ListHarnessConfigs(ctx, store.HarnessConfigFilter{
		Scope:     store.HarnessConfigScopeProject,
		ProjectID: project.ID,
	}, store.ListOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if result.TotalCount != 1 {
		t.Fatalf("expected 1 harness-config after re-import, got %d", result.TotalCount)
	}
}
