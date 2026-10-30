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
	"errors"
	"regexp"
	"strings"
	"testing"

	authv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

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

// doctorWithAccess returns a Kubernetes runtime whose SelfSubjectAccessReviews
// answer with decide: allowed, or an error when decide returns one.
func doctorWithAccess(decide func(attrs *authv1.ResourceAttributes) (bool, error)) *KubernetesRuntime {
	clientset := k8sfake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "agents"}})
	clientset.PrependReactor("create", "selfsubjectaccessreviews", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		review := action.(k8stesting.CreateAction).GetObject().(*authv1.SelfSubjectAccessReview).DeepCopy()
		allowed, err := decide(review.Spec.ResourceAttributes)
		if err != nil {
			return true, nil, err
		}
		review.Status.Allowed = allowed
		return true, review, nil
	})
	return NewKubernetesRuntime(k8s.NewTestClient(fake.NewSimpleDynamicClient(k8sruntime.NewScheme()), clientset))
}

var broadRoleWord = regexp.MustCompile(`(?i)\b(edit|admin|cluster-admin)\b`)

// assertBrokerRoleRemediation fails unless remediation recommends a
// namespaced Role for the Runtime Broker's own identity and points to the
// permissions docs. It must not name a ClusterRole, the edit or admin roles,
// or a namespace's default ServiceAccount.
func assertBrokerRoleRemediation(t *testing.T, check, remediation string) {
	t.Helper()
	lower := strings.ToLower(remediation)
	if !strings.Contains(lower, "namespaced role") {
		t.Errorf("%s remediation does not mention a namespaced Role: %q", check, remediation)
	}
	if !strings.Contains(remediation, "identity the Runtime Broker runs as") {
		t.Errorf("%s remediation does not name the broker's own identity: %q", check, remediation)
	}
	if !strings.Contains(remediation, brokerPermissionsDocsURL) {
		t.Errorf("%s remediation does not point to %s: %q", check, brokerPermissionsDocsURL, remediation)
	}
	if strings.Contains(lower, "clusterrole") {
		t.Errorf("%s remediation names a ClusterRole: %q", check, remediation)
	}
	if broadRoleWord.MatchString(remediation) {
		t.Errorf("%s remediation names the edit or admin role: %q", check, remediation)
	}
	if strings.Contains(remediation, ":default") || strings.Contains(lower, "--serviceaccount") {
		t.Errorf("%s remediation names a default ServiceAccount: %q", check, remediation)
	}
}

// Every pod and secret permission remediation of scion doctor, failed or
// unverifiable, recommends a namespaced Role bound to the Runtime Broker's
// own identity.
func TestRunDiagnostics_PermissionRemediationsUseNamespacedBrokerRole(t *testing.T) {
	errReview := errors.New("review unavailable")
	for _, tc := range []struct {
		name       string
		decide     func(attrs *authv1.ResourceAttributes) (bool, error)
		check      string
		wantStatus string
	}{
		{"pod verb denied", func(a *authv1.ResourceAttributes) (bool, error) {
			return a.Resource != "pods" || a.Verb != "list", nil
		}, "pod-permissions", "fail"},
		{"pod review error", func(a *authv1.ResourceAttributes) (bool, error) {
			if a.Resource == "pods" {
				return false, errReview
			}
			return true, nil
		}, "pod-permissions", "warn"},
		{"exec denied", func(a *authv1.ResourceAttributes) (bool, error) {
			return a.Subresource != "exec", nil
		}, "pod-permissions", "fail"},
		{"secret verb denied", func(a *authv1.ResourceAttributes) (bool, error) {
			return a.Resource != "secrets" || a.Verb != "delete", nil
		}, "secret-permissions", "fail"},
		{"secret review error", func(a *authv1.ResourceAttributes) (bool, error) {
			if a.Resource == "secrets" {
				return false, errReview
			}
			return true, nil
		}, "secret-permissions", "warn"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := doctorWithAccess(tc.decide).RunDiagnostics(DiagnosticOpts{Namespace: "agents"})
			var found bool
			for _, c := range report.Checks {
				if c.Name != tc.check {
					continue
				}
				found = true
				if c.Status != tc.wantStatus {
					t.Fatalf("%s status = %q (%s), want %q", c.Name, c.Status, c.Message, tc.wantStatus)
				}
				assertBrokerRoleRemediation(t, c.Name, c.Remediation)
				if !strings.Contains(c.Remediation, `"agents"`) {
					t.Errorf("%s remediation does not name the namespace: %q", c.Name, c.Remediation)
				}
			}
			if !found {
				t.Fatalf("check %s not in the report", tc.check)
			}
		})
	}
}

// With every review allowed, the pod and secret checks pass and carry no
// remediation.
func TestRunDiagnostics_PermissionChecksPassWhenAllowed(t *testing.T) {
	report := doctorWithAccess(func(*authv1.ResourceAttributes) (bool, error) { return true, nil }).RunDiagnostics(DiagnosticOpts{Namespace: "agents"})
	for _, c := range report.Checks {
		if c.Name != "pod-permissions" && c.Name != "secret-permissions" {
			continue
		}
		if c.Status != "pass" || c.Remediation != "" {
			t.Errorf("%s = %q, remediation %q; want pass with no remediation", c.Name, c.Status, c.Remediation)
		}
	}
}
