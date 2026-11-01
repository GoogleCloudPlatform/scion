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

package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A cache directory that cannot be removed before a refill fails the fetch:
// nothing is fetched into it (the old content is left as it is), and the
// error says why.
func TestFetchRemoteTemplate_CacheCleanupFailureFailsTheFetch(t *testing.T) {
	// Neither a GitHub folder nor an archive: a fetch past the cleanup would
	// fail at once as an unsupported type, with no network access.
	const uri = "https://example.invalid/scion/template"

	t.Run("planted RemoveAll error", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		orig := removeAllCache
		removeAllCache = func(string) error { return errors.New("planted removal failure") }
		t.Cleanup(func() { removeAllCache = orig })
		_, err := FetchRemoteTemplate(context.Background(), uri)
		if err == nil || !strings.Contains(err.Error(), "failed to remove the old cache directory") || !strings.Contains(err.Error(), "planted removal failure") {
			t.Fatalf("err = %v, want the cache cleanup failure", err)
		}
	})

	t.Run("directory that cannot be removed", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root removes read-only directories")
		}
		home := t.TempDir()
		t.Setenv("HOME", home)
		cacheDir, err := remoteCacheDir()
		if err != nil {
			t.Fatal(err)
		}
		locked := filepath.Join(cacheDir, generateCacheKey(uri), "locked")
		if err := os.MkdirAll(locked, 0o755); err != nil {
			t.Fatal(err)
		}
		stale := filepath.Join(locked, "stale.txt")
		if err := os.WriteFile(stale, []byte("old\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(locked, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
		_, err = FetchRemoteTemplate(context.Background(), uri)
		if err == nil || !strings.Contains(err.Error(), "failed to remove the old cache directory") {
			t.Fatalf("err = %v, want the cache cleanup failure", err)
		}
		if got, err := os.ReadFile(stale); err != nil || string(got) != "old\n" {
			t.Errorf("stale content changed (%q, %v)", got, err)
		}
	})
}
