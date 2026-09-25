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
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ii2HubEnvNames is the full 21-name SCION_* env var list of the running
// scion-hub process on scion-integration2 (findings/ii2-hub-env-names.txt,
// round 6 addendum Update 04:40), with plausible values: comma lists for
// AUTHORIZEDDOMAINS/ADMINEMAILS, "true" for CLOUD_LOGGING, strings
// elsewhere. Mirrors pkg/config's copy of the same fixture — duplicated
// rather than shared across packages, since it's test-only data.
var ii2HubEnvNames = map[string]string{
	"SCION_CLOUD_LOGGING":                           "true",
	"SCION_DEV_BINARIES":                            "false",
	"SCION_GCP_PROJECT_ID":                          "deploy-demo-test",
	"SCION_HUB_ENDPOINT":                            "https://community.projects.scion-ai.dev",
	"SCION_HUB_STORAGE_BUCKET":                      "scion-hub-storage",
	"SCION_IMAGE_REGISTRY":                          "us-docker.pkg.dev/deploy-demo-test/scion",
	"SCION_MAINTENANCE_REPO_BRANCH":                 "main",
	"SCION_MAINTENANCE_REPO_PATH":                   "/srv/scion-maintenance",
	"SCION_SERVER_AUTH_AUTHORIZEDDOMAINS":           "example.com,corp.example.com",
	"SCION_SERVER_BASE_URL":                         "https://community.projects.scion-ai.dev",
	"SCION_SERVER_HUB_ADMINEMAILS":                  "admin@example.com,ops@example.com",
	"SCION_SERVER_HUB_GCPPROJECTID":                 "deploy-demo-test",
	"SCION_SERVER_LOG_LEVEL":                        "info",
	"SCION_SERVER_OAUTH_CLI_GOOGLE_CLIENTID":        "cli-client-id.apps.googleusercontent.com",
	"SCION_SERVER_OAUTH_CLI_GOOGLE_CLIENTSECRET":    "cli-client-secret",
	"SCION_SERVER_OAUTH_DEVICE_GOOGLE_CLIENTID":     "device-client-id.apps.googleusercontent.com",
	"SCION_SERVER_OAUTH_DEVICE_GOOGLE_CLIENTSECRET": "device-client-secret",
	"SCION_SERVER_OAUTH_WEB_GOOGLE_CLIENTID":        "web-client-id.apps.googleusercontent.com",
	"SCION_SERVER_OAUTH_WEB_GOOGLE_CLIENTSECRET":    "web-client-secret",
	"SCION_SERVER_SECRETS_BACKEND":                  "gcp",
	"SCION_SERVER_SECRETS_GCPPROJECTID":             "deploy-demo-test",
}

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

// newSharedDirStorageRunFixture sets the process's current working
// directory to INSIDE the project fixture (not $HOME/tmpDir) — round 2
// review finding C1/T1/S-F2 showed that the naive "global-only" load,
// LoadEffectiveSettings(""), actually resolves a project from the current
// working directory via FindProjectRoot(). `scion start` run from inside a
// project checkout is the common case, so every test built on this fixture
// exercises that realistic CWD; a regression to LoadEffectiveSettings("")
// would make the AC5 tests below fail.
func newSharedDirStorageRunFixture(t *testing.T) sharedDirStorageRunFixture {
	t.Helper()
	tmpDir := t.TempDir()
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

	oldWd, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(projectDir))
	t.Cleanup(func() { _ = os.Chdir(oldWd) })

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
// runtime gets RunConfig.SharedDirStorage populated from it. Round 2 review
// finding C2/T3 (AC3): also asserts that setting shared_dir_storage=nfs
// leaves the workspace fields exactly as they'd be if it were unset.
func TestStartSharedDirStorage_GlobalWinsOverProjectLevel(t *testing.T) {
	f := newSharedDirStorageRunFixture(t)

	globalMountRoot := filepath.Join(f.tmpDir, "global-nfs")
	// The host base must exist regardless of runtime.
	require.NoError(t, os.MkdirAll(filepath.Join(globalMountRoot, "global-share"), 0o775))
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

	// AC3: shared_dir_storage=nfs must not touch workspace storage.
	assert.Equal(t, "", capturedConfig.WorkspaceBackendName)
	assert.Equal(t, "", capturedConfig.NFSPVClaimName)
	assert.Equal(t, 0, capturedConfig.NFSUID)
	assert.Equal(t, 0, capturedConfig.NFSGID)
	wantWorkspace := filepath.Dir(f.projectScionDir) // the project root
	assert.Equal(t, wantWorkspace, capturedConfig.Workspace)
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

	// AC3 (round 2 review finding C2/T3): shared_dir_storage=nfs must not
	// touch workspace storage, even for the local-container runtime.
	assert.Equal(t, "", capturedConfig.WorkspaceBackendName)
	assert.Equal(t, "", capturedConfig.NFSPVClaimName)
	assert.Equal(t, 0, capturedConfig.NFSUID)
	assert.Equal(t, 0, capturedConfig.NFSGID)
	wantWorkspace := filepath.Dir(f.projectScionDir) // the project root
	assert.Equal(t, wantWorkspace, capturedConfig.Workspace)
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

// TestStartSharedDirStorageNFS_ProjectSettingsProjectID_Ignored is round 2
// review item 3 (S-F4): the nfs branch must use only the dispatch-provided
// SCION_PROJECT_ID/SCION_GROVE_ID (opts.Env), never the broker-local
// project-settings fallback (settings.Hub.ProjectID) that the general
// projectID variable elsewhere in run.go may carry. A project's settings —
// including in-repo settings.yaml content from a cloned repository — must
// not be able to choose which project's shared tree an nfs-backed agent
// mounts. With a project-level hub.project_id set but no env project ID,
// Start must still fail closed with the same "hub project ID" error as if
// no project ID were configured anywhere.
func TestStartSharedDirStorageNFS_ProjectSettingsProjectID_Ignored(t *testing.T) {
	f := newSharedDirStorageRunFixture(t)
	f.writeGlobalSettings(t, sprintfServerYAML(sharedDirStorageNFSGlobalYAML, filepath.Join(f.tmpDir, "srv"), "share", "pv"))
	require.NoError(t, os.WriteFile(filepath.Join(f.projectScionDir, "settings.yaml"), []byte(`schema_version: "1"
hub:
  project_id: victim-project-id
`), 0644))

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
			"SCION_AGENT_ID": "agent-8",
			// No SCION_PROJECT_ID / SCION_GROVE_ID — only the project
			// settings' hub.project_id, which the nfs branch must ignore.
		},
		SharedDirs: []api.SharedDir{{Name: "scratchpad"}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "hub project ID")
	assert.Equal(t, 0, ranCount, "Run must never be called when the project-settings project ID is used instead of the dispatch-provided one")
}

// TestStartSharedDirStorageNFS_HarnessConfigEnvProjectID_Ignored is round 3
// review finding C1/S-L1 (Medium): resolveAuthEnvOverlay copies a project's
// harness_configs.<name>.env into opts.Env for any key not already present
// — including SCION_PROJECT_ID — before the nfs branch used to re-read
// opts.Env. On a hubless start (no dispatch-provided project ID), a
// project's settings (including in-repo settings.yaml content from a cloned
// repository) could therefore pick another project's shared tree. The nfs
// branch must use only the project ID snapshotted at Start entry, before
// any settings-driven env merging runs.
func TestStartSharedDirStorageNFS_HarnessConfigEnvProjectID_Ignored(t *testing.T) {
	f := newSharedDirStorageRunFixture(t)
	f.writeGlobalSettings(t, sprintfServerYAML(sharedDirStorageNFSGlobalYAML, filepath.Join(f.tmpDir, "srv"), "share", "pv"))
	require.NoError(t, os.WriteFile(filepath.Join(f.projectScionDir, "settings.yaml"), []byte(`schema_version: "1"
harness_configs:
  test-harness:
    harness: gemini
    env:
      SCION_PROJECT_ID: victim-project
`), 0644))

	ranCount := 0
	mockRT := &runtime.MockRuntime{
		NameFunc: func() string { return "docker" },
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
			"SCION_AGENT_ID": "agent-9",
			// No SCION_PROJECT_ID / SCION_GROVE_ID — only the harness
			// config's env, which the nfs branch must not pick up.
		},
		SharedDirs: []api.SharedDir{{Name: "scratchpad"}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "hub project ID")
	assert.Equal(t, 0, ranCount, "Run must never be called when a harness-config env project ID is used instead of the dispatch-provided one")
}

// TestStartSharedDirStorageNFS_HarnessConfigEnvGroveID_Ignored is the
// SCION_GROVE_ID variant of the same finding (C1/S-L1).
func TestStartSharedDirStorageNFS_HarnessConfigEnvGroveID_Ignored(t *testing.T) {
	f := newSharedDirStorageRunFixture(t)
	f.writeGlobalSettings(t, sprintfServerYAML(sharedDirStorageNFSGlobalYAML, filepath.Join(f.tmpDir, "srv"), "share", "pv"))
	require.NoError(t, os.WriteFile(filepath.Join(f.projectScionDir, "settings.yaml"), []byte(`schema_version: "1"
harness_configs:
  test-harness:
    harness: gemini
    env:
      SCION_GROVE_ID: victim-project
`), 0644))

	ranCount := 0
	mockRT := &runtime.MockRuntime{
		NameFunc: func() string { return "docker" },
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
			"SCION_AGENT_ID": "agent-10",
		},
		SharedDirs: []api.SharedDir{{Name: "scratchpad"}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "hub project ID")
	assert.Equal(t, 0, ranCount, "Run must never be called when a harness-config env grove ID is used instead of the dispatch-provided one")
}

// TestStartSharedDirStorageNFS_GroveIDFallback_Succeeds is round 3 test
// review Low #3 / disposition item 1's T3: the SCION_GROVE_ID fallback
// (older hubs / grove-era dispatch) must still work when it is the
// dispatch-provided value, not a project-settings injection.
func TestStartSharedDirStorageNFS_GroveIDFallback_Succeeds(t *testing.T) {
	f := newSharedDirStorageRunFixture(t)
	// The host base must exist regardless of runtime.
	require.NoError(t, os.MkdirAll(filepath.Join(f.tmpDir, "srv", "share"), 0o775))
	f.writeGlobalSettings(t, sprintfServerYAML(sharedDirStorageNFSGlobalYAML, filepath.Join(f.tmpDir, "srv"), "share", "pv"))
	f.writeProjectSettings(t, "")

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
			"SCION_AGENT_ID": "agent-11",
			"SCION_GROVE_ID": "grove-pid",
			// No SCION_PROJECT_ID.
		},
		SharedDirs: []api.SharedDir{{Name: "scratchpad"}},
	})
	require.NoError(t, err)
	require.NotNil(t, capturedConfig.SharedDirStorage)
	assert.Equal(t, "projects/grove-pid/shared-dirs/scratchpad", capturedConfig.SharedDirStorage.SubPaths["scratchpad"])
}

// TestStartSharedDirStorageNFS_AmbientHubEnv_ColidingVar_StillSucceeds is the
// Start-level half of the round 6 addendum's headline fix (hy-em/hy-rev-6/
// hy-aud-6, nfs-gke UAT blocker): with the full ii2 hub 21-variable SCION_*
// env set (findings/ii2-hub-env-names.txt) PLUS a variable whose mapped key
// collides with a struct-typed VersionedSettings field, Start must still
// resolve and mount the configured nfs shared_dir_storage block — not fail
// closed just because the process's environment happens to carry unrelated
// SCION_* variables. Before this fix, LoadGlobalSettings went through the
// general env-merging loader, so SCION_AUTO_EXPOSE_PORTS=true (a real,
// common in-container variable) made the entire global settings decode
// fail, and — because the file mentions "shared_dir_storage" — Start failed
// closed for every agent on that broker.
func TestStartSharedDirStorageNFS_AmbientHubEnv_ColidingVar_StillSucceeds(t *testing.T) {
	// Round 6 dispositions nit N2/#5: mirror the loader-level test
	// (TestLoadGlobalSettings_EnvFree in pkg/config), which exercises three
	// colliding vars, not just one.
	collidingVars := []string{"SCION_AUTO_EXPOSE_PORTS", "SCION_SERVER", "SCION_TELEMETRY"}
	for _, name := range collidingVars {
		t.Run(name, func(t *testing.T) {
			f := newSharedDirStorageRunFixture(t)
			// The host base must exist regardless of runtime.
			require.NoError(t, os.MkdirAll(filepath.Join(f.tmpDir, "srv", "share"), 0o775))
			f.writeGlobalSettings(t, sprintfServerYAML(sharedDirStorageNFSGlobalYAML, filepath.Join(f.tmpDir, "srv"), "share", "pv"))
			f.writeProjectSettings(t, "")

			// This simulates the BROKER PROCESS's own ambient environment —
			// the actual vulnerable surface — not the started agent's
			// opts.Env (which LoadGlobalSettings never consulted even
			// before this fix).
			for k, v := range ii2HubEnvNames {
				t.Setenv(k, v)
			}
			t.Setenv(name, "true") // the colliding var under test

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
					"SCION_AGENT_ID":   "agent-16",
					"SCION_PROJECT_ID": "pid-ambient-env",
				},
				SharedDirs: []api.SharedDir{{Name: "scratchpad"}},
			})
			require.NoError(t, err, "an unrelated ambient SCION_* variable must never break the shared_dir_storage read")
			require.NotNil(t, capturedConfig.SharedDirStorage)
			assert.Equal(t, "projects/pid-ambient-env/shared-dirs/scratchpad", capturedConfig.SharedDirStorage.SubPaths["scratchpad"])
		})
	}
}

// TestStartSharedDirStorage_AmbientHubEnv_ColidingVar_UnsetBlock_SucceedsAsMain
// is the companion half: the same ambient env, but with NO shared_dir_storage
// block configured at all, must behave exactly like main (AC1) — the env
// leak must not fail Start even when there is nothing to protect.
func TestStartSharedDirStorage_AmbientHubEnv_ColidingVar_UnsetBlock_SucceedsAsMain(t *testing.T) {
	f := newSharedDirStorageRunFixture(t)
	f.writeGlobalSettings(t, "")
	f.writeProjectSettings(t, "")

	for k, v := range ii2HubEnvNames {
		t.Setenv(k, v)
	}
	t.Setenv("SCION_AUTO_EXPOSE_PORTS", "true")

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
		"SCION_AGENT_ID":   "agent-17",
		"SCION_PROJECT_ID": "pid-ambient-env-unset",
	}

	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: f.projectScionDir,
		NoAuth:      true,
		Env:         envMap,
		SharedDirs:  []api.SharedDir{{Name: "scratchpad"}},
	})
	require.NoError(t, err)
	assertLegacyLocalSharedDirBehavior(t, capturedConfig, envMap, f.projectScionDir)
}

// TestStartSharedDirStorage_Unset_AC1 is the unset-block variant of item 3 /
// T8 (renamed from ..._Unset_AC1AndAC3 per round 2 review finding C2/T8: the
// AC3 claims held here trivially, since nothing in this path touches
// workspace fields regardless of shared_dir_storage — the real AC3
// assertions now live in the nfs-set tests above, where shared_dir_storage
// is actually exercised). This test is AC1: with no shared_dir_storage
// configured anywhere, RunConfig.SharedDirStorage is nil, the legacy local
// shared-dir volume is still produced byte-identically, and SCION_VOLUMES is
// set (round 2 review finding T2 — this used to be proven only at the
// resolveSharedDirs level, not at the Start/RunConfig wiring level).
func TestStartSharedDirStorage_Unset_AC1(t *testing.T) {
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

	envMap := map[string]string{
		"SCION_AGENT_ID":   "agent-5",
		"SCION_PROJECT_ID": "pid-unset",
	}
	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: f.projectScionDir,
		NoAuth:      true,
		Env:         envMap,
		SharedDirs:  []api.SharedDir{{Name: "scratchpad"}},
	})
	require.NoError(t, err)

	assertLegacyLocalSharedDirBehavior(t, capturedConfig, envMap, f.projectScionDir)
}

// assertLegacyLocalSharedDirBehavior asserts the full AC1 "byte-identical to
// main" contract for the legacy local shared-dir layout: no
// SharedDirStorage, the expected /scion-volumes/scratchpad volume present
// with the legacy source, the directory actually created on disk, and
// SCION_VOLUMES set on the caller's env map. Factored out (round 4 review
// finding T2) so the unset-block test and the "no key" malformed-global
// tests, which must all produce identical output, can't drift apart.
func assertLegacyLocalSharedDirBehavior(t *testing.T, capturedConfig runtime.RunConfig, envMap map[string]string, projectScionDir string) {
	t.Helper()
	assert.Nil(t, capturedConfig.SharedDirStorage, "AC1: unset shared_dir_storage never populates SharedDirStorage")

	basePath, err := config.GetSharedDirsBasePath(projectScionDir)
	require.NoError(t, err)
	wantSource := filepath.Join(basePath, "scratchpad")
	var found bool
	for _, v := range capturedConfig.Volumes {
		if v.Target == "/scion-volumes/scratchpad" {
			found = true
			assert.Equal(t, wantSource, v.Source)
		}
	}
	assert.True(t, found, "expected the legacy local /scion-volumes/scratchpad volume (AC1)")
	info, statErr := os.Stat(wantSource)
	require.NoError(t, statErr, "the legacy shared dir should exist on disk")
	assert.True(t, info.IsDir())
	assert.Equal(t, "/scion-volumes", envMap["SCION_VOLUMES"])
}

// TestStartSharedDirStorage_NoSharedDirs_SCIONVolumesNotSet is the other
// half of item 7/T2: when a project declares no shared dirs at all,
// SCION_VOLUMES must not be set (matching pre-existing, byte-identical
// behaviour — resolveSharedDirs short-circuits on len(dirs)==0).
func TestStartSharedDirStorage_NoSharedDirs_SCIONVolumesNotSet(t *testing.T) {
	f := newSharedDirStorageRunFixture(t)
	f.writeGlobalSettings(t, "")
	f.writeProjectSettings(t, "")

	mockRT := &runtime.MockRuntime{
		NameFunc: func() string { return "docker" },
	}
	mgr := NewManager(mockRT)

	envMap := map[string]string{
		"SCION_AGENT_ID":   "agent-6",
		"SCION_PROJECT_ID": "pid-no-shared-dirs",
	}
	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: f.projectScionDir,
		NoAuth:      true,
		Env:         envMap,
		// No SharedDirs.
	})
	require.NoError(t, err)

	_, ok := envMap["SCION_VOLUMES"]
	assert.False(t, ok, "SCION_VOLUMES must not be set when there are no shared dirs")
}

// TestStartSharedDirStorage_MalformedGlobalSettings_MentionsKey_FailsClosed
// is round 2 review finding C3/T5, amended in round 3 disposition 6': a
// global settings.yaml that fails to parse, and whose raw bytes mention
// "shared_dir_storage", must not silently fall back to the local shared-dir
// layout on a broker with shared dirs to mount — that would be exactly the
// G5 split-brain failure mode this feature exists to prevent, and here the
// operator plausibly intended to configure it. Start must return the load
// error, and Run must never be called.
func TestStartSharedDirStorage_MalformedGlobalSettings_MentionsKey_FailsClosed(t *testing.T) {
	f := newSharedDirStorageRunFixture(t)
	// Invalid YAML: an unterminated flow mapping, but the raw bytes still
	// mention shared_dir_storage.
	require.NoError(t, os.WriteFile(filepath.Join(f.globalScionDir, "settings.yaml"),
		[]byte("schema_version: \"1\"\nserver: {shared_dir_storage: [\n"), 0644))
	f.writeProjectSettings(t, "")

	ranCount := 0
	mockRT := &runtime.MockRuntime{
		NameFunc: func() string { return "docker" },
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
			"SCION_AGENT_ID":   "agent-7",
			"SCION_PROJECT_ID": "pid-malformed-global",
		},
		SharedDirs: []api.SharedDir{{Name: "scratchpad"}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "server.shared_dir_storage")
	// Round 5 review nit T5: pin the wrapping context, not just the setting
	// name, so this test can't be satisfied by an unrelated error that
	// merely happens to mention "server.shared_dir_storage".
	assert.Contains(t, err.Error(), "loading global settings")
	assert.Equal(t, 0, ranCount, "Run must never be called when the global settings load fails closed")
}

// TestStartSharedDirStorage_MalformedGlobalSettings_NoKey_SucceedsAsMain is
// the amended half of disposition 6': a malformed global settings.yaml that
// does NOT mention shared_dir_storage must behave exactly like main —
// Start succeeds with the legacy local shared-dir layout — because every
// project has a default scratchpad shared dir, so failing closed here would
// regress essentially every agent start on any broker whose global settings
// happen to be broken, whether or not it ever used this feature.
func TestStartSharedDirStorage_MalformedGlobalSettings_NoKey_SucceedsAsMain(t *testing.T) {
	f := newSharedDirStorageRunFixture(t)
	// Invalid YAML that never mentions shared_dir_storage at all.
	require.NoError(t, os.WriteFile(filepath.Join(f.globalScionDir, "settings.yaml"),
		[]byte("schema_version: \"1\"\nsomething: [\n"), 0644))
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
		"SCION_AGENT_ID":   "agent-12",
		"SCION_PROJECT_ID": "pid-malformed-global-no-key",
	}
	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: f.projectScionDir,
		NoAuth:      true,
		Env:         envMap,
		SharedDirs:  []api.SharedDir{{Name: "scratchpad"}},
	})
	require.NoError(t, err)

	// Round 4 review finding T2: identical to the unset case, not merely
	// "SharedDirStorage is nil and the source matches" — also SCION_VOLUMES
	// and the on-disk directory.
	assertLegacyLocalSharedDirBehavior(t, capturedConfig, envMap, f.projectScionDir)
}

// TestStartSharedDirStorage_LegacyGlobalSettings_MentionsKey_FailsClosed is
// round 4 review finding S-L1: a global settings.yaml with no
// `schema_version: "1"` takes the LEGACY loader path, which silently drops
// the entire `server` block — LoadGlobalSettings returns err == nil with
// Server == nil, so the gErr != nil / GlobalSettingsMentions fail-closed
// branch (6') never even runs. Without an explicit check, an operator's
// shared_dir_storage=nfs config would vanish with only a generic
// "unrecognized keys" WARN, which is a worse silent failure than the
// malformed-YAML case 6' already covers. Start must error and Run must
// never be called.
func TestStartSharedDirStorage_LegacyGlobalSettings_MentionsKey_FailsClosed(t *testing.T) {
	f := newSharedDirStorageRunFixture(t)
	// Well-formed YAML, but no schema_version/harnesses/v1-runtime
	// indicators, so it takes the legacy path — which has no "server" field
	// at all, silently dropping this whole block.
	require.NoError(t, os.WriteFile(filepath.Join(f.globalScionDir, "settings.yaml"), []byte(`active_profile: local
server:
  shared_dir_storage:
    backend: nfs
    nfs:
      mount_root: /srv
      shares:
        - id: share
          pv_name: pv
`), 0644))
	f.writeProjectSettings(t, "")

	ranCount := 0
	mockRT := &runtime.MockRuntime{
		NameFunc: func() string { return "docker" },
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
			"SCION_AGENT_ID":   "agent-13",
			"SCION_PROJECT_ID": "pid-legacy-global",
		},
		SharedDirs: []api.SharedDir{{Name: "scratchpad"}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "server.shared_dir_storage")
	assert.Contains(t, err.Error(), "schema_version")
	assert.Equal(t, 0, ranCount, "Run must never be called when a legacy global file silently dropped a configured shared_dir_storage")
}

// TestStartSharedDirStorage_LegacyGlobalSettings_V1ProjectConfigsOverlay_StillFailsClosed
// is round 6 disposition item 1 (rev L1 = tst L2)'s Start-level half: the
// same legacy-global-with-block scenario as
// ..._LegacyGlobalSettings_MentionsKey_FailsClosed above, but with
// ~/.scion ALSO carrying a project-id file plus a v1-format
// project-configs settings.yaml — the exact combination that made the
// pre-fix detectHierarchyFormat-based gate see the global directory as
// "versioned" (via the project layer) and skip the fail-closed check
// entirely, silently dropping the operator's nfs config. Run must still
// never be called.
func TestStartSharedDirStorage_LegacyGlobalSettings_V1ProjectConfigsOverlay_StillFailsClosed(t *testing.T) {
	f := newSharedDirStorageRunFixture(t)
	require.NoError(t, os.WriteFile(filepath.Join(f.globalScionDir, "settings.yaml"), []byte(`active_profile: local
server:
  shared_dir_storage:
    backend: nfs
    nfs:
      mount_root: /srv
      shares:
        - id: share
          pv_name: pv
`), 0644))
	f.writeProjectSettings(t, "")

	// ~/.scion carries its own project identity, resolving to a v1-format
	// project-configs file that must NOT make the legacy gate above think
	// the global directory is versioned.
	require.NoError(t, os.WriteFile(filepath.Join(f.globalScionDir, "project-id"),
		[]byte("11111111-2222-3333-4444-555555555555\n"), 0644))
	externalDir, err := config.GetGitProjectExternalConfigDir(f.globalScionDir)
	require.NoError(t, err)
	require.NotEmpty(t, externalDir)
	require.NoError(t, os.MkdirAll(externalDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(externalDir, "settings.yaml"),
		[]byte("schema_version: \"1\"\nactive_profile: local\n"), 0644))

	ranCount := 0
	mockRT := &runtime.MockRuntime{
		NameFunc: func() string { return "docker" },
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			ranCount++
			return "mock-id", nil
		},
	}
	mgr := NewManager(mockRT)

	_, startErr := mgr.Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: f.projectScionDir,
		NoAuth:      true,
		Env: map[string]string{
			"SCION_AGENT_ID":   "agent-18",
			"SCION_PROJECT_ID": "pid-legacy-global-overlay",
		},
		SharedDirs: []api.SharedDir{{Name: "scratchpad"}},
	})
	require.Error(t, startErr)
	assert.Contains(t, startErr.Error(), "server.shared_dir_storage")
	assert.Contains(t, startErr.Error(), "schema_version")
	assert.Equal(t, 0, ranCount,
		"Run must never be called: a v1 project-configs overlay must not make the global-only legacy gate think the global file is versioned")
}

// TestStartSharedDirStorage_LegacyGlobalSettings_NoKey_SucceedsAsMain
// confirms the companion half of S-L1: a well-formed legacy-format global
// settings.yaml (no schema_version) that never mentions shared_dir_storage
// at all must behave exactly like main — Start succeeds with the legacy
// local shared-dir layout, with no error.
func TestStartSharedDirStorage_LegacyGlobalSettings_NoKey_SucceedsAsMain(t *testing.T) {
	f := newSharedDirStorageRunFixture(t)
	require.NoError(t, os.WriteFile(filepath.Join(f.globalScionDir, "settings.yaml"), []byte(`active_profile: local
`), 0644))
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
		"SCION_AGENT_ID":   "agent-14",
		"SCION_PROJECT_ID": "pid-legacy-global-no-key",
	}
	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: f.projectScionDir,
		NoAuth:      true,
		Env:         envMap,
		SharedDirs:  []api.SharedDir{{Name: "scratchpad"}},
	})
	require.NoError(t, err)
	assertLegacyLocalSharedDirBehavior(t, capturedConfig, envMap, f.projectScionDir)
}

// TestStartSharedDirStorage_V1GlobalSettings_CommentedOutBlock_SucceedsAsMain
// is round 5 review finding C1=T1=S-L2: a well-formed, successfully-loaded
// v1 global settings file whose ONLY mention of "shared_dir_storage" is a
// YAML comment (the design's documented rollback path — "comment out the
// block to disable") must behave exactly like main. Before this fix,
// GlobalSettingsMentions' raw substring check fired unconditionally on the
// "not loaded" branch, even for a file that loaded successfully with
// Server == nil, and Start failed every single time with a misleading
// "missing schema_version" error — regressing AC1 and the rollback path.
func TestStartSharedDirStorage_V1GlobalSettings_CommentedOutBlock_SucceedsAsMain(t *testing.T) {
	f := newSharedDirStorageRunFixture(t)
	require.NoError(t, os.WriteFile(filepath.Join(f.globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
# server:
#   shared_dir_storage:
#     backend: nfs
`), 0644))
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
		"SCION_AGENT_ID":   "agent-15",
		"SCION_PROJECT_ID": "pid-v1-commented-out",
	}
	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: f.projectScionDir,
		NoAuth:      true,
		Env:         envMap,
		SharedDirs:  []api.SharedDir{{Name: "scratchpad"}},
	})
	require.NoError(t, err, "a commented-out shared_dir_storage block in a well-formed v1 file must not fail Start")
	assertLegacyLocalSharedDirBehavior(t, capturedConfig, envMap, f.projectScionDir)
}

// sprintfServerYAML formats the sharedDirStorageNFSGlobalYAML template.
func sprintfServerYAML(format, mountRoot, shareID, pvName string) string {
	return fmt.Sprintf(format, mountRoot, shareID, pvName)
}
