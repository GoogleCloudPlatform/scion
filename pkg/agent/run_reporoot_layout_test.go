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

// These tests exercise unix-only, Windows-gated agent storage code.
//go:build unix

package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// mkGitRepo creates dir and makes it a git repository with one commit.
func mkGitRepo(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	setupGitRepo(t, dir)
	return dir
}

// setCommonDir writes a commondir file into repo's .git directory and checks
// that git now reports want as the common dir, so a test never passes only
// because git ignored the file.
func setCommonDir(t *testing.T, repo, commondir, want string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, ".git", "commondir"), []byte(commondir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := util.GetCommonGitDir(repo)
	if err != nil {
		t.Fatalf("GetCommonGitDir(%s) after writing commondir: %v", repo, err)
	}
	if eval(t, got) != eval(t, want) {
		t.Fatalf("fixture: git reports common dir %s, want %s", got, want)
	}
}

// mkFakeCommonDir creates the minimal layout git accepts as a common dir.
func mkFakeCommonDir(t *testing.T, dir string) {
	t.Helper()
	for _, sub := range []string{"objects", "refs"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// An in-project shared workspace: the workspace is the project root, so the
// repo root is the workspace (unchanged from before).
func TestDetectRepoRoot_SharedWorkspaceAtProjectRoot(t *testing.T) {
	ws := mkGitRepo(t, filepath.Join(t.TempDir(), "proj"))
	projectDir := filepath.Join(ws, config.DotScion)
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := mustDetectRepoRoot(t, false, ws, projectDir); eval(t, got) != eval(t, ws) {
		t.Fatalf("repo root = %q, want the workspace %q", got, ws)
	}
}

// A shared workspace whose .git/commondir names another project's clone is
// refused: no repo root is returned for the mount layout.
func TestDetectRepoRoot_SharedWorkspaceRefusesCommonDirOfOtherProject(t *testing.T) {
	base := t.TempDir()
	ws := mkGitRepo(t, filepath.Join(base, "projects", "a"))
	other := mkGitRepo(t, filepath.Join(base, "projects", "b"))
	projectDir := filepath.Join(ws, config.DotScion)
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	setCommonDir(t, ws, "../../b/.git", filepath.Join(other, ".git"))

	got, err := detectRepoRoot(false, ws, projectDir, nil)
	if err == nil {
		t.Fatalf("expected a refusal, got repo root %q", got)
	}
	if got != "" {
		t.Fatalf("repo root = %q on refusal, want \"\"", got)
	}
	if !strings.Contains(err.Error(), "outside the project") {
		t.Fatalf("error should say the common dir is outside the project: %v", err)
	}
}

// A shared workspace whose .git/commondir names a directory inside the
// workspace itself is refused too.
func TestDetectRepoRoot_SharedWorkspaceRefusesCommonDirInsideWorkspace(t *testing.T) {
	ws := mkGitRepo(t, filepath.Join(t.TempDir(), "proj"))
	projectDir := filepath.Join(ws, config.DotScion)
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(ws, "sub", "fake")
	mkFakeCommonDir(t, fake)
	setCommonDir(t, ws, "../sub/fake", fake)

	got, err := detectRepoRoot(false, ws, projectDir, nil)
	if err == nil || got != "" {
		t.Fatalf("expected a refusal with no repo root, got (%q, %v)", got, err)
	}
}

// A hub-native shared workspace: the project dir is the broker's
// project-configs dir and the workspace is the recorded workspace_path. The
// repo root is the workspace; a commondir pointing at a sibling project's
// clone is refused.
func TestDetectRepoRoot_HubNativeSharedWorkspace(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	projects := filepath.Join(home, config.GlobalDir, config.ProjectsDir)
	ws := mkGitRepo(t, filepath.Join(projects, "a"))
	other := mkGitRepo(t, filepath.Join(projects, "b"))
	projectDir := filepath.Join(home, config.GlobalDir, config.ProjectConfigsDir, "a__11111111", config.DotScion)
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if !isProjectConfigsPath(projectDir) {
		t.Fatalf("fixture: %s should be a project-configs path", projectDir)
	}
	settings := &config.VersionedSettings{WorkspacePath: ws}

	got, err := detectRepoRoot(false, ws, projectDir, settings)
	if err != nil || eval(t, got) != eval(t, ws) {
		t.Fatalf("detectRepoRoot = (%q, %v), want the workspace %q", got, err, ws)
	}

	setCommonDir(t, ws, "../../b/.git", filepath.Join(other, ".git"))
	if got, err := detectRepoRoot(false, ws, projectDir, settings); err == nil || got != "" {
		t.Fatalf("tampered commondir: detectRepoRoot = (%q, %v), want a refusal", got, err)
	}
}

// A worktree workspace of the project's own repository resolves to that
// repository; one whose .git file is redirected to another repository's
// worktree is refused.
func TestDetectRepoRoot_WorktreeWorkspace(t *testing.T) {
	base := t.TempDir()
	root := mkGitRepo(t, filepath.Join(base, "repo"))
	other := mkGitRepo(t, filepath.Join(base, "other"))
	projectDir := filepath.Join(root, config.DotScion)
	if err := os.MkdirAll(filepath.Join(projectDir, "agents", "dev"), 0o755); err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(projectDir, "agents", "dev", "workspace")
	runGitIn(t, root, "worktree", "add", "-b", "dev", ws)

	if got := mustDetectRepoRoot(t, false, ws, projectDir); eval(t, got) != eval(t, root) {
		t.Fatalf("repo root = %q, want the project repository %q", got, root)
	}

	otherWT := filepath.Join(base, "other-wt")
	runGitIn(t, other, "worktree", "add", "-b", "x", otherWT)
	pointer, err := os.ReadFile(filepath.Join(otherWT, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".git"), pointer, 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := detectRepoRoot(false, ws, projectDir, nil); err == nil || got != "" {
		t.Fatalf("redirected worktree: detectRepoRoot = (%q, %v), want a refusal", got, err)
	}
}

// A project in a subdirectory of a repository (a monorepo): its worktree
// workspace still resolves to the enclosing repository root, as before.
func TestDetectRepoRoot_WorktreeOfSubdirectoryProject(t *testing.T) {
	base := t.TempDir()
	root := mkGitRepo(t, filepath.Join(base, "mono"))
	projectDir := filepath.Join(root, "svc", config.DotScion)
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(base, "wt")
	runGitIn(t, root, "worktree", "add", "-b", "dev", ws)

	if got := mustDetectRepoRoot(t, false, ws, projectDir); eval(t, got) != eval(t, root) {
		t.Fatalf("repo root = %q, want the enclosing repository %q", got, root)
	}
}

// A plain checkout outside the project root (for example a workspace storage
// backend's host path) resolves to itself, as before.
func TestDetectRepoRoot_PlainCheckoutOutsideProjectRoot(t *testing.T) {
	base := t.TempDir()
	ws := mkGitRepo(t, filepath.Join(base, "backend", "workspace"))
	projectDir := filepath.Join(base, "project", config.DotScion)
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := mustDetectRepoRoot(t, false, ws, projectDir); eval(t, got) != eval(t, ws) {
		t.Fatalf("repo root = %q, want the workspace %q", got, ws)
	}

	other := mkGitRepo(t, filepath.Join(base, "other"))
	setCommonDir(t, ws, filepath.Join(other, ".git"), filepath.Join(other, ".git"))
	if got, err := detectRepoRoot(false, ws, projectDir, nil); err == nil || got != "" {
		t.Fatalf("tampered commondir: detectRepoRoot = (%q, %v), want a refusal", got, err)
	}
}

// The project-dir fallback (no workspace path): a repository whose
// core.worktree names another directory is refused instead of mounting it.
func TestDetectRepoRoot_ProjectDirFallbackRefusesForeignTopLevel(t *testing.T) {
	base := t.TempDir()
	root := mkGitRepo(t, filepath.Join(base, "repo"))
	elsewhere := filepath.Join(base, "elsewhere")
	if err := os.MkdirAll(elsewhere, 0o755); err != nil {
		t.Fatal(err)
	}
	projectDir := filepath.Join(root, config.DotScion)
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := mustDetectRepoRoot(t, false, "", projectDir); eval(t, got) != eval(t, root) {
		t.Fatalf("repo root = %q, want %q", got, root)
	}

	runGitIn(t, root, "config", "core.worktree", elsewhere)
	if top, _ := util.RepoRootDir(projectDir); eval(t, top) != eval(t, elsewhere) {
		t.Fatalf("fixture: git should report %s as the top level, got %s", elsewhere, top)
	}
	if got, err := detectRepoRoot(false, "", projectDir, nil); err == nil || got != "" {
		t.Fatalf("foreign top level: detectRepoRoot = (%q, %v), want a refusal", got, err)
	}
}

// hubNativeSharedStartFixture sets up HOME with a harness-config and default
// template, and a hub-native shared-workspace project "a" whose clone at
// ~/.scion/projects/a is a git repository, plus a sibling project clone "b".
// It returns the project's marker path and both clone paths.
func hubNativeSharedStartFixture(t *testing.T) (markerPath, ws, other string) {
	t.Helper()
	home := t.TempDir()
	oldWd, _ := os.Getwd()
	_ = os.Chdir(home)
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
	t.Setenv("HOME", home)
	// A container UID makes provisioning skip its git checks; keep them on.
	t.Setenv("SCION_HOST_UID", "")
	if old, ok := os.LookupEnv("SCION_AUTO_EXPOSE_PORTS"); ok {
		_ = os.Unsetenv("SCION_AUTO_EXPOSE_PORTS")
		t.Cleanup(func() { _ = os.Setenv("SCION_AUTO_EXPOSE_PORTS", old) })
	}

	globalScionDir := filepath.Join(home, config.GlobalDir)
	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
	if err := os.MkdirAll(hcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: generic\nuser: scion\nimage: layout-image:v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tplDir := filepath.Join(globalScionDir, "templates", "default")
	if err := os.MkdirAll(tplDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte("schema_version: \"1\"\nactive_profile: local\nprofiles:\n  local:\n    runtime: docker\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	markerPath, projectDir := makeHubNativeProject(t, home, "a", hubNativeTestProjectID)
	ws = filepath.Join(home, config.GlobalDir, config.ProjectsDir, "a")
	setupGitRepo(t, ws)
	other = mkGitRepo(t, filepath.Join(home, config.GlobalDir, config.ProjectsDir, "b"))
	if err := os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(fmt.Sprintf("schema_version: \"1\"\nworkspace_path: %q\n", ws)), 0o644); err != nil {
		t.Fatal(err)
	}
	return markerPath, ws, other
}

func hubNativeSharedStartOpts(markerPath string) api.StartOptions {
	return api.StartOptions{
		Name:            "dev",
		ProjectPath:     markerPath,
		SharedWorkspace: true,
		HubProjectID:    hubNativeTestProjectID,
		BrokerMode:      true,
		NoAuth:          true,
		Env: map[string]string{
			"SCION_AGENT_ID":   "agent-layout",
			"SCION_PROJECT_ID": hubNativeTestProjectID,
		},
	}
}

// Start of a hub-native shared-workspace agent: the runtime gets the
// workspace as both RepoRoot and Workspace (the shared layout), and after the
// workspace's .git/commondir is pointed at a sibling project's clone, or at a
// directory inside the workspace, the next start is refused before the
// runtime is called.
func TestStart_SharedWorkspaceLayoutFromBrokerPaths(t *testing.T) {
	markerPath, ws, other := hubNativeSharedStartFixture(t)

	var calls int
	var captured runtime.RunConfig
	mgr := NewManager(&runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
			calls++
			captured = cfg
			return "mock-id", nil
		},
	})

	createOpts := hubNativeSharedStartOpts(markerPath)
	createOpts.FreshProvision = true // a create dispatch
	if _, err := mgr.Start(context.Background(), createOpts); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if calls != 1 {
		t.Fatalf("runtime Run calls = %d, want 1", calls)
	}
	if eval(t, captured.RepoRoot) != eval(t, ws) || eval(t, captured.Workspace) != eval(t, ws) {
		t.Fatalf("RunConfig (RepoRoot %q, Workspace %q), want both %q", captured.RepoRoot, captured.Workspace, ws)
	}

	for _, tc := range []struct {
		name, commondir, want string
		setup                 func()
	}{
		{name: "sibling project", commondir: "../../b/.git", want: filepath.Join(other, ".git")},
		{name: "inside workspace", commondir: "../sub/fake", want: filepath.Join(ws, "sub", "fake"),
			setup: func() { mkFakeCommonDir(t, filepath.Join(ws, "sub", "fake")) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.setup != nil {
				tc.setup()
			}
			setCommonDir(t, ws, tc.commondir, tc.want)
			t.Cleanup(func() { _ = os.Remove(filepath.Join(ws, ".git", "commondir")) })

			calls = 0
			_, err := mgr.Start(context.Background(), hubNativeSharedStartOpts(markerPath))
			if err == nil {
				t.Fatal("Start succeeded with a redirected git common dir; want a refusal")
			}
			if !strings.Contains(err.Error(), "outside the project") {
				t.Fatalf("Start error should name the refused common dir: %v", err)
			}
			if calls != 0 {
				t.Fatalf("runtime Run was called %d time(s) on a refused start", calls)
			}
		})
	}
}

// A shared workspace whose .git was removed, inside an enclosing repository
// (a repository in a parent directory, such as a home directory kept in git):
// the result is a plain mount, never the enclosing repository.
func TestDetectRepoRoot_SharedWorkspaceWithoutGitNeverUsesAncestorRepo(t *testing.T) {
	ancestor := mkGitRepo(t, filepath.Join(t.TempDir(), "home"))
	ws := mkGitRepo(t, filepath.Join(ancestor, "projects", "a"))
	projectDir := filepath.Join(ws, config.DotScion)
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(ws, ".git")); err != nil {
		t.Fatal(err)
	}
	if !util.IsGitRepoDir(ws) {
		t.Fatal("fixture: git should find the enclosing repository from the workspace")
	}
	if got := mustDetectRepoRoot(t, false, ws, projectDir); got != "" {
		t.Fatalf("repo root = %q, want \"\" (plain mount), never the enclosing repository %q", got, ancestor)
	}
}

// The hub-native form of the same case: the project's clone lost its .git
// and HOME is itself a git repository.
func TestDetectRepoRoot_HubNativeSharedWorkspaceWithoutGitInGitHome(t *testing.T) {
	home := mkGitRepo(t, filepath.Join(t.TempDir(), "home"))
	t.Setenv("HOME", home)
	ws := mkGitRepo(t, filepath.Join(home, config.GlobalDir, config.ProjectsDir, "a"))
	projectDir := filepath.Join(home, config.GlobalDir, config.ProjectConfigsDir, "a__11111111", config.DotScion)
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(ws, ".git")); err != nil {
		t.Fatal(err)
	}
	got, err := detectRepoRoot(false, ws, projectDir, &config.VersionedSettings{WorkspacePath: ws})
	if err != nil || got != "" {
		t.Fatalf("detectRepoRoot = (%q, %v), want (\"\", nil): never the git HOME %q", got, err, home)
	}
}

// A shared project whose root is a subdirectory of a repository (no .git of
// its own) gets a plain mount: the broker has no record that tells it apart
// from a workspace whose .git was removed.
func TestDetectRepoRoot_SharedSubdirectoryProjectIsPlain(t *testing.T) {
	repo := mkGitRepo(t, filepath.Join(t.TempDir(), "mono"))
	ws := filepath.Join(repo, "svc")
	projectDir := filepath.Join(ws, config.DotScion)
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var logs strings.Builder
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(old)

	if got := mustDetectRepoRoot(t, false, ws, projectDir); got != "" {
		t.Fatalf("repo root = %q, want \"\" (plain mount)", got)
	}
	out := logs.String()
	for _, want := range []string{"Project root is a subdirectory of a repository", "in-container git is unavailable", "repository=" + eval(t, repo)} {
		if !strings.Contains(out, want) {
			t.Errorf("warning should contain %q, got: %s", want, out)
		}
	}
}

// brokerProjectRoot takes workspace_path only for a project-configs project
// dir; for any other project dir a workspace_path from merged settings is
// ignored.
func TestBrokerProjectRoot_WorkspacePathOnlyFromProjectConfigs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	settings := &config.VersionedSettings{WorkspacePath: filepath.Join(home, "elsewhere")}

	inRepo := filepath.Join(home, "proj", config.DotScion)
	if got := brokerProjectRoot(settings, inRepo); got != filepath.Join(home, "proj") {
		t.Errorf("in-repo .scion: got %q, want the directory holding .scion", got)
	}
	other := filepath.Join(home, "some-project-dir")
	if got := brokerProjectRoot(settings, other); got != other {
		t.Errorf("other project dir: got %q, want the project dir itself", got)
	}
	external := filepath.Join(home, config.GlobalDir, config.ProjectConfigsDir, "p__1", config.DotScion)
	if got := brokerProjectRoot(settings, external); got != settings.WorkspacePath {
		t.Errorf("project-configs dir: got %q, want workspace_path %q", got, settings.WorkspacePath)
	}
}

// Start of a hub-native shared-workspace agent whose clone lost its .git
// after HOME became a git repository: the restart never mounts HOME's
// repository. With a container UID (SCION_HOST_UID, a broker running in a
// container) provisioning's git checks are off and the restart gets a plain
// workspace mount; without one the workspace containment check refuses the
// restart before the runtime is called. Both values are set explicitly, so
// the result does not depend on the environment the tests run in.
func TestStart_SharedWorkspaceWithoutGitNeverMountsAncestorRepo(t *testing.T) {
	for _, tc := range []struct {
		name, hostUID string
		wantRefused   bool
	}{
		{name: "container UID", hostUID: "1000"},
		{name: "no container UID", hostUID: "", wantRefused: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			markerPath, ws, _ := hubNativeSharedStartFixture(t)
			t.Setenv("SCION_HOST_UID", tc.hostUID)

			var calls int
			var captured runtime.RunConfig
			mgr := NewManager(&runtime.MockRuntime{
				ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
					return []api.AgentInfo{}, nil
				},
				RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
					calls++
					captured = cfg
					return "mock-id", nil
				},
			})
			createOpts := hubNativeSharedStartOpts(markerPath)
			createOpts.FreshProvision = true
			if _, err := mgr.Start(context.Background(), createOpts); err != nil {
				t.Fatalf("Start: %v", err)
			}
			if eval(t, captured.RepoRoot) != eval(t, ws) {
				t.Fatalf("first start RepoRoot = %q, want the clone %q", captured.RepoRoot, ws)
			}

			// HOME becomes a git repository (ignoring the agents dirs, as
			// any project-local layout must) after the agent exists, and the
			// clone loses its .git.
			home := os.Getenv("HOME")
			if err := os.WriteFile(filepath.Join(home, ".gitignore"), []byte(".scion/project-configs/*/.scion/agents/\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			setupGitRepo(t, home)
			if err := os.RemoveAll(filepath.Join(ws, ".git")); err != nil {
				t.Fatal(err)
			}
			if !util.IsGitRepoDir(ws) {
				t.Fatal("fixture: git should find HOME's repository from the clone")
			}

			calls = 0
			captured = runtime.RunConfig{}
			_, err := mgr.Start(context.Background(), hubNativeSharedStartOpts(markerPath))
			if tc.wantRefused {
				if err == nil {
					t.Fatalf("restart succeeded with RepoRoot %q; want a refusal", captured.RepoRoot)
				}
				if !strings.Contains(err.Error(), "outside the permitted workspace root") {
					t.Fatalf("restart should be refused by the workspace containment check, got: %v", err)
				}
				if calls != 0 {
					t.Fatalf("runtime Run was called %d time(s) on a refused start", calls)
				}
				return
			}
			if err != nil {
				t.Fatalf("Start after .git removal: %v", err)
			}
			if captured.RepoRoot != "" {
				t.Fatalf("RunConfig.RepoRoot = %q after .git removal, want \"\" (plain mount), never HOME %q", captured.RepoRoot, home)
			}
			if eval(t, captured.Workspace) != eval(t, ws) {
				t.Fatalf("RunConfig.Workspace = %q, want %q", captured.Workspace, ws)
			}
		})
	}
}

// A shared workspace whose .git directory git does not accept (empty, or
// with a commondir naming a missing directory) inside an enclosing
// repository: a plain mount, never the enclosing repository.
func TestDetectRepoRoot_SharedWorkspaceWithUnusableGitDirIsPlain(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, ws string)
	}{
		{name: "empty .git dir", setup: func(t *testing.T, ws string) {
			if err := os.RemoveAll(filepath.Join(ws, ".git")); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(ws, ".git"), 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "commondir to a missing dir", setup: func(t *testing.T, ws string) {
			if err := os.WriteFile(filepath.Join(ws, ".git", "commondir"), []byte("/nonexistent-common-dir\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ancestor := mkGitRepo(t, filepath.Join(t.TempDir(), "home"))
			ws := mkGitRepo(t, filepath.Join(ancestor, "projects", "a"))
			projectDir := filepath.Join(ws, config.DotScion)
			if err := os.MkdirAll(projectDir, 0o755); err != nil {
				t.Fatal(err)
			}
			tc.setup(t, ws)
			if got := mustDetectRepoRoot(t, false, ws, projectDir); got != "" {
				t.Fatalf("repo root = %q, want \"\" (plain mount), never %q", got, ancestor)
			}
		})
	}
}

// A project checked out in a linked worktree of another checkout: its
// agents' worktrees share the main repository's common dir, and the repo
// root is the main repository, as before. Rewriting the commondir files in
// the main repository's worktree admin dirs (which a worktree layout mounts
// writable) to name another repository is refused: the project's common
// dir is taken from the path in the project checkout's own .git file.
func TestDetectRepoRoot_ProjectInLinkedWorktree(t *testing.T) {
	base := t.TempDir()
	main := mkGitRepo(t, filepath.Join(base, "main"))
	other := mkGitRepo(t, filepath.Join(base, "other"))
	wt := filepath.Join(base, "wt")
	runGitIn(t, main, "worktree", "add", "-b", "wt", wt)
	projectDir := filepath.Join(wt, config.DotScion)
	if err := os.MkdirAll(filepath.Join(projectDir, "agents", "dev"), 0o755); err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(projectDir, "agents", "dev", "workspace")
	runGitIn(t, wt, "worktree", "add", "-b", "dev", ws)

	if got := mustDetectRepoRoot(t, false, ws, projectDir); eval(t, got) != eval(t, main) {
		t.Fatalf("repo root = %q, want the main repository %q", got, main)
	}

	otherGit := eval(t, filepath.Join(other, ".git"))
	for _, checkout := range []string{wt, ws} {
		gitdir, ok := readGitdirPointer(filepath.Join(checkout, ".git"))
		if !ok {
			t.Fatalf("fixture: %s has no .git pointer", checkout)
		}
		if err := os.WriteFile(filepath.Join(gitdir, "commondir"), []byte(otherGit+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := util.GetCommonGitDir(ws); eval(t, got) != otherGit {
		t.Fatalf("fixture: git should report %s as the workspace's common dir, got %s", otherGit, got)
	}
	if got, err := detectRepoRoot(false, ws, projectDir, nil); err == nil || got != "" {
		t.Fatalf("rewritten commondir files: detectRepoRoot = (%q, %v), want a refusal", got, err)
	}
}

// A workspace at the project root whose .git is a file (a checkout that is
// itself a linked worktree) gets the shared layout: the root is the
// workspace, not the repository the file points into.
func TestDetectRepoRoot_ProjectRootWithGitFileIsShared(t *testing.T) {
	base := t.TempDir()
	main := mkGitRepo(t, filepath.Join(base, "main"))
	ws := filepath.Join(base, "wt")
	runGitIn(t, main, "worktree", "add", "-b", "wt", ws)
	projectDir := filepath.Join(ws, config.DotScion)
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := mustDetectRepoRoot(t, false, ws, projectDir); eval(t, got) != eval(t, ws) {
		t.Fatalf("repo root = %q, want the workspace %q", got, ws)
	}
}

// The workspace storage backend re-resolution: the backend's workspace on
// the export is a checkout whose commondir names another repository, so the
// start is refused there and the runtime is not called.
func TestStartNFSWorkspace_RefusesRedirectedCommonDir(t *testing.T) {
	mountRoot := filepath.Join(t.TempDir(), "nfs")
	wsPath := mkGitRepo(t, filepath.Join(mountRoot, "share-1", "projects", testNFSWorkspaceProjectID, "workspace"))
	other := mkGitRepo(t, filepath.Join(mountRoot, "share-1", "projects", "other", "workspace"))
	setCommonDir(t, wsPath, filepath.Join(other, ".git"), filepath.Join(other, ".git"))

	f := newSharedDirStorageRunFixture(t)
	f.writeGlobalSettings(t, fmt.Sprintf(nfsWorkspaceStartYAML, mountRoot))

	var calls int
	mgr := NewManager(&runtime.MockRuntime{
		NameFunc: func() string { return "kubernetes" },
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, rc runtime.RunConfig) (string, error) {
			calls++
			return "mock-id", nil
		},
	})
	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: f.projectScionDir,
		NoAuth:      true,
		Env: map[string]string{
			"SCION_AGENT_ID":   "agent-2157",
			"SCION_PROJECT_ID": testNFSWorkspaceProjectID,
		},
		GitClone: &api.GitCloneConfig{URL: "https://example.com/repo.git", Branch: "main"},
	})
	if err == nil || !strings.Contains(err.Error(), "outside the project") {
		t.Fatalf("Start error = %v, want a refusal naming the common dir", err)
	}
	if calls != 0 {
		t.Fatalf("runtime Run was called %d time(s) on a refused start", calls)
	}
}

// A plain (non-git) workspace outside the project's repository, under an
// enclosing repository (a git HOME): the git probe in the workspace is
// bounded at the workspace, so git never answers with the enclosing
// repository (which would be refused), and the workspace gets a plain mount.
func TestDetectRepoRoot_PlainWorkspaceUnderGitHomeIsPlain(t *testing.T) {
	home := mkGitRepo(t, filepath.Join(t.TempDir(), "home"))
	proj := mkGitRepo(t, filepath.Join(home, "proj"))
	projectDir := filepath.Join(proj, config.DotScion)
	ws := filepath.Join(home, "plain-ws")
	for _, d := range []string{projectDir, ws} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if !util.IsGitRepoDir(ws) {
		t.Fatal("fixture: an unbounded git search from the workspace should reach the git HOME")
	}
	got, err := detectRepoRoot(false, ws, projectDir, nil)
	if err != nil || got != "" {
		t.Fatalf("detectRepoRoot = (%q, %v), want (\"\", nil): a plain mount, never the git HOME %q", got, err, home)
	}
}

// A checkout outside the project's repository (for example a workspace
// storage backend's host path) whose own .git directory git rejects gets a
// plain mount; it is never widened to the project's repository.
func TestDetectRepoRoot_RejectedGitDirOutsideProjectRepoIsPlain(t *testing.T) {
	base := t.TempDir()
	root := mkGitRepo(t, filepath.Join(base, "repo"))
	projectDir := filepath.Join(root, config.DotScion)
	ws := filepath.Join(base, "backend", "workspace")
	for _, d := range []string{projectDir, filepath.Join(ws, ".git")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if got := mustDetectRepoRoot(t, false, ws, projectDir); got != "" {
		t.Fatalf("repo root = %q, want \"\" (plain mount), never the project repository %q", got, root)
	}
}

// slowGitIn puts a git first on PATH that blocks (until the probe deadline
// kills it) when its working directory is dir, and runs the real git
// everywhere else, and lowers the probe deadline. Only probes run in dir
// time out; probes in the broker-side project dir still complete.
func slowGitIn(t *testing.T, dir string) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("git not found: %v", err)
	}
	binDir := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\ncase \"$(pwd -P)\" in\n  %q) exec sleep 30 ;;\nesac\nexec %q \"$@\"\n", eval(t, dir), realGit)
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	orig := worktreeGitTimeout
	worktreeGitTimeout = 500 * time.Millisecond
	t.Cleanup(func() { worktreeGitTimeout = orig })
}

// A probe that does not finish in time in the workspace refuses the start,
// in every branch that consults the workspace's git metadata: it is never
// read as "not a repository", which would otherwise give the project's
// repository (for a checkout outside the project) or let a redirected
// worktree pointer through.
func TestDetectRepoRoot_WorkspaceProbeTimeoutRefuses(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, base, root string) (ws, projectDir string)
	}{
		{name: "checkout outside the project", setup: func(t *testing.T, base, root string) (string, string) {
			return mkGitRepo(t, filepath.Join(base, "backend", "workspace")), filepath.Join(root, config.DotScion)
		}},
		{name: "redirected worktree pointer", setup: func(t *testing.T, base, root string) (string, string) {
			other := mkGitRepo(t, filepath.Join(base, "other"))
			otherWT := filepath.Join(base, "other-wt")
			runGitIn(t, other, "worktree", "add", "-b", "x", otherWT)
			projectDir := filepath.Join(root, config.DotScion)
			ws := filepath.Join(projectDir, "agents", "dev", "workspace")
			if err := os.MkdirAll(filepath.Dir(ws), 0o755); err != nil {
				t.Fatal(err)
			}
			runGitIn(t, root, "worktree", "add", "-b", "dev", ws)
			pointer, err := os.ReadFile(filepath.Join(otherWT, ".git"))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(ws, ".git"), pointer, 0o644); err != nil {
				t.Fatal(err)
			}
			return ws, projectDir
		}},
		{name: "shared workspace at the project root", setup: func(t *testing.T, base, root string) (string, string) {
			return root, filepath.Join(root, config.DotScion)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			root := mkGitRepo(t, filepath.Join(base, "repo"))
			ws, projectDir := tc.setup(t, base, root)
			if err := os.MkdirAll(projectDir, 0o755); err != nil {
				t.Fatal(err)
			}
			slowGitIn(t, ws)
			got, err := detectRepoRoot(false, ws, projectDir, nil)
			if err == nil || got != "" {
				t.Fatalf("detectRepoRoot = (%q, %v), want a refusal", got, err)
			}
			if !errors.Is(err, errGitProbeTimeout) {
				t.Fatalf("refusal should name the probe timeout: %v", err)
			}
		})
	}
}

// A checkout nested inside the project's repository whose own .git
// directory git rejects gets a plain mount: git's search past the rejected
// .git (which would reach the project's repository) is not used.
func TestDetectRepoRoot_RejectedGitDirInsideProjectRepoIsPlain(t *testing.T) {
	root := mkGitRepo(t, filepath.Join(t.TempDir(), "repo"))
	projectDir := filepath.Join(root, config.DotScion)
	ws := filepath.Join(root, "vendor", "nested")
	for _, d := range []string{projectDir, filepath.Join(ws, ".git")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if got := mustDetectRepoRoot(t, false, ws, projectDir); got != "" {
		t.Fatalf("repo root = %q, want \"\" (plain mount), never the project repository %q", got, root)
	}
}

// Through Start: a workspace storage backend checkout outside the project
// whose git probe does not finish in time refuses the start; the runtime is
// not called (it would otherwise get the project's repository).
func TestStartNFSWorkspace_ProbeTimeoutRefuses(t *testing.T) {
	mountRoot := filepath.Join(t.TempDir(), "nfs")
	wsPath := mkGitRepo(t, filepath.Join(mountRoot, "share-1", "projects", testNFSWorkspaceProjectID, "workspace"))

	f := newSharedDirStorageRunFixture(t)
	f.writeGlobalSettings(t, fmt.Sprintf(nfsWorkspaceStartYAML, mountRoot))

	var calls int
	mgr := NewManager(&runtime.MockRuntime{
		NameFunc: func() string { return "kubernetes" },
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, rc runtime.RunConfig) (string, error) {
			calls++
			return "mock-id", nil
		},
	})
	slowGitIn(t, wsPath)
	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: f.projectScionDir,
		NoAuth:      true,
		Env: map[string]string{
			"SCION_AGENT_ID":   "agent-2157",
			"SCION_PROJECT_ID": testNFSWorkspaceProjectID,
		},
		GitClone: &api.GitCloneConfig{URL: "https://example.com/repo.git", Branch: "main"},
	})
	if err == nil || !errors.Is(err, errGitProbeTimeout) {
		t.Fatalf("Start error = %v, want a refusal naming the probe timeout", err)
	}
	if calls != 0 {
		t.Fatalf("runtime Run was called %d time(s) on a refused start", calls)
	}
}

// Through Start: a worktree-per-agent agent of a local git project whose
// worktree .git pointer was redirected to another repository, and whose git
// probe then does not finish in time, is refused on restart; the runtime is
// not called.
func TestStart_RedirectedWorktreeProbeTimeoutRefuses(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	home := t.TempDir()
	oldWd, _ := os.Getwd()
	_ = os.Chdir(home)
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
	t.Setenv("HOME", home)
	if old, ok := os.LookupEnv("SCION_AUTO_EXPOSE_PORTS"); ok {
		_ = os.Unsetenv("SCION_AUTO_EXPOSE_PORTS")
		t.Cleanup(func() { _ = os.Setenv("SCION_AUTO_EXPOSE_PORTS", old) })
	}
	globalScionDir := filepath.Join(home, config.GlobalDir)
	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
	tplDir := filepath.Join(globalScionDir, "templates", "default")
	for _, d := range []string{hcDir, tplDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for path, content := range map[string]string{
		filepath.Join(hcDir, "config.yaml"):            "harness: generic\nuser: scion\nimage: layout-image:v1\n",
		filepath.Join(tplDir, "scion-agent.json"):      `{"default_harness_config": "test-harness"}`,
		filepath.Join(globalScionDir, "settings.yaml"): "schema_version: \"1\"\nactive_profile: local\nprofiles:\n  local:\n    runtime: docker\n",
	} {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	proj := filepath.Join(home, "proj")
	if err := os.MkdirAll(filepath.Join(proj, config.DotScion), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, ".gitignore"), []byte("agents/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	setupGitRepo(t, proj)
	other := mkGitRepo(t, filepath.Join(home, "other"))
	otherWT := filepath.Join(home, "other-wt")
	runGitIn(t, other, "worktree", "add", "-b", "x", otherWT)

	var calls int
	var captured runtime.RunConfig
	mgr := NewManager(&runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
			calls++
			captured = cfg
			return "mock-id", nil
		},
	})
	opts := api.StartOptions{Name: "dev", ProjectPath: filepath.Join(proj, config.DotScion), NoAuth: true}
	if _, err := mgr.Start(context.Background(), opts); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if calls != 1 || eval(t, captured.RepoRoot) != eval(t, proj) {
		t.Fatalf("first start: calls %d, RepoRoot %q, want 1 and the project repository %q", calls, captured.RepoRoot, proj)
	}
	ws := captured.Workspace

	pointer, err := os.ReadFile(filepath.Join(otherWT, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".git"), pointer, 0o644); err != nil {
		t.Fatal(err)
	}
	slowGitIn(t, ws)
	calls = 0
	_, err = mgr.Start(context.Background(), opts)
	if err == nil || !errors.Is(err, errGitProbeTimeout) {
		t.Fatalf("restart error = %v, want a refusal naming the probe timeout", err)
	}
	if calls != 0 {
		t.Fatalf("runtime Run was called %d time(s) on a refused start", calls)
	}
}
