//go:build unix

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

package commands

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/initenv"
)

func TestScopedSafeDirectoryEntries(t *testing.T) {
	cases := []struct {
		name string
		root string
		want []string
	}{
		{"workspace", "/workspace", []string{"/workspace", "/workspace/*"}},
		{"workspace trailing slash", "/workspace/", []string{"/workspace", "/workspace/*"}},
		{"repo-root", "/repo-root", []string{"/repo-root", "/repo-root/*"}},
		{"nested root", "/mnt/project-storage/p1", []string{"/mnt/project-storage/p1", "/mnt/project-storage/p1/*"}},
		{"empty refused", "", nil},
		{"filesystem root refused", "/", nil},
		{"double slash root refused", "//", nil},
		{"wildcard refused", "*", nil},
		{"relative refused", "workspace", nil},
		{"dot refused", ".", nil},
		{"dotdot refused", "..", nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := scopedSafeDirectoryEntries(tc.root)
			if !slices.Equal(got, tc.want) {
				t.Errorf("scopedSafeDirectoryEntries(%q) = %v, want %v", tc.root, got, tc.want)
			}
			for _, entry := range got {
				if entry == "*" || entry == "/*" {
					t.Errorf("scopedSafeDirectoryEntries(%q) produced forbidden global wildcard %q", tc.root, entry)
				}
			}
		})
	}
}

func TestAgentSafeDirectoryPaths(t *testing.T) {
	noDirs := func(string) bool { return false }
	hasRepoRoot := func(p string) bool { return p == "/repo-root" }

	cases := []struct {
		name      string
		env       map[string]string
		cwd       string
		dirExists func(string) bool
		want      []string
	}{
		{
			name:      "default workspace without repo-root",
			cwd:       "/workspace",
			dirExists: noDirs,
			want:      []string{"/workspace", "/workspace/*"},
		},
		{
			name:      "k8s nfs worktree-per-agent",
			env:       map[string]string{"SCION_WORKSPACE_PATH": "/repo-root/worktrees/agent-1"},
			cwd:       "/repo-root/worktrees/agent-1",
			dirExists: hasRepoRoot,
			want:      []string{"/repo-root", "/repo-root/*"},
		},
		{
			name:      "docker linked worktree-per-agent",
			cwd:       "/repo-root/.scion/agents/dev/workspace",
			dirExists: hasRepoRoot,
			want:      []string{"/workspace", "/workspace/*", "/repo-root", "/repo-root/*"},
		},
		{
			name:      "outside-root registered worktree",
			cwd:       "/workspace",
			dirExists: hasRepoRoot,
			want:      []string{"/workspace", "/workspace/*", "/repo-root", "/repo-root/*"},
		},
		{
			name:      "custom workspace path",
			env:       map[string]string{"SCION_WORKSPACE_PATH": "/custom/ws"},
			cwd:       "/custom/ws",
			dirExists: noDirs,
			want:      []string{"/custom/ws", "/custom/ws/*"},
		},
		{
			name:      "dangerous root path ignored",
			env:       map[string]string{"SCION_WORKSPACE_PATH": "/"},
			cwd:       "/",
			dirExists: noDirs,
			want:      nil,
		},
		{
			name:      "wildcard path ignored",
			env:       map[string]string{"SCION_WORKSPACE_PATH": "*"},
			cwd:       "/tmp",
			dirExists: noDirs,
			want:      nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			getenv := func(k string) string { return tc.env[k] }
			got := agentSafeDirectoryPaths(getenv, tc.cwd, tc.dirExists)
			if !slices.Equal(got, tc.want) {
				t.Errorf("agentSafeDirectoryPaths() = %v, want %v", got, tc.want)
			}
			for _, entry := range got {
				if entry == "*" || entry == "/*" {
					t.Errorf("agentSafeDirectoryPaths() produced forbidden global wildcard %q", entry)
				}
			}
		})
	}
}

func TestConfigureGitSafeDirectories_RealGitDifferentOwner(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	root := t.TempDir()
	ws := filepath.Join(root, "project-storage")
	other := filepath.Join(root, "untrusted-other")
	agentHome := filepath.Join(root, "home")
	for _, d := range []string{ws, other, agentHome} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// Initialize workspace repo with a commit and a worktree, plus a nested repo
	// and an unrelated repo outside the project-storage root.
	gitT(t, "", "init", "--initial-branch=main", ws)
	if err := os.WriteFile(filepath.Join(ws, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, ws, "add", "README.md")
	gitT(t, ws, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "initial")

	wt := filepath.Join(ws, "worktrees", "agent-1")
	gitT(t, ws, "worktree", "add", "-b", "agent-one", wt)

	nested := filepath.Join(ws, "nested", "sub")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	gitT(t, "", "init", nested)

	gitT(t, "", "init", other)

	// Snapshot the repository's .git/config to verify it is never modified.
	repoConfigPath := filepath.Join(ws, ".git", "config")
	repoConfigBefore, err := os.ReadFile(repoConfigPath)
	if err != nil {
		t.Fatal(err)
	}

	gitconfigPath := filepath.Join(agentHome, ".gitconfig")
	runGitAsDifferentOwner := func(dir string) (string, error) {
		cmd := exec.Command("git", "-C", dir, "status", "--porcelain")
		cmd.Env = []string{
			"PATH=" + os.Getenv("PATH"),
			"HOME=" + agentHome,
			"GIT_CONFIG_GLOBAL=" + gitconfigPath,
			"GIT_CONFIG_NOSYSTEM=1",
			"GIT_TERMINAL_PROMPT=0",
			"GIT_TEST_ASSUME_DIFFERENT_OWNER=1",
		}
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	// Prove the problem first: before configuration, git refuses with dubious
	// ownership in both the root and its worktree.
	for _, dir := range []string{ws, wt, nested} {
		out, err := runGitAsDifferentOwner(dir)
		if err == nil {
			t.Fatalf("expected dubious ownership failure in %s before configuration, but git succeeded", dir)
		}
		if !strings.Contains(out, "dubious ownership") {
			t.Fatalf("expected dubious ownership error in %s, got: %v\n%s", dir, err, out)
		}
	}

	// Pre-seed a user entry to ensure configureGitSafeDirectoriesWithPolicy
	// preserves existing entries and stays idempotent.
	if err := os.WriteFile(gitconfigPath, []byte("[safe]\n\tdirectory = /existing/user-path\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("SCION_WORKSPACE_PATH", ws)
	if err := configureGitSafeDirectoriesWithPolicy(agentHome, os.Getuid(), os.Getgid(), false, initenv.Policy{}); err != nil {
		t.Fatalf("configureGitSafeDirectoriesWithPolicy: %v", err)
	}
	// Run a second time to verify idempotency (no duplicate entries).
	if err := configureGitSafeDirectoriesWithPolicy(agentHome, os.Getuid(), os.Getgid(), false, initenv.Policy{}); err != nil {
		t.Fatalf("second configureGitSafeDirectoriesWithPolicy: %v", err)
	}

	// Verify ~/.gitconfig entries: scoped to ws and ws/*, preserving user's
	// entry, no duplicates, and no wildcard "*".
	out, err := exec.Command("git", "config", "--file", gitconfigPath, "--get-all", "safe.directory").CombinedOutput()
	if err != nil {
		t.Fatalf("read safe.directory: %v\n%s", err, out)
	}
	gotEntries := strings.Split(strings.TrimSpace(string(out)), "\n")
	wantEntries := []string{"/existing/user-path", ws, ws + "/*"}
	if !slices.Equal(gotEntries, wantEntries) {
		t.Errorf("safe.directory entries = %v, want %v", gotEntries, wantEntries)
	}
	for _, entry := range gotEntries {
		if entry == "*" || entry == "/*" {
			t.Errorf("forbidden global wildcard %q in %s", entry, gitconfigPath)
		}
	}

	// After configuration, git succeeds under GIT_TEST_ASSUME_DIFFERENT_OWNER=1
	// in the root, worktree, and nested repo...
	for _, dir := range []string{ws, wt, nested} {
		if out, err := runGitAsDifferentOwner(dir); err != nil {
			t.Errorf("git status in %s failed after safe.directory config: %v\n%s", dir, err, out)
		}
	}

	// ...while an unrelated repo outside the project-storage root is STILL refused.
	if out, err := runGitAsDifferentOwner(other); err == nil {
		t.Errorf("git status in untrusted repo %s succeeded; trust must be scoped, not global", other)
	} else if !strings.Contains(out, "dubious ownership") {
		t.Errorf("expected dubious ownership in untrusted repo %s, got: %v\n%s", other, err, out)
	}

	// Repository .git/config must be completely untouched.
	repoConfigAfter, err := os.ReadFile(repoConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(repoConfigBefore, repoConfigAfter) {
		t.Errorf("repository .git/config was modified!\nbefore:\n%s\nafter:\n%s", repoConfigBefore, repoConfigAfter)
	}
}

func TestConfigureGitSafeDirectories_PrivilegeDropRequired(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SCION_WORKSPACE_PATH", "/workspace")
	err := configureGitSafeDirectoriesWithPolicy(home, 0, 0, true, initenv.Policy{})
	if err != errSafeDirectoryPrivilegeDropRequired {
		t.Fatalf("err = %v, want %v", err, errSafeDirectoryPrivilegeDropRequired)
	}
	if _, statErr := os.Stat(filepath.Join(home, ".gitconfig")); !os.IsNotExist(statErr) {
		t.Errorf(".gitconfig should not have been created when privilege drop is required, stat err = %v", statErr)
	}
}

func TestConfigureGitSafeDirectories_RefusesNonRegularFile(t *testing.T) {
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, ".gitconfig"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SCION_WORKSPACE_PATH", "/workspace")
	if err := configureGitSafeDirectoriesWithPolicy(home, os.Getuid(), os.Getgid(), false, initenv.Policy{}); err != nil {
		t.Fatalf("expected non-regular .gitconfig to be skipped without error, got: %v", err)
	}
}

func TestRunProvision_SharedPlain_SetsSafeDirectory(t *testing.T) {
	origin := provisionTestRepo(t)
	workspace := filepath.Join(t.TempDir(), "workspace")
	setupProvisionCmd(t, workspace, "shared-plain", origin)

	if err := runProvision(context.Background()); err != nil {
		t.Fatalf("runProvision (shared-plain): %v", err)
	}

	want := map[string]string{
		"GIT_CONFIG_COUNT":   "2",
		"GIT_CONFIG_KEY_0":   "safe.directory",
		"GIT_CONFIG_VALUE_0": workspace,
		"GIT_CONFIG_KEY_1":   "safe.directory",
		"GIT_CONFIG_VALUE_1": workspace + "/*",
	}
	for k, v := range want {
		if got := os.Getenv(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

func TestRunProvision_WorktreeMode_DifferentOwner(t *testing.T) {
	origin := provisionTestRepo(t)
	workspace := filepath.Join(t.TempDir(), "workspace")
	// First provision the shared checkout (simulating a first broker writing
	// the project-storage tree under its own UID).
	setupProvisionCmd(t, workspace, "shared-plain", origin)
	if err := runProvision(context.Background()); err != nil {
		t.Fatalf("initial shared-plain provision: %v", err)
	}

	// Now simulate another broker/container with a different UID adding a
	// worktree into the existing project-storage tree.
	setupProvisionCmd(t, workspace, "shared-plain", origin)
	setWorktreeEnv(t, "agent-1", "agent-one")
	t.Setenv("GIT_TEST_ASSUME_DIFFERENT_OWNER", "1")

	if err := runProvision(context.Background()); err != nil {
		t.Fatalf("runProvision (worktree-per-agent, different owner): %v", err)
	}
	assertAgentWorktree(t, workspace, "agent-1", "agent-one")
}
