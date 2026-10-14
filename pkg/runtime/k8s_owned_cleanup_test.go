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
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
)

// Owner-scoped leftover cleanup for a flat Runtime Broker instance's agents
// (ptone/scion#3274).

var _ OwnedAgentResourceCleaner = (*KubernetesRuntime)(nil)

func seedLabelledSecret(t *testing.T, rt *KubernetesRuntime, name string, labels map[string]string) {
	t.Helper()
	if _, err := rt.Client.Clientset.CoreV1().Secrets("default").Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Labels: labels},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func withOwner(l map[string]string, owner string) map[string]string {
	out := map[string]string{}
	for k, v := range l {
		out[k] = v
	}
	out[api.LabelRuntimeBrokerID] = owner
	return out
}

func TestCleanupOwnedAgentResources_RemovesOnlyOwnObjects(t *testing.T) {
	rt, _, _ := newNonGKECleanupTestRuntime(t)
	base := productionAgentLabels("agent", "p1")
	seedLabelledSecret(t, rt, "scion-agent-proj1--agent", withOwner(base, "rb-a"))
	seedLabelledSecret(t, rt, "scion-auth-proj1--agent", withOwner(base, "rb-b")) // another instance's
	seedLabelledSecret(t, rt, "scion-env-proj1--agent", base)                     // unlabelled (no owner)

	if err := rt.CleanupOwnedAgentResources(context.Background(), "agent", "p1", "rb-a", ""); err != nil {
		t.Fatal(err)
	}
	if secretExists(t, rt, "default", "scion-agent-proj1--agent") {
		t.Error("the instance's own object was not removed")
	}
	if !secretExists(t, rt, "default", "scion-auth-proj1--agent") {
		t.Error("another instance's same-name, same-project object was removed")
	}
	if !secretExists(t, rt, "default", "scion-env-proj1--agent") {
		t.Error("an unlabelled object was removed")
	}
}

func TestCleanupOwnedAgentResources_EmptyOrInvalidIDDeletesNothing(t *testing.T) {
	for _, id := range []string{"", "not a label value!", strings.Repeat("x", 64)} {
		rt, clientset, _ := newNonGKECleanupTestRuntime(t)
		seedLabelledSecret(t, rt, "scion-agent-proj1--agent", withOwner(productionAgentLabels("agent", "p1"), "rb-a"))
		seedLabelledSecret(t, rt, "scion-auth-proj1--agent", productionAgentLabels("agent", "p1"))
		clientset.ClearActions()
		if err := rt.CleanupOwnedAgentResources(context.Background(), "agent", "p1", id, ""); err == nil {
			t.Errorf("id %q: want an error", id)
		}
		for _, a := range clientset.Actions() {
			if a.GetVerb() == "delete" || a.GetVerb() == "list" {
				t.Errorf("id %q: %s %s issued (no unscoped fallback)", id, a.GetVerb(), a.GetResource().Resource)
			}
		}
		if !secretExists(t, rt, "default", "scion-agent-proj1--agent") || !secretExists(t, rt, "default", "scion-auth-proj1--agent") {
			t.Errorf("id %q: an object was deleted", id)
		}
	}
}

// TestCleanupAgentResources_SelectorUnchanged: the legacy cleanup lists by
// exactly scion.name and the project ID; the owned variant adds only the
// owner label.
func TestCleanupAgentResources_SelectorUnchanged(t *testing.T) {
	selectors := func(run func(*KubernetesRuntime) error) []string {
		rt, clientset, _ := newNonGKECleanupTestRuntime(t)
		var got []string
		clientset.PrependReactor("list", "secrets", func(a k8stesting.Action) (bool, k8sruntime.Object, error) {
			got = append(got, a.(k8stesting.ListAction).GetListRestrictions().Labels.String())
			return false, nil, nil
		})
		if err := run(rt); err != nil {
			t.Fatal(err)
		}
		return got
	}
	legacy := selectors(func(rt *KubernetesRuntime) error {
		return rt.CleanupAgentResources(context.Background(), "agent", "p1", "")
	})
	if want := "scion.name=agent," + projectkeys.LabelProjectID + "=p1"; len(legacy) != 1 || legacy[0] != want {
		t.Fatalf("legacy selector = %v, want [%s]", legacy, want)
	}
	owned := selectors(func(rt *KubernetesRuntime) error {
		return rt.CleanupOwnedAgentResources(context.Background(), "agent", "p1", "rb-a", "")
	})
	if want := "scion.name=agent," + projectkeys.LabelProjectID + "=p1," + api.LabelRuntimeBrokerID + "=rb-a"; len(owned) != 1 || owned[0] != want {
		t.Fatalf("owned selector = %v, want [%s]", owned, want)
	}
}

// TestK8sRun_ChildSecretsCarryOwnerLabel: the per-agent Secrets Run creates
// carry the reserved owner label from the run's labels, so the owner-scoped
// cleanup can find them.
func TestK8sRun_ChildSecretsCarryOwnerLabel(t *testing.T) {
	rt, clientset, _ := newTestK8sRuntime()
	var fx uidFixture
	fx.install(t, clientset)
	var mu sync.Mutex
	var created []map[string]string
	clientset.PrependReactor("create", "secrets", func(a k8stesting.Action) (bool, k8sruntime.Object, error) {
		mu.Lock()
		created = append(created, a.(k8stesting.CreateAction).GetObject().(*corev1.Secret).Labels)
		mu.Unlock()
		return false, nil, nil
	})
	clientset.PrependReactor("create", "pods", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, errors.New("admission denied")
	})
	config := hookTestConfig("owned-agent")
	config.Labels[api.LabelRuntimeBrokerID] = "rb-a"
	config.Labels[projectkeys.LabelProjectID] = "p1"
	config.ResolvedSecrets = envSecrets(1)
	config.ResolvedAuth = authFiles(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, _ = rt.Run(ctx, config)
	mu.Lock()
	defer mu.Unlock()
	if len(created) == 0 {
		t.Fatal("Run created no Secret")
	}
	for _, l := range created {
		if l[api.LabelRuntimeBrokerID] != "rb-a" {
			t.Errorf("a Run-created Secret lacks the owner label: %v", l)
		}
	}
}

// seedOwnedAgentObjectsForRun is seedAgentObjectsForRun with every object
// also carrying the owner label of the flat instance owner.
func seedOwnedAgentObjectsForRun(t *testing.T, rt *KubernetesRuntime, runID, owner string) {
	t.Helper()
	seedAgentObjectsForRun(t, rt, runID)
	for _, name := range []string{"scion-agent-proj1--agent", "scion-auth-proj1--agent"} {
		s, err := rt.Client.Clientset.CoreV1().Secrets("default").Get(context.Background(), name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		s.Labels = withOwner(s.Labels, owner)
		if _, err := rt.Client.Clientset.CoreV1().Secrets("default").Update(context.Background(), s, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
}

// The owned cleanup scopes by run exactly as the unscoped one
// (TestCleanupAgentResources_RunScoped): a stale run leaves a newer run's
// objects, its own run's and legacy run-less objects are removed, and an
// empty runID removes by name, project and owner, as before.
func TestCleanupOwnedAgentResources_RunScoped(t *testing.T) {
	for _, tc := range []struct {
		name       string
		objectsRun string
		cleanupRun string
		wantKept   bool
	}{
		{"stale run leaves a newer run's objects", "run-b", "run-a", true},
		{"own run's objects are removed", "run-a", "run-a", false},
		{"legacy run-less objects are removed", "", "run-a", false},
		{"no run removes by name, as on the unscoped path", "run-b", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt, _, _ := newNonGKECleanupTestRuntime(t)
			seedOwnedAgentObjectsForRun(t, rt, tc.objectsRun, "rb-a")
			if err := rt.CleanupOwnedAgentResources(context.Background(), "agent", "p1", "rb-a", tc.cleanupRun); err != nil {
				t.Fatalf("CleanupOwnedAgentResources: %v", err)
			}
			for _, name := range []string{"scion-agent-proj1--agent", "scion-auth-proj1--agent"} {
				if got := secretExists(t, rt, "default", name); got != tc.wantKept {
					t.Errorf("Secret %s exists = %v, want %v", name, got, tc.wantKept)
				}
			}
		})
	}
}

// TestCleanupOwnedAgentResources_RunScopedSparesAnotherInstance: an object
// of another instance carrying the named run is never removed.
func TestCleanupOwnedAgentResources_RunScopedSparesAnotherInstance(t *testing.T) {
	rt, _, _ := newNonGKECleanupTestRuntime(t)
	seedOwnedAgentObjectsForRun(t, rt, "run-a", "rb-b")
	if err := rt.CleanupOwnedAgentResources(context.Background(), "agent", "p1", "rb-a", "run-a"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"scion-agent-proj1--agent", "scion-auth-proj1--agent"} {
		if !secretExists(t, rt, "default", name) {
			t.Errorf("another instance's %s was removed", name)
		}
	}
}

// TestCleanupOwnedAgentResources_InvalidRunID: an invalid run ID is refused
// on the owned path before anything is listed or deleted.
func TestCleanupOwnedAgentResources_InvalidRunID(t *testing.T) {
	rt, clientset, _ := newNonGKECleanupTestRuntime(t)
	seedOwnedAgentObjectsForRun(t, rt, "", "rb-a")
	clientset.ClearActions()
	if err := rt.CleanupOwnedAgentResources(context.Background(), "agent", "p1", "rb-a", "run-a,!x"); err == nil {
		t.Fatal("expected an error for an invalid run ID")
	}
	for _, a := range clientset.Actions() {
		if a.GetVerb() == "delete" || a.GetVerb() == "list" {
			t.Errorf("%s %s issued for an invalid run ID", a.GetVerb(), a.GetResource().Resource)
		}
	}
	if !secretExists(t, rt, "default", "scion-agent-proj1--agent") {
		t.Error("an invalid run ID removed an object")
	}
}
