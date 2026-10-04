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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
)

// These tests drive Manager.Start end to end, so the runtime name and the
// experiment flag are taken where Start passes them to the resolver.

func homeStorageStartSettings(mountRoot string) string {
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
    home_storage_backend: nfs
    shared_dir_storage_backend: nfs
  gke:
    runtime: k8s
    home_storage_backend: nfs
    shared_dir_storage_backend: nfs
server:
  home_storage:
    allow_incomplete_phases: true
  shared_dir_storage:
    backend: local
    nfs:
      mount_root: %s
      shares:
        - id: share-1
          pv_name: pv-1
`, mountRoot)
}

func homeStorageStartOpts(f sharedDirStorageRunFixture, agent, profile string) api.StartOptions {
	return api.StartOptions{
		Name:        agent,
		ProjectPath: f.projectScionDir,
		Profile:     profile,
		NoAuth:      true,
		Env: map[string]string{
			"SCION_AGENT_ID":   hsTestAgentID,
			"SCION_PROJECT_ID": hsTestProjectID,
		},
	}
}

func withNFSHomeExperiment(ctx context.Context) context.Context {
	return api.ContextWithHubAgentDefaults(ctx, &api.HubAgentDefaults{Experiments: []string{experiments.K8sNFSHome}})
}

func startHomeRecord(t *testing.T, f sharedDirStorageRunFixture, agent string) *homeStorageRecord {
	t.Helper()
	rec, err := readHomeStorageRecord(config.ResolveAgentDir(f.projectScionDir, agent))
	require.NoError(t, err)
	return rec
}

// A docker dispatch with nfs configured on its profile and the experiment
// on gets a local home and never touches the record.
func TestStartHomeStorage_DockerDispatchIsLocal(t *testing.T) {
	f := newSharedDirStorageRunFixture(t)
	f.writeRawGlobalSettings(t, homeStorageStartSettings(f.tmpDir))
	var c sdsCapture
	_, err := NewManager(newSDSMockRuntime("docker", &c)).Start(withNFSHomeExperiment(context.Background()), homeStorageStartOpts(f, "dock-agent", "local"))
	require.NoError(t, err)
	require.Equal(t, 1, c.ran)
	assert.Empty(t, c.cfg.HomeStorageBackend)
	assert.Equal(t, &homeStorageRecord{Backend: homeStoragePending}, startHomeRecord(t, f, "dock-agent"))
}

// A Kubernetes dispatch with the experiment off gets, and records, a local
// home.
func TestStartHomeStorage_ExperimentOffIsLocal(t *testing.T) {
	f := newSharedDirStorageRunFixture(t)
	f.writeRawGlobalSettings(t, homeStorageStartSettings(f.tmpDir))
	var c sdsCapture
	_, err := NewManager(newSDSMockRuntime("kubernetes", &c)).Start(context.Background(), homeStorageStartOpts(f, "gke-agent", "gke"))
	require.NoError(t, err)
	require.Equal(t, 1, c.ran)
	assert.Empty(t, c.cfg.HomeStorageBackend)
	assert.Equal(t, &homeStorageRecord{Backend: "local"}, startHomeRecord(t, f, "gke-agent"))
}

// A Kubernetes dispatch with the experiment on resolves to an NFS home,
// which this version refuses before any pod is started or record written.
func TestStartHomeStorage_NFSRefusedInThisVersion(t *testing.T) {
	f := newSharedDirStorageRunFixture(t)
	f.writeRawGlobalSettings(t, homeStorageStartSettings(f.tmpDir))
	var c sdsCapture
	_, err := NewManager(newSDSMockRuntime("kubernetes", &c)).Start(withNFSHomeExperiment(context.Background()), homeStorageStartOpts(f, "gke-agent", "gke"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not available in this version")
	assert.Equal(t, 0, c.ran)
	assert.Equal(t, &homeStorageRecord{Backend: homeStoragePending}, startHomeRecord(t, f, "gke-agent"))
}
