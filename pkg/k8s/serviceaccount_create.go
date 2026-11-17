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

package k8s

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
)

const (
	// ManagedByLabel and ManagedByScion mark Kubernetes objects Scion created.
	ManagedByLabel = "app.kubernetes.io/managed-by"
	ManagedByScion = "scion"

	// CreatedForGSAAnnotation records the GCP service account email for which
	// Scion originally created a conventional Kubernetes ServiceAccount.
	CreatedForGSAAnnotation = "scion.dev/created-for-gsa"
)

// KSACreateOutcome classifies the result of EnsureConventionalServiceAccount.
type KSACreateOutcome int

const (
	// KSACreateError indicates a non-Forbidden, non-AlreadyExists Kubernetes
	// API error during create or re-read.
	KSACreateError KSACreateOutcome = iota
	// KSACreateCreated indicates the conventional ServiceAccount was created.
	KSACreateCreated
	// KSACreateReusedManaged indicates Create returned AlreadyExists and a
	// re-read showed a Scion-managed ServiceAccount annotated for gsaEmail
	// (a concurrent start won the creation race).
	KSACreateReusedManaged
	// KSACreateReusedAnnotated indicates Create returned AlreadyExists and a
	// re-read showed an operator-managed ServiceAccount annotated for gsaEmail.
	KSACreateReusedAnnotated
	// KSACreateConflict indicates Create returned AlreadyExists and a re-read
	// showed an existing ServiceAccount that is not annotated for gsaEmail.
	KSACreateConflict
	// KSACreateForbiddenCreate indicates Create returned 403 Forbidden.
	KSACreateForbiddenCreate
	// KSACreateForbiddenGet indicates Create returned AlreadyExists and the
	// subsequent Get returned 403 Forbidden.
	KSACreateForbiddenGet
)

// ConventionalKSAName derives the conventional Kubernetes ServiceAccount name
// (<gsa-account-id>) from a GCP service account email and verifies that it is
// a valid DNS-1123 label.
func ConventionalKSAName(gsaEmail string) (string, error) {
	normalized := normalizeGSAAnnotation(gsaEmail)
	local, domain, ok := strings.Cut(normalized, "@")
	if !ok || local == "" || domain == "" || strings.Contains(domain, "@") {
		return "", fmt.Errorf("invalid GCP service account email %q", gsaEmail)
	}
	if errs := validation.IsDNS1123Label(local); len(errs) > 0 {
		return "", fmt.Errorf("invalid conventional Kubernetes ServiceAccount name %q: %s", local, strings.Join(errs, "; "))
	}
	return local, nil
}

// ServiceAccountHasGSAAnnotation reports whether sa carries an
// iam.gke.io/gcp-service-account annotation matching gsaEmail (ignoring case
// and surrounding whitespace).
func ServiceAccountHasGSAAnnotation(sa *corev1.ServiceAccount, gsaEmail string) bool {
	if sa == nil {
		return false
	}
	want := normalizeGSAAnnotation(gsaEmail)
	if want == "" {
		return false
	}
	return normalizeGSAAnnotation(sa.Annotations[WorkloadIdentityGSAAnnotation]) == want
}

// ServiceAccountManagedForGSA reports whether sa is labeled
// app.kubernetes.io/managed-by=scion AND annotated for gsaEmail.
func ServiceAccountManagedForGSA(sa *corev1.ServiceAccount, gsaEmail string) bool {
	return ServiceAccountHasGSAAnnotation(sa, gsaEmail) && sa.Labels[ManagedByLabel] == ManagedByScion
}

// EnsureConventionalServiceAccount creates the conventional ServiceAccount
// ksaName in namespace for gsaEmail, setting iam.gke.io/gcp-service-account,
// scion.dev/created-for-gsa, app.kubernetes.io/managed-by=scion, and
// automountServiceAccountToken=false.
//
// On AlreadyExists it never modifies the existing ServiceAccount: it re-reads
// ksaName via Get and classifies it as Scion-managed reuse, annotated reuse,
// or conflict. Takes (client, namespace, ksaName, gsaEmail) with no dependency
// on legacy discovery so both legacy and flat resolvers can reuse it.
func EnsureConventionalServiceAccount(
	ctx context.Context,
	client kubernetes.Interface,
	namespace, ksaName, gsaEmail string,
) (KSACreateOutcome, error) {
	gsa := normalizeGSAAnnotation(gsaEmail)
	automount := false
	desired := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ksaName,
			Namespace: namespace,
			Labels: map[string]string{
				ManagedByLabel: ManagedByScion,
			},
			Annotations: map[string]string{
				WorkloadIdentityGSAAnnotation: gsa,
				CreatedForGSAAnnotation:       gsa,
			},
		},
		AutomountServiceAccountToken: &automount,
	}
	_, err := client.CoreV1().ServiceAccounts(namespace).Create(ctx, desired, metav1.CreateOptions{})
	if err == nil {
		return KSACreateCreated, nil
	}
	if apierrors.IsForbidden(err) {
		return KSACreateForbiddenCreate, err
	}
	if !apierrors.IsAlreadyExists(err) {
		return KSACreateError, err
	}

	existing, getErr := client.CoreV1().ServiceAccounts(namespace).Get(ctx, ksaName, metav1.GetOptions{})
	if getErr != nil {
		if apierrors.IsForbidden(getErr) {
			return KSACreateForbiddenGet, getErr
		}
		return KSACreateError, getErr
	}
	if ServiceAccountManagedForGSA(existing, gsa) {
		return KSACreateReusedManaged, nil
	}
	if ServiceAccountHasGSAAnnotation(existing, gsa) {
		return KSACreateReusedAnnotated, nil
	}
	return KSACreateConflict, nil
}
