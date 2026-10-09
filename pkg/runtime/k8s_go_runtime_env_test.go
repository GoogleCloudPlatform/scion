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
	"k8s.io/apimachinery/pkg/api/resource"
)

// TestK8sBuildPod_GoRuntimeEnvFromLimits checks the GOMAXPROCS and
// GOMEMLIMIT entries in the generated pod spec's container env.
func TestK8sBuildPod_GoRuntimeEnvFromLimits(t *testing.T) {
	const unset = "<unset>"
	tests := []struct {
		name          string
		spec          *api.ResourceSpec
		k8s           *api.K8sResources
		env           []string
		wantMaxProcs  string
		wantMemLimit  string
		memLimitBelow string // when set, GOMEMLIMIT must be below this quantity
	}{
		{
			name:          "4 CPU and 8Gi limits",
			spec:          &api.ResourceSpec{Limits: api.ResourceList{CPU: "4", Memory: "8Gi"}},
			wantMaxProcs:  "4",
			wantMemLimit:  "7372MiB",
			memLimitBelow: "8Gi",
		},
		{
			name:         "fractional CPU limit rounds up",
			spec:         &api.ResourceSpec{Limits: api.ResourceList{CPU: "1500m"}},
			wantMaxProcs: "2",
			wantMemLimit: unset,
		},
		{
			name:         "CPU limit below one core gives one",
			spec:         &api.ResourceSpec{Limits: api.ResourceList{CPU: "100m"}},
			wantMaxProcs: "1",
			wantMemLimit: unset,
		},
		{
			name:          "small memory limit",
			spec:          &api.ResourceSpec{Limits: api.ResourceList{Memory: "128Mi"}},
			wantMaxProcs:  unset,
			wantMemLimit:  "115MiB",
			memLimitBelow: "128Mi",
		},
		{
			name:         "CPU-only limit sets only GOMAXPROCS",
			spec:         &api.ResourceSpec{Limits: api.ResourceList{CPU: "2"}},
			wantMaxProcs: "2",
			wantMemLimit: unset,
		},
		{
			name:          "memory-only limit sets only GOMEMLIMIT",
			spec:          &api.ResourceSpec{Limits: api.ResourceList{Memory: "2Gi"}},
			wantMaxProcs:  unset,
			wantMemLimit:  "1843MiB",
			memLimitBelow: "2Gi",
		},
		{
			name:          "decimal memory units",
			spec:          &api.ResourceSpec{Limits: api.ResourceList{Memory: "8G"}},
			wantMaxProcs:  unset,
			wantMemLimit:  "6866MiB",
			memLimitBelow: "8G",
		},
		{
			name:         "memory limit too small for one MiB sets nothing",
			spec:         &api.ResourceSpec{Limits: api.ResourceList{Memory: "1Mi"}},
			wantMaxProcs: unset,
			wantMemLimit: unset,
		},
		{
			name:         "CPU limit of MaxInt32 cores is kept",
			spec:         &api.ResourceSpec{Limits: api.ResourceList{CPU: "2147483647"}},
			wantMaxProcs: "2147483647",
			wantMemLimit: unset,
		},
		{
			name:         "CPU limit above MaxInt32 cores sets nothing",
			spec:         &api.ResourceSpec{Limits: api.ResourceList{CPU: "2147483648"}},
			wantMaxProcs: unset,
			wantMemLimit: unset,
		},
		{
			name:         "CPU limit of MaxInt64 cores sets nothing",
			spec:         &api.ResourceSpec{Limits: api.ResourceList{CPU: "9223372036854775807"}},
			wantMaxProcs: unset,
			wantMemLimit: unset,
		},
		{
			name:         "CPU limit above MaxInt64 cores sets nothing",
			spec:         &api.ResourceSpec{Limits: api.ResourceList{CPU: "1e30"}},
			wantMaxProcs: unset,
			wantMemLimit: unset,
		},
		{
			name:         "memory limit above MaxInt64 bytes sets nothing",
			spec:         &api.ResourceSpec{Limits: api.ResourceList{Memory: "1e30"}},
			wantMaxProcs: unset,
			wantMemLimit: unset,
		},
		{
			name:         "requests without limits set neither",
			spec:         &api.ResourceSpec{Requests: api.ResourceList{CPU: "2", Memory: "4Gi"}},
			wantMaxProcs: unset,
			wantMemLimit: unset,
		},
		{
			name:         "no resources set neither",
			wantMaxProcs: unset,
			wantMemLimit: unset,
		},
		{
			name:          "kubernetes.resources limits are used",
			k8s:           &api.K8sResources{Limits: map[string]string{"cpu": "3", "memory": "1Gi"}},
			wantMaxProcs:  "3",
			wantMemLimit:  "921MiB",
			memLimitBelow: "1Gi",
		},
		{
			name:         "explicit GOMAXPROCS wins",
			spec:         &api.ResourceSpec{Limits: api.ResourceList{CPU: "4", Memory: "8Gi"}},
			env:          []string{"GOMAXPROCS=8"},
			wantMaxProcs: "8",
			wantMemLimit: "7372MiB",
		},
		{
			name:         "explicit GOMEMLIMIT=off is kept",
			spec:         &api.ResourceSpec{Limits: api.ResourceList{CPU: "4", Memory: "8Gi"}},
			env:          []string{"GOMEMLIMIT=off"},
			wantMaxProcs: "4",
			wantMemLimit: "off",
		},
		{
			name:         "explicit values win without limits",
			env:          []string{"GOMAXPROCS=2", "GOMEMLIMIT=1GiB"},
			wantMaxProcs: "2",
			wantMemLimit: "1GiB",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt, _, _ := newTestK8sRuntime()
			cfg := RunConfig{
				Name:         "go-env-agent",
				Image:        "test:latest",
				UnixUsername: "scion",
				Resources:    tt.spec,
				Env:          tt.env,
			}
			if tt.k8s != nil {
				cfg.Kubernetes = &api.KubernetesConfig{Resources: tt.k8s}
			}
			pod, err := rt.buildPod("default", cfg)
			if err != nil {
				t.Fatalf("buildPod: %v", err)
			}
			env := pod.Spec.Containers[0].Env

			got := func(name string) string {
				t.Helper()
				value, count := unset, 0
				for _, e := range env {
					if e.Name == name {
						count++
						value = e.Value
					}
				}
				if count > 1 {
					t.Errorf("%s appears %d times in the container env", name, count)
				}
				return value
			}

			if v := got("GOMAXPROCS"); v != tt.wantMaxProcs {
				t.Errorf("GOMAXPROCS = %q, want %q", v, tt.wantMaxProcs)
			}
			memLimit := got("GOMEMLIMIT")
			if memLimit != tt.wantMemLimit {
				t.Errorf("GOMEMLIMIT = %q, want %q", memLimit, tt.wantMemLimit)
			}
			if tt.memLimitBelow != "" {
				// GOMEMLIMIT uses IEC suffixes such as MiB; resource
				// quantities spell the same unit as Mi.
				q, err := resource.ParseQuantity(trimTrailingB(memLimit))
				if err != nil {
					t.Fatalf("parse GOMEMLIMIT %q: %v", memLimit, err)
				}
				limit := resource.MustParse(tt.memLimitBelow)
				if q.Cmp(limit) >= 0 {
					t.Errorf("GOMEMLIMIT %s is not below the %s memory limit", memLimit, tt.memLimitBelow)
				}
			}
		})
	}
}

func trimTrailingB(s string) string {
	if n := len(s); n > 0 && s[n-1] == 'B' {
		return s[:n-1]
	}
	return s
}

// TestAppendGoRuntimeEnvFromLimits_LargeMemoryLimit checks that a very
// large memory limit does not overflow.
func TestAppendGoRuntimeEnvFromLimits_LargeMemoryLimit(t *testing.T) {
	env := appendGoRuntimeEnvFromLimits(nil, corev1.ResourceList{
		corev1.ResourceMemory: resource.MustParse("4Ei"),
	})
	if len(env) != 1 || env[0].Name != "GOMEMLIMIT" || env[0].Value != "3958241859993MiB" {
		t.Fatalf("env = %+v, want GOMEMLIMIT=3958241859993MiB", env)
	}
}
