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

//go:build unix

package agent

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestNewGitHubResolutionCache_DirModeFailureIsNotFatal checks that a cache
// directory whose mode cannot be changed (for example one owned by another
// user) still yields a working cache.
func TestNewGitHubResolutionCache_DirModeFailureIsNotFatal(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	orig := chmod
	chmod = func(string, os.FileMode) error { return errors.New("operation not permitted") }
	t.Cleanup(func() { chmod = orig })

	cache, err := newTestResolutionCache(dir, time.Hour)
	if err != nil {
		t.Fatalf("NewGitHubResolutionCache: %v", err)
	}
	cache.putEntry("gh://o/r/s@main", ResolvedSkill{Name: "s"}, true)
	cache.Flush()
	if f := readCacheFile(t, dir); len(f.Entries) != 1 {
		t.Fatalf("file has %d entries, want 1", len(f.Entries))
	}
}

// On unix, the cache save creates its temp file with privatefs
// Tree.CreateTemp(Private), which chmods it to 0600, so a umask stricter
// than 077 cannot leave the cache file unwritable by its owner. Control: under the default umask it is 0600.
func TestGitHubResolutionCache_SaveChmodsUnderStrictUmask(t *testing.T) {
	dir := t.TempDir()
	cache, err := newTestResolutionCache(dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, resolutionCacheFileName)

	cache.putEntry("gh://o/r/a@main", ResolvedSkill{Name: "a"}, true)
	old := syscall.Umask(0o277)
	cache.Flush()
	syscall.Umask(old)
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("cache file mode after a save under umask 0277 = %v, %v; want 0600", fi.Mode().Perm(), err)
	}

	cache.putEntry("gh://o/r/b@main", ResolvedSkill{Name: "b"}, true)
	cache.Flush()
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("control: cache file mode = %v, %v; want 0600", fi.Mode().Perm(), err)
	}
}
