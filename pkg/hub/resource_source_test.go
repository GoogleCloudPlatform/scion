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
	"io/fs"
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/resources"
)

// testFS creates a minimal in-memory fs.FS with the given file contents.
func testFS(files map[string]string) fs.FS {
	m := fstest.MapFS{}
	for path, content := range files {
		m[path] = &fstest.MapFile{Data: []byte(content), Mode: 0644}
	}
	return m
}

// testBundledResource creates a BundledResource for testing.
func testBundledResource(kind storage.ResourceKind, name string, files map[string]string) resources.BundledResource {
	return resources.BundledResource{
		Kind:      kind,
		Name:      name,
		Scope:     "global",
		ScopeID:   "",
		SourceURL: "builtin://scion/dev/" + string(kind) + "/" + name,
		FS:        testFS(files),
		Root:      ".",
	}
}

func TestFSResourceSource_Files(t *testing.T) {
	br := testBundledResource(storage.ResourceKindTemplate, "default", map[string]string{
		"scion-agent.yaml": "harness: claude\n",
		"home/.bashrc":     "# bashrc",
	})
	src := NewFSResourceSource(br)

	files, err := src.Files(context.Background())
	if err != nil {
		t.Fatalf("Files() failed: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(files))
	}

	paths := map[string]bool{}
	for _, f := range files {
		paths[f.Path] = true
		if f.Hash == "" {
			t.Errorf("expected non-empty hash for %s", f.Path)
		}
		if f.Size == 0 {
			t.Errorf("expected non-zero size for %s", f.Path)
		}
	}
	if !paths["scion-agent.yaml"] {
		t.Error("missing scion-agent.yaml")
	}
	if !paths["home/.bashrc"] {
		t.Error("missing home/.bashrc")
	}
}

func TestFSResourceSource_Metadata(t *testing.T) {
	br := testBundledResource(storage.ResourceKindHarnessConfig, "claude", map[string]string{
		"config.yaml": "harness: claude\n",
	})
	src := NewFSResourceSource(br)

	meta, err := src.Metadata(context.Background())
	if err != nil {
		t.Fatalf("Metadata() failed: %v", err)
	}
	if meta.Kind != storage.ResourceKindHarnessConfig {
		t.Errorf("expected kind %q, got %q", storage.ResourceKindHarnessConfig, meta.Kind)
	}
	if meta.Name != "claude" {
		t.Errorf("expected name claude, got %q", meta.Name)
	}
	if meta.Scope != "global" {
		t.Errorf("expected scope global, got %q", meta.Scope)
	}
	if !strings.HasPrefix(meta.SourceURL, "builtin://scion/") {
		t.Errorf("expected builtin source URL, got %q", meta.SourceURL)
	}
}

func TestIsBuiltinManaged(t *testing.T) {
	tests := []struct {
		sourceURL string
		want      bool
	}{
		{"builtin://scion/dev/template/default", true},
		{"builtin://scion/v1.0.0/harness-config/claude", true},
		{"https://github.com/example/templates", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := IsBuiltinManaged(tt.sourceURL); got != tt.want {
			t.Errorf("IsBuiltinManaged(%q) = %v, want %v", tt.sourceURL, got, tt.want)
		}
	}
}

func TestBootstrapSource_CreateTemplate(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()

	br := testBundledResource(storage.ResourceKindTemplate, "my-template", map[string]string{
		"scion-agent.yaml": "harness: claude\nimage: test:latest\n",
		"home/.bashrc":     "# bashrc content",
	})
	src := NewFSResourceSource(br)

	rs := srv.templateStore()
	result, err := rs.BootstrapSource(ctx, src, BootstrapOptions{
		OverwritePolicy: OverwriteBuiltinManaged,
	})
	if err != nil {
		t.Fatalf("BootstrapSource failed: %v", err)
	}
	if result.Created != 1 {
		t.Errorf("expected Created=1, got %d", result.Created)
	}

	templates, err := s.ListTemplates(ctx, store.TemplateFilter{}, store.ListOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if templates.TotalCount != 1 {
		t.Fatalf("expected 1 template, got %d", templates.TotalCount)
	}

	tmpl := templates.Items[0]
	if tmpl.Name != "my-template" {
		t.Errorf("expected name 'my-template', got %q", tmpl.Name)
	}
	if tmpl.Status != store.TemplateStatusActive {
		t.Errorf("expected status active, got %q", tmpl.Status)
	}
	if tmpl.ContentHash == "" {
		t.Error("expected non-empty content hash")
	}
	if len(tmpl.Files) != 2 {
		t.Errorf("expected 2 files, got %d", len(tmpl.Files))
	}
	if !IsBuiltinManaged(tmpl.SourceURL) {
		t.Errorf("expected built-in source URL, got %q", tmpl.SourceURL)
	}
}

func TestBootstrapSource_CreateHarnessConfig(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()

	br := testBundledResource(storage.ResourceKindHarnessConfig, "claude", map[string]string{
		"config.yaml":  "harness: claude\nimage: scion-claude:latest\nuser: scion\n",
		"home/.bashrc": "# bashrc",
	})
	src := NewFSResourceSource(br)

	rs := srv.harnessConfigStore("claude")
	result, err := rs.BootstrapSource(ctx, src, BootstrapOptions{
		OverwritePolicy: OverwriteBuiltinManaged,
	})
	if err != nil {
		t.Fatalf("BootstrapSource failed: %v", err)
	}
	if result.Created != 1 {
		t.Errorf("expected Created=1, got %d", result.Created)
	}

	configs, err := s.ListHarnessConfigs(ctx, store.HarnessConfigFilter{}, store.ListOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if configs.TotalCount != 1 {
		t.Fatalf("expected 1 harness config, got %d", configs.TotalCount)
	}

	hc := configs.Items[0]
	if hc.Name != "claude" {
		t.Errorf("expected name 'claude', got %q", hc.Name)
	}
	if hc.Harness != "claude" {
		t.Errorf("expected harness 'claude', got %q", hc.Harness)
	}
	if hc.Status != store.HarnessConfigStatusActive {
		t.Errorf("expected status active, got %q", hc.Status)
	}
	if hc.ContentHash == "" {
		t.Error("expected non-empty content hash")
	}
	if len(hc.Files) != 2 {
		t.Errorf("expected 2 files, got %d", len(hc.Files))
	}
	if !IsBuiltinManaged(hc.SourceURL) {
		t.Errorf("expected built-in source URL, got %q", hc.SourceURL)
	}
}

func TestBootstrapSource_NoOpOnHashMatch(t *testing.T) {
	srv, _, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()

	br := testBundledResource(storage.ResourceKindTemplate, "stable", map[string]string{
		"config.yaml": "version: 1\n",
	})
	src := NewFSResourceSource(br)
	rs := srv.templateStore()

	// First bootstrap: creates the resource
	r1, err := rs.BootstrapSource(ctx, src, BootstrapOptions{
		OverwritePolicy: OverwriteBuiltinManaged,
	})
	if err != nil {
		t.Fatalf("first bootstrap failed: %v", err)
	}
	if r1.Created != 1 {
		t.Fatalf("expected Created=1 on first run, got %d", r1.Created)
	}

	// Second bootstrap with identical content: should skip
	r2, err := rs.BootstrapSource(ctx, src, BootstrapOptions{
		OverwritePolicy: OverwriteBuiltinManaged,
	})
	if err != nil {
		t.Fatalf("second bootstrap failed: %v", err)
	}
	if r2.Skipped != 1 {
		t.Errorf("expected Skipped=1 on second run, got %d", r2.Skipped)
	}
	if r2.Created != 0 {
		t.Errorf("expected Created=0 on second run, got %d", r2.Created)
	}
	if r2.Updated != 0 {
		t.Errorf("expected Updated=0 on second run, got %d", r2.Updated)
	}
}

func TestBootstrapSource_UpdateBuiltinManaged(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()

	// First create with version 1
	br1 := testBundledResource(storage.ResourceKindTemplate, "evolving", map[string]string{
		"config.yaml": "version: 1\n",
	})
	src1 := NewFSResourceSource(br1)
	rs := srv.templateStore()

	r1, err := rs.BootstrapSource(ctx, src1, BootstrapOptions{
		OverwritePolicy: OverwriteBuiltinManaged,
	})
	if err != nil {
		t.Fatalf("first bootstrap failed: %v", err)
	}
	if r1.Created != 1 {
		t.Fatalf("expected Created=1, got %d", r1.Created)
	}

	// Capture original hash
	tmpl, err := s.GetTemplateBySlug(ctx, "evolving", "global", "")
	if err != nil {
		t.Fatal(err)
	}
	origHash := tmpl.ContentHash

	// Now bootstrap with changed content
	br2 := testBundledResource(storage.ResourceKindTemplate, "evolving", map[string]string{
		"config.yaml": "version: 2\nnew-field: true\n",
	})
	src2 := NewFSResourceSource(br2)

	r2, err := rs.BootstrapSource(ctx, src2, BootstrapOptions{
		OverwritePolicy: OverwriteBuiltinManaged,
	})
	if err != nil {
		t.Fatalf("second bootstrap failed: %v", err)
	}
	if r2.Updated != 1 {
		t.Errorf("expected Updated=1, got %d", r2.Updated)
	}

	tmpl2, err := s.GetTemplateBySlug(ctx, "evolving", "global", "")
	if err != nil {
		t.Fatal(err)
	}
	if tmpl2.ContentHash == origHash {
		t.Error("expected content hash to change after update")
	}
	if tmpl2.Status != store.TemplateStatusActive {
		t.Errorf("expected active status after update, got %q", tmpl2.Status)
	}
}

func TestBootstrapSource_SkipNonBuiltinConflict(t *testing.T) {
	srv, s, stor := testTemplateBootstrapServer(t)
	ctx := context.Background()

	// Pre-create a non-built-in template with the same slug
	existing := &store.Template{
		ID:            tid("conflict-template"),
		Name:          "conflict",
		Slug:          "conflict",
		Scope:         "global",
		ScopeID:       "",
		Status:        store.TemplateStatusActive,
		SourceURL:     "https://github.com/user/templates",
		StoragePath:   "templates/global/conflict",
		StorageBucket: stor.Bucket(),
		StorageURI:    "gs://test-bucket/templates/global/conflict",
		Visibility:    store.VisibilityPrivate,
	}
	if err := s.CreateTemplate(ctx, existing); err != nil {
		t.Fatalf("pre-create failed: %v", err)
	}

	// Now try to bootstrap a built-in with the same name
	br := testBundledResource(storage.ResourceKindTemplate, "conflict", map[string]string{
		"config.yaml": "version: builtin\n",
	})
	src := NewFSResourceSource(br)
	rs := srv.templateStore()

	result, err := rs.BootstrapSource(ctx, src, BootstrapOptions{
		OverwritePolicy: OverwriteBuiltinManaged,
	})
	if err != nil {
		t.Fatalf("BootstrapSource failed: %v", err)
	}
	if result.Skipped != 1 {
		t.Errorf("expected Skipped=1, got %d", result.Skipped)
	}
	if result.Created != 0 {
		t.Errorf("expected Created=0, got %d", result.Created)
	}

	// Verify the existing template was not modified
	tmpl, err := s.GetTemplateBySlug(ctx, "conflict", "global", "")
	if err != nil {
		t.Fatal(err)
	}
	if tmpl.SourceURL != "https://github.com/user/templates" {
		t.Errorf("existing template source URL was modified: %q", tmpl.SourceURL)
	}
}

// TODO: This test exercises the normal update path (GetBySlug finds pre-existing record).
// A proper race test requires a mock persistence where GetBySlug returns nil on first call
// but Create returns ErrAlreadyExists, simulating the HA window between read and write.
// Add this test before or alongside the HA integration tests.
func TestBootstrapSource_DuplicateCreateRace(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()

	// Pre-create a built-in template to simulate a race where another replica
	// created it between our GetBySlug and Create calls.
	existing := &store.Template{
		ID:            tid("raced-template"),
		Name:          "raced",
		Slug:          "raced",
		Scope:         "global",
		ScopeID:       "",
		Status:        store.TemplateStatusActive,
		SourceURL:     "builtin://scion/dev/template/raced",
		ContentHash:   "old-hash",
		StoragePath:   "templates/global/raced",
		StorageBucket: "test-bucket",
		StorageURI:    "gs://test-bucket/templates/global/raced",
		Visibility:    store.VisibilityPrivate,
	}
	if err := s.CreateTemplate(ctx, existing); err != nil {
		t.Fatalf("pre-create failed: %v", err)
	}

	// Bootstrap with a built-in resource that has the same name.
	// The Create will fail with ErrAlreadyExists, then BootstrapSource should
	// re-read and fall through to the update path.
	br := testBundledResource(storage.ResourceKindTemplate, "raced", map[string]string{
		"config.yaml": "version: raced\n",
	})
	src := NewFSResourceSource(br)
	rs := srv.templateStore()

	result, err := rs.BootstrapSource(ctx, src, BootstrapOptions{
		OverwritePolicy: OverwriteBuiltinManaged,
	})
	if err != nil {
		t.Fatalf("BootstrapSource failed: %v", err)
	}

	// Since the pre-existing record has a different content hash, the race
	// recovery should update it.
	if result.Updated != 1 && result.Skipped != 1 {
		t.Errorf("expected Updated=1 or Skipped=1 after race recovery, got Created=%d Updated=%d Skipped=%d",
			result.Created, result.Updated, result.Skipped)
	}

	// Verify the template still exists and is active
	tmpl, err := s.GetTemplateBySlug(ctx, "raced", "global", "")
	if err != nil {
		t.Fatal(err)
	}
	if tmpl.Status != store.TemplateStatusActive {
		t.Errorf("expected active status, got %q", tmpl.Status)
	}
}

func TestBootstrapSource_OverwriteNever(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()

	// Create an initial resource
	br := testBundledResource(storage.ResourceKindTemplate, "readonly", map[string]string{
		"config.yaml": "version: 1\n",
	})
	src := NewFSResourceSource(br)
	rs := srv.templateStore()

	r1, err := rs.BootstrapSource(ctx, src, BootstrapOptions{
		OverwritePolicy: OverwriteBuiltinManaged,
	})
	if err != nil {
		t.Fatalf("first bootstrap failed: %v", err)
	}
	if r1.Created != 1 {
		t.Fatalf("expected Created=1, got %d", r1.Created)
	}

	origHash, _ := func() (string, error) {
		tmpl, err := s.GetTemplateBySlug(ctx, "readonly", "global", "")
		if err != nil {
			return "", err
		}
		return tmpl.ContentHash, nil
	}()

	// Try to update with OverwriteNever — should skip
	br2 := testBundledResource(storage.ResourceKindTemplate, "readonly", map[string]string{
		"config.yaml": "version: 2\n",
	})
	src2 := NewFSResourceSource(br2)

	r2, err := rs.BootstrapSource(ctx, src2, BootstrapOptions{
		OverwritePolicy: OverwriteNever,
	})
	if err != nil {
		t.Fatalf("second bootstrap failed: %v", err)
	}
	if r2.Skipped != 1 {
		t.Errorf("expected Skipped=1 with OverwriteNever, got %d", r2.Skipped)
	}

	// Verify content was NOT updated
	tmpl, err := s.GetTemplateBySlug(ctx, "readonly", "global", "")
	if err != nil {
		t.Fatal(err)
	}
	if tmpl.ContentHash != origHash {
		t.Error("expected content hash unchanged with OverwriteNever")
	}
}

func TestBootstrapSource_OverwriteAlways(t *testing.T) {
	srv, s, stor := testTemplateBootstrapServer(t)
	ctx := context.Background()

	// Pre-create a non-built-in template
	existing := &store.Template{
		ID:            tid("admin-override-template"),
		Name:          "admin-override",
		Slug:          "admin-override",
		Scope:         "global",
		ScopeID:       "",
		Status:        store.TemplateStatusActive,
		SourceURL:     "https://github.com/user/custom",
		ContentHash:   "custom-hash",
		StoragePath:   "templates/global/admin-override",
		StorageBucket: stor.Bucket(),
		StorageURI:    "gs://test-bucket/templates/global/admin-override",
		Visibility:    store.VisibilityPrivate,
	}
	if err := s.CreateTemplate(ctx, existing); err != nil {
		t.Fatalf("pre-create failed: %v", err)
	}

	// OverwriteAlways should overwrite even non-built-in resources
	br := testBundledResource(storage.ResourceKindTemplate, "admin-override", map[string]string{
		"config.yaml": "version: forced\n",
	})
	src := NewFSResourceSource(br)
	rs := srv.templateStore()

	result, err := rs.BootstrapSource(ctx, src, BootstrapOptions{
		OverwritePolicy: OverwriteAlways,
	})
	if err != nil {
		t.Fatalf("BootstrapSource failed: %v", err)
	}
	if result.Updated != 1 {
		t.Errorf("expected Updated=1 with OverwriteAlways, got Updated=%d Skipped=%d",
			result.Updated, result.Skipped)
	}

	// Verify the source URL was updated to the built-in one
	tmpl, err := s.GetTemplateBySlug(ctx, "admin-override", "global", "")
	if err != nil {
		t.Fatal(err)
	}
	if !IsBuiltinManaged(tmpl.SourceURL) {
		t.Errorf("expected built-in source URL after OverwriteAlways, got %q", tmpl.SourceURL)
	}
}

func TestStageResourceSource(t *testing.T) {
	br := testBundledResource(storage.ResourceKindTemplate, "stage-test", map[string]string{
		"a.txt":       "hello",
		"sub/b.txt":   "world",
		"sub/c/d.txt": "nested",
	})
	src := NewFSResourceSource(br)

	dir, cleanup, err := stageResourceSource(src)
	if err != nil {
		t.Fatalf("stageResourceSource failed: %v", err)
	}
	defer cleanup()

	if dir == "" {
		t.Fatal("expected non-empty dir")
	}

	// Verify files were staged correctly
	entries := map[string]bool{}
	err = fs.WalkDir(os.DirFS(dir), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			entries[path] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	expected := []string{"a.txt", "sub/b.txt", "sub/c/d.txt"}
	for _, e := range expected {
		if !entries[e] {
			t.Errorf("missing staged file: %s", e)
		}
	}
}
