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

// These tests run git and create symlinks; the seed check reads files with a
// unix-only no-follow open.
//go:build unix

package util

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// seedLayersRepo makes a repository with one commit and a linked worktree, and
// returns its common dir, the worktree path and the worktree's admin name.
func seedLayersRepo(t *testing.T) (commonDir, worktree, adminName string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run(root, "init", "-q", "-b", "main", repo)
	if err := os.WriteFile(filepath.Join(repo, "f"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(repo, "add", "f")
	run(repo, "commit", "-q", "-m", "init")
	worktree = filepath.Join(root, "wt")
	run(repo, "worktree", "add", "-q", "-b", "agent-x", worktree)
	commonDir = filepath.Join(repo, ".git")
	name, ok := TrustedWorktreeAdminName(commonDir, worktree)
	if !ok {
		t.Fatal("fixture: worktree admin name does not resolve")
	}
	return commonDir, worktree, name
}

func writeLayer(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCheckSeedRepoConfigLayers_CleanPasses(t *testing.T) {
	commonDir, _, name := seedLayersRepo(t)
	// A key the redactor drops is exempt in .git/config, as on the seed path.
	f, err := os.OpenFile(filepath.Join(commonDir, "config"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("[credential]\n\thelper = store\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	writeLayer(t, filepath.Join(commonDir, "config.worktree"), "[core]\n\tsparseCheckout = true\n")
	for _, admin := range []string{"", name} {
		if err := CheckSeedRepoConfigLayers(context.Background(), commonDir, admin); err != nil {
			t.Errorf("admin %q: clean layers refused: %v", admin, err)
		}
	}
}

// A refused, pin-only or dropped key in either config.worktree layer
// refuses, naming the key and the layer, never the value.
func TestCheckSeedRepoConfigLayers_LayerKeysRefuse(t *testing.T) {
	for _, tc := range []struct {
		name, line, key string
		own             bool
	}{
		{"common refused key", "[core]\n\tsshCommand = ssh -i /tmp/layermarker-q1\n", "core.sshcommand", false},
		{"common pin-only key", "[core]\n\thooksPath = /tmp/layermarker-q1\n", "core.hookspath", false},
		{"common dropped key", "[credential]\n\thelper = /tmp/layermarker-q1\n", "credential.helper", false},
		{"own refused key", "[core]\n\tsshCommand = ssh -i /tmp/layermarker-q1\n", "core.sshcommand", true},
		{"own pin-only key", "[core]\n\tfsmonitor = /tmp/layermarker-q1\n", "core.fsmonitor", true},
		{"own dropped key", "[http]\n\textraHeader = Authorization: layermarker-q1\n", "http.extraheader", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			commonDir, _, name := seedLayersRepo(t)
			path, layer := filepath.Join(commonDir, "config.worktree"), seedLayerCommonWorktree
			if tc.own {
				path, layer = filepath.Join(commonDir, "worktrees", name, "config.worktree"), seedLayerOwnWorktree
			}
			writeLayer(t, path, tc.line)
			err := CheckSeedRepoConfigLayers(context.Background(), commonDir, name)
			var refused *RepoConfigRefusedError
			if !errors.As(err, &refused) {
				t.Fatalf("error = %v, want *RepoConfigRefusedError", err)
			}
			if refused.Key != tc.key || refused.Layer != layer || !refused.Seeded {
				t.Errorf("refusal = key %q layer %q seeded %v; want %q, %q, true", refused.Key, refused.Layer, refused.Seeded, tc.key, layer)
			}
			msg := err.Error()
			if strings.Contains(msg, "layermarker-q1") || !strings.Contains(msg, "(in "+layer+")") {
				t.Errorf("error must name the layer and not the value: %s", msg)
			}
			if !strings.Contains(msg, "remove it from the git config file that sets it (in "+layer+")") || strings.Contains(msg, ".git/config") || strings.Contains(msg, "global") {
				t.Errorf("error must give the remedy for its own layer: %s", msg)
			}
		})
	}
}

// The layers are checked whether or not extensions.worktreeConfig is set
// (it is not set in the fixture), and an include in .git/config refuses before
// git runs.
func TestCheckSeedRepoConfigLayers_IncludeInConfigRefusesFirst(t *testing.T) {
	commonDir, _, name := seedLayersRepo(t)
	f, err := os.OpenFile(filepath.Join(commonDir, "config"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("[include]\n\tpath = /nonexistent/includemarker-q2\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	err = CheckSeedRepoConfigLayers(context.Background(), commonDir, name)
	var refused *RepoConfigRefusedError
	if !errors.As(err, &refused) || refused.Reason != "an include directive" || refused.Layer != "" {
		t.Fatalf("error = %v, want an include refusal in .git/config", err)
	}
	if strings.Contains(err.Error(), "includemarker-q2") {
		t.Errorf("error leaks the include path: %s", err)
	}
}

// A layer that is a symlink, or not a regular file, fails closed.
func TestCheckSeedRepoConfigLayers_NonRegularLayerFailsClosed(t *testing.T) {
	commonDir, _, name := seedLayersRepo(t)
	target := filepath.Join(t.TempDir(), "elsewhere")
	writeLayer(t, target, "[core]\n\tsparseCheckout = true\n")
	if err := os.Symlink(target, filepath.Join(commonDir, "worktrees", name, "config.worktree")); err != nil {
		t.Fatal(err)
	}
	if err := CheckSeedRepoConfigLayers(context.Background(), commonDir, name); err == nil {
		t.Fatal("a symlinked config.worktree was accepted")
	}
}

// A root-level .git/commondir or .git/gitdir refuses the seed with the file
// remedy and no host path: Dir is empty and the repository path is not printed.
func TestCheckSeedRepoConfigLayers_RootRedirectFileRefuses(t *testing.T) {
	for _, name := range []string{"commondir", "gitdir"} {
		t.Run(name, func(t *testing.T) {
			commonDir, _, adminName := seedLayersRepo(t)
			writeLayer(t, filepath.Join(commonDir, name), "../elsewhere\n")
			err := CheckSeedRepoConfigLayers(context.Background(), commonDir, adminName)
			var refused *RepoConfigRefusedError
			if !errors.As(err, &refused) {
				t.Fatalf("error = %v, want *RepoConfigRefusedError", err)
			}
			if !refused.Seeded || refused.Dir != "" || refused.Key != ".git/"+name {
				t.Errorf("refusal = seeded %v dir %q key %q; want true, \"\", .git/%s", refused.Seeded, refused.Dir, refused.Key, name)
			}
			msg := err.Error()
			if !strings.Contains(msg, "remove that file from the repository's .git directory") {
				t.Errorf("refusal lacks the file remedy: %s", msg)
			}
			for _, bad := range []string{filepath.Dir(commonDir), "global", "config file"} {
				if strings.Contains(msg, bad) {
					t.Errorf("refusal mentions %q: %s", bad, msg)
				}
			}
		})
	}
}

// A key git resolves but the parsed set lacks (a parser differential, made here
// by a reader that drops one key) refuses through the cross-check.
func TestCheckSeedRepoConfigLayers_ParserDifferentialRefuses(t *testing.T) {
	commonDir, _, name := seedLayersRepo(t)
	writeLayer(t, filepath.Join(commonDir, "config.worktree"), "[core]\n\tsparseCheckout = true\n")
	// git reads config.worktree only with extensions.worktreeConfig set.
	f, err := os.OpenFile(filepath.Join(commonDir, "config"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("[extensions]\n\tworktreeConfig = true\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	// The real reader passes.
	if err := CheckSeedRepoConfigLayers(context.Background(), commonDir, ""); err != nil {
		t.Fatalf("baseline refused: %v", err)
	}
	dropping := func(path, label string) ([]gitConfigVar, error) {
		vars, err := readSeedConfigLayer(path, label)
		if label != seedLayerCommonWorktree {
			return vars, err
		}
		var kept []gitConfigVar
		for _, v := range vars {
			if v.Key() != "core.sparsecheckout" {
				kept = append(kept, v)
			}
		}
		return kept, err
	}
	err = checkSeedRepoConfigLayers(context.Background(), commonDir, "", dropping)
	var refused *RepoConfigRefusedError
	if !errors.As(err, &refused) || !strings.Contains(refused.Reason, "read differently") || refused.Key != "core.sparsecheckout" || !refused.Seeded {
		t.Fatalf("error = %v, want a seeded cross-check refusal for core.sparsecheckout", err)
	}
	// The differing key may come from any layer, so the remedy names them all.
	if msg := err.Error(); !strings.Contains(msg, "remove it from the git config file that sets it (in .git/config or a config.worktree)") || strings.Contains(msg, "global") {
		t.Errorf("cross-check refusal lacks the combined-layer remedy: %s", msg)
	}
	_ = name
}
