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
	"testing"
)

// caseSensitiveTempFS reports whether dir's filesystem tells names apart by
// case.
func caseSensitiveTempFS(t *testing.T, dir string) bool {
	t.Helper()
	probe := filepath.Join(dir, "case-probe")
	if err := os.Mkdir(probe, 0o755); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Remove(probe) }()
	_, err := os.Lstat(filepath.Join(dir, "CASE-PROBE"))
	return err != nil
}

// isWorkspaceEmptyDir uses the shared workspace marker rule: marker entries
// (including the .agents mount point) leave the workspace empty; anything
// else, including a cased marker name on a case-sensitive filesystem, does
// not.
func TestIsWorkspaceEmptyDir(t *testing.T) {
	tests := []struct {
		name          string
		dirs          []string
		files         []string
		needsCaseSens bool
		want          bool
	}{
		{name: "empty", want: true},
		{name: "provisioning markers", dirs: []string{".scion", ".scion-volumes"}, want: true},
		{name: "shared-dir mount point", dirs: []string{".agents"}, want: true},
		{name: "all markers", dirs: []string{".scion", ".scion-volumes", ".agents"}, want: true},
		{name: "real file", dirs: []string{".scion"}, files: []string{"README.md"}, want: false},
		{name: "non-marker directory", dirs: []string{".scionx"}, want: false},
		{name: "cased marker name on a case-sensitive filesystem", dirs: []string{".SCION"}, needsCaseSens: true, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if tt.needsCaseSens && !caseSensitiveTempFS(t, dir) {
				t.Skip("the temp filesystem is case-insensitive")
			}
			for _, d := range tt.dirs {
				if err := os.Mkdir(filepath.Join(dir, d), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			for _, f := range tt.files {
				if err := os.WriteFile(filepath.Join(dir, f), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if got := isWorkspaceEmptyDir(dir); got != tt.want {
				t.Errorf("isWorkspaceEmptyDir = %v, want %v", got, tt.want)
			}
		})
	}
}

// A symlink named like a marker under another case is content on a
// case-sensitive filesystem, so the workspace is not empty.
func TestIsWorkspaceEmptyDir_CaseSensitiveSymlinkIsContent(t *testing.T) {
	dir := t.TempDir()
	if !caseSensitiveTempFS(t, dir) {
		t.Skip("the temp filesystem is case-insensitive")
	}
	if err := os.Mkdir(filepath.Join(dir, ".scion"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(".scion", filepath.Join(dir, ".SCION")); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	if isWorkspaceEmptyDir(dir) {
		t.Error("isWorkspaceEmptyDir = true, want false: the .SCION symlink is content")
	}
}

// A workspace whose only entry is an exact-name ".agents" SYMLINK is not
// empty: a symlink is never a marker, even one that points at a directory.
func TestIsWorkspaceEmptyDir_AgentsSymlinkIsContent(t *testing.T) {
	dir := t.TempDir()
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(dir, ".agents")); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	if isWorkspaceEmptyDir(dir) {
		t.Error("isWorkspaceEmptyDir = true, want false: the .agents symlink is content")
	}
}
