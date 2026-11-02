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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	dynfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/k8s"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// refusalSettings is a global settings file with one profile carrying a
// removed storage override (on the profile), one whose runtime entry
// carries one, and one with neither. The shared-dir block is global with
// backend local, as before server.project_storage.
const refusalSettings = `schema_version: "1"
active_profile: plain
runtimes:
  cluster:
    type: kubernetes
  cluster-homes:
    type: kubernetes
    home_storage_backend: nfs
profiles:
  shared-nfs:
    runtime: cluster
    shared_dir_storage_backend: nfs
  per-dir:
    runtime: cluster
    shared_dir_storage_backends:
      scratchpad: nfs
  runtime-homes:
    runtime: cluster-homes
  plain:
    runtime: cluster
server:
  shared_dir_storage:
    backend: local
    nfs:
      mount_root: /mnt/share
      subpath_root: tree/projects
      shares:
        - id: share-a
          server: 192.0.2.10
          export: /export/a
          pv_name: share-a-pv
`

// refusalGlobalHome points HOME at a temp dir holding content as the
// global settings file and installs overlay (nil: none).
func refusalGlobalHome(t *testing.T, content string, overlay *config.SettingsOverlay) {
	t.Helper()
	old := config.GetGlobalSettingsOverlay()
	t.Cleanup(func() { config.SetGlobalSettingsOverlay(old) })
	config.SetGlobalSettingsOverlay(overlay)
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".scion")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte(content), 0o644))
}

// storageRefusalOf asserts err is a *config.StorageMigrationRequiredError
// whose log and public texts both name each of wantKeys, and returns it.
func storageRefusalOf(t *testing.T, err error, wantKeys ...string) *config.StorageMigrationRequiredError {
	t.Helper()
	var se *config.StorageMigrationRequiredError
	require.ErrorAs(t, err, &se)
	for _, k := range wantKeys {
		assert.Contains(t, se.Error(), k)
		assert.Contains(t, se.PublicMessage(), k)
	}
	return se
}

// requireStorageRefusal is storageRefusalOf for callers that do not read
// the refusal.
func requireStorageRefusal(t *testing.T, err error, wantKeys ...string) {
	t.Helper()
	_ = storageRefusalOf(t, err, wantKeys...)
}

func TestCheckStartStorageRefusals_ProfileChain(t *testing.T) {
	refusalGlobalHome(t, refusalSettings, nil)
	ctx := context.Background()

	se := storageRefusalOf(t, checkStartStorageRefusals(ctx, "", "shared-nfs"), "profiles.shared-nfs.shared_dir_storage_backend")
	assert.Contains(t, se.Error(), `pv_name "share-a-pv"`, "the log text names the claim")
	assert.NotContains(t, se.PublicMessage(), "share-a-pv", "the public text names no claim")
	assert.NotContains(t, se.Error(), "runtimes.cluster-homes", "only the start's own chain is named")

	requireStorageRefusal(t, checkStartStorageRefusals(ctx, "", "per-dir"), "profiles.per-dir.shared_dir_storage_backends")
	requireStorageRefusal(t, checkStartStorageRefusals(ctx, "", "runtime-homes"), "runtimes.cluster-homes.home_storage_backend")

	// Unaffected profiles are admitted, including the active profile when
	// the start names none, and a profile the settings do not define.
	assert.NoError(t, checkStartStorageRefusals(ctx, "", "plain"))
	assert.NoError(t, checkStartStorageRefusals(ctx, "", ""))
	assert.NoError(t, checkStartStorageRefusals(ctx, "", "unknown"))
}

func TestCheckStartStorageRefusals_HubStoredProfiles(t *testing.T) {
	o := config.NewSettingsOverlay()
	o.Update(
		map[string]config.V1RuntimeConfig{"cluster": {Type: "kubernetes"}},
		map[string]config.V1ProfileConfig{
			"stored-nfs": {Runtime: "cluster", HomeStorageLeaf: "pod"},
			"plain":      {Runtime: "cluster"},
		}, nil)
	refusalGlobalHome(t, "schema_version: \"1\"\nactive_profile: plain\n", o)
	ctx := context.Background()

	requireStorageRefusal(t, checkStartStorageRefusals(ctx, "", "stored-nfs"), "profiles.stored-nfs.home_storage_leaf")
	assert.NoError(t, checkStartStorageRefusals(ctx, "", "plain"))
}

func TestCheckStartStorageRefusals_UnloadableSettingsRefuseNothing(t *testing.T) {
	refusalGlobalHome(t, "schema_version: \"1\"\nprofiles: [not, a, map]\n", nil)
	assert.NoError(t, checkStartStorageRefusals(context.Background(), "", "shared-nfs"))
}

func writeSharedDirRecord(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return path
}

func TestSharedDirRecordPinRefusal(t *testing.T) {
	local := projectfs.Layout{Backend: projectfs.BackendLocal}
	shared := projectfs.Layout{Backend: projectfs.BackendNFS}
	sd := &config.V1SharedDirStorageConfig{NFS: &config.V1NFSConfig{SubPathRoot: "tree/projects", Shares: []config.V1NFSShare{{ID: "a", PVName: "share-a-pv"}}}}
	cases := []struct {
		name       string
		file       string
		content    string
		layout     projectfs.Layout
		wantRefuse string
	}{
		{name: "live record pins nfs, local layout", file: legacySharedDirStorageRecordFile, content: `{"backend":"nfs"}`, layout: local, wantRefuse: "(all)"},
		{name: "retired record pins one dir, local layout", file: legacySharedDirStorageRecordFile + retiredSharedDirRecordSuffix,
			content: `{"backend":"local","dirs":{"scratchpad":"nfs"}}`, layout: local, wantRefuse: "(scratchpad)"},
		{name: "record pins nfs, shared layout", file: legacySharedDirStorageRecordFile, content: `{"backend":"nfs"}`, layout: shared},
		{name: "record pins local, local layout", file: legacySharedDirStorageRecordFile, content: `{"backend":"local"}`, layout: local},
		{name: "unreadable record", file: legacySharedDirStorageRecordFile, content: `{`, layout: local},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := writeSharedDirRecord(t, dir, tc.file, tc.content)
			err := sharedDirRecordPinRefusal(dir, tc.layout, sd)
			if tc.wantRefuse == "" {
				assert.NoError(t, err)
				return
			}
			se := storageRefusalOf(t, err, tc.wantRefuse, tc.file, "server.shared_dir_storage.backend")
			assert.Contains(t, se.Error(), `pv_name "share-a-pv"`)
			_, statErr := os.Stat(path)
			assert.NoError(t, statErr, "the record is kept")
		})
	}
	assert.NoError(t, sharedDirRecordPinRefusal(t.TempDir(), local, sd), "no record")
}

// TestStart_RefusedStorageCreatesNothing: a start refused for a removed
// storage override, or for a per-agent nfs pin under a local layout, makes
// no runtime call at all: on Kubernetes no claim, pod or secret is created
// (ptone/scion#6371).
func TestStart_RefusedStorageCreatesNothing(t *testing.T) {
	refusalGlobalHome(t, refusalSettings, nil)
	projectDir := filepath.Join(t.TempDir(), ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))

	cs := k8sfake.NewClientset()
	rt := runtime.NewKubernetesRuntime(k8s.NewTestClient(dynfake.NewSimpleDynamicClient(k8sruntime.NewScheme()), cs))
	mgr := NewManager(rt)

	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "agent-1",
		ProjectPath: projectDir,
		Profile:     "shared-nfs",
		BrokerMode:  true,
		SharedDirs:  []api.SharedDir{{Name: "scratchpad"}},
	})
	requireStorageRefusal(t, err, "profiles.shared-nfs.shared_dir_storage_backend")
	assert.Empty(t, cs.Actions(), "a refused start must not call the cluster (no claim, pod or secret)")

	// The pinned agent: an unaffected profile, but its record pins nfs
	// while the layout is local.
	agentDir, _, err := AgentStateDir(projectDir, "agent-2", false, "", true)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(agentDir, 0o755))
	recPath := writeSharedDirRecord(t, agentDir, legacySharedDirStorageRecordFile, `{"backend":"nfs"}`)
	_, err = mgr.Start(context.Background(), api.StartOptions{
		Name:        "agent-2",
		ProjectPath: projectDir,
		Profile:     "plain",
		BrokerMode:  true,
		SharedDirs:  []api.SharedDir{{Name: "scratchpad"}},
	})
	requireStorageRefusal(t, err, legacySharedDirStorageRecordFile)
	assert.Empty(t, cs.Actions(), "a refused start must not call the cluster")
	_, statErr := os.Stat(recPath)
	assert.NoError(t, statErr, "the record is kept")
}

// CheckProfileStorageRefusal is the profile check the runtime broker runs
// at create admission.
func TestCheckProfileStorageRefusal(t *testing.T) {
	refusalGlobalHome(t, refusalSettings, nil)
	requireStorageRefusal(t, CheckProfileStorageRefusal(context.Background(), "shared-nfs"), "profiles.shared-nfs.shared_dir_storage_backend")
	assert.NoError(t, CheckProfileStorageRefusal(context.Background(), "plain"))
}
