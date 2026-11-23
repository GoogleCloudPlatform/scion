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
// identity (passthrough, assign) keeps the ServiceAccount as configured,
// has automountServiceAccountToken false, as every agent pod, and gets no
// added node selector.
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
	if a := pod.Spec.AutomountServiceAccountToken; a == nil || *a {
		t.Errorf("automountServiceAccountToken = %v, want false", a)
	}
	if _, ok := pod.Spec.NodeSelector[KubernetesWorkloadIdentityNodeLabel]; ok {
		t.Errorf("expected no Workload Identity node selector, got %v", pod.Spec.NodeSelector)
	}
}

// TestBuildPod_MetadataModeLabel verifies that buildPod stamps
// scion.metadata-mode from KubernetesBlockIdentity or SCION_METADATA_MODE,
// ignores unrecognized values, strips caller-supplied overrides, and never
// mutates config.Labels.
func TestBuildPod_MetadataModeLabel(t *testing.T) {
	cases := []struct {
		name      string
		blockID   bool
		env       []string
		labels    map[string]string
		wantMode  string
		wantLabel bool
	}{
		{
			name:      "block identity sets block and overrides spoofed label",
			blockID:   true,
			env:       []string{"SCION_METADATA_MODE=passthrough"},
			labels:    map[string]string{"scion.name": "a1", k8sMetadataModeLabel: "passthrough"},
			wantMode:  "block",
			wantLabel: true,
		},
		{
			name:      "env block sets block",
			env:       []string{"SCION_METADATA_MODE=block"},
			wantMode:  "block",
			wantLabel: true,
		},
		{
			// On Kubernetes, Workload Identity assign pods are rewritten to
			// SCION_METADATA_MODE=passthrough by the Runtime Broker before
			// dispatch, so they receive scion.metadata-mode=passthrough.
			name:      "env passthrough sets passthrough",
			env:       []string{"SCION_METADATA_MODE=passthrough"},
			wantMode:  "passthrough",
			wantLabel: true,
		},
		{
			name:      "last env entry wins",
			env:       []string{"SCION_METADATA_MODE=passthrough", "SCION_METADATA_MODE=block"},
			wantMode:  "block",
			wantLabel: true,
		},
		{
			name:      "unrecognized env value omits label and strips caller label",
			env:       []string{"SCION_METADATA_MODE=invalid/value"},
			labels:    map[string]string{"scion.name": "a2", k8sMetadataModeLabel: "assign"},
			wantLabel: false,
		},
		{
			name:      "unset env omits label and strips caller label",
			labels:    map[string]string{"scion.name": "a3", k8sMetadataModeLabel: "block"},
			wantLabel: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt, _, _ := newTestK8sRuntime()
			var origLabels map[string]string
			if tc.labels != nil {
				origLabels = make(map[string]string, len(tc.labels))
				for k, v := range tc.labels {
					origLabels[k] = v
				}
			}

			pod, err := rt.buildPod("default", RunConfig{
				Name:                    "mode-label-test",
				Image:                   "example.com/image:v1",
				UnixUsername:            "scion",
				Harness:                 &EnvHarness{},
				Env:                     tc.env,
				Labels:                  tc.labels,
				KubernetesBlockIdentity: tc.blockID,
			})
			if err != nil {
				t.Fatalf("buildPod: %v", err)
			}

			got, ok := pod.Labels[k8sMetadataModeLabel]
			if ok != tc.wantLabel {
				t.Fatalf("pod.Labels[%q] present = %v (%q), want present=%v", k8sMetadataModeLabel, ok, got, tc.wantLabel)
			}
			if tc.wantLabel && got != tc.wantMode {
				t.Errorf("pod.Labels[%q] = %q, want %q", k8sMetadataModeLabel, got, tc.wantMode)
			}
			if len(tc.labels) != len(origLabels) {
				t.Errorf("caller's Labels length mutated: got %v, want %v", tc.labels, origLabels)
			}
			for k, v := range origLabels {
				if tc.labels[k] != v {
					t.Errorf("caller's Labels[%q] mutated: got %q, want %q", k, tc.labels[k], v)
				}
			}
		})
	}
}
