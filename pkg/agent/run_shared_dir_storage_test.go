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
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Round 1 review findings C1/T2 (AC5) and High-1/High-2 in r1-test.md: the
// unit tests below drive the real Manager.Start entry point end to end,
// modelled on TestStartPropagatesNFSWorkspaceBackendToRunConfig
// (run_test.go:684). Mutation testing (r1-test.md M1/M2/M12) showed that
// nothing previously exercised this wiring: resolveSharedDirs could be
// unwired from Start, RunConfig.SharedDirStorage could be dropped, or the
// fail-closed error could be swallowed, and the full suite still passed.

// sharedDirStorageRunFixture sets up a minimal, working project (harness
// config + template at the GLOBAL scion dir, matching
// TestStartResolvesHarnessConfigUserFromAbsTemplateDir's pattern) under a
// temp HOME, so each test only needs to write its own global and/or
// project-level settings.yaml content.
type sharedDirStorageRunFixture struct {
	tmpDir          string
	globalScionDir  string
	projectScionDir string
}

func newSharedDirStorageRunFixture(t *testing.T) sharedDirStorageRunFixture {
	t.Helper()
	tmpDir := t.TempDir()

	oldWd, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(tmpDir))
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
	t.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	require.NoError(t, os.MkdirAll(globalScionDir, 0755))

	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
	require.NoError(t, os.MkdirAll(hcDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(hcDir, "config.yaml"),
		[]byte("harness: gemini\nuser: scion\nimage: test-image:latest\n"), 0644))

	tplDir := filepath.Join(globalScionDir, "templates", "default")
	require.NoError(t, os.MkdirAll(tplDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(tplDir, "scion-agent.json"),
		[]byte(`{"default_harness_config": "test-harness"}`), 0644))

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	require.NoError(t, os.MkdirAll(projectScionDir, 0755))

	return sharedDirStorageRunFixture{tmpDir: tmpDir, globalScionDir: globalScionDir, projectScionDir: projectScionDir}
}

// baseSettingsYAML is the minimal boilerplate every settings.yaml in these
// tests needs (schema_version + a runnable "local" profile). extraServerYAML,
// if non-empty, is spliced in as additional lines under "server:".
func baseSettingsYAML(extraServerYAML string) string {
	yaml := `schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
`
	if extraServerYAML != "" {
		yaml += "server:\n" + extraServerYAML
	}
	return yaml
}

func (f sharedDirStorageRunFixture) writeGlobalSettings(t *testing.T, extraServerYAML string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(f.globalScionDir, "settings.yaml"),
		[]byte(baseSettingsYAML(extraServerYAML)), 0644))
}

func (f sharedDirStorageRunFixture) writeProjectSettings(t *testing.T, extraServerYAML string) {
	t.Helper()
	content := `schema_version: "1"
`
	if extraServerYAML != "" {
		content += "server:\n" + extraServerYAML
	}
	require.NoError(t, os.WriteFile(filepath.Join(f.projectScionDir, "settings.yaml"), []byte(content), 0644))
}

const sharedDirStorageNFSGlobalYAML = `  shared_dir_storage:
    backend: nfs
    nfs:
      mount_root: %s
      shares:
        - id: %s
          pv_name: %s
`

// TestStartSharedDirStorage_ProjectLevelOnly_Ignored is round 1 review item
// 1 (AC5, C1/T2), the "ignored" half: a shared_dir_storage block set ONLY at
// the project level (no global block at all) must be ignored — Start falls
// through to the local layout, exactly as if shared_dir_storage were unset.
func TestStartSharedDirStorage_ProjectLevelOnly_Ignored(t *testing.T) {
	f := newSharedDirStorageRunFixture(t)
	f.writeGlobalSettings(t, "") // no shared_dir_storage globally

	projectMountRoot := filepath.Join(f.tmpDir, "project-only-nfs")
	f.writeProjectSettings(t, sprintfServerYAML(sharedDirStorageNFSGlobalYAML, projectMountRoot, "proj-share", "proj-pv"))

	var capturedConfig runtime.RunConfig
	var ranCount int
	mockRT := &runtime.MockRuntime{
		NameFunc: func() string { return "kubernetes" },
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			capturedConfig = config
			ranCount++
			return "mock-id", nil
		},
	}
	mgr := NewManager(mockRT)

	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: f.projectScionDir,
		NoAuth:      true,
		Env: map[string]string{
			"SCION_AGENT_ID":   "agent-1",
			"SCION_PROJECT_ID": "pid-project-only",
		},
		SharedDirs: []api.SharedDir{{Name: "scratchpad"}},
	})
	require.NoError(t, err)
	require.Equal(t, 1, ranCount)

	assert.Nil(t, capturedConfig.SharedDirStorage,
		"a project-level-only shared_dir_storage block must be ignored (AC5)")

	// The project-level mount_root must never appear in the resolved volumes.
	for _, v := range capturedConfig.Volumes {
		assert.NotContains(t, v.Source, projectMountRoot,
			"project-level shared_dir_storage must not redirect the bind source")
	}
}

// TestStartSharedDirStorage_GlobalWinsOverProjectLevel is round 1 review
// item 1 (AC5, C1/T2), the "global wins" half, and also covers item 3(i)
// (T1-i): with a global shared_dir_storage=nfs block and a conflicting
// project-level block, the global block wins, and a kubernetes-named mock
// runtime gets RunConfig.SharedDirStorage populated from it.
func TestStartSharedDirStorage_GlobalWinsOverProjectLevel(t *testing.T) {
	f := newSharedDirStorageRunFixture(t)

	globalMountRoot := filepath.Join(f.tmpDir, "global-nfs")
	f.writeGlobalSettings(t, sprintfServerYAML(sharedDirStorageNFSGlobalYAML, globalMountRoot, "global-share", "global-pv"))

	// Project tries to disable NFS entirely — must have no effect.
	f.writeProjectSettings(t, "  shared_dir_storage:\n    backend: local\n")

	var capturedConfig runtime.RunConfig
	mockRT := &runtime.MockRuntime{
		NameFunc: func() string { return "kubernetes" },
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			capturedConfig = config
			return "mock-id", nil
		},
	}
	mgr := NewManager(mockRT)

	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: f.projectScionDir,
		NoAuth:      true,
		Env: map[string]string{
			"SCION_AGENT_ID":   "agent-2",
			"SCION_PROJECT_ID": "pid-global-wins",
		},
		SharedDirs: []api.SharedDir{{Name: "scratchpad"}},
	})
	require.NoError(t, err)

	require.NotNil(t, capturedConfig.SharedDirStorage,
		"the global shared_dir_storage=nfs block must win over the conflicting project-level one (AC5)")
	assert.Equal(t, "nfs", capturedConfig.SharedDirStorage.Backend)
	assert.Equal(t, "global-pv", capturedConfig.SharedDirStorage.PVClaimName)
	assert.Equal(t, "projects/pid-global-wins/shared-dirs/scratchpad", capturedConfig.SharedDirStorage.SubPaths["scratchpad"])
}

// TestStartPropagatesSharedDirStorageToLocalContainerRunConfig is round 1
// review item 3(ii) (T1-ii): a local-container-named mock runtime (docker)
// with shared_dir_storage=nfs and an existing host base gets a Volumes entry
// with the resolved bind source, and SCION_VOLUMES is set on the env map.
func TestStartPropagatesSharedDirStorageToLocalContainerRunConfig(t *testing.T) {
	f := newSharedDirStorageRunFixture(t)

	mountRoot := filepath.Join(f.tmpDir, "srv")
	shareID := "docker-share"
	hostBase := filepath.Join(mountRoot, shareID)
	require.NoError(t, os.MkdirAll(hostBase, 0o775))

	f.writeGlobalSettings(t, sprintfServerYAML(sharedDirStorageNFSGlobalYAML, mountRoot, shareID, "docker-pv"))
	f.writeProjectSettings(t, "")

	var capturedConfig runtime.RunConfig
	mockRT := &runtime.MockRuntime{
		NameFunc: func() string { return "docker" },
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			capturedConfig = config
			return "mock-id", nil
		},
	}
	mgr := NewManager(mockRT)

	envMap := map[string]string{
		"SCION_AGENT_ID":   "agent-3",
		"SCION_PROJECT_ID": "pid-docker",
	}
	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: f.projectScionDir,
		NoAuth:      true,
		Env:         envMap,
		SharedDirs:  []api.SharedDir{{Name: "scratchpad"}},
	})
	require.NoError(t, err)

	wantSource := filepath.Join(hostBase, "projects", "pid-docker", "shared-dirs", "scratchpad")
	var found bool
	for _, v := range capturedConfig.Volumes {
		if v.Target == "/scion-volumes/scratchpad" {
			found = true
			assert.Equal(t, wantSource, v.Source)
		}
	}
	assert.True(t, found, "expected a /scion-volumes/scratchpad volume in RunConfig.Volumes")
	assert.Equal(t, "/scion-volumes", envMap["SCION_VOLUMES"])
}

// TestStartSharedDirStorageNFS_MissingProjectID_ErrorsAndNeverRuns is round 1
// review item 3(iii) (T1-iii): shared_dir_storage=nfs with no hub project ID
// available makes Start fail closed, and the runtime's Run is never called
// (no agent silently starts with the wrong/no shared dirs).
func TestStartSharedDirStorageNFS_MissingProjectID_ErrorsAndNeverRuns(t *testing.T) {
	f := newSharedDirStorageRunFixture(t)
	f.writeGlobalSettings(t, sprintfServerYAML(sharedDirStorageNFSGlobalYAML, filepath.Join(f.tmpDir, "srv"), "share", "pv"))
	f.writeProjectSettings(t, "")

	ranCount := 0
	mockRT := &runtime.MockRuntime{
		NameFunc: func() string { return "kubernetes" },
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			ranCount++
			return "mock-id", nil
		},
	}
	mgr := NewManager(mockRT)

	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: f.projectScionDir,
		NoAuth:      true,
		Env: map[string]string{
			"SCION_AGENT_ID": "agent-4",
			// No SCION_PROJECT_ID / SCION_GROVE_ID.
		},
		SharedDirs: []api.SharedDir{{Name: "scratchpad"}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "hub project ID")
	assert.Equal(t, 0, ranCount, "Run must never be called when shared_dir_storage=nfs fails closed")
}

// TestStartSharedDirStorage_Unset_AC1AndAC3 is the unset-block variant of
// item 3 / T8: with no shared_dir_storage configured anywhere,
// RunConfig.SharedDirStorage is nil (AC1), and AC3's workspace claims hold
// trivially since nothing in this code path touches workspace fields:
// WorkspaceBackendName=="" and Workspace is a local (non-NFS) path.
func TestStartSharedDirStorage_Unset_AC1AndAC3(t *testing.T) {
	f := newSharedDirStorageRunFixture(t)
	f.writeGlobalSettings(t, "")
	f.writeProjectSettings(t, "")

	var capturedConfig runtime.RunConfig
	mockRT := &runtime.MockRuntime{
		NameFunc: func() string { return "docker" },
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			capturedConfig = config
			return "mock-id", nil
		},
	}
	mgr := NewManager(mockRT)

	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: f.projectScionDir,
		NoAuth:      true,
		Env: map[string]string{
			"SCION_AGENT_ID":   "agent-5",
			"SCION_PROJECT_ID": "pid-unset",
		},
		SharedDirs: []api.SharedDir{{Name: "scratchpad"}},
	})
	require.NoError(t, err)

	assert.Nil(t, capturedConfig.SharedDirStorage, "AC1: unset shared_dir_storage never populates SharedDirStorage")
	assert.Equal(t, "", capturedConfig.WorkspaceBackendName, "AC3: workspace backend is untouched by shared_dir_storage")
	assert.NotEmpty(t, capturedConfig.Workspace, "AC3: workspace should resolve to a local path")
	assert.NotContains(t, capturedConfig.Workspace, "srv", "AC3: workspace must not be redirected onto any NFS mount")
}

// sprintfServerYAML formats the sharedDirStorageNFSGlobalYAML template.
func sprintfServerYAML(format, mountRoot, shareID, pvName string) string {
	return fmt.Sprintf(format, mountRoot, shareID, pvName)
}
