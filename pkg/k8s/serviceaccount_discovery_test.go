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
	"errors"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func testKSA(namespace, name, gsa string) *corev1.ServiceAccount {
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
	if gsa != "" {
		sa.Annotations = map[string]string{WorkloadIdentityGSAAnnotation: gsa}
	}
	return sa
}

func TestFindServiceAccountsForGSA(t *testing.T) {
	const gsa = "worker@proj.iam.gserviceaccount.com"
	client := fake.NewClientset(
		testKSA("agents", "b-ksa", gsa),
		testKSA("agents", "a-ksa", " Worker@Proj.iam.gserviceaccount.com "),
		testKSA("agents", "other", "other@proj.iam.gserviceaccount.com"),
		testKSA("agents", "plain", ""),
		testKSA("elsewhere", "far-ksa", gsa),
	)
	got, err := FindServiceAccountsForGSA(context.Background(), client, "agents", gsa)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a-ksa", "b-ksa"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}

	got, err = FindServiceAccountsForGSA(context.Background(), client, "agents", "")
	if err != nil || len(got) != 0 {
		t.Errorf("empty GSA: got %v, %v; want no match", got, err)
	}

	got, err = FindServiceAccountsForGSA(context.Background(), client, "empty-ns", gsa)
	if err != nil || len(got) != 0 {
		t.Errorf("empty namespace: got %v, %v; want no match", got, err)
	}
}

func TestFindServiceAccountsForGSA_ListError(t *testing.T) {
	client := fake.NewClientset()
	client.PrependReactor("list", "serviceaccounts", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "serviceaccounts"}, "", errors.New("denied"))
	})
	_, err := FindServiceAccountsForGSA(context.Background(), client, "agents", "worker@proj.iam.gserviceaccount.com")
	if !apierrors.IsForbidden(err) {
		t.Errorf("err = %v, want the forbidden list error", err)
	}
}

func TestServiceAccountsByGSA(t *testing.T) {
	const gsa = "worker@proj.iam.gserviceaccount.com"
	client := fake.NewClientset(
		testKSA("agents", "b-ksa", gsa),
		testKSA("agents", "a-ksa", " Worker@Proj.iam.gserviceaccount.com "),
		testKSA("agents", "other", "other@proj.iam.gserviceaccount.com"),
		testKSA("agents", "plain", ""),
		testKSA("agents", "blank", "  "),
		testKSA("elsewhere", "far-ksa", gsa),
	)
	got, err := ServiceAccountsByGSA(context.Background(), client, "agents")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		gsa:                                  {"a-ksa", "b-ksa"},
		"other@proj.iam.gserviceaccount.com": {"other"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}

	client.PrependReactor("list", "serviceaccounts", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "serviceaccounts"}, "", errors.New("denied"))
	})
	if _, err := ServiceAccountsByGSA(context.Background(), client, "agents"); !apierrors.IsForbidden(err) {
		t.Errorf("err = %v, want the forbidden list error", err)
	}
}
