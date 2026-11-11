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
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// WorkloadIdentityGSAAnnotation is the ServiceAccount annotation GKE
// Workload Identity reads to bind a Kubernetes ServiceAccount to a GCP
// service account.
const WorkloadIdentityGSAAnnotation = "iam.gke.io/gcp-service-account"

// FindServiceAccountsForGSA lists the ServiceAccounts in namespace and
// returns, sorted, the names of those whose
// iam.gke.io/gcp-service-account annotation equals gsaEmail. The comparison
// ignores case and surrounding whitespace, since GCP service account emails
// are case-insensitive. It needs only list access to serviceaccounts in
// that namespace. A list error is returned as is, so callers can tell a
// forbidden error apart. An empty gsaEmail matches nothing.
func FindServiceAccountsForGSA(ctx context.Context, client kubernetes.Interface, namespace, gsaEmail string) ([]string, error) {
	want := strings.ToLower(strings.TrimSpace(gsaEmail))
	if want == "" {
		return nil, nil
	}
	list, err := client.CoreV1().ServiceAccounts(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var names []string
	for _, sa := range list.Items {
		if normalizeGSAAnnotation(sa.Annotations[WorkloadIdentityGSAAnnotation]) == want {
			names = append(names, sa.Name)
		}
	}
	sort.Strings(names)
	return names, nil
}

// ServiceAccountsByGSA lists the ServiceAccounts in namespace once and
// returns, for each GCP service account named by an
// iam.gke.io/gcp-service-account annotation, the sorted names of the
// ServiceAccounts annotated with it. Keys are compared and returned the way
// FindServiceAccountsForGSA compares them: lowercased, surrounding
// whitespace removed. ServiceAccounts with no or an empty annotation are
// left out. A list error is returned as is.
func ServiceAccountsByGSA(ctx context.Context, client kubernetes.Interface, namespace string) (map[string][]string, error) {
	list, err := client.CoreV1().ServiceAccounts(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, sa := range list.Items {
		gsa := normalizeGSAAnnotation(sa.Annotations[WorkloadIdentityGSAAnnotation])
		if gsa == "" {
			continue
		}
		out[gsa] = append(out[gsa], sa.Name)
	}
	for _, names := range out {
		sort.Strings(names)
	}
	return out, nil
}

func normalizeGSAAnnotation(v string) string {
	return strings.ToLower(strings.TrimSpace(v))
}
