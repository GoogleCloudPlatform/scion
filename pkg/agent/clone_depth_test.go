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
	"strconv"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func intPtrCD(v int) *int { return &v }

// requestGitClone is the clone config as sent with a start request: the
// default shallow depth 1.
func requestGitClone() *api.GitCloneConfig {
	return &api.GitCloneConfig{URL: "https://example.com/repo.git", Branch: "main", Depth: intPtrCD(1)}
}

func TestApplyCloneDepth(t *testing.T) {
	tests := []struct {
		name      string
		in        cloneDepthInput
		wantDepth int
		wantEnv   string // "" means SCION_GIT_DEPTH keeps the request value
	}{
		{
			name:      "unset keeps the request default",
			in:        cloneDepthInput{},
			wantDepth: 1,
		},
		{
			name:      "profile full",
			in:        cloneDepthInput{Profile: "full", ProfileSource: "profiles.gke.clone_depth"},
			wantDepth: 0,
			wantEnv:   "0",
		},
		{
			name:      "profile N",
			in:        cloneDepthInput{Profile: "50", ProfileSource: "profiles.gke.clone_depth"},
			wantDepth: 50,
			wantEnv:   "50",
		},
		{
			name:      "template overrides profile",
			in:        cloneDepthInput{Template: "10", Profile: "full", ProfileSource: "profiles.gke.clone_depth"},
			wantDepth: 10,
			wantEnv:   "10",
		},
		{
			name:      "template full overrides profile N",
			in:        cloneDepthInput{Template: "full", Profile: "3", ProfileSource: "profiles.gke.clone_depth"},
			wantDepth: 0,
			wantEnv:   "0",
		},
		{
			name:      "template alone",
			in:        cloneDepthInput{Template: "7"},
			wantDepth: 7,
			wantEnv:   "7",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			orig := requestGitClone()
			env := map[string]string{gitDepthEnvKey: "1"}
			got, err := applyCloneDepth(orig, env, tt.in)
			require.NoError(t, err)
			require.NotNil(t, got)
			require.NotNil(t, got.Depth)
			assert.Equal(t, tt.wantDepth, *got.Depth)
			assert.Equal(t, orig.URL, got.URL)
			assert.Equal(t, orig.Branch, got.Branch)
			wantEnv := tt.wantEnv
			if wantEnv == "" {
				wantEnv = "1"
			}
			assert.Equal(t, wantEnv, env[gitDepthEnvKey])
			// The request's config is never modified.
			assert.Equal(t, 1, *orig.Depth)

			// The Kubernetes NFS init container takes its depth from the
			// same config.
			initGC := nfsInitGitClone(api.StartOptions{GitClone: got})
			require.NotNil(t, initGC)
			assert.Equal(t, tt.wantDepth, *initGC.Depth)
		})
	}
}

func TestApplyCloneDepth_NoGitClone(t *testing.T) {
	env := map[string]string{}
	got, err := applyCloneDepth(nil, env, cloneDepthInput{Template: "full"})
	require.NoError(t, err)
	assert.Nil(t, got)
	assert.NotContains(t, env, gitDepthEnvKey)
}

func TestApplyCloneDepth_InvalidNamesSource(t *testing.T) {
	_, err := applyCloneDepth(requestGitClone(), map[string]string{}, cloneDepthInput{Profile: "0", ProfileSource: "profiles.gke.clone_depth"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "profiles.gke.clone_depth")

	_, err = applyCloneDepth(requestGitClone(), map[string]string{}, cloneDepthInput{Template: "shallow", Profile: "full"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "clone_depth in the agent or template config")
}

// clone_depth from a settings profile and a template reaches the run
// config through the real Manager.Start path: GitClone.Depth (read by the
// Kubernetes NFS init container) and SCION_GIT_DEPTH (read by the
// in-container clone).

const cloneDepthRunSettings = `schema_version: "1"
active_profile: plain
runtimes:
  k8s:
    type: kubernetes
profiles:
  plain:
    runtime: k8s
  full:
    runtime: k8s
    clone_depth: full
  deep:
    runtime: k8s
    clone_depth: 40
`

func startForCloneDepth(t *testing.T, templateJSON string, opts api.StartOptions) runtime.RunConfig {
	t.Helper()
	f := newSharedDirStorageRunFixture(t)
	require.NoError(t, os.WriteFile(filepath.Join(f.globalScionDir, "settings.yaml"),
		[]byte(cloneDepthRunSettings), 0644))
	if templateJSON != "" {
		require.NoError(t, os.WriteFile(filepath.Join(f.globalScionDir, "templates", "default", "scion-agent.json"),
			[]byte(templateJSON), 0644))
	}
	var captured runtime.RunConfig
	ran := 0
	mockRT := &runtime.MockRuntime{
		NameFunc: func() string { return "kubernetes" },
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			captured = config
			ran++
			return "mock-id", nil
		},
	}
	opts.Name = "cd-agent"
	opts.ProjectPath = f.projectScionDir
	opts.NoAuth = true
	opts.GitClone = requestGitClone()
	opts.Env = map[string]string{
		"SCION_AGENT_ID":      "agent-cd",
		"SCION_PROJECT_ID":    "pid-cd",
		"SCION_GIT_CLONE_URL": opts.GitClone.URL,
		gitDepthEnvKey:        "1",
	}
	_, err := NewManager(mockRT).Start(context.Background(), opts)
	require.NoError(t, err)
	require.Equal(t, 1, ran)
	return captured
}

func runEnvValue(cfg runtime.RunConfig, key string) (string, bool) {
	for _, kv := range cfg.Env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v, true
		}
	}
	return "", false
}

func TestStartCloneDepth_Precedence(t *testing.T) {
	tests := []struct {
		name     string
		template string
		opts     api.StartOptions
		want     int
	}{
		{"unset keeps default depth 1", "", api.StartOptions{Profile: "plain"}, 1},
		{"profile full", "", api.StartOptions{Profile: "full"}, 0},
		{"profile N", "", api.StartOptions{Profile: "deep"}, 40},
		{"template overrides profile full",
			`{"default_harness_config": "test-harness", "clone_depth": 5}`,
			api.StartOptions{Profile: "full"}, 5},
		{"template full overrides profile N",
			`{"default_harness_config": "test-harness", "clone_depth": "full"}`,
			api.StartOptions{Profile: "deep"}, 0},
		{"agent config overrides template",
			`{"default_harness_config": "test-harness", "clone_depth": 5}`,
			api.StartOptions{Profile: "deep", InlineConfig: &api.ScionConfig{CloneDepth: "9"}}, 9},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := startForCloneDepth(t, tt.template, tt.opts)
			require.NotNil(t, cfg.GitClone)
			require.NotNil(t, cfg.GitClone.Depth)
			assert.Equal(t, tt.want, *cfg.GitClone.Depth)
			require.NotNil(t, cfg.GitCloneForInit, "the Kubernetes NFS init container must get the clone config")
			require.NotNil(t, cfg.GitCloneForInit.Depth)
			assert.Equal(t, tt.want, *cfg.GitCloneForInit.Depth)
			env, ok := runEnvValue(cfg, gitDepthEnvKey)
			require.True(t, ok, "SCION_GIT_DEPTH must be set")
			assert.Equal(t, strconv.Itoa(tt.want), env)
		})
	}
}

// A start that names no profile uses the profile the agent was created
// with, not the active profile.
func TestStartCloneDepth_RestartUsesCreatedWithProfile(t *testing.T) {
	f := newSharedDirStorageRunFixture(t)
	require.NoError(t, os.WriteFile(filepath.Join(f.globalScionDir, "settings.yaml"),
		[]byte(cloneDepthRunSettings), 0644))
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
		gc := requestGitClone()
		_, err := mgr.Start(context.Background(), api.StartOptions{
			Name: "cd-agent", ProjectPath: f.projectScionDir, NoAuth: true,
			Profile:  profile,
			GitClone: gc,
			Env: map[string]string{
				"SCION_AGENT_ID":      "agent-cd",
				"SCION_PROJECT_ID":    "pid-cd",
				"SCION_GIT_CLONE_URL": gc.URL,
				gitDepthEnvKey:        "1",
			},
		})
		require.NoError(t, err)
	}
	start("deep") // created under "deep"; the active profile is "plain"
	start("")     // restart without a profile
	require.Len(t, captured, 2)
	for i, cfg := range captured {
		require.NotNil(t, cfg.GitClone, "start %d", i)
		require.NotNil(t, cfg.GitClone.Depth, "start %d", i)
		assert.Equal(t, 40, *cfg.GitClone.Depth, "start %d", i)
		env, _ := runEnvValue(cfg, gitDepthEnvKey)
		assert.Equal(t, "40", env, "start %d", i)
	}
}

func TestStartCloneDepth_InvalidProfileFailsStart(t *testing.T) {
	f := newSharedDirStorageRunFixture(t)
	require.NoError(t, os.WriteFile(filepath.Join(f.globalScionDir, "settings.yaml"),
		[]byte("schema_version: \"1\"\nactive_profile: bad\nruntimes:\n  k8s:\n    type: kubernetes\nprofiles:\n  bad:\n    runtime: k8s\n    clone_depth: \"0\"\n"), 0644))
	mockRT := &runtime.MockRuntime{
		NameFunc: func() string { return "kubernetes" },
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			t.Fatal("runtime must not run with an invalid clone_depth")
			return "", nil
		},
	}
	_, err := NewManager(mockRT).Start(context.Background(), api.StartOptions{
		Name: "cd-agent", ProjectPath: f.projectScionDir, NoAuth: true,
		GitClone: requestGitClone(),
		Env:      map[string]string{"SCION_AGENT_ID": "agent-cd", "SCION_PROJECT_ID": "pid-cd"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "profiles.bad.clone_depth")
}
