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
	corev1 "k8s.io/api/core/v1"
)

// --- Stage 3: Enhanced error messages ---

func TestBuildPod_InvalidImagePullPolicy_ErrorMessage(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()

	config := RunConfig{
		Name:         "test-agent",
		Image:        "test:latest",
		UnixUsername: "scion",
		Kubernetes: &api.KubernetesConfig{
			ImagePullPolicy: "MaybeLater",
		},
	}

	_, err := rt.buildPod("default", config)
	if err == nil {
		t.Fatal("expected error for invalid imagePullPolicy")
	}
	expected := "invalid imagePullPolicy"
	if !strings.Contains(err.Error(), expected) {
		t.Errorf("error message should contain %q, got: %s", expected, err.Error())
	}
	// Should also mention valid options
	if !strings.Contains(err.Error(), "Always") || !strings.Contains(err.Error(), "IfNotPresent") || !strings.Contains(err.Error(), "Never") {
		t.Errorf("error message should list valid options, got: %s", err.Error())
	}
}

func TestBuildPod_InvalidResource_ErrorMessage(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()

	config := RunConfig{
		Name:  "test-agent",
		Image: "test:latest",
		Resources: &api.ResourceSpec{
			Requests: api.ResourceList{CPU: "not-a-number"},
		},
	}

	_, err := rt.buildPod("default", config)
	if err == nil {
		t.Fatal("expected error for invalid CPU value")
	}
	// Error should include field name and value
	if !strings.Contains(err.Error(), "requests.cpu") {
		t.Errorf("error should mention field name 'requests.cpu', got: %s", err.Error())
	}
	if !strings.Contains(err.Error(), "not-a-number") {
		t.Errorf("error should include the invalid value, got: %s", err.Error())
	}
}

// --- Stage 3: Diagnostics interface ---

func TestKubernetesRuntime_ImplementsDiagnosable(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()

	// Verify it implements Diagnosable
	diag, ok := interface{}(rt).(Diagnosable)
	if !ok {
		t.Fatal("KubernetesRuntime should implement Diagnosable")
	}

	report := diag.RunDiagnostics(DiagnosticOpts{})
	if report.Runtime != "kubernetes" {
		t.Errorf("expected runtime 'kubernetes', got %q", report.Runtime)
	}
}

// --- Stage 3: Full config with context ---

func TestBuildPod_FullConfig_Stage3(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()

	config := RunConfig{
		Name:         "stage3-test",
		Image:        "gcr.io/test/image:v2",
		UnixUsername: "scion",
		Harness:      &EnvHarness{},
		Resources: &api.ResourceSpec{
			Requests: api.ResourceList{CPU: "1", Memory: "2Gi"},
			Limits:   api.ResourceList{CPU: "4", Memory: "8Gi"},
			Disk:     "50Gi",
		},
		Kubernetes: &api.KubernetesConfig{
			RuntimeClassName:   "kata",
			ServiceAccountName: "scion-agent-sa",
			ImagePullPolicy:    "IfNotPresent",
			NodeSelector: map[string]string{
				"accelerator": "gpu",
				"pool":        "agents",
			},
			Tolerations: []api.K8sToleration{
				{Key: "gpu", Operator: "Exists", Effect: "NoSchedule"},
			},
			Resources: &api.K8sResources{
				Requests: map[string]string{"nvidia.com/gpu": "1"},
				Limits:   map[string]string{"nvidia.com/gpu": "1"},
			},
		},
	}

	pod, err := rt.buildPod("production", config)
	if err != nil {
		t.Fatalf("buildPod failed: %v", err)
	}

	// Verify all fields
	if pod.Namespace != "production" {
		t.Errorf("expected namespace 'production', got %s", pod.Namespace)
	}
	if pod.Spec.RuntimeClassName == nil || *pod.Spec.RuntimeClassName != "kata" {
		t.Error("expected RuntimeClassName 'kata'")
	}
	if pod.Spec.ServiceAccountName != "scion-agent-sa" {
		t.Error("expected ServiceAccountName 'scion-agent-sa'")
	}
	if len(pod.Spec.NodeSelector) != 2 {
		t.Errorf("expected 2 nodeSelector entries, got %d", len(pod.Spec.NodeSelector))
	}
	if len(pod.Spec.Tolerations) != 1 {
		t.Errorf("expected 1 toleration, got %d", len(pod.Spec.Tolerations))
	}

	// GPU resources
	res := pod.Spec.Containers[0].Resources
	if _, ok := res.Requests["nvidia.com/gpu"]; !ok {
		t.Error("expected GPU request")
	}
	if _, ok := res.Limits["nvidia.com/gpu"]; !ok {
		t.Error("expected GPU limit")
	}
}

func TestBuildPod_DefaultResources_WhenNoneSpecified(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()

	config := RunConfig{
		Name:         "no-resources",
		Image:        "test:latest",
		UnixUsername: "scion",
	}

	pod, err := rt.buildPod("default", config)
	if err != nil {
		t.Fatalf("buildPod failed: %v", err)
	}

	res := pod.Spec.Containers[0].Resources

	// Should have default CPU request
	cpuReq, ok := res.Requests[corev1.ResourceCPU]
	if !ok {
		t.Fatal("expected default CPU request to be set")
	}
	if cpuReq.String() != "250m" {
		t.Errorf("expected CPU request '250m', got %q", cpuReq.String())
	}

	// Should have default memory request
	memReq, ok := res.Requests[corev1.ResourceMemory]
	if !ok {
		t.Fatal("expected default memory request to be set")
	}
	if memReq.String() != "512Mi" {
		t.Errorf("expected memory request '512Mi', got %q", memReq.String())
	}

	// Only requests are defaulted: no CPU, memory or ephemeral-storage limit.
	if len(res.Limits) != 0 {
		t.Errorf("expected no default limits, got %v", res.Limits)
	}

	// Should have default ephemeral storage
	diskReq, ok := res.Requests[corev1.ResourceEphemeralStorage]
	if !ok {
		t.Fatal("expected default ephemeral storage request to be set")
	}
	if diskReq.String() != "10Gi" {
		t.Errorf("expected ephemeral storage '10Gi', got %q", diskReq.String())
	}
}

func TestBuildPod_ExplicitResources_OverrideDefaults(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()

	config := RunConfig{
		Name:         "custom-resources",
		Image:        "test:latest",
		UnixUsername: "scion",
		Resources: &api.ResourceSpec{
			Requests: api.ResourceList{CPU: "500m", Memory: "1Gi"},
			Limits:   api.ResourceList{CPU: "4", Memory: "8Gi"},
		},
	}

	pod, err := rt.buildPod("default", config)
	if err != nil {
		t.Fatalf("buildPod failed: %v", err)
	}

	res := pod.Spec.Containers[0].Resources

	cpuReq := res.Requests[corev1.ResourceCPU]
	if cpuReq.String() != "500m" {
		t.Errorf("expected CPU request '500m', got %q", cpuReq.String())
	}

	memLim := res.Limits[corev1.ResourceMemory]
	if memLim.String() != "8Gi" {
		t.Errorf("expected memory limit '8Gi', got %q", memLim.String())
	}
}
