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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

func TestNFSProvisionStateSubPath(t *testing.T) {
	got, err := NFSProvisionStateSubPath("projects/proj-123/workspace", "proj-123")
	require.NoError(t, err)
	assert.Equal(t, "projects/proj-123/provision", got)

	got, err = NFSProvisionStateSubPath("proj-123/workspace", "")
	require.NoError(t, err)
	assert.Equal(t, "proj-123/provision", got)

	for _, tc := range []struct{ subPath, projectID string }{
		{"", "proj-123"},
		{"workspace", ""},
		{"/projects/proj-123/workspace", "proj-123"},
		{"projects/proj-123/workspace/", "proj-123"},
		{"projects//proj-123/workspace", "proj-123"},
		{"projects/./proj-123/workspace", "proj-123"},
		{"projects/other/../proj-123/workspace", "proj-123"},
		{"../proj-123/workspace", "proj-123"},
		{"projects/proj-123/workspace/worktrees/agent-1", "proj-123"},
		{"projects/proj-123/agents/agent-1", "proj-123"},
		{"projects/proj-123/workspaces", "proj-123"},
		{"projects/proj-123/workspace", "proj-456"},
		{"..", ""},
		{"/abs/x/workspace", ""},
		{"../x/workspace", ""},
		{"projects/proj-\x00123/workspace", ""},
		{"projects/proj-123\x00/workspace", "proj-123"},
	} {
		_, err := NFSProvisionStateSubPath(tc.subPath, tc.projectID)
		assert.Error(t, err, "subPath %q project %q", tc.subPath, tc.projectID)
	}

	// A component that merely starts with ".." is an ordinary name.
	got, err = NFSProvisionStateSubPath("..foo/proj-123/workspace", "proj-123")
	require.NoError(t, err)
	assert.Equal(t, "..foo/proj-123/provision", got)
}

func TestIsLocalSlashPath(t *testing.T) {
	for p, want := range map[string]bool{
		"":                 false,
		"..":               false,
		"../x":             false,
		"x/..":             true, // cleans to "."
		"x/../..":          false,
		"/abs/x":           false,
		"a\x00b":           false,
		"..foo/x":          true,
		"x/..foo":          true,
		"projects/p1/work": true,
	} {
		assert.Equal(t, want, isLocalSlashPath(p), "%q", p)
	}
}

// assertNoProjectDirMount checks that no init container mount covers the
// project directory itself or any agent's directory.
func assertNoProjectDirMount(t *testing.T, pod *corev1.Pod) {
	t.Helper()
	for _, ic := range pod.Spec.InitContainers {
		for _, m := range ic.VolumeMounts {
			assert.NotEqual(t, "projects/proj-123", m.SubPath, "init container %s mounts the project directory", ic.Name)
			assert.NotEqual(t, "", m.SubPath, "init container %s mounts the claim root", ic.Name)
			assert.False(t, strings.HasPrefix(m.SubPath, "projects/proj-123/agents"),
				"init container %s mounts %s", ic.Name, m.SubPath)
		}
	}
}

// assertAgentHasNoProvisionState checks that the agent container gets
// neither the state directory's mount nor its env var.
func assertAgentHasNoProvisionState(t *testing.T, pod *corev1.Pod) {
	t.Helper()
	for _, c := range pod.Spec.Containers {
		for _, m := range c.VolumeMounts {
			assert.NotEqual(t, "projects/proj-123/provision", m.SubPath, "container %s", c.Name)
			assert.NotEqual(t, NFSProvisionStateMountPath, m.MountPath, "container %s", c.Name)
		}
		_, ok := envValue(c.Env, NFSProvisionStateEnv)
		assert.False(t, ok, "container %s has %s", c.Name, NFSProvisionStateEnv)
	}
}

// Shared-plain and worktree-per-agent (#2670): the init container, whether
// it provisions or only waits for the sentinel, mounts the workspace and the
// provisioning state directory and is told where the latter is; the agent
// container's mounts are unchanged.
func TestBuildPod_NFSProvisionStateMount(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cfg       RunConfig
		agentWant []corev1.VolumeMount
	}{
		{
			name:      "shared-plain",
			cfg:       nfsBaseConfig("sp"),
			agentWant: []corev1.VolumeMount{{Name: "workspace", MountPath: "/workspace", SubPath: "projects/proj-123/workspace"}},
		},
		{
			name: "shared-plain waiter",
			cfg: func() RunConfig {
				c := nfsBaseConfig("sp-wait")
				c.nfsProvisionLockLost = true
				return c
			}(),
			agentWant: []corev1.VolumeMount{{Name: "workspace", MountPath: "/workspace", SubPath: "projects/proj-123/workspace"}},
		},
		{
			name: "worktree-per-agent",
			cfg:  nfsWorktreeConfig("wt"),
			agentWant: []corev1.VolumeMount{
				{Name: "workspace", MountPath: "/repo-root/.git", SubPath: "projects/proj-123/workspace/.git"},
				{Name: "workspace", MountPath: "/repo-root/worktrees/agent-1", SubPath: "projects/proj-123/workspace/worktrees/agent-1"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod, err := newNFSTestK8sRuntime().buildPod("default", tc.cfg)
			require.NoError(t, err)
			require.Len(t, pod.Spec.InitContainers, 1)
			ic := pod.Spec.InitContainers[0]
			assert.Equal(t, []corev1.VolumeMount{
				{Name: "workspace", MountPath: "/workspace", SubPath: "projects/proj-123/workspace"},
				{Name: "workspace", MountPath: "/scion-provision", SubPath: "projects/proj-123/provision"},
			}, ic.VolumeMounts)
			v, ok := envValue(ic.Env, NFSProvisionStateEnv)
			assert.True(t, ok)
			assert.Equal(t, "/scion-provision", v)
			if tc.cfg.nfsProvisionLockLost {
				assert.True(t, hasFlag(ic.Command, "--wait-for-sentinel"))
			} else {
				assert.False(t, hasFlag(ic.Command, "--wait-for-sentinel"))
			}

			assert.Equal(t, tc.agentWant, pod.Spec.Containers[0].VolumeMounts)
			assertAgentHasNoProvisionState(t, pod)
			assertNoProjectDirMount(t, pod)
		})
	}
}

// Clone-per-agent and empty-per-agent keep their sentinel in the mounted
// agent directory: no state directory mount and no env var.
func TestBuildPod_NFSProvisionStateMount_NotForAgentDirModes(t *testing.T) {
	for name, cfg := range map[string]RunConfig{
		"clone-per-agent": nfsAgentDirConfig("cpa"),
		"empty-per-agent": nfsEmptyAgentDirConfig("epa"),
	} {
		t.Run(name, func(t *testing.T) {
			pod, err := newNFSTestK8sRuntime().buildPod("default", cfg)
			require.NoError(t, err)
			require.Len(t, pod.Spec.InitContainers, 1)
			ic := pod.Spec.InitContainers[0]
			for _, m := range ic.VolumeMounts {
				assert.NotEqual(t, NFSProvisionStateMountPath, m.MountPath)
				assert.False(t, strings.HasSuffix(m.SubPath, "/provision"), m.SubPath)
			}
			_, ok := envValue(ic.Env, NFSProvisionStateEnv)
			assert.False(t, ok)
			assertAgentHasNoProvisionState(t, pod)
		})
	}
}

// A workspace subPath outside the expected project layout fails the build
// instead of mounting a guessed directory.
func TestBuildPod_NFSProvisionStateMount_FailsClosed(t *testing.T) {
	for _, mutate := range []func(*RunConfig){
		func(c *RunConfig) { c.ProjectID = "proj-other" },
		func(c *RunConfig) { c.NFSSubPath = "projects/proj-123/../proj-999/workspace" },
		func(c *RunConfig) { c.NFSSubPath = "projects/proj-123/data" },
	} {
		cfg := nfsBaseConfig("bad")
		mutate(&cfg)
		_, err := newNFSTestK8sRuntime().buildPod("default", cfg)
		assert.ErrorContains(t, err, "provisioning state", "subPath %q project %q", cfg.NFSSubPath, cfg.ProjectID)
	}
}

// A local-backend pod has no provisioning init container at all.
func TestBuildPod_NFSProvisionStateMount_NotForLocalBackend(t *testing.T) {
	cfg := nfsBaseConfig("local")
	cfg.WorkspaceBackendName = "local"
	pod, err := newNFSTestK8sRuntime().buildPod("default", cfg)
	require.NoError(t, err)
	assert.Empty(t, pod.Spec.InitContainers)
	assertAgentHasNoProvisionState(t, pod)
}
