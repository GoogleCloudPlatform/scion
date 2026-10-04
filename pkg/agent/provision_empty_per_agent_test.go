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
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// emptyPerAgentFixture sets HOME and the working directory to a fresh temp
// dir with machine-level config, and returns that dir.
func emptyPerAgentFixture(t *testing.T) string {
	t.Helper()
	mockRuntimeForTest(t)
	tmpDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(tmpDir)
	t.Setenv("HOME", tmpDir)
	if err := config.InitMachine(getTestHarnesses()); err != nil {
		t.Fatalf("InitMachine failed: %v", err)
	}
	return tmpDir
}

// initEmptyPerAgentProject creates a project .scion dir at projectScionDir
// and returns the resolved project dir (InitProject externalizes a project
// to ~/.scion/project-configs, as on a broker). bare creates a plain
// in-place .scion dir instead, the linked-project layout. A non-empty
// workspacePath is written as the project's settings.workspace_path, the
// setting that wins for an ordinary non-git project.
func initEmptyPerAgentProject(t *testing.T, projectScionDir string, bare bool, workspacePath string) string {
	t.Helper()
	if bare {
		if err := os.MkdirAll(projectScionDir, 0755); err != nil {
			t.Fatal(err)
		}
	} else if err := config.InitProject(projectScionDir, getTestHarnesses()); err != nil {
		t.Fatalf("InitProject failed: %v", err)
	}
	projectDir, err := config.GetResolvedProjectDir(projectScionDir)
	if err != nil {
		t.Fatalf("GetResolvedProjectDir: %v", err)
	}
	if workspacePath != "" {
		if err := os.MkdirAll(workspacePath, 0755); err != nil {
			t.Fatal(err)
		}
		settings := "schema_version: \"1\"\nworkspace_path: " + workspacePath + "\n"
		if err := os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(settings), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return projectDir
}

// gitInitForTest makes root a git repository that ignores .scion/agents/.
func gitInitForTest(t *testing.T, root string) {
	t.Helper()
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "init", "-q", root).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(".scion/agents/\n"), 0644); err != nil {
		t.Fatal(err)
	}
}

func workspaceVolumeSource(cfg *api.ScionConfig) string {
	if cfg == nil {
		return ""
	}
	for _, v := range cfg.Volumes {
		if v.Target == "/workspace" {
			return v.Source
		}
	}
	return ""
}

func assertEmptyDir(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("expected workspace dir %s: %v", dir, err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected empty workspace dir %s, found %d entries", dir, len(entries))
	}
}

// TestProvisionAgent_EmptyPerAgent covers the explicit empty-per-agent
// branch (design #2703 P2) on the projectDir shapes a broker sees: the
// workspace is exactly <projectDir>/agents/<slug>/workspace, empty, never a
// git worktree, and settings.workspace_path (which wins for an ordinary
// non-git project) does not.
func TestProvisionAgent_EmptyPerAgent(t *testing.T) {
	cases := []struct {
		name string
		// projectScionDir returns the project's .scion dir under tmpDir.
		projectScionDir func(tmpDir string) string
		// workspacePath is written as settings.workspace_path when non-empty.
		workspacePath func(tmpDir string) string
		// gitRoot, when non-empty, is git-initialised before provisioning.
		gitRoot func(tmpDir string) string
		// bare uses an in-place .scion dir (linked layout).
		bare bool
	}{
		{
			name:            "hub-managed project dir",
			projectScionDir: func(d string) string { return filepath.Join(d, ".scion", "projects", "notes", ".scion") },
		},
		{
			name:            "linked-style project dir with settings.workspace_path",
			projectScionDir: func(d string) string { return filepath.Join(d, "project", ".scion") },
			workspacePath:   func(d string) string { return filepath.Join(d, "shared-files") },
			bare:            true,
		},
		{
			name:            "project dir inside a git repository",
			projectScionDir: func(d string) string { return filepath.Join(d, "repo", ".scion") },
			gitRoot:         func(d string) string { return filepath.Join(d, "repo") },
			bare:            true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := emptyPerAgentFixture(t)
			projectScionDir := tc.projectScionDir(tmpDir)
			workspacePath := ""
			if tc.workspacePath != nil {
				workspacePath = tc.workspacePath(tmpDir)
			}
			if tc.gitRoot != nil {
				gitInitForTest(t, tc.gitRoot(tmpDir))
			}
			projectDir := initEmptyPerAgentProject(t, projectScionDir, tc.bare, workspacePath)

			if workspacePath != "" {
				// Precondition: without the mode, settings.workspace_path is
				// what an ordinary non-git agent gets, so the assertion below
				// proves the empty-per-agent branch beats it.
				_, ws, cfg, err := ProvisionAgent(context.Background(), "plain", "default", "", "", projectScionDir, "", "", "", "")
				if err != nil {
					t.Fatalf("ProvisionAgent (plain) failed: %v", err)
				}
				if ws != "" || workspaceVolumeSource(cfg) != workspacePath {
					t.Fatalf("precondition: plain non-git agent should mount settings.workspace_path %s, got ws=%q volume=%q",
						workspacePath, ws, workspaceVolumeSource(cfg))
				}
			}

			ctx := api.ContextWithEmptyPerAgentWorkspace(context.Background())
			_, ws, cfg, err := ProvisionAgent(ctx, "worker", "default", "", "", projectScionDir, "", "", "", "")
			if err != nil {
				t.Fatalf("ProvisionAgent failed: %v", err)
			}
			want := filepath.Join(projectDir, "agents", "worker", "workspace")
			if ws != want {
				t.Fatalf("workspace = %q, want %q", ws, want)
			}
			if src := workspaceVolumeSource(cfg); src != "" {
				t.Fatalf("expected no extra /workspace volume, got source %q", src)
			}
			assertEmptyDir(t, want)
			if _, err := os.Stat(filepath.Join(want, ".git")); !os.IsNotExist(err) {
				t.Fatalf("empty-per-agent workspace must not be a git worktree (stat .git err = %v)", err)
			}
			// Only workspace (plus the agent's ordinary files) is created
			// under agents/<slug>; nothing is created beside the project
			// dir for the agent.
			if workspacePath != "" {
				assertEmptyDir(t, workspacePath)
			}
		})
	}
}

// TestProvisionAgent_EmptyPerAgent_RefusesOtherWorkspaceSources pins that a
// request naming another workspace source alongside empty-per-agent is
// refused rather than resolved in favour of either.
func TestProvisionAgent_EmptyPerAgent_RefusesOtherWorkspaceSources(t *testing.T) {
	tmpDir := emptyPerAgentFixture(t)
	projectScionDir := filepath.Join(tmpDir, ".scion", "projects", "notes", ".scion")
	initEmptyPerAgentProject(t, projectScionDir, false, "")
	if err := os.MkdirAll(filepath.Join(tmpDir, "elsewhere"), 0755); err != nil {
		t.Fatal(err)
	}

	base := api.ContextWithEmptyPerAgentWorkspace(context.Background())
	cases := []struct {
		name      string
		ctx       context.Context
		workspace string
		wantErr   string
	}{
		{"absolute workspace", base, filepath.Join(tmpDir, "elsewhere"), "does not take a workspace path"},
		{"relative workspace", base, "sub", "does not take a workspace path"},
		{"shared workspace", api.ContextWithSharedWorkspace(base), "", "shared workspace"},
		{"git clone", api.ContextWithGitClone(base, &api.GitCloneConfig{URL: "https://example.com/o/r.git"}), "", "git clone"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, err := ProvisionAgent(tc.ctx, "worker", "default", "", "", projectScionDir, "", "", "", tc.workspace)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// TestGetAgent_EmptyPerAgent_ResumeKeepsAndRecreatesWorkspace covers
// restart and resume: existing content survives (no wipe, even on a create
// dispatch for a leftover directory: see #2675), and a workspace that did
// not survive is recreated empty, never as a worktree even when projectDir
// sits in a git repository, and never cleared so another mount wins.
func TestGetAgent_EmptyPerAgent_ResumeKeepsAndRecreatesWorkspace(t *testing.T) {
	tmpDir := emptyPerAgentFixture(t)
	root := filepath.Join(tmpDir, "repo")
	gitInitForTest(t, root)
	projectScionDir := filepath.Join(root, ".scion")
	projectDir := initEmptyPerAgentProject(t, projectScionDir, true, "")

	ctx := api.ContextWithEmptyPerAgentWorkspace(context.Background())
	_, _, ws, _, err := GetAgent(ctx, "worker", "default", "", "", projectScionDir, "", "", "", "")
	if err != nil {
		t.Fatalf("GetAgent (provision) failed: %v", err)
	}
	want := filepath.Join(projectDir, "agents", "worker", "workspace")
	if ws != want {
		t.Fatalf("workspace = %q, want %q", ws, want)
	}
	note := filepath.Join(ws, "notes.txt")
	if err := os.WriteFile(note, []byte("keep me"), 0644); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name string
		ctx  context.Context
	}{
		{"restart", ctx},
		{"create over leftover dir", api.ContextWithFreshProvision(ctx)},
	} {
		_, _, ws, _, err = GetAgent(c.ctx, "worker", "", "", "", projectScionDir, "", "", "", "")
		if err != nil {
			t.Fatalf("GetAgent (%s) failed: %v", c.name, err)
		}
		if ws != want {
			t.Fatalf("%s: workspace = %q, want %q", c.name, ws, want)
		}
		if b, err := os.ReadFile(note); err != nil || string(b) != "keep me" {
			t.Fatalf("%s: workspace content not kept: %q, %v", c.name, b, err)
		}
	}

	if err := os.RemoveAll(ws); err != nil {
		t.Fatal(err)
	}
	_, _, ws, _, err = GetAgent(ctx, "worker", "", "", "", projectScionDir, "", "", "", "")
	if err != nil {
		t.Fatalf("GetAgent (resume) failed: %v", err)
	}
	if ws != want {
		t.Fatalf("resume: workspace = %q, want %q", ws, want)
	}
	assertEmptyDir(t, want)
}

// TestDeleteAgentFiles_EmptyPerAgent_RemovesWorkspace pins that deleting an
// empty-per-agent agent removes agents/<slug> including its private
// workspace, and leaves a sibling agent's alone.
func TestDeleteAgentFiles_EmptyPerAgent_RemovesWorkspace(t *testing.T) {
	tmpDir := emptyPerAgentFixture(t)
	projectScionDir := filepath.Join(tmpDir, ".scion", "projects", "notes", ".scion")
	projectDir := initEmptyPerAgentProject(t, projectScionDir, false, "")

	ctx := api.ContextWithEmptyPerAgentWorkspace(context.Background())
	for _, name := range []string{"worker", "sibling"} {
		_, ws, _, err := ProvisionAgent(ctx, name, "default", "", "", projectScionDir, "", "", "", "")
		if err != nil {
			t.Fatalf("ProvisionAgent(%s) failed: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(ws, "f.txt"), []byte(name), 0644); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := DeleteAgentFiles("worker", projectScionDir, false); err != nil {
		t.Fatalf("DeleteAgentFiles failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(projectDir, "agents", "worker")); !os.IsNotExist(err) {
		t.Fatalf("expected agents/worker to be removed, stat err = %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(projectDir, "agents", "sibling", "workspace", "f.txt")); err != nil || string(b) != "sibling" {
		t.Fatalf("sibling workspace must be kept: %q, %v", b, err)
	}
}

// TestGetAgent_EmptyPerAgent_PersistedModeSurvivesLostFlag pins review N1 of
// #2760: the mode is persisted in scion-agent.json at provision, so a resume
// whose request lost it (no ctx flag) still recreates the private workspace
// rather than falling back to legacy resolution or the enclosing repo.
func TestGetAgent_EmptyPerAgent_PersistedModeSurvivesLostFlag(t *testing.T) {
	tmpDir := emptyPerAgentFixture(t)
	root := filepath.Join(tmpDir, "repo")
	gitInitForTest(t, root)
	projectScionDir := filepath.Join(root, ".scion")
	projectDir := initEmptyPerAgentProject(t, projectScionDir, true, "")

	ctx := api.ContextWithEmptyPerAgentWorkspace(context.Background())
	_, ws, _, err := ProvisionAgent(ctx, "worker", "default", "", "", projectScionDir, "", "", "", "")
	if err != nil {
		t.Fatalf("ProvisionAgent failed: %v", err)
	}
	agentDir := filepath.Join(projectDir, "agents", "worker")
	persisted, err := (&config.Template{Path: agentDir}).LoadConfig()
	if err != nil {
		t.Fatalf("load persisted scion-agent.json: %v", err)
	}
	if !persisted.EmptyPerAgentWorkspace {
		t.Fatal("scion-agent.json must record empty_per_agent_workspace")
	}

	if err := os.RemoveAll(ws); err != nil {
		t.Fatal(err)
	}
	_, _, got, _, err := GetAgent(context.Background(), "worker", "", "", "", projectScionDir, "", "", "", "")
	if err != nil {
		t.Fatalf("GetAgent (resume without flag) failed: %v", err)
	}
	want := filepath.Join(agentDir, "workspace")
	if got != want {
		t.Fatalf("workspace = %q, want %q", got, want)
	}
	assertEmptyDir(t, want)
	if _, err := os.Stat(filepath.Join(want, ".git")); !os.IsNotExist(err) {
		t.Fatalf("resumed workspace must not be a worktree, stat .git err = %v", err)
	}
}

// TestDeleteAgentFiles_EmptyPerAgent_GitInitWorkspaceKeepsBranches pins
// review N4 of #2760: deleting an empty-per-agent agent whose workspace the
// agent turned into a git repo, in a project inside an enclosing repository
// that has a branch named like the agent, removes only the agent's
// directories -- no worktree removal and no branch deletion.
func TestDeleteAgentFiles_EmptyPerAgent_GitInitWorkspaceKeepsBranches(t *testing.T) {
	for name, gitWorkspace := range map[string]func(git func(string, ...string) string, root, ws string){
		// The agent ran `git init` in its workspace.
		"git init in workspace": func(git func(string, ...string) string, root, ws string) {
			git(ws, "init", "-q", "-b", "worker")
			git(ws, "commit", "-q", "--allow-empty", "-m", "agent work")
		},
		// The workspace's .git points into the enclosing repository with
		// branch worker checked out; worktree cleanup would follow it and
		// delete that branch.
		"workspace linked to the enclosing repo": func(git func(string, ...string) string, root, ws string) {
			git(root, "worktree", "add", "-q", ws, "worker")
		},
	} {
		t.Run(name, func(t *testing.T) {
			// Host conditions: inside an agent container SCION_HOST_UID
			// disables worktree pruning, which would mask a branch delete.
			t.Setenv("SCION_HOST_UID", "")
			tmpDir := emptyPerAgentFixture(t)
			root := filepath.Join(tmpDir, "repo")
			gitInitForTest(t, root)
			git := func(dir string, args ...string) string {
				t.Helper()
				full := append([]string{"-C", dir, "-c", "user.email=t@example.com", "-c", "user.name=t"}, args...)
				out, err := exec.Command("git", full...).CombinedOutput()
				if err != nil {
					t.Fatalf("git %v: %v: %s", args, err, out)
				}
				return string(out)
			}
			git(root, "commit", "-q", "--allow-empty", "-m", "init")
			git(root, "branch", "worker")
			projectScionDir := filepath.Join(root, ".scion")
			projectDir := initEmptyPerAgentProject(t, projectScionDir, true, "")

			ctx := api.ContextWithEmptyPerAgentWorkspace(context.Background())
			_, ws, _, err := ProvisionAgent(ctx, "worker", "default", "", "", projectScionDir, "", "", "", "")
			if err != nil {
				t.Fatalf("ProvisionAgent failed: %v", err)
			}
			gitWorkspace(git, root, ws)

			if _, err := DeleteAgentFiles("worker", projectScionDir, true); err != nil {
				t.Fatalf("DeleteAgentFiles failed: %v", err)
			}
			if _, err := os.Stat(filepath.Join(projectDir, "agents", "worker")); !os.IsNotExist(err) {
				t.Fatalf("expected agents/worker to be removed, stat err = %v", err)
			}
			if out := git(root, "branch", "--list", "worker"); !strings.Contains(out, "worker") {
				t.Fatal("the enclosing repository's branch 'worker' must survive deleting an empty-per-agent agent")
			}
		})
	}
}
