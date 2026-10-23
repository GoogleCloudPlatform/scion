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
	"slices"
	"strings"
	"testing"
)

func TestManifestBuilder_RecursiveExcludeBoundary(t *testing.T) {
	cases := []struct {
		name     string
		patterns []string
		path     string
		excluded bool
	}{
		{"default_root_store", nil, ".DS_Store", true},
		{"default_nested_store", nil, "nested/.DS_Store", true},
		{"default_root_lookalike", nil, "report.DS_Store", false},
		{"default_nested_lookalike", nil, "nested/report.DS_Store", false},
		{"recursive_file", []string{"**/cache"}, "cache", true},
		{"recursive_nested_file", []string{"**/cache"}, "nested/cache", true},
		{"recursive_directory", []string{"**/cache"}, "nested/cache/drop.txt", true},
		{"recursive_prefixed_file", []string{"**/cache"}, "mycache", false},
		{"recursive_nested_prefixed_file", []string{"**/cache"}, "nested/mycache", false},
		{"recursive_prefixed_directory", []string{"**/cache"}, "nested/mycache/keep.txt", false},
		{"compound_suffix", []string{"**/cache/data.txt"}, "cache/data.txt", true},
		{"nested_compound_suffix", []string{"**/cache/data.txt"}, "nested/cache/data.txt", true},
		{"compound_lookalike", []string{"**/cache/data.txt"}, "mycache/data.txt", false},
		{"nested_compound_lookalike", []string{"**/cache/data.txt"}, "nested/mycache/data.txt", false},
		{"recursive_glob_root", []string{"**/*.log"}, "debug.log", true},
		{"recursive_glob_nested", []string{"**/*.log"}, "nested/debug.log", true},
		{"recursive_glob_deep", []string{"**/*.log"}, "a/b/debug.log", true},
		{"recursive_glob_root_lookalike", []string{"**/*.log"}, "debug.log.txt", false},
		{"recursive_glob_nested_lookalike", []string{"**/*.log"}, "nested/debug.log.txt", false},
		{"recursive_glob_compound_root", []string{"**/logs/*.json"}, "logs/data.json", true},
		{"recursive_glob_compound_nested", []string{"**/logs/*.json"}, "nested/logs/data.json", true},
		{"recursive_glob_compound_deep", []string{"**/logs/*.json"}, "a/b/logs/data.json", true},
		{"recursive_glob_directory_lookalike", []string{"**/logs/*.json"}, "nested/mylogs/data.json", false},
		{"recursive_glob_file_lookalike", []string{"**/logs/*.json"}, "logs/data.json.txt", false},
		{"recursive_glob_star_stops_at_slash", []string{"**/logs/*.json"}, "logs/deep/data.json", false},
		{"recursive_glob_directory", []string{"**/cache-*"}, "nested/cache-one/drop.txt", true},
		{"recursive_glob_prefixed_directory", []string{"**/cache-*"}, "nested/mycache-one/keep.txt", false},
		{"recursive_glob_compound_directory", []string{"**/logs-*/*.json"}, "nested/logs-one/data.json", true},
		{"recursive_glob_compound_directory_lookalike", []string{"**/logs-*/*.json"}, "nested/mylogs-one/data.json", false},
		{"recursive_glob_compound_directory_depth", []string{"**/logs-*/*.json"}, "logs-one/deep/data.json", false},
		{"recursive_glob_question", []string{"**/file?.log"}, "nested/file1.log", true},
		{"recursive_glob_question_lookalike", []string{"**/file?.log"}, "nested/file12.log", false},
		{"recursive_glob_question_unicode", []string{"**/file?.log"}, "nested/fileé.log", true},
		{"recursive_glob_class", []string{"**/[ab]?.json"}, "nested/a1.json", true},
		{"recursive_glob_class_lookalike", []string{"**/[ab]?.json"}, "nested/c1.json", false},
		{"recursive_glob_escaped", []string{`**/\[draft\].txt`}, "nested/[draft].txt", true},
		{"recursive_glob_escaped_lookalike", []string{`**/\[draft\].txt`}, "nested/draft.txt", false},
		{"recursive_glob_malformed", []string{"**/["}, "[", false},
		{"recursive_glob_after_malformed", []string{"**/[", "**/*.log"}, "nested/debug.log", true},
		{"simple_glob", []string{"*.log"}, "nested/debug.log", true},
		{"simple_glob_lookalike", []string{"*.log"}, "nested/debug.log.txt", false},
		{"root_tree", []string{"build/**"}, "build/output.txt", true},
		{"root_tree_nested", []string{"build/**"}, "nested/build/output.txt", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			fullPath := filepath.Join(dir, filepath.FromSlash(tc.path))
			if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(fullPath, []byte("content"), 0644); err != nil {
				t.Fatal(err)
			}
			collected, err := CollectFiles(dir, tc.patterns)
			if err != nil {
				t.Fatal(err)
			}
			manifest, err := NewManifestBuilder(dir).WithExcludePatterns(tc.patterns).Build()
			if err != nil {
				t.Fatal(err)
			}
			want := []string{tc.path}
			if tc.excluded {
				want = nil
			}
			for label, files := range map[string][]FileInfo{"CollectFiles": collected, "Build": manifest.Files} {
				var got []string
				for _, f := range files {
					got = append(got, f.Path)
				}
				if !slices.Equal(got, want) {
					t.Errorf("%s collected %v, want %v", label, got, want)
				}
			}
		})
	}
}

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

func TestManifestBuilder_IncludesDotfiles(t *testing.T) {
	dir := t.TempDir()

	// Create a home/.tmux.conf dotfile
	homeDir := filepath.Join(dir, "home")
	if err := os.MkdirAll(homeDir, 0755); err != nil {
		t.Fatalf("failed to create home dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(homeDir, ".tmux.conf"), []byte("set -g mouse on"), 0644); err != nil {
		t.Fatalf("failed to create .tmux.conf: %v", err)
	}

	// Create a regular file too
	if err := os.WriteFile(filepath.Join(dir, "scion-agent.yaml"), []byte("name: test"), 0644); err != nil {
		t.Fatalf("failed to create scion-agent.yaml: %v", err)
	}

	builder := NewManifestBuilder(dir)
	manifest, err := builder.Build()
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	// Should include both files
	if len(manifest.Files) != 2 {
		t.Errorf("expected 2 files, got %d", len(manifest.Files))
	}

	// Verify home/.tmux.conf is in the manifest with the correct relative path
	found := false
	for _, f := range manifest.Files {
		if f.Path == "home/.tmux.conf" {
			found = true
			break
		}
	}
	if !found {
		paths := make([]string, len(manifest.Files))
		for i, f := range manifest.Files {
			paths[i] = f.Path
		}
		t.Errorf("expected manifest to include home/.tmux.conf, got: %v", paths)
	}
}

func TestCollectFiles_SkipsSymlinks(t *testing.T) {
	dir := t.TempDir()

	// Create a regular file
	if err := os.WriteFile(filepath.Join(dir, "real.txt"), []byte("content"), 0644); err != nil {
		t.Fatalf("failed to create file: %v", err)
	}

	// Create a valid symlink
	validTarget := filepath.Join(dir, "real.txt")
	if err := os.Symlink(validTarget, filepath.Join(dir, "valid-link")); err != nil {
		t.Fatalf("failed to create valid symlink: %v", err)
	}

	// Create a dangling symlink (like .claude/debug/latest)
	if err := os.Symlink("/nonexistent/path", filepath.Join(dir, "dangling-link")); err != nil {
		t.Fatalf("failed to create dangling symlink: %v", err)
	}

	files, err := CollectFiles(dir, nil)
	if err != nil {
		t.Fatalf("CollectFiles failed: %v", err)
	}

	// Should only include the real file, not any symlinks
	if len(files) != 1 {
		names := make([]string, len(files))
		for i, f := range files {
			names[i] = f.Path
		}
		t.Errorf("expected 1 file (skipping symlinks), got %d: %v", len(files), names)
	}

	if files[0].Path != "real.txt" {
		t.Errorf("expected real.txt, got %s", files[0].Path)
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
