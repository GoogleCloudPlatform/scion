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

package config

import (
	"maps"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// BuiltinDefaultResources returns the fallback resource spec applied when no
// tier in the resolution chain specifies resources. It is the lowest-priority
// tier, below settings default_resources, profiles, templates and inline config.
//
// Only a CPU limit is set, and deliberately so:
//
//   - limits.cpu "2" maps to Docker/Podman `--cpus 2` and to the Kubernetes CPU
//     limit. It is a CFS quota: it throttles a busy container, it cannot kill
//     one. Without it, Docker and Podman run agent containers with no cgroup
//     limits at all, so a single agent running a parallel `go build` can consume
//     every core on the host. That is what happened during the 2026-07-28 hub
//     outage (~550% aggregate CPU, 13.5 s hub request latency). The value
//     matches the Kubernetes adapter's existing CPU limit fallback
//     (k8s_runtime.go) so behaviour is consistent across runtimes.
//
//   - limits.memory is intentionally left EMPTY. A hard memory limit maps to
//     `--memory`, and exceeding it makes the kernel OOM-kill the largest process
//     in the cgroup — which is frequently the agent harness rather than the
//     build that caused the growth, so the agent dies with no useful diagnostic.
//     A memory limit is available and supported, but it must be opted into
//     per deployment rather than shipped as a default.
//
// The returned spec is freshly allocated on every call. Callers merge it as the
// base of MergeResourceSpec and may mutate the result, so it must never be a
// shared package-level value.
func BuiltinDefaultResources() *api.ResourceSpec {
	return &api.ResourceSpec{
		Limits: api.ResourceList{CPU: "2"},
	}
}

// k8sCPUResource is the resource name used for CPU in kubernetes.resources maps.
const k8sCPUResource = "cpu"

// ApplyBuiltinDefaultResources applies the built-in CPU limit from
// BuiltinDefaultResources to a resolved resource spec, as the lowest-priority
// tier. It does nothing when any tier already set resources.limits.cpu.
//
// The built-in limit must not conflict with a larger CPU request, or Kubernetes
// rejects the pod (a request may not exceed its limit). Two requests are
// handled separately, so a Kubernetes-only request never changes the limit that
// Docker and Podman use:
//
//   - resources.requests.cpu larger than the built-in limit raises the built-in
//     resources.limits.cpu to that request. Every runtime stays CPU-bounded
//     (Docker and Podman ignore requests.cpu, so skipping the limit would leave
//     them unbounded).
//   - kubernetes.resources.requests.cpu larger than the resulting limit, with
//     kubernetes.resources.limits.cpu unset, sets kubernetes.resources.limits.cpu
//     to that request. resources.limits.cpu is left alone.
//
// Values are compared as CPU quantities ("2" equals "2000m"); a raised limit is
// the request string copied verbatim. A value that does not parse causes no
// raise; later validation reports it. The inputs are never mutated: both return
// values are either the inputs unchanged or fresh copies.
func ApplyBuiltinDefaultResources(res *api.ResourceSpec, k8s *api.K8sResources) (*api.ResourceSpec, *api.K8sResources) {
	if res != nil && res.Limits.CPU != "" {
		return res, k8s
	}
	merged := MergeResourceSpec(BuiltinDefaultResources(), res)
	if cpuExceeds(merged.Requests.CPU, merged.Limits.CPU) {
		merged.Limits.CPU = merged.Requests.CPU
	}

	if k8s != nil {
		_, k8sLimitSet := k8s.Limits[k8sCPUResource]
		k8sReq := k8s.Requests[k8sCPUResource]
		if !k8sLimitSet && cpuExceeds(k8sReq, merged.Limits.CPU) {
			raised := &api.K8sResources{
				Requests: maps.Clone(k8s.Requests),
				Limits:   maps.Clone(k8s.Limits),
			}
			if raised.Limits == nil {
				raised.Limits = map[string]string{}
			}
			raised.Limits[k8sCPUResource] = k8sReq
			k8s = raised
		}
	}
	return merged, k8s
}

// cpuExceeds reports whether request is a larger CPU quantity than limit. It
// returns false when either value is empty or does not parse.
func cpuExceeds(request, limit string) bool {
	if request == "" || limit == "" {
		return false
	}
	r, err := util.ParseCPU(request)
	if err != nil {
		return false
	}
	l, err := util.ParseCPU(limit)
	if err != nil {
		return false
	}
	return r > l
}

// ShouldEnforceResourceDefaults reports whether the built-in resource defaults
// from BuiltinDefaultResources should be applied.
//
// It is controlled by runtime.enforce_resource_defaults, which defaults to true
// when unset so the protective behaviour is fail-safe: a deployment that has
// never heard of this key still gets a CPU limit. Operators who need the
// previous unlimited behaviour can set it to false without a rollback.
func ShouldEnforceResourceDefaults(vs *VersionedSettings) bool {
	if vs == nil || vs.Runtime == nil || vs.Runtime.EnforceResourceDefaults == nil {
		return true
	}
	return *vs.Runtime.EnforceResourceDefaults
}
