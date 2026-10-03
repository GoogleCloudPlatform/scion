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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNFSWorktreeSelection(t *testing.T) {
	git := &api.GitCloneConfig{URL: "https://example.com/repo.git"}
	wt := map[string]string{"SCION_WORKSPACE_MODE": "worktree-per-agent"}
	cases := []struct {
		name       string
		env        map[string]string
		gitClone   *api.GitCloneConfig
		agentName  string
		wantName   string
		wantBranch string
	}{
		{name: "worktree mode", env: wt, gitClone: git, agentName: "my-agent", wantName: "my-agent", wantBranch: "my-agent"},
		{name: "explicit branch", env: map[string]string{"SCION_WORKSPACE_MODE": "worktree-per-agent", "SCION_AGENT_BRANCH": "feature/x"}, gitClone: git, agentName: "my-agent", wantName: "my-agent", wantBranch: "feature/x"},
		{name: "clone-per-agent", env: map[string]string{"SCION_WORKSPACE_MODE": "clone-per-agent"}, gitClone: git, agentName: "my-agent"},
		{name: "per-agent label", env: map[string]string{"SCION_WORKSPACE_MODE": "per-agent"}, gitClone: git, agentName: "my-agent"},
		{name: "shared-plain", env: map[string]string{"SCION_WORKSPACE_MODE": "shared-plain"}, gitClone: git, agentName: "my-agent"},
		{name: "no mode", env: map[string]string{}, gitClone: git, agentName: "my-agent"},
		{name: "non-git", env: wt, agentName: "my-agent"},
		{name: "empty clone URL", env: wt, gitClone: &api.GitCloneConfig{}, agentName: "my-agent"},
		{name: "nil env", gitClone: git, agentName: "my-agent"},
		// The name must be an agent slug before it is used in a path.
		{name: "empty name", env: wt, gitClone: git, agentName: ""},
		{name: "name with separator", env: wt, gitClone: git, agentName: "a/b"},
		{name: "name dot-dot", env: wt, gitClone: git, agentName: ".."},
		{name: "name not a slug", env: wt, gitClone: git, agentName: "My_Agent"},
		{name: "name with dot", env: wt, gitClone: git, agentName: "a.b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name, branch := nfsWorktreeSelection(tc.env, tc.gitClone, tc.agentName)
			assert.Equal(t, tc.wantName, name)
			assert.Equal(t, tc.wantBranch, branch)
		})
	}
}

// startNFSWorktreeAgent runs Manager.Start for a git project on the NFS
// workspace backend with the given workspace mode, and reports the
// RunConfig and whether the agent's worktree directory existed when the
// runtime was asked to create the pod.
func startNFSWorktreeAgent(t *testing.T, runtimeName, mountRoot, mode string) (cfg runtime.RunConfig, worktreeDirAtRun bool, err error) {
	t.Helper()
	f := newSharedDirStorageRunFixture(t)
	f.writeGlobalSettings(t, fmt.Sprintf(nfsWorkspaceStartYAML, mountRoot))
	wtPath := filepath.Join(mountRoot, "share-1", "projects", testNFSWorkspaceProjectID, "workspace", "worktrees", "test-agent")
	mockRT := &runtime.MockRuntime{
		NameFunc: func() string { return runtimeName },
		RunFunc: func(ctx context.Context, rc runtime.RunConfig) (string, error) {
			cfg = rc
			info, statErr := os.Stat(wtPath)
			worktreeDirAtRun = statErr == nil && info.IsDir()
			return "mock-id", nil
		},
	}
	env := map[string]string{
		"SCION_PROJECT_ID": testNFSWorkspaceProjectID,
	}
	if mode != "" {
		env["SCION_WORKSPACE_MODE"] = mode
	}
	_, err = NewManager(mockRT).Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: f.projectScionDir,
		NoAuth:      true,
		Env:         env,
		GitClone:    &api.GitCloneConfig{URL: "https://example.com/repo.git"},
	})
	return cfg, worktreeDirAtRun, err
}

func nfsTestWorkspaceDir(mountRoot string) string {
	return filepath.Join(mountRoot, "share-1", "projects", testNFSWorkspaceProjectID, "workspace")
}

// First start of a worktree-per-agent project: the shared checkout's
// directory is created, the agent's worktree directory is not (the clone
// needs an empty directory; the init container adds the worktree), and the
// pod is told to mount the agent's worktree.
func TestStartNFSWorktree_FirstStart(t *testing.T) {
	mountRoot := filepath.Join(t.TempDir(), "nfs")
	require.NoError(t, os.MkdirAll(filepath.Join(mountRoot, "share-1"), 0o755))

	cfg, wtAtRun, err := startNFSWorktreeAgent(t, "kubernetes", mountRoot, "worktree-per-agent")
	require.NoError(t, err)
	assert.False(t, wtAtRun, "the worktree directory must not exist before the first clone")
	assert.Equal(t, "test-agent", cfg.NFSWorktreeName)
	assert.Equal(t, "test-agent", cfg.NFSWorktreeBranch)
	assert.Equal(t, "projects/"+testNFSWorkspaceProjectID+"/workspace", cfg.NFSSubPath, "the init container provisions the shared checkout")
	assert.Equal(t, "/repo-root/worktrees/test-agent", cfg.ContainerWorkspace)
	assert.True(t, cfg.NFSWorkspacePreCreated)
	entries, err := os.ReadDir(nfsTestWorkspaceDir(mountRoot))
	require.NoError(t, err)
	assert.Empty(t, entries, "the shared checkout's directory must stay empty for the clone")
}

// Later starts (shared checkout provisioned): the agent's worktree
// directory is created before the pod, so the pod can mount it.
func TestStartNFSWorktree_LaterStartCreatesWorktreeDir(t *testing.T) {
	mountRoot := filepath.Join(t.TempDir(), "nfs")
	ws := nfsTestWorkspaceDir(mountRoot)
	require.NoError(t, os.MkdirAll(ws, 0o770))
	require.NoError(t, os.WriteFile(filepath.Join(ws, provision.ProvisionSentinelFile), []byte("x"), 0o644))

	cfg, wtAtRun, err := startNFSWorktreeAgent(t, "kubernetes", mountRoot, "worktree-per-agent")
	require.NoError(t, err)
	assert.True(t, wtAtRun, "the worktree directory must exist when the pod is created")
	assert.Equal(t, "test-agent", cfg.NFSWorktreeName)
	entries, err := os.ReadDir(filepath.Join(ws, "worktrees", "test-agent"))
	require.NoError(t, err)
	assert.Empty(t, entries)
}

// shared-plain on NFS is unchanged: every agent mounts
// projects/<project ID>/workspace at /workspace, and no worktree directory
// is created. clone-per-agent creates no worktree directory either (see
// TestStartNFSAgentDir_OtherModesUnchanged for its own layout).
func TestStartNFSWorktree_OtherModesUnchanged(t *testing.T) {
	for _, mode := range []string{"shared-plain", ""} {
		t.Run("mode="+mode, func(t *testing.T) {
			mountRoot := filepath.Join(t.TempDir(), "nfs")
			ws := nfsTestWorkspaceDir(mountRoot)
			require.NoError(t, os.MkdirAll(ws, 0o770))
			require.NoError(t, os.WriteFile(filepath.Join(ws, provision.ProvisionSentinelFile), []byte("x"), 0o644))

			cfg, wtAtRun, err := startNFSWorktreeAgent(t, "kubernetes", mountRoot, mode)
			require.NoError(t, err)
			assert.False(t, wtAtRun)
			assert.Empty(t, cfg.NFSWorktreeName)
			assert.Empty(t, cfg.NFSWorktreeBranch)
			assert.Equal(t, "projects/"+testNFSWorkspaceProjectID+"/workspace", cfg.NFSSubPath)
			assert.Equal(t, "/workspace", cfg.ContainerWorkspace)
			_, statErr := os.Stat(filepath.Join(ws, "worktrees"))
			assert.True(t, os.IsNotExist(statErr), "no worktrees directory for mode %q", mode)
		})
	}
}

// Other runtimes on the NFS backend are unaffected by worktree mode.
func TestStartNFSWorktree_NonKubernetesUnaffected(t *testing.T) {
	mountRoot := filepath.Join(t.TempDir(), "nfs")
	ws := nfsTestWorkspaceDir(mountRoot)
	require.NoError(t, os.MkdirAll(ws, 0o770))
	require.NoError(t, os.WriteFile(filepath.Join(ws, provision.ProvisionSentinelFile), []byte("x"), 0o644))

	cfg, wtAtRun, err := startNFSWorktreeAgent(t, "docker", mountRoot, "worktree-per-agent")
	require.NoError(t, err)
	assert.False(t, wtAtRun)
	assert.Empty(t, cfg.NFSWorktreeName)
	assert.NotEqual(t, "/repo-root/worktrees/test-agent", cfg.ContainerWorkspace)
}

// ensureNFSWorktreeLeaf: no-op outside Kubernetes/NFS, nothing created
// before the shared checkout is provisioned, the directory created after,
// and an existing directory left alone.
func TestEnsureNFSWorktreeLeaf(t *testing.T) {
	mountRoot := filepath.Join(t.TempDir(), "nfs")
	resolved := resolveTestNFSWorkspace(t, mountRoot)
	wt := filepath.Join(resolved.HostPath, "worktrees", "agent-1")

	ok, err := ensureNFSWorktreeLeaf("docker", resolved, "ws-pv", "agent-1")
	require.NoError(t, err)
	assert.False(t, ok)

	// Export not mounted on the broker.
	ok, err = ensureNFSWorktreeLeaf("kubernetes", resolved, "ws-pv", "agent-1")
	require.NoError(t, err)
	assert.False(t, ok)

	require.NoError(t, os.MkdirAll(resolved.HostPath, 0o770))
	ok, err = ensureNFSWorktreeLeaf("kubernetes", resolved, "ws-pv", "agent-1")
	require.NoError(t, err)
	assert.True(t, ok)
	_, statErr := os.Stat(filepath.Join(resolved.HostPath, "worktrees"))
	assert.True(t, os.IsNotExist(statErr), "nothing is created before the shared checkout is provisioned")

	require.NoError(t, os.WriteFile(filepath.Join(resolved.HostPath, provision.ProvisionSentinelFile), []byte("x"), 0o644))
	ok, err = ensureNFSWorktreeLeaf("kubernetes", resolved, "ws-pv", "agent-1")
	require.NoError(t, err)
	assert.True(t, ok)
	info, err := os.Stat(wt)
	require.NoError(t, err)
	assert.True(t, info.IsDir())

	marker := filepath.Join(wt, "work.txt")
	require.NoError(t, os.WriteFile(marker, []byte("x"), 0o644))
	_, err = ensureNFSWorktreeLeaf("kubernetes", resolved, "ws-pv", "agent-1")
	require.NoError(t, err)
	_, err = os.Stat(marker)
	assert.NoError(t, err, "an existing directory is left alone")

	for _, bad := range []string{"../x", "a/b", "Agent-1", ""} {
		_, err = ensureNFSWorktreeLeaf("kubernetes", resolved, "ws-pv", bad)
		assert.Error(t, err, "name %q", bad)
	}
}

// nfsWorktreeGit runs git in dir for the removal tests.
func nfsWorktreeGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "init.defaultBranch=main"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
}

// setupNFSWorktreeRemoval writes NFS workspace settings for a project
// whose shared checkout, on the broker's mount of the export, has
// worktrees for test-agent and other-agent, as the init containers add
// them. It returns the fixture and the workspace directory.
func setupNFSWorktreeRemoval(t *testing.T) (sharedDirStorageRunFixture, string) {
	t.Helper()
	f := newSharedDirStorageRunFixture(t)
	mountRoot := filepath.Join(t.TempDir(), "nfs")
	f.writeGlobalSettings(t, fmt.Sprintf(nfsWorkspaceStartYAML, mountRoot))

	origin := filepath.Join(t.TempDir(), "origin")
	require.NoError(t, os.MkdirAll(origin, 0o755))
	nfsWorktreeGit(t, origin, "init")
	require.NoError(t, os.WriteFile(filepath.Join(origin, "README.md"), []byte("hi\n"), 0o644))
	nfsWorktreeGit(t, origin, "add", ".")
	nfsWorktreeGit(t, origin, "commit", "-m", "init")

	ws := nfsTestWorkspaceDir(mountRoot)
	for _, agent := range []string{"test-agent", "other-agent"} {
		require.NoError(t, provision.ProvisionShared(provision.ProvisionInput{
			Resolved:        provision.ResolvedWorkspace{HostPath: ws},
			ProjectID:       testNFSWorkspaceProjectID,
			AgentID:         agent,
			Mode:            "worktree-per-agent",
			GitClone:        &api.GitCloneConfig{URL: origin},
			NFSUID:          os.Getuid(),
			NFSGID:          os.Getgid(),
			SentinelDir:     ws,
			MountedWorktree: true,
		}))
		require.True(t, provision.IsRealWorktreeDir(provision.WorktreePath(ws, agent), ws))
	}
	return f, ws
}

func kubernetesTestManager(name string) *AgentManager {
	return NewManager(&runtime.MockRuntime{NameFunc: func() string { return name }}).(*AgentManager)
}

// runPurgeInline makes RemoveNFSWorktree delete the moved-aside files
// before it returns, and records the directories it purged.
func runPurgeInline(t *testing.T) *[]string {
	t.Helper()
	var purged []string
	orig := purgeRemovedWorktrees
	purgeRemovedWorktrees = func(base string) {
		purged = append(purged, base)
		require.NoError(t, provision.PurgeRemovedWorktrees(base))
	}
	t.Cleanup(func() { purgeRemovedWorktrees = orig })
	return &purged
}

// Delete with files on Kubernetes removes the agent's worktree from the
// export, files included, and nothing else.
func TestRemoveNFSWorktree_RemovesAgentWorktree(t *testing.T) {
	f, ws := setupNFSWorktreeRemoval(t)
	purged := runPurgeInline(t)
	path, err := kubernetesTestManager("kubernetes").RemoveNFSWorktree(context.Background(), f.projectScionDir, testNFSWorkspaceProjectID, "test-agent")
	require.NoError(t, err)
	assert.Equal(t, provision.WorktreePath(ws, "test-agent"), path)
	assert.NoDirExists(t, path)
	assert.True(t, provision.IsRealWorktreeDir(provision.WorktreePath(ws, "other-agent"), ws))
	assert.FileExists(t, filepath.Join(ws, "README.md"))
	assert.Equal(t, []string{ws}, *purged)
	entries, err := os.ReadDir(filepath.Join(ws, "worktrees"))
	require.NoError(t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	assert.Equal(t, []string{"other-agent"}, names, "the removed worktree's files are deleted")

	// Again: nothing left to remove.
	_, err = kubernetesTestManager("kubernetes").RemoveNFSWorktree(context.Background(), f.projectScionDir, testNFSWorkspaceProjectID, "test-agent")
	require.NoError(t, err)
}

// Other runtimes, and an export that is not mounted on the broker, are
// left alone.
func TestRemoveNFSWorktree_NoOpCases(t *testing.T) {
	f, ws := setupNFSWorktreeRemoval(t)
	_, err := kubernetesTestManager("docker").RemoveNFSWorktree(context.Background(), f.projectScionDir, testNFSWorkspaceProjectID, "test-agent")
	require.NoError(t, err)
	assert.True(t, provision.IsRealWorktreeDir(provision.WorktreePath(ws, "test-agent"), ws))

	f.writeGlobalSettings(t, fmt.Sprintf(nfsWorkspaceStartYAML, filepath.Join(t.TempDir(), "not-mounted")))
	_, err = kubernetesTestManager("kubernetes").RemoveNFSWorktree(context.Background(), f.projectScionDir, testNFSWorkspaceProjectID, "test-agent")
	require.NoError(t, err)
	assert.True(t, provision.IsRealWorktreeDir(provision.WorktreePath(ws, "test-agent"), ws))
}

// The agent name and project ID are checked before any path is built.
func TestRemoveNFSWorktree_RejectsBadNames(t *testing.T) {
	f, ws := setupNFSWorktreeRemoval(t)
	m := kubernetesTestManager("kubernetes")
	for _, name := range []string{"", "..", "../other-agent", "a/b", "Test-Agent", "test-agent/.", "a.b"} {
		_, err := m.RemoveNFSWorktree(context.Background(), f.projectScionDir, testNFSWorkspaceProjectID, name)
		assert.ErrorContains(t, err, "invalid agent name", "name %q", name)
	}
	for _, projectID := range []string{"", "..", "../" + testNFSWorkspaceProjectID, "a/b"} {
		_, err := m.RemoveNFSWorktree(context.Background(), f.projectScionDir, projectID, "test-agent")
		assert.ErrorContains(t, err, "invalid project ID", "project ID %q", projectID)
	}
	assert.True(t, provision.IsRealWorktreeDir(provision.WorktreePath(ws, "test-agent"), ws))
	assert.True(t, provision.IsRealWorktreeDir(provision.WorktreePath(ws, "other-agent"), ws))
}

// A symlinked worktrees/ directory on the export is refused, and what it
// points to is untouched.
func TestRemoveNFSWorktree_RefusesSymlinkedWorktreesDir(t *testing.T) {
	f, ws := setupNFSWorktreeRemoval(t)
	elsewhere := filepath.Join(t.TempDir(), "elsewhere")
	require.NoError(t, os.Rename(filepath.Join(ws, "worktrees"), elsewhere))
	require.NoError(t, os.Symlink(elsewhere, filepath.Join(ws, "worktrees")))

	path, err := kubernetesTestManager("kubernetes").RemoveNFSWorktree(context.Background(), f.projectScionDir, testNFSWorkspaceProjectID, "test-agent")
	assert.ErrorContains(t, err, "is not a plain directory on the export")
	assert.Equal(t, provision.WorktreePath(ws, "test-agent"), path)
	assert.DirExists(t, filepath.Join(elsewhere, "test-agent"))
}

// A symlinked workspace directory on the export is refused as well, even
// though the worktrees/ directory it leads to is a plain directory.
func TestRemoveNFSWorktree_RefusesSymlinkedWorkspaceDir(t *testing.T) {
	f, ws := setupNFSWorktreeRemoval(t)
	elsewhere := filepath.Join(t.TempDir(), "elsewhere")
	require.NoError(t, os.Rename(ws, elsewhere))
	require.NoError(t, os.Symlink(elsewhere, ws))

	_, err := kubernetesTestManager("kubernetes").RemoveNFSWorktree(context.Background(), f.projectScionDir, testNFSWorkspaceProjectID, "test-agent")
	assert.ErrorContains(t, err, "is not a plain directory on the export")
	assert.True(t, provision.IsRealWorktreeDir(filepath.Join(elsewhere, "worktrees", "test-agent"), elsewhere))
}

// offExportBackend resolves the workspace as the real backend does, then
// applies edit to the result.
type offExportBackend struct {
	runtime.WorkspaceBackend
	edit func(*runtime.ResolvedWorkspace)
}

func (b offExportBackend) Resolve(in runtime.ResolveInput) (runtime.ResolvedWorkspace, error) {
	r, err := b.WorkspaceBackend.Resolve(in)
	b.edit(&r)
	return r, err
}

// A resolved workspace path that does not lie under the export mount is
// refused before anything is touched, including a worktree at that path.
func TestRemoveNFSWorktree_RefusesPathOutsideExport(t *testing.T) {
	f, ws := setupNFSWorktreeRemoval(t)
	outside := filepath.Join(t.TempDir(), "outside")
	require.NoError(t, os.MkdirAll(filepath.Join(outside, "worktrees", "test-agent"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(outside, "worktrees", "test-agent", "keep"), []byte("x"), 0o644))

	cases := map[string]func(*runtime.ResolvedWorkspace){
		"relative path leaves the export": func(r *runtime.ResolvedWorkspace) {
			rel, err := filepath.Rel(r.HostBase, outside)
			require.NoError(t, err)
			r.ServerRelativePath = rel
			r.HostPath = filepath.Join(r.HostBase, rel)
		},
		"host path differs from the export path": func(r *runtime.ResolvedWorkspace) {
			r.HostPath = outside
		},
		"empty relative path": func(r *runtime.ResolvedWorkspace) {
			r.ServerRelativePath = ""
			r.HostPath = r.HostBase
		},
	}
	orig := nfsWorktreeBackend
	t.Cleanup(func() { nfsWorktreeBackend = orig })
	for name, edit := range cases {
		nfsWorktreeBackend = func(cfg *config.V1WorkspaceStorageConfig, mode store.WorkspaceSharingMode) runtime.WorkspaceBackend {
			return offExportBackend{WorkspaceBackend: orig(cfg, mode), edit: edit}
		}
		_, err := kubernetesTestManager("kubernetes").RemoveNFSWorktree(context.Background(), f.projectScionDir, testNFSWorkspaceProjectID, "test-agent")
		assert.ErrorContains(t, err, "unexpected workspace path", name)
		assert.FileExists(t, filepath.Join(outside, "worktrees", "test-agent", "keep"), name)
		assert.True(t, provision.IsRealWorktreeDir(provision.WorktreePath(ws, "test-agent"), ws), name)
	}
}
