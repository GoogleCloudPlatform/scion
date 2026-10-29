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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// writeSharedProjectConfig seeds the hub's shared project configuration
// <root>/.scion (project-id, project-slug "web", settings.yaml).
func writeSharedProjectConfig(t *testing.T, root, projectID string) string {
	t.Helper()
	scionDir := filepath.Join(root, config.DotScion)
	mustMkdirAll(t, scionDir)
	for name, content := range map[string]string{
		projectkeys.ProjectIDFile:   projectID + "\n",
		projectkeys.ProjectSlugFile: "web\n",
		"settings.yaml":             "schema_version: \"1\"\n",
	} {
		require.NoError(t, os.WriteFile(filepath.Join(scionDir, name), []byte(content), 0o644))
	}
	return scionDir
}

// externalAgentDir is the broker-local external agent dir of agent in the
// shared project projectID (slug "web") under home.
func externalAgentDir(home, projectID, agent string) string {
	return filepath.Join(home, config.GlobalDir, config.ProjectConfigsDir,
		"web__"+strings.ReplaceAll(projectID, "-", "")[:8], config.DotScion, "agents", agent)
}

// sharedLayoutStart starts agent "test-agent" from the shared project root
// on an nfs project storage Layout with a mock runtime named rtName, and
// returns the RunConfig, the project root, the external agent dir and the
// Start options it used.
func sharedLayoutStart(t *testing.T, rtName string, mutate func(*api.StartOptions)) (runtime.RunConfig, string, string, api.StartOptions, error) {
	t.Helper()
	f := newSharedDirStorageRunFixture(t)
	mountRoot := filepath.Join(t.TempDir(), "nfs")
	f.writeGlobalSettings(t, fmt.Sprintf(nfsWorkspaceStartYAML, mountRoot))
	root := filepath.Join(mountRoot, "share-1", "projects", sharedRootUUID)
	scionDir := writeSharedProjectConfig(t, root, sharedRootUUID)
	before := listTree(t, scionDir)

	var cfg runtime.RunConfig
	mockRT := &runtime.MockRuntime{
		NameFunc: func() string { return rtName },
		RunFunc: func(ctx context.Context, rc runtime.RunConfig) (string, error) {
			cfg = rc
			return "mock-id", nil
		},
	}
	opts := api.StartOptions{
		Name:           "test-agent",
		ProjectPath:    root,
		NoAuth:         true,
		RunID:          "run-shared-1",
		FreshProvision: true,
		Env: map[string]string{
			"SCION_AGENT_ID":   "agent-shared-1",
			"SCION_PROJECT_ID": sharedRootUUID,
		},
	}
	mutate(&opts)
	_, err := NewManager(mockRT).Start(context.Background(), opts)
	if after := listTree(t, scionDir); strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Errorf("the agent manager wrote into the shared project configuration:\nbefore %v\nafter  %v", before, after)
	}
	return cfg, root, externalAgentDir(f.tmpDir, sharedRootUUID, "test-agent"), opts, err
}

// A clone-per-agent agent on a shared Kubernetes NFS Layout: its workspace
// is the agent's directory on the share (<HostBase>/projects/<uuid>/agents/
// <slug>/workspace), which the workspace source guard admits; its state
// stays in the broker-local external root, with no workspace there; and
// the run is recorded where delete and launch recovery read it.
func TestStart_SharedLayoutClonePerAgentWorkspaceOnShare(t *testing.T) {
	cfg, root, ext, opts, err := sharedLayoutStart(t, "kubernetes", func(o *api.StartOptions) {
		o.GitClone = &api.GitCloneConfig{URL: "https://example.com/repo.git"}
		o.Env["SCION_WORKSPACE_MODE"] = "clone-per-agent"
	})
	require.NoError(t, err, "the agent's workspace on the share must pass the workspace source guard")

	shareWS := filepath.Join(root, "agents", "test-agent", "workspace")
	assert.Equal(t, "test-agent", cfg.NFSAgentDirName, "the pod uses the agent's own directory on the share")
	assert.Equal(t, "scion/test-agent", cfg.NFSAgentBranch)
	assert.DirExists(t, shareWS, "the NFS leaf helpers create the agent's workspace on the share")

	assert.FileExists(t, filepath.Join(ext, "scion-agent.json"), "agent state stays in the broker-local external root")
	assert.NoDirExists(t, filepath.Join(ext, "workspace"), "no workspace is created below the broker-local agent dir")

	assert.FileExists(t, filepath.Join(ext, HostStateFile), "the run is recorded in the agent's state dir")
	got, err := RecordedRunOwner(RunOwnerTargetFor(opts))
	require.NoError(t, err)
	assert.Equal(t, "run-shared-1", got, "delete and launch recovery find the recorded run")
}

// The same for an empty-per-agent agent: the workspace handed to the
// runtime is the agent's directory on the share.
func TestStart_SharedLayoutEmptyPerAgentWorkspaceOnShare(t *testing.T) {
	cfg, root, ext, opts, err := sharedLayoutStart(t, "kubernetes", func(o *api.StartOptions) {
		o.EmptyPerAgentWorkspace = true
		o.Env["SCION_WORKSPACE_MODE"] = "empty-per-agent"
	})
	require.NoError(t, err, "the agent's workspace on the share must pass the workspace source guard")

	shareWS := filepath.Join(root, "agents", "test-agent", "workspace")
	assert.Equal(t, shareWS, cfg.Workspace)
	assert.Equal(t, "test-agent", cfg.NFSAgentDirName)
	assert.True(t, cfg.NFSAgentDirEmpty)
	assert.DirExists(t, shareWS)
	assert.NoDirExists(t, filepath.Join(root, "workspace"), "the project's shared workspace is not created")

	assert.FileExists(t, filepath.Join(ext, "scion-agent.json"))
	assert.NoDirExists(t, filepath.Join(ext, "workspace"))

	got, err := RecordedRunOwner(RunOwnerTargetFor(opts))
	require.NoError(t, err)
	assert.Equal(t, "run-shared-1", got)
}

// A shared-plain hub-native agent on the same Layout is unaffected: its
// workspace is the project's NFS workspace leaf (projects/<uuid>/
// workspace, the backend override), never an agent directory on the share.
func TestStart_SharedLayoutSharedPlainUnaffected(t *testing.T) {
	for _, rtName := range []string{"docker", "kubernetes"} {
		t.Run(rtName, func(t *testing.T) {
			cfg, root, ext, opts, err := sharedLayoutStart(t, rtName, func(o *api.StartOptions) {
				o.SharedWorkspace = true
			})
			require.NoError(t, err)
			if rtName == "docker" {
				assert.Equal(t, filepath.Join(root, "workspace"), cfg.Workspace)
			} else {
				assert.Equal(t, "projects/"+sharedRootUUID+"/workspace", cfg.NFSSubPath)
				assert.Empty(t, cfg.NFSAgentDirName)
			}
			assert.NoDirExists(t, filepath.Join(root, "agents"), "a shared-plain agent gets no agent directory on the share")
			assert.FileExists(t, filepath.Join(ext, "scion-agent.json"))
			got, err := RecordedRunOwner(RunOwnerTargetFor(opts))
			require.NoError(t, err)
			assert.Equal(t, "run-shared-1", got)
		})
	}
}

// sharedLayoutAgentWorkspace applies only to a shared project configuration
// on a shared Layout, never to a legacy agent dir, and refuses a Layout
// that places the project elsewhere.
func TestSharedLayoutAgentWorkspace(t *testing.T) {
	nfsSettings := func(t *testing.T, mountRoot string) *config.VersionedSettings {
		t.Helper()
		return &config.VersionedSettings{Server: &config.V1ServerConfig{
			WorkspaceStorage: &config.V1WorkspaceStorageConfig{
				Backend: config.WorkspaceStorageBackendNFS,
				NFS: &config.V1NFSConfig{
					MountRoot: mountRoot,
					Shares:    []config.V1NFSShare{{ID: "share-1", Server: "10.0.0.2", Export: "/scion-workspaces", PVName: "ws-pv"}},
				},
			},
		}}
	}
	// The agent manager records its runtime; the rule applies to Kubernetes.
	ctx := contextWithAgentRuntime(context.Background(), "kubernetes")

	t.Run("shared Layout", func(t *testing.T) {
		_, home := sharedConfigProject(t)
		mountRoot := filepath.Join(t.TempDir(), "nfs")
		root := filepath.Join(mountRoot, "share-1", "projects", sharedCfgProjectID)
		scionDir := writeSharedProjectConfig(t, root, sharedCfgProjectID)
		got, err := sharedLayoutAgentWorkspace(ctx, nfsSettings(t, mountRoot), scionDir, "a1", externalAgentDir(home, sharedCfgProjectID, "a1"))
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(root, "agents", "a1", "workspace"), got)
	})

	t.Run("not a shared project configuration", func(t *testing.T) {
		f := newSharedDirStorageRunFixture(t)
		got, err := sharedLayoutAgentWorkspace(ctx, nfsSettings(t, t.TempDir()), f.projectScionDir, "a1", filepath.Join(f.projectScionDir, "agents", "a1"))
		require.NoError(t, err)
		assert.Empty(t, got, "a non-shared project keeps <agentDir>/workspace")
	})

	t.Run("local Layout", func(t *testing.T) {
		root, home := sharedConfigProject(t)
		got, err := sharedLayoutAgentWorkspace(ctx, &config.VersionedSettings{}, filepath.Join(root, config.DotScion), "a1", externalAgentDir(home, sharedCfgProjectID, "a1"))
		require.NoError(t, err)
		assert.Empty(t, got, "a non-shared Layout keeps <agentDir>/workspace")
	})

	t.Run("legacy agent dir", func(t *testing.T) {
		_, home := sharedConfigProject(t)
		mountRoot := filepath.Join(t.TempDir(), "nfs")
		root := filepath.Join(mountRoot, "share-1", "projects", sharedCfgProjectID)
		scionDir := writeSharedProjectConfig(t, root, sharedCfgProjectID)
		legacyScion := filepath.Join(home, config.GlobalDir, config.ProjectsDir, "web", config.DotScion)
		mustMkdirAll(t, legacyScion)
		require.NoError(t, config.WriteProjectID(legacyScion, sharedCfgProjectID))
		legacy := filepath.Join(legacyScion, "agents", "old")
		mustMkdirAll(t, legacy)
		require.NoError(t, os.WriteFile(filepath.Join(legacy, "scion-agent.json"), []byte("{}"), 0o644))
		got, err := sharedLayoutAgentWorkspace(ctx, nfsSettings(t, mountRoot), scionDir, "old", legacy)
		require.NoError(t, err)
		assert.Empty(t, got, "a legacy agent keeps its legacy workspace")
	})

	// Only Kubernetes creates the agent's directory on the share, so every
	// other runtime, and a caller that recorded no runtime, keeps
	// <agentDir>/workspace (the share placement for them is a follow-up).
	for _, rt := range []string{"docker", "podman", ""} {
		t.Run("runtime "+rt+" keeps the broker-local workspace", func(t *testing.T) {
			_, home := sharedConfigProject(t)
			mountRoot := filepath.Join(t.TempDir(), "nfs")
			root := filepath.Join(mountRoot, "share-1", "projects", sharedCfgProjectID)
			scionDir := writeSharedProjectConfig(t, root, sharedCfgProjectID)
			rtCtx := context.Background()
			if rt != "" {
				rtCtx = contextWithAgentRuntime(rtCtx, rt)
			}
			got, err := sharedLayoutAgentWorkspace(rtCtx, nfsSettings(t, mountRoot), scionDir, "a1", externalAgentDir(home, sharedCfgProjectID, "a1"))
			require.NoError(t, err)
			assert.Empty(t, got, "a non-Kubernetes runtime keeps <agentDir>/workspace")
		})
	}

	t.Run("Layout places the project elsewhere", func(t *testing.T) {
		root, home := sharedConfigProject(t)
		_, err := sharedLayoutAgentWorkspace(ctx, nfsSettings(t, filepath.Join(t.TempDir(), "other")), filepath.Join(root, config.DotScion), "a1", externalAgentDir(home, sharedCfgProjectID, "a1"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "refusing to start")
	})
}

// The host state root of a shared project configuration is the root of
// the agent dir config.AgentDirForProject resolves, for every sharing mode:
// the record is written and read in the agent's state dir.
func TestHostStateRootFor_SharedProjectConfig(t *testing.T) {
	root, home := sharedConfigProject(t)
	ext := externalAgentDir(home, sharedCfgProjectID, "a1")
	mustMkdirAll(t, ext)
	extRoot := filepath.Dir(filepath.Dir(filepath.Dir(ext)))

	for _, shared := range []bool{false, true} {
		for _, hubID := range []string{sharedCfgProjectID, ""} {
			t.Run(fmt.Sprintf("shared=%v hub id %q", shared, hubID), func(t *testing.T) {
				target := RunOwnerTarget{ProjectPath: root, AgentName: "a1", HubProjectID: hubID, SharedWorkspace: shared}
				got, err := HostStateRootFor(target)
				require.NoError(t, err)
				assert.Equal(t, extRoot, got)
			})
		}
	}

	target := RunOwnerTarget{ProjectPath: root, AgentName: "a1", HubProjectID: sharedCfgProjectID}
	require.NoError(t, recordRunOwnerInAgentDir(target, ext, "run-shared-2"))
	got, err := RecordedRunOwner(target)
	require.NoError(t, err)
	assert.Equal(t, "run-shared-2", got)
	assert.FileExists(t, filepath.Join(ext, HostStateFile))
	assert.NoFileExists(t, filepath.Join(root, config.DotScion, "agents", "a1", HostStateFile), "nothing is recorded on the share")

	_, err = HostStateRootFor(RunOwnerTarget{ProjectPath: root, AgentName: "a1", HubProjectID: "11111111-2222-4333-8444-555555555555"})
	assert.ErrorIs(t, err, errHostStateRootRefused, "a hub project ID naming another project is refused")
}

// A legacy agent of a shared project configuration keeps its record in its
// legacy in-project dir, where config.AgentDirForProject finds it.
func TestHostStateRootFor_SharedProjectConfigLegacyAgent(t *testing.T) {
	root, home := sharedConfigProject(t)
	legacyRoot := filepath.Join(home, config.GlobalDir, config.ProjectsDir, "web")
	legacyScion := filepath.Join(legacyRoot, config.DotScion)
	mustMkdirAll(t, legacyScion)
	require.NoError(t, config.WriteProjectID(legacyScion, sharedCfgProjectID))
	legacy := filepath.Join(legacyScion, "agents", "old")
	mustMkdirAll(t, legacy)
	require.NoError(t, os.WriteFile(filepath.Join(legacy, "scion-agent.json"), []byte("{}"), 0o644))

	target := RunOwnerTarget{ProjectPath: root, AgentName: "old", HubProjectID: sharedCfgProjectID}
	got, err := HostStateRootFor(target)
	require.NoError(t, err)
	assert.True(t, samePath(legacyRoot, got), "root %q, want the legacy project root %q", got, legacyRoot)
	require.NoError(t, recordRunOwnerInAgentDir(target, legacy, "run-shared-3"))
	owner, err := RecordedRunOwner(target)
	require.NoError(t, err)
	assert.Equal(t, "run-shared-3", owner)
}

// Only a git clone in the clone-per-agent mode is an agent-scoped clone
// workspace; a worktree-per-agent dispatch also carries the clone (on
// Kubernetes) and is not.
func TestIsClonePerAgentStart(t *testing.T) {
	gc := &api.GitCloneConfig{URL: "https://example.com/repo.git"}
	cases := []struct {
		name string
		opts api.StartOptions
		want bool
	}{
		{"clone-per-agent", api.StartOptions{GitClone: gc, Env: map[string]string{"SCION_WORKSPACE_MODE": "clone-per-agent"}}, true},
		{"per-agent label", api.StartOptions{GitClone: gc, Env: map[string]string{"SCION_WORKSPACE_MODE": "per-agent"}}, true},
		{"worktree-per-agent", api.StartOptions{GitClone: gc, Env: map[string]string{"SCION_WORKSPACE_MODE": "worktree-per-agent"}}, false},
		{"no mode (shared-plain)", api.StartOptions{GitClone: gc}, false},
		{"clone-per-agent without a clone", api.StartOptions{Env: map[string]string{"SCION_WORKSPACE_MODE": "clone-per-agent"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isClonePerAgentStart(tc.opts))
			ctx, _ := buildProvisionContext(context.Background(), tc.opts)
			assert.Equal(t, tc.want, isClonePerAgentWorkspace(ctx), "provisioning context")
			assert.Equal(t, tc.want, agentScopedWorkspace(ctx), "agent-scoped")
		})
	}
}

// Reprovision of a clone-per-agent agent on a shared Layout checks the clone
// in the agent's directory on the share, where the provisioning init
// container clones it.
func TestReprovision_SharedLayoutClonePerAgentChecksShare(t *testing.T) {
	_, root, _, opts, err := sharedLayoutStart(t, "kubernetes", func(o *api.StartOptions) {
		o.GitClone = &api.GitCloneConfig{URL: "https://example.com/repo.git"}
		o.Env["SCION_WORKSPACE_MODE"] = "clone-per-agent"
	})
	require.NoError(t, err)
	shareWS := filepath.Join(root, "agents", "test-agent", "workspace")
	m := NewManager(&runtime.MockRuntime{NameFunc: func() string { return "kubernetes" }})

	_, err = m.Reprovision(context.Background(), opts)
	require.Error(t, err, "no clone on the share yet")
	assert.ErrorIs(t, err, ErrReprovisionRefused)
	assert.Contains(t, err.Error(), "has no existing git clone at "+shareWS)

	mustMkdirAll(t, filepath.Join(shareWS, ".git"))
	_, err = m.Reprovision(context.Background(), opts)
	if err != nil {
		assert.NotContains(t, err.Error(), "has no existing git clone", "the clone on the share must be found")
	}
}

// A worktree-per-agent dispatch on Kubernetes also carries the git clone,
// but it is not an agent-scoped workspace: Reprovision keeps checking the
// broker-local agentDir/workspace, never the agent's directory on the
// share, even when that directory holds a clone.
func TestReprovision_SharedLayoutWorktreeKeepsOldWorkspace(t *testing.T) {
	_, root, ext, opts, err := sharedLayoutStart(t, "kubernetes", func(o *api.StartOptions) {
		o.GitClone = &api.GitCloneConfig{URL: "https://example.com/repo.git"}
		o.Env["SCION_WORKSPACE_MODE"] = "clone-per-agent"
	})
	require.NoError(t, err)
	shareWS := filepath.Join(root, "agents", "test-agent", "workspace")
	mustMkdirAll(t, filepath.Join(shareWS, ".git"))

	wt := opts
	wt.Env = map[string]string{}
	for k, v := range opts.Env {
		wt.Env[k] = v
	}
	wt.Env["SCION_WORKSPACE_MODE"] = "worktree-per-agent"
	m := NewManager(&runtime.MockRuntime{NameFunc: func() string { return "kubernetes" }})
	_, err = m.Reprovision(context.Background(), wt)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrReprovisionRefused)
	assert.Contains(t, err.Error(), "has no existing git clone at "+filepath.Join(ext, "workspace"))
	assert.NotContains(t, err.Error(), shareWS)
}

// A fresh agent-scoped agent on the nfs workspace backend: its workspace on
// the share does not exist when Start begins (provisioning leaves it to
// Start's NFS leaf helpers), and Start's workspace safety net must not refuse
// it. The leaf helper then creates the directory, so the runtime is told the
// workspace was pre-created.
func TestStart_SharedLayoutFreshAgentWorkspaceCreatedByLeafHelper(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*api.StartOptions)
	}{
		{"clone-per-agent", func(o *api.StartOptions) {
			o.GitClone = &api.GitCloneConfig{URL: "https://example.com/repo.git"}
			o.Env["SCION_WORKSPACE_MODE"] = "clone-per-agent"
		}},
		{"empty-per-agent", func(o *api.StartOptions) {
			o.EmptyPerAgentWorkspace = true
			o.Env["SCION_WORKSPACE_MODE"] = "empty-per-agent"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, root, _, _, err := sharedLayoutStart(t, "kubernetes", func(o *api.StartOptions) {
				tc.mutate(o)
				shareWS := filepath.Join(o.ProjectPath, "agents", "test-agent", "workspace")
				_, statErr := os.Stat(shareWS)
				require.True(t, os.IsNotExist(statErr), "precondition: the agent's workspace on the share must not exist yet (stat: %v)", statErr)
			})
			require.NoError(t, err, "a fresh agent's workspace on the share is created by Start's leaf helper, not refused as missing")
			assert.NotContains(t, fmt.Sprint(err), "workspace directory does not exist")
			assert.DirExists(t, filepath.Join(root, "agents", "test-agent", "workspace"), "the leaf helper creates the workspace")
			assert.Equal(t, "test-agent", cfg.NFSAgentDirName)
			assert.True(t, cfg.NFSWorkspacePreCreated, "the leaf helper prepared the workspace before the pod")
		})
	}
}

// The safety net keeps refusing a missing workspace on every backend other
// than nfs and on every runtime other than Kubernetes, and skips it for a
// Kubernetes start on the nfs backend.
func TestCheckStartWorkspaceExists(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "gone", "workspace")
	present := t.TempDir()
	nfs := &config.VersionedSettings{Server: &config.V1ServerConfig{WorkspaceStorage: &config.V1WorkspaceStorageConfig{
		Backend: "nfs",
		NFS:     &config.V1NFSConfig{MountRoot: t.TempDir(), Shares: []config.V1NFSShare{{ID: "share-1", Server: "10.0.0.2", Export: "/x"}}},
	}}}
	local := &config.VersionedSettings{Server: &config.V1ServerConfig{}}
	gitOpts := api.StartOptions{GitClone: &api.GitCloneConfig{URL: "https://example.com/repo.git"}}

	err := checkStartWorkspaceExists("kubernetes", missing, local, gitOpts, false)
	require.Error(t, err, "local backend, missing workspace")
	assert.Contains(t, err.Error(), "workspace directory does not exist: "+missing)
	err = checkStartWorkspaceExists("kubernetes", missing, nil, api.StartOptions{}, false)
	require.Error(t, err, "no settings, missing workspace")
	assert.NoError(t, checkStartWorkspaceExists("kubernetes", present, local, gitOpts, false), "local backend, existing workspace")
	assert.NoError(t, checkStartWorkspaceExists("kubernetes", "", local, gitOpts, false), "no workspace")

	assert.NoError(t, checkStartWorkspaceExists("kubernetes", missing, nfs, gitOpts, false), "Kubernetes on the nfs backend (shared-plain clone): the leaf helpers create it")
	assert.NoError(t, checkStartWorkspaceExists("kubernetes", missing, nfs, api.StartOptions{}, true), "Kubernetes on the nfs backend (empty-per-agent)")
	// Another runtime on the nfs backend: nothing creates the workspace, so
	// a missing one is still refused.
	for _, rt := range []string{"docker", "podman", "apple"} {
		err = checkStartWorkspaceExists(rt, missing, nfs, gitOpts, false)
		require.Error(t, err, "%s on the nfs backend, missing workspace", rt)
		assert.Contains(t, err.Error(), "workspace directory does not exist: "+missing)
	}
	assert.True(t, startUsesNFSWorkspaceBackend(nfs, api.StartOptions{}, false), "worktree-per-agent default on nfs")
	assert.False(t, startUsesNFSWorkspaceBackend(local, gitOpts, false))
}

// A Docker broker on a shared nfs Layout keeps the pre-existing placement for
// a clone-per-agent agent: its workspace is the broker-local
// <agentDir>/workspace (which provisioning creates) and nothing is placed in
// the agent's directory on the share. The workspace source guard refuses
// that workspace, because it is outside the project's root on the share, so
// the start fails closed, as it did before agent-scoped workspaces moved to
// the share on Kubernetes. Running such agents on other runtimes is a
// follow-up.
func TestStart_SharedLayoutDockerClonePerAgentRefusedAsBefore(t *testing.T) {
	cfg, root, ext, _, err := sharedLayoutStart(t, "docker", func(o *api.StartOptions) {
		o.GitClone = &api.GitCloneConfig{URL: "https://example.com/repo.git"}
		o.Env["SCION_WORKSPACE_MODE"] = "clone-per-agent"
	})
	require.Error(t, err, "a Docker clone-per-agent start on a shared Layout is refused")
	assert.Contains(t, err.Error(), "is outside the permitted workspace root", "the workspace source guard refuses the broker-local workspace")
	assert.DirExists(t, filepath.Join(ext, "workspace"), "the workspace stays below the broker-local agent state dir")
	assert.FileExists(t, filepath.Join(ext, "scion-agent.json"), "agent state stays in the broker-local external root")
	assert.NoDirExists(t, filepath.Join(root, "agents", "test-agent"), "nothing is placed in the agent's directory on the share")
	assert.Empty(t, cfg.NFSAgentDirName, "the runtime is never reached")
	assert.Empty(t, cfg.Name, "no run is started")
}
