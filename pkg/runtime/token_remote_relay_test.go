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

package runtime

import (
	"slices"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const runtimeTokenRemote = "github.com/example/repo"

var tokenRemoteEnvEntry = api.EnvGitTokenRemote + "=" + runtimeTokenRemote

// The Kubernetes provisioning init container gets the hub's token remote
// from GitCloneForInit unchanged, and none when the GitClone has none.
func TestNFSProvisionEnv_TokenRemote(t *testing.T) {
	envs := nfsProvisionEnv(&api.GitCloneConfig{URL: "https://github.com/example/repo.git", TokenRemote: runtimeTokenRemote})
	got, ok := envValue(envs, api.EnvGitTokenRemote)
	require.True(t, ok, "the token remote must reach the init container")
	assert.Equal(t, runtimeTokenRemote, got)

	envs = nfsProvisionEnv(&api.GitCloneConfig{URL: "https://github.com/example/repo.git"})
	_, ok = envValue(envs, api.EnvGitTokenRemote)
	assert.False(t, ok, "no token remote without the hub's")
}

// buildPod relays the token remote to both containers that clone: the
// provisioning init container (from GitCloneForInit) and the agent
// container (from the broker's env).
func TestBuildPod_TokenRemoteRelayed(t *testing.T) {
	r := newNFSTestK8sRuntime()
	cfg := RunConfig{
		Name:                 "shared-agent",
		Image:                "test-image",
		UnixUsername:         "scion",
		WorkspaceBackendName: "nfs",
		NFSPVClaimName:       "scion-workspaces",
		NFSSubPath:           "projects/proj-123/workspace",
		ProjectID:            "proj-123",
		Env:                  []string{tokenRemoteEnvEntry},
		GitCloneForInit: &api.GitCloneConfig{
			URL:         "https://github.com/example/repo.git",
			Branch:      "main",
			Depth:       intPtr(0),
			TokenRemote: runtimeTokenRemote,
		},
	}
	pod, err := r.buildPod("default", cfg)
	require.NoError(t, err)
	require.Len(t, pod.Spec.InitContainers, 1)
	got, ok := envValue(pod.Spec.InitContainers[0].Env, api.EnvGitTokenRemote)
	require.True(t, ok, "init container")
	assert.Equal(t, runtimeTokenRemote, got)
	got, ok = envValue(pod.Spec.Containers[0].Env, api.EnvGitTokenRemote)
	require.True(t, ok, "agent container")
	assert.Equal(t, runtimeTokenRemote, got)
}

// The Cloud Run runtimes pass the broker's token remote to the agent
// container unchanged.
func TestCloudRun_TokenRemoteRelayed(t *testing.T) {
	rt, err := NewCloudRunRuntime(&config.CloudRunConfig{ProjectID: "test-project", Location: "us-central1"})
	require.NoError(t, err)
	inst := rt.buildCloudRunInstance(RunConfig{Image: "test-image", Env: []string{tokenRemoteEnvEntry}}, 1000, 1000, nil)
	require.Len(t, inst.Containers, 1)
	found := false
	for _, ev := range inst.Containers[0].Env {
		if ev.GetName() == api.EnvGitTokenRemote {
			found = true
			assert.Equal(t, runtimeTokenRemote, ev.GetValue())
		}
	}
	assert.True(t, found, "cloud run instance")

	env := envFor(RunConfig{Env: []string{tokenRemoteEnvEntry}, Project: "p", ProjectID: "proj-123"}, scionPaths{})
	assert.Equal(t, runtimeTokenRemote, env[api.EnvGitTokenRemote], "cloud run sandbox")
}

// The container runtimes (Docker, Podman, Apple) pass the broker's token
// remote to the agent container unchanged.
func TestCommonRunArgs_TokenRemoteRelayed(t *testing.T) {
	args, err := buildCommonRunArgs(RunConfig{Harness: &harness.Generic{}, Name: "a", Image: "test-image", UnixUsername: "scion", Env: []string{tokenRemoteEnvEntry}})
	require.NoError(t, err)
	i := slices.Index(args, tokenRemoteEnvEntry)
	require.Positive(t, i, "the token remote env entry must be passed: %v", args)
	assert.Equal(t, "-e", args[i-1])
}
