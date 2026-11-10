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
	"io/fs"
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
	sl := newSharedLayoutStarter(t, rtName)
	cfg, opts, err := sl.start(mutate)
	return cfg, sl.root, sl.ext, opts, err
}

// sharedLayoutStarter starts agent "test-agent" from one shared project
// root on an nfs project storage Layout, any number of times
// (newSharedLayoutStarter).
type sharedLayoutStarter struct {
	t         *testing.T
	f         sharedDirStorageRunFixture
	rtName    string
	mountRoot string
	root      string // the project root on the share
	ext       string // the agent's broker-local external agent dir
	scionDir  string
	// run, when set, is the mock runtime's Run (it records the RunConfig
	// in any case).
	run func(ctx context.Context, rc runtime.RunConfig) (string, error)
}

// newSharedLayoutStarter seeds the fixture sharedLayoutStart uses.
func newSharedLayoutStarter(t *testing.T, rtName string) *sharedLayoutStarter {
	t.Helper()
	f := newSharedDirStorageRunFixture(t)
	mountRoot := filepath.Join(t.TempDir(), "nfs")
	f.writeGlobalSettings(t, fmt.Sprintf(nfsWorkspaceStartYAML, mountRoot))
	root := filepath.Join(mountRoot, "share-1", "projects", sharedRootUUID)
	scionDir := writeSharedProjectConfig(t, root, sharedRootUUID)
	return &sharedLayoutStarter{t: t, f: f, rtName: rtName, mountRoot: mountRoot, root: root,
		ext: externalAgentDir(f.tmpDir, sharedRootUUID, "test-agent"), scionDir: scionDir}
}

// start runs one Start of "test-agent" (a fresh provision unless mutate
// says otherwise) and returns the RunConfig the runtime got (zero when it
// was never reached), the options used and Start's error. The shared
// project configuration must not change.
func (sl *sharedLayoutStarter) start(mutate func(*api.StartOptions)) (runtime.RunConfig, api.StartOptions, error) {
	t := sl.t
	t.Helper()
	before := listTree(t, sl.scionDir)
	var cfg runtime.RunConfig
	mockRT := &runtime.MockRuntime{
		NameFunc: func() string { return sl.rtName },
		RunFunc: func(ctx context.Context, rc runtime.RunConfig) (string, error) {
			cfg = rc
			if sl.run != nil {
				return sl.run(ctx, rc)
			}
			return "mock-id", nil
		},
	}
	opts := api.StartOptions{
		Name:           "test-agent",
		ProjectPath:    sl.root,
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
	if after := listTree(t, sl.scionDir); strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Errorf("the agent manager wrote into the shared project configuration:\nbefore %v\nafter  %v", before, after)
	}
	return cfg, opts, err
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

	// Kubernetes and the local container runtimes resolve the SAME path for
	// the same agent: the agent's directory on the share, which Start's NFS
	// leaf helpers create for both (ptone/scion#7224).
	t.Run("local container runtimes match Kubernetes", func(t *testing.T) {
		_, home := sharedConfigProject(t)
		mountRoot := filepath.Join(t.TempDir(), "nfs")
		root := filepath.Join(mountRoot, "share-1", "projects", sharedCfgProjectID)
		scionDir := writeSharedProjectConfig(t, root, sharedCfgProjectID)
		agentDir := externalAgentDir(home, sharedCfgProjectID, "a1")
		want, err := sharedLayoutAgentWorkspace(ctx, nfsSettings(t, mountRoot), scionDir, "a1", agentDir)
		require.NoError(t, err)
		require.Equal(t, filepath.Join(root, "agents", "a1", "workspace"), want)
		for _, rt := range []string{"docker", "podman", "container"} {
			got, err := sharedLayoutAgentWorkspace(contextWithAgentRuntime(context.Background(), rt), nfsSettings(t, mountRoot), scionDir, "a1", agentDir)
			require.NoError(t, err, rt)
			assert.Equal(t, want, got, "%s resolves the same workspace as Kubernetes", rt)
		}
	})

	// Every other runtime, and a caller that recorded no runtime, keeps
	// <agentDir>/workspace (Cloud Run mounts its own, see
	// cloudRunClonePerAgentStart).
	for _, rt := range []string{"cloudrun", ""} {
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
			assert.Empty(t, got, "runtime %q keeps <agentDir>/workspace", rt)
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

	err := checkStartWorkspaceExists("kubernetes", "", missing, local, gitOpts, false)
	require.Error(t, err, "local backend, missing workspace")
	assert.Contains(t, err.Error(), "workspace directory does not exist: "+missing)
	err = checkStartWorkspaceExists("kubernetes", "", missing, nil, api.StartOptions{}, false)
	require.Error(t, err, "no settings, missing workspace")
	assert.NoError(t, checkStartWorkspaceExists("kubernetes", "", present, local, gitOpts, false), "local backend, existing workspace")
	assert.NoError(t, checkStartWorkspaceExists("kubernetes", "", "", local, gitOpts, false), "no workspace")

	assert.NoError(t, checkStartWorkspaceExists("kubernetes", "", missing, nfs, gitOpts, false), "Kubernetes on the nfs backend (shared-plain clone): the leaf helpers create it")
	assert.NoError(t, checkStartWorkspaceExists("kubernetes", "", missing, nfs, api.StartOptions{}, true), "Kubernetes on the nfs backend (empty-per-agent)")
	// Another runtime on the nfs backend: nothing creates the workspace, so
	// a missing one is still refused.
	for _, rt := range []string{"docker", "podman", "apple"} {
		err = checkStartWorkspaceExists(rt, "", missing, nfs, gitOpts, false)
		require.Error(t, err, "%s on the nfs backend, missing workspace", rt)
		assert.Contains(t, err.Error(), "workspace directory does not exist: "+missing)
	}
	// A local container runtime start of an agent-scoped workspace of a
	// project on a shared Layout, through nfs, skips it: the leaf helpers
	// create the agent's directory on the share. Any other such start keeps
	// the check.
	sharedRoot, _ := sharedConfigProject(t)
	sharedDir := filepath.Join(sharedRoot, config.DotScion)
	cloneOpts := api.StartOptions{GitClone: gitOpts.GitClone, Env: map[string]string{"SCION_WORKSPACE_MODE": "clone-per-agent"}}
	for _, rt := range []string{"docker", "podman", "container"} {
		assert.NoError(t, checkStartWorkspaceExists(rt, sharedDir, missing, nfs, cloneOpts, false), "%s clone-per-agent on a shared Layout", rt)
		assert.NoError(t, checkStartWorkspaceExists(rt, sharedDir, missing, nfs, api.StartOptions{}, true), "%s empty-per-agent on a shared Layout", rt)
		for name, call := range map[string]func() error{
			"shared-plain clone on a shared Layout": func() error {
				return checkStartWorkspaceExists(rt, sharedDir, missing, nfs, gitOpts, false)
			},
			"clone-per-agent, not a shared project configuration": func() error {
				return checkStartWorkspaceExists(rt, "", missing, nfs, cloneOpts, false)
			},
			"clone-per-agent on the local backend": func() error {
				return checkStartWorkspaceExists(rt, sharedDir, missing, local, cloneOpts, false)
			},
		} {
			err := call()
			require.Error(t, err, "%s: %s", rt, name)
			assert.Contains(t, err.Error(), "workspace directory does not exist: "+missing)
		}
	}
	assert.True(t, startUsesNFSWorkspaceBackend(nfs, api.StartOptions{}, false), "worktree-per-agent default on nfs")
	assert.False(t, startUsesNFSWorkspaceBackend(local, gitOpts, false))
}

// localRuntimeModes are the agent-scoped workspace modes a local container
// runtime places in the agent's directory on a shared Layout.
var localRuntimeModes = []struct {
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
}

// A local container runtime (docker, podman, Apple's container) on a shared
// nfs Layout starts a clone-per-agent or empty-per-agent agent in its own
// directory on the share, agents/<slug>/workspace (ptone/scion#7224): the
// NFS leaf helpers create the directory and its workspace before the run,
// with the leaf modes (setgid, group write), and the runtime bind-mounts
// that workspace, never the project's shared checkout. Nothing is placed in
// the broker-local agent state dir.
func TestStart_SharedLayoutLocalRuntimeAgentWorkspaceOnShare(t *testing.T) {
	for _, rt := range []string{"docker", "podman", "container"} {
		for _, mode := range localRuntimeModes {
			t.Run(rt+"/"+mode.name, func(t *testing.T) {
				cfg, root, ext, _, err := sharedLayoutStart(t, rt, mode.mutate)
				require.NoError(t, err, "a %s %s start on a shared Layout succeeds", rt, mode.name)
				agentDir := filepath.Join(root, "agents", "test-agent")
				ws := filepath.Join(agentDir, "workspace")
				require.DirExists(t, ws, "the leaf helper creates the agent's workspace on the share")
				for _, dir := range []string{agentDir, ws} {
					assert.Equal(t, uint32(nfsLeafGroupAccessBits), statMode(t, dir).Mode&nfsLeafGroupAccessBits, "%s gets the leaf modes", dir)
				}
				realWS, err := filepath.EvalSymlinks(ws)
				require.NoError(t, err)
				assert.Equal(t, realWS, cfg.Workspace, "the runtime mounts the agent's own workspace on the share")
				assert.NotEqual(t, filepath.Join(root, "workspace"), cfg.Workspace, "never the project's shared checkout")
				assert.Equal(t, "nfs", cfg.WorkspaceBackendName)
				assert.Equal(t, "test-agent", cfg.NFSAgentDirName)
				assert.Contains(t, cfg.Env, "SCION_WORKSPACE_MODE="+mode.name, "the mode is kept")
				assert.NoDirExists(t, filepath.Join(ext, "workspace"), "nothing is placed below the broker-local agent state dir")
				assert.FileExists(t, filepath.Join(ext, "scion-agent.json"), "agent state stays in the broker-local external root")
			})
		}
	}
}

// fakeContainerCommand writes an executable that stands in for the docker
// or podman CLI: it records its arguments, one per line, and prints a
// container ID. It returns the command and the file its arguments go to.
func fakeContainerCommand(t *testing.T) (command, argsFile string) {
	t.Helper()
	dir := t.TempDir()
	command = filepath.Join(dir, "fake-container-cli")
	argsFile = filepath.Join(dir, "args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + argsFile + "'\necho 0123456789abcdef\n"
	require.NoError(t, os.WriteFile(command, []byte(script), 0o755))
	return command, argsFile
}

// The RunConfig Start builds for a local container runtime on a shared
// layout goes through the real Docker and Podman Run, and so through
// buildCommonRunArgs and its NFS bind-scope check, not only a mock: the
// agent's own workspace on the share is bound at the container workspace.
// The same RunConfig without NFSAgentDirName is refused by that check,
// so the test sees the real layer.
func TestStart_SharedLayoutLocalRuntimeRealRunArgs(t *testing.T) {
	runtimes := map[string]func(command string) runtime.Runtime{
		"docker": func(c string) runtime.Runtime { return &runtime.DockerRuntime{Command: c} },
		"podman": func(c string) runtime.Runtime { return &runtime.PodmanRuntime{Command: c} },
	}
	for rtName, newRT := range runtimes {
		for _, mode := range localRuntimeModes {
			t.Run(rtName+"/"+mode.name, func(t *testing.T) {
				command, argsFile := fakeContainerCommand(t)
				sl := newSharedLayoutStarter(t, rtName)
				sl.run = func(ctx context.Context, rc runtime.RunConfig) (string, error) {
					return newRT(command).Run(ctx, rc)
				}
				cfg, _, err := sl.start(mode.mutate)
				require.NoError(t, err, "the real %s Run accepts the agent's workspace on the share", rtName)
				realWS, err := filepath.EvalSymlinks(filepath.Join(sl.root, "agents", "test-agent", "workspace"))
				require.NoError(t, err)
				args, err := os.ReadFile(argsFile)
				require.NoError(t, err, "the container CLI was run")
				assert.Contains(t, string(args), realWS+":/workspace", "the agent's own workspace is bound at /workspace")
				realRoot, err := filepath.EvalSymlinks(sl.root)
				require.NoError(t, err)
				assert.NotContains(t, string(args), filepath.Join(realRoot, "workspace")+":", "the project's shared checkout is not bound")

				// The same RunConfig without the agent's name is refused by
				// the runtime's bind-scope check.
				rc := cfg
				rc.NFSAgentDirName = ""
				_, err = newRT(command).Run(context.Background(), rc)
				require.Error(t, err)
				assert.Contains(t, err.Error(), "mount outside the agent's directories on NFS project storage")
			})
		}
	}
}

// NFSAgentDirName comes only from the agent's own validated slug: Start
// sets it to api.ValidateAgentName of the agent's name, and a request that
// names a workspace in another agent's directory on the share changes
// neither it nor the workspace. For clone-per-agent the clone takes
// priority and the requested workspace is ignored (ProvisionAgent); for
// empty-per-agent a requested workspace is refused before the runtime.
func TestStart_SharedLayoutLocalRuntimeAgentDirNameFromAgentOnly(t *testing.T) {
	for _, mode := range localRuntimeModes {
		t.Run(mode.name+"/slug", func(t *testing.T) {
			cfg, _, _, opts, err := sharedLayoutStart(t, "docker", mode.mutate)
			require.NoError(t, err)
			slug, err := api.ValidateAgentName(opts.Name)
			require.NoError(t, err)
			assert.Equal(t, slug, cfg.NFSAgentDirName)
		})
	}
	requestOther := func(t *testing.T, mode int) (runtime.RunConfig, *sharedLayoutStarter, string, error) {
		t.Helper()
		sl := newSharedLayoutStarter(t, "docker")
		other := filepath.Join(sl.root, "agents", "other", "workspace")
		mustMkdirAll(t, other)
		require.NoError(t, os.WriteFile(filepath.Join(other, "theirs.txt"), []byte("x"), 0o644))
		cfg, _, err := sl.start(func(o *api.StartOptions) {
			localRuntimeModes[mode].mutate(o)
			o.Workspace = other
		})
		return cfg, sl, other, err
	}
	t.Run("clone-per-agent/request names another agent's workspace", func(t *testing.T) {
		// Through the real Docker Run: the binds it emits are scanned, not
		// only the RunConfig.
		command, argsFile := fakeContainerCommand(t)
		sl := newSharedLayoutStarter(t, "docker")
		sl.run = func(ctx context.Context, rc runtime.RunConfig) (string, error) {
			return (&runtime.DockerRuntime{Command: command}).Run(ctx, rc)
		}
		other := filepath.Join(sl.root, "agents", "other", "workspace")
		mustMkdirAll(t, other)
		require.NoError(t, os.WriteFile(filepath.Join(other, "theirs.txt"), []byte("x"), 0o644))
		cfg, _, err := sl.start(func(o *api.StartOptions) {
			localRuntimeModes[0].mutate(o)
			o.Workspace = other
		})
		require.NoError(t, err, "the requested workspace is ignored, not refused")
		realOwn, err := filepath.EvalSymlinks(filepath.Join(sl.root, "agents", "test-agent", "workspace"))
		require.NoError(t, err)
		realOther, err := filepath.EvalSymlinks(other)
		require.NoError(t, err)
		assert.Equal(t, realOwn, cfg.Workspace, "the agent's own workspace, not the requested one")
		assert.Equal(t, "test-agent", cfg.NFSAgentDirName)

		data, err := os.ReadFile(argsFile)
		require.NoError(t, err, "the container CLI was run")
		args := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
		var sources []string
		for i := 0; i+1 < len(args); i++ {
			switch args[i] {
			case "-v", "--volume":
				src, _, _ := strings.Cut(args[i+1], ":")
				sources = append(sources, src)
			case "--mount":
				for _, kv := range strings.Split(args[i+1], ",") {
					if v, ok := strings.CutPrefix(kv, "source="); ok {
						sources = append(sources, v)
					} else if v, ok := strings.CutPrefix(kv, "src="); ok {
						sources = append(sources, v)
					}
				}
			}
		}
		require.NotEmpty(t, sources, "the run args carry binds")
		assert.Contains(t, args, realOwn+":/workspace", "the mount source of /workspace is the agent's own leaf")
		for _, src := range sources {
			assert.False(t, src == realOther || strings.HasPrefix(src, realOther+string(filepath.Separator)) ||
				src == other || strings.HasPrefix(src, other+string(filepath.Separator)),
				"bind source %q is the other agent's workspace", src)
		}
		assert.NotContains(t, string(data), realOther, "the other agent's leaf appears in no run argument")
		entries, err := os.ReadDir(other)
		require.NoError(t, err)
		require.Len(t, entries, 1, "the other agent's workspace is left as it is")
		assert.Equal(t, "theirs.txt", entries[0].Name())
	})
	t.Run("empty-per-agent/request names another agent's workspace", func(t *testing.T) {
		cfg, sl, other, err := requestOther(t, 1)
		require.Error(t, err, "an empty-per-agent request with a workspace is refused")
		assert.Contains(t, err.Error(), "does not take a workspace path", "refused by ProvisionAgent for that cause")
		assert.Empty(t, cfg.Name, "the runtime is never reached")
		assert.NoDirExists(t, filepath.Join(sl.root, "agents", "test-agent"), "nothing is created for the agent")
		assert.FileExists(t, filepath.Join(other, "theirs.txt"))
	})
}

// Reincarnating (Reprovision) an agent-scoped agent of a local container
// runtime on a shared layout checks its workspace on the share, where Start
// placed it: an empty-per-agent agent's workspace is reused in place with
// its content, a missing one is refused naming the share path, and a
// clone-per-agent agent needs its clone there.
func TestReprovision_SharedLayoutDockerChecksShare(t *testing.T) {
	t.Run("empty-per-agent", func(t *testing.T) {
		_, root, _, opts, err := sharedLayoutStart(t, "docker", localRuntimeModes[1].mutate)
		require.NoError(t, err)
		shareWS := filepath.Join(root, "agents", "test-agent", "workspace")
		note := filepath.Join(shareWS, "notes.md")
		require.NoError(t, os.WriteFile(note, []byte("keep"), 0o644))
		m := NewManager(&runtime.MockRuntime{NameFunc: func() string { return "docker" }})

		_, err = m.Reprovision(context.Background(), opts)
		if err != nil {
			assert.NotContains(t, err.Error(), "workspace missing or not a real directory", "the workspace on the share must be found")
		}
		data, err := os.ReadFile(note)
		require.NoError(t, err)
		assert.Equal(t, "keep", string(data))

		require.NoError(t, os.RemoveAll(shareWS))
		_, err = m.Reprovision(context.Background(), opts)
		require.ErrorIs(t, err, ErrReprovisionRefused)
		assert.Contains(t, err.Error(), shareWS, "the refusal names the workspace on the share")
		assert.NoDirExists(t, shareWS, "reincarnate never recreates it")
	})
	t.Run("clone-per-agent", func(t *testing.T) {
		_, root, _, opts, err := sharedLayoutStart(t, "docker", localRuntimeModes[0].mutate)
		require.NoError(t, err)
		shareWS := filepath.Join(root, "agents", "test-agent", "workspace")
		m := NewManager(&runtime.MockRuntime{NameFunc: func() string { return "docker" }})

		_, err = m.Reprovision(context.Background(), opts)
		require.ErrorIs(t, err, ErrReprovisionRefused, "no clone on the share yet")
		assert.Contains(t, err.Error(), "has no existing git clone at "+shareWS)

		mustMkdirAll(t, filepath.Join(shareWS, ".git"))
		_, err = m.Reprovision(context.Background(), opts)
		if err != nil {
			assert.NotContains(t, err.Error(), "has no existing git clone", "the clone on the share must be found")
		}
	})
}

// Docker and Kubernetes place the same agent's workspace at the same path
// on the share: the Kubernetes leaf helper and the Docker bind mount both
// address <root>/agents/<slug>/workspace.
func TestStart_SharedLayoutDockerAndKubernetesSameWorkspace(t *testing.T) {
	for _, mode := range localRuntimeModes {
		t.Run(mode.name, func(t *testing.T) {
			dockerCfg, dockerRoot, _, _, err := sharedLayoutStart(t, "docker", mode.mutate)
			require.NoError(t, err)
			k8sCfg, k8sRoot, _, _, err := sharedLayoutStart(t, "kubernetes", mode.mutate)
			require.NoError(t, err)
			k8sWS := filepath.Join(k8sRoot, "agents", k8sCfg.NFSAgentDirName, "workspace")
			require.DirExists(t, k8sWS, "the Kubernetes leaf helper created the agent's workspace")
			realDockerRoot, err := filepath.EvalSymlinks(dockerRoot)
			require.NoError(t, err)
			dockerRel, err := filepath.Rel(realDockerRoot, dockerCfg.Workspace)
			require.NoError(t, err)
			k8sRel, err := filepath.Rel(k8sRoot, k8sWS)
			require.NoError(t, err)
			assert.Equal(t, k8sRel, dockerRel, "same workspace below the project root on the share")
			assert.Equal(t, k8sCfg.NFSAgentDirName, dockerCfg.NFSAgentDirName)
		})
	}
}

// Negative control for ptone/scion#7224: the workspace source guard and the
// roots Start gives it are unchanged. For a project on a shared Layout the
// same roots admit the agent's workspace on the share and still refuse a
// path outside the project's tree on the share, such as the broker-local
// <agentDir>/workspace a local container runtime used before.
func TestWorkspaceSourceRoots_SharedLayoutAdmitsShareOnly(t *testing.T) {
	_, home := sharedConfigProject(t)
	mountRoot := filepath.Join(t.TempDir(), "nfs")
	root := filepath.Join(mountRoot, "share-1", "projects", sharedCfgProjectID)
	scionDir := writeSharedProjectConfig(t, root, sharedCfgProjectID)
	shareWS := filepath.Join(root, "agents", "a1", "workspace")
	mustMkdirAll(t, shareWS)
	localWS := filepath.Join(externalAgentDir(home, sharedCfgProjectID, "a1"), "workspace")
	mustMkdirAll(t, localWS)
	outside := t.TempDir()

	for _, tc := range []struct {
		name, workspace string
		admitted        bool
	}{
		{"the agent's workspace on the share", shareWS, true},
		{"the broker-local agent workspace", localWS, false},
		{"a directory outside the layout", outside, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			roots, err := workspaceSourceRoots(false, tc.workspace, nil, scionDir)
			require.NoError(t, err)
			require.Equal(t, []string{root}, roots, "the roots are the project's root on the share")
			_, err = runtime.ValidateWorkspaceSource(tc.workspace, roots...)
			if tc.admitted {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), "is outside the permitted workspace root")
		})
	}
}

// checkLocalRuntimeAgentWorkspace admits a local container runtime start
// only when the workspace Start validated is the agent's workspace on the
// share, and refuses an agent whose work is still in its broker-local
// workspace while the share has none. It never creates or changes anything.
func TestCheckLocalRuntimeAgentWorkspace(t *testing.T) {
	setup := func(t *testing.T) (resolved runtime.ResolvedWorkspace, shareWS, agentDir string) {
		t.Helper()
		mountRoot := filepath.Join(t.TempDir(), "nfs")
		resolved = resolveTestNFSWorkspace(t, mountRoot)
		mustMkdirAll(t, resolved.HostBase)
		hostBase, err := filepath.EvalSymlinks(resolved.HostBase)
		require.NoError(t, err)
		shareWS = filepath.Join(hostBase, "projects", testNFSWorkspaceProjectID, "agents", "a1", "workspace")
		return resolved, shareWS, filepath.Join(t.TempDir(), "agents", "a1")
	}
	localWS := func(agentDir string) string { return filepath.Join(agentDir, "workspace") }

	t.Run("fresh agent", func(t *testing.T) {
		resolved, shareWS, agentDir := setup(t)
		require.NoError(t, checkLocalRuntimeAgentWorkspace("a1", shareWS, agentDir, resolved, "a1"))
		assert.NoDirExists(t, shareWS, "nothing is created")
	})
	t.Run("restart with the workspace on the share", func(t *testing.T) {
		resolved, shareWS, agentDir := setup(t)
		mustMkdirAll(t, shareWS)
		mustMkdirAll(t, localWS(agentDir))
		require.NoError(t, os.WriteFile(filepath.Join(localWS(agentDir), "stray.txt"), []byte("x"), 0o644))
		logs := captureSlogText(t)
		require.NoError(t, checkLocalRuntimeAgentWorkspace("a1", shareWS, agentDir, resolved, "a1"))
		assert.Contains(t, logs.String(), "still holds files while it runs on its directory on the shared project storage",
			"broker-local work left off the mount is logged")
		assert.FileExists(t, filepath.Join(localWS(agentDir), "stray.txt"), "it is left as it is")
	})
	t.Run("restart with the workspace on the share, nothing left locally", func(t *testing.T) {
		resolved, shareWS, agentDir := setup(t)
		mustMkdirAll(t, shareWS)
		mustMkdirAll(t, filepath.Join(localWS(agentDir), ".scion"))
		logs := captureSlogText(t)
		require.NoError(t, checkLocalRuntimeAgentWorkspace("a1", shareWS, agentDir, resolved, "a1"))
		assert.NotContains(t, logs.String(), "broker-local workspace", "provisioning entries only: nothing to report")
	})
	t.Run("restart with the workspace on the share, local workspace unreadable", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads a mode-000 directory")
		}
		resolved, shareWS, agentDir := setup(t)
		mustMkdirAll(t, shareWS)
		mustMkdirAll(t, localWS(agentDir))
		require.NoError(t, os.Chmod(localWS(agentDir), 0))
		t.Cleanup(func() { _ = os.Chmod(localWS(agentDir), 0o755) })
		logs := captureSlogText(t)
		require.NoError(t, checkLocalRuntimeAgentWorkspace("a1", shareWS, agentDir, resolved, "a1"), "a failed check never blocks the start")
		assert.Contains(t, logs.String(), "could not read the agent's broker-local workspace")
	})
	t.Run("empty broker-local workspace", func(t *testing.T) {
		resolved, shareWS, agentDir := setup(t)
		mustMkdirAll(t, localWS(agentDir))
		require.NoError(t, checkLocalRuntimeAgentWorkspace("a1", shareWS, agentDir, resolved, "a1"),
			"an empty broker-local workspace (a start refused before the fix never cloned into it) does not refuse")
		assert.DirExists(t, localWS(agentDir), "it is left as it is")
	})
	t.Run("work in the broker-local workspace", func(t *testing.T) {
		resolved, shareWS, agentDir := setup(t)
		mustMkdirAll(t, localWS(agentDir))
		work := filepath.Join(localWS(agentDir), "unpushed.txt")
		require.NoError(t, os.WriteFile(work, []byte("work"), 0o644))
		err := checkLocalRuntimeAgentWorkspace("a1", shareWS, agentDir, resolved, "a1")
		require.ErrorIs(t, err, ErrAgentWorkspaceOffShare)
		assert.True(t, config.IsAgentStateConflict(err), "brokers answer it with 409 and its message")
		assert.NotContains(t, err.Error(), agentDir, "the message names no path")
		assert.FileExists(t, work, "the broker-local workspace is left as it is")
		assert.NoDirExists(t, shareWS, "nothing is created on the share")
	})
	t.Run("only provisioning entries in the broker-local workspace", func(t *testing.T) {
		resolved, shareWS, agentDir := setup(t)
		mustMkdirAll(t, filepath.Join(localWS(agentDir), ".scion"))
		mustMkdirAll(t, filepath.Join(localWS(agentDir), ".scion-volumes"))
		require.NoError(t, checkLocalRuntimeAgentWorkspace("a1", shareWS, agentDir, resolved, "a1"))
	})
	t.Run("unreadable broker-local workspace", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads a mode-000 directory")
		}
		resolved, shareWS, agentDir := setup(t)
		mustMkdirAll(t, localWS(agentDir))
		require.NoError(t, os.WriteFile(filepath.Join(localWS(agentDir), "unpushed.txt"), []byte("work"), 0o644))
		require.NoError(t, os.Chmod(localWS(agentDir), 0))
		t.Cleanup(func() { _ = os.Chmod(localWS(agentDir), 0o755) })
		require.ErrorIs(t, checkLocalRuntimeAgentWorkspace("a1", shareWS, agentDir, resolved, "a1"), ErrAgentWorkspaceOffShare,
			"a workspace that cannot be read is never taken for empty")
		assert.NoDirExists(t, shareWS)
	})
	t.Run("broker-local workspace is a file", func(t *testing.T) {
		resolved, shareWS, agentDir := setup(t)
		mustMkdirAll(t, agentDir)
		require.NoError(t, os.WriteFile(localWS(agentDir), []byte("x"), 0o644))
		require.ErrorIs(t, checkLocalRuntimeAgentWorkspace("a1", shareWS, agentDir, resolved, "a1"), ErrAgentWorkspaceOffShare)
	})
	t.Run("share workspace cannot be checked", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root searches a mode-000 directory")
		}
		resolved, shareWS, agentDir := setup(t)
		agents := filepath.Dir(filepath.Dir(shareWS))
		mustMkdirAll(t, agents)
		require.NoError(t, os.Chmod(agents, 0))
		t.Cleanup(func() { _ = os.Chmod(agents, 0o755) })
		err := checkLocalRuntimeAgentWorkspace("a1", shareWS, agentDir, resolved, "a1")
		require.Error(t, err, "an error other than absence fails closed")
		assert.NotErrorIs(t, err, fs.ErrNotExist)
	})
	t.Run("share workspace is not a directory", func(t *testing.T) {
		resolved, shareWS, agentDir := setup(t)
		mustMkdirAll(t, filepath.Dir(shareWS))
		require.NoError(t, os.WriteFile(shareWS, []byte("x"), 0o644))
		require.ErrorIs(t, checkLocalRuntimeAgentWorkspace("a1", shareWS, agentDir, resolved, "a1"), ErrAgentWorkspaceOffShare)
	})
	t.Run("validated workspace is not the share's", func(t *testing.T) {
		resolved, shareWS, agentDir := setup(t)
		legacy := t.TempDir()
		require.ErrorIs(t, checkLocalRuntimeAgentWorkspace("a1", legacy, agentDir, resolved, "a1"), ErrAgentWorkspaceOffShare)
		require.ErrorIs(t, checkLocalRuntimeAgentWorkspace("a1", shareWS, agentDir, resolved, "a2"), ErrAgentWorkspaceOffShare,
			"another agent's directory")
		assert.NoDirExists(t, shareWS)
	})
}

// Resume of a clone-per-agent or empty-per-agent agent provisioned BEFORE
// its project storage moved to the shared Layout, on a local container
// runtime: it is refused with a clear cause (ErrAgentWorkspaceOffShare, an
// agent-state conflict), its broker-local workspace and its work are left
// as they are, and nothing is created on the share. Two shapes:
//   - legacy: the agent's state and workspace are in its legacy in-project
//     directory (a hub-managed project before its configuration moved to
//     the shared project root);
//   - external: the agent's state is in the broker-local external root and
//     its work in <agentDir>/workspace, with no workspace on the share.
func TestStart_SharedLayoutLocalRuntimePreSwitchAgentRefused(t *testing.T) {
	for _, mode := range localRuntimeModes {
		t.Run("legacy/"+mode.name, func(t *testing.T) {
			f := newSharedDirStorageRunFixture(t)
			f.writeGlobalSettings(t, "")
			mountRoot := filepath.Join(t.TempDir(), "nfs")
			root := filepath.Join(mountRoot, "share-1", "projects", sharedRootUUID)
			mustMkdirAll(t, filepath.Join(root, "workspace"))
			writeSharedProjectConfig(t, root, sharedRootUUID)
			legacyRoot := filepath.Join(f.tmpDir, config.GlobalDir, config.ProjectsDir, "web")
			legacyScion := filepath.Join(legacyRoot, config.DotScion)
			mustMkdirAll(t, legacyScion)
			require.NoError(t, config.WriteProjectID(legacyScion, sharedRootUUID))
			require.NoError(t, os.WriteFile(filepath.Join(legacyScion, "settings.yaml"), []byte("schema_version: \"1\"\n"), 0o644))
			var cfg runtime.RunConfig
			start := func(projectPath string, fresh bool) error {
				cfg = runtime.RunConfig{}
				mockRT := &runtime.MockRuntime{
					NameFunc: func() string { return "docker" },
					RunFunc: func(ctx context.Context, rc runtime.RunConfig) (string, error) {
						cfg = rc
						return "mock-id", nil
					},
				}
				opts := api.StartOptions{
					Name: "test-agent", ProjectPath: projectPath, NoAuth: true, FreshProvision: fresh,
					Env: map[string]string{"SCION_AGENT_ID": "agent-7224", "SCION_PROJECT_ID": sharedRootUUID},
				}
				mode.mutate(&opts)
				_, err := NewManager(mockRT).Start(context.Background(), opts)
				return err
			}
			// Before the switch: local project storage, the hub-managed
			// project's own directory.
			require.NoError(t, start(legacyRoot, true), "pre-switch start")
			legacyAgent := filepath.Join(legacyScion, "agents", "test-agent")
			require.True(t, strings.HasPrefix(cfg.Workspace, legacyAgent+string(filepath.Separator)),
				"fixture: the pre-switch workspace %q is in the agent's legacy dir %q", cfg.Workspace, legacyAgent)
			work := filepath.Join(cfg.Workspace, "unpushed.txt")
			require.NoError(t, os.WriteFile(work, []byte("work"), 0o644))

			// After the switch: nfs project storage, the shared project root.
			f.writeGlobalSettings(t, fmt.Sprintf(nfsWorkspaceStartYAML, mountRoot))
			err := start(root, false)
			require.ErrorIs(t, err, ErrAgentWorkspaceOffShare, "the resume is refused with a clear cause")
			assert.True(t, config.IsAgentStateConflict(err))
			assert.Empty(t, cfg.Name, "no run is started")
			data, readErr := os.ReadFile(work)
			require.NoError(t, readErr, "the pre-switch workspace is left as it is")
			assert.Equal(t, "work", string(data))
			assert.NoDirExists(t, filepath.Join(root, "agents"), "nothing is created on the share")
		})

		t.Run("external/"+mode.name, func(t *testing.T) {
			sl := newSharedLayoutStarter(t, "docker")
			_, _, err := sl.start(mode.mutate)
			require.NoError(t, err, "fixture: first start")
			// Model the pre-switch agent: its work in the broker-local
			// workspace, none on the share.
			require.NoError(t, os.RemoveAll(filepath.Join(sl.root, "agents")))
			local := filepath.Join(sl.ext, "workspace")
			mustMkdirAll(t, local)
			work := filepath.Join(local, "unpushed.txt")
			require.NoError(t, os.WriteFile(work, []byte("work"), 0o644))

			cfg, _, err := sl.start(func(o *api.StartOptions) {
				mode.mutate(o)
				o.FreshProvision = false
				o.RunID = "run-shared-2"
			})
			require.ErrorIs(t, err, ErrAgentWorkspaceOffShare, "the resume is refused with a clear cause")
			assert.Empty(t, cfg.Name, "no run is started")
			assert.FileExists(t, work, "the broker-local workspace is left as it is")
			assert.NoDirExists(t, filepath.Join(sl.root, "agents"), "nothing is created on the share")
		})
	}

	// A clone-per-agent agent created after the switch but refused at start
	// before this fix has an EMPTY broker-local workspace (it never cloned):
	// its next start is admitted and runs on the share, and the empty
	// directory is left as it is.
	t.Run("external/clone-per-agent with an empty broker-local workspace starts", func(t *testing.T) {
		sl := newSharedLayoutStarter(t, "docker")
		_, _, err := sl.start(localRuntimeModes[0].mutate)
		require.NoError(t, err, "fixture: first start")
		require.NoError(t, os.RemoveAll(filepath.Join(sl.root, "agents")))
		local := filepath.Join(sl.ext, "workspace")
		mustMkdirAll(t, local)

		cfg, _, err := sl.start(func(o *api.StartOptions) {
			localRuntimeModes[0].mutate(o)
			o.FreshProvision = false
			o.RunID = "run-shared-2"
		})
		require.NoError(t, err)
		realWS, err := filepath.EvalSymlinks(filepath.Join(sl.root, "agents", "test-agent", "workspace"))
		require.NoError(t, err)
		assert.Equal(t, realWS, cfg.Workspace)
		assert.DirExists(t, local, "the empty broker-local workspace is left as it is")
	})
}

// A Cloud Run broker on a shared nfs Layout runs a clone-per-agent agent
// in its own workspace on the share, agents/<slug>/workspace, which the
// cloudrun runtime creates and mounts (provisionCloudRunNFS) and the agent
// clones into, so the broker-local <agentDir>/workspace is never mounted
// and must not refuse the start. The runtime is reached with the clone, on
// the nfs backend; the broker itself places nothing on the share (the
// runtime is mocked here).
func TestStart_SharedLayoutCloudRunClonePerAgentStarts(t *testing.T) {
	cfg, root, ext, _, err := sharedLayoutStart(t, "cloudrun", func(o *api.StartOptions) {
		o.GitClone = &api.GitCloneConfig{URL: "https://example.com/repo.git"}
		o.Env["SCION_WORKSPACE_MODE"] = "clone-per-agent"
	})
	require.NoError(t, err, "a Cloud Run clone-per-agent start on a shared Layout is admitted")
	require.NotNil(t, cfg.GitClone, "the runtime gets the clone to make in-container")
	assert.Equal(t, "nfs", cfg.WorkspaceBackendName, "home and secrets stay on the nfs backend")
	assert.Contains(t, cfg.Env, "SCION_WORKSPACE_MODE=clone-per-agent")
	assert.Empty(t, cfg.NFSAgentDirName, "the broker selects no agent directory; the runtime creates the workspace")
	assert.Equal(t, filepath.Join(root, "workspace"), cfg.Workspace,
		"the runtime gets the project's workspace path on the share, from which it derives the agent's workspace, home and secrets")
	assert.NoDirExists(t, filepath.Join(root, "agents", "test-agent"), "the broker places nothing in the agent's directory on the share; the runtime creates the workspace")
	assert.FileExists(t, filepath.Join(ext, "scion-agent.json"), "agent state stays in the broker-local external root")
}
