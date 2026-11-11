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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func steBool(b bool) *bool { return &b }

func safeToEvictRunConfig(k *api.KubernetesConfig, annotations map[string]string) RunConfig {
	return RunConfig{
		Name:         "test-agent",
		Image:        "test:latest",
		UnixUsername: "scion",
		Labels:       map[string]string{"scion.project": "myproject"},
		Annotations:  annotations,
		Kubernetes:   k,
	}
}

func TestBuildPod_SafeToEvict(t *testing.T) {
	tests := []struct {
		name    string
		k8s     *api.KubernetesConfig
		wantSet bool
	}{
		{"no kubernetes block", nil, false},
		{"unset", &api.KubernetesConfig{Namespace: "ns"}, false},
		{"true adds nothing", &api.KubernetesConfig{SafeToEvict: steBool(true)}, false},
		{"false annotates", &api.KubernetesConfig{SafeToEvict: steBool(false)}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt, _, _ := newTestK8sRuntime()
			pod, err := rt.buildPod("default", safeToEvictRunConfig(tt.k8s, nil))
			require.NoError(t, err)
			v, ok := pod.Annotations[annotationSafeToEvict]
			assert.Equal(t, tt.wantSet, ok)
			if tt.wantSet {
				assert.Equal(t, "false", v)
			}
		})
	}
}

func TestBuildPod_SafeToEvict_WithPriorityClass(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()
	rt.PriorityClassName = "scion-agent-priority"
	pod, err := rt.buildPod("default", safeToEvictRunConfig(&api.KubernetesConfig{SafeToEvict: steBool(false)}, nil))
	require.NoError(t, err)
	assert.Equal(t, "scion-agent-priority", pod.Spec.PriorityClassName)
	assert.Equal(t, "false", pod.Annotations[annotationSafeToEvict])
}

// The resolved setting replaces a same-key annotation from config, keeps
// other annotations, and does not modify the caller's map.
func TestBuildPod_SafeToEvict_OverridesConfigAnnotation(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()
	annotations := map[string]string{annotationSafeToEvict: "true", "example.com/keep": "yes"}
	pod, err := rt.buildPod("default", safeToEvictRunConfig(&api.KubernetesConfig{SafeToEvict: steBool(false)}, annotations))
	require.NoError(t, err)
	assert.Equal(t, "false", pod.Annotations[annotationSafeToEvict])
	assert.Equal(t, "yes", pod.Annotations["example.com/keep"])
	assert.Equal(t, "true", annotations[annotationSafeToEvict], "caller's annotations must not be modified")
}

// With safeToEvict unset or true, a user-supplied annotation passes through
// unchanged.
func TestBuildPod_SafeToEvict_UnsetKeepsConfigAnnotation(t *testing.T) {
	for _, k := range []*api.KubernetesConfig{nil, {SafeToEvict: steBool(true)}} {
		rt, _, _ := newTestK8sRuntime()
		pod, err := rt.buildPod("default", safeToEvictRunConfig(k, map[string]string{annotationSafeToEvict: "false"}))
		require.NoError(t, err)
		assert.Equal(t, "false", pod.Annotations[annotationSafeToEvict])
	}
}

func TestAnnotationSafeToEvictKey(t *testing.T) {
	assert.Equal(t, "cluster-autoscaler.kubernetes.io/safe-to-evict", annotationSafeToEvict)
}
