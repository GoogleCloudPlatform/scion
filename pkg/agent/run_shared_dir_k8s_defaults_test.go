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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Shared-dir PVC defaults (shared_dir_storage_class / shared_dir_size) from
// settings runtime and profile entries reach RunConfig.Kubernetes through
// the real Manager.Start path, with template/agent > profile > runtime
// precedence (ptone/scion#2634).

const sharedDirK8sDefaultsSettings = `schema_version: "1"
active_profile: gke
runtimes:
  gke-autopilot:
    type: kubernetes
    shared_dir_storage_class: rt-rwx
    shared_dir_size: 1Ti
profiles:
  gke:
    runtime: gke-autopilot
  gke-premium:
    runtime: gke-autopilot
    shared_dir_storage_class: prof-rwx
`

func startForSharedDirK8sDefaults(t *testing.T, runtimeName, templateJSON string, opts api.StartOptions) runtime.RunConfig {
	t.Helper()
	f := newSharedDirStorageRunFixture(t)
	require.NoError(t, os.WriteFile(filepath.Join(f.globalScionDir, "settings.yaml"),
		[]byte(sharedDirK8sDefaultsSettings), 0644))
	if templateJSON != "" {
		require.NoError(t, os.WriteFile(filepath.Join(f.globalScionDir, "templates", "default", "scion-agent.json"),
			[]byte(templateJSON), 0644))
	}

	var captured runtime.RunConfig
	var ran int
	mockRT := &runtime.MockRuntime{
		NameFunc: func() string { return runtimeName },
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			captured = config
			ran++
			return "mock-id", nil
		},
	}
	opts.Name = "sd-agent"
	opts.ProjectPath = f.projectScionDir
	opts.NoAuth = true
	opts.Env = map[string]string{"SCION_AGENT_ID": "agent-sd", "SCION_PROJECT_ID": "pid-sd"}
	opts.SharedDirs = []api.SharedDir{{Name: "scratchpad"}}
	_, err := NewManager(mockRT).Start(context.Background(), opts)
	require.NoError(t, err)
	require.Equal(t, 1, ran)
	return captured
}

func TestStartSharedDirK8sDefaults_RuntimeEntry(t *testing.T) {
	cfg := startForSharedDirK8sDefaults(t, "kubernetes", "", api.StartOptions{})
	require.NotNil(t, cfg.Kubernetes)
	assert.Equal(t, "rt-rwx", cfg.Kubernetes.SharedDirStorageClass)
	assert.Equal(t, "1Ti", cfg.Kubernetes.SharedDirSize)
}

func TestStartSharedDirK8sDefaults_ProfileWinsOverRuntime(t *testing.T) {
	cfg := startForSharedDirK8sDefaults(t, "kubernetes", "", api.StartOptions{Profile: "gke-premium"})
	require.NotNil(t, cfg.Kubernetes)
	assert.Equal(t, "prof-rwx", cfg.Kubernetes.SharedDirStorageClass)
	assert.Equal(t, "1Ti", cfg.Kubernetes.SharedDirSize, "size not set on the profile falls back to the runtime entry")
}

func TestStartSharedDirK8sDefaults_TemplateWinsOverProfile(t *testing.T) {
	cfg := startForSharedDirK8sDefaults(t, "kubernetes",
		`{"default_harness_config": "test-harness", "kubernetes": {"shared_dir_storage_class": "tpl-rwx"}}`,
		api.StartOptions{Profile: "gke-premium"})
	require.NotNil(t, cfg.Kubernetes)
	assert.Equal(t, "tpl-rwx", cfg.Kubernetes.SharedDirStorageClass)
	assert.Equal(t, "1Ti", cfg.Kubernetes.SharedDirSize)
}

func TestStartSharedDirK8sDefaults_InlineAgentConfigWins(t *testing.T) {
	cfg := startForSharedDirK8sDefaults(t, "kubernetes", "", api.StartOptions{
		Profile: "gke-premium",
		InlineConfig: &api.ScionConfig{Kubernetes: &api.KubernetesConfig{
			SharedDirStorageClass: "agent-rwx", SharedDirSize: "5Gi",
		}},
	})
	require.NotNil(t, cfg.Kubernetes)
	assert.Equal(t, "agent-rwx", cfg.Kubernetes.SharedDirStorageClass)
	assert.Equal(t, "5Gi", cfg.Kubernetes.SharedDirSize)
}

// The settings defaults are Kubernetes-only: other runtimes get no
// Kubernetes block from them.
func TestStartSharedDirK8sDefaults_NotAppliedOnOtherRuntimes(t *testing.T) {
	cfg := startForSharedDirK8sDefaults(t, "docker", "", api.StartOptions{})
	assert.Nil(t, cfg.Kubernetes)
}
