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

package transfer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewManifestBuilder(t *testing.T) {
	builder := NewManifestBuilder("/tmp/test")

	if builder.BasePath != "/tmp/test" {
		t.Errorf("unexpected BasePath: %s", builder.BasePath)
	}

	// Check default exclude patterns
	if len(builder.ExcludePatterns) != len(DefaultExcludePatterns) {
		t.Errorf("expected %d default exclude patterns, got %d",
			len(DefaultExcludePatterns), len(builder.ExcludePatterns))
	}
}

func TestManifestBuilder_WithExcludePatterns(t *testing.T) {
	builder := NewManifestBuilder("/tmp/test").
		WithExcludePatterns([]string{"*.log", "node_modules/**"})

	expectedCount := len(DefaultExcludePatterns) + 2
	if len(builder.ExcludePatterns) != expectedCount {
		t.Errorf("expected %d patterns, got %d", expectedCount, len(builder.ExcludePatterns))
	}
}

func TestManifestBuilder_Build(t *testing.T) {
	// Create a temporary directory structure
	dir := t.TempDir()

	// Create test files
	files := map[string]string{
		"file1.txt":     "content1",
		"dir/file2.txt": "content2",
		"dir/file3.txt": "content3",
	}

	for path, content := range files {
		fullPath := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
			t.Fatalf("failed to create directory: %v", err)
		}
		if err := os.WriteFile(fullPath, []byte(content), 0644); err != nil {
			t.Fatalf("failed to create file: %v", err)
		}
	}

	builder := NewManifestBuilder(dir)
	manifest, err := builder.Build()
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	if manifest.Version != "1.0" {
		t.Errorf("unexpected version: %s", manifest.Version)
	}

	if len(manifest.Files) != len(files) {
		t.Errorf("expected %d files, got %d", len(files), len(manifest.Files))
	}

	// Verify content hash is computed
	if manifest.ContentHash == "" {
		t.Error("expected ContentHash to be set")
	}

	// Verify files are sorted
	for i := 1; i < len(manifest.Files); i++ {
		if manifest.Files[i].Path < manifest.Files[i-1].Path {
			t.Error("files should be sorted by path")
		}
	}
}

func TestManifestBuilder_ExcludesGit(t *testing.T) {
	dir := t.TempDir()

	// Create .git directory with files
	gitDir := filepath.Join(dir, ".git")
	if err := os.MkdirAll(gitDir, 0755); err != nil {
		t.Fatalf("failed to create .git: %v", err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "config"), []byte("config"), 0644); err != nil {
		t.Fatalf("failed to create .git/config: %v", err)
	}

	// Create regular file
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main"), 0644); err != nil {
		t.Fatalf("failed to create main.go: %v", err)
	}

	builder := NewManifestBuilder(dir)
	manifest, err := builder.Build()
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	// Should only have main.go
	if len(manifest.Files) != 1 {
		t.Errorf("expected 1 file, got %d", len(manifest.Files))
	}

	if manifest.Files[0].Path != "main.go" {
		t.Errorf("unexpected file: %s", manifest.Files[0].Path)
	}
}

func TestManifestBuilder_ExcludesDSStore(t *testing.T) {
	dir := t.TempDir()

	// Create .DS_Store files
	if err := os.WriteFile(filepath.Join(dir, ".DS_Store"), []byte("ds"), 0644); err != nil {
		t.Fatalf("failed to create .DS_Store: %v", err)
	}

	subdir := filepath.Join(dir, "subdir")
	if err := os.MkdirAll(subdir, 0755); err != nil {
		t.Fatalf("failed to create subdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(subdir, ".DS_Store"), []byte("ds"), 0644); err != nil {
		t.Fatalf("failed to create subdir/.DS_Store: %v", err)
	}

	// Create regular file
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main"), 0644); err != nil {
		t.Fatalf("failed to create main.go: %v", err)
	}

	builder := NewManifestBuilder(dir)
	manifest, err := builder.Build()
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	// Should only have main.go
	if len(manifest.Files) != 1 {
		t.Errorf("expected 1 file, got %d", len(manifest.Files))
	}

	for _, f := range manifest.Files {
		if strings.Contains(f.Path, ".DS_Store") {
			t.Errorf("should not include .DS_Store: %s", f.Path)
		}
	}
}

func TestCollectFiles(t *testing.T) {
	dir := t.TempDir()

	// Create test files
	if err := os.WriteFile(filepath.Join(dir, "file1.txt"), []byte("content1"), 0644); err != nil {
		t.Fatalf("failed to create file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "file2.txt"), []byte("content2"), 0755); err != nil {
		t.Fatalf("failed to create file: %v", err)
	}

	files, err := CollectFiles(dir, nil)
	if err != nil {
		t.Fatalf("CollectFiles failed: %v", err)
	}

	if len(files) != 2 {
		t.Errorf("expected 2 files, got %d", len(files))
	}

	// Verify FullPath is set
	for _, f := range files {
		if f.FullPath == "" {
			t.Error("expected FullPath to be set")
		}
		if !filepath.IsAbs(f.FullPath) {
			t.Errorf("FullPath should be absolute: %s", f.FullPath)
		}
	}
}

func TestCollectFiles_WithExcludePatterns(t *testing.T) {
	dir := t.TempDir()

	// Create test files
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("code"), 0644); err != nil {
		t.Fatalf("failed to create file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "debug.log"), []byte("logs"), 0644); err != nil {
		t.Fatalf("failed to create file: %v", err)
	}

	files, err := CollectFiles(dir, []string{"*.log"})
	if err != nil {
		t.Fatalf("CollectFiles failed: %v", err)
	}

	if len(files) != 1 {
		t.Errorf("expected 1 file (excluding .log), got %d", len(files))
	}

	if files[0].Path != "main.go" {
		t.Errorf("unexpected file: %s", files[0].Path)
	}
}

func TestBuildManifest(t *testing.T) {
	files := []FileInfo{
		{Path: "b.txt", Size: 100, Hash: "sha256:bbb"},
		{Path: "a.txt", Size: 50, Hash: "sha256:aaa"},
	}

	manifest := BuildManifest(files)

	if manifest.Version != "1.0" {
		t.Errorf("unexpected version: %s", manifest.Version)
	}

	if manifest.ContentHash == "" {
		t.Error("expected ContentHash to be set")
	}

	if len(manifest.Files) != 2 {
		t.Errorf("expected 2 files, got %d", len(manifest.Files))
	}
}

func TestBuildManifest_Empty(t *testing.T) {
	manifest := BuildManifest([]FileInfo{})

	if manifest.Version != "1.0" {
		t.Errorf("unexpected version: %s", manifest.Version)
	}

	if len(manifest.Files) != 0 {
		t.Errorf("expected 0 files, got %d", len(manifest.Files))
	}
}
