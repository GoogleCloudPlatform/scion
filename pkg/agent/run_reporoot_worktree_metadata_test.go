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
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// repoRootMetadataFixture is a git project whose agent was provisioned by
// Start (no broker repo-root signal on ctx), plus a second, unrelated
// repository on the same host.
type repoRootMetadataFixture struct {
	projectDir      string
	projectScionDir string
	agentName       string
	workspace       string // the agent's worktree, as RunConfig.Workspace reported it
	otherRepo       string
	mgr             Manager
	captured        *runtime.RunConfig
}

func newRepoRootMetadataFixture(t *testing.T) *repoRootMetadataFixture {
	t.Helper()
	t.Setenv("SCION_HOST_UID", "")

	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	t.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
	mustMkdir(t, hcDir)
	mustWrite(t, filepath.Join(hcDir, "config.yaml"), "harness: gemini\nuser: scion\nimage: test-image:latest\n")
	tplDir := filepath.Join(globalScionDir, "templates", "default")
	mustMkdir(t, tplDir)
	mustWrite(t, filepath.Join(tplDir, "scion-agent.json"), `{"default_harness_config": "test-harness"}`)
	mustWrite(t, filepath.Join(globalScionDir, "settings.yaml"), "schema_version: \"1\"\nactive_profile: local\nprofiles:\n  local:\n    runtime: docker\n")

	f := &repoRootMetadataFixture{agentName: "meta-agent", captured: &runtime.RunConfig{}}
	f.projectDir = filepath.Join(tmpDir, "project")
	setupTestGitRepoWithBranch(t, f.projectDir, "unused-branch")
	f.projectScionDir = filepath.Join(f.projectDir, ".scion")
	mustMkdir(t, f.projectScionDir)
	mustWrite(t, filepath.Join(f.projectDir, ".gitignore"), ".scion/agents/\n")

	f.otherRepo = filepath.Join(tmpDir, "other-repo")
	setupTestGitRepoWithBranch(t, f.otherRepo, "unused-branch")

	f.mgr = NewManager(&runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
			*f.captured = cfg
			return "mock-id", nil
		},
	})

	// First start: ProvisionAgent creates the agent's worktree. There is no
	// broker repo-root signal; the root is recorded at provision.
	cfg := f.start(t)
	f.workspace = cfg.Workspace
	t.Logf("first start: RepoRoot=%q Workspace=%q", cfg.RepoRoot, cfg.Workspace)
	if got, want := cfg.RepoRoot, eval(t, f.projectDir); got != want {
		t.Fatalf("first start: RepoRoot = %q, want the project repository %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(f.workspace, ".git")); err != nil {
		t.Fatalf("first start: workspace %q is not a worktree: %v", f.workspace, err)
	}
	agentDir := config.GetAgentDir(f.projectScionDir, f.agentName, false)
	rec, ok := readRepoRootRecord(agentDir)
	if !ok || rec.Source != repoRootSourceProvision || rec.RepoRoot != eval(t, f.projectDir) {
		t.Fatalf("first start: repository root record = %+v (ok=%v), want a provision record of %q", rec, ok, eval(t, f.projectDir))
	}
	return f
}

func (f *repoRootMetadataFixture) start(t *testing.T) runtime.RunConfig {
	t.Helper()
	if err := f.startErr(t); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return *f.captured
}

// startErr starts the agent and returns the error (if any) instead of failing
// the test, for cases that must fail closed.
func (f *repoRootMetadataFixture) startErr(t *testing.T) error {
	t.Helper()
	*f.captured = runtime.RunConfig{}
	_, err := f.mgr.Start(context.Background(), api.StartOptions{
		Name:        f.agentName,
		ProjectPath: f.projectScionDir,
		NoAuth:      true,
	})
	return err
}

// adminDir returns the worktree's admin directory in the project repository,
// read from the worktree's .git pointer as written by git worktree add.
func (f *repoRootMetadataFixture) adminDir(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.workspace, ".git"))
	if err != nil {
		t.Fatalf("read worktree pointer: %v", err)
	}
	gitdir := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(data)), "gitdir:"))
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(f.workspace, gitdir)
	}
	return filepath.Clean(gitdir)
}

// TestStart_RepoRootFollowsOnlyTheProjectRepository checks what a restart does
// after the worktree's own metadata files (the .git pointer in the workspace,
// or commondir in its admin directory) are changed to name a different
// repository. The recorded root no longer validates against the workspace, so
// the derivation falls to detectRepoRoot, which finds the worktree's git common
// dir pointing outside the project's repository and refuses. The start must
// fail closed and must never mount the other repository. (Before the root was
// recorded at provision, the restart derived it from the workspace and got the
// other repository.)
func TestStart_RepoRootFollowsOnlyTheProjectRepository(t *testing.T) {
	cases := []struct {
		name string
		edit func(t *testing.T, f *repoRootMetadataFixture)
	}{
		{
			name: "pointer file names another repository",
			edit: func(t *testing.T, f *repoRootMetadataFixture) {
				mustWrite(t, filepath.Join(f.workspace, ".git"), "gitdir: "+filepath.Join(f.otherRepo, ".git")+"\n")
			},
		},
		{
			name: "commondir names another repository",
			edit: func(t *testing.T, f *repoRootMetadataFixture) {
				mustWrite(t, filepath.Join(f.adminDir(t), "commondir"), filepath.Join(f.otherRepo, ".git")+"\n")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRepoRootMetadataFixture(t)
			tc.edit(t, f)

			err := f.startErr(t)
			if err == nil {
				t.Fatalf("restart: want a refusal after the worktree metadata was changed to name another repository; got RepoRoot=%q", f.captured.RepoRoot)
			}
			// detectRepoRoot refuses because the worktree's git common dir now
			// points outside the project's own repository.
			if !strings.Contains(err.Error(), "outside the project's own repository") {
				t.Errorf("restart refused for an unexpected reason: %v", err)
			}
		})
	}
}
