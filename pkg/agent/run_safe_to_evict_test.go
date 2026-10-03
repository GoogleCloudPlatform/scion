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
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// safe_to_evict from settings runtime and profile entries reaches
// RunConfig.Kubernetes.SafeToEvict through the real Manager.Start path, with
// template/agent > profile > runtime precedence.

const safeToEvictRunSettings = `schema_version: "1"
active_profile: gke
runtimes:
  gke-autopilot:
    type: kubernetes
    safe_to_evict: false
  plain-k8s:
    type: kubernetes
  docker:
    type: docker
    safe_to_evict: false
profiles:
  gke:
    runtime: gke-autopilot
  gke-evictable:
    runtime: gke-autopilot
    safe_to_evict: true
  plain:
    runtime: plain-k8s
  plain-pinned:
    runtime: plain-k8s
    safe_to_evict: false
  local:
    runtime: docker
`

func safeToEvictPtr(b bool) *bool { return &b }

func startForSafeToEvict(t *testing.T, runtimeName, templateJSON string, opts api.StartOptions) runtime.RunConfig {
	t.Helper()
	f := newSharedDirStorageRunFixture(t)
	require.NoError(t, os.WriteFile(filepath.Join(f.globalScionDir, "settings.yaml"),
		[]byte(safeToEvictRunSettings), 0644))
	if templateJSON != "" {
		require.NoError(t, os.WriteFile(filepath.Join(f.globalScionDir, "templates", "default", "scion-agent.json"),
			[]byte(templateJSON), 0644))
	}
	var captured runtime.RunConfig
	ran := 0
	mockRT := &runtime.MockRuntime{
		NameFunc: func() string { return runtimeName },
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			captured = config
			ran++
			return "mock-id", nil
		},
	}
	opts.Name = "ste-agent"
	opts.ProjectPath = f.projectScionDir
	opts.NoAuth = true
	opts.Env = map[string]string{"SCION_AGENT_ID": "agent-ste", "SCION_PROJECT_ID": "pid-ste"}
	_, err := NewManager(mockRT).Start(context.Background(), opts)
	require.NoError(t, err)
	require.Equal(t, 1, ran)
	return captured
}

func safeToEvictOf(cfg runtime.RunConfig) *bool {
	if cfg.Kubernetes == nil {
		return nil
	}
	return cfg.Kubernetes.SafeToEvict
}

func TestStartSafeToEvict_Precedence(t *testing.T) {
	tests := []struct {
		name     string
		template string
		opts     api.StartOptions
		want     *bool
	}{
		{"runtime entry false", "", api.StartOptions{}, safeToEvictPtr(false)},
		{"profile true over runtime false", "", api.StartOptions{Profile: "gke-evictable"}, safeToEvictPtr(true)},
		{"profile false over unset runtime", "", api.StartOptions{Profile: "plain-pinned"}, safeToEvictPtr(false)},
		{"unset everywhere", "", api.StartOptions{Profile: "plain"}, nil},
		{"template true over runtime false",
			`{"default_harness_config": "test-harness", "kubernetes": {"safeToEvict": true}}`,
			api.StartOptions{}, safeToEvictPtr(true)},
		{"template false over profile true",
			`{"default_harness_config": "test-harness", "kubernetes": {"safeToEvict": false}}`,
			api.StartOptions{Profile: "gke-evictable"}, safeToEvictPtr(false)},
		{"agent config true over template false",
			`{"default_harness_config": "test-harness", "kubernetes": {"safeToEvict": false}}`,
			api.StartOptions{InlineConfig: &api.ScionConfig{Kubernetes: &api.KubernetesConfig{SafeToEvict: safeToEvictPtr(true)}}},
			safeToEvictPtr(true)},
		{"agent config false over unset settings", "",
			api.StartOptions{Profile: "plain", InlineConfig: &api.ScionConfig{Kubernetes: &api.KubernetesConfig{SafeToEvict: safeToEvictPtr(false)}}},
			safeToEvictPtr(false)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := startForSafeToEvict(t, "kubernetes", tt.template, tt.opts)
			assert.Equal(t, tt.want, safeToEvictOf(cfg))
		})
	}
}

// The settings value is applied to a copy: the agent config's own
// kubernetes block is not modified.
func TestStartSafeToEvict_DoesNotMutateAgentConfig(t *testing.T) {
	inline := &api.ScionConfig{Kubernetes: &api.KubernetesConfig{Namespace: "agents"}}
	cfg := startForSafeToEvict(t, "kubernetes", "", api.StartOptions{InlineConfig: inline})
	require.NotNil(t, cfg.Kubernetes)
	require.NotNil(t, cfg.Kubernetes.SafeToEvict)
	assert.False(t, *cfg.Kubernetes.SafeToEvict)
	assert.Equal(t, "agents", cfg.Kubernetes.Namespace)
	assert.Nil(t, inline.Kubernetes.SafeToEvict, "agent config must not be modified")
	assert.NotSame(t, inline.Kubernetes, cfg.Kubernetes)
}

// On a non-Kubernetes runtime the setting is accepted and ignored with a
// warning: the settings value does not create a Kubernetes block.
func TestStartSafeToEvict_IgnoredOnOtherRuntimes(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(old)

	cfg := startForSafeToEvict(t, "docker", "", api.StartOptions{Profile: "local"})
	assert.Nil(t, cfg.Kubernetes)
	assert.Contains(t, buf.String(), "level=WARN")
	assert.Contains(t, buf.String(), "safe_to_evict applies only to the Kubernetes runtime")
	assert.Contains(t, buf.String(), "runtimes.docker.safe_to_evict")
}

func TestStartSafeToEvict_NoWarningWhenUnset(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(old)

	f := newSharedDirStorageRunFixture(t)
	mockRT := &runtime.MockRuntime{
		NameFunc: func() string { return "docker" },
		RunFunc:  func(ctx context.Context, config runtime.RunConfig) (string, error) { return "mock-id", nil },
	}
	_, err := NewManager(mockRT).Start(context.Background(), api.StartOptions{
		Name: "ste-agent", ProjectPath: f.projectScionDir, NoAuth: true,
		Env: map[string]string{"SCION_AGENT_ID": "agent-ste", "SCION_PROJECT_ID": "pid-ste"},
	})
	require.NoError(t, err)
	assert.NotContains(t, buf.String(), "safe_to_evict")
}
