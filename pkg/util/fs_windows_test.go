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

//go:build windows

package util

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/internal/testutil/wintest"
)

// assertOnlyEntries fails unless dir holds exactly the named entries (no
// leftover rename-over temp files).
func assertOnlyEntries(t *testing.T, dir string, want ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, e := range entries {
		got[e.Name()] = true
	}
	if len(got) != len(want) {
		t.Fatalf("%s holds %v, want exactly %v", dir, got, want)
	}
	for _, name := range want {
		if !got[name] {
			t.Fatalf("%s holds %v, want exactly %v", dir, got, want)
		}
	}
}

// TestWindowsRemoveSymlinkSafe_FailsClosed checks that without unlinkat the
// removal refuses with ErrUnsupported and leaves the entry in place, instead
// of falling back to rename-over or a path-based os.Remove.
func TestWindowsRemoveSymlinkSafe_FailsClosed(t *testing.T) {
	outside := t.TempDir()
	target := filepath.Join(outside, "target.txt")
	if err := os.WriteFile(target, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	link := filepath.Join(dir, "link")
	wintest.MustSymlink(t, target, link)
	file := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, p := range []string{link, file} {
		if err := removeSymlinkSafe(p); !errors.Is(err, errors.ErrUnsupported) {
			t.Fatalf("removeSymlinkSafe(%q) = %v, want ErrUnsupported", p, err)
		}
		if _, err := os.Lstat(p); err != nil {
			t.Fatalf("%s was touched: %v", p, err)
		}
	}
	assertOnlyEntries(t, dir, "link", "file.txt")
	if got, err := os.ReadFile(target); err != nil || string(got) != "data" {
		t.Fatalf("link target changed: %q, %v", got, err)
	}
}

// TestWindowsRemoveAllSafe_SymlinkFailsClosed checks that RemoveAllSafe
// reports the refused symlink removal instead of succeeding, and that the
// link and its target outside the tree are left in place.
func TestWindowsRemoveAllSafe_SymlinkFailsClosed(t *testing.T) {
	outside := t.TempDir()
	target := filepath.Join(outside, "target.txt")
	if err := os.WriteFile(target, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "agent-dir")
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(sub, "link")
	wintest.MustSymlink(t, target, link)

	err := RemoveAllSafe(root)
	if !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("RemoveAllSafe = %v, want ErrUnsupported", err)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("symlink was removed: %v", err)
	}
	assertOnlyEntries(t, sub, "link")
	if got, err := os.ReadFile(target); err != nil || string(got) != "data" {
		t.Fatalf("link target changed: %q, %v", got, err)
	}
}

// TestWindowsRemoveAllSafe_NoSymlinkRemoves checks that a tree without
// symlinks is still removed, so the refusal above is specific to symlinks.
func TestWindowsRemoveAllSafe_NoSymlinkRemoves(t *testing.T) {
	root := filepath.Join(t.TempDir(), "agent-dir")
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "f.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RemoveAllSafe(root); err != nil {
		t.Fatalf("RemoveAllSafe = %v", err)
	}
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Fatalf("root still exists: %v", err)
	}
}
