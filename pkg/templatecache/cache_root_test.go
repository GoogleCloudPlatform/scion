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
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
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

var nonSingleElementHashes = []string{
	"../sibling",
	"../sibling/entry",
	"a/b",
	"..",
	"",
	".",
	`a\b`,
	"a\x00b",
	"/abs",
}

func TestPut_ContentHashMustBeSingleElement(t *testing.T) {
	for _, hash := range nonSingleElementHashes {
		t.Run(hash, func(t *testing.T) {
			cache, cacheDir, sibling := newCacheWithSibling(t)
			_, err := cache.Put(hash, map[string][]byte{"file.txt": []byte("x")})
			if err == nil {
				t.Fatal("expected an error for a contentHash that is not a single path element")
			}
			if !strings.Contains(err.Error(), "invalid contentHash") {
				t.Errorf("error %q should name the contentHash field", err)
			}
			if hash != "" && strings.Contains(err.Error(), hash) {
				t.Errorf("error %q should not include the contentHash value", err)
			}
			assertDirEmpty(t, sibling)
			assertNoEntryDirs(t, cacheDir)
			if n := len(cache.index.Entries); n != 0 {
				t.Errorf("index has %d entries, want 0", n)
			}
		})
	}
}

func TestPut_ExistingDirOutsideCacheIsNotIndexed(t *testing.T) {
	cache, cacheDir, sibling := newCacheWithSibling(t)
	marker := filepath.Join(sibling, "keep.txt")
	if err := os.WriteFile(marker, []byte("keep"), 0644); err != nil {
		t.Fatal(err)
	}

	if _, err := cache.Put("../sibling", map[string][]byte{"file.txt": []byte("x")}); err == nil {
		t.Fatal("expected an error")
	}
	if n := len(cache.index.Entries); n != 0 {
		t.Fatalf("index has %d entries, want 0", n)
	}
	cache.Invalidate("../sibling")
	if err := cache.Clear(); err != nil {
		t.Fatalf("Clear() error = %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("directory outside the cache should be untouched: %v", err)
	}
	assertNoEntryDirs(t, cacheDir)
}

func TestGet_ContentHashMustBeSingleElement(t *testing.T) {
	cache, _, sibling := newCacheWithSibling(t)
	if err := os.WriteFile(filepath.Join(sibling, "file.txt"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, hash := range nonSingleElementHashes {
		// Simulate an index entry loaded from disk for this key.
		cache.index.Entries[hash] = &CacheEntry{}
		if path, ok := cache.Get(hash); ok {
			t.Errorf("Get(%q) = %q, true; want a miss", hash, path)
		}
	}
}

// linkEntryToSibling creates a symlink named name in cacheDir that points to
// the sibling directory.
func linkEntryToSibling(t *testing.T, cacheDir, sibling, name string) {
	t.Helper()
	if err := os.Symlink(sibling, filepath.Join(cacheDir, name)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
}

func TestPut_EntryDirStaysInsideCacheDirThroughSymlink(t *testing.T) {
	cache, cacheDir, sibling := newCacheWithSibling(t)
	linkEntryToSibling(t, cacheDir, sibling, "hash1")

	path, err := cache.Put("hash1", map[string][]byte{"file.txt": []byte("x")})
	if err == nil {
		t.Fatalf("Put() = %q; expected an error for an entry directory linked outside the cache", path)
	}
	if n := len(cache.index.Entries); n != 0 {
		t.Errorf("index has %d entries, want 0", n)
	}
	assertDirEmpty(t, sibling)
}

func TestPut_TempDirStaysInsideCacheDirThroughSymlink(t *testing.T) {
	cache, cacheDir, sibling := newCacheWithSibling(t)
	linkEntryToSibling(t, cacheDir, sibling, "hash1.tmp")

	if _, err := cache.Put("hash1", map[string][]byte{"file.txt": []byte("x")}); err == nil {
		t.Fatal("expected an error for a temporary directory linked outside the cache")
	}
	assertDirEmpty(t, sibling)
	assertNoEntryDirs(t, cacheDir)
}

func TestGet_EntryDirStaysInsideCacheDirThroughSymlink(t *testing.T) {
	cache, cacheDir, sibling := newCacheWithSibling(t)
	linkEntryToSibling(t, cacheDir, sibling, "hash1")
	cache.index.Entries["hash1"] = &CacheEntry{}

	if path, ok := cache.Get("hash1"); ok {
		t.Errorf("Get() = %q, true; want a miss for an entry directory linked outside the cache", path)
	}
}

func TestPut_CanonicalContentHashRoundTrips(t *testing.T) {
	cache, _, sibling := newCacheWithSibling(t)
	files := map[string][]byte{"a/b.txt": []byte("b"), "c.txt": []byte("c")}
	hash := transfer.ComputeContentHash([]transfer.FileInfo{
		{Path: "a/b.txt", Hash: transfer.HashBytes(files["a/b.txt"])},
		{Path: "c.txt", Hash: transfer.HashBytes(files["c.txt"])},
	})
	if !strings.HasPrefix(hash, "sha256:") {
		t.Fatalf("unexpected hash format %q", hash)
	}

	storedPath, err := cache.Put(hash, files)
	if err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	gotPath, ok := cache.Get(hash)
	if !ok || gotPath != storedPath {
		t.Fatalf("Get() = %q, %v; want %q, true", gotPath, ok, storedPath)
	}
	got, err := os.ReadFile(filepath.Join(gotPath, "a", "b.txt"))
	if err != nil || string(got) != "b" {
		t.Errorf("a/b.txt = %q, %v", got, err)
	}

	cache.Invalidate(hash)
	if _, ok := cache.Get(hash); ok {
		t.Error("Get() after Invalidate() should miss")
	}
	if _, err := os.Stat(storedPath); !os.IsNotExist(err) {
		t.Errorf("entry directory should be removed, stat err = %v", err)
	}
	assertDirEmpty(t, sibling)
}

func TestEviction_StaysInsideCacheDir(t *testing.T) {
	base := t.TempDir()
	cacheDir := filepath.Join(base, "cache")
	sibling := filepath.Join(base, "sibling")
	if err := os.MkdirAll(sibling, 0755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(sibling, "keep.txt")
	if err := os.WriteFile(marker, []byte("keep"), 0644); err != nil {
		t.Fatal(err)
	}
	cache, err := New(cacheDir, 10)
	if err != nil {
		t.Fatal(err)
	}
	// An index entry loaded from disk whose key is not a single element.
	cache.index.Entries["../sibling"] = &CacheEntry{Size: 10}
	cache.index.TotalSize = 10

	if _, err := cache.Put("hash1", map[string][]byte{"file.txt": []byte("12345")}); err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	if _, ok := cache.index.Entries["../sibling"]; ok {
		t.Error("evicted index entry should be dropped")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("directory outside the cache should be untouched: %v", err)
	}
}

func TestInvalidate_StaysInsideCacheDir(t *testing.T) {
	cache, _, sibling := newCacheWithSibling(t)
	marker := filepath.Join(sibling, "keep.txt")
	if err := os.WriteFile(marker, []byte("keep"), 0644); err != nil {
		t.Fatal(err)
	}
	// An index entry loaded from disk whose key is not a single element.
	cache.index.Entries["../sibling"] = &CacheEntry{}

	cache.Invalidate("../sibling")
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("directory outside the cache should be untouched: %v", err)
	}
}
