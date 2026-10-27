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

package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/internal/testutil/wintest"
)

// Windows site tests for the GitHub resolution cache. They
// run on the windows-latest job through the anchored
// "./pkg/agent -run=^TestWindowsResolutionCache" line.

func usersInheritParent(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	wintest.Icacls(t, dir, "/grant", "*"+wintest.SIDUsers+":(OI)(CI)R")
	return dir
}

// A new cache directory (and missing parents) and the cache file are
// created private even under a parent handing out an inheritable Users ACE.
func TestWindowsResolutionCache_NewDirAndFilePrivate(t *testing.T) {
	parent := usersInheritParent(t)
	dir := filepath.Join(parent, "cache", "github-resolution")
	cache, err := newTestResolutionCache(dir, time.Hour)
	if err != nil {
		t.Fatalf("NewGitHubResolutionCache: %v", err)
	}
	cache.putEntry("gh://o/r/s@main", ResolvedSkill{Name: "s"}, true)
	cache.Flush()
	wintest.AssertPrivateSD(t, filepath.Join(parent, "cache"))
	wintest.AssertPrivateSD(t, dir)
	wintest.AssertPrivateSD(t, filepath.Join(dir, resolutionCacheFileName))
	if f := readCacheFile(t, dir); len(f.Entries) != 1 {
		t.Fatalf("file has %d entries, want 1", len(f.Entries))
	}
}

// A too-open cache file owned by the user is repaired when the cache loads
// it, before any rewrite (the verify-or-repair rule). Control: the file
// carried the Users ACE before the load.
func TestWindowsResolutionCache_RepairsOwnedTooOpenFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, resolutionCacheFileName)
	if err := os.WriteFile(path, []byte(`{"entries":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	wintest.MakeUsersReadable(t, path)
	if _, err := newTestResolutionCache(dir, time.Hour); err != nil {
		t.Fatalf("NewGitHubResolutionCache: %v", err)
	}
	wintest.AssertPrivateSD(t, path)
}

// A too-open cache file owned by someone else (SYSTEM) is reported and
// left as it is. Control: the same check on a file the user owns passes
// after repairing it.
func TestWindowsResolutionCache_ForeignOwnedFileWarnedNotRepaired(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, resolutionCacheFileName)
	if err := os.WriteFile(path, []byte(`{"entries":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	wintest.MakeUsersReadable(t, path)
	wintest.SetOwner(t, path, wintest.SIDSystem)
	err := tightenCacheFile(path)
	if err == nil || !strings.Contains(err.Error(), wintest.SIDSystem) {
		t.Fatalf("tightenCacheFile(SYSTEM-owned) = %v; want a refusal naming the owner", err)
	}
	if sd := wintest.ReadSD(t, path); sd.Owner != wintest.SIDSystem || !sd.HasAllowACE(wintest.SIDUsers) {
		t.Fatalf("the foreign-owned file was changed: %+v", sd)
	}

	mine := filepath.Join(t.TempDir(), resolutionCacheFileName)
	if err := os.WriteFile(mine, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	wintest.MakeUsersReadable(t, mine)
	if err := tightenCacheFile(mine); err != nil {
		t.Fatalf("control: tightenCacheFile(owned) = %v", err)
	}
	wintest.AssertPrivateSD(t, mine)
	if err := tightenCacheFile(filepath.Join(t.TempDir(), "absent")); err != nil {
		t.Fatalf("tightenCacheFile(missing) = %v, want nil", err)
	}
}

// A too-open existing cache directory is reported and never repaired
// (existing directories are not silently re-secured); the cache
// still works. Control: a private directory passes the check.
func TestWindowsResolutionCache_TooOpenDirWarnedNotRepaired(t *testing.T) {
	dir := filepath.Join(usersInheritParent(t), "cache")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if !wintest.ReadSD(t, dir).HasAllowACE(wintest.SIDUsers) {
		t.Fatal("setup: the directory did not inherit the Users ACE")
	}
	if err := tightenCacheDir(dir); err == nil || !strings.Contains(err.Error(), "not private") {
		t.Fatalf("tightenCacheDir(too open) = %v; want a not-private report", err)
	}
	cache, err := newTestResolutionCache(dir, time.Hour)
	if err != nil {
		t.Fatalf("NewGitHubResolutionCache on a too-open dir: %v", err)
	}
	cache.putEntry("gh://o/r/s@main", ResolvedSkill{Name: "s"}, true)
	cache.Flush()
	if !wintest.ReadSD(t, dir).HasAllowACE(wintest.SIDUsers) {
		t.Error("the existing directory was repaired; it must be left as it is")
	}
	wintest.AssertPrivateSD(t, filepath.Join(dir, resolutionCacheFileName))

	private := filepath.Join(t.TempDir(), "private")
	if _, err := newTestResolutionCache(private, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := tightenCacheDir(private); err != nil {
		t.Fatalf("control: tightenCacheDir(private) = %v", err)
	}
}
