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

package agentcache

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGenerateCacheKey(t *testing.T) {
	tests := []struct {
		name      string
		grovePath string
	}{
		{"simple path", "/home/user/project/.scion"},
		{"path with spaces", "/home/user/my project/.scion"},
		{"global grove", "/home/user/.scion"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key := GenerateCacheKey(tt.grovePath)

			// Key should not be empty
			if key == "" {
				t.Error("GenerateCacheKey returned empty string")
			}

			// Key should be consistent for same input
			key2 := GenerateCacheKey(tt.grovePath)
			if key != key2 {
				t.Errorf("GenerateCacheKey not consistent: got %q then %q", key, key2)
			}

			// Key should be a valid hex string (16 chars for 8 bytes)
			if len(key) != 16 {
				t.Errorf("expected key length 16, got %d", len(key))
			}
		})
	}
}

func TestGenerateCacheKey_Uniqueness(t *testing.T) {
	path1 := "/home/user/project1/.scion"
	path2 := "/home/user/project2/.scion"

	key1 := GenerateCacheKey(path1)
	key2 := GenerateCacheKey(path2)

	if key1 == key2 {
		t.Errorf("different paths produced same key: %q", key1)
	}
}

func TestReadWriteCache(t *testing.T) {
	// Use a temporary directory for testing
	tmpDir := t.TempDir()
	oldHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", oldHome)

	cacheKey := "test-key-123456"
	agents := []string{"agent-1", "agent-2", "agent-3"}

	// Write to cache
	err := WriteCache(cacheKey, agents)
	if err != nil {
		t.Fatalf("WriteCache failed: %v", err)
	}

	// Verify cache file exists
	cachePath := filepath.Join(tmpDir, ".scion", "cache", "agent-names", cacheKey+".json")
	if _, err := os.Stat(cachePath); os.IsNotExist(err) {
		t.Fatalf("cache file not created at %s", cachePath)
	}

	// Read from cache
	result, err := ReadCache(cacheKey)
	if err != nil {
		t.Fatalf("ReadCache failed: %v", err)
	}

	// Verify result
	if len(result) != len(agents) {
		t.Errorf("expected %d agents, got %d", len(agents), len(result))
	}

	for i, name := range agents {
		if result[i] != name {
			t.Errorf("agent[%d] = %q, expected %q", i, result[i], name)
		}
	}
}

func TestReadCache_NotExists(t *testing.T) {
	tmpDir := t.TempDir()
	oldHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", oldHome)

	// Reading non-existent cache should return nil, nil
	result, err := ReadCache("nonexistent-key")
	if err != nil {
		t.Errorf("ReadCache for nonexistent key returned error: %v", err)
	}
	if result != nil {
		t.Errorf("ReadCache for nonexistent key returned non-nil result: %v", result)
	}
}

func TestReadCache_CorruptedFile(t *testing.T) {
	tmpDir := t.TempDir()
	oldHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", oldHome)

	cacheKey := "corrupt-test"
	cacheDir := filepath.Join(tmpDir, ".scion", "cache", "agent-names")
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		t.Fatalf("failed to create cache dir: %v", err)
	}

	// Write corrupted JSON
	cachePath := filepath.Join(cacheDir, cacheKey+".json")
	if err := os.WriteFile(cachePath, []byte("not valid json"), 0644); err != nil {
		t.Fatalf("failed to write corrupted file: %v", err)
	}

	// Reading corrupted cache should return nil, nil (treat as cache miss)
	result, err := ReadCache(cacheKey)
	if err != nil {
		t.Errorf("ReadCache for corrupted file returned error: %v", err)
	}
	if result != nil {
		t.Errorf("ReadCache for corrupted file returned non-nil result: %v", result)
	}
}

func TestWriteCache_EmptyList(t *testing.T) {
	tmpDir := t.TempDir()
	oldHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", oldHome)

	cacheKey := "empty-test"

	// Write empty list
	err := WriteCache(cacheKey, []string{})
	if err != nil {
		t.Fatalf("WriteCache with empty list failed: %v", err)
	}

	// Read back
	result, err := ReadCache(cacheKey)
	if err != nil {
		t.Fatalf("ReadCache failed: %v", err)
	}

	if len(result) != 0 {
		t.Errorf("expected empty list, got %v", result)
	}
}

func TestClearCache(t *testing.T) {
	tmpDir := t.TempDir()
	oldHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", oldHome)

	// Write some cache entries
	_ = WriteCache("key1", []string{"agent1"})
	_ = WriteCache("key2", []string{"agent2"})

	// Clear cache
	err := ClearCache()
	if err != nil {
		t.Fatalf("ClearCache failed: %v", err)
	}

	// Verify cache is cleared
	result1, _ := ReadCache("key1")
	result2, _ := ReadCache("key2")

	if result1 != nil || result2 != nil {
		t.Error("cache not cleared properly")
	}
}
