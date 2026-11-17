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
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestConventionalKSAName(t *testing.T) {
	for email, want := range map[string]string{
		"scion-worker@hub-proj.iam.gserviceaccount.com":    "scion-worker",
		"  SCION-abc123@hub-proj.iam.gserviceaccount.com ": "scion-abc123",
	} {
		got, err := ConventionalKSAName(email)
		if err != nil || got != want {
			t.Errorf("ConventionalKSAName(%q) = %q, %v; want %q, nil", email, got, err, want)
		}
	}
	for _, bad := range []string{"", "no-domain", "@domain", "scion-worker@", "-bad@proj.iam.gserviceaccount.com", "bad.dot@proj.iam.gserviceaccount.com"} {
		if got, err := ConventionalKSAName(bad); err == nil {
			t.Errorf("ConventionalKSAName(%q) = %q, nil; want error", bad, got)
		}
	}
}

func TestEnsureConventionalServiceAccount_CreatesWithContractFields(t *testing.T) {
	const (
		ns  = "agents"
		gsa = "scion-worker@hub-proj.iam.gserviceaccount.com"
		ksa = "scion-worker"
	)
	client := fake.NewSimpleClientset()
	outcome, err := EnsureConventionalServiceAccount(context.Background(), client, ns, ksa, gsa)
	if err != nil {
		t.Fatalf("EnsureConventionalServiceAccount: %v", err)
	}
	if outcome != KSACreateCreated {
		t.Fatalf("outcome = %v, want %v", outcome, KSACreateCreated)
	}

	got, err := client.CoreV1().ServiceAccounts(ns).Get(context.Background(), ksa, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get created KSA: %v", err)
	}
	if got.Annotations[WorkloadIdentityGSAAnnotation] != gsa {
		t.Errorf("annotation %s = %q, want %q", WorkloadIdentityGSAAnnotation, got.Annotations[WorkloadIdentityGSAAnnotation], gsa)
	}
	if got.Annotations[CreatedForGSAAnnotation] != gsa {
		t.Errorf("annotation %s = %q, want %q", CreatedForGSAAnnotation, got.Annotations[CreatedForGSAAnnotation], gsa)
	}
	if got.Labels[ManagedByLabel] != ManagedByScion {
		t.Errorf("label %s = %q, want %q", ManagedByLabel, got.Labels[ManagedByLabel], ManagedByScion)
	}
	if got.AutomountServiceAccountToken == nil || *got.AutomountServiceAccountToken {
		t.Errorf("AutomountServiceAccountToken = %v, want false", got.AutomountServiceAccountToken)
	}
	if !ServiceAccountManagedForGSA(got, gsa) {
		t.Error("ServiceAccountManagedForGSA = false, want true")
	}
}

// AlreadyExists never modifies the existing ServiceAccount: it re-reads by
// name and classifies managed reuse, annotated-only reuse, and conflict.
func TestEnsureConventionalServiceAccount_AlreadyExistsNeverModifies(t *testing.T) {
	const (
		ns  = "agents"
		gsa = "scion-worker@hub-proj.iam.gserviceaccount.com"
		ksa = "scion-worker"
	)
	cases := []struct {
		name        string
		existing    *corev1.ServiceAccount
		wantOutcome KSACreateOutcome
		wantManaged bool
	}{
		{
			name: "concurrent scion create won race",
			existing: &corev1.ServiceAccount{
				ObjectMeta: metav1.ObjectMeta{
					Name:        ksa,
					Namespace:   ns,
					Labels:      map[string]string{ManagedByLabel: ManagedByScion},
					Annotations: map[string]string{WorkloadIdentityGSAAnnotation: " " + gsa + " "},
				},
			},
			wantOutcome: KSACreateReusedManaged,
			wantManaged: true,
		},
		{
			name: "annotated for same GSA without scion label",
			existing: &corev1.ServiceAccount{
				ObjectMeta: metav1.ObjectMeta{
					Name:        ksa,
					Namespace:   ns,
					Annotations: map[string]string{WorkloadIdentityGSAAnnotation: gsa},
				},
			},
			wantOutcome: KSACreateReusedAnnotated,
			wantManaged: false,
		},
		{
			name: "different GSA annotation conflicts",
			existing: &corev1.ServiceAccount{
				ObjectMeta: metav1.ObjectMeta{
					Name:        ksa,
					Namespace:   ns,
					Labels:      map[string]string{ManagedByLabel: ManagedByScion, "keep": "yes"},
					Annotations: map[string]string{WorkloadIdentityGSAAnnotation: "other@hub-proj.iam.gserviceaccount.com"},
				},
			},
			wantOutcome: KSACreateConflict,
			wantManaged: false,
		},
		{
			name: "unannotated KSA conflicts",
			existing: &corev1.ServiceAccount{
				ObjectMeta: metav1.ObjectMeta{
					Name:      ksa,
					Namespace: ns,
					Labels:    map[string]string{"keep": "yes"},
				},
			},
			wantOutcome: KSACreateConflict,
			wantManaged: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := fake.NewSimpleClientset(tc.existing.DeepCopy())
			outcome, err := EnsureConventionalServiceAccount(context.Background(), client, ns, ksa, gsa)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if outcome != tc.wantOutcome {
				t.Errorf("outcome = %v, want %v", outcome, tc.wantOutcome)
			}
			if got := ServiceAccountManagedForGSA(tc.existing, gsa); got != tc.wantManaged {
				t.Errorf("ServiceAccountManagedForGSA = %v, want %v", got, tc.wantManaged)
			}
			for _, action := range client.Actions() {
				verb := action.GetVerb()
				if verb == "update" || verb == "patch" || verb == "delete" {
					t.Errorf("existing ServiceAccount modified via %s", verb)
				}
			}
		})
	}
}

func TestEnsureConventionalServiceAccount_ForbiddenAndErrors(t *testing.T) {
	const (
		ns  = "agents"
		gsa = "scion-worker@hub-proj.iam.gserviceaccount.com"
		ksa = "scion-worker"
	)
	gr := schema.GroupResource{Resource: "serviceaccounts"}

	t.Run("create forbidden", func(t *testing.T) {
		client := fake.NewSimpleClientset()
		client.PrependReactor("create", "serviceaccounts", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
			return true, nil, apierrors.NewForbidden(gr, ksa, errors.New("denied"))
		})
		outcome, err := EnsureConventionalServiceAccount(context.Background(), client, ns, ksa, gsa)
		if outcome != KSACreateForbiddenCreate || !apierrors.IsForbidden(err) {
			t.Fatalf("got (%v, %v), want (%v, Forbidden)", outcome, err, KSACreateForbiddenCreate)
		}
	})

	t.Run("already exists then get forbidden", func(t *testing.T) {
		client := fake.NewSimpleClientset(&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: ksa, Namespace: ns}})
		client.PrependReactor("get", "serviceaccounts", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
			return true, nil, apierrors.NewForbidden(gr, ksa, errors.New("get denied"))
		})
		outcome, err := EnsureConventionalServiceAccount(context.Background(), client, ns, ksa, gsa)
		if outcome != KSACreateForbiddenGet || !apierrors.IsForbidden(err) {
			t.Fatalf("got (%v, %v), want (%v, Forbidden)", outcome, err, KSACreateForbiddenGet)
		}
	})

	t.Run("other API error", func(t *testing.T) {
		client := fake.NewSimpleClientset()
		client.PrependReactor("create", "serviceaccounts", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
			return true, nil, apierrors.NewInternalError(errors.New("boom"))
		})
		outcome, err := EnsureConventionalServiceAccount(context.Background(), client, ns, ksa, gsa)
		if outcome != KSACreateError || err == nil {
			t.Fatalf("got (%v, %v), want (%v, non-nil)", outcome, err, KSACreateError)
		}
	})
}
