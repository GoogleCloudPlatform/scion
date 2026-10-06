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
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestNFSAgentDirSelection(t *testing.T) {
	git := &api.GitCloneConfig{URL: "https://example.com/repo.git"}
	cpa := map[string]string{"SCION_WORKSPACE_MODE": "clone-per-agent"}
	cases := []struct {
		name       string
		env        map[string]string
		gitClone   *api.GitCloneConfig
		agentName  string
		wantName   string
		wantBranch string
		wantErr    bool
	}{
		{name: "clone-per-agent", env: cpa, gitClone: git, agentName: "my-agent", wantName: "my-agent", wantBranch: "scion/my-agent"},
		{name: "per-agent label", env: map[string]string{"SCION_WORKSPACE_MODE": "per-agent"}, gitClone: git, agentName: "my-agent", wantName: "my-agent", wantBranch: "scion/my-agent"},
		{name: "explicit branch", env: map[string]string{"SCION_WORKSPACE_MODE": "clone-per-agent", "SCION_AGENT_BRANCH": "feature/x"}, gitClone: git, agentName: "my-agent", wantName: "my-agent", wantBranch: "feature/x"},
		{name: "worktree-per-agent", env: map[string]string{"SCION_WORKSPACE_MODE": "worktree-per-agent"}, gitClone: git, agentName: "my-agent"},
		{name: "shared-plain", env: map[string]string{"SCION_WORKSPACE_MODE": "shared-plain"}, gitClone: git, agentName: "my-agent"},
		{name: "no mode", env: map[string]string{}, gitClone: git, agentName: "my-agent"},
		{name: "non-git", env: cpa, agentName: "my-agent"},
		{name: "empty clone URL", env: cpa, gitClone: &api.GitCloneConfig{}, agentName: "my-agent"},
		{name: "nil env", gitClone: git, agentName: "my-agent"},
		// The name becomes a path segment: anything that is not an agent
		// slug is an error, never a fallback to the shared checkout.
		{name: "empty name", env: cpa, gitClone: git, agentName: "", wantErr: true},
		{name: "name with separator", env: cpa, gitClone: git, agentName: "a/b", wantErr: true},
		{name: "name dot-dot", env: cpa, gitClone: git, agentName: "..", wantErr: true},
		{name: "name not a slug", env: cpa, gitClone: git, agentName: "My_Agent", wantErr: true},
		{name: "name with dot", env: cpa, gitClone: git, agentName: "a.b", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name, branch, err := nfsAgentDirSelection(tc.env, tc.gitClone, tc.agentName)
			if tc.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
			assert.Equal(t, tc.wantName, name)
			assert.Equal(t, tc.wantBranch, branch)
		})
	}
}

func nfsTestAgentDir(mountRoot, agentName string) string {
	return filepath.Join(mountRoot, "share-1", "projects", testNFSWorkspaceProjectID, "agents", agentName)
}

// startNFSAgentDirAgent runs Manager.Start for a project on the NFS
// workspace backend with the given env, and reports the RunConfig and
// whether the agent's own workspace existed (and was empty) when the
// runtime was asked to create the pod.
func startNFSAgentDirAgent(t *testing.T, runtimeName, mountRoot string, env map[string]string, gitClone *api.GitCloneConfig) (cfg runtime.RunConfig, emptyWorkspaceAtRun bool, err error) {
	t.Helper()
	cfg, emptyWorkspaceAtRun, _, err = startNFSAgentDirAgentNamed(t, "test-agent", runtimeName, mountRoot, env, gitClone)
	return cfg, emptyWorkspaceAtRun, err
}

// startNFSAgentDirAgentNamed is startNFSAgentDirAgent for an agent named
// name; ran reports whether the runtime was asked to run the agent.
func startNFSAgentDirAgentNamed(t *testing.T, name, runtimeName, mountRoot string, env map[string]string, gitClone *api.GitCloneConfig) (cfg runtime.RunConfig, emptyWorkspaceAtRun, ran bool, err error) {
	t.Helper()
	f := newSharedDirStorageRunFixture(t)
	f.writeGlobalSettings(t, fmt.Sprintf(nfsWorkspaceStartYAML, mountRoot))
	ws := filepath.Join(nfsTestAgentDir(mountRoot, "test-agent"), provision.AgentWorkspaceDir)
	mockRT := &runtime.MockRuntime{
		NameFunc: func() string { return runtimeName },
		RunFunc: func(ctx context.Context, rc runtime.RunConfig) (string, error) {
			ran = true
			cfg = rc
			entries, readErr := os.ReadDir(ws)
			emptyWorkspaceAtRun = readErr == nil && len(entries) == 0
			return "mock-id", nil
		},
	}
	fullEnv := map[string]string{"SCION_PROJECT_ID": testNFSWorkspaceProjectID}
	for k, v := range env {
		fullEnv[k] = v
	}
	_, err = NewManager(mockRT).Start(context.Background(), api.StartOptions{
		Name:        name,
		ProjectPath: f.projectScionDir,
		NoAuth:      true,
		Env:         fullEnv,
		GitClone:    gitClone,
	})
	return cfg, emptyWorkspaceAtRun, ran, err
}

var testGitClone = &api.GitCloneConfig{URL: "https://example.com/repo.git"}

// First start of a clone-per-agent agent on Kubernetes: the agent's own
// workspace (agents/<agent name>/workspace) is created empty before the
// pod, the project's workspace directory is not, and the pod is told to
// use the agent directory.
func TestStartNFSAgentDir_FirstStart(t *testing.T) {
	mountRoot := filepath.Join(t.TempDir(), "nfs")
	require.NoError(t, os.MkdirAll(filepath.Join(mountRoot, "share-1"), 0o755))

	cfg, emptyAtRun, err := startNFSAgentDirAgent(t, "kubernetes", mountRoot, map[string]string{"SCION_WORKSPACE_MODE": "clone-per-agent"}, testGitClone)
	require.NoError(t, err)
	assert.True(t, emptyAtRun, "the agent's workspace must exist, empty, when the pod is created")
	assert.Equal(t, "test-agent", cfg.NFSAgentDirName)
	assert.Equal(t, "scion/test-agent", cfg.NFSAgentBranch)
	assert.Empty(t, cfg.NFSWorktreeName)
	assert.Equal(t, "projects/"+testNFSWorkspaceProjectID+"/workspace", cfg.NFSSubPath)
	assert.Equal(t, "/workspace", cfg.ContainerWorkspace)
	assert.True(t, cfg.NFSWorkspacePreCreated)
	assert.NoDirExists(t, nfsTestWorkspaceDir(mountRoot), "the project's shared workspace is not created")
}

// A clone-per-agent agent whose name is not an agent slug stops the start
// with an error: it does not fall back to the project's shared workspace,
// the pod is not created, and nothing is created on the export.
func TestStartNFSAgentDir_NonSlugNameFails(t *testing.T) {
	for _, name := range []string{"My_Agent", "a.b"} {
		t.Run(name, func(t *testing.T) {
			mountRoot := filepath.Join(t.TempDir(), "nfs")
			require.NoError(t, os.MkdirAll(filepath.Join(mountRoot, "share-1"), 0o755))

			_, _, ran, err := startNFSAgentDirAgentNamed(t, name, "kubernetes", mountRoot,
				map[string]string{"SCION_WORKSPACE_MODE": "clone-per-agent"}, testGitClone)
			require.Error(t, err)
			assert.False(t, ran, "the pod must not be created")
			assert.NoDirExists(t, nfsTestWorkspaceDir(mountRoot), "the project's shared workspace is not created")
			assert.NoDirExists(t, filepath.Dir(nfsTestAgentDir(mountRoot, "x")), "no agents/ entry is created")
		})
	}
}

// An explicit branch is passed to the pod; a restart keeps an existing
// workspace's files.
func TestStartNFSAgentDir_ExplicitBranchAndRestart(t *testing.T) {
	mountRoot := filepath.Join(t.TempDir(), "nfs")
	ws := filepath.Join(nfsTestAgentDir(mountRoot, "test-agent"), provision.AgentWorkspaceDir)
	require.NoError(t, os.MkdirAll(ws, 0o770))
	require.NoError(t, os.WriteFile(filepath.Join(ws, "work.txt"), []byte("kept"), 0o644))

	cfg, _, err := startNFSAgentDirAgent(t, "kubernetes", mountRoot,
		map[string]string{"SCION_WORKSPACE_MODE": "clone-per-agent", "SCION_AGENT_BRANCH": "feature/x"}, testGitClone)
	require.NoError(t, err)
	assert.Equal(t, "feature/x", cfg.NFSAgentBranch)
	assert.FileExists(t, filepath.Join(ws, "work.txt"))
}

// The export not mounted on the broker: the pod still uses the agent
// directory, and the node creates it.
func TestStartNFSAgentDir_ExportNotMounted(t *testing.T) {
	mountRoot := filepath.Join(t.TempDir(), "nfs")
	cfg, emptyAtRun, err := startNFSAgentDirAgent(t, "kubernetes", mountRoot, map[string]string{"SCION_WORKSPACE_MODE": "clone-per-agent"}, testGitClone)
	require.NoError(t, err)
	assert.False(t, emptyAtRun)
	assert.Equal(t, "test-agent", cfg.NFSAgentDirName)
	assert.False(t, cfg.NFSWorkspacePreCreated)
	assert.NoDirExists(t, mountRoot)
}

// Other modes, non-git projects and other runtimes get no agent directory.
func TestStartNFSAgentDir_OtherModesUnchanged(t *testing.T) {
	cases := []struct {
		name     string
		runtime  string
		mode     string
		gitClone *api.GitCloneConfig
	}{
		{name: "shared-plain", runtime: "kubernetes", mode: "shared-plain", gitClone: testGitClone},
		{name: "no mode", runtime: "kubernetes", gitClone: testGitClone},
		{name: "worktree-per-agent", runtime: "kubernetes", mode: "worktree-per-agent", gitClone: testGitClone},
		{name: "non-git clone-per-agent", runtime: "kubernetes", mode: "clone-per-agent"},
		{name: "docker clone-per-agent", runtime: "docker", mode: "clone-per-agent", gitClone: testGitClone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mountRoot := filepath.Join(t.TempDir(), "nfs")
			ws := nfsTestWorkspaceDir(mountRoot)
			require.NoError(t, os.MkdirAll(ws, 0o770))
			require.NoError(t, os.WriteFile(filepath.Join(ws, provision.ProvisionSentinelFile), []byte("x"), 0o644))
			env := map[string]string{}
			if tc.mode != "" {
				env["SCION_WORKSPACE_MODE"] = tc.mode
			}
			cfg, _, err := startNFSAgentDirAgent(t, tc.runtime, mountRoot, env, tc.gitClone)
			require.NoError(t, err)
			assert.Empty(t, cfg.NFSAgentDirName)
			assert.Empty(t, cfg.NFSAgentBranch)
			assert.NoDirExists(t, filepath.Join(mountRoot, "share-1", "projects", testNFSWorkspaceProjectID, "agents"))
		})
	}
}

// ensureNFSAgentWorkspaceLeaf creates the agent's own workspace, not the
// project's, keeps an existing one as it is, and refuses non-slug names.
func TestEnsureNFSAgentWorkspaceLeaf(t *testing.T) {
	mountRoot := filepath.Join(t.TempDir(), "nfs")
	resolved := resolveTestNFSWorkspace(t, mountRoot)
	ws := filepath.Join(nfsTestAgentDir(mountRoot, "agent-1"), provision.AgentWorkspaceDir)

	ok, err := ensureNFSAgentWorkspaceLeaf("docker", testNFSWorkspaceProjectID, resolved, "ws-pv", nil, "agent-1")
	require.NoError(t, err)
	assert.False(t, ok)

	require.NoError(t, os.MkdirAll(filepath.Join(mountRoot, "share-1"), 0o755))
	ok, err = ensureNFSAgentWorkspaceLeaf("kubernetes", testNFSWorkspaceProjectID, resolved, "ws-pv", nil, "agent-1")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.DirExists(t, ws)
	assert.NoDirExists(t, resolved.HostPath)
	st := statMode(t, ws)
	assert.Equal(t, uint32(nfsLeafGroupAccessBits), st.Mode&nfsLeafGroupAccessBits)
	// The init container writes its lock, record and sentinel in the agent
	// directory, so it gets the same group access as the workspace.
	st = statMode(t, nfsTestAgentDir(mountRoot, "agent-1"))
	assert.Equal(t, uint32(nfsLeafGroupAccessBits), st.Mode&nfsLeafGroupAccessBits)

	marker := filepath.Join(ws, "work.txt")
	require.NoError(t, os.WriteFile(marker, []byte("x"), 0o644))
	ok, err = ensureNFSAgentWorkspaceLeaf("kubernetes", testNFSWorkspaceProjectID, resolved, "ws-pv", nil, "agent-1")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.FileExists(t, marker)

	for _, bad := range []string{"../x", "a/b", "Agent-1", "", ".."} {
		_, err = ensureNFSAgentWorkspaceLeaf("kubernetes", testNFSWorkspaceProjectID, resolved, "ws-pv", nil, bad)
		assert.Error(t, err, "name %q", bad)
	}
}

// An existing agent directory without setgid and group write is left as
// it is, and the leaves do not count as prepared, so the provisioning chown
// stays strict; the workspace is still created in it.
func TestEnsureNFSAgentWorkspaceLeaf_ExistingAgentDirWithoutGroupWrite(t *testing.T) {
	mountRoot := filepath.Join(t.TempDir(), "nfs")
	resolved := resolveTestNFSWorkspace(t, mountRoot)
	require.NoError(t, os.MkdirAll(filepath.Join(mountRoot, "share-1"), 0o755))
	agentDir := nfsTestAgentDir(mountRoot, "agent-1")
	require.NoError(t, os.MkdirAll(agentDir, 0o755))
	require.NoError(t, os.Chmod(agentDir, os.ModeSetgid|0o755))

	ok, err := ensureNFSAgentWorkspaceLeaf("kubernetes", testNFSWorkspaceProjectID, resolved, "ws-pv", nil, "agent-1")
	require.NoError(t, err)
	assert.False(t, ok)
	ws := filepath.Join(agentDir, provision.AgentWorkspaceDir)
	assert.DirExists(t, ws)
	assert.Equal(t, uint32(nfsLeafGroupAccessBits), statMode(t, ws).Mode&nfsLeafGroupAccessBits)
	assert.Equal(t, uint32(unix.S_ISGID|0o755), statMode(t, agentDir).Mode&0o7777)
}

// runAgentWorkspacePurgeInline makes RemoveNFSAgentFiles delete the
// moved-aside workspace before it returns.
func runAgentWorkspacePurgeInline(t *testing.T) *[]string {
	t.Helper()
	var purged []string
	orig := purgeRemovedAgentWorkspaces
	purgeRemovedAgentWorkspaces = func(dir string) {
		purged = append(purged, dir)
		require.NoError(t, provision.PurgeRemovedAgentWorkspaces(dir))
	}
	t.Cleanup(func() { purgeRemovedAgentWorkspaces = orig })
	return &purged
}

// provisionTestAgentDir prepares agents/<name> on the export as the
// clone-per-agent init container does, and fills its workspace as the
// agent container's clone would.
func provisionTestAgentDir(t *testing.T, mountRoot, name string) string {
	t.Helper()
	agentDir := nfsTestAgentDir(mountRoot, name)
	require.NoError(t, os.MkdirAll(agentDir, 0o770))
	require.NoError(t, provision.ProvisionAgentDir(provision.ProvisionInput{
		Resolved:  provision.ResolvedWorkspace{HostPath: agentDir},
		ProjectID: testNFSWorkspaceProjectID,
		AgentID:   name,
		AgentName: "scion/" + name,
		Mode:      store.SharingModeClonePerAgent,
		NFSUID:    os.Getuid(),
		NFSGID:    os.Getgid(),
	}))
	ws := filepath.Join(agentDir, provision.AgentWorkspaceDir)
	require.NoError(t, os.MkdirAll(filepath.Join(ws, ".git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(ws, "README.md"), []byte("hi\n"), 0o644))
	return agentDir
}

// treeSnapshot lists every path under root with its size, symlinks not
// followed, skipping the subtree skip.
func treeSnapshot(t *testing.T, root, skip string) []string {
	t.Helper()
	var out []string
	require.NoError(t, filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if skip != "" && p == skip {
			return filepath.SkipDir
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		out = append(out, fmt.Sprintf("%s %v %d", p, info.Mode(), info.Size()))
		return nil
	}))
	sort.Strings(out)
	return out
}

func nfsMountRootOf(ws string) string {
	// ws is <mountRoot>/share-1/projects/<id>/workspace.
	return filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(ws))))
}

// Delete with files: the agent's workspace and branch record are removed,
// the agent directory and other agents' directories stay, and creating
// the agent again starts from an empty workspace.
func TestRemoveNFSAgentFiles_RemovesAgentWorkspace(t *testing.T) {
	f := newSharedDirStorageRunFixture(t)
	mountRoot := filepath.Join(t.TempDir(), "nfs")
	f.writeGlobalSettings(t, fmt.Sprintf(nfsWorkspaceStartYAML, mountRoot))
	agentDir := provisionTestAgentDir(t, mountRoot, "test-agent")
	otherDir := provisionTestAgentDir(t, mountRoot, "other-agent")
	purged := runAgentWorkspacePurgeInline(t)

	paths, err := kubernetesTestManager("kubernetes").RemoveNFSAgentFiles(context.Background(), f.projectScionDir, testNFSWorkspaceProjectID, "test-agent")
	require.NoError(t, err)
	assert.Contains(t, paths, agentDir)
	assert.Equal(t, []string{agentDir}, *purged)
	var left []string
	for _, name := range nfsDirNames(t, agentDir) {
		// Released locks leave entries the next acquisition clears.
		if !strings.HasPrefix(name, ".scion-provision.lock") {
			left = append(left, name)
		}
	}
	assert.Equal(t, []string{provision.ProvisionSentinelFile}, left, "only the workspace and the record are removed")
	assert.FileExists(t, filepath.Join(otherDir, provision.AgentWorkspaceDir, "README.md"))

	// Again: nothing left to remove.
	_, err = kubernetesTestManager("kubernetes").RemoveNFSAgentFiles(context.Background(), f.projectScionDir, testNFSWorkspaceProjectID, "test-agent")
	require.NoError(t, err)

	// Created again on another branch: a fresh, empty workspace.
	require.NoError(t, provision.ProvisionAgentDir(provision.ProvisionInput{
		Resolved:  provision.ResolvedWorkspace{HostPath: agentDir},
		AgentID:   "test-agent",
		AgentName: "feature/other",
		Mode:      store.SharingModeClonePerAgent,
		NFSUID:    os.Getuid(),
		NFSGID:    os.Getgid(),
	}))
	assert.Empty(t, nfsDirNames(t, filepath.Join(agentDir, provision.AgentWorkspaceDir)))
}

// Delete for an agent that has no files on the export (never started, or
// the files were already removed) does nothing and is not an error.
func TestRemoveNFSAgentFiles_NoFiles(t *testing.T) {
	f := newSharedDirStorageRunFixture(t)
	mountRoot := filepath.Join(t.TempDir(), "nfs")
	f.writeGlobalSettings(t, fmt.Sprintf(nfsWorkspaceStartYAML, mountRoot))
	require.NoError(t, os.MkdirAll(filepath.Join(mountRoot, "share-1"), 0o755))
	before := treeSnapshot(t, mountRoot, "")

	_, err := kubernetesTestManager("kubernetes").RemoveNFSAgentFiles(context.Background(), f.projectScionDir, testNFSWorkspaceProjectID, "test-agent")
	require.NoError(t, err)
	assert.Equal(t, before, treeSnapshot(t, mountRoot, ""))
}

// Mode switch, clone-per-agent then shared-plain: the project's shared
// workspace now exists next to the agent's directory, from an agent
// started after the switch. Delete still removes the agent's workspace,
// and leaves the shared workspace alone.
func TestRemoveNFSAgentFiles_AfterSwitchToSharedPlain(t *testing.T) {
	f := newSharedDirStorageRunFixture(t)
	mountRoot := filepath.Join(t.TempDir(), "nfs")
	f.writeGlobalSettings(t, fmt.Sprintf(nfsWorkspaceStartYAML, mountRoot))
	agentDir := provisionTestAgentDir(t, mountRoot, "test-agent")
	ws := nfsTestWorkspaceDir(mountRoot)
	require.NoError(t, provision.ProvisionShared(provision.ProvisionInput{
		Resolved:    provision.ResolvedWorkspace{HostPath: ws},
		ProjectID:   testNFSWorkspaceProjectID,
		AgentID:     "later-agent",
		Mode:        store.SharingModeSharedPlain,
		NFSUID:      os.Getuid(),
		NFSGID:      os.Getgid(),
		SentinelDir: ws,
	}))
	require.NoError(t, os.WriteFile(filepath.Join(ws, "shared.txt"), []byte("x"), 0o644))
	sharedBefore := treeSnapshot(t, ws, "")
	runAgentWorkspacePurgeInline(t)

	_, err := kubernetesTestManager("kubernetes").RemoveNFSAgentFiles(context.Background(), f.projectScionDir, testNFSWorkspaceProjectID, "test-agent")
	require.NoError(t, err)
	assert.NoDirExists(t, filepath.Join(agentDir, provision.AgentWorkspaceDir))
	assert.NoFileExists(t, filepath.Join(agentDir, provision.AgentBranchFile))
	assert.Equal(t, sharedBefore, treeSnapshot(t, ws, ""))
}

// Mode switch, shared-plain (or worktree-per-agent) then clone-per-agent:
// the agent being deleted has no agent directory, while other agents do.
// Nothing outside agents/<agent name> is touched; in particular the
// shared workspace and other agents' directories are unchanged.
func TestRemoveNFSAgentFiles_AfterSwitchToClonePerAgent(t *testing.T) {
	f, ws := setupNFSWorktreeRemoval(t)
	mountRoot := nfsMountRootOf(ws)
	otherDir := provisionTestAgentDir(t, mountRoot, "other-agent")
	runPurgeInline(t)
	runAgentWorkspacePurgeInline(t)
	projectRoot := filepath.Dir(ws)
	// The delete may touch only worktrees/test-agent (the worktree the
	// agent got before the switch) and agents/test-agent, which does not
	// exist.
	worktree := provision.WorktreePath(ws, "test-agent")
	before := treeSnapshot(t, filepath.Join(projectRoot, "agents"), "")
	require.Equal(t, []string{"other-agent"}, nfsDirNames(t, filepath.Join(projectRoot, "agents")))

	paths, err := kubernetesTestManager("kubernetes").RemoveNFSAgentFiles(context.Background(), f.projectScionDir, testNFSWorkspaceProjectID, "test-agent")
	require.NoError(t, err)
	assert.Contains(t, paths, worktree)
	assert.Contains(t, paths, nfsTestAgentDir(mountRoot, "test-agent"))
	assert.NoDirExists(t, worktree)
	assert.NoDirExists(t, nfsTestAgentDir(mountRoot, "test-agent"), "no agent directory is created")
	assert.Equal(t, before, treeSnapshot(t, filepath.Join(projectRoot, "agents"), ""))
	assert.FileExists(t, filepath.Join(otherDir, provision.AgentWorkspaceDir, "README.md"))
	assert.True(t, provision.IsRealWorktreeDir(provision.WorktreePath(ws, "other-agent"), ws))
	assert.FileExists(t, filepath.Join(ws, "README.md"))
}

// An agent that had both a worktree and an agent directory (created in
// each mode at different times) loses both on delete.
func TestRemoveNFSAgentFiles_RemovesBoth(t *testing.T) {
	f, ws := setupNFSWorktreeRemoval(t)
	mountRoot := nfsMountRootOf(ws)
	agentDir := provisionTestAgentDir(t, mountRoot, "test-agent")
	runPurgeInline(t)
	runAgentWorkspacePurgeInline(t)

	_, err := kubernetesTestManager("kubernetes").RemoveNFSAgentFiles(context.Background(), f.projectScionDir, testNFSWorkspaceProjectID, "test-agent")
	require.NoError(t, err)
	assert.NoDirExists(t, provision.WorktreePath(ws, "test-agent"))
	assert.NoDirExists(t, filepath.Join(agentDir, provision.AgentWorkspaceDir))
}

// Other runtimes, an unmounted export and bad names leave the agent
// directory alone.
func TestRemoveNFSAgentFiles_NoOpAndBadNames(t *testing.T) {
	f := newSharedDirStorageRunFixture(t)
	mountRoot := filepath.Join(t.TempDir(), "nfs")
	f.writeGlobalSettings(t, fmt.Sprintf(nfsWorkspaceStartYAML, mountRoot))
	agentDir := provisionTestAgentDir(t, mountRoot, "test-agent")
	runAgentWorkspacePurgeInline(t)
	readme := filepath.Join(agentDir, provision.AgentWorkspaceDir, "README.md")

	paths, err := kubernetesTestManager("docker").RemoveNFSAgentFiles(context.Background(), f.projectScionDir, testNFSWorkspaceProjectID, "test-agent")
	require.NoError(t, err)
	assert.Empty(t, paths)
	assert.FileExists(t, readme)

	m := kubernetesTestManager("kubernetes")
	for _, name := range []string{"", "..", "../test-agent", "a/b", "Test-Agent", "a.b"} {
		_, err := m.RemoveNFSAgentFiles(context.Background(), f.projectScionDir, testNFSWorkspaceProjectID, name)
		assert.Error(t, err, "name %q", name)
	}
	for _, projectID := range []string{"", "..", "a/b"} {
		_, err := m.RemoveNFSAgentFiles(context.Background(), f.projectScionDir, projectID, "test-agent")
		assert.Error(t, err, "project ID %q", projectID)
	}
	assert.FileExists(t, readme)

	f.writeGlobalSettings(t, fmt.Sprintf(nfsWorkspaceStartYAML, filepath.Join(t.TempDir(), "not-mounted")))
	_, err = m.RemoveNFSAgentFiles(context.Background(), f.projectScionDir, testNFSWorkspaceProjectID, "test-agent")
	require.NoError(t, err)
	assert.FileExists(t, readme)
}

// A symlinked agent directory, or a symlinked agents/ directory, is
// refused, and what it points to is untouched.
func TestRemoveNFSAgentFiles_RefusesSymlinks(t *testing.T) {
	for _, which := range []string{"agent dir", "agents dir"} {
		t.Run(which, func(t *testing.T) {
			f := newSharedDirStorageRunFixture(t)
			mountRoot := filepath.Join(t.TempDir(), "nfs")
			f.writeGlobalSettings(t, fmt.Sprintf(nfsWorkspaceStartYAML, mountRoot))
			agentDir := provisionTestAgentDir(t, mountRoot, "test-agent")
			runAgentWorkspacePurgeInline(t)
			link := agentDir
			if which == "agents dir" {
				link = filepath.Dir(agentDir)
			}
			elsewhere := filepath.Join(t.TempDir(), "elsewhere")
			require.NoError(t, os.Rename(link, elsewhere))
			require.NoError(t, os.Symlink(elsewhere, link))
			before := treeSnapshot(t, elsewhere, "")

			_, err := kubernetesTestManager("kubernetes").RemoveNFSAgentFiles(context.Background(), f.projectScionDir, testNFSWorkspaceProjectID, "test-agent")
			assert.ErrorContains(t, err, "is not a plain directory on the export")
			assert.Equal(t, before, treeSnapshot(t, elsewhere, ""))
		})
	}
}

// While the agent directory's lock is held (an init container is
// preparing it), the delete gives up after its wait and leaves the
// workspace in place.
func TestRemoveNFSAgentFiles_LockHeld(t *testing.T) {
	f := newSharedDirStorageRunFixture(t)
	mountRoot := filepath.Join(t.TempDir(), "nfs")
	f.writeGlobalSettings(t, fmt.Sprintf(nfsWorkspaceStartYAML, mountRoot))
	agentDir := provisionTestAgentDir(t, mountRoot, "test-agent")
	runAgentWorkspacePurgeInline(t)
	orig := nfsAgentDirLockWait
	nfsAgentDirLockWait = 300 * time.Millisecond
	t.Cleanup(func() { nfsAgentDirLockWait = orig })

	release := holdAgentDirLock(t, agentDir)
	_, err := kubernetesTestManager("kubernetes").RemoveNFSAgentFiles(context.Background(), f.projectScionDir, testNFSWorkspaceProjectID, "test-agent")
	assert.Error(t, err)
	assert.FileExists(t, filepath.Join(agentDir, provision.AgentWorkspaceDir, "README.md"))
	release()

	_, err = kubernetesTestManager("kubernetes").RemoveNFSAgentFiles(context.Background(), f.projectScionDir, testNFSWorkspaceProjectID, "test-agent")
	require.NoError(t, err)
	assert.NoDirExists(t, filepath.Join(agentDir, provision.AgentWorkspaceDir))
}

// holdAgentDirLock puts a freshly taken provisioning lock in agentDir, as
// another holder would, and returns a function that releases it.
func holdAgentDirLock(t *testing.T, agentDir string) func() {
	t.Helper()
	lockDir := filepath.Join(agentDir, ".scion-provision.lock")
	require.NoError(t, os.Mkdir(lockDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(lockDir, "owner"), []byte("other-holder"), 0o600))
	release := func() { require.NoError(t, os.RemoveAll(lockDir)) }
	return release
}

func nfsDirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}
