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
)

// newCacheWithSibling returns a cache rooted in a fresh directory together
// with an empty sibling directory next to it.
func newCacheWithSibling(t *testing.T) (*Cache, string, string) {
	t.Helper()
	base := t.TempDir()
	cacheDir := filepath.Join(base, "cache")
	sibling := filepath.Join(base, "sibling")
	if err := os.MkdirAll(sibling, 0755); err != nil {
		t.Fatal(err)
	}
	cache, err := New(cacheDir, DefaultMaxSize)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return cache, cacheDir, sibling
}

func assertDirEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("%s should be empty, has %d entries", dir, len(entries))
	}
}

// assertNoEntryDirs checks that the cache directory holds no entry or
// temporary directories.
func assertNoEntryDirs(t *testing.T, cacheDir string) {
	t.Helper()
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			t.Errorf("unexpected directory %s in cache", e.Name())
		}
	}
}

func TestPut_RejectsNonCanonicalKey(t *testing.T) {
	keys := []string{
		"../sibling/file.txt",
		"../../file.txt",
		"a/../../../sibling/file.txt",
		"./file.txt",
		"a//file.txt",
		`a\file.txt`,
		"file\x00.txt",
		"",
		".",
	}
	for _, key := range keys {
		t.Run(key, func(t *testing.T) {
			cache, cacheDir, sibling := newCacheWithSibling(t)
			files := map[string][]byte{
				"scion-agent.yaml": []byte("harness: claude\n"),
				key:                []byte("x"),
			}
			if _, err := cache.Put("hash1", files); err == nil {
				t.Fatal("expected an error for a non-canonical key")
			}
			assertDirEmpty(t, sibling)
			assertNoEntryDirs(t, cacheDir)
			if _, ok := cache.Get("hash1"); ok {
				t.Error("rejected content should not be cached")
			}
		})
	}

	t.Run("absolute", func(t *testing.T) {
		cache, cacheDir, sibling := newCacheWithSibling(t)
		files := map[string][]byte{filepath.Join(sibling, "file.txt"): []byte("x")}
		if _, err := cache.Put("hash1", files); err == nil {
			t.Fatal("expected an error for an absolute key")
		}
		assertDirEmpty(t, sibling)
		assertNoEntryDirs(t, cacheDir)
	})
}

func TestPut_StaysInsideEntryDirThroughSymlinkedDir(t *testing.T) {
	cache, cacheDir, sibling := newCacheWithSibling(t)

	// A leftover temporary directory containing a symlink to the sibling.
	tmpPath := filepath.Join(cacheDir, "hash1.tmp")
	if err := os.MkdirAll(tmpPath, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sibling, filepath.Join(tmpPath, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	files := map[string][]byte{"link/file.txt": []byte("x")}
	if _, err := cache.Put("hash1", files); err == nil {
		t.Fatal("expected an error writing through a symlink that leaves the entry directory")
	}
	assertDirEmpty(t, sibling)
	assertNoEntryDirs(t, cacheDir)
}

func TestPut_NestedKeys(t *testing.T) {
	cache, _, sibling := newCacheWithSibling(t)
	files := map[string][]byte{
		"a/b/c.txt":              []byte("nested"),
		"home/.claude/CLAUDE.md": []byte("# Test\n"),
		"scion-agent.yaml":       []byte("harness: claude\n"),
	}
	storedPath, err := cache.Put("hash1", files)
	if err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	for rel, want := range files {
		got, err := os.ReadFile(filepath.Join(storedPath, filepath.FromSlash(rel)))
		if err != nil || string(got) != string(want) {
			t.Errorf("%s = %q, %v; want %q", rel, got, err, want)
		}
	}
	assertDirEmpty(t, sibling)
}
