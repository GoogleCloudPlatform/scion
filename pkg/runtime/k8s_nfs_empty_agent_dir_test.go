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
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

// nfsEmptyAgentDirConfig is nfsBaseConfig for an empty-per-agent agent
// (design #2703 P3).
func nfsEmptyAgentDirConfig(name string) RunConfig {
	cfg := nfsBaseConfig(name)
	cfg.NFSAgentDirName = "agent-1"
	cfg.NFSAgentDirEmpty = true
	cfg.Env = append(cfg.Env, "SCION_WORKSPACE_MODE="+string(store.SharingModeEmptyPerAgent))
	return cfg
}

// Empty-per-agent on NFS: the agent container mounts only its own
// workspace (agents/<slug>/workspace) at /workspace, the init container
// mounts only the agent's directory, and no container in the pod mounts
// the project's shared workspace path. The init container gets the
// empty-per-agent mode and the slug, no branch and no clone settings, even
// when a branch or clone settings are set by mistake.
func TestBuildPod_NFSEmptyAgentDir_MountsAndEnv(t *testing.T) {
	cfg := nfsEmptyAgentDirConfig("epa")
	cfg.NFSAgentBranch = "scion/stray"
	cfg.GitCloneForInit = &api.GitCloneConfig{URL: "https://example.com/repo.git"}
	pod, err := newNFSTestK8sRuntime().buildPod("default", cfg)
	require.NoError(t, err)

	main := pod.Spec.Containers[0]
	assert.Equal(t, "/workspace", main.WorkingDir)
	assert.Equal(t, []corev1.VolumeMount{
		{Name: "workspace", MountPath: "/workspace", SubPath: "projects/proj-123/agents/agent-1/workspace"},
	}, main.VolumeMounts)

	require.Len(t, pod.Spec.InitContainers, 1)
	ic := pod.Spec.InitContainers[0]
	assert.Equal(t, []corev1.VolumeMount{
		{Name: "workspace", MountPath: "/workspace", SubPath: "projects/proj-123/agents/agent-1"},
	}, ic.VolumeMounts)
	assert.Equal(t, []string{"sciontool", "provision", "--uid", "1000", "--gid", "1000"}, ic.Command)
	for name, want := range map[string]string{
		"SCION_WORKSPACE_MODE": "empty-per-agent",
		"SCION_AGENT_SLUG":     "agent-1",
		"SCION_PROJECT_ID":     "proj-123",
	} {
		got, ok := envValue(ic.Env, name)
		assert.True(t, ok, "init env %s missing", name)
		assert.Equal(t, want, got, "init env %s", name)
	}
	for _, name := range []string{"SCION_AGENT_BRANCH", "SCION_CLONE_URL", "SCION_CLONE_BRANCH", "SCION_WORKSPACE_PATH"} {
		_, ok := envValue(ic.Env, name)
		assert.False(t, ok, "init env must not have %s", name)
	}
	modes := 0
	for _, e := range ic.Env {
		if e.Name == "SCION_WORKSPACE_MODE" {
			modes++
		}
	}
	assert.Equal(t, 1, modes, "SCION_WORKSPACE_MODE is set once")

	const shared = "projects/proj-123/workspace"
	for _, c := range append(append([]corev1.Container{}, pod.Spec.InitContainers...), pod.Spec.Containers...) {
		for _, m := range c.VolumeMounts {
			if m.Name != "workspace" {
				continue
			}
			assert.NotEqual(t, shared, m.SubPath, "container %s mounts the project's shared workspace", c.Name)
			assert.True(t, strings.HasPrefix(m.SubPath, "projects/proj-123/agents/agent-1"),
				"container %s mounts %q outside the agent's directory", c.Name, m.SubPath)
		}
	}
}

// Shared dirs keep their own mounts next to the agent's directory, as in
// clone-per-agent mode.
func TestBuildPod_NFSEmptyAgentDir_SharedDirs(t *testing.T) {
	cfg := nfsEmptyAgentDirConfig("epa-shared")
	cfg.SharedDirs = []api.SharedDir{{Name: "scratchpad"}}
	pod, err := newNFSTestK8sRuntime().buildPod("default", cfg)
	require.NoError(t, err)
	ic := pod.Spec.InitContainers[0]
	sd := findVolumeMount(&corev1.Container{VolumeMounts: ic.VolumeMounts}, "shared-dir-0")
	require.NotNil(t, sd)
	assert.Equal(t, "projects/proj-123/shared-dirs/scratchpad", sd.SubPath)
	assert.Equal(t, "projects/proj-123/agents/agent-1", ic.VolumeMounts[0].SubPath)
}

// Each pod prepares its own agent directory, so a broker lock loser gets
// the provisioning init container rather than the wait-only one.
func TestBuildPod_NFSEmptyAgentDir_LockLoserProvisions(t *testing.T) {
	cfg := nfsEmptyAgentDirConfig("epa-loser")
	cfg.nfsProvisionLockLost = true
	pod, err := newNFSTestK8sRuntime().buildPod("default", cfg)
	require.NoError(t, err)
	assert.False(t, hasFlag(pod.Spec.InitContainers[0].Command, "--wait-for-sentinel"))
}

// NFSAgentDirEmpty without NFSAgentDirName changes nothing: the agent keeps
// the layout the other fields select.
func TestBuildPod_NFSEmptyAgentDir_FlagAloneIgnored(t *testing.T) {
	cfg := nfsBaseConfig("epa-flag")
	cfg.NFSAgentDirEmpty = true
	pod, err := newNFSTestK8sRuntime().buildPod("default", cfg)
	require.NoError(t, err)
	base, err := newNFSTestK8sRuntime().buildPod("default", nfsBaseConfig("epa-flag"))
	require.NoError(t, err)
	assert.Equal(t, base.Spec.Containers[0].VolumeMounts, pod.Spec.Containers[0].VolumeMounts)
	assert.Equal(t, base.Spec.InitContainers[0].Env, pod.Spec.InitContainers[0].Env)
}
