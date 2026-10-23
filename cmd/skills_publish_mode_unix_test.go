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

//go:build !windows

package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCollectPublishFiles_ModesFromFilesAndSymlinkTargets pins that publish
// records an executable script as "0755", a data file as "0644", and a
// symlink with its target's mode and size, not the link's own 0777
// (ptone/scion#5657 review).
func TestCollectPublishFiles_ModesFromFilesAndSymlinkTargets(t *testing.T) {
	outside := t.TempDir()
	target := filepath.Join(outside, "data.txt")
	if err := os.WriteFile(target, []byte("shared data\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	mustWrite := func(name string, perm os.FileMode) {
		t.Helper()
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(name), perm); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, perm); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("SKILL.md", 0o644)
	mustWrite("scripts/run.sh", 0o755)
	if err := os.Symlink(target, filepath.Join(dir, "data.txt")); err != nil {
		t.Fatal(err)
	}

	files, err := collectPublishFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]publishFile{}
	for _, f := range files {
		got[f.path] = f
	}
	want := map[string]string{"SKILL.md": "0644", "scripts/run.sh": "0755", "data.txt": "0644"}
	if len(got) != len(want) {
		t.Fatalf("collected %v, want %v", got, want)
	}
	for p, m := range want {
		if got[p].mode != m {
			t.Errorf("%s: mode %q, want %q", p, got[p].mode, m)
		}
	}
	if got["data.txt"].size != int64(len("shared data\n")) {
		t.Errorf("symlink size %d, want the target's size", got["data.txt"].size)
	}
}
