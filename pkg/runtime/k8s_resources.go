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
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// Kubernetes default resource requests. They are filled in field by field for
// any request the resolved resource spec leaves empty, so the scheduler (and GKE
// Autopilot) always sees a predictable request.
//
// Only requests are defaulted. No memory or ephemeral-storage limit is ever
// added by default: exceeding a memory limit OOM-kills the container and
// exceeding an ephemeral-storage limit evicts the pod, and real agent workloads
// (large Go builds, module and build caches, /workspace) routinely exceed any
// fixed default. Memory and disk limits apply only when set explicitly. The CPU
// limit comes from the resolved spec (the built-in limits.cpu default is applied
// upstream in agent provisioning), never from here.
const (
	k8sDefaultCPURequest              = "250m"
	k8sDefaultMemoryRequest           = "512Mi"
	k8sDefaultEphemeralStorageRequest = "10Gi"
)

// k8sDefaultResourceRequests returns a freshly allocated spec holding the
// Kubernetes request defaults, used as the base of MergeResourceSpec.
func k8sDefaultResourceRequests() *api.ResourceSpec {
	return &api.ResourceSpec{
		Requests: api.ResourceList{CPU: k8sDefaultCPURequest, Memory: k8sDefaultMemoryRequest},
	}
}

// buildK8sResourceRequirements converts the common resource spec plus the
// Kubernetes-specific resources map into container resource requirements.
//
//   - Explicit fields in spec always win; empty request fields get the
//     Kubernetes request defaults (merged with config.MergeResourceSpec).
//   - An explicit disk maps to ephemeral-storage in both requests and limits.
//     Without one, only a default ephemeral-storage request is set.
//   - The kubernetes.resources map is applied last and overrides any key.
//   - A defaulted request that is larger than an explicit limit for the same
//     resource is lowered to that limit, so a defaulted value never makes the
//     pod invalid. Explicit values are never changed; a conflict between an
//     explicit request and an explicit limit is left to the API server.
func buildK8sResourceRequirements(spec *api.ResourceSpec, k8s *api.K8sResources) (corev1.ResourceRequirements, error) {
	var explicit api.ResourceSpec
	if spec != nil {
		explicit = *spec
	}
	merged := config.MergeResourceSpec(k8sDefaultResourceRequests(), spec)

	reqs := corev1.ResourceList{}
	limits := corev1.ResourceList{}
	defaulted := map[corev1.ResourceName]bool{}

	set := func(list corev1.ResourceList, name corev1.ResourceName, value, field string) error {
		if value == "" {
			return nil
		}
		q, err := parseResourceSafe(value, field)
		if err != nil {
			return err
		}
		list[name] = q
		return nil
	}

	if err := set(reqs, corev1.ResourceCPU, merged.Requests.CPU, "requests.cpu"); err != nil {
		return corev1.ResourceRequirements{}, err
	}
	defaulted[corev1.ResourceCPU] = explicit.Requests.CPU == ""
	if err := set(reqs, corev1.ResourceMemory, merged.Requests.Memory, "requests.memory"); err != nil {
		return corev1.ResourceRequirements{}, err
	}
	defaulted[corev1.ResourceMemory] = explicit.Requests.Memory == ""
	if err := set(limits, corev1.ResourceCPU, merged.Limits.CPU, "limits.cpu"); err != nil {
		return corev1.ResourceRequirements{}, err
	}
	if err := set(limits, corev1.ResourceMemory, merged.Limits.Memory, "limits.memory"); err != nil {
		return corev1.ResourceRequirements{}, err
	}
	if merged.Disk != "" {
		q, err := parseResourceSafe(merged.Disk, "disk (ephemeral-storage)")
		if err != nil {
			return corev1.ResourceRequirements{}, err
		}
		reqs[corev1.ResourceEphemeralStorage] = q
		limits[corev1.ResourceEphemeralStorage] = q
	} else {
		reqs[corev1.ResourceEphemeralStorage] = resource.MustParse(k8sDefaultEphemeralStorageRequest)
		defaulted[corev1.ResourceEphemeralStorage] = true
	}

	// Kubernetes-specific resources (extended resources like GPUs, or any
	// standard key) are merged on top and win over everything above.
	if k8s != nil {
		for k, v := range k8s.Requests {
			q, err := parseResourceSafe(v, fmt.Sprintf("kubernetes.resources.requests.%s", k))
			if err != nil {
				return corev1.ResourceRequirements{}, err
			}
			reqs[corev1.ResourceName(k)] = q
			delete(defaulted, corev1.ResourceName(k))
		}
		for k, v := range k8s.Limits {
			q, err := parseResourceSafe(v, fmt.Sprintf("kubernetes.resources.limits.%s", k))
			if err != nil {
				return corev1.ResourceRequirements{}, err
			}
			limits[corev1.ResourceName(k)] = q
		}
	}

	for name, isDefault := range defaulted {
		if !isDefault {
			continue
		}
		lim, hasLimit := limits[name]
		req, hasReq := reqs[name]
		if hasLimit && hasReq && lim.Cmp(req) < 0 {
			reqs[name] = lim.DeepCopy()
		}
	}

	return corev1.ResourceRequirements{Requests: reqs, Limits: limits}, nil
}
