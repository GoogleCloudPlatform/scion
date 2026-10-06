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

// The resolved spec normally carries only the built-in limits.cpu default. CPU
// then has a limit, so no cpu request is added (Kubernetes defaults it to the
// limit, as before); memory and ephemeral-storage get default requests.
func TestK8sResources_OnlyCPULimit(t *testing.T) {
	res := buildPodResources(t, &api.ResourceSpec{Limits: api.ResourceList{CPU: "2"}}, nil)

	assertAbsent(t, res.Requests, corev1.ResourceCPU, "request")
	assertQuantity(t, res.Limits, corev1.ResourceCPU, "2", "limit")
	assertQuantity(t, res.Requests, corev1.ResourceMemory, "512Mi", "request")
	assertQuantity(t, res.Requests, corev1.ResourceEphemeralStorage, "10Gi", "request")
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

// A memory limit without a memory request leaves the request unset, so the pod
// is scheduled at the limit as before. CPU, with nothing set, gets its default.
func TestK8sResources_OnlyMemoryLimit(t *testing.T) {
	res := buildPodResources(t, &api.ResourceSpec{Limits: api.ResourceList{Memory: "8Gi"}}, nil)

	assertAbsent(t, res.Requests, corev1.ResourceMemory, "request")
	assertQuantity(t, res.Limits, corev1.ResourceMemory, "8Gi", "limit")
	assertQuantity(t, res.Requests, corev1.ResourceCPU, "250m", "request")
	assertQuantity(t, res.Requests, corev1.ResourceEphemeralStorage, "10Gi", "request")
}

// With nothing set (nil spec, e.g. runtime.enforce_resource_defaults: false, or
// an empty spec) every resource gets its default request and there are no limits.
func TestK8sResources_NothingSet_RequestDefaults(t *testing.T) {
	for name, spec := range map[string]*api.ResourceSpec{"nil": nil, "empty": {}} {
		t.Run(name, func(t *testing.T) {
			res := buildPodResources(t, spec, nil)
			assertQuantity(t, res.Requests, corev1.ResourceCPU, "250m", "request")
			assertQuantity(t, res.Requests, corev1.ResourceMemory, "512Mi", "request")
			assertQuantity(t, res.Requests, corev1.ResourceEphemeralStorage, "10Gi", "request")
			if len(res.Limits) != 0 {
				t.Errorf("expected no limits, got %v", res.Limits)
			}
		})
	}
}

// An explicit request without a limit is kept as is and no limit is added.
func TestK8sResources_ExplicitRequestOnly(t *testing.T) {
	res := buildPodResources(t, &api.ResourceSpec{
		Requests: api.ResourceList{CPU: "1", Memory: "3Gi"},
	}, nil)

	assertQuantity(t, res.Requests, corev1.ResourceCPU, "1", "request")
	assertQuantity(t, res.Requests, corev1.ResourceMemory, "3Gi", "request")
	if len(res.Limits) != 0 {
		t.Errorf("expected no limits, got %v", res.Limits)
	}
}

// An explicit disk (e.g. from a profile) maps to ephemeral-storage request and
// limit; the remaining defaults still follow the rule.
func TestK8sResources_DiskSet_EphemeralStorageRequestAndLimit(t *testing.T) {
	res := buildPodResources(t, &api.ResourceSpec{
		Limits: api.ResourceList{CPU: "2"},
		Disk:   "40Gi",
	}, nil)

	assertQuantity(t, res.Requests, corev1.ResourceEphemeralStorage, "40Gi", "request")
	assertQuantity(t, res.Limits, corev1.ResourceEphemeralStorage, "40Gi", "limit")
	assertQuantity(t, res.Requests, corev1.ResourceMemory, "512Mi", "request")
	assertAbsent(t, res.Requests, corev1.ResourceCPU, "request")
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

// An explicit kubernetes.resources map wins over the common spec.
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
	assertAbsent(t, res.Requests, corev1.ResourceCPU, "request")
}

// A limit set only in the kubernetes.resources map also suppresses the default
// request for that resource.
func TestK8sResources_KubernetesMapLimitSuppressesDefaultRequest(t *testing.T) {
	res := buildPodResources(t, nil, &api.K8sResources{
		Limits: map[string]string{"memory": "6Gi", "ephemeral-storage": "25Gi"},
	})

	assertAbsent(t, res.Requests, corev1.ResourceMemory, "request")
	assertAbsent(t, res.Requests, corev1.ResourceEphemeralStorage, "request")
	assertQuantity(t, res.Limits, corev1.ResourceMemory, "6Gi", "limit")
	assertQuantity(t, res.Limits, corev1.ResourceEphemeralStorage, "25Gi", "limit")
	assertQuantity(t, res.Requests, corev1.ResourceCPU, "250m", "request")
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

// The caller's spec must not be mutated.
func TestK8sResources_SpecNotMutated(t *testing.T) {
	spec := &api.ResourceSpec{Limits: api.ResourceList{CPU: "2"}}
	_ = buildPodResources(t, spec, nil)
	if spec.Requests.CPU != "" || spec.Requests.Memory != "" || spec.Disk != "" || spec.Limits.Memory != "" {
		t.Errorf("spec was mutated: %+v", *spec)
	}
}
