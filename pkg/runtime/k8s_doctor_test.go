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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/GoogleCloudPlatform/scion/pkg/k8s"
)

func TestRunDiagnostics_BasicChecks(t *testing.T) {
	// Create a fake clientset with the default namespace
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
	}
	clientset := k8sfake.NewClientset(ns)

	scheme := k8sruntime.NewScheme()
	dynClient := fake.NewSimpleDynamicClient(scheme)
	client := k8s.NewTestClient(dynClient, clientset)

	rt := NewKubernetesRuntime(client)

	report := rt.RunDiagnostics(DiagnosticOpts{Namespace: "default"})

	if report.Runtime != "kubernetes" {
		t.Errorf("expected runtime 'kubernetes', got %q", report.Runtime)
	}

	// Should have basic checks: connectivity, namespace, pods, secrets
	if len(report.Checks) < 4 {
		t.Errorf("expected at least 4 checks, got %d", len(report.Checks))
	}

	// Check that expected check names are present
	checkNames := map[string]bool{}
	for _, c := range report.Checks {
		checkNames[c.Name] = true
	}

	for _, expected := range []string{"cluster-connectivity", "namespace-access", "pod-permissions", "secret-permissions"} {
		if !checkNames[expected] {
			t.Errorf("expected check %q not found in report", expected)
		}
	}
}

func TestRunDiagnostics_GKEChecks(t *testing.T) {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
	}
	clientset := k8sfake.NewClientset(ns)

	scheme := k8sruntime.NewScheme()
	dynClient := fake.NewSimpleDynamicClient(scheme)
	client := k8s.NewTestClient(dynClient, clientset)

	rt := NewKubernetesRuntime(client)
	rt.GKEMode = true

	report := rt.RunDiagnostics(DiagnosticOpts{GKEMode: true})

	// GKE mode should include additional checks
	checkNames := map[string]bool{}
	for _, c := range report.Checks {
		checkNames[c.Name] = true
	}

	for _, expected := range []string{"secretproviderclass-crd", "secrets-store-csi-driver", "gcsfuse-csi-driver"} {
		if !checkNames[expected] {
			t.Errorf("expected GKE check %q not found in report", expected)
		}
	}
}

func TestRunDiagnostics_NonGKE_NoCSIChecks(t *testing.T) {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
	}
	clientset := k8sfake.NewClientset(ns)

	scheme := k8sruntime.NewScheme()
	dynClient := fake.NewSimpleDynamicClient(scheme)
	client := k8s.NewTestClient(dynClient, clientset)

	rt := NewKubernetesRuntime(client)

	report := rt.RunDiagnostics(DiagnosticOpts{})

	// Non-GKE mode should NOT include CSI checks
	for _, c := range report.Checks {
		switch c.Name {
		case "secretproviderclass-crd", "secrets-store-csi-driver", "gcsfuse-csi-driver":
			t.Errorf("non-GKE mode should not include check %q", c.Name)
		}
	}
}

func TestRunDiagnostics_NamespaceNotFound(t *testing.T) {
	// Create clientset WITHOUT the target namespace
	clientset := k8sfake.NewClientset()

	scheme := k8sruntime.NewScheme()
	dynClient := fake.NewSimpleDynamicClient(scheme)
	client := k8s.NewTestClient(dynClient, clientset)

	rt := NewKubernetesRuntime(client)

	report := rt.RunDiagnostics(DiagnosticOpts{Namespace: "nonexistent"})

	// Find namespace check
	for _, c := range report.Checks {
		if c.Name == "namespace-access" {
			if c.Status != "fail" {
				t.Errorf("expected namespace-access to fail for nonexistent namespace, got %q", c.Status)
			}
			return
		}
	}
	t.Error("namespace-access check not found in report")
}

func TestRunDiagnostics_CustomNamespace(t *testing.T) {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "custom-ns"},
	}
	clientset := k8sfake.NewClientset(ns)

	scheme := k8sruntime.NewScheme()
	dynClient := fake.NewSimpleDynamicClient(scheme)
	client := k8s.NewTestClient(dynClient, clientset)

	rt := NewKubernetesRuntime(client)

	report := rt.RunDiagnostics(DiagnosticOpts{Namespace: "custom-ns"})

	for _, c := range report.Checks {
		if c.Name == "namespace-access" {
			if c.Status != "pass" {
				t.Errorf("expected namespace-access to pass for existing namespace, got %q: %s", c.Status, c.Message)
			}
			return
		}
	}
	t.Error("namespace-access check not found in report")
}

func TestDiagnosable_Interface(t *testing.T) {
	// Verify KubernetesRuntime implements Diagnosable
	var _ Diagnosable = (*KubernetesRuntime)(nil)
}

func TestCheckResult_Fields(t *testing.T) {
	result := CheckResult{
		Name:        "test-check",
		Status:      "fail",
		Message:     "something broke",
		Remediation: "fix it",
	}

	if result.Name != "test-check" {
		t.Errorf("unexpected Name: %s", result.Name)
	}
	if result.Status != "fail" {
		t.Errorf("unexpected Status: %s", result.Status)
	}
	if result.Remediation != "fix it" {
		t.Errorf("unexpected Remediation: %s", result.Remediation)
	}
}
