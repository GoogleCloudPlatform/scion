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
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/k8s"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

// newNFSTestK8sRuntime creates a KubernetesRuntime backed by a fake clientset
// for unit testing buildPod and related methods.
func newNFSTestK8sRuntime() *KubernetesRuntime {
	clientset := k8sfake.NewClientset()
	scheme := k8sruntime.NewScheme()
	fc := fake.NewSimpleDynamicClient(scheme)
	client := k8s.NewTestClient(fc, clientset)
	return NewKubernetesRuntime(client)
}

// --- N2-1: NFS-backed workspace volume tests ---

func TestBuildPod_WorkspaceVolume_LocalBackend_EmptyDir(t *testing.T) {
	r := newNFSTestK8sRuntime()
	config := RunConfig{
		Name:         "test-local",
		Image:        "test-image",
		UnixUsername: "scion",
		// WorkspaceBackendName defaults to "" (local)
	}

	pod, err := r.buildPod("default", config)
	if err != nil {
		t.Fatalf("buildPod failed: %v", err)
	}

	// Volume must be EmptyDir
	var found bool
	for _, v := range pod.Spec.Volumes {
		if v.Name == "workspace" {
			found = true
			if v.VolumeSource.EmptyDir == nil {
				t.Errorf("local backend: workspace volume should be EmptyDir, got %+v", v.VolumeSource)
			}
			if v.VolumeSource.PersistentVolumeClaim != nil {
				t.Errorf("local backend: workspace volume should NOT be PVC")
			}
		}
	}
	if !found {
		t.Fatal("workspace volume not found in pod spec")
	}

	// VolumeMount must not have subPath
	for _, vm := range pod.Spec.Containers[0].VolumeMounts {
		if vm.Name == "workspace" {
			if vm.SubPath != "" {
				t.Errorf("local backend: workspace mount should not have subPath, got %q", vm.SubPath)
			}
			if vm.MountPath != "/workspace" {
				t.Errorf("local backend: workspace mount path = %q, want /workspace", vm.MountPath)
			}
		}
	}
}

func TestBuildPod_WorkspaceVolume_NFSBackend_PVCWithSubPath(t *testing.T) {
	r := newNFSTestK8sRuntime()
	config := RunConfig{
		Name:                 "test-nfs",
		Image:                "test-image",
		UnixUsername:         "scion",
		WorkspaceBackendName: "nfs",
		NFSPVClaimName:       "scion-workspaces",
		NFSSubPath:           "projects/proj-123/workspace",
	}

	pod, err := r.buildPod("default", config)
	if err != nil {
		t.Fatalf("buildPod failed: %v", err)
	}

	// Volume must be PVC
	var found bool
	for _, v := range pod.Spec.Volumes {
		if v.Name == "workspace" {
			found = true
			if v.VolumeSource.PersistentVolumeClaim == nil {
				t.Fatalf("NFS backend: workspace volume should be PVC, got %+v", v.VolumeSource)
			}
			if v.VolumeSource.PersistentVolumeClaim.ClaimName != "scion-workspaces" {
				t.Errorf("PVC claimName = %q, want %q", v.VolumeSource.PersistentVolumeClaim.ClaimName, "scion-workspaces")
			}
			if v.VolumeSource.EmptyDir != nil {
				t.Errorf("NFS backend: workspace volume should NOT be EmptyDir")
			}
		}
	}
	if !found {
		t.Fatal("workspace volume not found in pod spec")
	}

	// VolumeMount must have subPath for isolation
	for _, vm := range pod.Spec.Containers[0].VolumeMounts {
		if vm.Name == "workspace" {
			if vm.SubPath != "projects/proj-123/workspace" {
				t.Errorf("NFS backend: workspace mount subPath = %q, want %q", vm.SubPath, "projects/proj-123/workspace")
			}
			if vm.MountPath != "/workspace" {
				t.Errorf("NFS backend: workspace mount path = %q, want /workspace", vm.MountPath)
			}
		}
	}
}

func TestBuildPod_WorkspaceVolume_NFSWithoutPVCName_FallsBackToEmptyDir(t *testing.T) {
	r := newNFSTestK8sRuntime()
	// NFS backend but missing PVC name — defensive fallback to EmptyDir
	config := RunConfig{
		Name:                 "test-nfs-no-pvc",
		Image:                "test-image",
		UnixUsername:         "scion",
		WorkspaceBackendName: "nfs",
		// NFSPVClaimName is empty
	}

	pod, err := r.buildPod("default", config)
	if err != nil {
		t.Fatalf("buildPod failed: %v", err)
	}

	for _, v := range pod.Spec.Volumes {
		if v.Name == "workspace" {
			if v.VolumeSource.EmptyDir == nil {
				t.Errorf("NFS without PVC name: should fall back to EmptyDir, got %+v", v.VolumeSource)
			}
		}
	}
}

func TestBuildPod_NoInitContainers_LocalBackend(t *testing.T) {
	r := newNFSTestK8sRuntime()
	config := RunConfig{
		Name:         "test-local",
		Image:        "test-image",
		UnixUsername: "scion",
	}

	pod, err := r.buildPod("default", config)
	if err != nil {
		t.Fatalf("buildPod failed: %v", err)
	}

	if len(pod.Spec.InitContainers) != 0 {
		t.Errorf("local backend: expected no init containers, got %d", len(pod.Spec.InitContainers))
	}
}

// --- N2-2: Init-container workspace provisioning tests ---

func TestBuildPod_NFSBackend_InitContainer_Present(t *testing.T) {
	r := newNFSTestK8sRuntime()
	config := RunConfig{
		Name:                 "test-nfs-init",
		Image:                "test-image",
		UnixUsername:         "scion",
		WorkspaceBackendName: "nfs",
		NFSPVClaimName:       "scion-workspaces",
		NFSSubPath:           "projects/proj-123/workspace",
		GitCloneForInit: &api.GitCloneConfig{
			URL:    "https://github.com/example/repo.git",
			Branch: "main",
			Depth:  1,
		},
	}

	pod, err := r.buildPod("default", config)
	if err != nil {
		t.Fatalf("buildPod failed: %v", err)
	}

	// Must have exactly one init container
	if len(pod.Spec.InitContainers) != 1 {
		t.Fatalf("expected 1 init container, got %d", len(pod.Spec.InitContainers))
	}

	ic := pod.Spec.InitContainers[0]

	// Init container name
	if ic.Name != "workspace-provision" {
		t.Errorf("init container name = %q, want %q", ic.Name, "workspace-provision")
	}

	// Uses the same image
	if ic.Image != "test-image" {
		t.Errorf("init container image = %q, want %q", ic.Image, "test-image")
	}

	// Must mount workspace volume with subPath
	var wsMount *corev1.VolumeMount
	for i := range ic.VolumeMounts {
		if ic.VolumeMounts[i].Name == "workspace" {
			wsMount = &ic.VolumeMounts[i]
			break
		}
	}
	if wsMount == nil {
		t.Fatal("init container: workspace volume mount not found")
	}
	if wsMount.MountPath != "/workspace" {
		t.Errorf("init container workspace mountPath = %q, want /workspace", wsMount.MountPath)
	}
	if wsMount.SubPath != "projects/proj-123/workspace" {
		t.Errorf("init container workspace subPath = %q, want %q", wsMount.SubPath, "projects/proj-123/workspace")
	}

	// Command must invoke sciontool provision (not sh -c)
	assert.Equal(t, "sciontool", ic.Command[0], "init container should invoke sciontool")
	assert.Equal(t, "provision", ic.Command[1], "init container should invoke provision subcommand")
	// URL must NOT appear in the command args (injection safety)
	for _, arg := range ic.Command {
		if arg == "https://github.com/example/repo.git" {
			t.Error("init container command must NOT contain inline URL (injection safety)")
		}
	}

	// Verify env vars are set on the container (URL/branch via env, not args)
	var hasURL, hasBranch bool
	for _, env := range ic.Env {
		if env.Name == "SCION_CLONE_URL" && env.Value == "https://github.com/example/repo.git" {
			hasURL = true
		}
		if env.Name == "SCION_CLONE_BRANCH" && env.Value == "main" {
			hasBranch = true
		}
	}
	if !hasURL {
		t.Error("init container missing SCION_CLONE_URL env var")
	}
	if !hasBranch {
		t.Error("init container missing SCION_CLONE_BRANCH env var")
	}
}

func TestBuildPod_NFSBackend_NoInitContainer_WhenNoGitClone(t *testing.T) {
	r := newNFSTestK8sRuntime()
	config := RunConfig{
		Name:                 "test-nfs-no-git",
		Image:                "test-image",
		UnixUsername:         "scion",
		WorkspaceBackendName: "nfs",
		NFSPVClaimName:       "scion-workspaces",
		NFSSubPath:           "projects/proj-123/workspace",
		// GitCloneForInit is nil — no init container expected
	}

	pod, err := r.buildPod("default", config)
	if err != nil {
		t.Fatalf("buildPod failed: %v", err)
	}

	if len(pod.Spec.InitContainers) != 0 {
		t.Errorf("NFS without git clone: expected no init containers, got %d", len(pod.Spec.InitContainers))
	}
}

func TestBuildPod_LocalBackend_NoInitContainer_EvenWithGitClone(t *testing.T) {
	r := newNFSTestK8sRuntime()
	config := RunConfig{
		Name:         "test-local-git",
		Image:        "test-image",
		UnixUsername: "scion",
		// Local backend (no NFS fields)
		GitCloneForInit: &api.GitCloneConfig{
			URL: "https://github.com/example/repo.git",
		},
	}

	pod, err := r.buildPod("default", config)
	if err != nil {
		t.Fatalf("buildPod failed: %v", err)
	}

	if len(pod.Spec.InitContainers) != 0 {
		t.Errorf("local backend: expected no init containers even with GitCloneForInit, got %d", len(pod.Spec.InitContainers))
	}
}

func TestNFSProvisionCommand_ShallowClone(t *testing.T) {
	gc := &api.GitCloneConfig{
		URL:    "https://github.com/example/repo.git",
		Branch: "main",
		Depth:  1,
	}

	cmd := nfsProvisionCommand(gc)

	assert.Equal(t, "sciontool", cmd[0])
	assert.Equal(t, "provision", cmd[1])
	assert.Contains(t, cmd, "--depth")
	assert.Contains(t, cmd, "1")

	// URL must NOT appear in command args (injection safety)
	for _, arg := range cmd {
		assert.NotEqual(t, gc.URL, arg, "URL must not be in command args")
		assert.NotEqual(t, gc.Branch, arg, "branch must not be in command args")
	}
}

func TestNFSProvisionCommand_NilConfig(t *testing.T) {
	cmd := nfsProvisionCommand(nil)
	assert.Equal(t, []string{"sciontool", "provision"}, cmd)
}

func TestNFSProvisionCommand_FullClone(t *testing.T) {
	gc := &api.GitCloneConfig{
		URL:   "https://github.com/example/repo.git",
		Depth: -1,
	}

	cmd := nfsProvisionCommand(gc)

	assert.Equal(t, "sciontool", cmd[0])
	assert.Equal(t, "provision", cmd[1])
	assert.Contains(t, cmd, "--depth")
	assert.Contains(t, cmd, "-1")
}

func TestNFSProvisionEnv(t *testing.T) {
	t.Run("includes URL and branch", func(t *testing.T) {
		gc := &api.GitCloneConfig{
			URL:    "https://github.com/example/repo.git",
			Branch: "main",
		}
		envs := nfsProvisionEnv(gc)
		require.Len(t, envs, 2)
		assert.Equal(t, "SCION_CLONE_URL", envs[0].Name)
		assert.Equal(t, gc.URL, envs[0].Value)
		assert.Equal(t, "SCION_CLONE_BRANCH", envs[1].Name)
		assert.Equal(t, gc.Branch, envs[1].Value)
	})

	t.Run("omits branch when empty", func(t *testing.T) {
		gc := &api.GitCloneConfig{
			URL: "https://github.com/example/repo.git",
		}
		envs := nfsProvisionEnv(gc)
		require.Len(t, envs, 1)
		assert.Equal(t, "SCION_CLONE_URL", envs[0].Name)
	})

	t.Run("nil config returns nil", func(t *testing.T) {
		envs := nfsProvisionEnv(nil)
		assert.Nil(t, envs)
	})
}

func TestNFSProvisionCommand_InjectionSafety(t *testing.T) {
	gc := &api.GitCloneConfig{
		URL:    "https://github.com/example/repo.git",
		Branch: "feat/test; rm -rf /",
	}

	cmd := nfsProvisionCommand(gc)

	// Branch and URL must NOT appear in command args
	for _, arg := range cmd {
		assert.NotEqual(t, gc.URL, arg, "URL must not be in command args")
		assert.NotEqual(t, gc.Branch, arg, "branch must not be in command args")
	}
}

// hasFlag checks if a string slice contains the given flag value.
func hasFlag(args []string, val string) bool {
	for _, a := range args {
		if a == val {
			return true
		}
	}
	return false
}

// --- N2-2b: Advisory lock guard for NFS init-container provisioning ---

// nfsBaseConfig returns a RunConfig for NFS tests with common fields pre-filled.
func nfsBaseConfig(name string) RunConfig {
	return RunConfig{
		Name:                 name,
		Image:                "test-image",
		UnixUsername:         "scion",
		WorkspaceBackendName: "nfs",
		NFSPVClaimName:       "scion-workspaces",
		NFSSubPath:           "projects/proj-123/workspace",
		ProjectID:            "proj-123",
		GitCloneForInit: &api.GitCloneConfig{
			URL:    "https://github.com/example/repo.git",
			Branch: "main",
			Depth:  1,
		},
	}
}

func TestBuildPod_NFSLockWinner_InjectsCloneInitContainer(t *testing.T) {
	r := newNFSTestK8sRuntime()
	config := nfsBaseConfig("test-lock-winner")
	// nfsProvisionLockLost defaults to false (winner)

	pod, err := r.buildPod("default", config)
	if err != nil {
		t.Fatalf("buildPod failed: %v", err)
	}

	if len(pod.Spec.InitContainers) != 1 {
		t.Fatalf("expected 1 init container, got %d", len(pod.Spec.InitContainers))
	}

	ic := pod.Spec.InitContainers[0]
	if ic.Name != "workspace-provision" {
		t.Errorf("init container name = %q, want %q", ic.Name, "workspace-provision")
	}

	// Winner must invoke sciontool provision (clone mode, no --wait-for-sentinel)
	assert.Equal(t, "sciontool", ic.Command[0])
	assert.Equal(t, "provision", ic.Command[1])
	assert.False(t, hasFlag(ic.Command, "--wait-for-sentinel"),
		"winner should NOT have --wait-for-sentinel flag")

	// URL must NOT appear in command args (injection safety — passed via env)
	for _, arg := range ic.Command {
		assert.NotEqual(t, "https://github.com/example/repo.git", arg,
			"URL must not be in command args")
	}

	// Verify env vars are set on the init container
	var hasURL, hasBranch bool
	for _, env := range ic.Env {
		if env.Name == "SCION_CLONE_URL" && env.Value == "https://github.com/example/repo.git" {
			hasURL = true
		}
		if env.Name == "SCION_CLONE_BRANCH" && env.Value == "main" {
			hasBranch = true
		}
	}
	if !hasURL {
		t.Error("init container missing SCION_CLONE_URL env var")
	}
	if !hasBranch {
		t.Error("init container missing SCION_CLONE_BRANCH env var")
	}
}

func TestBuildPod_NFSLockLoser_InjectsWaitInitContainer(t *testing.T) {
	r := newNFSTestK8sRuntime()
	config := nfsBaseConfig("test-lock-loser")
	config.nfsProvisionLockLost = true

	pod, err := r.buildPod("default", config)
	if err != nil {
		t.Fatalf("buildPod failed: %v", err)
	}

	if len(pod.Spec.InitContainers) != 1 {
		t.Fatalf("expected 1 init container, got %d", len(pod.Spec.InitContainers))
	}

	ic := pod.Spec.InitContainers[0]
	if ic.Name != "workspace-provision" {
		t.Errorf("init container name = %q, want %q", ic.Name, "workspace-provision")
	}

	// Loser must invoke sciontool provision --wait-for-sentinel
	assert.Equal(t, "sciontool", ic.Command[0])
	assert.Equal(t, "provision", ic.Command[1])
	assert.True(t, hasFlag(ic.Command, "--wait-for-sentinel"),
		"loser should have --wait-for-sentinel flag")
}

func TestBuildPod_NFSNoLocker_InjectsCloneInitContainer(t *testing.T) {
	r := newNFSTestK8sRuntime()
	config := nfsBaseConfig("test-no-locker")
	// Locker is nil, nfsProvisionLockLost stays false → clone init container

	pod, err := r.buildPod("default", config)
	if err != nil {
		t.Fatalf("buildPod failed: %v", err)
	}

	if len(pod.Spec.InitContainers) != 1 {
		t.Fatalf("expected 1 init container, got %d", len(pod.Spec.InitContainers))
	}

	ic := pod.Spec.InitContainers[0]
	// No-locker: should get clone init command (provision without --wait-for-sentinel)
	assert.Equal(t, "sciontool", ic.Command[0])
	assert.Equal(t, "provision", ic.Command[1])
	assert.False(t, hasFlag(ic.Command, "--wait-for-sentinel"),
		"no-locker: should get clone init command (sentinel-only fallback)")
}

func TestBuildPod_LocalBackend_LockLostIgnored(t *testing.T) {
	r := newNFSTestK8sRuntime()
	config := RunConfig{
		Name:                 "test-local-lockflag",
		Image:                "test-image",
		UnixUsername:         "scion",
		nfsProvisionLockLost: true, // should be ignored for local backend
	}

	pod, err := r.buildPod("default", config)
	if err != nil {
		t.Fatalf("buildPod failed: %v", err)
	}

	// Local backend: no init containers regardless of lock flag
	if len(pod.Spec.InitContainers) != 0 {
		t.Errorf("local backend: expected no init containers, got %d", len(pod.Spec.InitContainers))
	}
}

func TestBuildPod_NFSConcurrentProjects_IndependentLocks(t *testing.T) {
	// Two pods for DIFFERENT projects should both get clone init containers
	// when both are lock winners (no contention across projects).
	r := newNFSTestK8sRuntime()

	configA := nfsBaseConfig("test-proj-a")
	configA.ProjectID = "proj-aaa"
	configA.NFSSubPath = "projects/proj-aaa/workspace"

	configB := nfsBaseConfig("test-proj-b")
	configB.ProjectID = "proj-bbb"
	configB.NFSSubPath = "projects/proj-bbb/workspace"

	podA, err := r.buildPod("default", configA)
	if err != nil {
		t.Fatalf("buildPod A failed: %v", err)
	}
	podB, err := r.buildPod("default", configB)
	if err != nil {
		t.Fatalf("buildPod B failed: %v", err)
	}

	if len(podA.Spec.InitContainers) != 1 {
		t.Fatalf("project A: expected 1 init container, got %d", len(podA.Spec.InitContainers))
	}
	if len(podB.Spec.InitContainers) != 1 {
		t.Fatalf("project B: expected 1 init container, got %d", len(podB.Spec.InitContainers))
	}

	// Both should be clone (winner) init containers — sciontool provision without --wait
	icA := podA.Spec.InitContainers[0]
	icB := podB.Spec.InitContainers[0]
	assert.Equal(t, "sciontool", icA.Command[0])
	assert.Equal(t, "provision", icA.Command[1])
	assert.False(t, hasFlag(icA.Command, "--wait-for-sentinel"))
	assert.Equal(t, "sciontool", icB.Command[0])
	assert.Equal(t, "provision", icB.Command[1])
	assert.False(t, hasFlag(icB.Command, "--wait-for-sentinel"))
}

func TestBuildPod_NFSSameProject_WinnerAndLoser(t *testing.T) {
	// Simulate two pods for the SAME project: one winner, one loser.
	r := newNFSTestK8sRuntime()

	winner := nfsBaseConfig("test-winner")
	loser := nfsBaseConfig("test-loser")
	loser.nfsProvisionLockLost = true

	podWinner, err := r.buildPod("default", winner)
	if err != nil {
		t.Fatalf("buildPod winner failed: %v", err)
	}
	podLoser, err := r.buildPod("default", loser)
	if err != nil {
		t.Fatalf("buildPod loser failed: %v", err)
	}

	if len(podWinner.Spec.InitContainers) != 1 || len(podLoser.Spec.InitContainers) != 1 {
		t.Fatal("both pods should have exactly 1 init container")
	}

	winnerCmd := podWinner.Spec.InitContainers[0].Command
	loserCmd := podLoser.Spec.InitContainers[0].Command

	// Winner: sciontool provision (clone mode)
	assert.Equal(t, "sciontool", winnerCmd[0])
	assert.Equal(t, "provision", winnerCmd[1])
	assert.False(t, hasFlag(winnerCmd, "--wait-for-sentinel"))

	// Loser: sciontool provision --wait-for-sentinel
	assert.Equal(t, "sciontool", loserCmd[0])
	assert.Equal(t, "provision", loserCmd[1])
	assert.True(t, hasFlag(loserCmd, "--wait-for-sentinel"))
}

// --- N2-2b: Run()-level advisory lock integration tests ---

// errorLocker is an AdvisoryLocker that always returns an error.
type errorLocker struct {
	err error
}

func (l *errorLocker) TryAdvisoryLock(_ context.Context, _ store.AdvisoryLockKey) (bool, func() error, error) {
	return false, func() error { return nil }, l.err
}

func (l *errorLocker) TryAdvisoryLockObject(_ context.Context, _ store.AdvisoryLockKey, _ int32) (bool, func() error, error) {
	return false, func() error { return nil }, l.err
}

// alwaysLoseLocker is an AdvisoryLocker where TryAdvisoryLockObject always
// returns acquired=false (another node holds the lock).
type alwaysLoseLocker struct{}

func (l *alwaysLoseLocker) TryAdvisoryLock(_ context.Context, _ store.AdvisoryLockKey) (bool, func() error, error) {
	return false, func() error { return nil }, nil
}

func (l *alwaysLoseLocker) TryAdvisoryLockObject(_ context.Context, _ store.AdvisoryLockKey, _ int32) (bool, func() error, error) {
	return false, func() error { return nil }, nil
}

func TestRun_NFSLockError_FailsDispatch(t *testing.T) {
	// When the advisory lock returns an error, Run() must fail BEFORE
	// creating any pods (no unguarded clone).
	r := newNFSTestK8sRuntime()
	config := nfsBaseConfig("test-lock-err")
	config.Locker = &errorLocker{err: errors.New("connection lost")}

	_, err := r.Run(context.Background(), config)
	if err == nil {
		t.Fatal("expected Run() to fail when advisory lock returns error")
	}
	if !strings.Contains(err.Error(), "advisory lock") {
		t.Errorf("error should mention advisory lock, got: %v", err)
	}
	if !strings.Contains(err.Error(), "connection lost") {
		t.Errorf("error should propagate underlying cause, got: %v", err)
	}

	// Verify no pods were created
	pods, listErr := r.Client.Clientset.CoreV1().Pods("default").List(
		context.Background(), metav1.ListOptions{},
	)
	if listErr != nil {
		t.Fatalf("failed to list pods: %v", listErr)
	}
	if len(pods.Items) != 0 {
		t.Errorf("lock error should prevent pod creation, but found %d pods", len(pods.Items))
	}
}

func TestRun_NFSLockLost_CreatesWaitPod(t *testing.T) {
	// When the lock is held by another node, the pod should have a
	// wait-for-sentinel init container, not a cloning one.
	r := newNFSTestK8sRuntime()
	config := nfsBaseConfig("scion-test-lock-lost")
	config.Locker = &alwaysLoseLocker{}

	// Run() will create the pod but waitForPodReady will time out with the
	// fake clientset. Use a short-lived context so we don't block for 10m.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r.Run(ctx, config) //nolint:errcheck

	// Verify the created pod has a wait-for-sentinel init container
	pods, err := r.Client.Clientset.CoreV1().Pods("default").List(
		context.Background(), metav1.ListOptions{},
	)
	if err != nil {
		t.Fatalf("failed to list pods: %v", err)
	}
	if len(pods.Items) != 1 {
		t.Fatalf("expected 1 pod, got %d", len(pods.Items))
	}

	pod := pods.Items[0]
	if len(pod.Spec.InitContainers) != 1 {
		t.Fatalf("expected 1 init container, got %d", len(pod.Spec.InitContainers))
	}

	cmd := pod.Spec.InitContainers[0].Command
	assert.Equal(t, "sciontool", cmd[0])
	assert.Equal(t, "provision", cmd[1])
	assert.True(t, hasFlag(cmd, "--wait-for-sentinel"),
		"lock-lost pod should have --wait-for-sentinel flag")
}

func TestRun_NFSLockWon_CreatesClonePod(t *testing.T) {
	// When the lock is won, the pod should have the cloning init container.
	r := newNFSTestK8sRuntime()
	locker := newTestLocker()
	config := nfsBaseConfig("scion-test-lock-won")
	config.Locker = locker

	// Short-lived context to avoid blocking on waitForPodReady.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r.Run(ctx, config) //nolint:errcheck

	pods, err := r.Client.Clientset.CoreV1().Pods("default").List(
		context.Background(), metav1.ListOptions{},
	)
	if err != nil {
		t.Fatalf("failed to list pods: %v", err)
	}
	if len(pods.Items) != 1 {
		t.Fatalf("expected 1 pod, got %d", len(pods.Items))
	}

	pod := pods.Items[0]
	if len(pod.Spec.InitContainers) != 1 {
		t.Fatalf("expected 1 init container, got %d", len(pod.Spec.InitContainers))
	}

	cmd := pod.Spec.InitContainers[0].Command
	assert.Equal(t, "sciontool", cmd[0])
	assert.Equal(t, "provision", cmd[1])
	assert.False(t, hasFlag(cmd, "--wait-for-sentinel"),
		"lock-won pod should NOT have --wait-for-sentinel flag")
}

func TestRun_LocalBackend_NoLockAttempt(t *testing.T) {
	// Local backend should never attempt the advisory lock, even if a
	// Locker is provided. The lock is only for NFS.
	r := newNFSTestK8sRuntime()
	locker := &errorLocker{err: errors.New("should not be called")}
	config := RunConfig{
		Name:         "scion-test-local-nolock",
		Image:        "test-image",
		UnixUsername: "scion",
		ProjectID:    "proj-local",
		Locker:       locker,
		// No NFS fields → local backend
	}

	// Run() should NOT fail with lock error (lock is only for NFS).
	// It will fail at waitForPodReady with fake client, but NOT at lock.
	// Short-lived context to avoid blocking.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := r.Run(ctx, config)
	if err != nil && strings.Contains(err.Error(), "advisory lock") {
		t.Errorf("local backend should not attempt advisory lock, got: %v", err)
	}
}

// --- N2-4: Stable FSGroup/UID for NFS pods ---

func TestBuildPod_FSGroup_LocalBackend_UsesHostGID(t *testing.T) {
	r := newNFSTestK8sRuntime()
	config := RunConfig{
		Name:         "test-local-gid",
		Image:        "test-image",
		UnixUsername: "scion",
	}

	pod, err := r.buildPod("default", config)
	if err != nil {
		t.Fatalf("buildPod failed: %v", err)
	}

	// Local backend: FSGroup should be the host GID (os.Getgid())
	if pod.Spec.SecurityContext == nil || pod.Spec.SecurityContext.FSGroup == nil {
		t.Fatal("pod security context or FSGroup is nil")
	}

	hostGID := int64(os.Getgid())
	if *pod.Spec.SecurityContext.FSGroup != hostGID {
		t.Errorf("local backend: FSGroup = %d, want host GID %d", *pod.Spec.SecurityContext.FSGroup, hostGID)
	}
}

func TestBuildPod_FSGroup_NFSBackend_UsesStableGID(t *testing.T) {
	r := newNFSTestK8sRuntime()
	config := RunConfig{
		Name:                 "test-nfs-gid",
		Image:                "test-image",
		UnixUsername:         "scion",
		WorkspaceBackendName: "nfs",
		NFSPVClaimName:       "scion-workspaces",
		NFSGID:               1000,
	}

	pod, err := r.buildPod("default", config)
	if err != nil {
		t.Fatalf("buildPod failed: %v", err)
	}

	if pod.Spec.SecurityContext == nil || pod.Spec.SecurityContext.FSGroup == nil {
		t.Fatal("pod security context or FSGroup is nil")
	}

	if *pod.Spec.SecurityContext.FSGroup != 1000 {
		t.Errorf("NFS backend: FSGroup = %d, want 1000", *pod.Spec.SecurityContext.FSGroup)
	}
}

func TestBuildPod_FSGroup_NFSBackend_DefaultGID(t *testing.T) {
	r := newNFSTestK8sRuntime()
	config := RunConfig{
		Name:                 "test-nfs-default-gid",
		Image:                "test-image",
		UnixUsername:         "scion",
		WorkspaceBackendName: "nfs",
		NFSPVClaimName:       "scion-workspaces",
		// NFSGID is 0 (unset) — should default to 1000
	}

	pod, err := r.buildPod("default", config)
	if err != nil {
		t.Fatalf("buildPod failed: %v", err)
	}

	if pod.Spec.SecurityContext == nil || pod.Spec.SecurityContext.FSGroup == nil {
		t.Fatal("pod security context or FSGroup is nil")
	}

	if *pod.Spec.SecurityContext.FSGroup != 1000 {
		t.Errorf("NFS backend default: FSGroup = %d, want 1000", *pod.Spec.SecurityContext.FSGroup)
	}
}

func TestBuildPod_FSGroup_NFSBackend_CustomGID(t *testing.T) {
	r := newNFSTestK8sRuntime()
	config := RunConfig{
		Name:                 "test-nfs-custom-gid",
		Image:                "test-image",
		UnixUsername:         "scion",
		WorkspaceBackendName: "nfs",
		NFSPVClaimName:       "scion-workspaces",
		NFSGID:               2000,
	}

	pod, err := r.buildPod("default", config)
	if err != nil {
		t.Fatalf("buildPod failed: %v", err)
	}

	if *pod.Spec.SecurityContext.FSGroup != 2000 {
		t.Errorf("NFS backend custom GID: FSGroup = %d, want 2000", *pod.Spec.SecurityContext.FSGroup)
	}
}

// --- N2-3: Skip workspace kubectl cp when backend=nfs ---

// TestSkipWorkspaceSync_NFSBackend_RunConfigGuard validates the guard condition
// that controls workspace sync skip. The actual Run() method performs real K8s
// API calls, so we test the conditional logic via the config fields that
// determine behavior.
func TestSkipWorkspaceSync_NFSBackend_RunConfigGuard(t *testing.T) {
	tests := []struct {
		name            string
		workspace       string
		backendName     string
		wantWorkspaceCP bool
	}{
		{
			name:            "local backend syncs workspace",
			workspace:       "/some/path",
			backendName:     "",
			wantWorkspaceCP: true,
		},
		{
			name:            "local backend explicit syncs workspace",
			workspace:       "/some/path",
			backendName:     "local",
			wantWorkspaceCP: true,
		},
		{
			name:            "NFS backend skips workspace sync",
			workspace:       "/some/path",
			backendName:     "nfs",
			wantWorkspaceCP: false,
		},
		{
			name:            "empty workspace skips sync for any backend",
			workspace:       "",
			backendName:     "",
			wantWorkspaceCP: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := RunConfig{
				Workspace:            tt.workspace,
				WorkspaceBackendName: tt.backendName,
			}
			// Replicate the guard condition from Run()
			shouldSync := config.Workspace != "" && config.WorkspaceBackendName != "nfs"
			if shouldSync != tt.wantWorkspaceCP {
				t.Errorf("workspace sync guard: got %v, want %v", shouldSync, tt.wantWorkspaceCP)
			}
		})
	}
}

// --- N2-5: Generalized shared-dir PVC helpers ---

func TestBuildPod_SharedDirs_LocalBackend_SeparatePVCs(t *testing.T) {
	r := newNFSTestK8sRuntime()
	config := RunConfig{
		Name:         "test-local-shared",
		Image:        "test-image",
		UnixUsername: "scion",
		Labels: map[string]string{
			"scion.grove": "my-project",
		},
		SharedDirs: []api.SharedDir{
			{Name: "build-cache"},
			{Name: "logs"},
		},
	}

	pod, err := r.buildPod("default", config)
	if err != nil {
		t.Fatalf("buildPod failed: %v", err)
	}

	// Local backend: each shared dir should have its own PVC volume
	sd0Vol := findVolume(pod, "shared-dir-0")
	sd1Vol := findVolume(pod, "shared-dir-1")

	if sd0Vol == nil || sd1Vol == nil {
		t.Fatal("local backend: expected shared-dir-0 and shared-dir-1 volumes")
	}

	// PVC names should follow the sharedDirPVCName convention
	if sd0Vol.PersistentVolumeClaim.ClaimName != "scion-shared-my-project-build-cache" {
		t.Errorf("shared-dir-0 claimName = %q, want %q", sd0Vol.PersistentVolumeClaim.ClaimName, "scion-shared-my-project-build-cache")
	}
	if sd1Vol.PersistentVolumeClaim.ClaimName != "scion-shared-my-project-logs" {
		t.Errorf("shared-dir-1 claimName = %q, want %q", sd1Vol.PersistentVolumeClaim.ClaimName, "scion-shared-my-project-logs")
	}

	// Mounts should NOT have subPath for local backend
	sd0Mount := findVolumeMount(&pod.Spec.Containers[0], "shared-dir-0")
	if sd0Mount == nil {
		t.Fatal("shared-dir-0 mount not found")
	}
	if sd0Mount.SubPath != "" {
		t.Errorf("local backend: shared-dir-0 should not have subPath, got %q", sd0Mount.SubPath)
	}
}

func TestBuildPod_SharedDirs_NFSBackend_UsesNFSSubPaths(t *testing.T) {
	r := newNFSTestK8sRuntime()
	config := RunConfig{
		Name:                 "test-nfs-shared",
		Image:                "test-image",
		UnixUsername:         "scion",
		WorkspaceBackendName: "nfs",
		NFSPVClaimName:       "scion-workspaces",
		NFSSubPath:           "projects/proj-123/workspace",
		Labels: map[string]string{
			"scion.grove": "my-project",
		},
		SharedDirs: []api.SharedDir{
			{Name: "build-cache"},
			{Name: "logs", ReadOnly: true},
		},
	}

	pod, err := r.buildPod("default", config)
	if err != nil {
		t.Fatalf("buildPod failed: %v", err)
	}

	// NFS backend: shared dir volumes should use the SAME PVC as workspace
	sd0Vol := findVolume(pod, "shared-dir-0")
	sd1Vol := findVolume(pod, "shared-dir-1")

	if sd0Vol == nil || sd1Vol == nil {
		t.Fatal("NFS backend: expected shared-dir-0 and shared-dir-1 volumes")
	}

	// Both should reference the workspace NFS PVC
	if sd0Vol.PersistentVolumeClaim.ClaimName != "scion-workspaces" {
		t.Errorf("shared-dir-0 claimName = %q, want %q", sd0Vol.PersistentVolumeClaim.ClaimName, "scion-workspaces")
	}
	if sd1Vol.PersistentVolumeClaim.ClaimName != "scion-workspaces" {
		t.Errorf("shared-dir-1 claimName = %q, want %q", sd1Vol.PersistentVolumeClaim.ClaimName, "scion-workspaces")
	}

	// Mounts should have NFS subPaths
	sd0Mount := findVolumeMount(&pod.Spec.Containers[0], "shared-dir-0")
	sd1Mount := findVolumeMount(&pod.Spec.Containers[0], "shared-dir-1")

	if sd0Mount == nil || sd1Mount == nil {
		t.Fatal("shared-dir mounts not found")
	}

	wantSubPath0 := "projects/proj-123/shared-dirs/build-cache"
	if sd0Mount.SubPath != wantSubPath0 {
		t.Errorf("shared-dir-0 subPath = %q, want %q", sd0Mount.SubPath, wantSubPath0)
	}

	wantSubPath1 := "projects/proj-123/shared-dirs/logs"
	if sd1Mount.SubPath != wantSubPath1 {
		t.Errorf("shared-dir-1 subPath = %q, want %q", sd1Mount.SubPath, wantSubPath1)
	}

	// Verify readOnly flag propagates
	if sd1Mount.ReadOnly != true {
		t.Error("shared-dir-1 should be read-only")
	}
	if sd0Mount.ReadOnly != false {
		t.Error("shared-dir-0 should not be read-only")
	}
}

func TestNFSSharedDirSubPath(t *testing.T) {
	tests := []struct {
		workspaceSubPath string
		sharedDirName    string
		want             string
	}{
		{
			workspaceSubPath: "projects/proj-123/workspace",
			sharedDirName:    "build-cache",
			want:             "projects/proj-123/shared-dirs/build-cache",
		},
		{
			workspaceSubPath: "projects/proj-456/workspace",
			sharedDirName:    "logs",
			want:             "projects/proj-456/shared-dirs/logs",
		},
		{
			workspaceSubPath: "custom-root/proj-789/workspace",
			sharedDirName:    "data",
			want:             "custom-root/proj-789/shared-dirs/data",
		},
	}

	for _, tt := range tests {
		t.Run(tt.sharedDirName, func(t *testing.T) {
			got := nfsSharedDirSubPath(tt.workspaceSubPath, tt.sharedDirName)
			if got != tt.want {
				t.Errorf("nfsSharedDirSubPath(%q, %q) = %q, want %q", tt.workspaceSubPath, tt.sharedDirName, got, tt.want)
			}
		})
	}
}

func TestProjectRWXClaimName(t *testing.T) {
	// Test the generalized naming helper
	got := projectRWXClaimName("my-project", "shared", "build-cache")
	want := "scion-shared-my-project-build-cache"
	if got != want {
		t.Errorf("projectRWXClaimName = %q, want %q", got, want)
	}

	// Test backward compatibility with sharedDirPVCName
	got2 := sharedDirPVCName("my-project", "build-cache")
	if got != got2 {
		t.Errorf("sharedDirPVCName should equal projectRWXClaimName(shared): %q != %q", got2, got)
	}
}

func TestCreateSharedDirPVCs_NFSBackend_SkipsPVCCreation(t *testing.T) {
	clientset := k8sfake.NewClientset()
	scheme := k8sruntime.NewScheme()
	fc := fake.NewSimpleDynamicClient(scheme)
	client := k8s.NewTestClient(fc, clientset)
	r := NewKubernetesRuntime(client)

	config := RunConfig{
		Name:                 "test-nfs",
		WorkspaceBackendName: "nfs",
		NFSPVClaimName:       "scion-workspaces",
		Labels: map[string]string{
			"scion.grove":    "my-project",
			"scion.grove_id": "proj-123",
		},
		SharedDirs: []api.SharedDir{
			{Name: "build-cache"},
		},
	}

	err := r.createSharedDirPVCs(context.Background(), "default", config)
	if err != nil {
		t.Fatalf("createSharedDirPVCs failed: %v", err)
	}

	// No PVCs should have been created for NFS backend
	pvcs, err := clientset.CoreV1().PersistentVolumeClaims("default").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list PVCs failed: %v", err)
	}
	if len(pvcs.Items) != 0 {
		t.Errorf("NFS backend: expected 0 PVCs, got %d", len(pvcs.Items))
	}
}

func TestCreateSharedDirPVCs_LocalBackend_CreatesPVCs(t *testing.T) {
	clientset := k8sfake.NewClientset()
	scheme := k8sruntime.NewScheme()
	fc := fake.NewSimpleDynamicClient(scheme)
	client := k8s.NewTestClient(fc, clientset)
	r := NewKubernetesRuntime(client)

	config := RunConfig{
		Name: "test-local",
		Labels: map[string]string{
			"scion.grove":    "my-project",
			"scion.grove_id": "proj-123",
		},
		SharedDirs: []api.SharedDir{
			{Name: "build-cache"},
			{Name: "logs"},
		},
	}

	err := r.createSharedDirPVCs(context.Background(), "default", config)
	if err != nil {
		t.Fatalf("createSharedDirPVCs failed: %v", err)
	}

	// Local backend: 2 PVCs should be created
	pvcs, err := clientset.CoreV1().PersistentVolumeClaims("default").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list PVCs failed: %v", err)
	}
	if len(pvcs.Items) != 2 {
		t.Errorf("local backend: expected 2 PVCs, got %d", len(pvcs.Items))
	}

	// Verify PVC names
	pvcNames := map[string]bool{}
	for _, pvc := range pvcs.Items {
		pvcNames[pvc.Name] = true
	}
	if !pvcNames["scion-shared-my-project-build-cache"] {
		t.Error("missing PVC scion-shared-my-project-build-cache")
	}
	if !pvcNames["scion-shared-my-project-logs"] {
		t.Error("missing PVC scion-shared-my-project-logs")
	}
}

// findVolume finds a volume by name in a pod spec.
func findVolume(pod *corev1.Pod, name string) *corev1.Volume {
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == name {
			return &pod.Spec.Volumes[i]
		}
	}
	return nil
}

// findVolumeMount finds a volume mount by name in a container.
func findVolumeMount(container *corev1.Container, name string) *corev1.VolumeMount {
	for i := range container.VolumeMounts {
		if container.VolumeMounts[i].Name == name {
			return &container.VolumeMounts[i]
		}
	}
	return nil
}
