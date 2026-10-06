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
	"context"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

// nfsWorktreeConfig is nfsBaseConfig for a worktree-per-agent agent.
func nfsWorktreeConfig(name string) RunConfig {
	cfg := nfsBaseConfig(name)
	cfg.NFSWorktreeName = "agent-1"
	cfg.NFSWorktreeBranch = "agent-one"
	return cfg
}

// Worktree-per-agent: the agent container mounts the shared .git and its
// own worktree at /repo-root, works in the worktree, and its clone step is
// pointed there. The init container keeps the shared checkout at /workspace
// and gets the worktree env.
func TestBuildPod_NFSWorktree_MountsAndEnv(t *testing.T) {
	pod, err := newNFSTestK8sRuntime().buildPod("default", nfsWorktreeConfig("wt"))
	require.NoError(t, err)

	main := pod.Spec.Containers[0]
	assert.Equal(t, "/repo-root/worktrees/agent-1", main.WorkingDir)
	assert.Equal(t, []corev1.VolumeMount{
		{Name: "workspace", MountPath: "/repo-root/.git", SubPath: "projects/proj-123/workspace/.git"},
		{Name: "workspace", MountPath: "/repo-root/worktrees/agent-1", SubPath: "projects/proj-123/workspace/worktrees/agent-1"},
	}, main.VolumeMounts)
	v, ok := envValue(main.Env, "SCION_WORKSPACE_PATH")
	assert.True(t, ok)
	assert.Equal(t, "/repo-root/worktrees/agent-1", v)

	require.Len(t, pod.Spec.InitContainers, 1)
	ic := pod.Spec.InitContainers[0]
	assert.Equal(t, []corev1.VolumeMount{
		{Name: "workspace", MountPath: "/workspace", SubPath: "projects/proj-123/workspace"},
		{Name: "workspace", MountPath: "/scion-provision", SubPath: "projects/proj-123/provision"},
	}, ic.VolumeMounts)
	assert.False(t, hasFlag(ic.Command, "--wait-for-sentinel"))
	for name, want := range map[string]string{
		"SCION_PROVISION_STATE_DIR": "/scion-provision",
		"SCION_WORKSPACE_MODE":      "worktree-per-agent",
		"SCION_AGENT_SLUG":          "agent-1",
		"SCION_AGENT_BRANCH":        "agent-one",
		"SCION_PROJECT_ID":          "proj-123",
		"SCION_CLONE_URL":           "https://github.com/example/repo.git",
	} {
		got, ok := envValue(ic.Env, name)
		assert.True(t, ok, "init env %s missing", name)
		assert.Equal(t, want, got, "init env %s", name)
	}
	_, ok = envValue(ic.Env, "SCION_WORKSPACE_PATH")
	assert.False(t, ok, "the init container provisions /workspace")
}

// The workspace path the runtime sets wins over a value from the
// dispatch env.
func TestBuildPod_NFSWorktree_WorkspacePathWins(t *testing.T) {
	cfg := nfsWorktreeConfig("wt-env")
	cfg.Env = []string{"SCION_WORKSPACE_PATH=/workspace"}
	pod, err := newNFSTestK8sRuntime().buildPod("default", cfg)
	require.NoError(t, err)
	var values []string
	for _, e := range pod.Spec.Containers[0].Env {
		if e.Name == "SCION_WORKSPACE_PATH" {
			values = append(values, e.Value)
		}
	}
	assert.Equal(t, []string{"/repo-root/worktrees/agent-1"}, values)
}

// In worktree mode every pod adds its own worktree, so a broker lock
// loser gets the provisioning init container (root, with the chown
// capabilities), not the wait-only one. In shared-plain a loser still
// waits.
func TestBuildPod_NFSWorktree_LockLoserProvisions(t *testing.T) {
	cfg := nfsWorktreeConfig("wt-loser")
	cfg.nfsProvisionLockLost = true
	cfg.NFSWorkspacePreCreated = true
	pod, err := newNFSTestK8sRuntime().buildPod("default", cfg)
	require.NoError(t, err)
	ic := pod.Spec.InitContainers[0]
	assert.False(t, hasFlag(ic.Command, "--wait-for-sentinel"), "worktree-mode loser must provision")
	require.NotNil(t, ic.SecurityContext.RunAsUser)
	assert.Equal(t, int64(0), *ic.SecurityContext.RunAsUser)
	assert.ElementsMatch(t, []corev1.Capability{"CHOWN", "FOWNER", "DAC_OVERRIDE"}, ic.SecurityContext.Capabilities.Add)
	v, ok := envValue(ic.Env, provision.ChownBestEffortEnv)
	assert.True(t, ok, "a provisioning init container gets the best-effort chown env when the broker prepared the directories")
	assert.Equal(t, "1", v)

	plain := nfsBaseConfig("plain-loser")
	plain.nfsProvisionLockLost = true
	plain.NFSWorkspacePreCreated = true
	pod, err = newNFSTestK8sRuntime().buildPod("default", plain)
	require.NoError(t, err)
	ic = pod.Spec.InitContainers[0]
	assert.True(t, hasFlag(ic.Command, "--wait-for-sentinel"), "shared-plain loser still waits")
	assert.Nil(t, ic.SecurityContext.RunAsUser)
	_, ok = envValue(ic.Env, provision.ChownBestEffortEnv)
	assert.False(t, ok)
}

// Shared-plain pods are unchanged: no worktree mounts or env, the shared
// checkout at /workspace, and the branch field alone changes nothing.
func TestBuildPod_NFSSharedPlain_Unchanged(t *testing.T) {
	for _, lockLost := range []bool{false, true} {
		cfg := nfsBaseConfig("plain")
		cfg.nfsProvisionLockLost = lockLost
		pod, err := newNFSTestK8sRuntime().buildPod("default", cfg)
		require.NoError(t, err)

		main := pod.Spec.Containers[0]
		assert.Equal(t, "/workspace", main.WorkingDir)
		assert.Equal(t, []corev1.VolumeMount{
			{Name: "workspace", MountPath: "/workspace", SubPath: "projects/proj-123/workspace"},
		}, main.VolumeMounts)
		_, ok := envValue(main.Env, "SCION_WORKSPACE_PATH")
		assert.False(t, ok)
		for _, name := range []string{"SCION_WORKSPACE_MODE", "SCION_AGENT_SLUG", "SCION_AGENT_BRANCH"} {
			_, ok := envValue(pod.Spec.InitContainers[0].Env, name)
			assert.False(t, ok, "shared-plain init env must not have %s", name)
		}

		withBranch := cfg
		withBranch.NFSWorktreeBranch = "agent-one"
		pod2, err := newNFSTestK8sRuntime().buildPod("default", withBranch)
		require.NoError(t, err)
		assert.Equal(t, pod.Spec, pod2.Spec, "lockLost=%v", lockLost)
	}
}

// Without the NFS init container (local backend, or no claim) the worktree
// fields are ignored and the pod is the same as without them.
func TestBuildPod_NFSWorktree_IgnoredWithoutNFSClaim(t *testing.T) {
	for _, mutate := range []func(*RunConfig){
		func(c *RunConfig) { c.WorkspaceBackendName = "" },
		func(c *RunConfig) { c.NFSPVClaimName = "" },
	} {
		with := nfsWorktreeConfig("no-nfs")
		mutate(&with)
		without := with
		without.NFSWorktreeName = ""
		without.NFSWorktreeBranch = ""
		podWith, err := newNFSTestK8sRuntime().buildPod("default", with)
		require.NoError(t, err)
		podWithout, err := newNFSTestK8sRuntime().buildPod("default", without)
		require.NoError(t, err)
		assert.Equal(t, podWithout.Spec, podWith.Spec)
		assert.Equal(t, "/workspace", podWith.Spec.Containers[0].WorkingDir)
	}
}

// The agent name becomes a path segment of the mounts; anything that is
// not an agent slug is refused.
func TestBuildPod_NFSWorktree_RejectsBadAgentName(t *testing.T) {
	for _, name := range []string{"..", ".", "a/b", "../x", `a\b`, "a\x00b", "Agent-1", "a.b", "-a"} {
		cfg := nfsWorktreeConfig("bad-name")
		cfg.NFSWorktreeName = name
		_, err := newNFSTestK8sRuntime().buildPod("default", cfg)
		assert.Error(t, err, "agent name %q", name)
	}
}

// Run: a broker lock loser in worktree mode creates a pod whose init
// container provisions (adds the worktree) instead of only waiting.
func TestRun_NFSWorktreeLockLost_CreatesProvisioningPod(t *testing.T) {
	r := newNFSTestK8sRuntime()
	cfg := nfsWorktreeConfig("scion-wt-lock-lost")
	cfg.Locker = &alwaysLoseLocker{}
	// Run creates the pod, then fails readiness at its first poll (see
	// failPodReadiness), which keeps the pod for inspection.
	failPodReadiness(r.Client.Clientset.(*k8sfake.Clientset))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r.Run(ctx, cfg) //nolint:errcheck

	pods, err := r.Client.Clientset.CoreV1().Pods("default").List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, pods.Items, 1)
	require.Len(t, pods.Items[0].Spec.InitContainers, 1)
	ic := pods.Items[0].Spec.InitContainers[0]
	assert.False(t, hasFlag(ic.Command, "--wait-for-sentinel"))
	v, _ := envValue(ic.Env, "SCION_WORKSPACE_MODE")
	assert.Equal(t, "worktree-per-agent", v)
}

func TestNFSWorktreeSubPaths(t *testing.T) {
	gitSub, wtSub, err := nfsWorktreeSubPaths("projects/p/workspace", "agent-1")
	require.NoError(t, err)
	assert.Equal(t, "projects/p/workspace/.git", gitSub)
	assert.Equal(t, "projects/p/workspace/worktrees/agent-1", wtSub)
	// Kubernetes subPaths use forward slashes on every OS.
	assert.NotContains(t, gitSub+wtSub, `\`)
	gitSub, wtSub, err = nfsWorktreeSubPaths("projects/p/workspace/", "agent-1")
	require.NoError(t, err)
	assert.Equal(t, "projects/p/workspace/.git", gitSub)
	assert.Equal(t, "projects/p/workspace/worktrees/agent-1", wtSub)
	_, _, err = nfsWorktreeSubPaths("", "agent-1")
	assert.Error(t, err)
	assert.Equal(t, "/repo-root/worktrees/agent-1", NFSWorktreeContainerPath("agent-1"))
}
