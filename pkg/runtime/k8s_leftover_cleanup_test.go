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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/k8s"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// Tests for CleanupAgentResources: removing an agent's per-agent objects
// when its pod was already deleted outside scion. The fixtures are built
// from names and labels only; no Secret carries or is checked for data.

var _ AgentResourceCleaner = (*KubernetesRuntime)(nil)

// seedAgentSecret creates an empty Secret with the label shape Run gives
// every per-agent object.
func seedAgentSecret(t *testing.T, rt *KubernetesRuntime, namespace, name, slug, projectID string) {
	t.Helper()
	_, err := rt.Client.Clientset.CoreV1().Secrets(namespace).Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    productionAgentLabels(slug, projectID),
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("seed Secret %s/%s: %v", namespace, name, err)
	}
}

func seedAgentSPC(t *testing.T, rt *KubernetesRuntime, namespace, name, slug, projectID string) {
	t.Helper()
	spc := &unstructured.Unstructured{}
	spc.SetGroupVersionKind(schema.GroupVersionKind{Group: "secrets-store.csi.x-k8s.io", Version: "v1", Kind: "SecretProviderClass"})
	spc.SetName(name)
	spc.SetNamespace(namespace)
	spc.SetLabels(productionAgentLabels(slug, projectID))
	if _, err := rt.Client.Dynamic().Resource(k8s.SecretProviderClassGVR).Namespace(namespace).Create(context.Background(), spc, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed SecretProviderClass %s/%s: %v", namespace, name, err)
	}
}

func seedPod(t *testing.T, rt *KubernetesRuntime, namespace, name string) {
	t.Helper()
	_, err := rt.Client.Clientset.CoreV1().Pods(namespace).Create(context.Background(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("seed pod %s/%s: %v", namespace, name, err)
	}
}

// newNonGKECleanupTestRuntime is a non-GKE runtime on a fake that can still
// serve SecretProviderClasses, so a stray SecretProviderClass call outside
// GKE mode fails an assertion rather than panicking in the fake client.
func newNonGKECleanupTestRuntime(t *testing.T) (*KubernetesRuntime, *k8sfake.Clientset, *fake.FakeDynamicClient) {
	t.Helper()
	rt, clientset, dynClient := newGKECleanupTestRuntime(t)
	rt.GKEMode = false
	return rt, clientset, dynClient
}

func secretExists(t *testing.T, rt *KubernetesRuntime, namespace, name string) bool {
	t.Helper()
	_, err := rt.Client.Clientset.CoreV1().Secrets(namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err == nil {
		return true
	}
	if !k8serrors.IsNotFound(err) {
		t.Fatalf("get Secret %s/%s: %v", namespace, name, err)
	}
	return false
}

func spcExists(t *testing.T, rt *KubernetesRuntime, namespace, name string) bool {
	t.Helper()
	_, err := rt.Client.Dynamic().Resource(k8s.SecretProviderClassGVR).Namespace(namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err == nil {
		return true
	}
	if !k8serrors.IsNotFound(err) {
		t.Fatalf("get SecretProviderClass %s/%s: %v", namespace, name, err)
	}
	return false
}

func TestCleanupAgentResources_PodGone_RemovesOnlyThisAgentsObjects(t *testing.T) {
	rt, _, _ := newGKECleanupTestRuntime(t)
	ctx := context.Background()

	// The target: agent "agent" in project p1, whose pod is gone.
	seedAgentSecret(t, rt, "default", "scion-agent-proj1--agent", "agent", "p1")
	seedAgentSecret(t, rt, "default", "scion-auth-proj1--agent", "agent", "p1")
	seedAgentSPC(t, rt, "default", "scion-agent-proj1--agent", "agent", "p1")

	// Same agent name in another project, also with its pod gone.
	seedAgentSecret(t, rt, "default", "scion-agent-proj2--agent", "agent", "p2")
	seedAgentSPC(t, rt, "default", "scion-agent-proj2--agent", "agent", "p2")
	// Another agent in the same project.
	seedAgentSecret(t, rt, "default", "scion-agent-proj1--other", "other", "p1")
	// A non-agent Secret that happens to carry the same labels.
	seedAgentSecret(t, rt, "default", "unrelated", "agent", "p1")

	if err := rt.CleanupAgentResources(ctx, "agent", "p1"); err != nil {
		t.Fatalf("CleanupAgentResources: %v", err)
	}

	for _, name := range []string{"scion-agent-proj1--agent", "scion-auth-proj1--agent"} {
		if secretExists(t, rt, "default", name) {
			t.Errorf("Secret %s should be removed when its pod is gone", name)
		}
	}
	if spcExists(t, rt, "default", "scion-agent-proj1--agent") {
		t.Error("SecretProviderClass scion-agent-proj1--agent should be removed when its pod is gone")
	}

	for _, name := range []string{"scion-agent-proj2--agent", "scion-agent-proj1--other", "unrelated"} {
		if !secretExists(t, rt, "default", name) {
			t.Errorf("Secret %s belongs to another agent or project and must be left alone", name)
		}
	}
	if !spcExists(t, rt, "default", "scion-agent-proj2--agent") {
		t.Error("another project's SecretProviderClass must be left alone")
	}
}

func TestCleanupAgentResources_PodPresent_LeavesObjects(t *testing.T) {
	// A pod still exists for the agent (for example a newer start). Its
	// objects belong to that pod and are removed by Delete with it.
	rt, _, _ := newGKECleanupTestRuntime(t)
	seedPod(t, rt, "default", "proj1--agent")
	seedAgentSecret(t, rt, "default", "scion-agent-proj1--agent", "agent", "p1")
	seedAgentSecret(t, rt, "default", "scion-auth-proj1--agent", "agent", "p1")
	seedAgentSPC(t, rt, "default", "scion-agent-proj1--agent", "agent", "p1")

	if err := rt.CleanupAgentResources(context.Background(), "agent", "p1"); err != nil {
		t.Fatalf("CleanupAgentResources: %v", err)
	}
	for _, name := range []string{"scion-agent-proj1--agent", "scion-auth-proj1--agent"} {
		if !secretExists(t, rt, "default", name) {
			t.Errorf("Secret %s of a pod that still exists must be left alone", name)
		}
	}
	if !spcExists(t, rt, "default", "scion-agent-proj1--agent") {
		t.Error("SecretProviderClass of a pod that still exists must be left alone")
	}
}

func TestCleanupAgentResources_NoProjectID_NoOp(t *testing.T) {
	rt, clientset, _ := newNonGKECleanupTestRuntime(t)
	seedAgentSecret(t, rt, "default", "scion-agent-agent", "agent", "")
	clientset.ClearActions()

	if err := rt.CleanupAgentResources(context.Background(), "agent", ""); err != nil {
		t.Fatalf("CleanupAgentResources: %v", err)
	}
	if n := len(clientset.Actions()); n != 0 {
		t.Errorf("expected no API calls without a project ID, got %d: %v", n, clientset.Actions())
	}
	if !secretExists(t, rt, "default", "scion-agent-agent") {
		t.Error("Secret must be left alone without a project ID")
	}
}

func TestCleanupAgentResources_DeleteNotFoundIsSuccess(t *testing.T) {
	// Another actor removes the Secret between the list and the delete.
	rt, clientset, _ := newNonGKECleanupTestRuntime(t)
	seedAgentSecret(t, rt, "default", "scion-agent-proj1--agent", "agent", "p1")
	clientset.PrependReactor("delete", "secrets", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		name := action.(k8stesting.DeleteAction).GetName()
		return true, nil, k8serrors.NewNotFound(corev1.Resource("secrets"), name)
	})

	if err := rt.CleanupAgentResources(context.Background(), "agent", "p1"); err != nil {
		t.Fatalf("NotFound on delete should count as success, got %v", err)
	}
}

func TestCleanupAgentResources_AllNamespaces(t *testing.T) {
	// With ListAllNamespaces the agent may have run outside the default
	// namespace; its objects are found and removed there, scoped by label.
	rt, _, _ := newNonGKECleanupTestRuntime(t)
	rt.ListAllNamespaces = true
	seedAgentSecret(t, rt, "team-a", "scion-agent-proj1--agent", "agent", "p1")
	seedAgentSecret(t, rt, "team-b", "scion-agent-proj2--agent", "agent", "p2")
	// The same agent's pod is still running in another namespace: only the
	// object whose own pod is gone is removed.
	seedPod(t, rt, "team-c", "proj1--agent")
	seedAgentSecret(t, rt, "team-c", "scion-agent-proj1--agent", "agent", "p1")

	if err := rt.CleanupAgentResources(context.Background(), "agent", "p1"); err != nil {
		t.Fatalf("CleanupAgentResources: %v", err)
	}
	if secretExists(t, rt, "team-a", "scion-agent-proj1--agent") {
		t.Error("Secret in team-a should be removed when its pod is gone")
	}
	if !secretExists(t, rt, "team-b", "scion-agent-proj2--agent") {
		t.Error("another project's Secret must be left alone")
	}
	if !secretExists(t, rt, "team-c", "scion-agent-proj1--agent") {
		t.Error("Secret whose pod still exists must be left alone")
	}
}

func TestDelete_PodPresent_StillRemovesSecretsAndPod(t *testing.T) {
	// The pod-present path is unchanged: Delete removes the pod and its
	// per-agent objects by name.
	rt, _, _ := newGKECleanupTestRuntime(t)
	seedPod(t, rt, "default", "proj1--agent")
	seedAgentSecret(t, rt, "default", "scion-agent-proj1--agent", "agent", "p1")
	seedAgentSecret(t, rt, "default", "scion-auth-proj1--agent", "agent", "p1")
	seedAgentSPC(t, rt, "default", "scion-agent-proj1--agent", "agent", "p1")

	if err := rt.Delete(context.Background(), RunRef{ID: "proj1--agent"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := rt.Client.Clientset.CoreV1().Pods("default").Get(context.Background(), "proj1--agent", metav1.GetOptions{}); !k8serrors.IsNotFound(err) {
		t.Errorf("pod should be deleted, got err=%v", err)
	}
	for _, name := range []string{"scion-agent-proj1--agent", "scion-auth-proj1--agent"} {
		if secretExists(t, rt, "default", name) {
			t.Errorf("Secret %s should be removed with its pod", name)
		}
	}
	if spcExists(t, rt, "default", "scion-agent-proj1--agent") {
		t.Error("SecretProviderClass should be removed with its pod")
	}
}

func TestCleanupAgentResources_PodLookupError_KeepsObjects(t *testing.T) {
	// Only NotFound proves the pod is gone. Any other pod lookup error must
	// keep the objects and be returned.
	cases := map[string]error{
		"timeout":   k8serrors.NewTimeoutError("pod get timed out", 1),
		"forbidden": k8serrors.NewForbidden(corev1.Resource("pods"), "proj1--agent", errors.New("denied")),
	}
	for name, getErr := range cases {
		t.Run(name, func(t *testing.T) {
			rt, clientset, _ := newGKECleanupTestRuntime(t)
			seedAgentSecret(t, rt, "default", "scion-agent-proj1--agent", "agent", "p1")
			seedAgentSecret(t, rt, "default", "scion-auth-proj1--agent", "agent", "p1")
			seedAgentSPC(t, rt, "default", "scion-agent-proj1--agent", "agent", "p1")
			clientset.PrependReactor("get", "pods", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
				return true, nil, getErr
			})

			err := rt.CleanupAgentResources(context.Background(), "agent", "p1")
			if err == nil {
				t.Fatal("expected the pod lookup error to be returned")
			}
			if !errors.Is(err, getErr) {
				t.Errorf("returned error %v does not wrap the pod lookup error", err)
			}
			for _, n := range []string{"scion-agent-proj1--agent", "scion-auth-proj1--agent"} {
				if !secretExists(t, rt, "default", n) {
					t.Errorf("Secret %s must be kept when the pod lookup fails", n)
				}
			}
			if !spcExists(t, rt, "default", "scion-agent-proj1--agent") {
				t.Error("SecretProviderClass must be kept when the pod lookup fails")
			}
		})
	}
}

func TestCleanupAgentResources_SecretListFailure_ReturnedAndSPCStillCleaned(t *testing.T) {
	rt, clientset, _ := newGKECleanupTestRuntime(t)
	seedAgentSPC(t, rt, "default", "scion-agent-proj1--agent", "agent", "p1")
	listErr := errors.New("list secrets failed")
	clientset.PrependReactor("list", "secrets", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, listErr
	})

	err := rt.CleanupAgentResources(context.Background(), "agent", "p1")
	if !errors.Is(err, listErr) {
		t.Fatalf("expected the list error to be returned, got %v", err)
	}
	if spcExists(t, rt, "default", "scion-agent-proj1--agent") {
		t.Error("a Secret list failure should not stop the SecretProviderClass cleanup")
	}
}

func TestCleanupAgentResources_NonGKE_NoSecretProviderClassCalls(t *testing.T) {
	rt, _, dynClient := newNonGKECleanupTestRuntime(t)
	seedAgentSecret(t, rt, "default", "scion-agent-proj1--agent", "agent", "p1")

	if err := rt.CleanupAgentResources(context.Background(), "agent", "p1"); err != nil {
		t.Fatalf("CleanupAgentResources: %v", err)
	}
	if n := len(dynClient.Actions()); n != 0 {
		t.Errorf("expected no SecretProviderClass calls outside GKE mode, got %d: %v", n, dynClient.Actions())
	}
	if secretExists(t, rt, "default", "scion-agent-proj1--agent") {
		t.Error("Secret should still be removed outside GKE mode")
	}
}

func TestCleanupAgentResources_SPCListFailure_ReturnedAndSecretsStillCleaned(t *testing.T) {
	rt, _, dynClient := newGKECleanupTestRuntime(t)
	seedAgentSecret(t, rt, "default", "scion-agent-proj1--agent", "agent", "p1")
	listErr := errors.New("list secretproviderclasses failed")
	dynClient.PrependReactor("list", "secretproviderclasses", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, listErr
	})

	err := rt.CleanupAgentResources(context.Background(), "agent", "p1")
	if !errors.Is(err, listErr) {
		t.Fatalf("expected the SecretProviderClass list error to be returned, got %v", err)
	}
	if secretExists(t, rt, "default", "scion-agent-proj1--agent") {
		t.Error("a SecretProviderClass list failure should not stop the Secret cleanup")
	}
}
