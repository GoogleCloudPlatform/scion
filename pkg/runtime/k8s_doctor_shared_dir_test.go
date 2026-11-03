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
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/GoogleCloudPlatform/scion/pkg/k8s"
)

// sharedDirDoctorRuntime is a Kubernetes runtime on a fake clientset holding
// the agents namespace and objs.
func sharedDirDoctorRuntime(objs ...k8sruntime.Object) (*KubernetesRuntime, *k8sfake.Clientset) {
	objs = append(objs, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "agents"}})
	clientset := k8sfake.NewClientset(objs...)
	rt := NewKubernetesRuntime(k8s.NewTestClient(fake.NewSimpleDynamicClient(k8sruntime.NewScheme()), clientset))
	rt.DefaultNamespace = "agents"
	return rt, clientset
}

// sharedDirClaim is a per-dir shared-dir claim in namespace with class (nil
// for none) and phase.
func sharedDirClaim(namespace, name string, class *string, phase corev1.PersistentVolumeClaimPhase) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: map[string]string{sharedDirClaimLabel: "cache"}},
		Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: class},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: phase},
	}
}

// findCheck returns the named check of report, and whether it is present.
func findCheck(report DiagnosticReport, name string) (CheckResult, bool) {
	for _, c := range report.Checks {
		if c.Name == name {
			return c, true
		}
	}
	return CheckResult{}, false
}

func mustCheck(t *testing.T, report DiagnosticReport, name string) CheckResult {
	t.Helper()
	c, ok := findCheck(report, name)
	if !ok {
		t.Fatalf("check %q not in report: %+v", name, report.Checks)
	}
	return c
}

func TestDiagnosticsSharedDirClass_Missing(t *testing.T) {
	rt, _ := sharedDirDoctorRuntime()
	report := rt.RunDiagnostics(DiagnosticOpts{Namespace: "agents", SharedDirStorageClass: "rwx-files"})
	c := mustCheck(t, report, sharedDirStorageClassCheck)
	if c.Status != "fail" {
		t.Fatalf("status = %q, want fail: %+v", c.Status, c)
	}
	if !strings.Contains(c.Message, `"rwx-files"`) || !strings.Contains(c.Remediation, `"rwx-files"`) {
		t.Errorf("message and remediation must name the class: %+v", c)
	}
}

func TestDiagnosticsSharedDirClass_Present(t *testing.T) {
	rt, _ := sharedDirDoctorRuntime(&storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: "rwx-files"},
		Provisioner: "filestore.csi.storage.gke.io",
	})
	report := rt.RunDiagnostics(DiagnosticOpts{Namespace: "agents", SharedDirStorageClass: "rwx-files"})
	c := mustCheck(t, report, sharedDirStorageClassCheck)
	if c.Status != "pass" {
		t.Fatalf("status = %q, want pass: %+v", c.Status, c)
	}
	for _, want := range []string{`"rwx-files"`, "filestore.csi.storage.gke.io", "ReadWriteMany (not verified)"} {
		if !strings.Contains(c.Message, want) {
			t.Errorf("message %q does not contain %q", c.Message, want)
		}
	}
}

func TestDiagnosticsSharedDirClass_Forbidden(t *testing.T) {
	rt, clientset := sharedDirDoctorRuntime()
	clientset.PrependReactor("get", "storageclasses", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, k8serrors.NewForbidden(schema.GroupResource{Group: "storage.k8s.io", Resource: "storageclasses"}, "rwx-files", errors.New("no"))
	})
	report := rt.RunDiagnostics(DiagnosticOpts{Namespace: "agents", SharedDirStorageClass: "rwx-files"})
	c := mustCheck(t, report, sharedDirStorageClassCheck)
	if c.Status != "warn" {
		t.Fatalf("status = %q, want warn (never fail): %+v", c.Status, c)
	}
	if !strings.Contains(c.Message, "Could not verify") || !strings.Contains(c.Message, "cluster-scoped") {
		t.Errorf("message = %q", c.Message)
	}
}

func TestDiagnosticsSharedDirClass_GetError(t *testing.T) {
	rt, clientset := sharedDirDoctorRuntime()
	clientset.PrependReactor("get", "storageclasses", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, errors.New("connection reset")
	})
	report := rt.RunDiagnostics(DiagnosticOpts{Namespace: "agents", SharedDirStorageClass: "rwx-files"})
	if c := mustCheck(t, report, sharedDirStorageClassCheck); c.Status != "warn" {
		t.Fatalf("status = %q, want warn: %+v", c.Status, c)
	}
}

func TestDiagnosticsSharedDirClass_NotConfigured(t *testing.T) {
	rt, clientset := sharedDirDoctorRuntime()
	report := rt.RunDiagnostics(DiagnosticOpts{Namespace: "agents"})
	c := mustCheck(t, report, sharedDirStorageClassCheck)
	if c.Status != "info" {
		t.Fatalf("status = %q, want info: %+v", c.Status, c)
	}
	if !strings.Contains(c.Message, "refused") || !strings.Contains(c.Remediation, "server.project_storage backend nfs") {
		t.Errorf("check = %+v", c)
	}
	if _, ok := findCheck(report, sharedDirClaimClassesCheck); ok {
		t.Errorf("claim-classes check must not run without a configured class")
	}
	for _, a := range clientset.Actions() {
		if a.GetResource().Resource == "storageclasses" || a.GetResource().Resource == "persistentvolumeclaims" {
			t.Errorf("unexpected %s %s without a configured class", a.GetVerb(), a.GetResource().Resource)
		}
	}
}

func TestDiagnosticsSharedDirClass_SharedBackend(t *testing.T) {
	rt, clientset := sharedDirDoctorRuntime()
	report := rt.RunDiagnostics(DiagnosticOpts{Namespace: "agents", SharedDirStorageClass: "rwx-files", SharedDirStorageShared: true})
	if c := mustCheck(t, report, sharedDirStorageClassCheck); c.Status != "skip" {
		t.Fatalf("status = %q, want skip: %+v", c.Status, c)
	}
	if _, ok := findCheck(report, sharedDirClaimClassesCheck); ok {
		t.Errorf("claim-classes check must not run on a shared backend")
	}
	for _, a := range clientset.Actions() {
		if a.GetResource().Resource == "storageclasses" || a.GetResource().Resource == "persistentvolumeclaims" {
			t.Errorf("unexpected %s %s on a shared backend", a.GetVerb(), a.GetResource().Resource)
		}
	}
}

func TestDiagnosticsSharedDirClaimClasses_Mismatch(t *testing.T) {
	rt, clientset := sharedDirDoctorRuntime(
		sharedDirClaim("agents", "scion-shared-p-cache", strPtr("standard"), corev1.ClaimPending),
		sharedDirClaim("agents", "scion-shared-p-data", nil, corev1.ClaimBound),
		sharedDirClaim("agents", "scion-shared-p-ok", strPtr("rwx-files"), corev1.ClaimBound),
		// Another namespace, and an unlabelled claim: never listed.
		sharedDirClaim("other", "scion-shared-q-cache", strPtr("standard"), corev1.ClaimBound),
		&corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: "agents"},
			Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: strPtr("standard")},
		},
	)
	report := rt.RunDiagnostics(DiagnosticOpts{Namespace: "agents", SharedDirStorageClass: "rwx-files"})
	c := mustCheck(t, report, sharedDirClaimClassesCheck)
	if c.Status != "warn" {
		t.Fatalf("status = %q, want warn: %+v", c.Status, c)
	}
	for _, want := range []string{"2 shared-dir claims", "scion-shared-p-cache (class standard, Pending)", "scion-shared-p-data (class none)"} {
		if !strings.Contains(c.Message, want) {
			t.Errorf("message %q does not contain %q", c.Message, want)
		}
	}
	for _, unwanted := range []string{"scion-shared-p-ok", "scion-shared-q-cache", "unrelated"} {
		if strings.Contains(c.Message, unwanted) {
			t.Errorf("message %q must not name %q", c.Message, unwanted)
		}
	}
	for _, want := range []string{"template's or agent's kubernetes block", "another profile using this namespace", "only if its class is really wrong"} {
		if !strings.Contains(c.Remediation, want) {
			t.Errorf("remediation %q does not contain %q", c.Remediation, want)
		}
	}

	// Read-only and namespace-scoped: the claim check only lists, in the
	// diagnosed namespace, with the shared-dir label selector.
	for _, a := range clientset.Actions() {
		if a.GetResource().Resource != "persistentvolumeclaims" {
			continue
		}
		if a.GetVerb() != "list" {
			t.Errorf("claim check made a %s call; it must only list", a.GetVerb())
		}
		if a.GetNamespace() != "agents" {
			t.Errorf("claim list in namespace %q, want agents (never cluster-wide)", a.GetNamespace())
		}
		if l, ok := a.(k8stesting.ListAction); ok {
			if sel := l.GetListRestrictions().Labels.String(); sel != sharedDirClaimLabel {
				t.Errorf("claim list selector = %q, want %q", sel, sharedDirClaimLabel)
			}
		}
	}
}

func TestDiagnosticsSharedDirClaimClasses_ListCapped(t *testing.T) {
	// Inserted in reverse name order, so the sorted-order assertion below
	// does not depend on the order the fake clientset lists them in.
	var objs []k8sruntime.Object
	for i := sharedDirClaimClassesListMax + 2; i >= 0; i-- {
		objs = append(objs, sharedDirClaim("agents", fmt.Sprintf("scion-shared-p-d%02d", i), strPtr("standard"), corev1.ClaimBound))
	}
	rt, _ := sharedDirDoctorRuntime(objs...)
	report := rt.RunDiagnostics(DiagnosticOpts{Namespace: "agents", SharedDirStorageClass: "rwx-files"})
	c := mustCheck(t, report, sharedDirClaimClassesCheck)
	if got := strings.Count(c.Message, "(class standard)"); got != sharedDirClaimClassesListMax {
		t.Errorf("named %d claims, want %d: %q", got, sharedDirClaimClassesListMax, c.Message)
	}
	if !strings.Contains(c.Message, "and 3 more") || !strings.Contains(c.Message, "13 shared-dir claims") {
		t.Errorf("message = %q", c.Message)
	}
	// The first sharedDirClaimClassesListMax names in sorted order are named,
	// in that order; the rest are only counted.
	last := -1
	for i := 0; i < sharedDirClaimClassesListMax; i++ {
		name := fmt.Sprintf("scion-shared-p-d%02d (class standard)", i)
		at := strings.Index(c.Message, name)
		if at < 0 {
			t.Fatalf("message does not name %q: %q", name, c.Message)
		}
		if at <= last {
			t.Errorf("%q is out of sorted order in %q", name, c.Message)
		}
		last = at
	}
	for i := sharedDirClaimClassesListMax; i < sharedDirClaimClassesListMax+3; i++ {
		if name := fmt.Sprintf("scion-shared-p-d%02d", i); strings.Contains(c.Message, name) {
			t.Errorf("message names %q past the cap: %q", name, c.Message)
		}
	}
}

func TestDiagnosticsSharedDirClaimClasses_AllMatch(t *testing.T) {
	rt, _ := sharedDirDoctorRuntime(sharedDirClaim("agents", "scion-shared-p-cache", strPtr("rwx-files"), corev1.ClaimBound))
	report := rt.RunDiagnostics(DiagnosticOpts{Namespace: "agents", SharedDirStorageClass: "rwx-files"})
	c := mustCheck(t, report, sharedDirClaimClassesCheck)
	if c.Status != "pass" {
		t.Fatalf("status = %q, want pass: %+v", c.Status, c)
	}
	if want := `The 1 shared-dir claim in namespace "agents" uses StorageClass "rwx-files"`; c.Message != want {
		t.Errorf("message = %q, want %q", c.Message, want)
	}

	rt, _ = sharedDirDoctorRuntime(
		sharedDirClaim("agents", "scion-shared-p-cache", strPtr("rwx-files"), corev1.ClaimBound),
		sharedDirClaim("agents", "scion-shared-p-data", strPtr("rwx-files"), corev1.ClaimBound),
	)
	report = rt.RunDiagnostics(DiagnosticOpts{Namespace: "agents", SharedDirStorageClass: "rwx-files"})
	if c := mustCheck(t, report, sharedDirClaimClassesCheck); c.Message != `All 2 shared-dir claims in namespace "agents" use StorageClass "rwx-files"` {
		t.Errorf("message = %q", c.Message)
	}
}

func TestDiagnosticsSharedDirClaimClasses_NoClaims(t *testing.T) {
	rt, _ := sharedDirDoctorRuntime()
	report := rt.RunDiagnostics(DiagnosticOpts{Namespace: "agents", SharedDirStorageClass: "rwx-files"})
	c := mustCheck(t, report, sharedDirClaimClassesCheck)
	if c.Status != "pass" || c.Message != `No shared-dir claims in namespace "agents"` {
		t.Fatalf("check = %+v, want a pass saying there are no claims", c)
	}
}

func TestDiagnosticsSharedDirClaimClasses_SingleMismatch(t *testing.T) {
	rt, _ := sharedDirDoctorRuntime(sharedDirClaim("agents", "scion-shared-p-cache", strPtr("standard"), corev1.ClaimBound))
	report := rt.RunDiagnostics(DiagnosticOpts{Namespace: "agents", SharedDirStorageClass: "rwx-files"})
	c := mustCheck(t, report, sharedDirClaimClassesCheck)
	if !strings.HasPrefix(c.Message, `1 shared-dir claim in namespace "agents" uses a storage class other than "rwx-files": `) {
		t.Errorf("message = %q", c.Message)
	}
}

func TestDiagnosticsSharedDirClaimClasses_ListForbidden(t *testing.T) {
	rt, clientset := sharedDirDoctorRuntime()
	clientset.PrependReactor("list", "persistentvolumeclaims", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, k8serrors.NewForbidden(schema.GroupResource{Resource: "persistentvolumeclaims"}, "", errors.New("no"))
	})
	report := rt.RunDiagnostics(DiagnosticOpts{Namespace: "agents", SharedDirStorageClass: "rwx-files"})
	c := mustCheck(t, report, sharedDirClaimClassesCheck)
	if c.Status != "warn" || !strings.Contains(c.Message, "Could not list") {
		t.Fatalf("check = %+v, want a could-not-list warning (never fail)", c)
	}
	want := "To have doctor check existing claims, grant list on persistentvolumeclaims in this namespace; " +
		"the Runtime Broker also uses it, with delete, to release a deleted project's shared-dir claims"
	if c.Remediation != want {
		t.Errorf("remediation = %q, want %q", c.Remediation, want)
	}
}

func TestDiagnosticsSharedDirClaimClasses_ListError(t *testing.T) {
	rt, clientset := sharedDirDoctorRuntime()
	clientset.PrependReactor("list", "persistentvolumeclaims", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, errors.New("etcd timeout")
	})
	report := rt.RunDiagnostics(DiagnosticOpts{Namespace: "agents", SharedDirStorageClass: "rwx-files"})
	c := mustCheck(t, report, sharedDirClaimClassesCheck)
	if c.Status != "warn" || !strings.Contains(c.Message, "etcd timeout") {
		t.Fatalf("check = %+v, want a warning carrying the error (never fail)", c)
	}
}

func TestStorageClassMismatch(t *testing.T) {
	claim := func(class *string, phase corev1.PersistentVolumeClaimPhase) *corev1.PersistentVolumeClaim {
		return &corev1.PersistentVolumeClaim{Spec: corev1.PersistentVolumeClaimSpec{StorageClassName: class}, Status: corev1.PersistentVolumeClaimStatus{Phase: phase}}
	}
	for _, tc := range []struct {
		name         string
		pvc          *corev1.PersistentVolumeClaim
		requested    string
		wantExisting string
		wantPending  bool
		wantDiffers  bool
	}{
		{"nil claim", nil, "rwx", "", false, false},
		{"no requested class", claim(strPtr("standard"), corev1.ClaimBound), "", "", false, false},
		{"same class", claim(strPtr("rwx"), corev1.ClaimBound), "rwx", "rwx", false, false},
		{"other class, bound", claim(strPtr("standard"), corev1.ClaimBound), "rwx", "standard", false, true},
		{"other class, pending", claim(strPtr("standard"), corev1.ClaimPending), "rwx", "standard", true, true},
		{"no class on claim", claim(nil, corev1.ClaimBound), "rwx", "", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			existing, pending, differs := storageClassMismatch(tc.pvc, tc.requested)
			if existing != tc.wantExisting || pending != tc.wantPending || differs != tc.wantDiffers {
				t.Errorf("storageClassMismatch = (%q, %v, %v), want (%q, %v, %v)",
					existing, pending, differs, tc.wantExisting, tc.wantPending, tc.wantDiffers)
			}
		})
	}
}
