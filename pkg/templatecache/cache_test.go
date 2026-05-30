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

func TestPutAndGet(t *testing.T) {
	tmpDir := t.TempDir()
	cache, err := New(tmpDir, DefaultMaxSize)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	contentHash := "abc123hash"
	files := map[string][]byte{
		"scion-agent.yaml":       []byte("harness: claude\n"),
		"home/.claude/CLAUDE.md": []byte("# Test Template\n"),
	}

	// Store template by content hash
	storedPath, err := cache.Put(contentHash, files)
	if err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	if storedPath == "" {
		t.Fatal("Put() returned empty path")
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

	// Get by hash
	gotPath, ok := cache.Get(contentHash)
	if !ok {
		t.Fatal("Get() returned false")
	}
	if gotPath != storedPath {
		t.Errorf("Get() path = %v, want %v", gotPath, storedPath)
	}

	// Get with unknown hash should fail
	_, ok = cache.Get("wrong-hash")
	if ok {
		t.Error("Get() with unknown hash should return false")
	}

	// Get with empty hash should fail
	_, ok = cache.Get("")
	if ok {
		t.Error("Get() with empty hash should return false")
	}
}

func TestPutIdempotentByHash(t *testing.T) {
	tmpDir := t.TempDir()
	cache, err := New(tmpDir, DefaultMaxSize)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	contentHash := "shared-content-hash"
	files := map[string][]byte{"file.txt": []byte("shared content")}

	// Storing the same hash twice yields the same path and a single entry.
	path1, err := cache.Put(contentHash, files)
	if err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	path2, err := cache.Put(contentHash, files)
	if err != nil {
		t.Fatalf("Put() second call error = %v", err)
	}
	if path1 != path2 {
		t.Errorf("Same content hash should share storage: %s != %s", path1, path2)
	}

	if stats := cache.Stats(); stats.EntryCount != 1 {
		t.Errorf("Expected 1 entry for repeated hash, got %d", stats.EntryCount)
	}
}

func TestGetMissingFilesDropsEntry(t *testing.T) {
	tmpDir := t.TempDir()
	cache, err := New(tmpDir, DefaultMaxSize)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	contentHash := "vanishing-hash"
	path, err := cache.Put(contentHash, map[string][]byte{"file.txt": []byte("data")})
	if err != nil {
		t.Fatalf("Put() error = %v", err)
	}

	// Simulate files disappearing out from under the cache.
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}

	if _, ok := cache.Get(contentHash); ok {
		t.Error("Get() should return false when files are missing")
	}
	if stats := cache.Stats(); stats.EntryCount != 0 || stats.TotalSize != 0 {
		t.Errorf("stale entry should be dropped, got count=%d size=%d", stats.EntryCount, stats.TotalSize)
	}
}

func TestEviction(t *testing.T) {
	tmpDir := t.TempDir()

	// Create cache with very small max size (1KB)
	cache, err := New(tmpDir, 1024)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	files := func() map[string][]byte {
		return map[string][]byte{"file.txt": make([]byte, 500)}
	}

	// Store first template (500 bytes)
	if _, err := cache.Put("hash-1", files()); err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	time.Sleep(10 * time.Millisecond)

	// Store second template (500 bytes)
	if _, err := cache.Put("hash-2", files()); err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	time.Sleep(10 * time.Millisecond)

	// Store third template (500 bytes) - should trigger eviction of oldest
	if _, err := cache.Put("hash-3", files()); err != nil {
		t.Fatalf("Put() error = %v", err)
	}

	// First template should be evicted (LRU)
	if _, ok := cache.Get("hash-1"); ok {
		t.Error("hash-1 should have been evicted")
	}

	// Third template should still exist
	if _, ok := cache.Get("hash-3"); !ok {
		t.Error("hash-3 should exist")
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
	if _, err := cache.Put("h1", files); err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	if _, err := cache.Put("h2", files); err != nil {
		t.Fatalf("Put() error = %v", err)
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
	if _, err := cache.Put("hash", files); err != nil {
		t.Fatalf("Put() error = %v", err)
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
	if _, err := cache1.Put("hash-persist", files); err != nil {
		t.Fatalf("Put() error = %v", err)
	}

	// Create new cache instance pointing to same directory
	cache2, err := New(tmpDir, DefaultMaxSize)
	if err != nil {
		t.Fatalf("New() second instance error = %v", err)
	}

	// Should be able to find the previously stored template
	path, ok := cache2.Get("hash-persist")
	if !ok {
		t.Error("Index should persist across cache instances")
	}
	if path == "" {
		t.Error("Path should not be empty")
	}
}
