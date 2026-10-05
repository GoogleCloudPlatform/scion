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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

// nfsAgentDirConfig is nfsBaseConfig for a clone-per-agent agent.
func nfsAgentDirConfig(name string) RunConfig {
	cfg := nfsBaseConfig(name)
	cfg.NFSAgentDirName = "agent-1"
	cfg.NFSAgentBranch = "scion/agent-1"
	return cfg
}

// Clone-per-agent: the agent container mounts its own workspace at
// /workspace and works there, with no workspace path override, so its
// clone step fills that directory. The init container mounts the agent's
// directory, gets the clone-per-agent env and no clone settings.
func TestBuildPod_NFSAgentDir_MountsAndEnv(t *testing.T) {
	pod, err := newNFSTestK8sRuntime().buildPod("default", nfsAgentDirConfig("cpa"))
	require.NoError(t, err)

	main := pod.Spec.Containers[0]
	assert.Equal(t, "/workspace", main.WorkingDir)
	assert.Equal(t, []corev1.VolumeMount{
		{Name: "workspace", MountPath: "/workspace", SubPath: "projects/proj-123/agents/agent-1/workspace"},
	}, main.VolumeMounts)
	_, ok := envValue(main.Env, "SCION_WORKSPACE_PATH")
	assert.False(t, ok, "the agent container keeps the default workspace path")

	require.Len(t, pod.Spec.InitContainers, 1)
	ic := pod.Spec.InitContainers[0]
	assert.Equal(t, []corev1.VolumeMount{
		{Name: "workspace", MountPath: "/workspace", SubPath: "projects/proj-123/agents/agent-1"},
	}, ic.VolumeMounts)
	// No clone flags; ownership follows the pod securityContext, as in the
	// other modes.
	assert.Equal(t, []string{"sciontool", "provision", "--uid", "1000", "--gid", "1000"}, ic.Command)
	for name, want := range map[string]string{
		"SCION_WORKSPACE_MODE": "clone-per-agent",
		"SCION_AGENT_SLUG":     "agent-1",
		"SCION_AGENT_BRANCH":   "scion/agent-1",
		"SCION_PROJECT_ID":     "proj-123",
	} {
		got, ok := envValue(ic.Env, name)
		assert.True(t, ok, "init env %s missing", name)
		assert.Equal(t, want, got, "init env %s", name)
	}
	for _, name := range []string{"SCION_CLONE_URL", "SCION_CLONE_BRANCH", "SCION_WORKSPACE_PATH"} {
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
}

// Shared dirs keep their own mounts in the init container, next to the
// agent's directory.
func TestBuildPod_NFSAgentDir_SharedDirs(t *testing.T) {
	cfg := nfsAgentDirConfig("cpa-shared")
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
func TestBuildPod_NFSAgentDir_LockLoserProvisions(t *testing.T) {
	cfg := nfsAgentDirConfig("cpa-loser")
	cfg.nfsProvisionLockLost = true
	cfg.NFSWorkspacePreCreated = true
	pod, err := newNFSTestK8sRuntime().buildPod("default", cfg)
	require.NoError(t, err)
	ic := pod.Spec.InitContainers[0]
	assert.False(t, hasFlag(ic.Command, "--wait-for-sentinel"))
	require.NotNil(t, ic.SecurityContext.RunAsUser)
	assert.Equal(t, int64(0), *ic.SecurityContext.RunAsUser)
	v, ok := envValue(ic.Env, provision.ChownBestEffortEnv)
	assert.True(t, ok)
	assert.Equal(t, "1", v)
}

// Run: a broker lock loser in clone-per-agent mode creates a pod whose init
// container prepares the agent directory.
func TestRun_NFSAgentDirLockLost_CreatesProvisioningPod(t *testing.T) {
	r := newNFSTestK8sRuntime()
	cfg := nfsAgentDirConfig("scion-cpa-lock-lost")
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
	ic := pods.Items[0].Spec.InitContainers[0]
	assert.False(t, hasFlag(ic.Command, "--wait-for-sentinel"))
	v, _ := envValue(ic.Env, "SCION_WORKSPACE_MODE")
	assert.Equal(t, "clone-per-agent", v)
}

// The shared-plain pod is unchanged by the branch field alone, and without
// the NFS init container the agent directory fields are ignored.
func TestBuildPod_NFSAgentDir_OtherModesUnchanged(t *testing.T) {
	for _, lockLost := range []bool{false, true} {
		cfg := nfsBaseConfig("plain")
		cfg.nfsProvisionLockLost = lockLost
		pod, err := newNFSTestK8sRuntime().buildPod("default", cfg)
		require.NoError(t, err)
		withBranch := cfg
		withBranch.NFSAgentBranch = "scion/agent-1"
		pod2, err := newNFSTestK8sRuntime().buildPod("default", withBranch)
		require.NoError(t, err)
		assert.Equal(t, pod.Spec, pod2.Spec, "lockLost=%v", lockLost)
	}

	for _, mutate := range []func(*RunConfig){
		func(c *RunConfig) { c.WorkspaceBackendName = "" },
		func(c *RunConfig) { c.NFSPVClaimName = "" },
	} {
		with := nfsAgentDirConfig("no-nfs")
		mutate(&with)
		without := with
		without.NFSAgentDirName = ""
		without.NFSAgentBranch = ""
		podWith, err := newNFSTestK8sRuntime().buildPod("default", with)
		require.NoError(t, err)
		podWithout, err := newNFSTestK8sRuntime().buildPod("default", without)
		require.NoError(t, err)
		assert.Equal(t, podWithout.Spec, podWith.Spec)
	}

	// The worktree pod is unaffected by the agent branch field.
	wt := nfsWorktreeConfig("wt")
	podWt, err := newNFSTestK8sRuntime().buildPod("default", wt)
	require.NoError(t, err)
	wt.NFSAgentBranch = "scion/agent-1"
	podWt2, err := newNFSTestK8sRuntime().buildPod("default", wt)
	require.NoError(t, err)
	assert.Equal(t, podWt.Spec, podWt2.Spec)
}

// The agent name becomes a path segment; anything that is not an agent
// slug is refused, and an agent cannot be in both layouts.
func TestBuildPod_NFSAgentDir_RejectsBadConfig(t *testing.T) {
	for _, name := range []string{"..", ".", "a/b", "../x", `a\b`, "a\x00b", "Agent-1", "a.b", "-a"} {
		cfg := nfsAgentDirConfig("bad-name")
		cfg.NFSAgentDirName = name
		_, err := newNFSTestK8sRuntime().buildPod("default", cfg)
		assert.Error(t, err, "agent name %q", name)
	}
	both := nfsAgentDirConfig("both")
	both.NFSWorktreeName = "agent-1"
	_, err := newNFSTestK8sRuntime().buildPod("default", both)
	assert.Error(t, err)
}

func TestNFSAgentDirSubPath(t *testing.T) {
	got, err := NFSAgentDirSubPath("projects/p/workspace", "agent-1")
	require.NoError(t, err)
	assert.Equal(t, "projects/p/agents/agent-1", got)
	for _, sub := range []string{"", "workspace", "/projects/p/workspace", "../p/workspace", "projects/p/other"} {
		_, err := NFSAgentDirSubPath(sub, "agent-1")
		assert.Error(t, err, "subPath %q", sub)
	}
	_, err = NFSAgentDirSubPath("projects/p/workspace", "a/b")
	assert.Error(t, err)
}
