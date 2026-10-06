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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// Kubernetes default resource requests. A default request is added for a
// resource only when neither its request nor its limit is set anywhere (the
// common resource spec or the kubernetes.resources map), so the scheduler (and
// GKE Autopilot) always sees a request.
//
// When a limit is set but its request is not, the request is left unset and the
// Kubernetes API server defaults it to the limit. That keeps today's scheduling:
// the built-in limits.cpu "2" yields a 2 CPU request, and a profile that sets
// only limits.memory is scheduled at that limit.
//
// Only requests are ever defaulted. No memory or ephemeral-storage limit is
// added: exceeding a memory limit OOM-kills the container and exceeding an
// ephemeral-storage limit evicts the pod, and real agent workloads (large Go
// builds, module and build caches, /workspace) routinely exceed any fixed
// default. Memory and disk limits apply only when set explicitly.
var k8sDefaultResourceRequests = []struct {
	name  corev1.ResourceName
	value string
}{
	{corev1.ResourceCPU, "250m"},
	{corev1.ResourceMemory, "512Mi"},
	{corev1.ResourceEphemeralStorage, "10Gi"},
}

// buildK8sResourceRequirements converts the common resource spec plus the
// Kubernetes-specific resources map into container resource requirements.
//
//   - Explicit fields in spec map to the matching request or limit.
//   - An explicit disk maps to ephemeral-storage in both requests and limits.
//   - The kubernetes.resources map is applied on top and overrides any key.
//   - Finally, cpu, memory and ephemeral-storage each get a default request
//     only if neither a request nor a limit is set for that resource.
func buildK8sResourceRequirements(spec *api.ResourceSpec, k8s *api.K8sResources) (corev1.ResourceRequirements, error) {
	reqs := corev1.ResourceList{}
	limits := corev1.ResourceList{}

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

	if spec != nil {
		for _, f := range []struct {
			list  corev1.ResourceList
			name  corev1.ResourceName
			value string
			field string
		}{
			{reqs, corev1.ResourceCPU, spec.Requests.CPU, "requests.cpu"},
			{reqs, corev1.ResourceMemory, spec.Requests.Memory, "requests.memory"},
			{limits, corev1.ResourceCPU, spec.Limits.CPU, "limits.cpu"},
			{limits, corev1.ResourceMemory, spec.Limits.Memory, "limits.memory"},
		} {
			if err := set(f.list, f.name, f.value, f.field); err != nil {
				return corev1.ResourceRequirements{}, err
			}
		}
		if spec.Disk != "" {
			q, err := parseResourceSafe(spec.Disk, "disk (ephemeral-storage)")
			if err != nil {
				return corev1.ResourceRequirements{}, err
			}
			reqs[corev1.ResourceEphemeralStorage] = q
			limits[corev1.ResourceEphemeralStorage] = q.DeepCopy()
		}
	}

	// Kubernetes-specific resources (extended resources like GPUs, or any
	// standard key) are merged on top and win over the common spec.
	if k8s != nil {
		for k, v := range k8s.Requests {
			q, err := parseResourceSafe(v, fmt.Sprintf("kubernetes.resources.requests.%s", k))
			if err != nil {
				return corev1.ResourceRequirements{}, err
			}
			reqs[corev1.ResourceName(k)] = q
		}
		for k, v := range k8s.Limits {
			q, err := parseResourceSafe(v, fmt.Sprintf("kubernetes.resources.limits.%s", k))
			if err != nil {
				return corev1.ResourceRequirements{}, err
			}
			limits[corev1.ResourceName(k)] = q
		}
	}

	for _, d := range k8sDefaultResourceRequests {
		_, hasReq := reqs[d.name]
		_, hasLimit := limits[d.name]
		if !hasReq && !hasLimit {
			reqs[d.name] = resource.MustParse(d.value)
		}
	}

	return corev1.ResourceRequirements{Requests: reqs, Limits: limits}, nil
}
