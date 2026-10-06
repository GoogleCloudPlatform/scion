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
	"fmt"
	"os"
	"strconv"
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
			if v.EmptyDir == nil {
				t.Errorf("local backend: workspace volume should be EmptyDir, got %+v", v.VolumeSource)
			}
			if v.PersistentVolumeClaim != nil {
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
			if v.PersistentVolumeClaim == nil {
				t.Fatalf("NFS backend: workspace volume should be PVC, got %+v", v.VolumeSource)
			}
			if v.PersistentVolumeClaim.ClaimName != "scion-workspaces" {
				t.Errorf("PVC claimName = %q, want %q", v.PersistentVolumeClaim.ClaimName, "scion-workspaces")
			}
			if v.EmptyDir != nil {
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
			if v.EmptyDir == nil {
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
		ProjectID:            "proj-123",
		GitCloneForInit: &api.GitCloneConfig{
			URL:    "https://github.com/example/repo.git",
			Branch: "main",
			Depth:  intPtr(1),
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
	var hasURL, hasBranch, hasProjectID bool
	for _, env := range ic.Env {
		if env.Name == "SCION_CLONE_URL" && env.Value == "https://github.com/example/repo.git" {
			hasURL = true
		}
		if env.Name == "SCION_CLONE_BRANCH" && env.Value == "main" {
			hasBranch = true
		}
		if env.Name == "SCION_PROJECT_ID" && env.Value == "proj-123" {
			hasProjectID = true
		}
	}
	if !hasURL {
		t.Error("init container missing SCION_CLONE_URL env var")
	}
	if !hasBranch {
		t.Error("init container missing SCION_CLONE_BRANCH env var")
	}
	if !hasProjectID {
		t.Error("init container missing SCION_PROJECT_ID env var (logs would show project=unknown)")
	}
}

// F-111 (design §9): this test used to assert the BUG — that a non-git NFS
// project got no init container at all, and therefore no mkdir/chown ever
// ran for it. The gate is now nfs-backend + bound PV claim, independent of
// git config (nfsProvisionCommand already handles gc == nil gracefully), so
// a non-git project must still get the init container, running plain
// `sciontool provision` with no clone env vars.
func TestBuildPod_NFSBackend_InitContainer_Present_NonGit(t *testing.T) {
	r := newNFSTestK8sRuntime()
	config := RunConfig{
		Name:                 "test-nfs-no-git",
		Image:                "test-image",
		UnixUsername:         "scion",
		WorkspaceBackendName: "nfs",
		NFSPVClaimName:       "scion-workspaces",
		NFSSubPath:           "projects/proj-123/workspace",
		ProjectID:            "proj-123",
		// GitCloneForInit is nil — non-git, shared-plain project.
	}

	pod, err := r.buildPod("default", config)
	if err != nil {
		t.Fatalf("buildPod failed: %v", err)
	}

	if len(pod.Spec.InitContainers) != 1 {
		t.Fatalf("NFS without git clone: expected 1 init container (mkdir+chown must still run), got %d",
			len(pod.Spec.InitContainers))
	}

	ic := pod.Spec.InitContainers[0]
	assert.Equal(t, []string{"sciontool", "provision", "--uid", "1000", "--gid", "1000"}, ic.Command,
		"non-git init container should run plain provision with ownership flags only, no clone flags")
	var hasProjectID bool
	for _, env := range ic.Env {
		if env.Name == "SCION_CLONE_URL" || env.Name == "SCION_CLONE_BRANCH" {
			t.Errorf("non-git init container should not have clone env var %s", env.Name)
		}
		if env.Name == "SCION_PROJECT_ID" && env.Value == "proj-123" {
			hasProjectID = true
		}
	}
	assert.True(t, hasProjectID, "non-git init container missing SCION_PROJECT_ID env var")
}

// TestBuildPod_NFSBackend_InitContainer_SecurityContext_Winner verifies the
// F-111 capability/root fix: the lock-winner (or no-locker) init container —
// the one that actually chowns — must run as root with exactly CHOWN,
// FOWNER, DAC_OVERRIDE added on top of Drop:ALL. All three are in GKE
// Autopilot's default allowed capability set, as is running a container as
// root (see the comment on this security context in k8s_runtime.go for the
// source cited).
func TestBuildPod_NFSBackend_InitContainer_SecurityContext_Winner(t *testing.T) {
	r := newNFSTestK8sRuntime()
	config := RunConfig{
		Name:                 "test-nfs-secctx-winner",
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
	if len(pod.Spec.InitContainers) != 1 {
		t.Fatalf("expected 1 init container, got %d", len(pod.Spec.InitContainers))
	}

	sc := pod.Spec.InitContainers[0].SecurityContext
	if sc == nil {
		t.Fatal("init container has no SecurityContext")
	}
	if sc.RunAsUser == nil || *sc.RunAsUser != 0 {
		t.Errorf("winner RunAsUser = %v, want 0", sc.RunAsUser)
	}
	if sc.RunAsGroup == nil || *sc.RunAsGroup != 0 {
		t.Errorf("winner RunAsGroup = %v, want 0", sc.RunAsGroup)
	}
	if sc.RunAsNonRoot == nil || *sc.RunAsNonRoot != false {
		t.Errorf("winner RunAsNonRoot = %v, want false (override the pod-level true)", sc.RunAsNonRoot)
	}
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation != false {
		t.Errorf("winner AllowPrivilegeEscalation = %v, want false", sc.AllowPrivilegeEscalation)
	}
	assert.ElementsMatch(t, []corev1.Capability{"CHOWN", "FOWNER", "DAC_OVERRIDE"}, sc.Capabilities.Add,
		"winner must add exactly CHOWN, FOWNER, DAC_OVERRIDE")
	assert.Equal(t, []corev1.Capability{"ALL"}, sc.Capabilities.Drop, "winner must still drop ALL first")
}

// TestBuildPod_NFSBackend_InitContainer_SecurityContext_Loser verifies the
// wait-for-sentinel (lock-loser) init container keeps the minimal, fully-
// dropped, non-root default — it only os.Stats a file, so it never needs
// root or any added capability, unlike the winner above.
func TestBuildPod_NFSBackend_InitContainer_SecurityContext_Loser(t *testing.T) {
	r := newNFSTestK8sRuntime()
	config := nfsBaseConfig("test-nfs-secctx-loser")
	config.nfsProvisionLockLost = true

	pod, err := r.buildPod("default", config)
	if err != nil {
		t.Fatalf("buildPod failed: %v", err)
	}
	if len(pod.Spec.InitContainers) != 1 {
		t.Fatalf("expected 1 init container, got %d", len(pod.Spec.InitContainers))
	}

	sc := pod.Spec.InitContainers[0].SecurityContext
	if sc == nil {
		t.Fatal("init container has no SecurityContext")
	}
	if sc.RunAsUser != nil {
		t.Errorf("loser RunAsUser = %v, want nil (inherit pod-level uid 1000)", sc.RunAsUser)
	}
	if sc.RunAsNonRoot != nil {
		t.Errorf("loser RunAsNonRoot = %v, want nil (inherit pod-level true)", sc.RunAsNonRoot)
	}
	if len(sc.Capabilities.Add) != 0 {
		t.Errorf("loser Capabilities.Add = %v, want none", sc.Capabilities.Add)
	}
	assert.Equal(t, []corev1.Capability{"ALL"}, sc.Capabilities.Drop, "loser must still drop ALL")
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
		Depth:  intPtr(1),
	}

	cmd := nfsProvisionCommand(gc, 0, 0)

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

// TestNFSInitContainerInjected pins the F-111 gate: nfs backend + a bound PV
// claim, independent of git config. This is the single source of truth
// shared by the advisory-lock section, the init-container construction, and
// the workspace-sync skip logic in Run() — a regression in any one of those
// call sites would show up here first.
func TestNFSInitContainerInjected(t *testing.T) {
	tests := []struct {
		name        string
		backendName string
		pvClaimName string
		want        bool
	}{
		{name: "nfs with bound claim", backendName: "nfs", pvClaimName: "scion-workspaces", want: true},
		{name: "nfs without a claim", backendName: "nfs", pvClaimName: "", want: false},
		{name: "local backend", backendName: "local", pvClaimName: "scion-workspaces", want: false},
		{name: "empty backend", backendName: "", pvClaimName: "scion-workspaces", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := RunConfig{WorkspaceBackendName: tt.backendName, NFSPVClaimName: tt.pvClaimName}
			if got := nfsInitContainerInjected(config); got != tt.want {
				t.Errorf("nfsInitContainerInjected() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNFSProvisionCommand_NilConfig(t *testing.T) {
	cmd := nfsProvisionCommand(nil, 0, 0)
	assert.Equal(t, []string{"sciontool", "provision"}, cmd)
}

// TestNFSProvisionCommand_UIDGID verifies the helper passes the given
// uid/gid to `sciontool provision` independently, with and without a git
// clone config, and that zero values leave the flags off so sciontool's 1000
// default applies. Which uid/gid buildPod passes is covered by
// TestBuildPod_NFSInitOwnershipMatchesSecurityContext.
func TestNFSProvisionCommand_UIDGID(t *testing.T) {
	gc := &api.GitCloneConfig{
		URL:   "https://github.com/example/repo.git",
		Depth: intPtr(1),
	}

	tests := []struct {
		name string
		gc   *api.GitCloneConfig
		uid  int64
		gid  int64
		want []string
	}{
		{
			name: "git clone with uid and gid",
			gc:   gc,
			uid:  997,
			gid:  1003,
			want: []string{"sciontool", "provision", "--depth", "1", "--uid", "997", "--gid", "1003"},
		},
		{
			name: "no git config with uid and gid",
			gc:   nil,
			uid:  997,
			gid:  1003,
			want: []string{"sciontool", "provision", "--uid", "997", "--gid", "1003"},
		},
		{
			name: "only gid set",
			gc:   nil,
			gid:  1003,
			want: []string{"sciontool", "provision", "--gid", "1003"},
		},
		{
			name: "only uid set",
			gc:   nil,
			uid:  1000,
			want: []string{"sciontool", "provision", "--uid", "1000"},
		},
		{
			name: "unset with git clone",
			gc:   gc,
			want: []string{"sciontool", "provision", "--depth", "1"},
		},
		{
			name: "unset without git config",
			gc:   nil,
			want: []string{"sciontool", "provision"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, nfsProvisionCommand(tt.gc, tt.uid, tt.gid))
		})
	}
}

func TestNFSProvisionCommand_DefaultDepth(t *testing.T) {
	gc := &api.GitCloneConfig{
		URL: "https://github.com/example/repo.git",
		// Depth nil → no --depth flag; CLI defaults to shallow (depth 1)
	}

	cmd := nfsProvisionCommand(gc, 0, 0)

	assert.Equal(t, []string{"sciontool", "provision"}, cmd)
}

func TestNFSProvisionCommand_FullClone(t *testing.T) {
	gc := &api.GitCloneConfig{
		URL:   "https://github.com/example/repo.git",
		Depth: intPtr(0),
	}

	cmd := nfsProvisionCommand(gc, 0, 0)

	assert.Equal(t, []string{"sciontool", "provision", "--depth", "0"}, cmd)
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

	cmd := nfsProvisionCommand(gc, 0, 0)

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
			Depth:  intPtr(1),
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

// TestWaitForPodReady_NamesFailedInitContainer is the F-111 review fix
// (tf-lead): waitForPodReady used to check only the main container's
// status, so a failed init container (most notably workspace-provision,
// whose entire job is now a fatal chown — RequireChownSuccess, F-111) was
// invisible here — it just sat as PodInitializing until the full 10-minute
// timeout fired with a generic, unhelpful error. It must now name the
// failed init container and its actual exit reason immediately.
func TestWaitForPodReady_NamesFailedInitContainer(t *testing.T) {
	r := newNFSTestK8sRuntime()
	namespace := "default"
	podName := "test-init-fail"

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: namespace},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			InitContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "workspace-provision",
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							ExitCode: 1,
							Reason:   "Error",
							Message:  "provision failed: ProvisionShared: chown /workspace to 1000:1000: operation not permitted",
						},
					},
				},
			},
		},
	}
	if _, err := r.Client.Clientset.CoreV1().Pods(namespace).Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to create test pod: %v", err)
	}

	err := r.waitForPodReady(context.Background(), namespace, podName)
	if err == nil {
		t.Fatal("expected waitForPodReady to fail when an init container has terminated with a non-zero exit code")
	}
	if !strings.Contains(err.Error(), "workspace-provision") {
		t.Errorf("error should name the failed init container, got: %v", err)
	}
	if !strings.Contains(err.Error(), "operation not permitted") {
		t.Errorf("error should include the init container's failure message, got: %v", err)
	}
}

// TestWaitForPodReady_InitContainerTerminalWaitingReasons is the GCP#2027
// review fix: the init-container loop in waitForPodReady only recognized
// Terminated (non-zero exit) and Waiting=CrashLoopBackOff. It missed
// ImagePullBackOff, ErrImagePull, InvalidImageName, and
// CreateContainerConfigError — reasons the main-container block already
// failed fast on — so an init container stuck on a bad image (our
// provisioning init container uses the agent image) hung until the full
// 10-minute cap. classifyTerminalWaitingReason is now shared by both
// blocks so they can't drift; this proves the init-container side returns
// promptly and names the failing container for all five terminal reasons.
func TestWaitForPodReady_InitContainerTerminalWaitingReasons(t *testing.T) {
	reasons := []string{
		"ImagePullBackOff",
		"ErrImagePull",
		"InvalidImageName",
		"CreateContainerConfigError",
		"CrashLoopBackOff",
	}
	for _, reason := range reasons {
		t.Run(reason, func(t *testing.T) {
			r := newNFSTestK8sRuntime()
			namespace := "default"
			podName := "test-init-" + strings.ToLower(reason)

			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: namespace},
				Status: corev1.PodStatus{
					Phase: corev1.PodPending,
					InitContainerStatuses: []corev1.ContainerStatus{
						{
							Name: "workspace-provision",
							State: corev1.ContainerState{
								Waiting: &corev1.ContainerStateWaiting{
									Reason:  reason,
									Message: "test message for " + reason,
								},
							},
						},
					},
				},
			}
			if _, err := r.Client.Clientset.CoreV1().Pods(namespace).Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
				t.Fatalf("failed to create test pod: %v", err)
			}

			// Bounded timeout: if the fix regresses, waitForPodReady will
			// hang until this deadline instead of returning promptly.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			err := r.waitForPodReady(ctx, namespace, podName)
			if err == nil {
				t.Fatalf("expected waitForPodReady to fail promptly for init container reason %s", reason)
			}
			if errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("waitForPodReady should classify the terminal reason %s promptly instead of waiting for the timeout, got: %v", reason, err)
			}
			if !strings.Contains(err.Error(), "workspace-provision") {
				t.Errorf("error should name the failing init container, got: %v", err)
			}
		})
	}
}

// TestWaitForPodReady_InitContainerNonTerminalWaitingReasonKeepsWaiting
// confirms that a non-terminal init-container waiting reason
// (PodInitializing, the normal state while an init container runs) does not
// trip the new classification and does not return early — it keeps waiting
// until the caller's context is done.
func TestWaitForPodReady_InitContainerNonTerminalWaitingReasonKeepsWaiting(t *testing.T) {
	r := newNFSTestK8sRuntime()
	namespace := "default"
	podName := "test-init-podinitializing"

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: namespace},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			InitContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "workspace-provision",
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{
							Reason: "PodInitializing",
						},
					},
				},
			},
		},
	}
	if _, err := r.Client.Clientset.CoreV1().Pods(namespace).Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to create test pod: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := r.waitForPodReady(ctx, namespace, podName)
	if err == nil {
		t.Fatal("expected waitForPodReady to time out while the init container is still PodInitializing, not succeed")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected a context-deadline timeout while init container is PodInitializing, got: %v", err)
	}
}

// TestWaitForPodReady_MainContainerTerminalWaitingReasons pins the exact
// error text for the main container's terminal waiting reasons (including
// the kubernetes.imagePullPolicy hint) so that sharing
// classifyTerminalWaitingReason with the init-container block above cannot
// silently change these user-facing messages.
func TestWaitForPodReady_MainContainerTerminalWaitingReasons(t *testing.T) {
	cases := []struct {
		reason  string
		message string
		wantErr string
	}{
		{
			reason:  "ImagePullBackOff",
			message: "rpc error: image not found",
			wantErr: `image pull failed for pod "test-main-imagepullbackoff": rpc error: image not found — verify the image name and registry access (image pull policy: check kubernetes.imagePullPolicy)`,
		},
		{
			reason:  "ErrImagePull",
			message: "manifest unknown",
			wantErr: `image pull failed for pod "test-main-errimagepull": manifest unknown — verify the image name and registry access (image pull policy: check kubernetes.imagePullPolicy)`,
		},
		{
			reason:  "InvalidImageName",
			message: "invalid reference format",
			wantErr: `invalid image name for pod "test-main-invalidimagename": invalid reference format`,
		},
		{
			reason:  "CreateContainerConfigError",
			message: `secret "foo" not found`,
			wantErr: `container configuration error for pod "test-main-createcontainerconfigerror": secret "foo" not found — check secret references and volume mounts`,
		},
		{
			reason:  "CrashLoopBackOff",
			message: "back-off restarting failed container",
			wantErr: `container is crash-looping in pod "test-main-crashloopbackoff": back-off restarting failed container — check container logs with 'scion logs'`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.reason, func(t *testing.T) {
			r := newNFSTestK8sRuntime()
			namespace := "default"
			podName := "test-main-" + strings.ToLower(tc.reason)

			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: namespace},
				Status: corev1.PodStatus{
					Phase: corev1.PodPending,
					ContainerStatuses: []corev1.ContainerStatus{
						{
							Name: agentContainerName,
							State: corev1.ContainerState{
								Waiting: &corev1.ContainerStateWaiting{
									Reason:  tc.reason,
									Message: tc.message,
								},
							},
						},
					},
				},
			}
			if _, err := r.Client.Clientset.CoreV1().Pods(namespace).Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
				t.Fatalf("failed to create test pod: %v", err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			err := r.waitForPodReady(ctx, namespace, podName)
			if err == nil {
				t.Fatalf("expected waitForPodReady to fail for main container reason %s", tc.reason)
			}
			if err.Error() != tc.wantErr {
				t.Errorf("unexpected error text for %s:\n got:  %s\n want: %s", tc.reason, err.Error(), tc.wantErr)
			}
		})
	}
}

func TestRun_NFSLockLost_CreatesWaitPod(t *testing.T) {
	// When the lock is held by another node, the pod should have a
	// wait-for-sentinel init container, not a cloning one.
	r := newNFSTestK8sRuntime()
	config := nfsBaseConfig("scion-test-lock-lost")
	config.Locker = &alwaysLoseLocker{}

	// Run() creates the pod, then fails readiness at its first poll (see
	// failPodReadiness), which keeps the pod for inspection.
	failPodReadiness(r.Client.Clientset.(*k8sfake.Clientset))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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

	// Run() creates the pod, then fails readiness at its first poll (see
	// failPodReadiness), which keeps the pod for inspection.
	failPodReadiness(r.Client.Clientset.(*k8sfake.Clientset))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
	// F-111 (design §9): the guard used to key on WorkspaceBackendName alone,
	// so it claimed "pre-populated by init container" even in configurations
	// where no init container was ever injected (e.g. nfs backend selected
	// but no PV claim bound yet). It must agree with nfsInitContainerInjected
	// — the exact condition buildPod uses to decide whether to add the init
	// container — not a hand-copied guess of it.
	tests := []struct {
		name            string
		workspace       string
		backendName     string
		pvClaimName     string
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
			name:            "NFS backend with bound PV claim skips workspace sync",
			workspace:       "/some/path",
			backendName:     "nfs",
			pvClaimName:     "scion-workspaces",
			wantWorkspaceCP: false,
		},
		{
			name:            "NFS backend without a bound PV claim still syncs (no init container was injected)",
			workspace:       "/some/path",
			backendName:     "nfs",
			pvClaimName:     "",
			wantWorkspaceCP: true,
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
				NFSPVClaimName:       tt.pvClaimName,
			}
			// The real guard condition from Run(), not a hand-copied replica.
			shouldSync := config.Workspace != "" && (config.WorkspaceBackendName != "nfs" || !nfsInitContainerInjected(config))
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
			"scion.project": "my-project",
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
			"scion.project": "my-project",
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

// TestBuildPod_NFSBackend_InitContainer_MountsSharedDirs verifies F-111's
// shared-dir fix (tf-lead's addendum): a shared dir mounted from the
// workspace NFS PVC by subPath is its own, separate container-level volume
// mount — the workspace mount alone doesn't give the init container
// filesystem access to it, so a workspace-only chown would never reach it
// (matching vm-deploy's live evidence: projects/<pid>/shared-dirs/scratchpad
// was also left root:root). The init container must get the same mount(s)
// the main container gets, plus an env var telling `sciontool provision`
// which paths to also chown.
func TestBuildPod_NFSBackend_InitContainer_MountsSharedDirs(t *testing.T) {
	r := newNFSTestK8sRuntime()
	config := RunConfig{
		Name:                 "test-nfs-shared-init",
		Image:                "test-image",
		UnixUsername:         "scion",
		WorkspaceBackendName: "nfs",
		NFSPVClaimName:       "scion-workspaces",
		NFSSubPath:           "projects/proj-123/workspace",
		SharedDirs: []api.SharedDir{
			{Name: "scratchpad"},
			{Name: "logs", ReadOnly: true},
		},
	}

	pod, err := r.buildPod("default", config)
	if err != nil {
		t.Fatalf("buildPod failed: %v", err)
	}
	if len(pod.Spec.InitContainers) != 1 {
		t.Fatalf("expected 1 init container, got %d", len(pod.Spec.InitContainers))
	}
	ic := pod.Spec.InitContainers[0]

	sd0 := findVolumeMount(&corev1.Container{VolumeMounts: ic.VolumeMounts}, "shared-dir-0")
	sd1 := findVolumeMount(&corev1.Container{VolumeMounts: ic.VolumeMounts}, "shared-dir-1")
	if sd0 == nil || sd1 == nil {
		t.Fatalf("init container missing shared-dir volume mounts, got %+v", ic.VolumeMounts)
	}
	if sd0.MountPath != "/scion-volumes/scratchpad" {
		t.Errorf("shared-dir-0 init mountPath = %q, want /scion-volumes/scratchpad", sd0.MountPath)
	}
	if sd0.SubPath != "projects/proj-123/shared-dirs/scratchpad" {
		t.Errorf("shared-dir-0 init subPath = %q, want projects/proj-123/shared-dirs/scratchpad", sd0.SubPath)
	}
	if sd1.MountPath != "/scion-volumes/logs" {
		t.Errorf("shared-dir-1 init mountPath = %q, want /scion-volumes/logs", sd1.MountPath)
	}

	var sharedPaths string
	for _, env := range ic.Env {
		if env.Name == "SCION_SHARED_DIR_PATHS" {
			sharedPaths = env.Value
		}
	}
	// F-111 review (tf-lead nit): keyed explicitly by name=path pairs, not a
	// bare path list — see nfsSharedDirMount and the CLI parser's comment.
	assert.Equal(t, "scratchpad=/scion-volumes/scratchpad,logs=/scion-volumes/logs", sharedPaths,
		"SCION_SHARED_DIR_PATHS must list both shared-dir name=mountPath pairs")

	// The volume names the init container's mounts reference must actually
	// exist in pod.Spec.Volumes (created by the later, unrelated main-loop
	// pass over config.SharedDirs) — a mount referencing a nonexistent
	// volume is a broken pod spec the API server would reject.
	if findVolume(pod, "shared-dir-0") == nil || findVolume(pod, "shared-dir-1") == nil {
		t.Fatal("init container mounts reference volumes that don't exist in pod.Spec.Volumes")
	}
}

// TestBuildPod_NFSBackend_InitAndMainSharedDirMounts_MatchExactly is a
// drift-prevention test (F-111 review, tf-lead nit): nfsSharedDirInitMounts
// re-derives the same volume-name/mountPath/subPath computation the main
// container's shared-dir loop makes independently, later in the same
// buildPod call. Rather than extracting a shared helper both call sites use
// (a larger, riskier change to the existing, already-covered main loop for a
// nit-level concern), this test pins byte-for-byte parity between the two:
// if either one's computation ever drifts from the other, this fails
// immediately instead of silently producing an init container that chowns
// the wrong path. Covers both the default target (/scion-volumes/<name>) and
// the InWorkspace nested target (<workspace>/.scion-volumes/<name>).
func TestBuildPod_NFSBackend_InitAndMainSharedDirMounts_MatchExactly(t *testing.T) {
	r := newNFSTestK8sRuntime()
	config := RunConfig{
		Name:                 "test-nfs-shared-drift",
		Image:                "test-image",
		UnixUsername:         "scion",
		WorkspaceBackendName: "nfs",
		NFSPVClaimName:       "scion-workspaces",
		NFSSubPath:           "projects/proj-123/workspace",
		SharedDirs: []api.SharedDir{
			{Name: "scratchpad"},
			{Name: "logs", ReadOnly: true},
			{Name: "nested-dir", InWorkspace: true},
		},
	}

	pod, err := r.buildPod("default", config)
	if err != nil {
		t.Fatalf("buildPod failed: %v", err)
	}
	if len(pod.Spec.InitContainers) != 1 {
		t.Fatalf("expected 1 init container, got %d", len(pod.Spec.InitContainers))
	}
	ic := pod.Spec.InitContainers[0]

	for i := range config.SharedDirs {
		volName := fmt.Sprintf("shared-dir-%d", i)
		initMount := findVolumeMount(&corev1.Container{VolumeMounts: ic.VolumeMounts}, volName)
		mainMount := findVolumeMount(&pod.Spec.Containers[0], volName)
		if initMount == nil {
			t.Fatalf("%s: no init-container mount found", volName)
		}
		if mainMount == nil {
			t.Fatalf("%s: no main-container mount found", volName)
		}
		if initMount.MountPath != mainMount.MountPath {
			t.Errorf("%s: init MountPath = %q, main MountPath = %q (drift)", volName, initMount.MountPath, mainMount.MountPath)
		}
		if initMount.SubPath != mainMount.SubPath {
			t.Errorf("%s: init SubPath = %q, main SubPath = %q (drift)", volName, initMount.SubPath, mainMount.SubPath)
		}
	}
}

// TestBuildPod_NFSBackend_RejectsTraversalSharedDirName is the F-111 review
// fix (tf-lead/tf-review-nfsfix, BLOCKING), exercised end to end through
// buildPod: a shared-dir name that would escape projects/<pid>/shared-dirs/
// once joined must fail the whole pod build — for both the shared-dir-only
// case (init container construction, which iterates config.SharedDirs
// first) and the case where the escaping entry sits alongside a valid one
// (the main-container loop, which processes them in order and must also
// reject once it reaches the bad one, not just skip it).
func TestBuildPod_NFSBackend_RejectsTraversalSharedDirName(t *testing.T) {
	badNames := []string{
		"../../../projects",
		"../../victim/shared-dirs/scratchpad",
		"/etc/passwd",
		".",
	}
	for _, name := range badNames {
		t.Run(name, func(t *testing.T) {
			r := newNFSTestK8sRuntime()
			config := RunConfig{
				Name:                 "test-nfs-shared-traversal",
				Image:                "test-image",
				UnixUsername:         "scion",
				WorkspaceBackendName: "nfs",
				NFSPVClaimName:       "scion-workspaces",
				NFSSubPath:           "projects/proj-123/workspace",
				SharedDirs: []api.SharedDir{
					{Name: "scratchpad"}, // valid, present alongside the bad one
					{Name: name},
				},
			}

			pod, err := r.buildPod("default", config)
			if err == nil {
				t.Fatalf("buildPod should have rejected shared-dir name %q, got a pod: %+v", name, pod)
			}
		})
	}
}

// TestBuildPod_NFSBackend_InitContainer_NoSharedDirMounts_WhenSharedDirStorageNFS
// confirms the scope boundary: server.shared_dir_storage's own NFS mechanism
// (a separate subsystem from workspace_storage:nfs) is not touched by this
// fix — the init container gets only the workspace mount.
func TestBuildPod_NFSBackend_InitContainer_NoSharedDirMounts_WhenSharedDirStorageNFS(t *testing.T) {
	r := newNFSTestK8sRuntime()
	config := RunConfig{
		Name:                 "test-nfs-shared-storage-nfs",
		Image:                "test-image",
		UnixUsername:         "scion",
		WorkspaceBackendName: "nfs",
		NFSPVClaimName:       "scion-workspaces",
		NFSSubPath:           "projects/proj-123/workspace",
		SharedDirs: []api.SharedDir{
			{Name: "scratchpad"},
		},
		SharedDirStorage: &SharedDirRealization{
			Backend:     "nfs",
			PVClaimName: "scion-shared-volumes",
			SubPaths:    map[string]string{"scratchpad": "projects/proj-123/shared-dirs/scratchpad"},
		},
	}

	pod, err := r.buildPod("default", config)
	if err != nil {
		t.Fatalf("buildPod failed: %v", err)
	}
	if len(pod.Spec.InitContainers) != 1 {
		t.Fatalf("expected 1 init container, got %d", len(pod.Spec.InitContainers))
	}
	ic := pod.Spec.InitContainers[0]
	// The workspace and the provisioning state directory (#2670); no
	// shared-dir mounts.
	if len(ic.VolumeMounts) != 2 || ic.VolumeMounts[1].MountPath != NFSProvisionStateMountPath {
		t.Errorf("expected only the workspace and provisioning state mounts on the init container, got %d: %+v",
			len(ic.VolumeMounts), ic.VolumeMounts)
	}
	for _, env := range ic.Env {
		if env.Name == "SCION_SHARED_DIR_PATHS" {
			t.Errorf("SCION_SHARED_DIR_PATHS should not be set when shared_dir_storage.backend=nfs, got %q", env.Value)
		}
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
			got, err := nfsSharedDirSubPath(tt.workspaceSubPath, tt.sharedDirName)
			if err != nil {
				t.Fatalf("nfsSharedDirSubPath(%q, %q) unexpected error: %v", tt.workspaceSubPath, tt.sharedDirName, err)
			}
			if got != tt.want {
				t.Errorf("nfsSharedDirSubPath(%q, %q) = %q, want %q", tt.workspaceSubPath, tt.sharedDirName, got, tt.want)
			}
		})
	}
}

// TestNFSSharedDirSubPath_RejectsTraversalNames is the F-111 review fix
// (tf-lead/tf-review-nfsfix, BLOCKING): filepath.Join silently collapses
// ".." segments, so an unvalidated shared-dir name can resolve outside
// projects/<pid>/shared-dirs/ entirely — onto another project's real
// workspace, which the winner init container (CHOWN/FOWNER/DAC_OVERRIDE,
// F-111) would then recursively re-own. Defense in depth alongside
// pkg/agent/shared_dir_storage.go's own validation gate.
func TestNFSSharedDirSubPath_RejectsTraversalNames(t *testing.T) {
	badNames := []string{
		"../../../projects",
		"../../victim/shared-dirs/scratchpad",
		"/etc/passwd",
		".",
	}
	for _, name := range badNames {
		t.Run(name, func(t *testing.T) {
			got, err := nfsSharedDirSubPath("projects/proj-123/workspace", name)
			if err == nil {
				t.Fatalf("nfsSharedDirSubPath(%q) = %q, want an error", name, got)
			}
			if got != "" {
				t.Errorf("nfsSharedDirSubPath(%q) returned a non-empty path %q alongside an error", name, got)
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
			"scion.project":    "my-project",
			"scion.project_id": "proj-123",
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
			"scion.project":    "my-project",
			"scion.project_id": "proj-123",
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

	// Verify PVC names and the exact label set — nothing beyond the
	// canonical project labels and the shared-dir marker (no unexpected
	// extra key survives).
	pvcNames := map[string]bool{}
	for _, pvc := range pvcs.Items {
		pvcNames[pvc.Name] = true
		wantDir := "build-cache"
		if strings.Contains(pvc.Name, "logs") {
			wantDir = "logs"
		}
		assert.Equal(t, map[string]string{
			"scion.project":    "my-project",
			"scion.project_id": "proj-123",
			"scion.shared-dir": wantDir,
		}, pvc.Labels)
	}
	if !pvcNames["scion-shared-my-project-build-cache"] {
		t.Error("missing PVC scion-shared-my-project-build-cache")
	}
	if !pvcNames["scion-shared-my-project-logs"] {
		t.Error("missing PVC scion-shared-my-project-logs")
	}
}

// --- Phase 3 guardrail regression: K8s + NFS + worktree-per-agent ---

func TestBuildPod_NFSBackend_WorktreeSubPath_Rejected(t *testing.T) {
	// A workspace subPath that does not end in <projectID>/workspace (here
	// an agent's worktree, a shape worktree-per-agent no longer uses: see
	// NFSWorktreeName) has no project directory to hold the provisioning
	// state directory next to the workspace, so the pod is not built
	// (#2670, fail closed).
	r := newNFSTestK8sRuntime()
	config := RunConfig{
		Name:                 "test-nfs-worktree",
		Image:                "test-image",
		UnixUsername:         "scion",
		WorkspaceBackendName: "nfs",
		NFSPVClaimName:       "scion-workspaces",
		NFSSubPath:           "projects/proj-123/workspace/worktrees/agent-1",
		GitCloneForInit: &api.GitCloneConfig{
			URL:    "https://github.com/org/repo.git",
			Branch: "main",
		},
	}

	_, err := r.buildPod("default", config)
	require.ErrorContains(t, err, "unexpected NFS workspace subPath")
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

// effectiveProvisionID returns the value `sciontool provision` uses for an
// ownership flag: the flag's argument if present, otherwise sciontool's
// default of 1000 (cmd/sciontool/commands/provision.go).
func effectiveProvisionID(t *testing.T, cmd []string, flag string) int64 {
	t.Helper()
	for i, arg := range cmd {
		if arg == flag {
			if i+1 >= len(cmd) {
				t.Fatalf("%s has no value in %v", flag, cmd)
			}
			v, err := strconv.ParseInt(cmd[i+1], 10, 64)
			if err != nil {
				t.Fatalf("%s value %q: %v", flag, cmd[i+1], err)
			}
			return v
		}
	}
	return 1000
}

// TestBuildPod_NFSInitOwnershipMatchesSecurityContext checks that the
// lock-winner init container chowns the workspace to the identity the agent
// container actually runs as: --uid follows the pod RunAsUser and --gid
// follows the pod fsGroup. A configured NFS uid is not applied to the pod
// RunAsUser, so it must not reach the init container either; otherwise the
// cloned tree ends up owned by a uid the agent is not (ptone/scion#2566).
func TestBuildPod_NFSInitOwnershipMatchesSecurityContext(t *testing.T) {
	tests := []struct {
		name    string
		uid     int
		gid     int
		wantUID int64
		wantGID int64
	}{
		{name: "configured uid and gid", uid: 997, gid: 1003, wantUID: 1000, wantGID: 1003},
		{name: "gid only", gid: 1003, wantUID: 1000, wantGID: 1003},
		{name: "uid only", uid: 997, wantUID: 1000, wantGID: 1000},
		{name: "unset", wantUID: 1000, wantGID: 1000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newNFSTestK8sRuntime()
			config := nfsBaseConfig("test-nfs-init-owner")
			config.NFSUID = tt.uid
			config.NFSGID = tt.gid

			pod, err := r.buildPod("default", config)
			if err != nil {
				t.Fatalf("buildPod failed: %v", err)
			}
			if len(pod.Spec.InitContainers) != 1 {
				t.Fatalf("expected 1 init container, got %d", len(pod.Spec.InitContainers))
			}
			sc := pod.Spec.SecurityContext
			if sc == nil || sc.RunAsUser == nil || sc.FSGroup == nil {
				t.Fatal("pod security context, RunAsUser or FSGroup is nil")
			}

			cmd := pod.Spec.InitContainers[0].Command
			assert.True(t, hasFlag(cmd, "--uid"), "init command should pass --uid explicitly: %v", cmd)
			assert.True(t, hasFlag(cmd, "--gid"), "init command should pass --gid explicitly: %v", cmd)
			gotUID := effectiveProvisionID(t, cmd, "--uid")
			gotGID := effectiveProvisionID(t, cmd, "--gid")

			assert.Equal(t, tt.wantUID, *sc.RunAsUser, "pod RunAsUser")
			assert.Equal(t, tt.wantGID, *sc.FSGroup, "pod FSGroup")
			assert.Equal(t, *sc.RunAsUser, gotUID, "init --uid must equal pod RunAsUser (cmd %v)", cmd)
			assert.Equal(t, *sc.FSGroup, gotGID, "init --gid must equal pod FSGroup (cmd %v)", cmd)
		})
	}
}
