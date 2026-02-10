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

func TestHashFile(t *testing.T) {
	// Create a temporary file
	dir := t.TempDir()
	testFile := filepath.Join(dir, "test.txt")
	content := []byte("hello world")
	if err := os.WriteFile(testFile, content, 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	hash, err := HashFile(testFile)
	if err != nil {
		t.Fatalf("HashFile failed: %v", err)
	}

	// Verify hash format
	if !strings.HasPrefix(hash, HashPrefix) {
		t.Errorf("hash should start with %q, got %s", HashPrefix, hash)
	}

	// "hello world" has a known SHA-256 hash
	expectedHash := "sha256:b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9"
	if hash != expectedHash {
		t.Errorf("unexpected hash: got %s, want %s", hash, expectedHash)
	}
}

func TestHashFile_NotFound(t *testing.T) {
	_, err := HashFile("/nonexistent/file.txt")
	if err == nil {
		t.Error("expected error for nonexistent file")
	}
}

func TestHashBytes(t *testing.T) {
	content := []byte("hello world")
	hash := HashBytes(content)

	expectedHash := "sha256:b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9"
	if hash != expectedHash {
		t.Errorf("unexpected hash: got %s, want %s", hash, expectedHash)
	}
}

func TestHashBytes_Empty(t *testing.T) {
	hash := HashBytes([]byte{})

	// Empty content has a known SHA-256 hash
	expectedHash := "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if hash != expectedHash {
		t.Errorf("unexpected hash for empty content: got %s, want %s", hash, expectedHash)
	}
}

func TestComputeContentHash(t *testing.T) {
	files := []FileInfo{
		{Path: "b.txt", Hash: "sha256:bbb"},
		{Path: "a.txt", Hash: "sha256:aaa"},
		{Path: "c.txt", Hash: "sha256:ccc"},
	}

	hash := ComputeContentHash(files)

	// Verify hash format
	if !strings.HasPrefix(hash, HashPrefix) {
		t.Errorf("hash should start with %q, got %s", HashPrefix, hash)
	}

	// Compute same hash with files already sorted
	sortedFiles := []FileInfo{
		{Path: "a.txt", Hash: "sha256:aaa"},
		{Path: "b.txt", Hash: "sha256:bbb"},
		{Path: "c.txt", Hash: "sha256:ccc"},
	}
	sortedHash := ComputeContentHash(sortedFiles)

	// Both should produce the same hash
	if hash != sortedHash {
		t.Errorf("hashes should be equal regardless of input order: %s != %s", hash, sortedHash)
	}
}

func TestComputeContentHash_Empty(t *testing.T) {
	hash := ComputeContentHash([]FileInfo{})
	if hash != "" {
		t.Errorf("expected empty hash for empty file list, got %s", hash)
	}
}

func TestComputeContentHash_Deterministic(t *testing.T) {
	files := []FileInfo{
		{Path: "file1.txt", Hash: "sha256:abc"},
		{Path: "file2.txt", Hash: "sha256:def"},
	}

	hash1 := ComputeContentHash(files)
	hash2 := ComputeContentHash(files)

	if hash1 != hash2 {
		t.Errorf("content hash should be deterministic: %s != %s", hash1, hash2)
	}
}
