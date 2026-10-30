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
	"context"
	"fmt"
	"time"

	authv1 "k8s.io/api/authorization/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// brokerPermissionsDocsURL is the documentation section that holds the
// Runtime Broker's Role and RoleBinding.
const brokerPermissionsDocsURL = "https://googlecloudplatform.github.io/scion/hosted/ha/kubernetes/#runtime-broker-permissions"

// brokerRoleRemediation is the remediation for a missing or unverifiable
// Runtime Broker permission in namespace: a namespaced Role, holding only what
// the broker uses, bound to the identity the broker itself runs as.
func brokerRoleRemediation(namespace string) string {
	return fmt.Sprintf("Bind a namespaced Role in %q, holding only the permissions the Runtime Broker uses, to the identity the Runtime Broker runs as "+
		"(its own ServiceAccount when it runs in the cluster, or its kubeconfig user). See %s", namespace, brokerPermissionsDocsURL)
}

// RunDiagnostics performs preflight checks against the Kubernetes cluster
// to verify that the runtime is properly configured and operational.
func (r *KubernetesRuntime) RunDiagnostics(opts DiagnosticOpts) DiagnosticReport {
	report := DiagnosticReport{
		Runtime: "kubernetes",
	}

	namespace := opts.Namespace
	if namespace == "" {
		namespace = r.DefaultNamespace
	}

	report.Checks = append(report.Checks, r.checkClusterConnectivity())
	report.Checks = append(report.Checks, r.checkNamespaceAccess(namespace))
	report.Checks = append(report.Checks, r.checkPodPermissions(namespace))
	report.Checks = append(report.Checks, r.checkSecretPermissions(namespace))

	if opts.GKEMode || r.GKEMode {
		report.Checks = append(report.Checks, r.checkSecretProviderClassCRD())
		report.Checks = append(report.Checks, r.checkSecretsStoreCSIDriver(namespace))
		report.Checks = append(report.Checks, r.checkGCSFuseCSIDriver(namespace))
	}

	return report
}

func (r *KubernetesRuntime) diagnosticContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}

func (r *KubernetesRuntime) checkClusterConnectivity() CheckResult {
	ctx, cancel := r.diagnosticContext()
	defer cancel()

	contextName := r.Client.CurrentContext
	if contextName == "" {
		contextName = "(default)"
	}

	_, err := r.Client.Clientset.Discovery().ServerVersion()
	if err != nil {
		return CheckResult{
			Name:        "cluster-connectivity",
			Status:      "fail",
			Message:     fmt.Sprintf("Cannot connect to cluster (context: %s): %v", contextName, err),
			Remediation: "Verify your kubeconfig is valid and the cluster is reachable. Run 'kubectl cluster-info' to test connectivity.",
		}
	}

	// Also verify we can make API calls (auth check)
	_, err = r.Client.Clientset.CoreV1().Namespaces().List(ctx, metav1.ListOptions{Limit: 1})
	if err != nil {
		return CheckResult{
			Name:        "cluster-connectivity",
			Status:      "warn",
			Message:     fmt.Sprintf("Connected to cluster (context: %s) but namespace listing failed: %v", contextName, err),
			Remediation: "Your credentials may have limited permissions. Ensure your user/service account has at least 'list' access to namespaces.",
		}
	}

	return CheckResult{
		Name:    "cluster-connectivity",
		Status:  "pass",
		Message: fmt.Sprintf("Connected to cluster (context: %s)", contextName),
	}
}

func (r *KubernetesRuntime) checkNamespaceAccess(namespace string) CheckResult {
	ctx, cancel := r.diagnosticContext()
	defer cancel()

	_, err := r.Client.Clientset.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if k8serrors.IsForbidden(err) {
		// The Namespace object is cluster-scoped, so the namespaced Role the
		// Runtime Broker permissions docs recommend cannot read it. Verify
		// access within the namespace instead.
		return r.checkNamespaceAccessWithinNamespace(ctx, namespace, err)
	}
	if err != nil {
		return namespaceAccessFailure(namespace, err)
	}

	return CheckResult{
		Name:    "namespace-access",
		Status:  "pass",
		Message: fmt.Sprintf("Namespace %q exists and is accessible", namespace),
	}
}

// checkNamespaceAccessWithinNamespace is the namespace check when the
// Namespace object cannot be read: it passes if the broker may list pods in
// namespace, and otherwise fails as an unreadable namespace does.
func (r *KubernetesRuntime) checkNamespaceAccessWithinNamespace(ctx context.Context, namespace string, getErr error) CheckResult {
	review := &authv1.SelfSubjectAccessReview{
		Spec: authv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authv1.ResourceAttributes{
				Namespace: namespace,
				Verb:      "list",
				Resource:  "pods",
			},
		},
	}
	result, err := r.Client.Clientset.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
	if err != nil || !result.Status.Allowed {
		return namespaceAccessFailure(namespace, getErr)
	}
	return CheckResult{
		Name:    "namespace-access",
		Status:  "pass",
		Message: fmt.Sprintf("Namespace %q object not readable with namespaced permissions; access verified within the namespace", namespace),
	}
}

// namespaceAccessFailure is the failed namespace check for namespace.
func namespaceAccessFailure(namespace string, err error) CheckResult {
	return CheckResult{
		Name:        "namespace-access",
		Status:      "fail",
		Message:     fmt.Sprintf("Namespace %q is not accessible: %v", namespace, err),
		Remediation: fmt.Sprintf("Create the namespace with 'kubectl create namespace %s' or verify your permissions.", namespace),
	}
}

func (r *KubernetesRuntime) checkPodPermissions(namespace string) CheckResult {
	ctx, cancel := r.diagnosticContext()
	defer cancel()

	checks := []struct {
		verb     string
		required bool
	}{
		{"create", true},
		{"get", true},
		{"list", true},
		{"delete", true},
	}

	for _, c := range checks {
		review := &authv1.SelfSubjectAccessReview{
			Spec: authv1.SelfSubjectAccessReviewSpec{
				ResourceAttributes: &authv1.ResourceAttributes{
					Namespace: namespace,
					Verb:      c.verb,
					Resource:  "pods",
				},
			},
		}

		result, err := r.Client.Clientset.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
		if err != nil {
			return CheckResult{
				Name:        "pod-permissions",
				Status:      "warn",
				Message:     fmt.Sprintf("Could not verify pod %s permission in %q: %v", c.verb, namespace, err),
				Remediation: brokerRoleRemediation(namespace),
			}
		}
		if !result.Status.Allowed {
			return CheckResult{
				Name:        "pod-permissions",
				Status:      "fail",
				Message:     fmt.Sprintf("Missing pod %q permission in namespace %q", c.verb, namespace),
				Remediation: fmt.Sprintf("Grant %q permission on pods. %s", c.verb, brokerRoleRemediation(namespace)),
			}
		}
	}

	// Also check pods/exec for attach/sync
	execReview := &authv1.SelfSubjectAccessReview{
		Spec: authv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authv1.ResourceAttributes{
				Namespace:   namespace,
				Verb:        "create",
				Resource:    "pods",
				Subresource: "exec",
			},
		},
	}
	execResult, err := r.Client.Clientset.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, execReview, metav1.CreateOptions{})
	if err == nil && !execResult.Status.Allowed {
		return CheckResult{
			Name:        "pod-permissions",
			Status:      "fail",
			Message:     fmt.Sprintf("Missing pods/exec permission in namespace %q (required for attach and sync)", namespace),
			Remediation: `Grant "create" permission on pods/exec. ` + brokerRoleRemediation(namespace),
		}
	}

	return CheckResult{
		Name:    "pod-permissions",
		Status:  "pass",
		Message: fmt.Sprintf("Pod CRUD and exec permissions verified in namespace %q", namespace),
	}
}

func (r *KubernetesRuntime) checkSecretPermissions(namespace string) CheckResult {
	ctx, cancel := r.diagnosticContext()
	defer cancel()

	for _, verb := range []string{"create", "list", "delete"} {
		review := &authv1.SelfSubjectAccessReview{
			Spec: authv1.SelfSubjectAccessReviewSpec{
				ResourceAttributes: &authv1.ResourceAttributes{
					Namespace: namespace,
					Verb:      verb,
					Resource:  "secrets",
				},
			},
		}

		result, err := r.Client.Clientset.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
		if err != nil {
			return CheckResult{
				Name:        "secret-permissions",
				Status:      "warn",
				Message:     fmt.Sprintf("Could not verify secret %s permission in %q: %v", verb, namespace, err),
				Remediation: brokerRoleRemediation(namespace),
			}
		}
		if !result.Status.Allowed {
			return CheckResult{
				Name:        "secret-permissions",
				Status:      "fail",
				Message:     fmt.Sprintf("Missing secret %q permission in namespace %q", verb, namespace),
				Remediation: fmt.Sprintf("Grant %q permission on secrets. %s", verb, brokerRoleRemediation(namespace)),
			}
		}
	}

	return CheckResult{
		Name:    "secret-permissions",
		Status:  "pass",
		Message: fmt.Sprintf("Secret permissions verified in namespace %q", namespace),
	}
}

func (r *KubernetesRuntime) checkSecretProviderClassCRD() CheckResult {
	ctx, cancel := r.diagnosticContext()
	defer cancel()

	_, err := r.Client.Clientset.Discovery().ServerResourcesForGroupVersion("secrets-store.csi.x-k8s.io/v1")
	if err != nil {
		return CheckResult{
			Name:        "secretproviderclass-crd",
			Status:      "fail",
			Message:     "SecretProviderClass CRD (secrets-store.csi.x-k8s.io/v1) not found",
			Remediation: "Install the Secrets Store CSI Driver. See: https://secrets-store-csi-driver.sigs.k8s.io/getting-started/installation",
		}
	}

	// Verify the specific resource exists in the API group
	resources, err := r.Client.Clientset.Discovery().ServerResourcesForGroupVersion("secrets-store.csi.x-k8s.io/v1")
	if err == nil {
		found := false
		for _, r := range resources.APIResources {
			if r.Name == "secretproviderclasses" {
				found = true
				break
			}
		}
		if !found {
			return CheckResult{
				Name:        "secretproviderclass-crd",
				Status:      "fail",
				Message:     "SecretProviderClass resource not found in secrets-store.csi.x-k8s.io/v1 API group",
				Remediation: "The Secrets Store CSI Driver may be partially installed. Reinstall or upgrade it.",
			}
		}
	}

	// Try listing SPCs to verify access
	_, err = r.Client.ListSecretProviderClasses(ctx, r.DefaultNamespace, "")
	if err != nil {
		return CheckResult{
			Name:        "secretproviderclass-crd",
			Status:      "warn",
			Message:     fmt.Sprintf("SecretProviderClass CRD exists but listing failed: %v", err),
			Remediation: "Ensure your service account has RBAC permissions for SecretProviderClass resources.",
		}
	}

	return CheckResult{
		Name:    "secretproviderclass-crd",
		Status:  "pass",
		Message: "SecretProviderClass CRD is available and accessible",
	}
}

func (r *KubernetesRuntime) checkSecretsStoreCSIDriver(namespace string) CheckResult {
	ctx, cancel := r.diagnosticContext()
	defer cancel()

	// Check for CSIDriver resource
	_, err := r.Client.Clientset.StorageV1().CSIDrivers().Get(ctx, "secrets-store.csi.k8s.io", metav1.GetOptions{})
	if err != nil {
		return CheckResult{
			Name:        "secrets-store-csi-driver",
			Status:      "fail",
			Message:     "Secrets Store CSI driver (secrets-store.csi.k8s.io) not found",
			Remediation: "Install the Secrets Store CSI Driver with the GCP provider. See: https://secrets-store-csi-driver.sigs.k8s.io/getting-started/installation",
		}
	}

	return CheckResult{
		Name:    "secrets-store-csi-driver",
		Status:  "pass",
		Message: "Secrets Store CSI driver is installed",
	}
}

func (r *KubernetesRuntime) checkGCSFuseCSIDriver(namespace string) CheckResult {
	ctx, cancel := r.diagnosticContext()
	defer cancel()

	_, err := r.Client.Clientset.StorageV1().CSIDrivers().Get(ctx, "gcsfuse.csi.storage.gke.io", metav1.GetOptions{})
	if err != nil {
		return CheckResult{
			Name:        "gcsfuse-csi-driver",
			Status:      "warn",
			Message:     "GCS FUSE CSI driver (gcsfuse.csi.storage.gke.io) not found — GCS volume mounts will not work",
			Remediation: "Enable the GCS FUSE CSI driver on your GKE cluster. See: https://cloud.google.com/kubernetes-engine/docs/how-to/persistent-volumes/cloud-storage-fuse-csi-driver",
		}
	}

	return CheckResult{
		Name:    "gcsfuse-csi-driver",
		Status:  "pass",
		Message: "GCS FUSE CSI driver is installed",
	}
}
