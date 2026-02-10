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

package templatecache

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNew(t *testing.T) {
	tmpDir := t.TempDir()

	// Test creating a new cache
	cache, err := New(tmpDir, 0)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if cache == nil {
		t.Fatal("New() returned nil cache")
	}

	// Verify default max size
	stats := cache.Stats()
	if stats.MaxSize != DefaultMaxSize {
		t.Errorf("Expected default max size %d, got %d", DefaultMaxSize, stats.MaxSize)
	}

	// Test with custom max size
	customSize := int64(50 * 1024 * 1024)
	cache2, err := New(filepath.Join(tmpDir, "custom"), customSize)
	if err != nil {
		t.Fatalf("New() with custom size error = %v", err)
	}
	stats2 := cache2.Stats()
	if stats2.MaxSize != customSize {
		t.Errorf("Expected max size %d, got %d", customSize, stats2.MaxSize)
	}
}

func TestStoreAndGet(t *testing.T) {
	tmpDir := t.TempDir()
	cache, err := New(tmpDir, DefaultMaxSize)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	templateID := "test-template-1"
	contentHash := "abc123hash"
	files := map[string][]byte{
		"scion-agent.yaml": []byte("harness: claude\n"),
		"home/.claude/CLAUDE.md": []byte("# Test Template\n"),
	}

	// Store template
	storedPath, err := cache.Store(templateID, contentHash, files)
	if err != nil {
		t.Fatalf("Store() error = %v", err)
	}
	if storedPath == "" {
		t.Fatal("Store() returned empty path")
	}

	// Verify files were written
	yamlPath := filepath.Join(storedPath, "scion-agent.yaml")
	content, err := os.ReadFile(yamlPath)
	if err != nil {
		t.Fatalf("Failed to read stored file: %v", err)
	}
	if string(content) != "harness: claude\n" {
		t.Errorf("File content mismatch: got %q", string(content))
	}

	// Get template
	gotPath, ok := cache.Get(templateID, contentHash)
	if !ok {
		t.Fatal("Get() returned false")
	}
	if gotPath != storedPath {
		t.Errorf("Get() path = %v, want %v", gotPath, storedPath)
	}

	// Get with wrong hash should fail
	_, ok = cache.Get(templateID, "wrong-hash")
	if ok {
		t.Error("Get() with wrong hash should return false")
	}

	// Get non-existent template
	_, ok = cache.Get("non-existent", contentHash)
	if ok {
		t.Error("Get() for non-existent template should return false")
	}
}

func TestGetByHash(t *testing.T) {
	tmpDir := t.TempDir()
	cache, err := New(tmpDir, DefaultMaxSize)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	contentHash := "unique-hash-456"
	files := map[string][]byte{
		"config.yaml": []byte("test: true\n"),
	}

	// Store template
	storedPath, err := cache.Store("template-id", contentHash, files)
	if err != nil {
		t.Fatalf("Store() error = %v", err)
	}

	// Get by hash
	gotPath, ok := cache.GetByHash(contentHash)
	if !ok {
		t.Fatal("GetByHash() returned false")
	}
	if gotPath != storedPath {
		t.Errorf("GetByHash() path = %v, want %v", gotPath, storedPath)
	}

	// Get non-existent hash
	_, ok = cache.GetByHash("non-existent-hash")
	if ok {
		t.Error("GetByHash() for non-existent hash should return false")
	}
}

func TestEviction(t *testing.T) {
	tmpDir := t.TempDir()

	// Create cache with very small max size (1KB)
	cache, err := New(tmpDir, 1024)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	// Store first template (500 bytes)
	files1 := map[string][]byte{
		"file.txt": make([]byte, 500),
	}
	_, err = cache.Store("template-1", "hash-1", files1)
	if err != nil {
		t.Fatalf("Store() error = %v", err)
	}

	// Wait a bit to ensure different timestamps
	time.Sleep(10 * time.Millisecond)

	// Store second template (500 bytes)
	files2 := map[string][]byte{
		"file.txt": make([]byte, 500),
	}
	_, err = cache.Store("template-2", "hash-2", files2)
	if err != nil {
		t.Fatalf("Store() error = %v", err)
	}

	// Wait a bit
	time.Sleep(10 * time.Millisecond)

	// Store third template (500 bytes) - should trigger eviction of oldest
	files3 := map[string][]byte{
		"file.txt": make([]byte, 500),
	}
	_, err = cache.Store("template-3", "hash-3", files3)
	if err != nil {
		t.Fatalf("Store() error = %v", err)
	}

	// First template should be evicted (LRU)
	_, ok := cache.Get("template-1", "hash-1")
	if ok {
		t.Error("template-1 should have been evicted")
	}

	// Third template should still exist
	_, ok = cache.Get("template-3", "hash-3")
	if !ok {
		t.Error("template-3 should exist")
	}
}

func TestClear(t *testing.T) {
	tmpDir := t.TempDir()
	cache, err := New(tmpDir, DefaultMaxSize)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	// Store some templates
	files := map[string][]byte{"file.txt": []byte("test")}
	_, err = cache.Store("t1", "h1", files)
	if err != nil {
		t.Fatalf("Store() error = %v", err)
	}
	_, err = cache.Store("t2", "h2", files)
	if err != nil {
		t.Fatalf("Store() error = %v", err)
	}

	// Verify cache is not empty
	stats := cache.Stats()
	if stats.EntryCount == 0 {
		t.Error("Cache should not be empty after storing")
	}

	// Clear cache
	if err := cache.Clear(); err != nil {
		t.Fatalf("Clear() error = %v", err)
	}

	// Verify cache is empty
	stats = cache.Stats()
	if stats.EntryCount != 0 {
		t.Errorf("Cache should be empty after Clear(), got %d entries", stats.EntryCount)
	}
	if stats.TotalSize != 0 {
		t.Errorf("Cache size should be 0 after Clear(), got %d", stats.TotalSize)
	}
}

func TestStats(t *testing.T) {
	tmpDir := t.TempDir()
	cache, err := New(tmpDir, 1024*1024) // 1MB
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	// Initial stats
	stats := cache.Stats()
	if stats.EntryCount != 0 {
		t.Errorf("Initial entry count should be 0, got %d", stats.EntryCount)
	}
	if stats.TotalSize != 0 {
		t.Errorf("Initial total size should be 0, got %d", stats.TotalSize)
	}

	// Store a template
	files := map[string][]byte{"file.txt": []byte("hello world")}
	_, err = cache.Store("test", "hash", files)
	if err != nil {
		t.Fatalf("Store() error = %v", err)
	}

	// Check updated stats
	stats = cache.Stats()
	if stats.EntryCount != 1 {
		t.Errorf("Entry count should be 1, got %d", stats.EntryCount)
	}
	if stats.TotalSize != 11 { // "hello world" = 11 bytes
		t.Errorf("Total size should be 11, got %d", stats.TotalSize)
	}
	if stats.UsagePercent <= 0 {
		t.Errorf("Usage percent should be > 0, got %f", stats.UsagePercent)
	}
}

func TestIndexPersistence(t *testing.T) {
	tmpDir := t.TempDir()

	// Create cache and store template
	cache1, err := New(tmpDir, DefaultMaxSize)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	files := map[string][]byte{"file.txt": []byte("test data")}
	_, err = cache1.Store("template-persist", "hash-persist", files)
	if err != nil {
		t.Fatalf("Store() error = %v", err)
	}

	// Create new cache instance pointing to same directory
	cache2, err := New(tmpDir, DefaultMaxSize)
	if err != nil {
		t.Fatalf("New() second instance error = %v", err)
	}

	// Should be able to find the previously stored template
	path, ok := cache2.Get("template-persist", "hash-persist")
	if !ok {
		t.Error("Index should persist across cache instances")
	}
	if path == "" {
		t.Error("Path should not be empty")
	}
}

func TestSharedContentHash(t *testing.T) {
	tmpDir := t.TempDir()
	cache, err := New(tmpDir, DefaultMaxSize)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	// Store template with a specific hash
	contentHash := "shared-content-hash"
	files := map[string][]byte{"file.txt": []byte("shared content")}

	path1, err := cache.Store("template-a", contentHash, files)
	if err != nil {
		t.Fatalf("Store() error = %v", err)
	}

	// Store another template ID with same content hash
	path2, err := cache.Store("template-b", contentHash, files)
	if err != nil {
		t.Fatalf("Store() error = %v", err)
	}

	// Both should point to the same path
	if path1 != path2 {
		t.Errorf("Templates with same hash should share storage: %s != %s", path1, path2)
	}

	// Both template IDs should work
	gotPath1, ok := cache.Get("template-a", contentHash)
	if !ok {
		t.Error("Get() for template-a should succeed")
	}
	gotPath2, ok := cache.Get("template-b", contentHash)
	if !ok {
		t.Error("Get() for template-b should succeed")
	}
	if gotPath1 != gotPath2 {
		t.Errorf("Both templates should return same path")
	}
}
