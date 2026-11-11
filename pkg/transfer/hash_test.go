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

func TestComputeContentHash_OrderIndependent(t *testing.T) {
	files := []FileInfo{
		{Path: "b.txt", Hash: "sha256:bbb", Mode: "0755"},
		{Path: "a.txt", Hash: "sha256:aaa"},
		{Path: "c.txt", Hash: "sha256:ccc"},
	}
	sortedFiles := []FileInfo{
		{Path: "a.txt", Hash: "sha256:aaa"},
		{Path: "b.txt", Hash: "sha256:bbb", Mode: "0755"},
		{Path: "c.txt", Hash: "sha256:ccc"},
	}
	for name, fn := range map[string]func([]FileInfo) string{"v1": ComputeContentHashV1, "v2": ComputeContentHashV2} {
		hash := fn(files)
		if !IsContentHash(hash) {
			t.Errorf("%s: hash %q is not in content-hash form", name, hash)
		}
		if again := fn(sortedFiles); hash != again {
			t.Errorf("%s: hashes should be equal regardless of input order: %s != %s", name, hash, again)
		}
		if again := fn(files); hash != again {
			t.Errorf("%s: content hash should be deterministic: %s != %s", name, hash, again)
		}
	}
}

func TestComputeContentHash_Empty(t *testing.T) {
	for name, fn := range map[string]func([]FileInfo) string{"v1": ComputeContentHashV1, "v2": ComputeContentHashV2} {
		if hash := fn([]FileInfo{}); hash != "" {
			t.Errorf("%s: expected empty hash for empty file list, got %s", name, hash)
		}
		if hash := fn(nil); hash != "" {
			t.Errorf("%s: expected empty hash for nil file list, got %s", name, hash)
		}
	}
}

// TestComputeContentHashV1_Golden pins the v1 value: skill versions are
// pinned by it, so it must never change.
func TestComputeContentHashV1_Golden(t *testing.T) {
	files := []FileInfo{
		{Path: "b.txt", Hash: "sha256:bbb", Mode: "0755"},
		{Path: "a.txt", Hash: "sha256:aaa"},
	}
	// sha256("sha256:aaasha256:bbb")
	want := HashBytes([]byte("sha256:aaasha256:bbb"))
	if got := ComputeContentHashV1(files); got != want {
		t.Errorf("v1 = %s, want %s", got, want)
	}
	// Paths and modes are not part of v1.
	renamed := []FileInfo{
		{Path: "z.txt", Hash: "sha256:bbb"},
		{Path: "a.txt", Hash: "sha256:aaa"},
	}
	if got := ComputeContentHashV1(renamed); got != want {
		t.Errorf("v1 of a renamed, chmodded set = %s, want unchanged %s", got, want)
	}
}

// TestComputeContentHashV2_Golden pins the v2 preimage format.
func TestComputeContentHashV2_Golden(t *testing.T) {
	files := []FileInfo{
		{Path: "bin/run", Hash: "sha256:bbb", Mode: "0700"},
		{Path: "a.txt", Hash: "sha256:aaa"},
	}
	want := HashBytes([]byte("a.txt\x000644\x00sha256:aaa\nbin/run\x000755\x00sha256:bbb\n"))
	if got := ComputeContentHashV2(files); got != want {
		t.Errorf("v2 = %s, want %s", got, want)
	}
}

// TestComputeContentHashV2_CoversPathsAndModes covers ptone/scion#5658: a
// rename-only or chmod-only change changes the v2 hash; a mode change that
// normalises to the same mode does not.
func TestComputeContentHashV2_CoversPathsAndModes(t *testing.T) {
	base := []FileInfo{
		{Path: "a.txt", Hash: "sha256:aaa", Mode: "0644"},
		{Path: "run.sh", Hash: "sha256:bbb", Mode: "0644"},
	}
	h := ComputeContentHashV2(base)

	cases := []struct {
		name    string
		files   []FileInfo
		changed bool
	}{
		{"rename only", []FileInfo{
			{Path: "a.txt", Hash: "sha256:aaa", Mode: "0644"},
			{Path: "bin/run.sh", Hash: "sha256:bbb", Mode: "0644"},
		}, true},
		{"swap contents between paths", []FileInfo{
			{Path: "a.txt", Hash: "sha256:bbb", Mode: "0644"},
			{Path: "run.sh", Hash: "sha256:aaa", Mode: "0644"},
		}, true},
		{"chmod +x only", []FileInfo{
			{Path: "a.txt", Hash: "sha256:aaa", Mode: "0644"},
			{Path: "run.sh", Hash: "sha256:bbb", Mode: "0755"},
		}, true},
		{"content change", []FileInfo{
			{Path: "a.txt", Hash: "sha256:aaa", Mode: "0644"},
			{Path: "run.sh", Hash: "sha256:ccc", Mode: "0644"},
		}, true},
		{"empty mode is 0644", []FileInfo{
			{Path: "a.txt", Hash: "sha256:aaa"},
			{Path: "run.sh", Hash: "sha256:bbb"},
		}, false},
		{"non-exec bits normalise away", []FileInfo{
			{Path: "a.txt", Hash: "sha256:aaa", Mode: "0600"},
			{Path: "run.sh", Hash: "sha256:bbb", Mode: "0666"},
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ComputeContentHashV2(tc.files)
			if (got != h) != tc.changed {
				t.Errorf("v2 changed = %v, want %v (%s vs %s)", got != h, tc.changed, got, h)
			}
		})
	}

	// v1 and v2 never coincide for the same files.
	if ComputeContentHashV1(base) == h {
		t.Error("v1 and v2 hashes are equal")
	}
}

// TestGitBlobHashBytes checks the implementation against object IDs produced
// by git itself (`git hash-object`), which is the authority for this format.
func TestGitBlobHashBytes(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		// printf '' | git hash-object --stdin
		{"empty", "", "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391"},
		// printf 'hello\n' | git hash-object --stdin
		{"hello newline", "hello\n", "ce013625030ba8dba906f756967f9e9ca394464a"},
		// printf 'hello world' | git hash-object --stdin
		{"hello world", "hello world", "95d09f2b10159347eece71399a7e2e907ea3df4f"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := GitBlobHashBytes([]byte(tc.content)); got != tc.want {
				t.Errorf("GitBlobHashBytes(%q) = %s, want %s", tc.content, got, tc.want)
			}
		})
	}
}

func TestGitBlobHashFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blob.txt")
	if err := os.WriteFile(path, []byte("hello\n"), 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	got, err := GitBlobHashFile(path)
	if err != nil {
		t.Fatalf("GitBlobHashFile: %v", err)
	}
	const want = "ce013625030ba8dba906f756967f9e9ca394464a"
	if got != want {
		t.Errorf("GitBlobHashFile = %s, want %s", got, want)
	}

	// Must agree with the in-memory variant.
	if mem := GitBlobHashBytes([]byte("hello\n")); mem != got {
		t.Errorf("GitBlobHashFile (%s) disagrees with GitBlobHashBytes (%s)", got, mem)
	}
}

func TestGitBlobHashFile_Missing(t *testing.T) {
	if _, err := GitBlobHashFile(filepath.Join(t.TempDir(), "nope.txt")); err == nil {
		t.Error("expected an error for a missing file")
	}
}

func TestIsGitBlobHash(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"ce013625030ba8dba906f756967f9e9ca394464a", true},
		{"", false},
		// A sha256 digest must not be mistaken for a git object ID.
		{"sha256:ce013625030ba8dba906f756967f9e9ca394464a", false},
		{HashBytes([]byte("hello")), false},
		// Wrong length.
		{"ce013625030ba8dba906f756967f9e9ca394464", false},
		{"ce013625030ba8dba906f756967f9e9ca394464ab", false},
		// Uppercase hex is not the canonical git form.
		{"CE013625030BA8DBA906F756967F9E9CA394464A", false},
		// Non-hex.
		{"ze013625030ba8dba906f756967f9e9ca394464a", false},
	}

	for _, tc := range cases {
		if got := IsGitBlobHash(tc.in); got != tc.want {
			t.Errorf("IsGitBlobHash(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestIsContentHash(t *testing.T) {
	valid := HashBytes([]byte("x"))
	tests := []struct {
		in   string
		want bool
	}{
		{valid, true},
		{"sha256:" + strings.Repeat("a", 64), true},
		{"", false},
		{strings.Repeat("a", 64), false},
		{"sha256:" + strings.Repeat("a", 63), false},
		{"sha256:" + strings.Repeat("a", 65), false},
		{"sha256:" + strings.Repeat("A", 64), false},
		{"sha256:" + strings.Repeat("g", 64), false},
		{"web-dev", false},
		{"/cache/" + valid, false},
	}
	for _, tt := range tests {
		if got := IsContentHash(tt.in); got != tt.want {
			t.Errorf("IsContentHash(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}
