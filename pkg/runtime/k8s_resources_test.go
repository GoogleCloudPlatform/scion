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
	corev1 "k8s.io/api/core/v1"
)

// buildPodResources builds a pod for spec/k8s and returns its container resources.
func buildPodResources(t *testing.T, spec *api.ResourceSpec, k8s *api.K8sResources) corev1.ResourceRequirements {
	t.Helper()
	rt, _, _ := newTestK8sRuntime()
	cfg := RunConfig{
		Name:         "res-agent",
		Image:        "test:latest",
		UnixUsername: "scion",
		Resources:    spec,
	}
	if k8s != nil {
		cfg.Kubernetes = &api.KubernetesConfig{Resources: k8s}
	}
	pod, err := rt.buildPod("default", cfg)
	if err != nil {
		t.Fatalf("buildPod failed: %v", err)
	}
	return pod.Spec.Containers[0].Resources
}

func assertQuantity(t *testing.T, list corev1.ResourceList, name corev1.ResourceName, want, kind string) {
	t.Helper()
	q, ok := list[name]
	if !ok {
		t.Errorf("expected %s %s=%s, not set", kind, name, want)
		return
	}
	if q.String() != want {
		t.Errorf("expected %s %s=%s, got %s", kind, name, want, q.String())
	}
}

func assertAbsent(t *testing.T, list corev1.ResourceList, name corev1.ResourceName, kind string) {
	t.Helper()
	if q, ok := list[name]; ok {
		t.Errorf("expected no %s %s, got %s", kind, name, q.String())
	}
}

// The resolved spec normally carries only the built-in limits.cpu default. The
// pod must still get the Kubernetes request defaults for the other fields.
func TestK8sResources_OnlyCPULimit_GetsRequestDefaults(t *testing.T) {
	res := buildPodResources(t, &api.ResourceSpec{Limits: api.ResourceList{CPU: "2"}}, nil)

	assertQuantity(t, res.Requests, corev1.ResourceCPU, "250m", "request")
	assertQuantity(t, res.Requests, corev1.ResourceMemory, "512Mi", "request")
	assertQuantity(t, res.Requests, corev1.ResourceEphemeralStorage, "10Gi", "request")
	assertQuantity(t, res.Limits, corev1.ResourceCPU, "2", "limit")
}

// Defaults are requests only: no memory or ephemeral-storage limit may appear
// unless one is set explicitly.
func TestK8sResources_OnlyCPULimit_NoDefaultMemoryOrDiskLimit(t *testing.T) {
	res := buildPodResources(t, &api.ResourceSpec{Limits: api.ResourceList{CPU: "2"}}, nil)

	assertAbsent(t, res.Limits, corev1.ResourceMemory, "limit")
	assertAbsent(t, res.Limits, corev1.ResourceEphemeralStorage, "limit")
	if len(res.Limits) != 1 {
		t.Errorf("expected only the cpu limit, got %v", res.Limits)
	}
}

// An explicit disk (e.g. from a profile) maps to ephemeral-storage request and
// limit; the remaining request defaults are still filled in.
func TestK8sResources_DiskSet_EphemeralStorageRequestAndLimit(t *testing.T) {
	res := buildPodResources(t, &api.ResourceSpec{
		Limits: api.ResourceList{CPU: "2"},
		Disk:   "40Gi",
	}, nil)

	assertQuantity(t, res.Requests, corev1.ResourceEphemeralStorage, "40Gi", "request")
	assertQuantity(t, res.Limits, corev1.ResourceEphemeralStorage, "40Gi", "limit")
	assertQuantity(t, res.Requests, corev1.ResourceCPU, "250m", "request")
	assertQuantity(t, res.Requests, corev1.ResourceMemory, "512Mi", "request")
	assertAbsent(t, res.Limits, corev1.ResourceMemory, "limit")
}

func TestK8sResources_FullySpecified_Unchanged(t *testing.T) {
	res := buildPodResources(t, &api.ResourceSpec{
		Requests: api.ResourceList{CPU: "1", Memory: "2Gi"},
		Limits:   api.ResourceList{CPU: "4", Memory: "16Gi"},
		Disk:     "50Gi",
	}, nil)

	assertQuantity(t, res.Requests, corev1.ResourceCPU, "1", "request")
	assertQuantity(t, res.Requests, corev1.ResourceMemory, "2Gi", "request")
	assertQuantity(t, res.Requests, corev1.ResourceEphemeralStorage, "50Gi", "request")
	assertQuantity(t, res.Limits, corev1.ResourceCPU, "4", "limit")
	assertQuantity(t, res.Limits, corev1.ResourceMemory, "16Gi", "limit")
	assertQuantity(t, res.Limits, corev1.ResourceEphemeralStorage, "50Gi", "limit")
	if len(res.Requests) != 3 || len(res.Limits) != 3 {
		t.Errorf("expected exactly 3 requests and 3 limits, got %v / %v", res.Requests, res.Limits)
	}
}

// An explicit kubernetes.resources map wins over both defaults and the common spec.
func TestK8sResources_KubernetesMapWins(t *testing.T) {
	res := buildPodResources(t,
		&api.ResourceSpec{Limits: api.ResourceList{CPU: "2"}, Disk: "20Gi"},
		&api.K8sResources{
			Requests: map[string]string{"memory": "3Gi", "ephemeral-storage": "30Gi", "nvidia.com/gpu": "1"},
			Limits:   map[string]string{"memory": "6Gi", "ephemeral-storage": "35Gi", "nvidia.com/gpu": "1"},
		})

	assertQuantity(t, res.Requests, corev1.ResourceMemory, "3Gi", "request")
	assertQuantity(t, res.Limits, corev1.ResourceMemory, "6Gi", "limit")
	assertQuantity(t, res.Requests, corev1.ResourceEphemeralStorage, "30Gi", "request")
	assertQuantity(t, res.Limits, corev1.ResourceEphemeralStorage, "35Gi", "limit")
	assertQuantity(t, res.Requests, "nvidia.com/gpu", "1", "request")
	assertQuantity(t, res.Requests, corev1.ResourceCPU, "250m", "request")
}

// A defaulted request larger than an explicit limit is lowered to the limit so
// the pod stays valid. Explicit values are never changed.
func TestK8sResources_DefaultedRequestClampedToExplicitLimit(t *testing.T) {
	res := buildPodResources(t, &api.ResourceSpec{
		Limits: api.ResourceList{CPU: "100m", Memory: "256Mi"},
	}, &api.K8sResources{
		Limits: map[string]string{"ephemeral-storage": "5Gi"},
	})

	assertQuantity(t, res.Requests, corev1.ResourceCPU, "100m", "request")
	assertQuantity(t, res.Requests, corev1.ResourceMemory, "256Mi", "request")
	assertQuantity(t, res.Requests, corev1.ResourceEphemeralStorage, "5Gi", "request")
	assertQuantity(t, res.Limits, corev1.ResourceCPU, "100m", "limit")
	assertQuantity(t, res.Limits, corev1.ResourceMemory, "256Mi", "limit")
	assertQuantity(t, res.Limits, corev1.ResourceEphemeralStorage, "5Gi", "limit")
}

// A defaulted request at or below an explicit limit is left alone.
func TestK8sResources_DefaultedRequestBelowLimitKept(t *testing.T) {
	res := buildPodResources(t, &api.ResourceSpec{
		Limits: api.ResourceList{CPU: "2", Memory: "8Gi"},
	}, nil)

	assertQuantity(t, res.Requests, corev1.ResourceCPU, "250m", "request")
	assertQuantity(t, res.Requests, corev1.ResourceMemory, "512Mi", "request")
}

// When both request and limit are explicit and conflict, nothing is adjusted;
// the API server rejects the pod as before.
func TestK8sResources_ExplicitConflictNotAdjusted(t *testing.T) {
	res := buildPodResources(t, &api.ResourceSpec{
		Requests: api.ResourceList{CPU: "4", Memory: "8Gi"},
		Limits:   api.ResourceList{CPU: "2", Memory: "4Gi"},
	}, nil)

	assertQuantity(t, res.Requests, corev1.ResourceCPU, "4", "request")
	assertQuantity(t, res.Limits, corev1.ResourceCPU, "2", "limit")
	assertQuantity(t, res.Requests, corev1.ResourceMemory, "8Gi", "request")
	assertQuantity(t, res.Limits, corev1.ResourceMemory, "4Gi", "limit")
}

// A request set via the kubernetes.resources map counts as explicit and is not
// clamped against an explicit limit.
func TestK8sResources_KubernetesMapRequestNotClamped(t *testing.T) {
	res := buildPodResources(t, &api.ResourceSpec{
		Limits: api.ResourceList{Memory: "1Gi"},
	}, &api.K8sResources{
		Requests: map[string]string{"memory": "2Gi"},
	})

	assertQuantity(t, res.Requests, corev1.ResourceMemory, "2Gi", "request")
}

// The caller's spec must not be mutated by default merging.
func TestK8sResources_SpecNotMutated(t *testing.T) {
	spec := &api.ResourceSpec{Limits: api.ResourceList{CPU: "2"}}
	_ = buildPodResources(t, spec, nil)
	if spec.Requests.CPU != "" || spec.Requests.Memory != "" || spec.Disk != "" || spec.Limits.Memory != "" {
		t.Errorf("spec was mutated: %+v", *spec)
	}
}
