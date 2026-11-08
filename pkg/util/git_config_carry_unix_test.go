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

// These tests run git and the seed layer check, which reads files with a
// unix-only no-follow open.
//go:build unix

package util

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func appendConfig(t *testing.T, path, body string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(body); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// With stripping, a never-carry key in .git/config passes the layer check (the
// copy leaves it out), but the same key in either config.worktree layer, which
// is bound raw, is still refused. Without stripping it is refused in .git/config.
func TestCheckSeedRepoConfigLayers_NeverCarryByLayer(t *testing.T) {
	commonDir, _, name := seedLayersRepo(t)
	appendConfig(t, filepath.Join(commonDir, "config"), "[core]\n\teditor = vi\n[alias]\n\tst = !sh\n")
	if err := CheckSeedRepoConfigLayers(context.Background(), commonDir, name, true); err != nil {
		t.Fatalf("with stripping, .git/config never-carry keys refused: %v", err)
	}
	var refused *RepoConfigRefusedError
	err := CheckSeedRepoConfigLayers(context.Background(), commonDir, name, false)
	if !errors.As(err, &refused) || refused.Key != "core.editor" || refused.Reason != neverCarryReason {
		t.Fatalf("without stripping: %v, want core.editor refused as never-carry", err)
	}
	for _, layer := range []struct{ path, label string }{
		{filepath.Join(commonDir, "config.worktree"), seedLayerCommonWorktree},
		{filepath.Join(commonDir, "worktrees", name, "config.worktree"), seedLayerOwnWorktree},
	} {
		writeLayer(t, layer.path, "[core]\n\thooksPath = /tmp/neverhooks\n")
		err := CheckSeedRepoConfigLayers(context.Background(), commonDir, name, true)
		if !errors.As(err, &refused) || refused.Key != "core.hookspath" || refused.Layer != layer.label || refused.Reason != neverCarryReason {
			t.Errorf("%s: %v, want core.hookspath refused in that raw layer even with stripping", layer.label, err)
		}
		if err := os.Remove(layer.path); err != nil {
			t.Fatal(err)
		}
	}
}

// seedFakeGitOnPath puts a "git" first on PATH that only creates sentinel and fails,
// so a test can tell whether the seed check ran git at all.
func seedFakeGitOnPath(t *testing.T) (sentinel string) {
	t.Helper()
	dir := t.TempDir()
	sentinel = filepath.Join(dir, "git-ran")
	script := "#!/bin/sh\n: > '" + sentinel + "'\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return sentinel
}

// An unknown key or an include in .git/config refuses BEFORE the seed runs any
// git, with stripping on. A clean config does reach git (the control).
func TestCheckSeedRepoConfigLayers_RefusesBeforeGit(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"unknown key", "[core]\n\teditor = vi\n[mytool]\n\tsetting = 1\n"},
		{"include", "[core]\n\teditor = vi\n[include]\n\tpath = /tmp/inc\n"},
		{"includeIf", "[includeIf \"gitdir:/x/\"]\n\tpath = /tmp/inc\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			commonDir, _, name := seedLayersRepo(t)
			appendConfig(t, filepath.Join(commonDir, "config"), tc.body)
			sentinel := seedFakeGitOnPath(t)
			var refused *RepoConfigRefusedError
			if err := CheckSeedRepoConfigLayers(context.Background(), commonDir, name, true); !errors.As(err, &refused) {
				t.Fatalf("error = %v, want a refusal", err)
			}
			if _, err := os.Stat(sentinel); err == nil {
				t.Error("git ran before the refusal")
			}
		})
	}
	t.Run("control: a clean config reaches git", func(t *testing.T) {
		commonDir, _, name := seedLayersRepo(t)
		sentinel := seedFakeGitOnPath(t)
		_ = CheckSeedRepoConfigLayers(context.Background(), commonDir, name, true)
		if _, err := os.Stat(sentinel); err != nil {
			t.Error("the fake git did not run for a clean config, so the probe proves nothing")
		}
	})
}
