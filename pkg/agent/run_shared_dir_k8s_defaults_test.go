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
	"strings"
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
  plain-k8s:
    type: kubernetes
profiles:
  gke:
    runtime: gke-autopilot
  gke-premium:
    runtime: gke-autopilot
    shared_dir_storage_class: prof-rwx
  other:
    runtime: gke-autopilot
    shared_dir_storage_class: other-rwx
  size-only:
    runtime: plain-k8s
    shared_dir_size: 50Gi
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

// A restart without a profile uses the profile the agent was created with
// (its saved profile), not active_profile, when resolving the defaults.
func TestStartSharedDirK8sDefaults_RestartUsesSavedProfile(t *testing.T) {
	f := newSharedDirStorageRunFixture(t)
	settings := strings.Replace(sharedDirK8sDefaultsSettings, "active_profile: gke", "active_profile: other", 1)
	require.NoError(t, os.WriteFile(filepath.Join(f.globalScionDir, "settings.yaml"), []byte(settings), 0644))

	var captured []runtime.RunConfig
	mockRT := &runtime.MockRuntime{
		NameFunc: func() string { return "kubernetes" },
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			captured = append(captured, config)
			return "mock-id", nil
		},
	}
	mgr := NewManager(mockRT)
	start := func(profile string) {
		t.Helper()
		_, err := mgr.Start(context.Background(), api.StartOptions{
			Name:        "sd-agent",
			ProjectPath: f.projectScionDir,
			Profile:     profile,
			NoAuth:      true,
			Env:         map[string]string{"SCION_AGENT_ID": "agent-sd", "SCION_PROJECT_ID": "pid-sd"},
			SharedDirs:  []api.SharedDir{{Name: "scratchpad"}},
		})
		require.NoError(t, err)
	}

	start("gke-premium")
	start("")
	require.Len(t, captured, 2)
	for i, cfg := range captured {
		require.NotNil(t, cfg.Kubernetes, "start %d", i)
		assert.Equal(t, "prof-rwx", cfg.Kubernetes.SharedDirStorageClass,
			"start %d must use the saved profile gke-premium, not active_profile", i)
	}
}

// An invalid shared_dir_size fails the start before the runtime is called,
// and the error names where the value is set.
func TestStartSharedDirK8sDefaults_InvalidSizeNamesSource(t *testing.T) {
	tests := []struct {
		name, settingsFrom, settingsTo string
		inline                         *api.ScionConfig
		profile, wantSource            string
	}{
		{"runtime entry", "shared_dir_size: 1Ti", "shared_dir_size: 1TB", nil, "", "runtimes.gke-autopilot.shared_dir_size"},
		{"profile", "shared_dir_storage_class: prof-rwx", "shared_dir_storage_class: prof-rwx\n    shared_dir_size: lots", nil, "gke-premium", "profiles.gke-premium.shared_dir_size"},
		{"agent config", "", "", &api.ScionConfig{Kubernetes: &api.KubernetesConfig{SharedDirSize: "5GB"}}, "", "kubernetes.shared_dir_size in the agent or template config"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newSharedDirStorageRunFixture(t)
			settings := sharedDirK8sDefaultsSettings
			if tt.settingsFrom != "" {
				settings = strings.Replace(settings, tt.settingsFrom, tt.settingsTo, 1)
			}
			require.NoError(t, os.WriteFile(filepath.Join(f.globalScionDir, "settings.yaml"), []byte(settings), 0644))
			ran := 0
			mockRT := &runtime.MockRuntime{
				NameFunc: func() string { return "kubernetes" },
				RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
					ran++
					return "mock-id", nil
				},
			}
			_, err := NewManager(mockRT).Start(context.Background(), api.StartOptions{
				Name:         "sd-agent",
				ProjectPath:  f.projectScionDir,
				Profile:      tt.profile,
				InlineConfig: tt.inline,
				NoAuth:       true,
				Env:          map[string]string{"SCION_AGENT_ID": "agent-sd", "SCION_PROJECT_ID": "pid-sd"},
				SharedDirs:   []api.SharedDir{{Name: "scratchpad"}},
			})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantSource)
			assert.Contains(t, err.Error(), "invalid shared_dir_size")
			assert.Equal(t, 0, ran, "the runtime must not be called")
		})
	}
}

// A settings entry that sets only shared_dir_size still applies it, with
// the class left unset (cluster default).
func TestStartSharedDirK8sDefaults_SizeOnly(t *testing.T) {
	cfg := startForSharedDirK8sDefaults(t, "kubernetes", "", api.StartOptions{Profile: "size-only"})
	require.NotNil(t, cfg.Kubernetes)
	assert.Equal(t, "50Gi", cfg.Kubernetes.SharedDirSize)
	assert.Equal(t, "", cfg.Kubernetes.SharedDirStorageClass)
}

// The start-time size check only runs when shared-dir PVCs are needed: with
// no shared dirs, an invalid settings size does not block the start.
func TestStartSharedDirK8sDefaults_InvalidSizeIgnoredWithoutSharedDirs(t *testing.T) {
	f := newSharedDirStorageRunFixture(t)
	settings := strings.Replace(sharedDirK8sDefaultsSettings, "shared_dir_size: 1Ti", "shared_dir_size: 1TB", 1)
	require.NoError(t, os.WriteFile(filepath.Join(f.globalScionDir, "settings.yaml"), []byte(settings), 0644))
	ran := 0
	mockRT := &runtime.MockRuntime{
		NameFunc: func() string { return "kubernetes" },
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			ran++
			return "mock-id", nil
		},
	}
	_, err := NewManager(mockRT).Start(context.Background(), api.StartOptions{
		Name:        "sd-agent",
		ProjectPath: f.projectScionDir,
		NoAuth:      true,
		Env:         map[string]string{"SCION_AGENT_ID": "agent-sd", "SCION_PROJECT_ID": "pid-sd"},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, ran, "the runtime must be called")
}
