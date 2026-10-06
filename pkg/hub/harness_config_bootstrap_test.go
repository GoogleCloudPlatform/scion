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
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/resources"
)

func (s *Server) importHarnessConfigsFromRemote(ctx context.Context, projectID, sourceURL string) ([]string, error) {
	return s.importFromRemote(ctx, projectID, sourceURL, store.HarnessConfigScopeProject, s.harnessConfigImportKind(), nil, nil)
}

func (s *Server) importHarnessConfigsFromWorkspace(ctx context.Context, project *store.Project, workspacePath string) ([]string, error) {
	return s.importFromWorkspace(ctx, project, workspacePath, store.HarnessConfigScopeProject, s.harnessConfigImportKind(), nil, nil)
}

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

func TestBootstrapHarnessConfigsFromDir_PersistsConfigImage(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()

	dir := makeHarnessConfigDir(t, "claude", map[string]string{
		"config.yaml": "harness: claude\nimage: scion-claude:latest\nuser: scion\n",
	})

	if err := srv.BootstrapHarnessConfigsFromDir(ctx, dir); err != nil {
		t.Fatalf("bootstrap failed: %v", err)
	}

	hc, err := s.GetHarnessConfigBySlug(ctx, "claude", store.HarnessConfigScopeGlobal, "")
	if err != nil {
		t.Fatal(err)
	}
	if hc.Config == nil {
		t.Fatal("expected Config to be populated after bootstrap, got nil")
	}
	if hc.Config.Image != "scion-claude:latest" {
		t.Errorf("expected Config.Image = %q, got %q", "scion-claude:latest", hc.Config.Image)
	}

	// Re-bootstrap with a different image and verify it updates.
	if err := os.WriteFile(filepath.Join(dir, "claude", "config.yaml"),
		[]byte("harness: claude\nimage: scion-claude:v2\nuser: scion\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := srv.BootstrapHarnessConfigsFromDir(ctx, dir); err != nil {
		t.Fatalf("second bootstrap failed: %v", err)
	}

	hc, err = s.GetHarnessConfigBySlug(ctx, "claude", store.HarnessConfigScopeGlobal, "")
	if err != nil {
		t.Fatal(err)
	}
	if hc.Config == nil || hc.Config.Image != "scion-claude:v2" {
		t.Errorf("expected Config.Image = %q after re-sync, got %+v", "scion-claude:v2", hc.Config)
	}
}

// TestBootstrapHarnessConfigsFromDir_PersistsModelAliases is a regression
// test for ptone/scion#2365: the hub used to never persist a config.yaml's
// model_aliases (or default model) onto the stored HarnessConfig record, so
// resolveModelAliasForAgent always fell through to the alias table baked
// into the hub binary at build time — meaning an alias update in
// config.yaml had no effect until the hub itself was rebuilt/redeployed.
// This verifies the extraction happens on both initial import and re-sync.
func TestBootstrapHarnessConfigsFromDir_PersistsModelAliases(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()

	dir := makeHarnessConfigDir(t, "codex", map[string]string{
		"config.yaml": "harness: codex\nmodel: medium\n" +
			"model_aliases:\n  small: gpt-6-luna\n  medium: gpt-6.1-sol\n  large: gpt-6.1-sol\n",
	})

	if err := srv.BootstrapHarnessConfigsFromDir(ctx, dir); err != nil {
		t.Fatalf("bootstrap failed: %v", err)
	}

	hc, err := s.GetHarnessConfigBySlug(ctx, "codex", store.HarnessConfigScopeGlobal, "")
	if err != nil {
		t.Fatal(err)
	}
	if hc.Config == nil {
		t.Fatal("expected Config to be populated after bootstrap, got nil")
	}
	if hc.Config.Model != "medium" {
		t.Errorf("expected Config.Model = %q, got %q", "medium", hc.Config.Model)
	}
	wantAliases := map[string]string{"small": "gpt-6-luna", "medium": "gpt-6.1-sol", "large": "gpt-6.1-sol"}
	if !reflect.DeepEqual(hc.Config.ModelAliases, wantAliases) {
		t.Errorf("expected Config.ModelAliases = %+v, got %+v", wantAliases, hc.Config.ModelAliases)
	}

	// Re-sync with updated aliases (as if the user edited config.yaml to
	// point "large" at a new model) and verify the stored record picks up
	// the change rather than keeping the value from the first sync.
	if err := os.WriteFile(filepath.Join(dir, "codex", "config.yaml"),
		[]byte("harness: codex\nmodel: medium\n"+
			"model_aliases:\n  small: gpt-6-luna\n  medium: gpt-6.1-sol\n  large: gpt-6.1-astra\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := srv.BootstrapHarnessConfigsFromDir(ctx, dir); err != nil {
		t.Fatalf("second bootstrap failed: %v", err)
	}

	hc, err = s.GetHarnessConfigBySlug(ctx, "codex", store.HarnessConfigScopeGlobal, "")
	if err != nil {
		t.Fatal(err)
	}
	if hc.Config == nil || hc.Config.ModelAliases["large"] != "gpt-6.1-astra" {
		t.Errorf("expected Config.ModelAliases[large] = %q after re-sync, got %+v", "gpt-6.1-astra", hc.Config)
	}
}

// TestBootstrapHarnessConfigsFromDir_PreservesModelAliasesWhenConfigYAMLSilent
// verifies extractModelConfig's "only overwrite when config.yaml declares
// it" contract: a hub-side manual edit to Config.ModelAliases (e.g. made
// through the harness-config API) must survive a re-sync of a config.yaml
// that has no model_aliases key at all, mirroring the existing
// TestSyncHarnessConfig_PreservesTypedConfig contract for Config.Model.
func TestBootstrapHarnessConfigsFromDir_PreservesModelAliasesWhenConfigYAMLSilent(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()

	dir := makeHarnessConfigDir(t, "custom", map[string]string{
		"config.yaml": "harness: custom\nimage: scion-custom:v1\n",
	})
	if err := srv.BootstrapHarnessConfigsFromDir(ctx, dir); err != nil {
		t.Fatalf("bootstrap failed: %v", err)
	}

	hc, err := s.GetHarnessConfigBySlug(ctx, "custom", store.HarnessConfigScopeGlobal, "")
	if err != nil {
		t.Fatal(err)
	}
	hc.Config.ModelAliases = map[string]string{"large": "manually-set-model"}
	if err := s.UpdateHarnessConfig(ctx, hc); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "custom", "config.yaml"),
		[]byte("harness: custom\nimage: scion-custom:v2\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := srv.BootstrapHarnessConfigsFromDir(ctx, dir); err != nil {
		t.Fatalf("second bootstrap failed: %v", err)
	}

	got, err := s.GetHarnessConfigBySlug(ctx, "custom", store.HarnessConfigScopeGlobal, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Config == nil || got.Config.ModelAliases["large"] != "manually-set-model" {
		t.Errorf("expected manually-set ModelAliases to survive a config.yaml silent on model_aliases, got %+v", got.Config)
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

func TestBootstrapHarnessConfigsFromDir_SkipsBackupAndTempFiles(t *testing.T) {
	srv, s, stor := testTemplateBootstrapServer(t)
	ctx := context.Background()

	dir := makeHarnessConfigDir(t, "claude", map[string]string{
		"config.yaml":                       "harness: claude\nimage: scion-claude:latest\nuser: scion\n",
		"dialect.yaml":                      "dialect: claude\n",
		"config.yaml.bak.20261003T193320Z":  "old\n",
		"provision.py.bak.20261003T193320Z": "old\n",
		".provision.py.tmp-123456":          "partial\n",
	})

	if err := srv.BootstrapHarnessConfigsFromDir(ctx, dir); err != nil {
		t.Fatalf("bootstrap failed: %v", err)
	}

	hc, err := s.GetHarnessConfigBySlug(ctx, "claude", store.HarnessConfigScopeGlobal, "")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range hc.Files {
		got = append(got, f.Path)
	}
	want := []string{"config.yaml", "dialect.yaml"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("manifest files = %v, want %v", got, want)
	}
	if len(stor.objects) != 2 {
		t.Errorf("expected 2 objects in storage, got %d", len(stor.objects))
	}
}

// TestBootstrapHarnessConfigsFromDir_DeletedBuiltinStaysDeleted covers AC1 of
// ptone/scion#3544 on the workstation path: the built-in is re-materialized on
// disk every start (UpdateDefaultTemplates(true)), but a deleted built-in is
// not re-imported into the hub.
func TestBootstrapHarnessConfigsFromDir_DeletedBuiltinStaysDeleted(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()
	globalDir := t.TempDir()
	hcDir := filepath.Join(globalDir, "harness-configs")
	const victim = "claude"

	materialize := func() {
		t.Helper()
		if err := config.MaterializeBundledHarnessConfigs(globalDir, config.MaterializeOptions{Force: true}); err != nil {
			t.Fatalf("materialize: %v", err)
		}
	}

	materialize()
	if err := srv.BootstrapHarnessConfigsFromDir(ctx, hcDir); err != nil {
		t.Fatalf("initial bootstrap: %v", err)
	}
	hc, err := s.GetHarnessConfigBySlug(ctx, victim, store.HarnessConfigScopeGlobal, "")
	if err != nil {
		t.Fatalf("built-in %q not imported: %v", victim, err)
	}
	if err := s.DeleteHarnessConfig(ctx, hc.ID); err != nil {
		t.Fatal(err)
	}

	// Restart twice: disk is re-materialized, then bootstrapped.
	for i := range 2 {
		materialize()
		if _, err := os.Stat(filepath.Join(hcDir, victim, "config.yaml")); err != nil {
			t.Fatalf("restart %d: expected %q re-materialized on disk: %v", i, victim, err)
		}
		if err := srv.BootstrapHarnessConfigsFromDir(ctx, hcDir); err != nil {
			t.Fatalf("restart %d bootstrap: %v", i, err)
		}
		if _, err := s.GetHarnessConfigBySlug(ctx, victim, store.HarnessConfigScopeGlobal, ""); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("restart %d: deleted built-in %q was re-imported (err=%v)", i, victim, err)
		}
	}

	result, _ := s.ListHarnessConfigs(ctx, store.HarnessConfigFilter{}, store.ListOptions{Limit: 100})
	if got, want := result.TotalCount, len(resources.BuiltinHarnessConfigNames())-1; got != want {
		t.Errorf("harness configs after restarts: got %d, want %d", got, want)
	}
	doc := readBuiltinSeedLedgerDoc(t, s)
	if doc == nil || !reflect.DeepEqual(doc.HarnessConfigs, allBuiltinHarnessConfigSlugs()) {
		t.Errorf("ledger harness_configs = %+v, want every built-in", doc)
	}
}

// TestBootstrapHarnessConfigsFromDir_UserDirStillImported verifies that a
// non-built-in directory keeps today's behaviour: disk is its source of
// truth, so deleting the row and leaving the directory re-imports it. It is
// never added to the ledger.
func TestBootstrapHarnessConfigsFromDir_UserDirStillImported(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()
	const name = "my-custom-config"

	dir := makeHarnessConfigDir(t, name, map[string]string{
		"config.yaml": "harness: claude\nimage: scion-claude:latest\nuser: scion\n",
	})
	if err := srv.BootstrapHarnessConfigsFromDir(ctx, dir); err != nil {
		t.Fatal(err)
	}
	hc, err := s.GetHarnessConfigBySlug(ctx, name, store.HarnessConfigScopeGlobal, "")
	if err != nil {
		t.Fatalf("user config not imported: %v", err)
	}
	if err := s.DeleteHarnessConfig(ctx, hc.ID); err != nil {
		t.Fatal(err)
	}
	if err := srv.BootstrapHarnessConfigsFromDir(ctx, dir); err != nil {
		t.Fatal(err)
	}
	again, err := s.GetHarnessConfigBySlug(ctx, name, store.HarnessConfigScopeGlobal, "")
	if err != nil {
		t.Fatalf("user config not re-imported after delete: %v", err)
	}
	if again.ID == hc.ID {
		t.Error("expected a new row after re-import")
	}
	if doc := readBuiltinSeedLedgerDoc(t, s); doc != nil {
		t.Errorf("non-built-in import wrote the ledger: %+v", doc)
	}
}

// TestBootstrapHarnessConfigsFromDir_CorruptLedgerFailsClosedForBuiltins
// verifies that an unreadable ledger row does not stop user dirs from being
// imported, does not let a deleted built-in come back, is not overwritten,
// and is reported as an error.
func TestBootstrapHarnessConfigsFromDir_CorruptLedgerFailsClosedForBuiltins(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()
	globalDir := t.TempDir()
	hcDir := filepath.Join(globalDir, "harness-configs")
	const victim = "claude"
	const userName = "my-custom-config"

	if err := config.MaterializeBundledHarnessConfigs(globalDir, config.MaterializeOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	if err := srv.BootstrapHarnessConfigsFromDir(ctx, hcDir); err != nil {
		t.Fatalf("initial bootstrap: %v", err)
	}
	hc, err := s.GetHarnessConfigBySlug(ctx, victim, store.HarnessConfigScopeGlobal, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteHarnessConfig(ctx, hc.ID); err != nil {
		t.Fatal(err)
	}

	// Corrupt the ledger row (non-JSON value) and add a user dir.
	if _, err := s.UpsertHubSetting(ctx, builtinSeedLedgerSection, json.RawMessage(`"not-a-ledger"`),
		"test", -1, "seeded"); err != nil {
		t.Fatalf("corrupt ledger: %v", err)
	}
	corrupt, _ := s.GetHubSetting(ctx, builtinSeedLedgerSection)
	userDir := filepath.Join(hcDir, userName)
	if err := os.MkdirAll(userDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userDir, "config.yaml"),
		[]byte("harness: claude\nimage: scion-claude:latest\nuser: scion\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := config.MaterializeBundledHarnessConfigs(globalDir, config.MaterializeOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	// Edit an existing built-in on disk: it must still be synced.
	const synced = "codex"
	syncedBefore, err := s.GetHarnessConfigBySlug(ctx, synced, store.HarnessConfigScopeGlobal, "")
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(hcDir, synced, "config.yaml"), os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("\n# edited locally\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	err = srv.BootstrapHarnessConfigsFromDir(ctx, hcDir)
	if err == nil {
		t.Fatal("expected an error for an unreadable ledger")
	}
	syncedAfter, err := s.GetHarnessConfigBySlug(ctx, synced, store.HarnessConfigScopeGlobal, "")
	if err != nil {
		t.Fatal(err)
	}
	if syncedAfter.ContentHash == syncedBefore.ContentHash {
		t.Errorf("existing built-in %q was not synced with a corrupt ledger (content hash unchanged)", synced)
	}
	if _, err := s.GetHarnessConfigBySlug(ctx, userName, store.HarnessConfigScopeGlobal, ""); err != nil {
		t.Errorf("user dir not imported with a corrupt ledger: %v", err)
	}
	if _, err := s.GetHarnessConfigBySlug(ctx, victim, store.HarnessConfigScopeGlobal, ""); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("deleted built-in %q re-created with a corrupt ledger (err=%v)", victim, err)
	}
	after, _ := s.GetHubSetting(ctx, builtinSeedLedgerSection)
	if after.Revision != corrupt.Revision || string(after.Value) != string(corrupt.Value) {
		t.Errorf("corrupt ledger row was overwritten: rev %d -> %d, value %s", corrupt.Revision, after.Revision, after.Value)
	}
}

// failingUploadStorage wraps mockStorage and fails every upload, to force a
// content sync of an existing row to fail.
type failingUploadStorage struct {
	*mockStorage
}

func (f *failingUploadStorage) Upload(context.Context, string, io.Reader, storage.UploadOptions) (*storage.Object, error) {
	return nil, errors.New("upload failed (test)")
}

// TestBootstrapHarnessConfigsFromDir_MarksExistingBuiltinWhenSyncFails
// verifies that an existing built-in row counts as seeded even if its content
// sync fails (mark before sync, same rule as the hosted path): after a failed
// sync the name is in the ledger, so deleting the row sticks.
func TestBootstrapHarnessConfigsFromDir_MarksExistingBuiltinWhenSyncFails(t *testing.T) {
	srv, s, stor := testTemplateBootstrapServer(t)
	ctx := context.Background()
	const name = "claude"

	dir := makeHarnessConfigDir(t, name, map[string]string{
		"config.yaml": "harness: claude\nimage: scion-claude:latest\nuser: scion\n",
	})
	if err := srv.BootstrapHarnessConfigsFromDir(ctx, dir); err != nil {
		t.Fatal(err)
	}
	// Simulate a hub that predates the ledger.
	if err := s.DeleteHubSetting(ctx, builtinSeedLedgerSection); err != nil {
		t.Fatal(err)
	}

	// Change the content so the sync must upload, and make uploads fail.
	if err := os.WriteFile(filepath.Join(dir, name, "config.yaml"),
		[]byte("harness: claude\nimage: scion-claude:v2\nuser: scion\n"), 0644); err != nil {
		t.Fatal(err)
	}
	srv.SetStorage(&failingUploadStorage{mockStorage: stor})
	if err := srv.BootstrapHarnessConfigsFromDir(ctx, dir); err != nil {
		t.Fatalf("bootstrap with failing sync: %v", err)
	}
	doc := readBuiltinSeedLedgerDoc(t, s)
	if doc == nil || !reflect.DeepEqual(doc.HarnessConfigs, []string{name}) {
		t.Fatalf("ledger after failed sync = %+v, want harness_configs [%s]", doc, name)
	}

	// Delete the row, then bootstrap with a working sync: it stays deleted.
	srv.SetStorage(stor)
	hc, err := s.GetHarnessConfigBySlug(ctx, name, store.HarnessConfigScopeGlobal, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteHarnessConfig(ctx, hc.ID); err != nil {
		t.Fatal(err)
	}
	if err := srv.BootstrapHarnessConfigsFromDir(ctx, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetHarnessConfigBySlug(ctx, name, store.HarnessConfigScopeGlobal, ""); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("deleted built-in %q re-created (err=%v)", name, err)
	}
}
