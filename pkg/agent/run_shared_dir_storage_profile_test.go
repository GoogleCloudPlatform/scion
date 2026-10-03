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
	"encoding/json"
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

// Per-profile shared_dir_storage backend: one broker whose docker default
// profile uses the local layout while a kubernetes profile uses nfs. These
// tests drive Manager.Start end to end with settings read from the global
// settings file, the way a broker without a DB settings overlay works.

const sdsProfileShareID = "share-1"

// sdsProfileSettingsYAML returns a global settings file with a "local"
// docker profile and a "gke" kubernetes profile. gkeBackend is written to
// the gke profile's shared_dir_storage_backend (empty omits it); the global
// backend is "local" and the nfs block points at mountRoot.
func sdsProfileSettingsYAML(mountRoot, gkeBackend string) string {
	gke := "  gke:\n    runtime: k8s\n"
	if gkeBackend != "" {
		gke += "    shared_dir_storage_backend: " + gkeBackend + "\n"
	}
	return fmt.Sprintf(`schema_version: "1"
active_profile: local
runtimes:
  docker:
    type: docker
  k8s:
    type: kubernetes
profiles:
  local:
    runtime: docker
%sserver:
  shared_dir_storage:
    backend: local
    nfs:
      mount_root: %s
      shares:
        - id: %s
          pv_name: pv-1
`, gke, mountRoot, sdsProfileShareID)
}

func (f sharedDirStorageRunFixture) writeRawGlobalSettings(t *testing.T, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(f.globalScionDir, "settings.yaml"), []byte(content), 0o644))
}

type sdsCapture struct {
	ran int
	cfg runtime.RunConfig
}

func newSDSMockRuntime(name string, c *sdsCapture) *runtime.MockRuntime {
	return &runtime.MockRuntime{
		NameFunc: func() string { return name },
		RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
			c.ran++
			c.cfg = cfg
			return "mock-id", nil
		},
	}
}

func sdsStartOpts(f sharedDirStorageRunFixture, agent, profile string) api.StartOptions {
	return api.StartOptions{
		Name:        agent,
		ProjectPath: f.projectScionDir,
		Profile:     profile,
		NoAuth:      true,
		Env: map[string]string{
			"SCION_AGENT_ID":   "id-" + agent,
			"SCION_PROJECT_ID": "pid-sds",
		},
		SharedDirs: []api.SharedDir{{Name: "data"}},
	}
}

// sdsRecordedBackend returns the shared-dir storage backend recorded for
// agent, read from the record file in its agent directory.
func sdsRecordedBackend(t *testing.T, f sharedDirStorageRunFixture, agent string) string {
	t.Helper()
	backend, err := readSharedDirStorageRecord(config.ResolveAgentDir(f.projectScionDir, agent))
	require.NoError(t, err)
	return backend
}

func sdsDataDirSource(cfg runtime.RunConfig) string {
	for _, v := range cfg.Volumes {
		if v.Target == "/scion-volumes/data" {
			return v.Source
		}
	}
	return ""
}

// A docker default on the local backend and a gke profile on nfs, on one
// broker: each dispatch gets its own profile's backend.
func TestStartSharedDirStorage_PerProfileBackend(t *testing.T) {
	f := newSharedDirStorageRunFixture(t)
	mountRoot := filepath.Join(f.tmpDir, "nfs")
	require.NoError(t, os.MkdirAll(filepath.Join(mountRoot, sdsProfileShareID), 0o775))
	f.writeRawGlobalSettings(t, sdsProfileSettingsYAML(mountRoot, "nfs"))

	var k8s sdsCapture
	_, err := NewManager(newSDSMockRuntime("kubernetes", &k8s)).Start(context.Background(), sdsStartOpts(f, "gke-agent", "gke"))
	require.NoError(t, err)
	require.Equal(t, 1, k8s.ran)
	require.NotNil(t, k8s.cfg.SharedDirStorage, "the gke profile must resolve to nfs")
	assert.Equal(t, "nfs", k8s.cfg.SharedDirStorage.Backend)
	assert.Equal(t, "pv-1", k8s.cfg.SharedDirStorage.PVClaimName)
	assert.Equal(t, "projects/pid-sds/shared-dirs/data", k8s.cfg.SharedDirStorage.SubPaths["data"])
	assert.Equal(t, "nfs", sdsRecordedBackend(t, f, "gke-agent"),
		"the backend is recorded for the agent")

	var docker sdsCapture
	_, err = NewManager(newSDSMockRuntime("docker", &docker)).Start(context.Background(), sdsStartOpts(f, "docker-agent", "local"))
	require.NoError(t, err)
	require.Equal(t, 1, docker.ran)
	assert.Nil(t, docker.cfg.SharedDirStorage)
	src := sdsDataDirSource(docker.cfg)
	require.NotEmpty(t, src)
	assert.NotContains(t, src, mountRoot, "the docker profile must keep the local layout")
	assert.Equal(t, "local", sdsRecordedBackend(t, f, "docker-agent"))
}

// A missing NFS mount fails only dispatches that resolve to nfs. A docker
// dispatch on the local backend still starts.
func TestStartSharedDirStorage_MissingMountFailsOnlyNFSDispatch(t *testing.T) {
	f := newSharedDirStorageRunFixture(t)
	mountRoot := filepath.Join(f.tmpDir, "not-mounted") // never created
	f.writeRawGlobalSettings(t, sdsProfileSettingsYAML(mountRoot, "nfs"))

	var docker sdsCapture
	_, err := NewManager(newSDSMockRuntime("docker", &docker)).Start(context.Background(), sdsStartOpts(f, "docker-agent", "local"))
	require.NoError(t, err, "a local-backend dispatch must not depend on the nfs mount")
	assert.Equal(t, 1, docker.ran)

	var k8s sdsCapture
	_, err = NewManager(newSDSMockRuntime("kubernetes", &k8s)).Start(context.Background(), sdsStartOpts(f, "gke-agent", "gke"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires this broker to have the export mounted")
	assert.Contains(t, err.Error(), filepath.Join(mountRoot, sdsProfileShareID))
	assert.Equal(t, 0, k8s.ran, "the runtime must not run when the nfs mount is missing")
}

// An nfs override without a usable nfs block is an error that names the
// override key, before anything runs.
func TestStartSharedDirStorage_OverrideWithoutNFSBlock(t *testing.T) {
	f := newSharedDirStorageRunFixture(t)
	f.writeRawGlobalSettings(t, `schema_version: "1"
active_profile: local
runtimes:
  k8s:
    type: kubernetes
profiles:
  local:
    runtime: docker
  gke:
    runtime: k8s
    shared_dir_storage_backend: nfs
`)
	var k8s sdsCapture
	_, err := NewManager(newSDSMockRuntime("kubernetes", &k8s)).Start(context.Background(), sdsStartOpts(f, "gke-agent", "gke"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "profiles.gke.shared_dir_storage_backend")
	assert.Equal(t, 0, k8s.ran)
}

// Settings are read on every start: a change to the settings file applies
// to the next new agent with no restart, while an existing agent keeps the
// backend it recorded.
func TestStartSharedDirStorage_SettingsChangeAppliesAndRecordedBackendKept(t *testing.T) {
	f := newSharedDirStorageRunFixture(t)
	mountRoot := filepath.Join(f.tmpDir, "nfs")
	require.NoError(t, os.MkdirAll(filepath.Join(mountRoot, sdsProfileShareID), 0o775))
	f.writeRawGlobalSettings(t, sdsProfileSettingsYAML(mountRoot, "nfs"))

	var first sdsCapture
	mgr := NewManager(newSDSMockRuntime("kubernetes", &first))
	_, err := mgr.Start(context.Background(), sdsStartOpts(f, "old-agent", "gke"))
	require.NoError(t, err)
	require.NotNil(t, first.cfg.SharedDirStorage)
	require.Equal(t, "nfs", sdsRecordedBackend(t, f, "old-agent"))

	// Same manager (no restart); the gke profile now selects local.
	f.writeRawGlobalSettings(t, sdsProfileSettingsYAML(mountRoot, "local"))

	var fresh sdsCapture
	_, err = NewManager(newSDSMockRuntime("kubernetes", &fresh)).Start(context.Background(), sdsStartOpts(f, "new-agent", "gke"))
	require.NoError(t, err)
	assert.Nil(t, fresh.cfg.SharedDirStorage, "a new agent uses the changed setting")
	assert.Equal(t, "local", sdsRecordedBackend(t, f, "new-agent"))

	var restart sdsCapture
	_, err = NewManager(newSDSMockRuntime("kubernetes", &restart)).Start(context.Background(), sdsStartOpts(f, "old-agent", ""))
	require.NoError(t, err)
	require.Equal(t, 1, restart.ran)
	require.NotNil(t, restart.cfg.SharedDirStorage, "an existing agent keeps its recorded nfs backend")
	assert.Equal(t, "nfs", restart.cfg.SharedDirStorage.Backend)
	assert.Equal(t, "nfs", sdsRecordedBackend(t, f, "old-agent"))
}

// A restart of an agent that recorded nfs, after the nfs block was removed,
// fails with a clear error before the runtime runs or any host path is
// touched.
func TestStartSharedDirStorage_RecordedNFSBlockGone(t *testing.T) {
	f := newSharedDirStorageRunFixture(t)
	mountRoot := filepath.Join(f.tmpDir, "nfs")
	require.NoError(t, os.MkdirAll(filepath.Join(mountRoot, sdsProfileShareID), 0o775))
	f.writeRawGlobalSettings(t, sdsProfileSettingsYAML(mountRoot, "nfs"))

	var first sdsCapture
	_, err := NewManager(newSDSMockRuntime("kubernetes", &first)).Start(context.Background(), sdsStartOpts(f, "old-agent", "gke"))
	require.NoError(t, err)

	f.writeRawGlobalSettings(t, `schema_version: "1"
active_profile: local
runtimes:
  k8s:
    type: kubernetes
profiles:
  local:
    runtime: docker
  gke:
    runtime: k8s
`)
	// Remove the old tree so any host operation would be visible.
	require.NoError(t, os.RemoveAll(filepath.Join(f.tmpDir, "nfs")))

	var restart sdsCapture
	_, err = NewManager(newSDSMockRuntime("kubernetes", &restart)).Start(context.Background(), sdsStartOpts(f, "old-agent", ""))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "was created with the nfs shared-dir storage backend")
	assert.Equal(t, 0, restart.ran)
	_, statErr := os.Stat(filepath.Join(f.tmpDir, "nfs"))
	assert.True(t, os.IsNotExist(statErr), "no host path may be created")
}

// An agent with no recorded backend (created before it was recorded) uses the current resolution, and records it.
func TestStartSharedDirStorage_NoRecordedBackendUsesCurrent(t *testing.T) {
	f := newSharedDirStorageRunFixture(t)
	mountRoot := filepath.Join(f.tmpDir, "nfs")
	require.NoError(t, os.MkdirAll(filepath.Join(mountRoot, sdsProfileShareID), 0o775))
	f.writeRawGlobalSettings(t, sdsProfileSettingsYAML(mountRoot, "local"))

	var first sdsCapture
	_, err := NewManager(newSDSMockRuntime("kubernetes", &first)).Start(context.Background(), sdsStartOpts(f, "old-agent", "gke"))
	require.NoError(t, err)
	require.Nil(t, first.cfg.SharedDirStorage)

	// Simulate an agent from before the backend was recorded.
	require.NoError(t, os.Remove(filepath.Join(config.ResolveAgentDir(f.projectScionDir, "old-agent"), sharedDirStorageRecordFile)))
	require.Empty(t, sdsRecordedBackend(t, f, "old-agent"))

	f.writeRawGlobalSettings(t, sdsProfileSettingsYAML(mountRoot, "nfs"))

	var restart sdsCapture
	_, err = NewManager(newSDSMockRuntime("kubernetes", &restart)).Start(context.Background(), sdsStartOpts(f, "old-agent", ""))
	require.NoError(t, err)
	require.NotNil(t, restart.cfg.SharedDirStorage, "with nothing recorded, the current resolution applies")
	assert.Equal(t, "nfs", restart.cfg.SharedDirStorage.Backend)
	assert.Equal(t, "nfs", sdsRecordedBackend(t, f, "old-agent"))
}

// A project's own settings cannot set the per-profile backend.
func TestStartSharedDirStorage_ProjectOverrideIgnored(t *testing.T) {
	f := newSharedDirStorageRunFixture(t)
	mountRoot := filepath.Join(f.tmpDir, "nfs")
	f.writeRawGlobalSettings(t, sdsProfileSettingsYAML(mountRoot, ""))
	require.NoError(t, os.WriteFile(filepath.Join(f.projectScionDir, "settings.yaml"), []byte(`schema_version: "1"
runtimes:
  k8s:
    type: kubernetes
    shared_dir_storage_backend: nfs
profiles:
  gke:
    runtime: k8s
    shared_dir_storage_backend: nfs
`), 0o644))

	var k8s sdsCapture
	_, err := NewManager(newSDSMockRuntime("kubernetes", &k8s)).Start(context.Background(), sdsStartOpts(f, "gke-agent", "gke"))
	require.NoError(t, err)
	assert.Nil(t, k8s.cfg.SharedDirStorage, "project settings must not select the nfs backend")
	_, statErr := os.Stat(mountRoot)
	assert.True(t, os.IsNotExist(statErr))
}

// With a DB settings overlay installed (co-located hub on a database that
// stores runtimes and profiles), the overlay's profiles decide the
// backend, and an overlay update applies to the next start.
func TestStartSharedDirStorage_OverlayProfiles(t *testing.T) {
	old := config.GetGlobalSettingsOverlay()
	t.Cleanup(func() { config.SetGlobalSettingsOverlay(old) })

	f := newSharedDirStorageRunFixture(t)
	mountRoot := filepath.Join(f.tmpDir, "nfs")
	require.NoError(t, os.MkdirAll(filepath.Join(mountRoot, sdsProfileShareID), 0o775))
	// The file has no override; only the overlay sets one.
	f.writeRawGlobalSettings(t, sdsProfileSettingsYAML(mountRoot, ""))

	runtimes := map[string]config.V1RuntimeConfig{"docker": {Type: "docker"}, "k8s": {Type: "kubernetes"}}
	o := config.NewSettingsOverlay()
	o.Update(runtimes, map[string]config.V1ProfileConfig{
		"local": {Runtime: "docker"},
		"gke":   {Runtime: "k8s", SharedDirStorageBackend: "nfs"},
	}, nil, "")
	config.SetGlobalSettingsOverlay(o)

	var a sdsCapture
	_, err := NewManager(newSDSMockRuntime("kubernetes", &a)).Start(context.Background(), sdsStartOpts(f, "agent-a", "gke"))
	require.NoError(t, err)
	require.NotNil(t, a.cfg.SharedDirStorage, "the overlay's profile override must apply")
	assert.Equal(t, "nfs", a.cfg.SharedDirStorage.Backend)

	o.Update(nil, map[string]config.V1ProfileConfig{
		"local": {Runtime: "docker"},
		"gke":   {Runtime: "k8s"},
	}, nil, "")

	var b sdsCapture
	_, err = NewManager(newSDSMockRuntime("kubernetes", &b)).Start(context.Background(), sdsStartOpts(f, "agent-b", "gke"))
	require.NoError(t, err)
	assert.Nil(t, b.cfg.SharedDirStorage, "an overlay update applies to the next start")
}

// sdsNoNFSBlockSettingsYAML is sdsProfileSettingsYAML with no
// server.shared_dir_storage block at all, and gkeBackend written to the gke
// profile's shared_dir_storage_backend.
func sdsNoNFSBlockSettingsYAML(gkeBackend string) string {
	return `schema_version: "1"
active_profile: local
runtimes:
  docker:
    type: docker
  k8s:
    type: kubernetes
profiles:
  local:
    runtime: docker
  gke:
    runtime: k8s
    shared_dir_storage_backend: ` + gkeBackend + "\n"
}

// Reprovisioning an agent keeps its recorded backend: a later settings
// change still does not move the agent's shared dirs.
func TestStartSharedDirStorage_ReprovisionKeepsRecordedBackend(t *testing.T) {
	f := newSharedDirStorageRunFixture(t)
	mountRoot := filepath.Join(f.tmpDir, "nfs")
	require.NoError(t, os.MkdirAll(filepath.Join(mountRoot, sdsProfileShareID), 0o775))
	f.writeRawGlobalSettings(t, sdsProfileSettingsYAML(mountRoot, "nfs"))
	workspace := filepath.Join(f.tmpDir, "checkout")
	require.NoError(t, os.MkdirAll(workspace, 0o755))

	opts := sdsStartOpts(f, "old-agent", "gke")
	opts.Workspace = workspace
	var first sdsCapture
	_, err := NewManager(newSDSMockRuntime("kubernetes", &first)).Start(context.Background(), opts)
	require.NoError(t, err)
	require.Equal(t, "nfs", sdsRecordedBackend(t, f, "old-agent"))

	_, err = NewManager(newSDSMockRuntime("kubernetes", &sdsCapture{})).Reprovision(context.Background(), api.StartOptions{
		Name: "old-agent", ProjectPath: f.projectScionDir, Profile: "gke", Workspace: workspace,
	})
	require.NoError(t, err)
	require.Equal(t, "nfs", sdsRecordedBackend(t, f, "old-agent"), "reprovisioning must keep the recorded backend")

	// Settings change after create: the gke profile now selects local.
	f.writeRawGlobalSettings(t, sdsProfileSettingsYAML(mountRoot, "local"))

	restartOpts := sdsStartOpts(f, "old-agent", "")
	restartOpts.Workspace = workspace
	var restart sdsCapture
	_, err = NewManager(newSDSMockRuntime("kubernetes", &restart)).Start(context.Background(), restartOpts)
	require.NoError(t, err)
	require.Equal(t, 1, restart.ran)
	require.NotNil(t, restart.cfg.SharedDirStorage, "the agent keeps nfs after reprovision and a settings change")
	assert.Equal(t, "nfs", restart.cfg.SharedDirStorage.Backend)
}

// An agent with a recorded backend keeps it even when its profile's
// override is later edited to a value that fails validation; the agent
// does not use the override.
func TestStartSharedDirStorage_RecordedBackendSkipsInvalidOverride(t *testing.T) {
	for _, tc := range []struct {
		name     string
		settings string
	}{
		{"nfs override without nfs block", sdsNoNFSBlockSettingsYAML("nfs")},
		{"unknown backend value", sdsNoNFSBlockSettingsYAML("nsf")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSharedDirStorageRunFixture(t)
			mountRoot := filepath.Join(f.tmpDir, "nfs")
			f.writeRawGlobalSettings(t, sdsProfileSettingsYAML(mountRoot, "local"))

			var first sdsCapture
			_, err := NewManager(newSDSMockRuntime("kubernetes", &first)).Start(context.Background(), sdsStartOpts(f, "old-agent", "gke"))
			require.NoError(t, err)
			require.Equal(t, "local", sdsRecordedBackend(t, f, "old-agent"))

			f.writeRawGlobalSettings(t, tc.settings)

			var restart sdsCapture
			_, err = NewManager(newSDSMockRuntime("kubernetes", &restart)).Start(context.Background(), sdsStartOpts(f, "old-agent", "gke"))
			require.NoError(t, err, "a recorded local agent must not fail on an override it does not use")
			assert.Equal(t, 1, restart.ran)
			assert.Nil(t, restart.cfg.SharedDirStorage)
			assert.Equal(t, "local", sdsRecordedBackend(t, f, "old-agent"))

			// A new agent on the same profile has nothing recorded, so the
			// invalid override is still an error for it.
			var fresh sdsCapture
			_, err = NewManager(newSDSMockRuntime("kubernetes", &fresh)).Start(context.Background(), sdsStartOpts(f, "new-agent", "gke"))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "profiles.gke.shared_dir_storage_backend")
			assert.Equal(t, 0, fresh.ran)
		})
	}
}

// When the global settings fail to load, a start that falls back to the
// local layout does not record that fallback, so the agent picks up its
// configured backend once the settings are fixed.
func TestStartSharedDirStorage_SettingsLoadFailureNotRecorded(t *testing.T) {
	f := newSharedDirStorageRunFixture(t)
	mountRoot := filepath.Join(f.tmpDir, "nfs")
	require.NoError(t, os.MkdirAll(filepath.Join(mountRoot, sdsProfileShareID), 0o775))
	// Malformed YAML that does not mention the shared-dir storage key, so
	// the start proceeds with the local layout.
	f.writeRawGlobalSettings(t, "schema_version: \"1\"\nprofiles: [unclosed\n")

	var broken sdsCapture
	_, err := NewManager(newSDSMockRuntime("kubernetes", &broken)).Start(context.Background(), sdsStartOpts(f, "gke-agent", "gke"))
	require.NoError(t, err)
	require.Equal(t, 1, broken.ran)
	assert.Nil(t, broken.cfg.SharedDirStorage)
	assert.Empty(t, sdsRecordedBackend(t, f, "gke-agent"), "a fallback after a settings load error must not be recorded")

	f.writeRawGlobalSettings(t, sdsProfileSettingsYAML(mountRoot, "nfs"))

	var fixed sdsCapture
	_, err = NewManager(newSDSMockRuntime("kubernetes", &fixed)).Start(context.Background(), sdsStartOpts(f, "gke-agent", "gke"))
	require.NoError(t, err)
	require.NotNil(t, fixed.cfg.SharedDirStorage, "once settings load, the configured backend applies")
	assert.Equal(t, "nfs", fixed.cfg.SharedDirStorage.Backend)
	assert.Equal(t, "nfs", sdsRecordedBackend(t, f, "gke-agent"))
}

// The recorded backend lives in the agent directory, outside the agent
// home that the container mounts. agent-info.json in the home neither
// carries it nor influences it.
func TestStartSharedDirStorage_RecordOutsideAgentHome(t *testing.T) {
	f := newSharedDirStorageRunFixture(t)
	mountRoot := filepath.Join(f.tmpDir, "nfs")
	require.NoError(t, os.MkdirAll(filepath.Join(mountRoot, sdsProfileShareID), 0o775))
	f.writeRawGlobalSettings(t, sdsProfileSettingsYAML(mountRoot, "nfs"))

	_, err := NewManager(newSDSMockRuntime("kubernetes", &sdsCapture{})).Start(context.Background(), sdsStartOpts(f, "gke-agent", "gke"))
	require.NoError(t, err)

	home := config.GetAgentHomePath(f.projectScionDir, "gke-agent")
	recordPath := filepath.Join(config.ResolveAgentDir(f.projectScionDir, "gke-agent"), sharedDirStorageRecordFile)
	require.FileExists(t, recordPath)
	rel, err := filepath.Rel(home, recordPath)
	require.NoError(t, err)
	assert.True(t, rel == ".." || len(rel) > 2 && rel[:3] == ".."+string(filepath.Separator),
		"the record %s must not be inside the agent home %s", recordPath, home)

	infoPath := filepath.Join(home, "agent-info.json")
	data, err := os.ReadFile(infoPath)
	require.NoError(t, err)
	var info map[string]any
	require.NoError(t, json.Unmarshal(data, &info))
	assert.NotContains(t, info, "sharedDirStorageBackend", "agent-info.json must not carry the backend")

	// A value written into agent-info.json from inside the container has
	// no effect on the next start.
	info["sharedDirStorageBackend"] = "local"
	data, err = json.Marshal(info)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(infoPath, data, 0o644))

	var restart sdsCapture
	_, err = NewManager(newSDSMockRuntime("kubernetes", &restart)).Start(context.Background(), sdsStartOpts(f, "gke-agent", "gke"))
	require.NoError(t, err)
	require.NotNil(t, restart.cfg.SharedDirStorage)
	assert.Equal(t, "nfs", restart.cfg.SharedDirStorage.Backend)
}

// A record that cannot be parsed is an error, not a silent fallback to
// the current settings.
func TestStartSharedDirStorage_DamagedRecordFails(t *testing.T) {
	f := newSharedDirStorageRunFixture(t)
	mountRoot := filepath.Join(f.tmpDir, "nfs")
	require.NoError(t, os.MkdirAll(filepath.Join(mountRoot, sdsProfileShareID), 0o775))
	f.writeRawGlobalSettings(t, sdsProfileSettingsYAML(mountRoot, "nfs"))

	_, err := NewManager(newSDSMockRuntime("kubernetes", &sdsCapture{})).Start(context.Background(), sdsStartOpts(f, "gke-agent", "gke"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(config.ResolveAgentDir(f.projectScionDir, "gke-agent"), sharedDirStorageRecordFile), []byte("{not json"), 0o644))

	var restart sdsCapture
	_, err = NewManager(newSDSMockRuntime("kubernetes", &restart)).Start(context.Background(), sdsStartOpts(f, "gke-agent", "gke"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "shared-dir storage record")
	assert.Equal(t, 0, restart.ran)
}
