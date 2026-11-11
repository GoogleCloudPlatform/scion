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
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/k8s"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
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

// These tests exercise the agent-delete Secret/SecretProviderClass cleanup
// path (cleanupAgentSecrets) using the SAME label shape Run() actually
// produces in production (pkg/agent/run.go): scion.agent is
// always "true" on the pod (and therefore on every per-agent object, via the
// generic scion.* label copy in createAgentSecret/createSecretProviderClass/
// createAuthFileSecret), and scion.name carries the bare agent slug, not the
// project-qualified name. Earlier tests in k8s_secrets_test.go only ever set
// scion.name in their input labels, so they never reproduced the label
// collision that made the old scion.agent=<name> selector never match.

// productionAgentLabels returns the label map Run() would actually attach to
// the pod (and, by the scion.* copy-through, to its per-agent Secrets/SPC)
// for an agent with the given bare slug and project ID.
func productionAgentLabels(bareName, projectID string) map[string]string {
	l := map[string]string{
		"scion.agent": "true", // the real pod marker label that clobbers the old scion.agent=<name> selector
		"scion.name":  bareName,
	}
	if projectID != "" {
		l[projectkeys.LabelProjectID] = projectID
	}
	return l
}

// newGKECleanupTestRuntime returns a K8s runtime in GKE mode backed by fake
// clients that understand the SecretProviderClass GVR.
func newGKECleanupTestRuntime(t *testing.T) (*KubernetesRuntime, *k8sfake.Clientset, *fake.FakeDynamicClient) {
	t.Helper()
	clientset := k8sfake.NewClientset()
	scheme := k8sruntime.NewScheme()
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "secrets-store.csi.x-k8s.io", Version: "v1", Kind: "SecretProviderClass"},
		&k8sruntime.Unknown{},
	)
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "secrets-store.csi.x-k8s.io", Version: "v1", Kind: "SecretProviderClassList"},
		&k8sruntime.Unknown{},
	)
	dynClient := fake.NewSimpleDynamicClient(scheme)
	client := k8s.NewTestClient(dynClient, clientset)
	rt := NewKubernetesRuntime(client)
	rt.GKEMode = true
	return rt, clientset, dynClient
}

// failPodReadiness makes every pod Get on clientset fail with a plain
// (non-context) error, so Run's waitForPodReady returns at its first poll
// instead of polling for up to 10 minutes against a fake API server that
// never reports the pod Ready. A plain error, rather than cancelling Run's
// context, models a start that failed but was not abandoned, so Run keeps
// the pod and its Secrets for the test to inspect (an abandoned start
// removes them; see TestRun_CancelledWhilePending_RemovesPodAndSecrets).
func failPodReadiness(clientset *k8sfake.Clientset) {
	clientset.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, fmt.Errorf("simulated readiness failure")
	})
}

// runUntilPodSubmitted drives Run(config) until it submits the pod Create
// call and then fails readiness (see failPodReadiness). Returns the pod as
// submitted.
func runUntilPodSubmitted(t *testing.T, rt *KubernetesRuntime, clientset *k8sfake.Clientset, config RunConfig) *corev1.Pod {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var (
		mu  sync.Mutex
		pod *corev1.Pod
	)
	clientset.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		p := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		mu.Lock()
		pod = p.DeepCopy()
		mu.Unlock()
		return false, nil, nil // let the default reactor actually store the pod
	})
	failPodReadiness(clientset)

	_, _ = rt.Run(ctx, config)

	mu.Lock()
	defer mu.Unlock()
	if pod == nil {
		t.Fatal("Run did not submit a pod")
	}
	return pod
}

// --- cleanupAgentSecrets: deterministic-name deletion ---

func TestCleanupAgentSecrets_DeletesAllObjectKinds(t *testing.T) {
	// Covers every object cleanupAgentSecrets must remove: the main agent
	// Secret, the ResolvedAuth Secret, and the SecretProviderClass (GKE
	// mode). These are the only per-agent Secret/SPC objects this runtime
	// itself ever creates (createAgentSecret, createSecretProviderClass,
	// createAuthFileSecret), so deleting exactly these three leaves nothing
	// of its own making behind.
	rt, clientset, dynClient := newGKECleanupTestRuntime(t)
	ctx := context.Background()
	agentName := "proj1--agent"
	labels := productionAgentLabels("agent", "proj1")

	secrets := []api.ResolvedSecret{
		{Name: "API_KEY", Type: "environment", Target: "API_KEY", Value: "sk-123", Source: "user", Ref: "projects/p/secrets/api-key"},
	}
	if _, err := rt.createSecretProviderClass(ctx, "default", agentName, secrets, labels); err != nil {
		t.Fatalf("createSecretProviderClass failed: %v", err)
	}
	if _, err := rt.createAgentSecret(ctx, "default", agentName, secrets, labels); err != nil {
		t.Fatalf("createAgentSecret failed: %v", err)
	}
	if err := rt.createAuthFileSecret(ctx, "default", agentName, nil, labels); err != nil {
		t.Fatalf("createAuthFileSecret failed: %v", err)
	}

	names := []string{
		fmt.Sprintf("scion-agent-%s", agentName),
		fmt.Sprintf("scion-auth-%s", agentName),
	}
	for _, n := range names {
		if _, err := clientset.CoreV1().Secrets("default").Get(ctx, n, metav1.GetOptions{}); err != nil {
			t.Fatalf("precondition: Secret %s should exist before cleanup: %v", n, err)
		}
	}
	spcName := fmt.Sprintf("scion-agent-%s", agentName)
	if _, err := dynClient.Resource(k8s.SecretProviderClassGVR).Namespace("default").Get(ctx, spcName, metav1.GetOptions{}); err != nil {
		t.Fatalf("precondition: SPC %s should exist before cleanup: %v", spcName, err)
	}

	rt.cleanupAgentSecrets(ctx, "default", agentName)

	for _, n := range names {
		if _, err := clientset.CoreV1().Secrets("default").Get(ctx, n, metav1.GetOptions{}); !k8serrors.IsNotFound(err) {
			t.Errorf("Secret %s should be deleted after cleanup, got err=%v", n, err)
		}
	}
	if _, err := dynClient.Resource(k8s.SecretProviderClassGVR).Namespace("default").Get(ctx, spcName, metav1.GetOptions{}); !k8serrors.IsNotFound(err) {
		t.Errorf("SPC %s should be deleted after cleanup, got err=%v", spcName, err)
	}
}

func TestCleanupAgentSecrets_DoesNotCrossAgentOnEnvSuffix(t *testing.T) {
	// A slug may itself end in "-env" (an agent literally named
	// "<agent>-env" is a valid, distinct agent). cleanupAgentSecrets must
	// not derive a "scion-agent-<name>-env" name to defensively probe for a
	// CSI-synced Secret, because that guessed name is exactly this other
	// agent's own real, deterministic Secret name. Cleaning up "agent" must
	// leave "agent-env" untouched.
	rt, clientset, _ := newTestK8sRuntime()
	ctx := context.Background()

	otherName := "proj1--agent-env"
	targetName := "proj1--agent"

	secrets := []api.ResolvedSecret{{Name: "KEY", Type: "environment", Target: "KEY", Value: "keep-value", Source: "user"}}
	if _, err := rt.createAgentSecret(ctx, "default", otherName, secrets, productionAgentLabels("agent-env", "proj1")); err != nil {
		t.Fatalf("createAgentSecret (other agent) failed: %v", err)
	}
	if _, err := rt.createAgentSecret(ctx, "default", targetName, secrets, productionAgentLabels("agent", "proj1")); err != nil {
		t.Fatalf("createAgentSecret (target) failed: %v", err)
	}

	rt.cleanupAgentSecrets(ctx, "default", targetName)

	otherSecret, err := clientset.CoreV1().Secrets("default").Get(ctx, fmt.Sprintf("scion-agent-%s", otherName), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("agent-env's own Secret should NOT be touched by cleaning up agent: %v", err)
	}
	if string(otherSecret.Data["KEY"]) != "keep-value" {
		t.Errorf("agent-env's Secret data should be untouched, got %q", string(otherSecret.Data["KEY"]))
	}

	if _, err := clientset.CoreV1().Secrets("default").Get(ctx, fmt.Sprintf("scion-agent-%s", targetName), metav1.GetOptions{}); !k8serrors.IsNotFound(err) {
		t.Errorf("agent's own Secret should be deleted, got err=%v", err)
	}
}

func TestCleanupAgentSecrets_DoesNotTouchOtherAgentOrProject(t *testing.T) {
	// Two agents share a bare name ("agent") across two different projects
	// in the same namespace — reproducing exactly the scenario the old
	// scion.agent/scion.name label selector could not safely disambiguate.
	// cleanupAgentSecrets must only ever touch the exact deterministic name
	// it is given.
	rt, clientset, _ := newTestK8sRuntime()
	ctx := context.Background()

	proj1Name := "proj1--agent"
	proj2Name := "proj2--agent"

	secrets1 := []api.ResolvedSecret{{Name: "KEY", Type: "environment", Target: "KEY", Value: "proj1-value", Source: "user"}}
	secrets2 := []api.ResolvedSecret{{Name: "KEY", Type: "environment", Target: "KEY", Value: "proj2-value", Source: "user"}}

	if _, err := rt.createAgentSecret(ctx, "default", proj1Name, secrets1, productionAgentLabels("agent", "proj1")); err != nil {
		t.Fatalf("createAgentSecret (proj1) failed: %v", err)
	}
	if _, err := rt.createAgentSecret(ctx, "default", proj2Name, secrets2, productionAgentLabels("agent", "proj2")); err != nil {
		t.Fatalf("createAgentSecret (proj2) failed: %v", err)
	}

	rt.cleanupAgentSecrets(ctx, "default", proj1Name)

	if _, err := clientset.CoreV1().Secrets("default").Get(ctx, fmt.Sprintf("scion-agent-%s", proj1Name), metav1.GetOptions{}); !k8serrors.IsNotFound(err) {
		t.Errorf("proj1's Secret should be deleted, got err=%v", err)
	}

	proj2Secret, err := clientset.CoreV1().Secrets("default").Get(ctx, fmt.Sprintf("scion-agent-%s", proj2Name), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("proj2's Secret should NOT be touched by proj1's cleanup: %v", err)
	}
	if string(proj2Secret.Data["KEY"]) != "proj2-value" {
		t.Errorf("proj2's Secret data should be untouched, got %q", string(proj2Secret.Data["KEY"]))
	}
}

func TestDelete_PodGone_CrossProjectSafety(t *testing.T) {
	// Gate scenario: the pod is already gone (force-deleted/evicted) for
	// BOTH agents, and they share a bare name across two projects. Delete
	// must still only remove the targeted project's objects.
	rt, clientset, _ := newTestK8sRuntime()
	ctx := context.Background()

	proj1Name := "proj1--agent"
	proj2Name := "proj2--agent"

	secrets := []api.ResolvedSecret{{Name: "KEY", Type: "environment", Target: "KEY", Value: "val", Source: "user"}}
	if _, err := rt.createAgentSecret(ctx, "default", proj1Name, secrets, productionAgentLabels("agent", "proj1")); err != nil {
		t.Fatalf("createAgentSecret (proj1) failed: %v", err)
	}
	if _, err := rt.createAgentSecret(ctx, "default", proj2Name, secrets, productionAgentLabels("agent", "proj2")); err != nil {
		t.Fatalf("createAgentSecret (proj2) failed: %v", err)
	}

	// No pod exists for either agent (simulating a previously force-deleted
	// or evicted agent). Delete must not error and must not cross projects.
	if err := rt.Delete(ctx, RunRef{ID: proj1Name}); err != nil {
		t.Fatalf("Delete should succeed when the pod is already gone: %v", err)
	}

	if _, err := clientset.CoreV1().Secrets("default").Get(ctx, fmt.Sprintf("scion-agent-%s", proj1Name), metav1.GetOptions{}); !k8serrors.IsNotFound(err) {
		t.Errorf("proj1's Secret should be deleted, got err=%v", err)
	}
	if _, err := clientset.CoreV1().Secrets("default").Get(ctx, fmt.Sprintf("scion-agent-%s", proj2Name), metav1.GetOptions{}); err != nil {
		t.Errorf("proj2's Secret should NOT be touched by proj1's Delete: %v", err)
	}
}

func TestDelete_DoesNotCrossAgentOnEnvSuffix(t *testing.T) {
	// Same scenario as TestCleanupAgentSecrets_DoesNotCrossAgentOnEnvSuffix,
	// but through Delete (scion rm/scion stop) with the pod already gone —
	// the gate scenario reported against the agent, not the helper directly.
	rt, clientset, _ := newTestK8sRuntime()
	ctx := context.Background()

	otherName := "proj1--agent-env"
	targetName := "proj1--agent"

	secrets := []api.ResolvedSecret{{Name: "KEY", Type: "environment", Target: "KEY", Value: "keep-value", Source: "user"}}
	if _, err := rt.createAgentSecret(ctx, "default", otherName, secrets, productionAgentLabels("agent-env", "proj1")); err != nil {
		t.Fatalf("createAgentSecret (other agent) failed: %v", err)
	}
	if _, err := rt.createAgentSecret(ctx, "default", targetName, secrets, productionAgentLabels("agent", "proj1")); err != nil {
		t.Fatalf("createAgentSecret (target) failed: %v", err)
	}

	if err := rt.Delete(ctx, RunRef{ID: targetName}); err != nil {
		t.Fatalf("Delete should succeed when the pod is already gone: %v", err)
	}

	otherSecret, err := clientset.CoreV1().Secrets("default").Get(ctx, fmt.Sprintf("scion-agent-%s", otherName), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("agent-env's own Secret should NOT be touched by deleting agent: %v", err)
	}
	if string(otherSecret.Data["KEY"]) != "keep-value" {
		t.Errorf("agent-env's Secret data should be untouched, got %q", string(otherSecret.Data["KEY"]))
	}
}

// --- Run(): per-agent Secret/SPC cleanup ---

func TestRun_Delete_RoundTrip_NoSecretsLeftBehind(t *testing.T) {
	rt, clientset, _ := newTestK8sRuntime()
	agentName := "proj1--agent"
	config := RunConfig{
		Name:         agentName,
		Image:        "test:latest",
		UnixUsername: "scion",
		ProjectID:    "proj1",
		Labels:       productionAgentLabels("agent", "proj1"),
		ResolvedSecrets: []api.ResolvedSecret{
			{Name: "API_KEY", Type: "environment", Target: "API_KEY", Value: "sk-123", Source: "user"},
		},
		ResolvedAuth: &api.ResolvedAuth{Files: []api.FileMapping{{SourcePath: "", ContainerPath: "~/.config/x"}}},
	}

	runUntilPodSubmitted(t, rt, clientset, config)

	secretName := fmt.Sprintf("scion-agent-%s", agentName)
	authSecretName := fmt.Sprintf("scion-auth-%s", agentName)
	if _, err := clientset.CoreV1().Secrets("default").Get(context.Background(), secretName, metav1.GetOptions{}); err != nil {
		t.Fatalf("precondition: agent Secret should exist after Run: %v", err)
	}
	if _, err := clientset.CoreV1().Secrets("default").Get(context.Background(), authSecretName, metav1.GetOptions{}); err != nil {
		t.Fatalf("precondition: auth Secret should exist after Run: %v", err)
	}

	if err := rt.Delete(context.Background(), RunRef{ID: agentName}); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	if _, err := clientset.CoreV1().Secrets("default").Get(context.Background(), secretName, metav1.GetOptions{}); !k8serrors.IsNotFound(err) {
		t.Errorf("agent Secret should be gone after Delete, got err=%v", err)
	}
	if _, err := clientset.CoreV1().Secrets("default").Get(context.Background(), authSecretName, metav1.GetOptions{}); !k8serrors.IsNotFound(err) {
		t.Errorf("auth Secret should be gone after Delete, got err=%v", err)
	}
}

func TestRun_Delete_RoundTrip_GKE_SPCCleanedUp(t *testing.T) {
	rt, clientset, dynClient := newGKECleanupTestRuntime(t)
	agentName := "proj1--agent"
	config := RunConfig{
		Name:         agentName,
		Image:        "test:latest",
		UnixUsername: "scion",
		ProjectID:    "proj1",
		Labels:       productionAgentLabels("agent", "proj1"),
		ResolvedSecrets: []api.ResolvedSecret{
			{Name: "API_KEY", Type: "environment", Target: "API_KEY", Value: "sk-123", Source: "user", Ref: "projects/p/secrets/api-key"},
		},
	}

	runUntilPodSubmitted(t, rt, clientset, config)

	spcName := fmt.Sprintf("scion-agent-%s", agentName)
	if _, err := dynClient.Resource(k8s.SecretProviderClassGVR).Namespace("default").Get(context.Background(), spcName, metav1.GetOptions{}); err != nil {
		t.Fatalf("precondition: SPC should exist after Run: %v", err)
	}

	if err := rt.Delete(context.Background(), RunRef{ID: agentName}); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	if _, err := dynClient.Resource(k8s.SecretProviderClassGVR).Namespace("default").Get(context.Background(), spcName, metav1.GetOptions{}); !k8serrors.IsNotFound(err) {
		t.Errorf("SPC should be gone after Delete, got err=%v", err)
	}
}

func TestRun_PreClean_RemovesStaleSecretsFromPriorAgent(t *testing.T) {
	// Covers the Run pre-clean call site (the cleanupAgentSecrets call at the
	// top of Run, before cleanupStalePod): a stale scion-auth-<name> Secret
	// left behind by a force-deleted or evicted prior agent must be removed
	// even when the new Run does not recreate that object (no ResolvedAuth
	// here), and likewise for a stale SecretProviderClass in GKE mode.
	rt, clientset, dynClient := newGKECleanupTestRuntime(t)
	agentName := "proj1--agent"

	staleAuthName := fmt.Sprintf("scion-auth-%s", agentName)
	staleAuth := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: staleAuthName, Namespace: "default", Labels: productionAgentLabels("agent", "proj1")},
		Data:       map[string][]byte{"auth-file-0": []byte("stale")},
	}
	if _, err := clientset.CoreV1().Secrets("default").Create(context.Background(), staleAuth, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to seed stale auth secret: %v", err)
	}

	staleSPCName := fmt.Sprintf("scion-agent-%s", agentName)
	staleSPC := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "secrets-store.csi.x-k8s.io/v1",
		"kind":       "SecretProviderClass",
		"metadata": map[string]interface{}{
			"name":      staleSPCName,
			"namespace": "default",
		},
	}}
	if _, err := dynClient.Resource(k8s.SecretProviderClassGVR).Namespace("default").Create(context.Background(), staleSPC, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to seed stale SPC: %v", err)
	}

	config := RunConfig{
		Name:         agentName,
		Image:        "test:latest",
		UnixUsername: "scion",
		Labels:       productionAgentLabels("agent", "proj1"),
		// No ResolvedSecrets/ResolvedAuth: this Run does not recreate either
		// stale object, so their removal can only be the pre-clean call.
	}

	runUntilPodSubmitted(t, rt, clientset, config)

	if _, err := clientset.CoreV1().Secrets("default").Get(context.Background(), staleAuthName, metav1.GetOptions{}); !k8serrors.IsNotFound(err) {
		t.Errorf("stale auth Secret should be removed by Run's pre-clean, got err=%v", err)
	}
	if _, err := dynClient.Resource(k8s.SecretProviderClassGVR).Namespace("default").Get(context.Background(), staleSPCName, metav1.GetOptions{}); !k8serrors.IsNotFound(err) {
		t.Errorf("stale SPC should be removed by Run's pre-clean, got err=%v", err)
	}
}

func TestRun_PodCreateFailure_CleansUpSecrets(t *testing.T) {
	rt, clientset, _ := newTestK8sRuntime()
	agentName := "proj1--agent"

	clientset.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, fmt.Errorf("simulated scheduler rejection")
	})

	config := RunConfig{
		Name:         agentName,
		Image:        "test:latest",
		UnixUsername: "scion",
		Labels:       productionAgentLabels("agent", "proj1"),
		ResolvedSecrets: []api.ResolvedSecret{
			{Name: "API_KEY", Type: "environment", Target: "API_KEY", Value: "sk-123", Source: "user"},
		},
	}

	_, err := rt.Run(context.Background(), config)
	if err == nil {
		t.Fatal("expected Run to fail when pod creation fails")
	}

	secretName := fmt.Sprintf("scion-agent-%s", agentName)
	if _, err := clientset.CoreV1().Secrets("default").Get(context.Background(), secretName, metav1.GetOptions{}); !k8serrors.IsNotFound(err) {
		t.Errorf("agent Secret created before the failed pod create should be cleaned up, got err=%v", err)
	}
}

// TestRun_InitContainerFailure_ThenHubDelete_CleansUpSecrets reproduces a
// live-cluster report: the pod Create call succeeds (so the per-agent Secret
// and the pod both exist), an init container then fails during startup, and
// the hub's reconciliation observes the failure and auto-cleans the agent —
// i.e. calls Delete the same way a user-initiated "scion rm" would. This is
// a distinct path from TestRun_PodCreateFailure_CleansUpSecrets (which covers
// the pod Create API call itself failing, before the pod exists at all).
// waitForPodReady's init-container check is exercised for real here (see the
// same technique in TestWaitForPodReady_* in k8s_nfs_test.go) rather than
// stubbed, so this proves Run surfaces the failure before anything calls
// Delete.
func TestRun_InitContainerFailure_ThenHubDelete_CleansUpSecrets(t *testing.T) {
	rt, clientset, _ := newTestK8sRuntime()
	agentName := "proj1--agent"
	config := RunConfig{
		Name:         agentName,
		Image:        "test:latest",
		UnixUsername: "scion",
		Labels:       productionAgentLabels("agent", "proj1"),
		ResolvedSecrets: []api.ResolvedSecret{
			{Name: "API_KEY", Type: "environment", Target: "API_KEY", Value: "sk-123", Source: "user"},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Once the pod exists, fail its init container the way a real cluster
	// would report a provisioning failure. waitForPodReady polls every 2s, so
	// a tight poll loop here reliably updates the status well before the
	// next poll. The loop exits on ctx.Done() too, so if Run returns before
	// the pod is ever created (e.g. a regression in an earlier step), this
	// goroutine still closes done instead of blocking the test forever.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			pod, err := clientset.CoreV1().Pods("default").Get(ctx, agentName, metav1.GetOptions{})
			if err == nil {
				pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{
					Name: "workspace-provision",
					State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
						ExitCode: 1,
						Reason:   "Error",
						Message:  "simulated provisioning failure",
					}},
				}}
				if _, err := clientset.CoreV1().Pods("default").UpdateStatus(ctx, pod, metav1.UpdateOptions{}); err == nil {
					return
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	_, runErr := rt.Run(ctx, config)
	// Cancel ctx now rather than waiting on the deferred cancel: if Run
	// returned early (e.g. before the pod ever exists), the poller goroutine
	// above is only watching ctx.Done() to exit, and ctx's own 5s
	// context.WithTimeout would otherwise make it run out the clock before
	// closing done, racing the select's own 5s timeout below.
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the init-container poller goroutine to finish")
	}
	if runErr == nil {
		t.Fatal("expected Run to surface the init container failure")
	}

	secretName := fmt.Sprintf("scion-agent-%s", agentName)
	if _, err := clientset.CoreV1().Secrets("default").Get(context.Background(), secretName, metav1.GetOptions{}); err != nil {
		t.Fatalf("precondition: agent Secret should still exist right after the failed Run (Run does not delete it on a post-create failure; Delete does): %v", err)
	}

	// Simulate the hub's reconciliation observing the failed agent and
	// auto-cleaning it, exactly as it would for a healthy agent being removed.
	if err := rt.Delete(context.Background(), RunRef{ID: agentName}); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	if _, err := clientset.CoreV1().Secrets("default").Get(context.Background(), secretName, metav1.GetOptions{}); !k8serrors.IsNotFound(err) {
		t.Errorf("agent Secret should be cleaned up once the hub deletes the failed agent, got err=%v", err)
	}
}
