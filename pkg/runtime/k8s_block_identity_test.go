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
)

// TestBuildPod_KubernetesBlockIdentity covers the GCP identity "block" pod
// spec (ptone/scion#4034): the configured ServiceAccount (the block
// ServiceAccount, or none for the namespace default), the Kubernetes API
// token not mounted, and the Workload Identity node selector merged into any
// existing one without mutating the caller's map.
func TestBuildPod_KubernetesBlockIdentity(t *testing.T) {
	cases := []struct {
		name     string
		k8s      *api.KubernetesConfig
		wantSA   string
		wantSels map[string]string
	}{
		{
			name:     "block service account",
			k8s:      &api.KubernetesConfig{ServiceAccountName: "scion-block"},
			wantSA:   "scion-block",
			wantSels: map[string]string{KubernetesWorkloadIdentityNodeLabel: "true"},
		},
		{
			name:     "namespace default",
			k8s:      nil,
			wantSA:   "",
			wantSels: map[string]string{KubernetesWorkloadIdentityNodeLabel: "true"},
		},
		{
			name: "merged node selector",
			k8s: &api.KubernetesConfig{NodeSelector: map[string]string{
				"pool":                              "agents",
				KubernetesWorkloadIdentityNodeLabel: "false",
			}},
			wantSels: map[string]string{"pool": "agents", KubernetesWorkloadIdentityNodeLabel: "true"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt, _, _ := newTestK8sRuntime()
			var origSel map[string]string
			if tc.k8s != nil && tc.k8s.NodeSelector != nil {
				origSel = map[string]string{}
				for k, v := range tc.k8s.NodeSelector {
					origSel[k] = v
				}
			}
			pod, err := rt.buildPod("default", RunConfig{
				Name:                    "block-test",
				Image:                   "example.com/image:v1",
				UnixUsername:            "scion",
				Harness:                 &EnvHarness{},
				Kubernetes:              tc.k8s,
				KubernetesBlockIdentity: true,
			})
			if err != nil {
				t.Fatalf("buildPod: %v", err)
			}
			if pod.Spec.ServiceAccountName != tc.wantSA {
				t.Errorf("ServiceAccountName = %q, want %q", pod.Spec.ServiceAccountName, tc.wantSA)
			}
			if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
				t.Errorf("expected automountServiceAccountToken false, got %v", pod.Spec.AutomountServiceAccountToken)
			}
			if len(pod.Spec.NodeSelector) != len(tc.wantSels) {
				t.Errorf("NodeSelector = %v, want %v", pod.Spec.NodeSelector, tc.wantSels)
			}
			for k, v := range tc.wantSels {
				if pod.Spec.NodeSelector[k] != v {
					t.Errorf("NodeSelector[%q] = %q, want %q", k, pod.Spec.NodeSelector[k], v)
				}
			}
			for k, v := range origSel {
				if tc.k8s.NodeSelector[k] != v {
					t.Errorf("caller's NodeSelector was mutated: %v", tc.k8s.NodeSelector)
				}
			}
		})
	}
}

// TestBuildPod_NoBlockIdentityUnchanged pins that a pod without the block
// identity (passthrough, assign) keeps today's spec: the ServiceAccount as
// configured, no automount setting and no added node selector.
func TestBuildPod_NoBlockIdentityUnchanged(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()
	pod, err := rt.buildPod("default", RunConfig{
		Name:         "assign-test",
		Image:        "example.com/image:v1",
		UnixUsername: "scion",
		Harness:      &EnvHarness{},
		Kubernetes:   &api.KubernetesConfig{ServiceAccountName: "agent-worker-ksa"},
	})
	if err != nil {
		t.Fatalf("buildPod: %v", err)
	}
	if pod.Spec.ServiceAccountName != "agent-worker-ksa" {
		t.Errorf("ServiceAccountName = %q, want agent-worker-ksa", pod.Spec.ServiceAccountName)
	}
	if pod.Spec.AutomountServiceAccountToken != nil {
		t.Errorf("expected automountServiceAccountToken unset, got %v", *pod.Spec.AutomountServiceAccountToken)
	}
	if _, ok := pod.Spec.NodeSelector[KubernetesWorkloadIdentityNodeLabel]; ok {
		t.Errorf("expected no Workload Identity node selector, got %v", pod.Spec.NodeSelector)
	}
}
