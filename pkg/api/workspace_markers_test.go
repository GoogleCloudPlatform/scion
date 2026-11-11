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

package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsWorkspaceMarker(t *testing.T) {
	for _, name := range []string{".scion", ".scion-volumes", ".agents"} {
		if !IsWorkspaceMarker(name) {
			t.Errorf("IsWorkspaceMarker(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"", ".git", ".scion-other", "scion", ".agents2", "README.md"} {
		if IsWorkspaceMarker(name) {
			t.Errorf("IsWorkspaceMarker(%q) = true, want false", name)
		}
	}
}

// caseInsensitiveLstat resolves the last path component the way a
// case-insensitive filesystem does: to the existing entry whose name
// equals it under case folding.
func caseInsensitiveLstat(p string) (os.FileInfo, error) {
	dir, base := filepath.Split(p)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if strings.EqualFold(e.Name(), base) {
			return os.Lstat(filepath.Join(dir, e.Name()))
		}
	}
	return nil, os.ErrNotExist
}

// caseSensitive reports whether dir's filesystem tells names apart by case.
func caseSensitive(t *testing.T, dir string) bool {
	t.Helper()
	probe := filepath.Join(dir, "case-probe")
	if err := os.Mkdir(probe, 0o755); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Remove(probe) }()
	_, err := os.Lstat(filepath.Join(dir, "CASE-PROBE"))
	return err != nil
}

// Entry kinds a test can create under a name.
const (
	kindDir      = "real dir"
	kindFile     = "regular file"
	kindSymlink  = "symlink to a dir"
	kindDangling = "dangling symlink"
)

// createEntry creates name in dir as the given kind. A symlink points at a
// separate real directory, "target", which it creates first.
func createEntry(t *testing.T, dir, name, kind string) {
	t.Helper()
	p := filepath.Join(dir, name)
	switch kind {
	case kindDir:
		if err := os.Mkdir(p, 0o755); err != nil {
			t.Fatal(err)
		}
	case kindFile:
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	case kindSymlink:
		if err := os.Mkdir(filepath.Join(dir, "target"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("target", p); err != nil {
			t.Skipf("cannot create symlink: %v", err)
		}
	case kindDangling:
		if err := os.Symlink("missing-target", p); err != nil {
			t.Skipf("cannot create symlink: %v", err)
		}
	default:
		t.Fatalf("unknown entry kind %q", kind)
	}
}

func TestIsWorkspaceMarkerEntry(t *testing.T) {
	tests := []struct {
		name    string
		create  string // the entry as it exists in the directory
		kind    string // what kind of entry create is
		entry   string // the name the caller read
		lstat   func(string) (os.FileInfo, error)
		fsLabel string // "case-sensitive" rows need a case-sensitive temp filesystem
		want    bool
	}{
		{name: "exact marker", create: ".scion", kind: kindDir, entry: ".scion", lstat: os.Lstat, fsLabel: "any", want: true},
		{name: "exact shared-dir mount point", create: ".agents", kind: kindDir, entry: ".agents", lstat: os.Lstat, fsLabel: "any", want: true},
		{name: "cased marker on a case-insensitive filesystem", create: ".SCION", kind: kindDir, entry: ".SCION", lstat: caseInsensitiveLstat, fsLabel: "case-insensitive", want: true},
		{name: "cased volumes marker on a case-insensitive filesystem", create: ".Scion-Volumes", kind: kindDir, entry: ".Scion-Volumes", lstat: caseInsensitiveLstat, fsLabel: "case-insensitive", want: true},
		{name: "cased name on a case-sensitive filesystem is a different entry", create: ".SCION", kind: kindDir, entry: ".SCION", lstat: os.Lstat, fsLabel: "case-sensitive", want: false},
		{name: "non-marker on a case-insensitive filesystem", create: ".scionx", kind: kindDir, entry: ".scionx", lstat: caseInsensitiveLstat, fsLabel: "case-insensitive", want: false},
		{name: "marker name without the dot", create: "Scion", kind: kindDir, entry: "Scion", lstat: caseInsensitiveLstat, fsLabel: "case-insensitive", want: false},
		{name: "unrelated entry", create: ".git", kind: kindDir, entry: ".git", lstat: os.Lstat, fsLabel: "any", want: false},

		// The entry kind decides (S1): only a real directory is a marker,
		// under the exact name and under another case alike.
		{name: "exact name, real dir", create: ".agents", kind: kindDir, entry: ".agents", lstat: os.Lstat, fsLabel: "any", want: true},
		{name: "exact name, regular file", create: ".agents", kind: kindFile, entry: ".agents", lstat: os.Lstat, fsLabel: "any", want: false},
		{name: "exact name, symlink to a dir", create: ".agents", kind: kindSymlink, entry: ".agents", lstat: os.Lstat, fsLabel: "any", want: false},
		{name: "exact name, dangling symlink", create: ".agents", kind: kindDangling, entry: ".agents", lstat: os.Lstat, fsLabel: "any", want: false},
		{name: "cased name, real dir", create: ".AGENTS", kind: kindDir, entry: ".AGENTS", lstat: caseInsensitiveLstat, fsLabel: "case-insensitive", want: true},
		{name: "cased name, regular file", create: ".AGENTS", kind: kindFile, entry: ".AGENTS", lstat: caseInsensitiveLstat, fsLabel: "case-insensitive", want: false},
		{name: "cased name, symlink to a dir", create: ".AGENTS", kind: kindSymlink, entry: ".AGENTS", lstat: caseInsensitiveLstat, fsLabel: "case-insensitive", want: false},
		{name: "cased name, dangling symlink", create: ".AGENTS", kind: kindDangling, entry: ".AGENTS", lstat: caseInsensitiveLstat, fsLabel: "case-insensitive", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if tt.fsLabel == "case-sensitive" && !caseSensitive(t, dir) {
				t.Skip("the temp filesystem is case-insensitive")
			}
			createEntry(t, dir, tt.create, tt.kind)
			if got := isWorkspaceMarkerEntry(dir, tt.entry, tt.lstat); got != tt.want {
				t.Errorf("isWorkspaceMarkerEntry(%q, %s) on a %s filesystem = %v, want %v", tt.entry, tt.kind, tt.fsLabel, got, tt.want)
			}
		})
	}
}

// An entry that cannot be Lstat'ed is not a marker.
func TestIsWorkspaceMarkerEntry_LstatErrorIsNotMarker(t *testing.T) {
	failing := func(string) (os.FileInfo, error) { return nil, os.ErrPermission }
	if isWorkspaceMarkerEntry(t.TempDir(), ".agents", failing) {
		t.Error(".agents with a failing Lstat: want not a marker")
	}
}

// On a case-sensitive filesystem, a cased name next to the real marker is
// still a different entry.
func TestIsWorkspaceMarkerEntry_CaseSensitiveBothPresent(t *testing.T) {
	dir := t.TempDir()
	if !caseSensitive(t, dir) {
		t.Skip("the temp filesystem is case-insensitive")
	}
	for _, n := range []string{".scion", ".SCION"} {
		if err := os.Mkdir(filepath.Join(dir, n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if !IsWorkspaceMarkerEntry(dir, ".scion") {
		t.Error(".scion: want marker")
	}
	if IsWorkspaceMarkerEntry(dir, ".SCION") {
		t.Error(".SCION next to .scion on a case-sensitive filesystem: want not a marker")
	}
}

// On a case-sensitive filesystem a symlink named like a marker under another
// case is its own entry (Lstat does not follow it), so it is not a marker,
// even when it points at the marker.
func TestIsWorkspaceMarkerEntry_CaseSensitiveSymlinkIsNotMarker(t *testing.T) {
	dir := t.TempDir()
	if !caseSensitive(t, dir) {
		t.Skip("the temp filesystem is case-insensitive")
	}
	if err := os.Mkdir(filepath.Join(dir, ".scion"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(".scion", filepath.Join(dir, ".SCION")); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	if IsWorkspaceMarkerEntry(dir, ".SCION") {
		t.Error(".SCION symlink to .scion on a case-sensitive filesystem: want not a marker")
	}
}

// On a case-insensitive filesystem a cased entry that is itself a symlink is
// NOT a marker: both names resolve to that one link entry, but a symlink is
// never a marker, whatever it points at.
func TestIsWorkspaceMarkerEntry_CaseInsensitiveSymlinkEntry(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "elsewhere"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("elsewhere", filepath.Join(dir, ".SCION")); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	if isWorkspaceMarkerEntry(dir, ".SCION", caseInsensitiveLstat) {
		t.Error(".SCION symlink entry on a case-insensitive filesystem: want not a marker (a symlink is never a marker)")
	}
	if isWorkspaceMarkerEntry(dir, "elsewhere", caseInsensitiveLstat) {
		t.Error("elsewhere: want not a marker")
	}
}

// An exact marker name that is a symlink is not a marker, even when it
// points at a real directory (Lstat does not follow it).
func TestIsWorkspaceMarkerEntry_ExactNameSymlinkIsNotMarker(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "elsewhere"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("elsewhere", filepath.Join(dir, ".agents")); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	if IsWorkspaceMarkerEntry(dir, ".agents") {
		t.Error(".agents symlink to a dir: want not a marker")
	}
}
