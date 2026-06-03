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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/k8s"
	corev1 "k8s.io/api/core/v1"
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

	// Command should reference the git URL and contain sentinel check
	if len(ic.Command) < 3 {
		t.Fatalf("init container command too short: %v", ic.Command)
	}
	script := ic.Command[2] // sh -c <script>
	if !contains(script, ".scion-provisioned") {
		t.Errorf("init script does not reference sentinel file .scion-provisioned")
	}
	if !contains(script, "https://github.com/example/repo.git") {
		t.Errorf("init script does not contain git clone URL")
	}
	if !contains(script, "--branch 'main'") {
		t.Errorf("init script does not contain branch flag")
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

func TestNFSInitProvisionScript_SentinelCheck(t *testing.T) {
	gc := &api.GitCloneConfig{
		URL:    "https://github.com/example/repo.git",
		Branch: "main",
		Depth:  1,
	}

	script := nfsInitProvisionScript(gc)

	// Must check sentinel before cloning
	if !contains(script, ".scion-provisioned") {
		t.Error("script missing sentinel check")
	}

	// Must contain git clone with the URL
	if !contains(script, "git") && !contains(script, "clone") {
		t.Error("script missing git clone command")
	}
	if !contains(script, gc.URL) {
		t.Errorf("script missing clone URL %q", gc.URL)
	}

	// Must write sentinel after successful clone
	if !contains(script, "provisioned_at=") {
		t.Error("script does not write provisioning timestamp to sentinel")
	}
}

func TestNFSInitProvisionScript_NilConfig(t *testing.T) {
	script := nfsInitProvisionScript(nil)
	if !contains(script, "skipping") {
		t.Error("nil config: expected skip message")
	}
}

func TestNFSInitProvisionScript_FullClone(t *testing.T) {
	gc := &api.GitCloneConfig{
		URL:   "https://github.com/example/repo.git",
		Depth: -1, // full clone (depth < 0 means no --depth flag)
	}

	script := nfsInitProvisionScript(gc)

	// With depth -1, should not include --depth flag
	if contains(script, "--depth") {
		t.Error("full clone (depth=-1): should not include --depth flag")
	}
}

func contains(s, substr string) bool {
	return len(s) > 0 && len(substr) > 0 && containsSubstring(s, substr)
}

func containsSubstring(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
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
